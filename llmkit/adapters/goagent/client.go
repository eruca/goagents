// Package goagent adapts llmkit routing to goagent's LLMClient port.
package goagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/llmkit/llmkit"
)

var _ ports.LLMClient = (*Client)(nil)

// ProviderClient is the provider-side chat client selected by route alias.
type ProviderClient interface {
	Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error)
}

// ProfileProvider supplies task routing metadata for a goagent request.
type ProfileProvider func(context.Context, ports.ChatRequest) llmkit.TaskProfile

// RouteMetadataProvider supplies allowlisted trace identifiers.
type RouteMetadataProvider func(context.Context, ports.ChatRequest) RouteMetadata

// RouteMetadata contains host-provided trace identifiers.
type RouteMetadata struct {
	RouteID string
	TaskID  string
	Attempt int
}

// Config configures the goagent adapter.
type Config struct {
	Policy                llmkit.RoutePolicy
	Candidates            []llmkit.Candidate
	Providers             map[string]ProviderClient
	ProfileProvider       ProfileProvider
	RouteMetadataProvider RouteMetadataProvider
	Recorder              llmkit.Recorder
	RecordOutcomes        bool
	ModelStats            *llmkit.ModelStats
}

// Client implements goagent's LLMClient by routing to a provider client.
type Client struct {
	policy                llmkit.RoutePolicy
	candidates            []llmkit.Candidate
	providers             map[string]ProviderClient
	profileProvider       ProfileProvider
	routeMetadataProvider RouteMetadataProvider
	recorder              llmkit.Recorder
	recordOutcomes        bool
	modelStats            *llmkit.ModelStats
}

// NewClient creates a goagent LLMClient adapter.
func NewClient(config Config) *Client {
	providers := make(map[string]ProviderClient, len(config.Providers))
	for alias, provider := range config.Providers {
		providers[alias] = provider
	}

	candidates := make([]llmkit.Candidate, len(config.Candidates))
	copy(candidates, config.Candidates)

	return &Client{
		policy:                config.Policy,
		candidates:            candidates,
		providers:             providers,
		profileProvider:       config.ProfileProvider,
		routeMetadataProvider: config.RouteMetadataProvider,
		recorder:              config.Recorder,
		recordOutcomes:        config.RecordOutcomes,
		modelStats:            config.ModelStats,
	}
}

// Chat selects a route, records the route trace, and delegates to the selected
// provider client.
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	profile := llmkit.DefaultTaskProfile()
	if c.profileProvider != nil {
		profile = c.profileProvider(ctx, req)
	}

	candidates := c.availableCandidates(profile)
	var failures []error
	for attemptOffset := 0; len(candidates) > 0; attemptOffset++ {
		decision, err := c.policy.Select(profile, candidates)
		if err != nil {
			return nil, joinRouteErrors(err, failures)
		}

		var trace llmkit.RouteTrace
		if c.recorder != nil {
			trace, err = c.routeTrace(ctx, req, profile, decision, attemptOffset)
			if err != nil {
				return nil, err
			}
			if err := c.recorder.RecordRoute(ctx, trace); err != nil {
				return nil, err
			}
		}

		provider, ok := c.providers[decision.SelectedAlias]
		if !ok || provider == nil {
			failures = append(failures, fmt.Errorf("missing provider client for selected model alias %q", decision.SelectedAlias))
		} else {
			resp, err := provider.Chat(ctx, req)
			if err == nil {
				if err := c.recordOutcome(ctx, trace, resp, nil); err != nil {
					return nil, err
				}
				return resp, nil
			}
			if err := c.recordOutcome(ctx, trace, nil, err); err != nil {
				return nil, err
			}
			failures = append(failures, fmt.Errorf("provider %q failed: %w", decision.SelectedAlias, err))
		}

		candidates = removeSelectedCandidate(candidates, decision.Selected)
	}

	return nil, joinRouteErrors(fmt.Errorf("all provider candidates failed"), failures)
}

func (c *Client) recordOutcome(ctx context.Context, trace llmkit.RouteTrace, resp *ports.ChatResponse, providerErr error) error {
	if c.recorder == nil || !c.recordOutcomes {
		return nil
	}
	outcome := llmkit.TaskOutcome{
		RouteID:      trace.RouteID,
		TaskID:       trace.TaskID,
		Attempt:      trace.Attempt,
		TaskType:     trace.TaskType,
		AccountAlias: trace.AccountAlias,
		ModelAlias:   trace.ModelAlias,
		Provider:     trace.Provider,
		Success:      providerErr == nil,
	}
	if resp != nil {
		outcome.InputTokens = resp.Usage.InputTokens
		outcome.OutputTokens = resp.Usage.OutputTokens
	}
	if providerErr != nil {
		outcome.ErrorCode = "provider_error"
	}
	return c.recorder.RecordOutcome(ctx, outcome)
}

func (c *Client) availableCandidates(profile llmkit.TaskProfile) []llmkit.Candidate {
	if len(c.candidates) == 0 {
		return nil
	}
	available := make([]llmkit.Candidate, 0, len(c.candidates))
	for _, candidate := range c.candidates {
		provider := c.providers[candidate.Model.Alias]
		if provider == nil {
			continue
		}
		available = append(available, candidate)
	}
	if c.modelStats != nil {
		available = llmkit.ApplyModelStats(*c.modelStats, profile, available)
	}
	return available
}

func (c *Client) routeTrace(ctx context.Context, req ports.ChatRequest, profile llmkit.TaskProfile, decision llmkit.RouteDecision, attemptOffset int) (llmkit.RouteTrace, error) {
	metadata := RouteMetadata{}
	if c.routeMetadataProvider != nil {
		metadata = c.routeMetadataProvider(ctx, req)
	}
	if err := validateRouteMetadata(metadata); err != nil {
		return llmkit.RouteTrace{}, err
	}
	return llmkit.RouteTrace{
		RouteID:               metadata.RouteID,
		TaskID:                metadata.TaskID,
		Attempt:               metadata.Attempt + attemptOffset,
		TaskType:              profile.TaskType,
		AccountAlias:          decision.Selected.AccountAlias,
		ModelAlias:            decision.SelectedAlias,
		Provider:              decision.Selected.Model.Provider,
		Selected:              true,
		Reason:                decision.Reason,
		Score:                 decision.Score,
		ScoreBreakdown:        copyScoreBreakdown(decision.ScoreBreakdown),
		CandidateModelAliases: candidateModelAliases(decision.Candidates),
	}, nil
}

func removeSelectedCandidate(candidates []llmkit.Candidate, selected llmkit.Candidate) []llmkit.Candidate {
	return slices.DeleteFunc(candidates, func(candidate llmkit.Candidate) bool {
		return candidate.Model.Alias == selected.Model.Alias &&
			candidate.AccountAlias == selected.AccountAlias &&
			candidate.Model.Provider == selected.Model.Provider
	})
}

func joinRouteErrors(primary error, failures []error) error {
	if len(failures) == 0 {
		return primary
	}
	all := make([]error, 0, len(failures)+1)
	all = append(all, primary)
	all = append(all, failures...)
	return errors.Join(all...)
}

func validateRouteMetadata(metadata RouteMetadata) error {
	if strings.TrimSpace(metadata.RouteID) == "" {
		return fmt.Errorf("route_id is required")
	}
	if strings.TrimSpace(metadata.TaskID) == "" {
		return fmt.Errorf("task_id is required")
	}
	if metadata.Attempt <= 0 {
		return fmt.Errorf("attempt must be greater than zero")
	}
	return nil
}

func candidateModelAliases(candidates []llmkit.CandidateScore) []string {
	if len(candidates) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Alias == "" {
			continue
		}
		aliases = append(aliases, candidate.Alias)
	}
	return aliases
}

func copyScoreBreakdown(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
