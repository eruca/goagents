package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"unicode/utf8"

	"github.com/eruca/goagents/memorykit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var errBackendOperation = errors.New("database operation failed")

// Config contains the validation and embedding contract shared by every Store operation.
type Config struct {
	Limits              memorykit.Limits
	EmbeddingProfileID  string
	EmbeddingDimensions int
}

// Store owns PostgreSQL persistence. ownsDB distinguishes Open from caller-controlled OpenDB.
type Store struct {
	db     *sql.DB
	cfg    Config
	ownsDB bool
}

// Open creates and owns a pgx-backed database pool.
func Open(ctx context.Context, dsn string, cfg Config) (*Store, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w: PostgreSQL DSN is required", memorykit.ErrInvalidMemory)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, wrapBackend("open", err)
	}
	store, err := openDB(ctx, db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// OpenDB initializes a Store without taking ownership of the supplied pool.
func OpenDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("%w: PostgreSQL database is required", memorykit.ErrInvalidMemory)
	}
	return openDB(ctx, db, cfg)
}

func openDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, wrapBackend("ping", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	return &Store{db: db, cfg: cfg}, nil
}

// Close closes only pools created by Open. Caller-supplied OpenDB pools remain untouched.
func (s *Store) Close() error {
	if s == nil || s.db == nil || !s.ownsDB {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return wrapBackend("close", err)
	}
	return nil
}

func validateConfig(cfg Config) error {
	if err := cfg.Limits.Validate(); err != nil {
		return err
	}
	profile := cfg.EmbeddingProfileID
	if profile == "" || profile != strings.TrimSpace(profile) ||
		utf8.RuneCountInString(profile) > cfg.Limits.MaxMetadataRunes ||
		strings.IndexFunc(profile, func(r rune) bool { return r <= 0x1f || r == 0x7f }) >= 0 ||
		cfg.EmbeddingDimensions <= 0 {
		return fmt.Errorf("%w: invalid embedding profile", memorykit.ErrInvalidMemory)
	}
	return nil
}

// wrapBackend exposes only a stable classification; raw driver messages may contain SQL or data.
func wrapBackend(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &memorykit.BackendError{
		Op:          op,
		Recoverable: isTransientBackendError(err),
		Err:         errBackendOperation,
	}
}

func isTransientBackendError(err error) bool {
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		if temporary, ok := networkErr.(interface{ Temporary() bool }); ok && temporary.Temporary() {
			return true
		}
	}
	var sqlState interface{ SQLState() string }
	if !errors.As(err, &sqlState) {
		return false
	}
	state := sqlState.SQLState()
	return strings.HasPrefix(state, "08") || strings.HasPrefix(state, "53") ||
		state == "57P01" || state == "57P02" || state == "57P03"
}
