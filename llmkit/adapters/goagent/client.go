// Package goagent adapts llmkit routing to goagent's LLMClient port.
package goagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

var _ ports.StreamingLLMClient = (*Client)(nil)

const providerOutcomeCleanupTimeout = 2 * time.Second

// ProviderClient is the provider-side chat client selected by route alias.
type ProviderClient interface {
	Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error)
}

// ProviderRequestDigester 暴露 Provider 实际发送的请求 body 摘要；
// Provider 不支持时，pre-dispatch hook 必须 fail closed。
type ProviderRequestDigester interface {
	ProviderRequestSHA256(ports.ChatRequest) ([sha256.Size]byte, error)
}

// ProviderStreamRequestDigester is ProviderRequestDigester for the body a
// streaming provider's ChatStream sends.
type ProviderStreamRequestDigester interface {
	ProviderStreamRequestSHA256(ports.ChatRequest) ([sha256.Size]byte, error)
}

// ProfileProvider supplies task routing metadata for a goagent request.
type ProfileProvider func(context.Context, ports.ChatRequest) llmkit.TaskProfile

// RouteMetadataProvider supplies allowlisted trace identifiers.
type RouteMetadataProvider func(context.Context, ports.ChatRequest) RouteMetadata

// ModelStatsProvider supplies fresh model statistics before each route decision.
type ModelStatsProvider func(context.Context) (*llmkit.ModelStats, error)

// ErrorClassifier maps provider errors to llmkit's stable fallback/audit classes.
type ErrorClassifier func(error) llmkit.ErrorClass

// RouteMetadata contains host-provided trace identifiers.
type RouteMetadata struct {
	RouteID string
	TaskID  string
	Attempt int
}

// PreDispatchMetadataProvider 为每次 Provider 尝试提供 Host 所有的 dispatch 标识，
// 同时不改变 v0.1.0 RouteMetadata 合同。
type PreDispatchMetadataProvider func(context.Context, ports.ChatRequest) PreDispatchMetadata

// PreDispatchMetadata 标识一次由 Host 所有的 Provider claim。
type PreDispatchMetadata struct {
	ProviderCallIndex int
	RunRequestSHA256  [sha256.Size]byte
}

// PreDispatchRequest 只包含白名单路由数据和请求摘要，
// 有意不暴露 ChatRequest 或凭据字段。
type PreDispatchRequest struct {
	Route                 llmkit.RouteTrace
	ProviderCallIndex     int
	Attempt               int
	ProviderClass         string
	RunRequestSHA256      [sha256.Size]byte
	ProviderRequestSHA256 [sha256.Size]byte
}

// PreDispatchHook 在路由和请求摘要构造后执行：此时 HealthStore 已经 Acquire
// 了这次调用的名额，Provider 还没有被调用。Hook 返回错误时名额会被释放。
type PreDispatchHook func(context.Context, PreDispatchRequest) error

// PreDispatchConfig 启用可选的 Provider claim 边界。
type PreDispatchConfig struct {
	MetadataProvider PreDispatchMetadataProvider
	Hook             PreDispatchHook
}

// FallbackPolicy controls which provider failures move on to the next
// candidate.
type FallbackPolicy struct {
	// MaxAttempts caps provider attempts per request; <= 0 tries every
	// remaining eligible candidate.
	MaxAttempts int
	// FailClosed restricts fallback to failures the provider proved happened
	// before dispatch and whose class is in RetryableErrorClasses, so a request
	// that may have reached a provider is never sent to a second one. Without
	// it, any provider failure falls back.
	FailClosed            bool
	RetryableErrorClasses []llmkit.ErrorClass
}

// ProviderDispatchStatus 让 Provider 证明错误发生在出站前；
// 无法证明的错误按已经 dispatch 处理。
type ProviderDispatchStatus interface {
	ProviderDispatched() bool
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
	ModelStatsProvider    ModelStatsProvider
	HealthStore           llmkit.HealthStore
	FallbackPolicy        FallbackPolicy
	ErrorClassifier       ErrorClassifier
	// PreDispatch installs a host claim boundary before each provider call.
	// It implies FallbackPolicy.FailClosed.
	PreDispatch PreDispatchConfig
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
	modelStatsProvider    ModelStatsProvider
	healthStore           llmkit.HealthStore
	preDispatchMetadata   PreDispatchMetadataProvider
	preDispatchHook       PreDispatchHook
	maxAttempts           int
	failClosed            bool
	retryableErrorClass   []llmkit.ErrorClass
	errorClassifier       ErrorClassifier
}

// NewClient creates a goagent LLMClient adapter.
func NewClient(config Config) *Client {
	providers := make(map[string]ProviderClient, len(config.Providers))
	for alias, provider := range config.Providers {
		providers[alias] = provider
	}

	candidates := make([]llmkit.Candidate, len(config.Candidates))
	copy(candidates, config.Candidates)
	errorClassifier := config.ErrorClassifier
	if errorClassifier == nil {
		errorClassifier = DefaultErrorClassifier
	}

	return &Client{
		policy:                config.Policy,
		candidates:            candidates,
		providers:             providers,
		profileProvider:       config.ProfileProvider,
		routeMetadataProvider: config.RouteMetadataProvider,
		recorder:              config.Recorder,
		recordOutcomes:        config.RecordOutcomes,
		modelStats:            config.ModelStats,
		modelStatsProvider:    config.ModelStatsProvider,
		healthStore:           config.HealthStore,
		preDispatchMetadata:   config.PreDispatch.MetadataProvider,
		preDispatchHook:       config.PreDispatch.Hook,
		maxAttempts:           config.FallbackPolicy.MaxAttempts,
		failClosed:            config.FallbackPolicy.FailClosed || config.PreDispatch.Hook != nil,
		retryableErrorClass:   slices.Clone(config.FallbackPolicy.RetryableErrorClasses),
		errorClassifier:       errorClassifier,
	}
}

// Chat selects a route, records the route trace, and delegates to the selected
// provider client.
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	return c.route(ctx, req, nil)
}

// ChatStream routes like Chat and delivers the selected provider's output to
// onDelta: streamed when the provider implements ports.StreamingLLMClient,
// otherwise as one delta of the complete response. An attempt that already
// delivered output never falls back, so the caller never sees two answers.
func (c *Client) ChatStream(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	return c.route(ctx, req, onDelta)
}

func (c *Client) route(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	profile := llmkit.DefaultTaskProfile()
	if c.profileProvider != nil {
		profile = c.profileProvider(ctx, req)
	}

	candidates, err := c.availableCandidates(ctx, profile)
	if err != nil {
		return nil, newRuntimeError(ErrorStageRoute, c.errorClassifier(err), false, err)
	}
	var failures []error
	var routeMetadata RouteMetadata
	routeMetadataLoaded := false
	lastProviderCallIndex := 0
	maxAttempts := c.attemptLimit(len(candidates))
	for attemptOffset := 0; len(candidates) > 0; {
		if maxAttempts > 0 && attemptOffset >= maxAttempts {
			break
		}
		decision, err := c.policy.Select(profile, candidates)
		if err != nil {
			return nil, joinRouteErrors(
				newRuntimeError(ErrorStageRoute, llmkit.ErrorClassPolicyBlocked, false, err),
				failures,
			)
		}
		provider, ok := c.providers[decision.SelectedAlias]
		if !ok || provider == nil {
			err := fmt.Errorf("missing provider client for selected model alias %q", decision.SelectedAlias)
			return nil, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, err)
		}
		if c.healthStore != nil {
			if err := c.healthStore.Acquire(ctx, decision.Selected); err != nil {
				if !errors.Is(err, llmkit.ErrProviderBusy) {
					return nil, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
				}
				// Another request took the last slot or the probe since the
				// snapshot: nothing was sent, so try the next candidate without
				// spending an attempt.
				failures = append(failures, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassProviderBusy, false, err))
				candidates = removeSelectedCandidate(candidates, decision.Selected)
				continue
			}
		}

		resp, delivered, err := c.attempt(ctx, req, onDelta, profile, decision, provider, attemptOffset, &routeMetadata, &routeMetadataLoaded, &lastProviderCallIndex)
		if err == nil {
			return resp, nil
		}
		var runtimeErr *RuntimeError
		if !errors.As(err, &runtimeErr) || runtimeErr.Stage != ErrorStageProvider {
			return nil, err
		}
		failures = append(failures, runtimeErr)
		if ctx.Err() != nil || delivered || !c.allowsFallback(runtimeErr) {
			return nil, joinRouteErrors(runtimeErr, failures[:len(failures)-1])
		}
		candidates = removeSelectedCandidate(candidates, decision.Selected)
		attemptOffset++
	}

	if len(failures) > 0 {
		last := failures[len(failures)-1]
		return nil, joinRouteErrors(last, failures[:len(failures)-1])
	}
	return nil, newRuntimeError(
		ErrorStageRoute,
		llmkit.ErrorClassPolicyBlocked,
		false,
		fmt.Errorf("all provider candidates failed"),
	)
}

// attempt makes one admitted provider call. It owns the health slot
// acquired for it: the slot is closed by the outcome once one is recorded, or
// released if the attempt stops before that, including when the provider or
// the stream callback panics.
func (c *Client) attempt(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error, profile llmkit.TaskProfile, decision llmkit.RouteDecision, provider ProviderClient, attemptOffset int, routeMetadata *RouteMetadata, routeMetadataLoaded *bool, lastProviderCallIndex *int) (*ports.ChatResponse, bool, error) {
	settled := false
	defer func() {
		if !settled && c.healthStore != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerOutcomeCleanupTimeout)
			defer cancel()
			_ = c.healthStore.Release(cleanupCtx, decision.Selected)
		}
	}()

	var trace llmkit.RouteTrace
	if c.recorder != nil || c.preDispatchHook != nil {
		if !*routeMetadataLoaded {
			metadata, err := c.loadRouteMetadata(ctx, req)
			if err != nil {
				return nil, false, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassConfiguration, false, err)
			}
			*routeMetadata = metadata
			*routeMetadataLoaded = true
		}
		trace = c.routeTrace(profile, decision, *routeMetadata, attemptOffset)
		if c.recorder != nil {
			if err := c.recorder.RecordRoute(ctx, trace); err != nil {
				return nil, false, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
			}
		}
	}

	streaming, streams := provider.(ports.StreamingLLMClient)
	streams = streams && onDelta != nil
	if c.preDispatchHook != nil {
		// claim 未明确成功前不能调用 Provider 或进入 fallback。
		providerCallIndex, err := c.runPreDispatchHook(ctx, req, provider, streams, trace, *lastProviderCallIndex)
		if err != nil {
			return nil, false, err
		}
		*lastProviderCallIndex = providerCallIndex
	}

	callStarted := time.Now()
	delivered := false
	var resp *ports.ChatResponse
	var providerErr error
	var consumerErr error
	if streams {
		resp, providerErr = streaming.ChatStream(ctx, req, func(delta ports.ChatDelta) error {
			delivered = true
			if err := onDelta(delta); err != nil {
				consumerErr = err
				return err
			}
			return nil
		})
	} else {
		resp, providerErr = provider.Chat(ctx, req)
	}
	latency := time.Since(callStarted)
	if requestErr := ctx.Err(); requestErr != nil {
		// 取消优先于晚到 Provider 结果，完整响应不得继续向上返回。
		resp = nil
		providerErr = requestErr
	}
	if consumerErr != nil && providerErr != nil {
		// The caller stopped reading; the provider did not fail.
		settled = true
		if err := c.recordOutcomeAfterCall(ctx, profile, decision, trace, latency, nil, newRuntimeError(ErrorStageProvider, llmkit.ErrorClassStreamConsumer, true, consumerErr)); err != nil {
			return nil, delivered, newRuntimeError(ErrorStagePostCall, llmkit.ErrorClassPostCall, true, err)
		}
		return nil, delivered, newRuntimeError(ErrorStageProvider, llmkit.ErrorClassStreamConsumer, true, consumerErr)
	}
	if providerErr == nil {
		settled = true
		if err := c.recordOutcomeAfterCall(ctx, profile, decision, trace, latency, resp, nil); err != nil {
			return nil, delivered, newRuntimeError(ErrorStagePostCall, llmkit.ErrorClassPostCall, true, err)
		}
		if onDelta != nil && !streams {
			for _, delta := range ports.ResponseDeltas(resp) {
				if err := onDelta(delta); err != nil {
					return nil, true, err
				}
			}
		}
		return resp, delivered, nil
	}
	settled = true
	if err := c.recordOutcomeAfterCall(ctx, profile, decision, trace, latency, nil, providerErr); err != nil {
		return nil, delivered, newRuntimeError(ErrorStagePostCall, llmkit.ErrorClassPostCall, true, err)
	}
	return nil, delivered, newRuntimeError(
		ErrorStageProvider,
		c.errorClassifier(providerErr),
		providerWasDispatched(providerErr),
		providerErr,
	)
}

func (c *Client) recordOutcomeAfterCall(requestCtx context.Context, profile llmkit.TaskProfile, decision llmkit.RouteDecision, trace llmkit.RouteTrace, latency time.Duration, resp *ports.ChatResponse, providerErr error) error {
	// Provider 返回后不能复用可能已取消的请求 context，否则已开始的 health 计数无法归零。
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), providerOutcomeCleanupTimeout)
	defer cancel()
	return c.recordOutcome(cleanupCtx, profile, decision, trace, latency, resp, providerErr)
}

func (c *Client) recordOutcome(ctx context.Context, profile llmkit.TaskProfile, decision llmkit.RouteDecision, trace llmkit.RouteTrace, latency time.Duration, resp *ports.ChatResponse, providerErr error) error {
	outcome := llmkit.TaskOutcome{
		RouteID:      trace.RouteID,
		TaskID:       trace.TaskID,
		Attempt:      trace.Attempt,
		TaskType:     profile.TaskType,
		AccountAlias: decision.Selected.AccountAlias,
		ModelAlias:   decision.SelectedAlias,
		Provider:     decision.Selected.Model.Provider,
		Success:      providerErr == nil,
	}
	if latency > 0 {
		// Round up so a sub-millisecond call still counts as a latency sample.
		outcome.LatencyMillis = int((latency + time.Millisecond - 1) / time.Millisecond)
	}
	if resp != nil {
		outcome.InputTokens = resp.Usage.InputTokens
		outcome.OutputTokens = resp.Usage.OutputTokens
	}
	if providerErr != nil {
		outcome.ErrorCode = "provider_error"
		// A class the adapter already assigned, such as a failed stream
		// consumer, wins over the host's classifier.
		var runtimeErr *RuntimeError
		if errors.As(providerErr, &runtimeErr) {
			outcome.ErrorClass = runtimeErr.Class
		} else if c.errorClassifier != nil {
			outcome.ErrorClass = c.errorClassifier(providerErr)
		}
	}
	if c.healthStore != nil {
		if err := c.healthStore.RecordOutcome(ctx, outcome); err != nil {
			return err
		}
	}
	if c.recorder == nil || !c.recordOutcomes {
		return nil
	}
	return c.recorder.RecordOutcome(ctx, outcome)
}

func (c *Client) availableCandidates(ctx context.Context, profile llmkit.TaskProfile) ([]llmkit.Candidate, error) {
	if len(c.candidates) == 0 {
		return nil, nil
	}
	available := make([]llmkit.Candidate, 0, len(c.candidates))
	for _, candidate := range c.candidates {
		provider := c.providers[candidate.Model.Alias]
		if provider == nil {
			continue
		}
		available = append(available, candidate)
	}
	stats := c.modelStats
	if c.modelStatsProvider != nil {
		loaded, err := c.modelStatsProvider(ctx)
		if err != nil {
			return nil, err
		}
		stats = loaded
	}
	if stats != nil {
		available = llmkit.ApplyModelStats(*stats, profile, available)
	}
	if c.healthStore != nil {
		available = llmkit.ApplyProviderHealth(c.healthStore.Snapshot(), available)
	}
	return available, nil
}

func (c *Client) loadRouteMetadata(ctx context.Context, req ports.ChatRequest) (RouteMetadata, error) {
	metadata := RouteMetadata{}
	if c.routeMetadataProvider != nil {
		metadata = c.routeMetadataProvider(ctx, req)
	}
	if err := validateRouteMetadata(metadata); err != nil {
		return RouteMetadata{}, err
	}
	return metadata, nil
}

func (c *Client) loadPreDispatchMetadata(ctx context.Context, req ports.ChatRequest) (PreDispatchMetadata, error) {
	if c.preDispatchMetadata == nil {
		return PreDispatchMetadata{}, fmt.Errorf("pre-dispatch metadata provider is required")
	}
	metadata := c.preDispatchMetadata(ctx, req)
	if metadata.ProviderCallIndex <= 0 {
		return PreDispatchMetadata{}, fmt.Errorf("provider_call_index must be greater than zero")
	}
	if metadata.RunRequestSHA256 == ([sha256.Size]byte{}) {
		return PreDispatchMetadata{}, fmt.Errorf("run request SHA-256 is required")
	}
	return metadata, nil
}

func (c *Client) routeTrace(profile llmkit.TaskProfile, decision llmkit.RouteDecision, metadata RouteMetadata, attemptOffset int) llmkit.RouteTrace {
	return llmkit.RouteTrace{
		RouteID:               metadata.RouteID,
		TaskID:                metadata.TaskID,
		Attempt:               metadata.Attempt + attemptOffset,
		TaskType:              profile.TaskType,
		TaskProfile:           copyTaskProfile(profile),
		AccountAlias:          decision.Selected.AccountAlias,
		ModelAlias:            decision.SelectedAlias,
		Provider:              decision.Selected.Model.Provider,
		Selected:              true,
		Reason:                decision.Reason,
		Score:                 decision.Score,
		ScoreBreakdown:        copyScoreBreakdown(decision.ScoreBreakdown),
		CandidateModelAliases: candidateModelAliases(decision.Candidates),
		Candidates:            copyCandidateScores(decision.Candidates),
		FallbackMaxAttempts:   c.maxAttempts,
	}
}

func (c *Client) runPreDispatchHook(ctx context.Context, req ports.ChatRequest, provider ProviderClient, stream bool, trace llmkit.RouteTrace, lastProviderCallIndex int) (int, error) {
	providerDigest, err := providerRequestSHA256(provider, req, stream)
	if err != nil {
		var runtimeErr *RuntimeError
		if errors.As(err, &runtimeErr) {
			return 0, runtimeErr
		}
		return 0, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
	}
	if providerDigest == ([sha256.Size]byte{}) {
		return 0, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassConfiguration, false, fmt.Errorf("provider request SHA-256 is required"))
	}
	metadata, err := c.loadPreDispatchMetadata(ctx, req)
	if err != nil {
		return 0, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassConfiguration, false, err)
	}
	if metadata.ProviderCallIndex <= lastProviderCallIndex {
		return 0, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassConfiguration, false, fmt.Errorf("provider_call_index must increase across attempts"))
	}
	hookRequest := PreDispatchRequest{
		Route:                 trace,
		ProviderCallIndex:     metadata.ProviderCallIndex,
		Attempt:               trace.Attempt,
		ProviderClass:         trace.Provider,
		RunRequestSHA256:      metadata.RunRequestSHA256,
		ProviderRequestSHA256: providerDigest,
	}
	if err := c.preDispatchHook(ctx, hookRequest); err != nil {
		return 0, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
	}
	return metadata.ProviderCallIndex, nil
}

func (c *Client) attemptLimit(candidateCount int) int {
	if c.maxAttempts <= 0 {
		return 0
	}
	return min(c.maxAttempts, candidateCount)
}

func (c *Client) allowsFallback(err *RuntimeError) bool {
	if !c.failClosed {
		return true
	}
	if err == nil || err.ProviderDispatched {
		return false
	}
	return slices.Contains(c.retryableErrorClass, err.Class)
}

func providerRequestSHA256(provider ProviderClient, req ports.ChatRequest, stream bool) ([sha256.Size]byte, error) {
	if stream {
		digester, ok := provider.(ProviderStreamRequestDigester)
		if !ok {
			return [sha256.Size]byte{}, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, fmt.Errorf("provider stream request digester is required when pre-dispatch hook is configured"))
		}
		return digester.ProviderStreamRequestSHA256(req)
	}
	digester, ok := provider.(ProviderRequestDigester)
	if !ok {
		return [sha256.Size]byte{}, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, fmt.Errorf("provider request digester is required when pre-dispatch hook is configured"))
	}
	return digester.ProviderRequestSHA256(req)
}

func providerWasDispatched(err error) bool {
	var status ProviderDispatchStatus
	if errors.As(err, &status) {
		return status.ProviderDispatched()
	}
	return true
}

func copyTaskProfile(profile llmkit.TaskProfile) *llmkit.TaskProfile {
	copied := profile
	return &copied
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

func copyCandidateScores(in []llmkit.CandidateScore) []llmkit.CandidateScore {
	if in == nil {
		return nil
	}
	out := make([]llmkit.CandidateScore, len(in))
	for i, score := range in {
		out[i] = llmkit.CandidateScore{
			Alias:          score.Alias,
			AccountAlias:   score.AccountAlias,
			Available:      score.Available,
			Score:          score.Score,
			ScoreBreakdown: copyScoreBreakdown(score.ScoreBreakdown),
			Reason:         score.Reason,
		}
	}
	return out
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
