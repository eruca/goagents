package llmkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrProviderBusy is wrapped by HealthStore.Acquire when a provider cannot
// take another call right now. It is not a provider failure: callers move on
// to another candidate and record no outcome.
var ErrProviderBusy = errors.New("provider cannot take another call now")

type ProviderAvailability string

const (
	ProviderAvailable   ProviderAvailability = "available"
	ProviderDegraded    ProviderAvailability = "degraded"
	ProviderUnavailable ProviderAvailability = "unavailable"
)

type ProviderHealthEntry struct {
	AccountAlias   string               `json:"account_alias,omitempty"`
	ModelAlias     string               `json:"model_alias,omitempty"`
	Provider       string               `json:"provider,omitempty"`
	Availability   ProviderAvailability `json:"availability,omitempty"`
	InFlight       int                  `json:"in_flight,omitempty"`
	MaxConcurrency int                  `json:"max_concurrency,omitempty"`
	QuotaRemaining int                  `json:"quota_remaining,omitempty"`
	QuotaExhausted bool                 `json:"quota_exhausted,omitempty"`
	FailureStreak  int                  `json:"failure_streak,omitempty"`
	LastFailureAt  time.Time            `json:"last_failure_at,omitempty"`
	CooldownUntil  time.Time            `json:"cooldown_until,omitempty"`
	UpdatedAt      time.Time            `json:"updated_at,omitempty"`
}

type ProviderHealthSnapshot struct {
	GeneratedAt time.Time                      `json:"generated_at,omitempty"`
	Entries     map[string]ProviderHealthEntry `json:"entries,omitempty"`
}

// HealthPolicy configures the circuit breaker. Reaching
// FailureCooldownThreshold consecutive provider failures opens it for
// CooldownDuration; each further failure, such as a failed recovery probe,
// doubles the cooldown up to MaxCooldownDuration. A degraded provider whose
// last failure is CooldownDuration old is available again: being ranked below
// healthy providers, it might otherwise never get the success that clears it.
type HealthPolicy struct {
	FailureCooldownThreshold int
	CooldownDuration         time.Duration
	MaxCooldownDuration      time.Duration
	Now                      func() time.Time
}

type HealthStore interface {
	// Acquire atomically admits one call to the candidate's provider. It
	// returns an error wrapping ErrProviderBusy while the circuit is open,
	// while a recovery probe is in flight (half-open admits exactly one call),
	// when quota is exhausted, or when the provider's or the account's
	// concurrency limit is reached. Each successful Acquire is closed by
	// exactly one RecordOutcome or Release.
	Acquire(context.Context, Candidate) error
	// Release closes an Acquire whose call never reached the provider.
	Release(context.Context, Candidate) error
	RecordOutcome(context.Context, TaskOutcome) error
	Snapshot() ProviderHealthSnapshot
}

type MemoryHealthStore struct {
	mu      sync.Mutex
	policy  HealthPolicy
	entries map[string]ProviderHealthEntry
}

func NewMemoryHealthStore(policy HealthPolicy) *MemoryHealthStore {
	if policy.FailureCooldownThreshold <= 0 {
		policy.FailureCooldownThreshold = 3
	}
	if policy.CooldownDuration <= 0 {
		policy.CooldownDuration = time.Minute
	}
	if policy.MaxCooldownDuration < policy.CooldownDuration {
		policy.MaxCooldownDuration = 32 * policy.CooldownDuration
	}
	return &MemoryHealthStore{
		policy:  policy,
		entries: map[string]ProviderHealthEntry{},
	}
}

func ProviderHealthKey(accountAlias, modelAlias, provider string) string {
	return strings.TrimSpace(accountAlias) + "|" + strings.TrimSpace(modelAlias) + "|" + strings.TrimSpace(provider)
}

func ApplyProviderHealth(snapshot ProviderHealthSnapshot, candidates []Candidate) []Candidate {
	enriched := append([]Candidate(nil), candidates...)
	if len(snapshot.Entries) == 0 {
		return enriched
	}
	accountInFlight := map[string]int{}
	for _, entry := range snapshot.Entries {
		accountInFlight[entry.AccountAlias] += entry.InFlight
	}
	for i := range enriched {
		enriched[i].AccountCurrentConcurrency = accountInFlight[strings.TrimSpace(enriched[i].AccountAlias)]
		key := ProviderHealthKey(enriched[i].AccountAlias, enriched[i].Model.Alias, enriched[i].Model.Provider)
		entry, ok := snapshot.Entries[key]
		if !ok {
			continue
		}
		enriched[i].ProviderAvailability = normalizedAvailability(entry.Availability)
		enriched[i].ProviderInFlight = entry.InFlight
		enriched[i].Model.CurrentConcurrency = entry.InFlight
		enriched[i].ProviderMaxConcurrency = entry.MaxConcurrency
		enriched[i].ProviderQuotaRemaining = entry.QuotaRemaining
		enriched[i].ProviderQuotaExhausted = entry.QuotaExhausted
		enriched[i].ProviderFailureStreak = entry.FailureStreak
		enriched[i].ProviderCooldownUntil = entry.CooldownUntil
	}
	return enriched
}

func (s *MemoryHealthStore) Acquire(ctx context.Context, candidate Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	key := ProviderHealthKey(candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	entry := ensureEntryIdentity(s.entries[key], candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	entry.Availability = normalizedAvailability(entry.Availability)
	entry = s.settle(entry, now)
	if reason := s.admissionBlock(entry, candidate, now); reason != "" {
		return fmt.Errorf("%w: %s", ErrProviderBusy, reason)
	}
	entry.InFlight++
	entry.UpdatedAt = now
	s.entries[key] = entry
	return nil
}

func (s *MemoryHealthStore) admissionBlock(entry ProviderHealthEntry, candidate Candidate, now time.Time) string {
	switch {
	case !entry.CooldownUntil.IsZero() && now.Before(entry.CooldownUntil):
		return "provider is in cooldown"
	case entry.Availability == ProviderUnavailable && entry.CooldownUntil.IsZero():
		return "provider is unavailable"
	case entry.Availability == ProviderUnavailable && entry.InFlight > 0:
		return "provider recovery probe is in flight"
	case entry.QuotaExhausted || entry.QuotaRemaining < 0:
		return "provider quota is exhausted"
	}
	limit := entry.MaxConcurrency
	if limit <= 0 {
		limit = candidate.Model.MaxConcurrency
	}
	if limit > 0 && entry.InFlight >= limit {
		return "provider concurrency is full"
	}
	if candidate.AccountMaxConcurrency > 0 {
		account := strings.TrimSpace(candidate.AccountAlias)
		inFlight := 0
		for _, other := range s.entries {
			if other.AccountAlias == account {
				inFlight += other.InFlight
			}
		}
		if inFlight >= candidate.AccountMaxConcurrency {
			return "account concurrency is full"
		}
	}
	return ""
}

func (s *MemoryHealthStore) Release(_ context.Context, candidate Candidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ProviderHealthKey(candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	entry, ok := s.entries[key]
	if !ok {
		return nil
	}
	if entry.InFlight > 0 {
		entry.InFlight--
	}
	entry.UpdatedAt = s.now()
	s.entries[key] = entry
	return nil
}

func (s *MemoryHealthStore) RecordOutcome(ctx context.Context, outcome TaskOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ProviderHealthKey(outcome.AccountAlias, outcome.ModelAlias, outcome.Provider)
	entry := s.entries[key]
	entry.AccountAlias = strings.TrimSpace(outcome.AccountAlias)
	entry.ModelAlias = strings.TrimSpace(outcome.ModelAlias)
	entry.Provider = strings.TrimSpace(outcome.Provider)
	if entry.InFlight > 0 {
		entry.InFlight--
	}
	now := s.now()
	entry = s.settle(entry, now)
	if !outcome.Success && !CountsTowardProviderHealth(outcome.ErrorClass) {
		entry.UpdatedAt = now
		s.entries[key] = entry
		return nil
	}
	if outcome.Success {
		entry.FailureStreak = 0
		entry.Availability = ProviderAvailable
		entry.CooldownUntil = time.Time{}
	} else {
		entry.FailureStreak++
		entry.LastFailureAt = now
		if entry.FailureStreak >= s.policy.FailureCooldownThreshold {
			entry.Availability = ProviderUnavailable
			entry.CooldownUntil = now.Add(s.cooldown(entry.FailureStreak))
		} else {
			entry.Availability = ProviderDegraded
		}
	}
	entry.UpdatedAt = now
	s.entries[key] = entry
	return nil
}

// CountsTowardProviderHealth reports whether a failure says something about
// the provider. Failures the caller caused, such as cancelling the request or
// sending one that is too large or over budget, leave provider health alone.
// An unclassified failure counts, which keeps the conservative default.
func CountsTowardProviderHealth(class ErrorClass) bool {
	switch class {
	case ErrorClassCanceled, ErrorClassRequestTooLarge, ErrorClassBudgetExceeded,
		ErrorClassPolicyBlocked, ErrorClassCapabilityMismatch, ErrorClassPostCall,
		ErrorClassProviderBusy:
		return false
	default:
		return true
	}
}

func (s *MemoryHealthStore) Snapshot() ProviderHealthSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	entries := make(map[string]ProviderHealthEntry, len(s.entries))
	for key, entry := range s.entries {
		entries[key] = s.settle(entry, now)
	}
	return ProviderHealthSnapshot{
		GeneratedAt: now,
		Entries:     entries,
	}
}

func (s *MemoryHealthStore) Set(entry ProviderHealthEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry.Availability = normalizedAvailability(entry.Availability)
	entry.UpdatedAt = s.now()
	s.entries[ProviderHealthKey(entry.AccountAlias, entry.ModelAlias, entry.Provider)] = entry
}

// settle forgets a failure streak that ended CooldownDuration ago without
// opening the circuit, so failures far apart are not counted as consecutive.
func (s *MemoryHealthStore) settle(entry ProviderHealthEntry, now time.Time) ProviderHealthEntry {
	if entry.Availability == ProviderDegraded && !entry.LastFailureAt.IsZero() &&
		now.Sub(entry.LastFailureAt) >= s.policy.CooldownDuration {
		entry.Availability = ProviderAvailable
		entry.FailureStreak = 0
	}
	return entry
}

func (s *MemoryHealthStore) cooldown(streak int) time.Duration {
	cooldown := s.policy.CooldownDuration
	for extra := streak - s.policy.FailureCooldownThreshold; extra > 0 && cooldown < s.policy.MaxCooldownDuration; extra-- {
		cooldown *= 2
	}
	return min(cooldown, s.policy.MaxCooldownDuration)
}

func (s *MemoryHealthStore) now() time.Time {
	if s.policy.Now != nil {
		return s.policy.Now().UTC()
	}
	return time.Now().UTC()
}

func ensureEntryIdentity(entry ProviderHealthEntry, accountAlias, modelAlias, provider string) ProviderHealthEntry {
	if entry.AccountAlias == "" {
		entry.AccountAlias = strings.TrimSpace(accountAlias)
	}
	if entry.ModelAlias == "" {
		entry.ModelAlias = strings.TrimSpace(modelAlias)
	}
	if entry.Provider == "" {
		entry.Provider = strings.TrimSpace(provider)
	}
	return entry
}

func normalizedAvailability(availability ProviderAvailability) ProviderAvailability {
	if availability == "" {
		return ProviderAvailable
	}
	return availability
}
