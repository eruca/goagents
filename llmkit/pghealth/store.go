// Package pghealth is a PostgreSQL llmkit.HealthStore that replicas of a host
// share, so the provider and account concurrency limits, the single half-open
// probe, and the circuit breaker hold across all of them.
//
// Each admitted call is a lease row that expires LeaseTTL after it was taken
// or last renewed. A Store renews its own leases in the background every
// LeaseTTL/3, so long calls keep their slot; the leases of a replica that
// crashed stop being renewed and free their slots within LeaseTTL. Admission
// and outcome rules are llmkit.EvaluateAdmission and llmkit.ApplyHealthOutcome,
// the same ones llmkit.MemoryHealthStore applies.
//
// Times come from Config.Policy.Now (the wall clock by default) on each
// replica and are compared with times other replicas wrote. Replicas' clocks
// must agree to well within LeaseTTL/3, or a lease can expire before its
// owner renews it; clock skew also shifts when a cooldown ends by as much.
// Keep replicas synchronized with NTP.
package pghealth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eruca/goagents/llmkit"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultLeaseTTL        = 30 * time.Second
	defaultRefreshInterval = time.Second
	refreshTimeout         = 5 * time.Second
)

// Config configures a Store.
type Config struct {
	// Policy configures the circuit breaker and the clock, as it does for
	// llmkit.NewMemoryHealthStore. Every replica should use the same policy.
	Policy llmkit.HealthPolicy
	// LeaseTTL is how long an admitted call holds its slot without being
	// renewed, which bounds how long a crashed replica's calls stay counted.
	// Zero means 30 seconds.
	LeaseTTL time.Duration
	// RefreshInterval is how often Snapshot's cached view is reloaded to pick
	// up other replicas' writes. Zero means one second.
	RefreshInterval time.Duration
}

// Store is a shared llmkit.HealthStore. Open or OpenDB starts its background
// lease renewal and snapshot refresh; Close stops them.
type Store struct {
	db     *sql.DB
	ownsDB bool
	cfg    Config
	owner  string

	stop      context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	refreshSeq atomic.Uint64
	mu         sync.Mutex
	appliedSeq uint64
	cachedAt   time.Time
	cache      map[string]llmkit.ProviderHealthEntry
}

var _ llmkit.HealthStore = (*Store)(nil)

// Open creates and owns a pgx-backed connection pool, applies the schema,
// and starts the background work.
func Open(ctx context.Context, dsn string, cfg Config) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("pghealth: PostgreSQL DSN is required")
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

// OpenDB is Open on a pool the caller keeps owning: Close leaves it open.
func OpenDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if db == nil {
		return nil, errors.New("pghealth: PostgreSQL database is required")
	}
	return openDB(ctx, db, cfg)
}

func openDB(ctx context.Context, db *sql.DB, cfg Config) (*Store, error) {
	if cfg.LeaseTTL < 0 || cfg.RefreshInterval < 0 {
		return nil, errors.New("pghealth: LeaseTTL and RefreshInterval must not be negative")
	}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, wrap("ping", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, err
	}
	s := &Store{db: db, cfg: cfg, owner: uuid.NewString()}
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}
	background, stop := context.WithCancel(context.Background())
	s.stop = stop
	s.wg.Go(func() { s.every(background, max(cfg.LeaseTTL/3, time.Millisecond), s.renew) })
	s.wg.Go(func() { s.every(background, cfg.RefreshInterval, s.refresh) })
	return s, nil
}

// Close stops the background work and, for a Store made by Open, closes its
// pool. It does not release the leases of calls still in flight: like a
// crashed replica's, they free their slots LeaseTTL after their last renewal.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.stop()
		s.wg.Wait()
		if s.ownsDB {
			s.closeErr = wrap("close", s.db.Close())
		}
	})
	return s.closeErr
}

// Acquire admits a call under a transaction-scoped advisory lock on the
// candidate's account, so the account limit across its keys and the single
// half-open probe hold across replicas. An admitted call takes a lease.
func (s *Store) Acquire(ctx context.Context, candidate llmkit.Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	account := strings.TrimSpace(candidate.AccountAlias)
	key := llmkit.ProviderHealthKey(candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("acquire", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('llmkit.account:' || $1::text))`, account); err != nil {
		return wrap("acquire lock", err)
	}
	entry, err := lockEntry(ctx, tx, key, account, candidate.Model.Alias, candidate.Model.Provider, now)
	if err != nil {
		return wrap("acquire entry", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM llmkit_provider_health_leases
WHERE account_alias = $1 AND expires_at <= $2`, account, now); err != nil {
		return wrap("acquire expire leases", err)
	}
	var keyInFlight, accountInFlight int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE key = $2), count(*)
FROM llmkit_provider_health_leases WHERE account_alias = $1`, account, key).Scan(&keyInFlight, &accountInFlight); err != nil {
		return wrap("acquire count leases", err)
	}
	entry.InFlight = keyInFlight
	admitted, err := llmkit.EvaluateAdmission(s.cfg.Policy, entry, candidate, accountInFlight, now)
	if err != nil {
		return err
	}
	if err := writeEntry(ctx, tx, key, admitted); err != nil {
		return wrap("acquire write entry", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llmkit_provider_health_leases
(lease_id, key, account_alias, owner, acquired_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), key, account, s.owner, now, now.Add(s.cfg.LeaseTTL)); err != nil {
		return wrap("acquire lease", err)
	}
	if err := tx.Commit(); err != nil {
		return wrap("acquire commit", err)
	}
	s.refreshAfterWrite(ctx)
	return nil
}

// Release gives back the oldest slot this Store holds for the candidate.
func (s *Store) Release(ctx context.Context, candidate llmkit.Candidate) error {
	key := llmkit.ProviderHealthKey(candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	if _, err := s.db.ExecContext(ctx, deleteOwnLeaseSQL, s.owner, key); err != nil {
		return wrap("release", err)
	}
	s.refreshAfterWrite(ctx)
	return nil
}

// RecordOutcome gives back the oldest slot this Store holds for the
// outcome's key and applies the outcome to the entry in one transaction.
func (s *Store) RecordOutcome(ctx context.Context, outcome llmkit.TaskOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := llmkit.ProviderHealthKey(outcome.AccountAlias, outcome.ModelAlias, outcome.Provider)
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("record outcome", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The entry is locked before the lease is deleted, the order Acquire
	// takes them in.
	entry, err := lockEntry(ctx, tx, key, outcome.AccountAlias, outcome.ModelAlias, outcome.Provider, now)
	if err != nil {
		return wrap("record outcome entry", err)
	}
	if _, err := tx.ExecContext(ctx, deleteOwnLeaseSQL, s.owner, key); err != nil {
		return wrap("record outcome lease", err)
	}
	if err := writeEntry(ctx, tx, key, llmkit.ApplyHealthOutcome(s.cfg.Policy, entry, outcome, now)); err != nil {
		return wrap("record outcome write entry", err)
	}
	if err := tx.Commit(); err != nil {
		return wrap("record outcome commit", err)
	}
	s.refreshAfterWrite(ctx)
	return nil
}

// Set stores an entry's health fields the way MemoryHealthStore.Set does, for
// hosts that set a provider's concurrency limit, quota, or availability.
// InFlight is ignored: it is the count of admitted calls.
func (s *Store) Set(ctx context.Context, entry llmkit.ProviderHealthEntry) error {
	entry.AccountAlias = strings.TrimSpace(entry.AccountAlias)
	entry.ModelAlias = strings.TrimSpace(entry.ModelAlias)
	entry.Provider = strings.TrimSpace(entry.Provider)
	if entry.Availability == "" {
		entry.Availability = llmkit.ProviderAvailable
	}
	entry.UpdatedAt = s.now()
	key := llmkit.ProviderHealthKey(entry.AccountAlias, entry.ModelAlias, entry.Provider)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO llmkit_provider_health
(key, account_alias, model_alias, provider, availability, max_concurrency, quota_remaining,
 quota_exhausted, failure_streak, last_failure_at, cooldown_until, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (key) DO UPDATE SET
 account_alias = EXCLUDED.account_alias, model_alias = EXCLUDED.model_alias, provider = EXCLUDED.provider,
 availability = EXCLUDED.availability, max_concurrency = EXCLUDED.max_concurrency,
 quota_remaining = EXCLUDED.quota_remaining, quota_exhausted = EXCLUDED.quota_exhausted,
 failure_streak = EXCLUDED.failure_streak, last_failure_at = EXCLUDED.last_failure_at,
 cooldown_until = EXCLUDED.cooldown_until, updated_at = EXCLUDED.updated_at`,
		key, entry.AccountAlias, entry.ModelAlias, entry.Provider, string(entry.Availability),
		entry.MaxConcurrency, entry.QuotaRemaining, entry.QuotaExhausted, entry.FailureStreak,
		nullTime(entry.LastFailureAt), nullTime(entry.CooldownUntil), entry.UpdatedAt); err != nil {
		return wrap("set", err)
	}
	s.refreshAfterWrite(ctx)
	return nil
}

// Snapshot returns the view cached by the last refresh, which runs every
// RefreshInterval and after each of this Store's writes, so other replicas'
// writes show up within RefreshInterval. GeneratedAt is when that view was
// read; the degraded-recovery rule is applied at the time of the call.
func (s *Store) Snapshot() llmkit.ProviderHealthSnapshot {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make(map[string]llmkit.ProviderHealthEntry, len(s.cache))
	for key, entry := range s.cache {
		entries[key] = llmkit.SettleHealthEntry(s.cfg.Policy, entry, now)
	}
	return llmkit.ProviderHealthSnapshot{GeneratedAt: s.cachedAt, Entries: entries}
}

// deleteOwnLeaseSQL deletes the oldest lease the owner holds for a key.
// SKIP LOCKED makes concurrent calls of one Store each take a different
// lease instead of all deleting the same one.
const deleteOwnLeaseSQL = `DELETE FROM llmkit_provider_health_leases WHERE lease_id = (
  SELECT lease_id FROM llmkit_provider_health_leases
  WHERE owner = $1 AND key = $2
  ORDER BY acquired_at, lease_id
  LIMIT 1
  FOR UPDATE SKIP LOCKED)`

// lockEntry returns the entry for key, created if missing, locked until the
// transaction ends. Its InFlight is zero.
func lockEntry(ctx context.Context, tx *sql.Tx, key, account, model, provider string, now time.Time) (llmkit.ProviderHealthEntry, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO llmkit_provider_health
(key, account_alias, model_alias, provider, updated_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (key) DO NOTHING`,
		key, strings.TrimSpace(account), strings.TrimSpace(model), strings.TrimSpace(provider), now); err != nil {
		return llmkit.ProviderHealthEntry{}, err
	}
	entry, err := scanEntry(tx.QueryRowContext(ctx, `SELECT `+entryColumns+`, 0
FROM llmkit_provider_health WHERE key = $1 FOR UPDATE`, key))
	return entry, err
}

// writeEntry stores every field of entry but InFlight.
func writeEntry(ctx context.Context, tx *sql.Tx, key string, entry llmkit.ProviderHealthEntry) error {
	_, err := tx.ExecContext(ctx, `UPDATE llmkit_provider_health SET
 account_alias = $2, model_alias = $3, provider = $4, availability = $5, max_concurrency = $6,
 quota_remaining = $7, quota_exhausted = $8, failure_streak = $9, last_failure_at = $10,
 cooldown_until = $11, updated_at = $12
WHERE key = $1`,
		key, entry.AccountAlias, entry.ModelAlias, entry.Provider, string(entry.Availability),
		entry.MaxConcurrency, entry.QuotaRemaining, entry.QuotaExhausted, entry.FailureStreak,
		nullTime(entry.LastFailureAt), nullTime(entry.CooldownUntil), entry.UpdatedAt)
	return err
}

const entryColumns = `account_alias, model_alias, provider, availability, max_concurrency,
 quota_remaining, quota_exhausted, failure_streak, last_failure_at, cooldown_until, updated_at`

type rowScanner interface{ Scan(...any) error }

// scanEntry reads entryColumns followed by the in-flight count.
func scanEntry(row rowScanner) (llmkit.ProviderHealthEntry, error) {
	var entry llmkit.ProviderHealthEntry
	var availability string
	var lastFailureAt, cooldownUntil sql.NullTime
	if err := row.Scan(&entry.AccountAlias, &entry.ModelAlias, &entry.Provider, &availability,
		&entry.MaxConcurrency, &entry.QuotaRemaining, &entry.QuotaExhausted, &entry.FailureStreak,
		&lastFailureAt, &cooldownUntil, &entry.UpdatedAt, &entry.InFlight); err != nil {
		return llmkit.ProviderHealthEntry{}, err
	}
	entry.Availability = llmkit.ProviderAvailability(availability)
	if lastFailureAt.Valid {
		entry.LastFailureAt = lastFailureAt.Time.UTC()
	}
	if cooldownUntil.Valid {
		entry.CooldownUntil = cooldownUntil.Time.UTC()
	}
	entry.UpdatedAt = entry.UpdatedAt.UTC()
	return entry, nil
}

// refresh reloads the cached snapshot. A refresh that finishes after a later
// one started is discarded, so the cache never goes back in time.
func (s *Store) refresh(ctx context.Context) error {
	seq := s.refreshSeq.Add(1)
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+`,
 (SELECT count(*) FROM llmkit_provider_health_leases l WHERE l.key = h.key AND l.expires_at > $1)
FROM llmkit_provider_health h`, now)
	if err != nil {
		return wrap("refresh", err)
	}
	defer rows.Close()
	entries := map[string]llmkit.ProviderHealthEntry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return wrap("refresh", err)
		}
		entries[llmkit.ProviderHealthKey(entry.AccountAlias, entry.ModelAlias, entry.Provider)] = entry
	}
	if err := rows.Err(); err != nil {
		return wrap("refresh", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.appliedSeq {
		s.appliedSeq, s.cachedAt, s.cache = seq, now, entries
	}
	return nil
}

// refreshAfterWrite shows this Store's write in Snapshot right away. A failed
// refresh keeps the previous view until the next background refresh.
func (s *Store) refreshAfterWrite(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	_ = s.refresh(ctx)
}

// renew extends this Store's unexpired leases. An expired lease is not
// revived: another replica may already have counted its slot as free.
func (s *Store) renew(ctx context.Context) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `UPDATE llmkit_provider_health_leases SET expires_at = $3
WHERE owner = $1 AND expires_at > $2`, s.owner, now, now.Add(s.cfg.LeaseTTL))
	return wrap("renew", err)
}

// every runs work each interval until ctx is done, giving each run at most
// one interval (and no less than refreshTimeout) to finish.
func (s *Store) every(ctx context.Context, interval time.Duration, work func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, max(interval, refreshTimeout))
			_ = work(runCtx)
			cancel()
		}
	}
}

// now is the policy's clock in UTC, truncated to PostgreSQL's microsecond
// precision so stored times compare equal to the ones computed here.
func (s *Store) now() time.Time {
	if s.cfg.Policy.Now != nil {
		return s.cfg.Policy.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("pghealth: %s: %w", op, err)
}
