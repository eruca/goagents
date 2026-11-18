// Package pgaudit is a PostgreSQL llmkit.Recorder that replicas of a host
// share, with the read side llmkit offers for its JSONL audit files:
// ReadRouteAudits and BuildModelStats return what llmkit.ReadRouteAudits and
// llmkit.BuildModelStats return for the same records written in the same
// order to a llmkit.JSONLRecorder.
//
// Records are sanitized with llmkit.SanitizeRouteTrace and
// llmkit.SanitizeTaskOutcome, the rules JSONLRecorder applies, and stored as
// JSON documents in append-only tables. An insertion sequence plays the part
// of JSONL append order: it orders the records ReadRouteAudits returns and
// decides which outcome is the latest for a route_id and attempt. Under
// concurrent writers that order is the order rows were inserted, which, as with
// concurrent appends to a file, need not be the order of RecordedAt.
//
// Like the file version, BuildModelStats recomputes the stats from every
// record on each call; hosts that call it often should cache the result.
package pgaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/llmkit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Store is a shared llmkit.Recorder backed by PostgreSQL. It is safe for
// concurrent use, and any number of Stores may share one database.
type Store struct {
	db     *sql.DB
	ownsDB bool
	// now is the reference time for BuildModelStats; tests replace it.
	now func() time.Time

	closeOnce sync.Once
	closeErr  error
}

var _ llmkit.Recorder = (*Store)(nil)

// Open creates and owns a pgx-backed connection pool and applies the schema
// (llmkit_audit_routes, llmkit_audit_outcomes) under an advisory lock.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("pgaudit: PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, wrap("open", err)
	}
	store, err := openDB(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// OpenDB is Open on a pool the caller keeps owning: Close leaves it open.
func OpenDB(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("pgaudit: PostgreSQL database is required")
	}
	return openDB(ctx, db)
}

func openDB(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, wrap("ping", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the pool of a Store made by Open; for one made by OpenDB it
// does nothing.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ownsDB {
			s.closeErr = wrap("close", s.db.Close())
		}
	})
	return s.closeErr
}

// RecordRoute appends llmkit.SanitizeRouteTrace(trace).
func (s *Store) RecordRoute(ctx context.Context, trace llmkit.RouteTrace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(llmkit.SanitizeRouteTrace(trace))
	if err != nil {
		return wrap("encode route", err)
	}
	// The columns come from the record as it will be read back, so filtering
	// on them agrees with filtering the decoded records.
	var stored llmkit.RouteTrace
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return wrap("encode route", err)
	}
	return s.insert(ctx, "llmkit_audit_routes", stored.TaskID, stored.RouteID, stored.Attempt, stored.RecordedAt, encoded)
}

// RecordOutcome appends llmkit.SanitizeTaskOutcome(outcome).
func (s *Store) RecordOutcome(ctx context.Context, outcome llmkit.TaskOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(llmkit.SanitizeTaskOutcome(outcome))
	if err != nil {
		return wrap("encode outcome", err)
	}
	var stored llmkit.TaskOutcome
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return wrap("encode outcome", err)
	}
	return s.insert(ctx, "llmkit_audit_outcomes", stored.TaskID, stored.RouteID, stored.Attempt, stored.RecordedAt, encoded)
}

func (s *Store) insert(ctx context.Context, table, taskID, routeID string, attempt int, recordedAt time.Time, record []byte) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO "+table+" (task_id, route_id, attempt, recorded_at, record) VALUES ($1, $2, $3, $4, $5::json)",
		columnText(taskID), columnText(routeID), attempt, recordedAt, string(record))
	return wrap("insert", err)
}

// ReadRouteAudits returns the route traces matching filter in the order they
// were recorded, each joined with the latest outcome recorded for its
// route_id and attempt, as llmkit.ReadRouteAudits does for JSONL files.
func (s *Store) ReadRouteAudits(ctx context.Context, filter llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, wrap("begin read", err)
	}
	defer func() { _ = tx.Rollback() }()

	outcomes := map[outcomeKey]llmkit.TaskOutcome{}
	if err := readRecords(ctx, tx, "llmkit_audit_outcomes", filter, func(outcome llmkit.TaskOutcome) {
		if matchesFilter(outcome.TaskID, filter) {
			outcomes[outcomeKey{outcome.RouteID, outcome.Attempt}] = outcome
		}
	}); err != nil {
		return nil, err
	}
	var records []llmkit.RouteAuditRecord
	if err := readRecords(ctx, tx, "llmkit_audit_routes", filter, func(trace llmkit.RouteTrace) {
		if !matchesFilter(trace.TaskID, filter) {
			return
		}
		record := llmkit.RouteAuditRecord{Route: trace}
		if outcome, ok := outcomes[outcomeKey{trace.RouteID, trace.Attempt}]; ok {
			record.Outcome = &outcome
		}
		records = append(records, record)
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, wrap("commit read", err)
	}
	return records, nil
}

// BuildModelStats aggregates every recorded route and outcome with
// llmkit.BuildModelStatsFromAudit, as llmkit.BuildModelStats does for JSONL
// files. It reads all records on each call.
func (s *Store) BuildModelStats(ctx context.Context) (*llmkit.ModelStats, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, wrap("begin read", err)
	}
	defer func() { _ = tx.Rollback() }()

	var routes []llmkit.RouteTrace
	if err := readRecords(ctx, tx, "llmkit_audit_routes", llmkit.AuditFilter{}, func(trace llmkit.RouteTrace) {
		routes = append(routes, trace)
	}); err != nil {
		return nil, err
	}
	var outcomes []llmkit.TaskOutcome
	if err := readRecords(ctx, tx, "llmkit_audit_outcomes", llmkit.AuditFilter{}, func(outcome llmkit.TaskOutcome) {
		outcomes = append(outcomes, outcome)
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, wrap("commit read", err)
	}
	return llmkit.BuildModelStatsFromAudit(s.now().UTC(), routes, outcomes), nil
}

type outcomeKey struct {
	routeID string
	attempt int
}

// readRecords decodes the records of table in insertion order. With a TaskID
// filter it narrows the query by the task_id column; callers still compare the
// decoded TaskID, which is exact where columnText had to substitute characters.
func readRecords[T any](ctx context.Context, tx *sql.Tx, table string, filter llmkit.AuditFilter, handle func(T)) error {
	query := "SELECT record FROM " + table
	var args []any
	if strings.TrimSpace(filter.TaskID) != "" {
		query += " WHERE task_id = $1"
		args = append(args, columnText(filter.TaskID))
	}
	query += " ORDER BY seq"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return wrap("read "+table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return wrap("read "+table, err)
		}
		var record T
		if err := json.Unmarshal(raw, &record); err != nil {
			return wrap("decode "+table, err)
		}
		handle(record)
	}
	return wrap("read "+table, rows.Err())
}

// matchesFilter is the TaskID rule of llmkit.ReadRouteAudits.
func matchesFilter(taskID string, filter llmkit.AuditFilter) bool {
	return strings.TrimSpace(filter.TaskID) == "" || taskID == filter.TaskID
}

// columnText makes s storable in a TEXT column, which rejects NUL and invalid
// UTF-8. The record keeps the original value.
func columnText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("pgaudit: %s: %w", op, err)
}
