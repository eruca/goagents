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
	"github.com/eruca/goagents/llmkit/llmkit"
)

var _ ports.LLMClient = (*Client)(nil)

const providerOutcomeCleanupTimeout = 2 * time.Second

// ProviderClient is the provider-side chat client selected by route alias.
type ProviderClient interface {
	Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error)
}

// ProviderClientWithMaxOutputTokens 是显式生成上限使用的增量 Provider 能力。
type ProviderClientWithMaxOutputTokens interface {
	ChatWithMaxOutputTokens(context.Context, ports.ChatRequest, int) (*ports.ChatResponse, error)
}

// ProviderRequestDigester 暴露 Provider 实际发送的请求 body 摘要；
// Provider 不支持时，pre-dispatch hook 必须 fail closed。
type ProviderRequestDigester interface {
	ProviderRequestSHA256(ports.ChatRequest) ([sha256.Size]byte, error)
}

// ProviderRequestDigesterWithMaxOutputTokens 计算限额 Provider 请求的精确摘要。
type ProviderRequestDigesterWithMaxOutputTokens interface {
	ProviderRequestSHA256WithMaxOutputTokens(ports.ChatRequest, int) ([sha256.Size]byte, error)
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

// PreDispatchHook 在路由和请求摘要构造后、health 计数或 Provider 调用前执行。
type PreDispatchHook func(context.Context, PreDispatchRequest) error

// PreDispatchConfig 启用可选的 Provider claim 边界。
type PreDispatchConfig struct {
	MetadataProvider PreDispatchMetadataProvider
	Hook             PreDispatchHook
}

// FallbackPolicy controls provider fallback after route-level failures.
type FallbackPolicy struct {
	MaxAttempts int
}

// RuntimeOption 在不改变已发布 Config 或 FallbackPolicy 形状的前提下，
// 配置新的 fail-closed runtime 路径。
type RuntimeOption func(*runtimeOptions)

type runtimeOptions struct {
	preDispatch         PreDispatchConfig
	retryableErrorClass []llmkit.ErrorClass
}

// WithPreDispatch 启用类型化的 Host claim 边界。
func WithPreDispatch(config PreDispatchConfig) RuntimeOption {
	return func(options *runtimeOptions) {
		options.preDispatch = config
	}
}

// WithRetryableErrorClasses 显式允许可安全 fallback 的 pre-dispatch 类别。
func WithRetryableErrorClasses(classes ...llmkit.ErrorClass) RuntimeOption {
	return func(options *runtimeOptions) {
		options.retryableErrorClass = append([]llmkit.ErrorClass(nil), classes...)
	}
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
	fallbackPolicy        FallbackPolicy
	strictFallback        bool
	retryableErrorClass   []llmkit.ErrorClass
	errorClassifier       ErrorClassifier
}

// NewClient creates a goagent LLMClient adapter.
func NewClient(config Config) *Client {
	return newClient(config, runtimeOptions{}, false)
}

// NewClientWithPreDispatch 创建带类型化 pre-dispatch hook 的 adapter。
func NewClientWithPreDispatch(config Config, preDispatch PreDispatchConfig) *Client {
	return NewRuntimeClient(config, WithPreDispatch(preDispatch))
}

// NewRuntimeClient 创建 fail-closed runtime 路径；只有显式允许且已证明未 dispatch 的
// 错误类别才能触发 fallback。
func NewRuntimeClient(config Config, options ...RuntimeOption) *Client {
	runtime := runtimeOptions{}
	for _, option := range options {
		if option != nil {
			option(&runtime)
		}
	}
	return newClient(config, runtime, true)
}

func newClient(config Config, runtime runtimeOptions, strictFallback bool) *Client {
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
		preDispatchMetadata:   runtime.preDispatch.MetadataProvider,
		preDispatchHook:       runtime.preDispatch.Hook,
		fallbackPolicy:        config.FallbackPolicy,
		strictFallback:        strictFallback,
		retryableErrorClass:   append([]llmkit.ErrorClass(nil), runtime.retryableErrorClass...),
		errorClassifier:       errorClassifier,
	}
}

// Chat selects a route, records the route trace, and delegates to the selected
// provider client.
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	return c.chat(ctx, req, 0)
}

// ChatWithMaxOutputTokens 在保持 ChatRequest v0.1.0 形状的同时路由限额请求，
// 并要求 Provider 明确支持该能力。
func (c *Client) ChatWithMaxOutputTokens(ctx context.Context, req ports.ChatRequest, maxOutputTokens int) (*ports.ChatResponse, error) {
	return c.chat(ctx, req, maxOutputTokens)
}

func (c *Client) chat(ctx context.Context, req ports.ChatRequest, maxOutputTokens int) (*ports.ChatResponse, error) {
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
	maxAttempts := c.maxAttempts(len(candidates))
	for attemptOffset := 0; len(candidates) > 0; attemptOffset++ {
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

		var trace llmkit.RouteTrace
		if c.recorder != nil || c.preDispatchHook != nil {
			if !routeMetadataLoaded {
				routeMetadata, err = c.loadRouteMetadata(ctx, req)
				if err != nil {
					return nil, newRuntimeError(ErrorStagePreCall, llmkit.ErrorClassConfiguration, false, err)
				}
				routeMetadataLoaded = true
			}
			trace = c.routeTrace(profile, decision, routeMetadata, attemptOffset)
			if c.recorder != nil {
				if err := c.recorder.RecordRoute(ctx, trace); err != nil {
					return nil, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
				}
			}
		}

		provider, ok := c.providers[decision.SelectedAlias]
		if !ok || provider == nil {
			err := fmt.Errorf("missing provider client for selected model alias %q", decision.SelectedAlias)
			return nil, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, err)
		} else {
			if maxOutputTokens != 0 {
				if _, ok := provider.(ProviderClientWithMaxOutputTokens); !ok {
					err := fmt.Errorf("provider max output tokens capability is required")
					return nil, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, err)
				}
			}
			if c.preDispatchHook != nil {
				// claim 未明确成功前不能开始 health、调用 Provider 或进入 fallback。
				providerCallIndex, err := c.runPreDispatchHook(ctx, req, maxOutputTokens, provider, trace, lastProviderCallIndex)
				if err != nil {
					return nil, err
				}
				lastProviderCallIndex = providerCallIndex
			}
			if c.healthStore != nil {
				if err := c.healthStore.Begin(ctx, decision.Selected); err != nil {
					return nil, newRuntimeError(ErrorStagePreCall, c.errorClassifier(err), false, err)
				}
			}
			resp, providerErr := callProvider(ctx, provider, req, maxOutputTokens)
			if requestErr := ctx.Err(); requestErr != nil {
				// 取消优先于晚到 Provider 结果，完整响应不得继续向上返回。
				resp = nil
				providerErr = requestErr
			}
			if providerErr == nil {
				if err := c.recordOutcomeAfterCall(ctx, profile, decision, trace, resp, nil); err != nil {
					return nil, newRuntimeError(ErrorStagePostCall, llmkit.ErrorClassPostCall, true, err)
				}
				return resp, nil
			}
			if err := c.recordOutcomeAfterCall(ctx, profile, decision, trace, nil, providerErr); err != nil {
				return nil, newRuntimeError(ErrorStagePostCall, llmkit.ErrorClassPostCall, true, err)
			}
			runtimeErr := newRuntimeError(
				ErrorStageProvider,
				c.errorClassifier(providerErr),
				providerWasDispatched(providerErr),
				providerErr,
			)
			failures = append(failures, runtimeErr)
			if ctx.Err() != nil || !c.allowsFallback(runtimeErr) {
				return nil, joinRouteErrors(runtimeErr, failures[:len(failures)-1])
			}
		}

		candidates = removeSelectedCandidate(candidates, decision.Selected)
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

func (c *Client) recordOutcomeAfterCall(requestCtx context.Context, profile llmkit.TaskProfile, decision llmkit.RouteDecision, trace llmkit.RouteTrace, resp *ports.ChatResponse, providerErr error) error {
	// Provider 返回后不能复用可能已取消的请求 context，否则已开始的 health 计数无法归零。
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), providerOutcomeCleanupTimeout)
	defer cancel()
	return c.recordOutcome(cleanupCtx, profile, decision, trace, resp, providerErr)
}

func (c *Client) recordOutcome(ctx context.Context, profile llmkit.TaskProfile, decision llmkit.RouteDecision, trace llmkit.RouteTrace, resp *ports.ChatResponse, providerErr error) error {
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
	if resp != nil {
		outcome.InputTokens = resp.Usage.InputTokens
		outcome.OutputTokens = resp.Usage.OutputTokens
	}
	if providerErr != nil {
		outcome.ErrorCode = "provider_error"
		if c.errorClassifier != nil {
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
		FallbackMaxAttempts:   c.fallbackPolicy.MaxAttempts,
	}
}

func (c *Client) runPreDispatchHook(ctx context.Context, req ports.ChatRequest, maxOutputTokens int, provider ProviderClient, trace llmkit.RouteTrace, lastProviderCallIndex int) (int, error) {
	providerDigest, err := providerRequestSHA256(provider, req, maxOutputTokens)
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

func (c *Client) maxAttempts(candidateCount int) int {
	if c.fallbackPolicy.MaxAttempts <= 0 {
		return 0
	}
	if c.fallbackPolicy.MaxAttempts > candidateCount {
		return candidateCount
	}
	return c.fallbackPolicy.MaxAttempts
}

func (c *Client) allowsFallback(err *RuntimeError) bool {
	if !c.strictFallback {
		return true
	}
	if err == nil || err.ProviderDispatched {
		return false
	}
	return slices.Contains(c.retryableErrorClass, err.Class)
}

func callProvider(ctx context.Context, provider ProviderClient, req ports.ChatRequest, maxOutputTokens int) (*ports.ChatResponse, error) {
	if maxOutputTokens == 0 {
		return provider.Chat(ctx, req)
	}
	return provider.(ProviderClientWithMaxOutputTokens).ChatWithMaxOutputTokens(ctx, req, maxOutputTokens)
}

func providerRequestSHA256(provider ProviderClient, req ports.ChatRequest, maxOutputTokens int) ([sha256.Size]byte, error) {
	if maxOutputTokens != 0 {
		digester, ok := provider.(ProviderRequestDigesterWithMaxOutputTokens)
		if !ok {
			return [sha256.Size]byte{}, newRuntimeError(ErrorStageConfig, llmkit.ErrorClassConfiguration, false, fmt.Errorf("provider limited request digester is required when pre-dispatch hook is configured"))
		}
		return digester.ProviderRequestSHA256WithMaxOutputTokens(req, maxOutputTokens)
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
