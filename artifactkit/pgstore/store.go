package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eruca/goagents/artifactkit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var _ artifactkit.Store = (*Store)(nil)

// Store persists artifacts in PostgreSQL. ownsDB distinguishes Open from
// caller-controlled OpenDB.
type Store struct {
	db     *sql.DB
	ownsDB bool
}

// Open creates and owns a pgx-backed database pool and migrates the schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("artifactkit pgstore: PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("artifactkit pgstore: open: %w", err)
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
		return nil, errors.New("artifactkit pgstore: PostgreSQL database is required")
	}
	return openDB(ctx, db)
}

func openDB(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("artifactkit pgstore: ping: %w", err)
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

// Put stores artifact under its ref, replacing any artifact already there
// (last writer wins). created_at keeps the time the ref was first written.
func (s *Store) Put(ctx context.Context, artifact artifactkit.Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Ref) == "" {
		return fmt.Errorf("artifact ref is required")
	}
	if !storableRef(artifact.Ref) {
		return errors.New("artifactkit pgstore: artifact ref must be valid UTF-8 without NUL bytes")
	}
	var metadata any
	if len(artifact.Metadata) > 0 {
		encoded, err := json.Marshal(artifact.Metadata)
		if err != nil {
			return fmt.Errorf("artifactkit pgstore: encode metadata: %w", err)
		}
		metadata = string(encoded)
	}
	content := artifact.Content
	if content == nil {
		content = []byte{}
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO artifacts (ref, content, content_type, metadata_json, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (ref) DO UPDATE SET
  content = EXCLUDED.content,
  content_type = EXCLUDED.content_type,
  metadata_json = EXCLUDED.metadata_json,
  updated_at = EXCLUDED.updated_at`,
		artifact.Ref, content, artifact.ContentType, metadata, stamp)
	if err != nil {
		return fmt.Errorf("artifactkit pgstore: put %s: %w", artifact.Ref, err)
	}
	return nil
}

// Get returns the artifact stored under ref, or an error wrapping
// artifactkit.ErrArtifactNotFound.
func (s *Store) Get(ctx context.Context, ref string) (artifactkit.Artifact, error) {
	if err := ctx.Err(); err != nil {
		return artifactkit.Artifact{}, err
	}
	if !storableRef(ref) {
		return artifactkit.Artifact{}, fmt.Errorf("%w: %s", artifactkit.ErrArtifactNotFound, ref)
	}
	var (
		content     []byte
		contentType string
		metadata    sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT content, content_type, metadata_json FROM artifacts WHERE ref = $1`, ref,
	).Scan(&content, &contentType, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactkit.Artifact{}, fmt.Errorf("%w: %s", artifactkit.ErrArtifactNotFound, ref)
	}
	if err != nil {
		return artifactkit.Artifact{}, fmt.Errorf("artifactkit pgstore: get %s: %w", ref, err)
	}
	artifact := artifactkit.Artifact{Ref: ref, ContentType: contentType}
	if len(content) > 0 {
		// database/sql already copies into content; keep nil for empty content
		// as FileStore and MemoryStore do.
		artifact.Content = content
	}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &artifact.Metadata); err != nil {
			return artifactkit.Artifact{}, fmt.Errorf("artifactkit pgstore: decode metadata %s: %w", ref, err)
		}
		if len(artifact.Metadata) == 0 {
			artifact.Metadata = nil
		}
	}
	return artifact, nil
}

// storableRef reports whether PostgreSQL TEXT can hold ref.
func storableRef(ref string) bool {
	return utf8.ValidString(ref) && !strings.ContainsRune(ref, 0)
}
