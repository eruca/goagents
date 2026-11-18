package pgstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/eruca/goagents/memorykit"
)

// runGateUnlockTimeout bounds releasing the lock after run returns. The
// unlock does not inherit the caller's cancellation.
const runGateUnlockTimeout = 5 * time.Second

// embeddingRunGateLockSQL derives a 64-bit advisory lock key from the job
// name, the connection's current schema, and the embedding profile, then
// tries to take it. $1 is the profile ID.
const embeddingRunGateLockSQL = `
	SELECT key, pg_try_advisory_lock(key) FROM (
		SELECT hashtextextended(
			'memorykit.embedding/' || COALESCE(current_schema(), '') || '/' || $1::text, 0) AS key
	) k`

// EmbeddingRunGate returns a memorykit.RunGate that lets one embedding worker
// at a time run against this database, so replicas sharing it do not embed
// the same batch at once. Pass it as memorykit.EmbeddingWorkerConfig.RunGate.
//
// Each TryRun takes a dedicated connection from the Store's pool and calls
// pg_try_advisory_lock without waiting: when another session holds the lock,
// TryRun returns ran=false at once. The lock is released with
// pg_advisory_unlock after run returns, panics, or its context is canceled,
// and the connection is then returned to the pool; if the unlock cannot be
// confirmed, the connection is discarded, which ends the session and so
// releases the lock.
//
// PostgreSQL advisory locks are scoped to one database, not to a schema. The
// key is a hash of "memorykit.embedding", the connection's current schema
// (the first existing schema on its search_path) and the Store's embedding
// profile, so Stores in different schemas of one database, or with different
// profiles, do not block each other, while every Store on the same schema and
// profile shares one lock. Stores in different databases never share a lock.
func (s *Store) EmbeddingRunGate() memorykit.RunGate {
	return &advisoryRunGate{store: s, lockSQL: embeddingRunGateLockSQL, lockArg: s.cfg.EmbeddingProfileID}
}

type advisoryRunGate struct {
	store   *Store
	lockSQL string
	lockArg any
}

var _ memorykit.RunGate = (*advisoryRunGate)(nil)

func (g *advisoryRunGate) TryRun(ctx context.Context, run func(context.Context) error) (ran bool, returnErr error) {
	if run == nil {
		return false, fmt.Errorf("%w: run gate requires a run function", memorykit.ErrInvalidMemory)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	conn, err := g.store.db.Conn(ctx)
	if err != nil {
		return false, wrapBackend("run gate connection", err)
	}
	discard := false
	defer func() {
		if discard {
			// Ending the session releases any lock it may still hold.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}()

	var key int64
	var locked bool
	if err := conn.QueryRowContext(ctx, g.lockSQL, g.lockArg).Scan(&key, &locked); err != nil {
		// The lock may have been taken before the error was seen.
		discard = true
		return false, wrapBackend("run gate lock", err)
	}
	if !locked {
		return false, nil
	}

	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runGateUnlockTimeout)
		defer cancel()
		var unlocked bool
		err := conn.QueryRowContext(unlockCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked)
		if err == nil && !unlocked {
			err = errors.New("advisory lock was not held")
		}
		if err != nil {
			discard = true
			returnErr = errors.Join(returnErr, wrapBackend("run gate unlock", err))
		}
	}()
	return true, run(ctx)
}
