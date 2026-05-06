package llmkit

import (
	"context"
	"strings"
	"sync"
	"time"
)

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
	CooldownUntil  time.Time            `json:"cooldown_until,omitempty"`
	UpdatedAt      time.Time            `json:"updated_at,omitempty"`
}

type ProviderHealthSnapshot struct {
	GeneratedAt time.Time                      `json:"generated_at,omitempty"`
	Entries     map[string]ProviderHealthEntry `json:"entries,omitempty"`
}

type HealthPolicy struct {
	FailureCooldownThreshold int
	CooldownDuration         time.Duration
	Now                      func() time.Time
}

type HealthStore interface {
	Begin(context.Context, Candidate) error
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
	for i := range enriched {
		key := ProviderHealthKey(enriched[i].AccountAlias, enriched[i].Model.Alias, enriched[i].Model.Provider)
		entry, ok := snapshot.Entries[key]
		if !ok {
			continue
		}
		enriched[i].ProviderAvailability = normalizedAvailability(entry.Availability)
		enriched[i].ProviderInFlight = entry.InFlight
		enriched[i].ProviderMaxConcurrency = entry.MaxConcurrency
		enriched[i].ProviderQuotaRemaining = entry.QuotaRemaining
		enriched[i].ProviderQuotaExhausted = entry.QuotaExhausted
		enriched[i].ProviderFailureStreak = entry.FailureStreak
		enriched[i].ProviderCooldownUntil = entry.CooldownUntil
	}
	return enriched
}

func (s *MemoryHealthStore) Begin(ctx context.Context, candidate Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := ProviderHealthKey(candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	entry := s.entries[key]
	entry = ensureEntryIdentity(entry, candidate.AccountAlias, candidate.Model.Alias, candidate.Model.Provider)
	entry.Availability = normalizedAvailability(entry.Availability)
	entry.InFlight++
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
	if outcome.Success {
		entry.FailureStreak = 0
		entry.Availability = ProviderAvailable
		entry.CooldownUntil = time.Time{}
	} else {
		entry.FailureStreak++
		if entry.FailureStreak >= s.policy.FailureCooldownThreshold {
			entry.Availability = ProviderUnavailable
			entry.CooldownUntil = now.Add(s.policy.CooldownDuration)
		} else {
			entry.Availability = ProviderDegraded
		}
	}
	entry.UpdatedAt = now
	s.entries[key] = entry
	return nil
}

func (s *MemoryHealthStore) Snapshot() ProviderHealthSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make(map[string]ProviderHealthEntry, len(s.entries))
	for key, entry := range s.entries {
		entries[key] = entry
	}
	return ProviderHealthSnapshot{
		GeneratedAt: s.now(),
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
