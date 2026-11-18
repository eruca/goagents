package goagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

var ErrVerifierRejected = errors.New("automatic verifier failed its labeled acceptance tests")

// VerifierCase labels a response independently of model generation.
type VerifierCase struct {
	ID             string
	Request        ports.ChatRequest
	Response       *ports.ChatResponse
	ExpectedPassed bool
}
type VerifierReport struct {
	Task                                                    llmkit.TaskKey
	Cases, Positives, Negatives, FalseAccepts, FalseRejects int
	Passed                                                  bool
}

func (c *CascadeExecutor) AuditVerifier(ctx context.Context, key llmkit.TaskKey, cases []VerifierCase) (*VerifierReport, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, ok := c.tasks[key]
	if !ok || len(cases) == 0 {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	report := &VerifierReport{Task: key, Cases: len(cases)}
	seen := map[string]bool{}
	for _, sample := range cases {
		if !cascadeLabel(sample.ID) || seen[sample.ID] || sample.Response == nil {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		seen[sample.ID] = true
		if sample.ExpectedPassed {
			report.Positives++
		} else {
			report.Negatives++
		}
	}
	if report.Positives == 0 || report.Negatives == 0 {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	for _, sample := range cases {
		passed, err := runVerifier(ctx, task.Verify, sample.Request, sample.Response)
		if err != nil {
			return nil, err
		}
		if passed && !sample.ExpectedPassed {
			report.FalseAccepts++
		}
		if !passed && sample.ExpectedPassed {
			report.FalseRejects++
		}
	}
	report.Passed = report.FalseAccepts == 0 && report.FalseRejects == 0
	if !report.Passed {
		return report, ErrVerifierRejected
	}
	return report, nil
}

type CascadeEvaluationSet struct {
	Qualification, Validation []QualityCase
	VerifierCases             []VerifierCase
}
type CascadeEvaluationOptions struct {
	ExpiresAt         time.Time
	MaxEstimatedCents int
	RescuePolicy      llmkit.RescuePolicy
}
type ModelEvaluation struct {
	Evidence         llmkit.QualityEvidence
	ProviderFailures int
}
type CascadeCaseResult struct {
	ID                       string
	Passed                   bool
	Attempts, EstimatedCents int
	LatencyMillis            float64
	ErrorCode                string
}
type CascadeChainReport struct {
	Cases, Solved, Failed, TotalAttempts, TotalEstimatedCents int
	SuccessRate, MeanEstimatedCents, P50Millis, P95Millis     float64
	EstimatedCentsPerSolved                                   *float64
	Results                                                   []CascadeCaseResult
}
type CascadeEvaluationReport struct {
	Task                                  llmkit.TaskKey
	QualificationSHA256, ValidationSHA256 string
	Verifier                              VerifierReport
	Models                                []ModelEvaluation
	Rescue                                []llmkit.RescueEvidence
	Chain                                 CascadeChainReport
	TotalEstimatedCents                   int
}

// EvaluateCascade audits the verifier, measures paired model failures on a
// qualification corpus, then executes the actual cascade on a disjoint holdout.
func (c *CascadeExecutor) EvaluateCascade(ctx context.Context, key llmkit.TaskKey, set CascadeEvaluationSet, options CascadeEvaluationOptions) (*CascadeEvaluationReport, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, ok := c.tasks[key]
	if !ok || options.MaxEstimatedCents < 0 || options.RescuePolicy.Validate() != nil || !options.ExpiresAt.After(c.now()) ||
		len(set.Qualification) < c.config.Policy.MinSamples || len(set.Validation) == 0 {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	trainHash, trainInputs, err := qualityCorpus(set.Qualification)
	if err != nil {
		return nil, err
	}
	validationHash, validationInputs, err := qualityCorpus(set.Validation)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, sample := range set.Qualification {
		ids[sample.ID] = true
	}
	for _, sample := range set.Validation {
		if ids[sample.ID] {
			return nil, llmkit.ErrInvalidQualityConfig
		}
	}
	for input := range validationInputs {
		if trainInputs[input] {
			return nil, llmkit.ErrInvalidQualityConfig
		}
	}
	models := c.evaluationModels(task)
	if len(models) == 0 {
		return nil, llmkit.ErrNoQualifiedModel
	}
	if !c.evaluationBudgetFits(models, len(set.Qualification), len(set.Validation), options.MaxEstimatedCents) {
		return nil, ErrCascadeBudget
	}
	verifier, err := c.AuditVerifier(ctx, key, set.VerifierCases)
	if err != nil {
		return nil, err
	}
	report := &CascadeEvaluationReport{Task: key, QualificationSHA256: trainHash, ValidationSHA256: validationHash, Verifier: *verifier}
	matrix := make([][]bool, len(models))
	for i, model := range models {
		matrix[i] = make([]bool, len(set.Qualification))
		measured := ModelEvaluation{Evidence: llmkit.QualityEvidence{Task: key, CorpusSHA256: trainHash,
			AccountAlias: model.Candidate.AccountAlias, ModelAlias: model.Candidate.Model.Alias, Provider: model.Candidate.Model.Provider,
			ModelRevision: model.Revision, Samples: len(set.Qualification), ExpiresAt: options.ExpiresAt}}
		for j, sample := range set.Qualification {
			passed, _, err := c.attempt(ctx, task, model, sample.ID, 1, sample.Request)
			if err != nil && !errors.Is(err, ErrCascadeProvider) {
				return nil, err
			}
			if err != nil {
				measured.ProviderFailures++
			}
			matrix[i][j] = passed
			if passed {
				measured.Evidence.Passed++
			}
			report.TotalEstimatedCents += model.Candidate.Model.EstimatedCents
		}
		measured.Evidence.EvaluatedAt = c.now()
		if measured.Evidence.Validate() != nil {
			return nil, llmkit.ErrInvalidQualityConfig
		}
		report.Models = append(report.Models, measured)
	}
	for from := range models {
		for to := range models {
			if from == to {
				continue
			}
			evidence := llmkit.RescueEvidence{Task: key, CorpusSHA256: trainHash, From: report.Models[from].Evidence.ModelKey(),
				To: report.Models[to].Evidence.ModelKey(), EvaluatedAt: c.now(), ExpiresAt: options.ExpiresAt}
			for sample := range set.Qualification {
				if !matrix[from][sample] {
					evidence.Failed++
					if matrix[to][sample] {
						evidence.Rescued++
					}
				}
			}
			if evidence.Validate() != nil {
				return nil, llmkit.ErrInvalidQualityConfig
			}
			report.Rescue = append(report.Rescue, evidence)
		}
	}
	// Qualification and holdout are separate provider runs. Replaying the matrix
	// would hide stochastic errors and could not measure actual cascade latency.
	config := c.config
	config.Evidence = nil
	config.RescueEvidence = report.Rescue
	config.RescuePolicy = &options.RescuePolicy
	for _, model := range report.Models {
		config.Evidence = append(config.Evidence, model.Evidence)
	}
	executor, err := NewCascadeExecutor(config)
	if err != nil {
		return nil, err
	}
	for _, sample := range set.Validation {
		started := time.Now()
		result, err := executor.Execute(ctx, key, sample.ID, sample.Request)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrCascadeVerifier) || errors.Is(err, ErrCascadeAudit) {
			return nil, err
		}
		outcome := CascadeCaseResult{ID: sample.ID, Passed: err == nil, Attempts: len(result.Attempts),
			EstimatedCents: result.EstimatedCents, LatencyMillis: float64(time.Since(started)) / float64(time.Millisecond), ErrorCode: cascadeEvaluationError(err)}
		report.Chain.Results = append(report.Chain.Results, outcome)
		report.Chain.TotalAttempts += outcome.Attempts
		report.Chain.TotalEstimatedCents += outcome.EstimatedCents
		if outcome.Passed {
			report.Chain.Solved++
		} else {
			report.Chain.Failed++
		}
	}
	if !options.ExpiresAt.After(c.now()) {
		return nil, llmkit.ErrInvalidQualityConfig
	}
	report.Chain.Cases = len(set.Validation)
	report.Chain.SuccessRate = float64(report.Chain.Solved) / float64(report.Chain.Cases)
	report.Chain.MeanEstimatedCents = float64(report.Chain.TotalEstimatedCents) / float64(report.Chain.Cases)
	if report.Chain.Solved > 0 {
		cost := float64(report.Chain.TotalEstimatedCents) / float64(report.Chain.Solved)
		report.Chain.EstimatedCentsPerSolved = &cost
	}
	latencies := make([]float64, 0, len(set.Validation))
	for _, outcome := range report.Chain.Results {
		latencies = append(latencies, outcome.LatencyMillis)
	}
	sort.Float64s(latencies)
	report.Chain.P50Millis = latencies[(len(latencies)-1)/2]
	report.Chain.P95Millis = latencies[(len(latencies)*95+99)/100-1]
	report.TotalEstimatedCents += report.Chain.TotalEstimatedCents
	return report, nil
}

// qualityCorpus checks identifiers and tool-free requests, then hashes canonical
// JSON. Reports expose only hashes/IDs; no prompts or generated responses.
func qualityCorpus(cases []QualityCase) (string, map[string]bool, error) {
	if len(cases) == 0 {
		return "", nil, llmkit.ErrInvalidQualityConfig
	}
	ids, inputs := map[string]bool{}, map[string]bool{}
	for _, sample := range cases {
		if !cascadeLabel(sample.ID) || ids[sample.ID] {
			return "", nil, llmkit.ErrInvalidQualityConfig
		}
		if len(sample.Request.Tools) > 0 {
			return "", nil, ErrCascadeUnsupported
		}
		ids[sample.ID] = true
		b, err := json.Marshal(sample.Request)
		if err != nil {
			return "", nil, llmkit.ErrInvalidQualityConfig
		}
		hash := sha256.Sum256(b)
		inputs[hex.EncodeToString(hash[:])] = true
	}
	encoded, err := json.Marshal(cases)
	if err != nil {
		return "", nil, llmkit.ErrInvalidQualityConfig
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), inputs, nil
}

func (c *CascadeExecutor) evaluationModels(task CascadeTask) []CascadeModel {
	result := make([]CascadeModel, 0, len(c.models))
	policy := llmkit.RoutePolicy{Now: c.now}
	for _, model := range c.models {
		candidate := model.Candidate
		if candidate.Model.EstimatedCents > c.config.Policy.MaxEstimatedCents || (candidate.Model.EstimatedCents == 0 && candidate.Model.PriceClass != llmkit.PriceFree) {
			continue
		}
		if c.config.HealthStore != nil {
			candidate = llmkit.ApplyProviderHealth(c.config.HealthStore.Snapshot(), []llmkit.Candidate{candidate})[0]
		}
		if _, err := policy.Select(task.Profile, []llmkit.Candidate{candidate}); err != nil {
			continue
		}
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].Candidate.Model, result[j].Candidate.Model
		if a.EstimatedCents != b.EstimatedCents {
			return a.EstimatedCents < b.EstimatedCents
		}
		return a.Alias < b.Alias
	})
	return result
}

// Reserve the full matrix and the maximum allowed holdout calls before any
// dispatch. The remaining-budget arithmetic cannot overflow an int.
func (c *CascadeExecutor) evaluationBudgetFits(models []CascadeModel, train, holdout, budget int) bool {
	remaining := budget
	for _, model := range models {
		cost := model.Candidate.Model.EstimatedCents
		if cost > 0 && train > remaining/cost {
			return false
		}
		remaining -= train * cost
	}
	perCase := 0
	for i, calls := len(models)-1, 0; i >= 0 && calls < c.config.Policy.MaxAttempts; i, calls = i-1, calls+1 {
		cost := models[i].Candidate.Model.EstimatedCents
		if cost > c.config.Policy.MaxEstimatedCents-perCase {
			perCase = c.config.Policy.MaxEstimatedCents
			break
		}
		perCase += cost
	}
	return perCase == 0 || holdout <= remaining/perCase
}

func cascadeEvaluationError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrCascadeBudget):
		return "budget_exhausted"
	case errors.Is(err, ErrCascadeExhausted):
		return "tests_not_passed"
	case errors.Is(err, llmkit.ErrNoQualifiedModel):
		return "no_qualified_model"
	case errors.Is(err, ErrCascadeProvider):
		return "provider_failed"
	case errors.Is(err, ErrCascadeUnsupported):
		return "unsupported_response"
	default:
		return "configuration_failed"
	}
}

// Both production acceptance and verifier audits use this boundary. A verifier
// panic is an infrastructure failure, never a negative test or valid evidence.
func runVerifier(ctx context.Context, verify Verify, req ports.ChatRequest, response *ports.ChatResponse) (passed bool, err error) {
	defer func() {
		if recover() != nil {
			passed = false
			err = ErrCascadeVerifier
		}
	}()
	if err = ctx.Err(); err != nil {
		return false, err
	}
	copyResponse := *response
	copyResponse.ToolCalls = append([]ports.ToolCall(nil), response.ToolCalls...)
	for i := range copyResponse.ToolCalls {
		copyResponse.ToolCalls[i].Input = append([]byte(nil), copyResponse.ToolCalls[i].Input...)
	}
	passed, err = verify(ctx, cloneCascadeRequest(req), &copyResponse)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, ErrCascadeVerifier
	}
	return passed, nil
}
