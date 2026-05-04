// Package goagent adapts llmkit routing to goagent's LLMClient port.
package goagent

import (
	"context"
	"fmt"

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
}

// Client implements goagent's LLMClient by routing to a provider client.
type Client struct {
	policy                llmkit.RoutePolicy
	candidates            []llmkit.Candidate
	providers             map[string]ProviderClient
	profileProvider       ProfileProvider
	routeMetadataProvider RouteMetadataProvider
	recorder              llmkit.Recorder
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
	}
}

// Chat selects a route, records the route trace, and delegates to the selected
// provider client.
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	profile := llmkit.DefaultTaskProfile()
	if c.profileProvider != nil {
		profile = c.profileProvider(ctx, req)
	}

	decision, err := c.policy.Select(profile, c.candidates)
	if err != nil {
		return nil, err
	}

	if c.recorder != nil {
		if err := c.recorder.RecordRoute(ctx, c.routeTrace(ctx, req, profile, decision)); err != nil {
			return nil, err
		}
	}

	provider, ok := c.providers[decision.SelectedAlias]
	if !ok || provider == nil {
		return nil, fmt.Errorf("missing provider client for selected model alias %q", decision.SelectedAlias)
	}
	return provider.Chat(ctx, req)
}

func (c *Client) routeTrace(ctx context.Context, req ports.ChatRequest, profile llmkit.TaskProfile, decision llmkit.RouteDecision) llmkit.RouteTrace {
	metadata := RouteMetadata{}
	if c.routeMetadataProvider != nil {
		metadata = c.routeMetadataProvider(ctx, req)
	}
	return llmkit.RouteTrace{
		RouteID:               metadata.RouteID,
		TaskID:                metadata.TaskID,
		Attempt:               metadata.Attempt,
		TaskType:              profile.TaskType,
		AccountAlias:          decision.Selected.AccountAlias,
		ModelAlias:            decision.SelectedAlias,
		Provider:              decision.Selected.Model.Provider,
		Selected:              true,
		Reason:                decision.Reason,
		Score:                 decision.Score,
		ScoreBreakdown:        copyScoreBreakdown(decision.ScoreBreakdown),
		CandidateModelAliases: candidateModelAliases(decision.Candidates),
	}
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
