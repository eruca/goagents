package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eruca/goagents/workflowkit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	_ workflowkit.CreateStore        = (*Store)(nil)
	_ workflowkit.WorkflowQueryStore = (*Store)(nil)
	_ workflowkit.QueueRecoveryStore = (*Store)(nil)
)

// Store persists workflow runs in PostgreSQL. ownsDB distinguishes Open from
// caller-controlled OpenDB.
type Store struct {
	db     *sql.DB
	ownsDB bool
}

type queryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// runColumns is the column list every query reads and writes, in scanRun order.
const runColumns = `id, status, input_ref, output_ref, agent_run_id, audit_ref, error,
	approval_ref, waiting_reason, current_step, completed_steps_json,
	step_attempts_json, step_retries, next_attempt_at, metadata_json,
	lease_owner, lease_until, created_at, updated_at`

// Open creates and owns a pgx-backed database pool and migrates the schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("workflowkit pgstore: PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("workflowkit pgstore: open: %w", err)
	}
	store, err := openDB(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// OpenDB migrates the schema and returns a Store using db without taking
// ownership of it: Close leaves db open.
func OpenDB(ctx context.Context, db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("workflowkit pgstore: PostgreSQL database is required")
	}
	return openDB(ctx, db)
}

func openDB(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("workflowkit pgstore: ping: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes only pools created by Open. Caller-supplied OpenDB pools remain untouched.
func (s *Store) Close() error {
	if s == nil || s.db == nil || !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// now is the current time as the store writes it.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// dbTime is the column value of t: NULL for the zero time, otherwise t in UTC
// truncated to the microseconds TIMESTAMPTZ keeps. PostgreSQL would round
// instead, which could move a time later than the one the caller wrote.
func dbTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Truncate(time.Microsecond)
}

type timeColumn struct {
	dst *time.Time
}

func scanTime(dst *time.Time) timeColumn {
	return timeColumn{dst: dst}
}

func (c timeColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = time.Time{}
	case time.Time:
		*c.dst = v.UTC()
	default:
		return fmt.Errorf("unsupported time column value %T", src)
	}
	return nil
}

func (s *Store) Save(ctx context.Context, run workflowkit.WorkflowRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveRun(ctx, tx, run); err != nil {
		return err
	}
	return tx.Commit()
}

// Create inserts run and fails with workflowkit.ErrRunExists when a run with
// the same id exists. A concurrent Create of the same id waits for the first
// to commit, then fails.
func (s *Store) Create(ctx context.Context, run workflowkit.WorkflowRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stamp(&run)
	args, err := runArgs(run)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO workflow_runs (`+runColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (id) DO NOTHING`, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return workflowkit.ErrRunExists
	}
	if err := saveStepRecords(ctx, tx, run.ID, run.StepRecords); err != nil {
		return err
	}
	return tx.Commit()
}

func stamp(run *workflowkit.WorkflowRun) {
	current := now()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = current
	}
	run.UpdatedAt = current
}

func runArgs(run workflowkit.WorkflowRun) ([]any, error) {
	completedSteps, err := marshalJSON(run.CompletedSteps)
	if err != nil {
		return nil, err
	}
	stepAttempts, err := marshalJSON(run.StepAttempts)
	if err != nil {
		return nil, err
	}
	metadata, err := marshalJSON(run.Metadata)
	if err != nil {
		return nil, err
	}
	return []any{
		run.ID, string(run.Status), run.InputRef, run.OutputRef, run.AgentRunID, run.AuditRef, run.Error,
		run.ApprovalRef, run.WaitingReason, run.CurrentStep, completedSteps,
		stepAttempts, run.StepRetries, dbTime(run.NextAttemptAt), metadata,
		run.LeaseOwner, dbTime(run.LeaseUntil), dbTime(run.CreatedAt), dbTime(run.UpdatedAt),
	}, nil
}

// saveRun upserts the run row before its step records, so the row lock the
// upsert takes serializes concurrent saves of the same run.
func saveRun(ctx context.Context, db queryExecer, run workflowkit.WorkflowRun) error {
	stamp(&run)
	args, err := runArgs(run)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO workflow_runs (`+runColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
ON CONFLICT (id) DO UPDATE SET
	status = excluded.status,
	input_ref = excluded.input_ref,
	output_ref = excluded.output_ref,
	agent_run_id = excluded.agent_run_id,
	audit_ref = excluded.audit_ref,
	error = excluded.error,
	approval_ref = excluded.approval_ref,
	waiting_reason = excluded.waiting_reason,
	current_step = excluded.current_step,
	completed_steps_json = excluded.completed_steps_json,
	step_attempts_json = excluded.step_attempts_json,
	step_retries = excluded.step_retries,
	next_attempt_at = excluded.next_attempt_at,
	metadata_json = excluded.metadata_json,
	lease_owner = excluded.lease_owner,
	lease_until = excluded.lease_until,
	created_at = excluded.created_at,
	updated_at = excluded.updated_at`, args...); err != nil {
		return err
	}
	return saveStepRecords(ctx, db, run.ID, run.StepRecords)
}

// saveStepRecords writes the records that differ from the stored ones and
// drops stored records past the end. Executors only append records, so a save
// usually inserts one row however long the history is.
func saveStepRecords(ctx context.Context, db queryExecer, runID string, records []workflowkit.StepRecord) error {
	stored, err := storedStepRecords(ctx, db, runID)
	if err != nil {
		return err
	}
	for seq, record := range records {
		data, err := marshalJSON(record)
		if err != nil {
			return err
		}
		if seq < len(stored) && stored[seq] == data {
			continue
		}
		if _, err := db.ExecContext(ctx, `
INSERT INTO workflow_step_records (run_id, seq, record_json) VALUES ($1, $2, $3)
ON CONFLICT (run_id, seq) DO UPDATE SET record_json = excluded.record_json`, runID, seq, data); err != nil {
			return err
		}
	}
	if len(stored) > len(records) {
		if _, err := db.ExecContext(ctx, `DELETE FROM workflow_step_records WHERE run_id = $1 AND seq >= $2`, runID, len(records)); err != nil {
			return err
		}
	}
	return nil
}

func storedStepRecords(ctx context.Context, db queryExecer, runID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT record_json FROM workflow_step_records WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stored []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		stored = append(stored, data)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stored, nil
}

func loadStepRecords(ctx context.Context, db queryExecer, run *workflowkit.WorkflowRun) error {
	stored, err := storedStepRecords(ctx, db, run.ID)
	if err != nil {
		return err
	}
	var records []workflowkit.StepRecord
	for _, data := range stored {
		var record workflowkit.StepRecord
		if err := unmarshalJSON(data, &record); err != nil {
			return err
		}
		records = append(records, record)
	}
	run.StepRecords = records
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(scanner rowScanner) (workflowkit.WorkflowRun, error) {
	var (
		run                                        workflowkit.WorkflowRun
		status                                     string
		completedSteps, stepAttempts, metadataJSON string
	)
	if err := scanner.Scan(&run.ID, &status, &run.InputRef, &run.OutputRef, &run.AgentRunID, &run.AuditRef, &run.Error,
		&run.ApprovalRef, &run.WaitingReason, &run.CurrentStep, &completedSteps,
		&stepAttempts, &run.StepRetries, scanTime(&run.NextAttemptAt), &metadataJSON,
		&run.LeaseOwner, scanTime(&run.LeaseUntil), scanTime(&run.CreatedAt), scanTime(&run.UpdatedAt)); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	run.Status = workflowkit.Status(status)
	if err := unmarshalJSON(completedSteps, &run.CompletedSteps); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := unmarshalJSON(stepAttempts, &run.StepAttempts); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := unmarshalJSON(metadataJSON, &run.Metadata); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return run, nil
}

// scanRuns reads every row and closes rows, so the caller can run further
// queries on the same transaction.
func scanRuns(rows *sql.Rows) ([]workflowkit.WorkflowRun, error) {
	defer rows.Close()
	var runs []workflowkit.WorkflowRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runs, rows.Close()
}

func (s *Store) Get(ctx context.Context, id string) (workflowkit.WorkflowRun, error) {
	return getRun(ctx, s.db, id, "")
}

// getRun reads a run and its step records; lock is appended to the run row
// query, such as "FOR UPDATE".
func getRun(ctx context.Context, db queryExecer, id string, lock string) (workflowkit.WorkflowRun, error) {
	run, err := scanRun(db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM workflow_runs WHERE id = $1 `+lock, id))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowkit.WorkflowRun{}, workflowkit.ErrRunNotFound
	}
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := loadStepRecords(ctx, db, &run); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return run, nil
}

// Update locks the run row for the whole read, mutate, and write, so a
// concurrent Save, Update, claim, or lease change of the same run waits for it
// instead of being lost.
func (s *Store) Update(ctx context.Context, id string, mutate func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error)) (workflowkit.WorkflowRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	defer func() { _ = tx.Rollback() }()

	current, err := getRun(ctx, tx, id, "FOR UPDATE")
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	updated, err := mutate(current)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = current.CreatedAt
	}
	if err := saveRun(ctx, tx, updated); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	persisted, err := getRun(ctx, tx, updated.ID, "")
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return persisted, nil
}

// ListWorkflows filters by status in SQL and by metadata in Go, with
// workflowkit.MetadataValueString, applying Limit after both filters. A
// metadata query therefore scans every run with the requested status.
func (s *Store) ListWorkflows(ctx context.Context, query workflowkit.WorkflowQuery) ([]workflowkit.WorkflowRun, error) {
	if query.Status != "" && !query.Status.IsValid() {
		return nil, fmt.Errorf("invalid workflow status: %s", query.Status)
	}
	if !query.Order.IsValid() {
		return nil, fmt.Errorf("invalid workflow order: %s", query.Order)
	}
	order := "ASC"
	if query.Order == workflowkit.WorkflowOrderDesc {
		order = "DESC"
	}

	where, args := "", []any{}
	if query.Status != "" {
		where, args = "WHERE status = $1", append(args, string(query.Status))
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+runColumns+` FROM workflow_runs %s ORDER BY created_at %s, id %s`, where, order, order), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []workflowkit.WorkflowRun{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		if !workflowMetadataMatches(run, query.MetadataEquals) {
			continue
		}
		runs = append(runs, run)
		if query.Limit > 0 && len(runs) >= query.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range runs {
		if err := loadStepRecords(ctx, s.db, &runs[i]); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func workflowMetadataMatches(run workflowkit.WorkflowRun, equals map[string]string) bool {
	for key, want := range equals {
		got, ok := run.Metadata[key]
		if !ok {
			return false
		}
		if workflowkit.MetadataValueString(got) != want {
			return false
		}
	}
	return true
}

// ClaimRunnable leases the oldest pending run that is due and has no live
// lease. SKIP LOCKED lets concurrent workers pass over a run another worker
// is claiming, and the claim re-checks the lease on the locked row, so each
// run goes to one worker.
func (s *Store) ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (workflowkit.WorkflowRun, error) {
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return workflowkit.WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	defer func() { _ = tx.Rollback() }()

	current := now()
	run, err := scanRun(tx.QueryRowContext(ctx, `
WITH claimable AS (
	SELECT id FROM workflow_runs
	WHERE status = $1
	  AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
	  AND (lease_owner = '' OR lease_until IS NULL OR lease_until <= $2)
	ORDER BY created_at ASC, id ASC
	FOR UPDATE SKIP LOCKED
	LIMIT 1
)
UPDATE workflow_runs AS runs
SET lease_owner = $3, lease_until = $4, updated_at = $2
FROM claimable
WHERE runs.id = claimable.id
RETURNING `+qualifiedRunColumns("runs"),
		string(workflowkit.StatusPending), current, workerID, dbTime(current.Add(lease))))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowkit.WorkflowRun{}, workflowkit.ErrNoRunnableWorkflow
	}
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := loadStepRecords(ctx, tx, &run); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return run, nil
}

func qualifiedRunColumns(alias string) string {
	columns := strings.Split(runColumns, ",")
	for i, column := range columns {
		columns[i] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}

func (s *Store) ExtendLease(ctx context.Context, id string, workerID string, lease time.Duration) (workflowkit.WorkflowRun, error) {
	if id == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return workflowkit.WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}
	current := now()
	return s.updateLease(ctx, id, `
UPDATE workflow_runs
SET lease_until = $1, updated_at = $2
WHERE id = $3
  AND lease_owner = $4
  AND lease_until > $2
RETURNING `+runColumns, dbTime(current.Add(lease)), current, id, workerID)
}

// ReleaseLease clears the lease of a run that is no longer running. A run
// still running keeps its owner and its lease expires now, so recovery fails it.
func (s *Store) ReleaseLease(ctx context.Context, id string, workerID string) (workflowkit.WorkflowRun, error) {
	if id == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	return s.updateLease(ctx, id, `
UPDATE workflow_runs
SET lease_owner = CASE WHEN status = $1 THEN lease_owner ELSE '' END,
    lease_until = CASE WHEN status = $1 THEN $2::timestamptz ELSE NULL END,
    updated_at = $2
WHERE id = $3
  AND lease_owner = $4
RETURNING `+runColumns, string(workflowkit.StatusRunning), now(), id, workerID)
}

// updateLease runs a conditional lease UPDATE ... RETURNING. No row means the
// run is missing or the caller does not own its lease.
func (s *Store) updateLease(ctx context.Context, id string, query string, args ...any) (workflowkit.WorkflowRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := getRun(ctx, tx, id, ""); err != nil {
			return workflowkit.WorkflowRun{}, err
		}
		return workflowkit.WorkflowRun{}, workflowkit.ErrWorkflowLeaseNotOwned
	}
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := loadStepRecords(ctx, tx, &run); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return run, nil
}

// FailExpiredRunning fails running runs whose lease owner stopped renewing
// the lease. SKIP LOCKED and the re-check on the locked row mean concurrent
// callers, even in different processes, each report a run at most once.
func (s *Store) FailExpiredRunning(ctx context.Context, reason string) ([]workflowkit.WorkflowRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	current := now()
	rows, err := tx.QueryContext(ctx, `
WITH expired AS (
	SELECT id FROM workflow_runs
	WHERE status = $1
	  AND lease_owner <> ''
	  AND (lease_until IS NULL OR lease_until <= $2)
	ORDER BY id
	FOR UPDATE SKIP LOCKED
)
UPDATE workflow_runs AS runs
SET status = $3, error = $4, step_retries = 0, next_attempt_at = NULL,
    lease_owner = '', lease_until = NULL, updated_at = $2
FROM expired
WHERE runs.id = expired.id
RETURNING `+qualifiedRunColumns("runs"),
		string(workflowkit.StatusRunning), current, string(workflowkit.StatusFailed), reason)
	if err != nil {
		return nil, err
	}
	failed, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].ID < failed[j].ID })
	for i := range failed {
		if err := loadStepRecords(ctx, tx, &failed[i]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if failed == nil {
		failed = []workflowkit.WorkflowRun{}
	}
	return failed, nil
}

func marshalJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal workflow run field: %w", err)
	}
	return string(data), nil
}

func unmarshalJSON[T any](data string, out *T) error {
	if data == "" {
		data = "null"
	}
	if err := json.Unmarshal([]byte(data), out); err != nil {
		return fmt.Errorf("unmarshal workflow run field: %w", err)
	}
	return nil
}
