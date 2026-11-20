// Package pgarchive is a PostgreSQL contextkit.Archive, so every process of
// a deployment reads back and searches the same archived context.
//
// It stores the same references and ranks search results exactly like
// contextkit/archive.Memory (see archive/archivetest). Unlike Memory it has no
// limit on the number of scopes: delete a scope when its conversation ends
// (DeleteScope) or delete old messages periodically (DeleteBefore).
package pgarchive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/archive"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const defaultMaxMessagesPerScope = 500

// Config bounds a Store.
type Config struct {
	// MaxMessagesPerScope deletes a scope's oldest messages beyond it; zero
	// means 500, the same as archive.Memory.
	MaxMessagesPerScope int
}

// Store is a contextkit.Archive in PostgreSQL. It is safe for concurrent use
// by several goroutines and processes.
type Store struct {
	db     *sql.DB
	cfg    Config
	ownsDB bool

	closeOnce sync.Once
	closeErr  error
}

var _ contextkit.Archive = (*Store)(nil)

// Open connects to dsn, applies the schema, and returns a Store that closes
// the connection pool on Close.
func Open(ctx context.Context, dsn string, cfg Config) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("pgarchive: PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, wrap("open", err)
	}
	store, err := openDB(ctx, db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// OpenDB applies the schema on db and returns a Store that leaves db open on
// Close, for a pool shared with other stores.
func OpenDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if db == nil {
		return nil, errors.New("pgarchive: PostgreSQL database is required")
	}
	return openDB(ctx, db, cfg)
}

func openDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if cfg.MaxMessagesPerScope < 0 {
		return nil, errors.New("pgarchive: MaxMessagesPerScope must not be negative")
	}
	if cfg.MaxMessagesPerScope == 0 {
		cfg.MaxMessagesPerScope = defaultMaxMessagesPerScope
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, wrap("ping", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return &Store{db: db, cfg: cfg}, nil
}

// Close closes the connection pool if Open created it.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ownsDB {
			s.closeErr = wrap("close", s.db.Close())
		}
	})
	return s.closeErr
}

var errNoScope = errors.New("pgarchive: the context has no archive scope")

// Offload stores msg in the context's scope and returns archive.Ref for it.
// Storing a message the scope already holds changes nothing.
func (s *Store) Offload(ctx context.Context, msg contextkit.Message) (string, error) {
	scope := contextkit.ArchiveScope(ctx)
	if scope == "" {
		return "", errNoScope
	}
	ref := archive.Ref(scope, msg)
	encoded, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("pgarchive: encode message: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO contextkit_archive_messages (scope, ref, message, terms, created_at)
		VALUES ($1, $2, $3, $4::text[], now())
		ON CONFLICT (scope, ref) DO NOTHING`,
		scope, ref, string(encoded), archive.Terms(archive.SearchText(msg)))
	if err != nil {
		return "", wrap("offload", err)
	}
	if inserted, _ := result.RowsAffected(); inserted > 0 {
		// Keep the newest MaxMessagesPerScope: delete everything older than
		// the oldest of them.
		if _, err := s.db.ExecContext(ctx, `
			DELETE FROM contextkit_archive_messages
			WHERE scope = $1 AND seq < (
				SELECT seq FROM contextkit_archive_messages
				WHERE scope = $1 ORDER BY seq DESC OFFSET $2 LIMIT 1
			)`,
			scope, s.cfg.MaxMessagesPerScope-1); err != nil {
			return "", wrap("trim scope", err)
		}
	}
	return ref, nil
}

// Read returns the message stored under ref in the context's scope.
func (s *Store) Read(ctx context.Context, ref string) (contextkit.Message, error) {
	scope := contextkit.ArchiveScope(ctx)
	if scope == "" {
		return contextkit.Message{}, contextkit.ErrArchiveNotFound
	}
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT message FROM contextkit_archive_messages WHERE scope = $1 AND ref = $2`, scope, ref).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return contextkit.Message{}, contextkit.ErrArchiveNotFound
	}
	if err != nil {
		return contextkit.Message{}, wrap("read", err)
	}
	return decode(encoded)
}

// Search ranks the scope's messages like archive.Memory: by the query terms
// they share, each weighted by how rare it is in the scope, newer first on
// ties. It returns at most limit hits.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]contextkit.ArchiveHit, error) {
	scope := contextkit.ArchiveScope(ctx)
	terms := archive.Terms(query)
	if limit <= 0 || scope == "" || len(terms) == 0 {
		return nil, nil
	}
	// One snapshot, so the counts and the candidates agree.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, wrap("search transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM contextkit_archive_messages WHERE scope = $1`, scope).Scan(&n); err != nil {
		return nil, wrap("search count", err)
	}
	documents := map[string]int{}
	rows, err := tx.QueryContext(ctx, `
		SELECT term, count(*) FROM contextkit_archive_messages, unnest(terms) AS term
		WHERE scope = $1 AND term = ANY($2::text[])
		GROUP BY term`, scope, terms)
	if err != nil {
		return nil, wrap("search terms", err)
	}
	for rows.Next() {
		var term string
		var count int
		if err := rows.Scan(&term, &count); err != nil {
			rows.Close()
			return nil, wrap("search terms", err)
		}
		documents[term] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, wrap("search terms", err)
	}
	rows.Close()

	rows, err = tx.QueryContext(ctx, `
		SELECT ref, message, position FROM (
			SELECT ref, message, terms, row_number() OVER (ORDER BY seq) - 1 AS position
			FROM contextkit_archive_messages WHERE scope = $1
		) ranked
		WHERE terms && $2::text[]
		ORDER BY position`, scope, terms)
	if err != nil {
		return nil, wrap("search", err)
	}
	defer rows.Close()
	var hits []contextkit.ArchiveHit
	for rows.Next() {
		var ref, encoded string
		var position int
		if err := rows.Scan(&ref, &encoded, &position); err != nil {
			return nil, wrap("search", err)
		}
		msg, err := decode(encoded)
		if err != nil {
			return nil, err
		}
		contained := map[string]bool{}
		for _, term := range archive.Terms(archive.SearchText(msg)) {
			contained[term] = true
		}
		score := 0.0
		for _, term := range terms {
			if contained[term] {
				score += archive.TermWeight(n, documents[term])
			}
		}
		if score > 0 {
			hits = append(hits, contextkit.ArchiveHit{Ref: ref, Message: msg, Score: archive.TieBreak(score, position)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("search", err)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// DeleteScope deletes a scope's messages, for example when its conversation
// ends.
func (s *Store) DeleteScope(ctx context.Context, scope string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM contextkit_archive_messages WHERE scope = $1`, scope)
	return wrap("delete scope", err)
}

// DeleteBefore deletes messages archived before cutoff and returns how many,
// for periodic retention.
func (s *Store) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM contextkit_archive_messages WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, wrap("delete before", err)
	}
	deleted, _ := result.RowsAffected()
	return deleted, nil
}

func decode(encoded string) (contextkit.Message, error) {
	var msg contextkit.Message
	if err := json.Unmarshal([]byte(encoded), &msg); err != nil {
		return contextkit.Message{}, fmt.Errorf("pgarchive: decode message: %w", err)
	}
	return msg, nil
}

func wrap(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("pgarchive: %s: %w", operation, err)
}
