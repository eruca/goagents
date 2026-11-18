package goagent

import (
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

// Verify returns false for a failed acceptance test, and an error when the
// test infrastructure cannot establish an outcome.
type Verify func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error)
type CascadeTask struct {
	Key     llmkit.TaskKey
	Profile llmkit.TaskProfile
	Verify  Verify
}
type CascadeModel struct {
	Candidate llmkit.Candidate
	Revision  string
	Client    ProviderClient
}
type CascadeConfig struct {
	RescuePolicy   *llmkit.RescuePolicy
	RescueEvidence []llmkit.RescueEvidence
	Tasks          []CascadeTask
	Models         []CascadeModel
	Evidence       []llmkit.QualityEvidence
	Policy         llmkit.QualityPolicy
	HealthStore    llmkit.HealthStore
	Recorder       llmkit.Recorder
	Now            func() time.Time
}
type CascadeAttempt struct {
	ModelAlias, ModelRevision string
	EstimatedCents            int
	TestsPassed               bool
}
type CascadeResult struct {
	Task           llmkit.TaskKey
	Response       *ports.ChatResponse
	Attempts       []CascadeAttempt
	EstimatedCents int
}

var (
	ErrCascadeExhausted = errors.New("no remaining stronger model passed the automatic tests")
	ErrCascadeBudget    = errors.New("quality cascade estimated budget exhausted")
	ErrCascadeVerifier  = errors.New("automatic test infrastructure failed")
	ErrCascadeProvider  = errors.New("quality cascade provider call failed")
	// ErrCascadeUnavailable means local health state (busy, cooldown, quota)
	// kept the request from being sent. It says nothing about model quality.
	ErrCascadeUnavailable = errors.New("quality cascade provider unavailable before dispatch")
	ErrCascadeAudit       = errors.New("quality cascade audit failed")
	ErrCascadeUnsupported = errors.New("quality cascade accepts generation-only requests")
)

// CascadeExecutor freezes registrations and qualification evidence.
// Provider clients and host verifiers must be safe for concurrent use.
type CascadeExecutor struct {
	config CascadeConfig
	tasks  map[llmkit.TaskKey]CascadeTask
	models map[string]CascadeModel
}

func NewCascadeExecutor(config CascadeConfig) (*CascadeExecutor, error) {
	if config.Policy.Validate() != nil || len(config.Tasks) == 0 || len(config.Models) == 0 {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	c := &CascadeExecutor{config: config, tasks: map[llmkit.TaskKey]CascadeTask{}, models: map[string]CascadeModel{}}
	c.config.Tasks = append([]CascadeTask(nil), config.Tasks...)
	c.config.Models = append([]CascadeModel(nil), config.Models...)
	c.config.Evidence = append([]llmkit.QualityEvidence(nil), config.Evidence...)
	c.config.RescueEvidence = append([]llmkit.RescueEvidence(nil), config.RescueEvidence...)
	if config.RescuePolicy != nil {
		if config.RescuePolicy.Validate() != nil {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		policy := *config.RescuePolicy
		c.config.RescuePolicy = &policy
	} else if len(config.RescueEvidence) > 0 {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	type rescueKey struct {
		task     llmkit.TaskKey
		corpus   string
		from, to llmkit.QualityModelKey
	}
	rescueSeen := map[rescueKey]bool{}
	for _, e := range c.config.RescueEvidence {
		if e.Validate() != nil {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		key := rescueKey{e.Task, e.CorpusSHA256, e.From, e.To}
		if rescueSeen[key] {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		rescueSeen[key] = true
	}
	for _, task := range config.Tasks {
		if task.Key.Validate() != nil || task.Verify == nil || task.Profile.NeedsTools || !validCascadeProfile(task.Profile) {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		if _, ok := c.tasks[task.Key]; ok {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		if task.Profile.TaskType != "" && task.Profile.TaskType != task.Key.Type {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		task.Profile.TaskType = task.Key.Type
		task.Profile.Source = llmkit.ProfileSourceHost
		c.tasks[task.Key] = task
	}
	for _, model := range config.Models {
		m := model.Candidate.Model
		if !cascadeLabel(m.Alias) || !cascadeLabel(m.Provider) || !cascadeLabel(model.Candidate.AccountAlias) || !cascadeLabel(model.Revision) || nilProvider(model.Client) || m.EstimatedCents < 0 {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		if _, ok := c.models[m.Alias]; ok {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		c.models[m.Alias] = model
	}
	type evidenceKey struct {
		task                               llmkit.TaskKey
		alias, account, provider, revision string
	}
	seen := map[evidenceKey]bool{}
	for _, e := range c.config.Evidence {
		if e.Validate() != nil {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		key := evidenceKey{e.Task, e.ModelAlias, e.AccountAlias, e.Provider, e.ModelRevision}
		if seen[key] {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		seen[key] = true
	}
	return c, nil
}

func (c *CascadeExecutor) Execute(ctx context.Context, key llmkit.TaskKey, taskID string, req ports.ChatRequest) (*CascadeResult, error) {
	result := &CascadeResult{Task: key}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	task, ok := c.tasks[key]
	if !ok || !cascadeLabel(taskID) {
		return result, llmkit.ErrInvalidQualityConfig
	}
	if len(req.Tools) > 0 {
		return result, ErrCascadeUnsupported
	}
	lastRate, lastCost := -1.0, -1
	var previous *llmkit.QualityEvidence
	attempted := map[string]bool{}
	for len(result.Attempts) < c.config.Policy.MaxAttempts {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		plan, err := c.config.Policy.Plan(key, task.Profile, c.candidates(key), c.now())
		if err != nil {
			if len(result.Attempts) > 0 && errors.Is(err, llmkit.ErrNoQualifiedModel) {
				return result, ErrCascadeExhausted
			}
			return result, err
		}
		var selected *llmkit.QualityCandidate
		selectedRescue := -1.0
		for i := range plan {
			candidate := &plan[i]
			if attempted[candidate.Candidate.Model.Alias] || candidate.Evidence.SuccessRate() <= lastRate || candidate.Candidate.Model.EstimatedCents < lastCost {
				continue
			}
			rescueRate, eligible := c.rescueRate(previous, candidate.Evidence)
			if !eligible {
				continue
			}
			if selected != nil && candidate.Candidate.Model.EstimatedCents > selected.Candidate.Model.EstimatedCents {
				break
			}
			if selected == nil || rescueRate > selectedRescue {
				selected = candidate
				selectedRescue = rescueRate
			}
		}
		if selected == nil {
			return result, ErrCascadeExhausted
		}
		model := c.models[selected.Candidate.Model.Alias]
		cost := selected.Candidate.Model.EstimatedCents
		// Check by subtraction to avoid overflowing accumulated cost.
		if cost > c.config.Policy.MaxEstimatedCents-result.EstimatedCents {
			return result, ErrCascadeBudget
		}
		result.EstimatedCents += cost
		attempted[model.Candidate.Model.Alias] = true
		passed, resp, err := c.attempt(ctx, task, model, taskID, len(result.Attempts)+1, req)
		if errors.Is(err, ErrCascadeUnavailable) {
			// Nothing was sent, so nothing is charged against the budget.
			result.EstimatedCents -= cost
			cost = 0
		}
		result.Attempts = append(result.Attempts, CascadeAttempt{ModelAlias: model.Candidate.Model.Alias, ModelRevision: model.Revision, EstimatedCents: cost, TestsPassed: passed})
		if err != nil {
			return result, err
		}
		if passed {
			result.Response = resp
			return result, nil
		}
		lastRate, lastCost = selected.Evidence.SuccessRate(), cost
		evidence := selected.Evidence
		previous = &evidence
	}
	return result, ErrCascadeExhausted
}

type QualityCase struct {
	ID      string
	Request ports.ChatRequest
}

// Evaluate measures one frozen corpus with the registered automatic verifier.
// It performs provider calls only when explicitly invoked by the host. Live
// residual cases never silently overwrite corpus-based qualification evidence.
func (c *CascadeExecutor) Evaluate(ctx context.Context, key llmkit.TaskKey, modelAlias string, cases []QualityCase, expiresAt time.Time) (llmkit.QualityEvidence, error) {
	empty := llmkit.QualityEvidence{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	task, ok := c.tasks[key]
	model, found := c.models[modelAlias]
	if !ok || !found || len(cases) == 0 || !expiresAt.After(c.now()) {
		return empty, llmkit.ErrInvalidQualityConfig
	}
	cost := model.Candidate.Model.EstimatedCents
	if cost == 0 && model.Candidate.Model.PriceClass != llmkit.PriceFree {
		return empty, llmkit.ErrInvalidQualityConfig
	}
	// Evaluation is explicitly invoked, but still respects the total configured
	// estimated budget. Division avoids overflow for a large caller corpus.
	if cost > 0 && len(cases) > c.config.Policy.MaxEstimatedCents/cost {
		return empty, ErrCascadeBudget
	}
	seen := map[string]bool{}
	for _, sample := range cases {
		if !cascadeLabel(sample.ID) || seen[sample.ID] {
			return empty, llmkit.ErrInvalidQualityConfig
		}
		if len(sample.Request.Tools) > 0 {
			return empty, ErrCascadeUnsupported
		}
		seen[sample.ID] = true
	}
	evidence := llmkit.QualityEvidence{Task: key, AccountAlias: model.Candidate.AccountAlias, ModelAlias: modelAlias, Provider: model.Candidate.Model.Provider, ModelRevision: model.Revision, Samples: len(cases), ExpiresAt: expiresAt}
	corpus, _, err := qualityCorpus(cases)
	if err != nil {
		return empty, err
	}
	evidence.CorpusSHA256 = corpus
	for _, sample := range cases {
		passed, _, err := c.attempt(ctx, task, model, sample.ID, 1, sample.Request)
		// A dispatched transport failure counts as a failed corpus case. A request
		// local health state kept from being sent, infrastructure failure or
		// cancellation yields no publishable partial evidence.
		if err != nil && !errors.Is(err, ErrCascadeProvider) {
			return empty, err
		}
		if passed {
			evidence.Passed++
		}
	}
	evidence.EvaluatedAt = c.now()
	if evidence.Validate() != nil {
		return empty, llmkit.ErrInvalidQualityConfig
	}
	return evidence, nil
}

func (c *CascadeExecutor) now() time.Time {
	if c.config.Now != nil {
		return c.config.Now().UTC()
	}
	return time.Now().UTC()
}

// Pairwise evidence filters upgrades; equal-cost upgrades prefer better rescue.
// Without an explicit rescue policy, the original overall-rate policy remains.
func (c *CascadeExecutor) rescueRate(from *llmkit.QualityEvidence, to llmkit.QualityEvidence) (float64, bool) {
	if from == nil || c.config.RescuePolicy == nil {
		return 0, true
	}
	for _, e := range c.config.RescueEvidence {
		if e.Qualifies(*from, to, *c.config.RescuePolicy, c.now()) {
			return e.SuccessRate(), true
		}
	}
	return 0, false
}

func (c *CascadeExecutor) candidates(key llmkit.TaskKey) []llmkit.QualityCandidate {
	candidates := make([]llmkit.QualityCandidate, 0, len(c.models))
	for _, e := range c.config.Evidence {
		model, ok := c.models[e.ModelAlias]
		if !ok || e.Task != key {
			continue
		}
		candidate := model.Candidate
		if c.config.HealthStore != nil {
			candidate = llmkit.ApplyProviderHealth(c.config.HealthStore.Snapshot(), []llmkit.Candidate{candidate})[0]
		}
		candidates = append(candidates, llmkit.QualityCandidate{Candidate: candidate, ModelRevision: model.Revision, Evidence: e})
	}
	return candidates
}

// attempt reuses transport cancellation, fail-closed fallback and health
// accounting. A rejected answer does not mark a reachable provider unhealthy.
func (c *CascadeExecutor) attempt(ctx context.Context, task CascadeTask, model CascadeModel, taskID string, index int, req ports.ChatRequest) (bool, *ports.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	routeID := "quality-" + rand.Text()
	metadata := RouteMetadata{TaskID: taskID, RouteID: routeID, Attempt: index}
	client := NewClient(Config{
		FallbackPolicy:        FallbackPolicy{FailClosed: true},
		Candidates:            []llmkit.Candidate{model.Candidate},
		Providers:             map[string]ProviderClient{model.Candidate.Model.Alias: model.Client},
		Policy:                llmkit.RoutePolicy{Now: c.now},
		ProfileProvider:       func(context.Context, ports.ChatRequest) llmkit.TaskProfile { return task.Profile },
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) RouteMetadata { return metadata },
		HealthStore:           c.config.HealthStore, Recorder: c.config.Recorder,
	})
	started := time.Now()
	response, providerErr := client.Chat(ctx, cloneCascadeRequest(req))
	outcome := llmkit.TaskOutcome{RouteID: routeID, TaskID: taskID, Attempt: index, TaskType: task.Key.Type,
		AccountAlias: model.Candidate.AccountAlias, ModelAlias: model.Candidate.Model.Alias, Provider: model.Candidate.Model.Provider,
		Success: providerErr == nil, EstimatedCents: model.Candidate.Model.EstimatedCents, BusinessOutcome: llmkit.BusinessOutcomeUnknown}
	var err error
	passed := false
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
		outcome.ErrorCode = "canceled"
	case providerErr != nil:
		err = ErrCascadeProvider
		outcome.ErrorCode = "provider_error"
		var runtimeErr *RuntimeError
		if errors.As(providerErr, &runtimeErr) && !runtimeErr.ProviderDispatched {
			switch {
			case runtimeErr.Class == llmkit.ErrorClassProviderBusy || runtimeErr.Stage == ErrorStageRoute:
				err = ErrCascadeUnavailable
				outcome.ErrorCode = "provider_unavailable"
			case runtimeErr.Stage == ErrorStagePreCall && c.config.Recorder != nil:
				err = ErrCascadeAudit
			}
		}
	case response == nil:
		err = ErrCascadeProvider
		outcome.Success = false
		outcome.ErrorCode = "invalid_response"
	case len(response.ToolCalls) > 0:
		err = ErrCascadeUnsupported
		outcome.ErrorCode = "unexpected_tool_calls"
	default:
		outcome.InputTokens = response.Usage.InputTokens
		outcome.OutputTokens = response.Usage.OutputTokens
		var verifyErr error
		passed, verifyErr = runVerifier(ctx, task.Verify, req, response)
		if ctx.Err() != nil {
			passed = false
			err = ctx.Err()
			outcome.ErrorCode = "canceled"
		} else if verifyErr != nil {
			passed = false
			err = ErrCascadeVerifier
			outcome.ErrorCode = "test_infrastructure_error"
		} else if passed {
			outcome.BusinessOutcome = llmkit.BusinessOutcomeSuccess
			outcome.SuccessSignal = llmkit.SuccessSignalTestsPassed
		} else {
			outcome.BusinessOutcome = llmkit.BusinessOutcomeFailure
			outcome.FailureReason = "automatic_tests_failed"
		}
	}
	outcome.LatencyMillis = int(time.Since(started).Milliseconds())
	if c.config.Recorder != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), providerOutcomeCleanupTimeout)
		recordErr := c.config.Recorder.RecordOutcome(cleanup, outcome)
		cancel()
		if recordErr != nil {
			if ctx.Err() != nil {
				return false, nil, ctx.Err()
			}
			return false, nil, ErrCascadeAudit
		}
	}
	// Cancellation may arrive while the bounded outcome recorder is finishing.
	if ctx.Err() != nil {
		return false, nil, ctx.Err()
	}
	if err != nil || !passed {
		return false, nil, err
	}
	return true, response, nil
}

func cascadeLabel(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= 256 && strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func nilProvider(client ProviderClient) bool {
	if client == nil {
		return true
	}
	v := reflect.ValueOf(client)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func validCascadeProfile(p llmkit.TaskProfile) bool {
	return (p.Complexity == llmkit.ComplexitySimple || p.Complexity == llmkit.ComplexityMedium || p.Complexity == llmkit.ComplexityHard) &&
		(p.Privacy == llmkit.PrivacyLocalOnly || p.Privacy == llmkit.PrivacyLocalPreferred || p.Privacy == llmkit.PrivacyCloudAllowed) &&
		(p.FailureCost == llmkit.FailureCostLow || p.FailureCost == llmkit.FailureCostMedium || p.FailureCost == llmkit.FailureCostHigh) &&
		(p.Latency == llmkit.LatencyNone || p.Latency == llmkit.LatencyNormal || p.Latency == llmkit.LatencyUrgent) && p.MaxEstimatedCents >= 0
}

func cloneCascadeRequest(req ports.ChatRequest) ports.ChatRequest {
	req.Messages = append([]ports.ChatMessage(nil), req.Messages...)
	for i := range req.Messages {
		req.Messages[i].ToolCalls = append([]ports.ToolCall(nil), req.Messages[i].ToolCalls...)
		for j := range req.Messages[i].ToolCalls {
			req.Messages[i].ToolCalls[j].Input = append([]byte(nil), req.Messages[i].ToolCalls[j].Input...)
		}
	}
	return req
}
