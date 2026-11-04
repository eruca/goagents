package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/llmkit"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
)

type modelsResponse struct {
	Models []modelResponse               `json:"models"`
	Health llmkit.ProviderHealthSnapshot `json:"health"`
	Stats  []modelStatsResponse          `json:"stats,omitempty"`
}

type llmRoutesResponse struct {
	WorkflowID string             `json:"workflow_id"`
	Routes     []llmRouteResponse `json:"routes"`
}

type llmRouteResponse struct {
	RouteID               string                      `json:"route_id,omitempty"`
	TaskID                string                      `json:"task_id,omitempty"`
	Attempt               int                         `json:"attempt,omitempty"`
	TaskType              string                      `json:"task_type,omitempty"`
	TaskProfile           *taskProfileResponse        `json:"task_profile,omitempty"`
	AccountAlias          string                      `json:"account_alias,omitempty"`
	ModelAlias            string                      `json:"model_alias,omitempty"`
	Provider              string                      `json:"provider,omitempty"`
	Selected              bool                        `json:"selected"`
	Reason                string                      `json:"reason,omitempty"`
	Score                 int                         `json:"score,omitempty"`
	ScoreBreakdown        map[string]int              `json:"score_breakdown,omitempty"`
	CandidateModelAliases []string                    `json:"candidate_model_aliases,omitempty"`
	Candidates            []llmRouteCandidateResponse `json:"candidates,omitempty"`
	Outcome               *llmRouteOutcomeResponse    `json:"outcome,omitempty"`
}

type llmRouteCandidateResponse struct {
	Alias          string         `json:"alias,omitempty"`
	AccountAlias   string         `json:"account_alias,omitempty"`
	Available      bool           `json:"available"`
	Score          int            `json:"score,omitempty"`
	ScoreBreakdown map[string]int `json:"score_breakdown,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

type llmRouteOutcomeResponse struct {
	Success         bool   `json:"success"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorClass      string `json:"error_class,omitempty"`
	LatencyMillis   int    `json:"latency_ms,omitempty"`
	InputTokens     int    `json:"input_tokens,omitempty"`
	OutputTokens    int    `json:"output_tokens,omitempty"`
	EstimatedCents  int    `json:"estimated_cents,omitempty"`
	BusinessOutcome string `json:"business_outcome,omitempty"`
	SuccessSignal   string `json:"success_signal,omitempty"`
	FailureReason   string `json:"failure_reason,omitempty"`
}

type modelResponse struct {
	Alias        string `json:"alias"`
	Provider     string `json:"provider"`
	AccountAlias string `json:"account_alias"`
	IsLocal      bool   `json:"is_local"`
	PriceClass   string `json:"price_class"`
}

type modelStatsResponse struct {
	TaskType          string  `json:"task_type,omitempty"`
	AccountAlias      string  `json:"account_alias,omitempty"`
	ModelAlias        string  `json:"model_alias,omitempty"`
	Provider          string  `json:"provider,omitempty"`
	RouteAttempts     int     `json:"route_attempts"`
	OutcomeCount      int     `json:"outcome_count"`
	PendingOutcomes   int     `json:"pending_outcomes"`
	Successes         int     `json:"successes"`
	Failures          int     `json:"failures"`
	SuccessRate       float64 `json:"success_rate"`
	FailureRate       float64 `json:"failure_rate"`
	AvgLatencyMillis  int     `json:"avg_latency_ms,omitempty"`
	AvgInputTokens    int     `json:"avg_input_tokens,omitempty"`
	AvgOutputTokens   int     `json:"avg_output_tokens,omitempty"`
	AvgEstimatedCents int     `json:"avg_estimated_cents,omitempty"`
}

func (s *Server) handleGetWorkflowLLMRoutes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.workflows.Get(r.Context(), id)
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if _, ok := s.authorizeStoredWorkflow(w, r, run, memoryRead); !ok {
		return
	}
	records, err := llmkit.ReadRouteAudits(s.llmHome, llmkit.AuditFilter{TaskID: id})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "llm_audit_error", err.Error())
		return
	}
	routes := make([]llmRouteResponse, 0, len(records))
	for _, record := range records {
		routes = append(routes, llmRouteToResponse(record))
	}
	writeJSON(w, http.StatusOK, llmRoutesResponse{
		WorkflowID: id,
		Routes:     routes,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	stats, err := llmkit.RefreshModelStats(s.llmHome)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "llm_audit_error", err.Error())
		return
	}
	models := make([]modelResponse, 0, len(s.models))
	for _, candidate := range s.models {
		models = append(models, modelResponse{
			Alias:        candidate.Model.Alias,
			Provider:     candidate.Model.Provider,
			AccountAlias: candidate.AccountAlias,
			IsLocal:      candidate.Model.IsLocal,
			PriceClass:   string(candidate.Model.PriceClass),
		})
	}
	writeJSON(w, http.StatusOK, modelsResponse{
		Models: models,
		Health: s.health.Snapshot(),
		Stats:  modelStatsToResponse(stats),
	})
}

func (s *Server) recordBusinessOutcome(ctx context.Context, workflowID string) error {
	records, err := llmkit.ReadRouteAudits(s.llmHome, llmkit.AuditFilter{TaskID: workflowID})
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	record := records[len(records)-1]
	outcome := llmkit.TaskOutcome{
		RouteID:         record.Route.RouteID,
		TaskID:          record.Route.TaskID,
		Attempt:         record.Route.Attempt,
		TaskType:        record.Route.TaskType,
		AccountAlias:    record.Route.AccountAlias,
		ModelAlias:      record.Route.ModelAlias,
		Provider:        record.Route.Provider,
		Success:         true,
		BusinessOutcome: llmkit.BusinessOutcomeSuccess,
		SuccessSignal:   llmkit.SuccessSignalHumanAccepted,
	}
	if record.Outcome != nil {
		outcome = *record.Outcome
		outcome.BusinessOutcome = llmkit.BusinessOutcomeSuccess
		outcome.SuccessSignal = llmkit.SuccessSignalHumanAccepted
		outcome.FailureReason = ""
	}
	recorder, err := llmkit.NewJSONLRecorder(s.llmHome)
	if err != nil {
		return err
	}
	return recorder.RecordOutcome(ctx, outcome)
}

func loadLLMKitComposition(home string) ([]llmkit.Candidate, map[string]goagentadapter.ProviderClient, *llmkit.ModelStats, error) {
	stats, err := llmkit.RefreshModelStats(home)
	if err != nil {
		return nil, nil, nil, err
	}

	configPath := filepath.Join(home, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return defaultCandidates(), defaultProviders(), stats, nil
		}
		return nil, nil, nil, err
	}

	config, err := llmkit.LoadConfig(home)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateConfiguredAPIKeyEnvs(*config, os.Getenv); err != nil {
		return nil, nil, nil, err
	}

	candidates := config.Candidates()
	if len(candidates) == 0 {
		return nil, nil, nil, fmt.Errorf("llmkit config has no models")
	}
	providers, err := goagentadapter.OpenAICompatibleProvidersFromConfig(*config, os.Getenv, nil, openaiapi.Limits{})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateProvidersForCandidates(candidates, providers); err != nil {
		return nil, nil, nil, err
	}
	return candidates, providers, stats, nil
}

func validateConfiguredAPIKeyEnvs(config llmkit.Config, getenv func(string) string) error {
	for _, account := range config.Accounts {
		envName := strings.TrimSpace(account.APIKeyEnv)
		if envName == "" {
			continue
		}
		if getenv == nil || strings.TrimSpace(getenv(envName)) == "" {
			return fmt.Errorf("account %q api_key_env %q is not set", account.Alias, envName)
		}
	}
	return nil
}

func validateProvidersForCandidates(candidates []llmkit.Candidate, providers map[string]goagentadapter.ProviderClient) error {
	for _, candidate := range candidates {
		if providers[candidate.Model.Alias] == nil {
			return fmt.Errorf("model %q has no configured provider client", candidate.Model.Alias)
		}
	}
	return nil
}

func defaultProviders() map[string]goagentadapter.ProviderClient {
	return map[string]goagentadapter.ProviderClient{
		"local-free":     staticProvider{content: "host API draft from local-free"},
		"cloud-advanced": staticProvider{content: "host API draft from cloud-advanced"},
	}
}

func defaultCandidates() []llmkit.Candidate {
	return []llmkit.Candidate{
		{
			Model: llmkit.ModelCapability{
				Alias:              "local-free",
				Provider:           "local",
				IsLocal:            true,
				CapabilityLevel:    llmkit.CapabilitySimple,
				SupportsTools:      true,
				ContextWindowClass: llmkit.ContextMedium,
				PriceClass:         llmkit.PriceFree,
				LatencyClass:       llmkit.LatencyFastClass,
			},
			AccountAlias: "local-dev",
		},
		{
			Model: llmkit.ModelCapability{
				Alias:              "cloud-advanced",
				Provider:           "openai",
				CapabilityLevel:    llmkit.CapabilityAdvanced,
				SupportsTools:      true,
				ContextWindowClass: llmkit.ContextLong,
				PriceClass:         llmkit.PriceHigh,
				LatencyClass:       llmkit.LatencyNormalClass,
			},
			AccountAlias: "cloud-prod",
		},
	}
}

func llmRouteToResponse(record llmkit.RouteAuditRecord) llmRouteResponse {
	route := record.Route
	response := llmRouteResponse{
		RouteID:               route.RouteID,
		TaskID:                route.TaskID,
		Attempt:               route.Attempt,
		TaskType:              route.TaskType,
		TaskProfile:           taskProfileToResponse(route.TaskProfile),
		AccountAlias:          route.AccountAlias,
		ModelAlias:            route.ModelAlias,
		Provider:              route.Provider,
		Selected:              route.Selected,
		Reason:                route.Reason,
		Score:                 route.Score,
		ScoreBreakdown:        copyScoreBreakdown(route.ScoreBreakdown),
		CandidateModelAliases: append([]string(nil), route.CandidateModelAliases...),
		Candidates:            routeCandidatesToResponse(route.Candidates),
	}
	if record.Outcome != nil {
		outcome := record.Outcome
		response.Outcome = &llmRouteOutcomeResponse{
			Success:         outcome.Success,
			ErrorCode:       outcome.ErrorCode,
			ErrorClass:      string(outcome.ErrorClass),
			LatencyMillis:   outcome.LatencyMillis,
			InputTokens:     outcome.InputTokens,
			OutputTokens:    outcome.OutputTokens,
			EstimatedCents:  outcome.EstimatedCents,
			BusinessOutcome: string(outcome.BusinessOutcome),
			SuccessSignal:   string(outcome.SuccessSignal),
			FailureReason:   outcome.FailureReason,
		}
	}
	return response
}

func routeCandidatesToResponse(candidates []llmkit.CandidateScore) []llmRouteCandidateResponse {
	if len(candidates) == 0 {
		return nil
	}
	response := make([]llmRouteCandidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		response = append(response, llmRouteCandidateResponse{
			Alias:          candidate.Alias,
			AccountAlias:   candidate.AccountAlias,
			Available:      candidate.Available,
			Score:          candidate.Score,
			ScoreBreakdown: copyScoreBreakdown(candidate.ScoreBreakdown),
			Reason:         candidate.Reason,
		})
	}
	return response
}

func modelStatsToResponse(stats *llmkit.ModelStats) []modelStatsResponse {
	if stats == nil || len(stats.Models) == 0 {
		return nil
	}
	keys := make([]string, 0, len(stats.Models))
	for key := range stats.Models {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	response := make([]modelStatsResponse, 0, len(keys))
	for _, key := range keys {
		entry := stats.Models[key]
		response = append(response, modelStatsResponse{
			TaskType:          entry.TaskType,
			AccountAlias:      entry.AccountAlias,
			ModelAlias:        entry.ModelAlias,
			Provider:          entry.Provider,
			RouteAttempts:     entry.RouteAttempts,
			OutcomeCount:      entry.OutcomeCount,
			PendingOutcomes:   entry.PendingOutcomes,
			Successes:         entry.Successes,
			Failures:          entry.Failures,
			SuccessRate:       entry.SuccessRate,
			FailureRate:       entry.FailureRate,
			AvgLatencyMillis:  entry.AvgLatencyMillis,
			AvgInputTokens:    entry.AvgInputTokens,
			AvgOutputTokens:   entry.AvgOutputTokens,
			AvgEstimatedCents: entry.AvgEstimatedCents,
		})
	}
	return response
}

func copyScoreBreakdown(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	copied := make(map[string]int, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
