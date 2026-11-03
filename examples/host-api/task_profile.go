package main

import (
	"fmt"
	"strings"

	"github.com/eruca/goagents/llmkit"
)

type taskProfileRequest struct {
	TaskType          *string `json:"task_type,omitempty"`
	Complexity        *string `json:"complexity,omitempty"`
	Latency           *string `json:"latency,omitempty"`
	FailureCost       *string `json:"failure_cost,omitempty"`
	Privacy           *string `json:"privacy,omitempty"`
	MaxEstimatedCents *int    `json:"max_estimated_cents,omitempty"`
	NeedsReasoning    *bool   `json:"needs_reasoning,omitempty"`
	NeedsTools        *bool   `json:"needs_tools,omitempty"`
	NeedsJSON         *bool   `json:"needs_json,omitempty"`
	NeedsLongContext  *bool   `json:"needs_long_context,omitempty"`
}

type taskProfileResponse struct {
	TaskType          string `json:"task_type,omitempty"`
	Complexity        string `json:"complexity,omitempty"`
	Latency           string `json:"latency,omitempty"`
	FailureCost       string `json:"failure_cost,omitempty"`
	Privacy           string `json:"privacy,omitempty"`
	MaxEstimatedCents int    `json:"max_estimated_cents,omitempty"`
	NeedsReasoning    bool   `json:"needs_reasoning,omitempty"`
	NeedsTools        bool   `json:"needs_tools,omitempty"`
	NeedsJSON         bool   `json:"needs_json,omitempty"`
	NeedsLongContext  bool   `json:"needs_long_context,omitempty"`
}

func taskProfileToResponse(profile *llmkit.TaskProfile) *taskProfileResponse {
	if profile == nil {
		return nil
	}
	return &taskProfileResponse{
		TaskType:          profile.TaskType,
		Complexity:        string(profile.Complexity),
		Latency:           string(profile.Latency),
		FailureCost:       string(profile.FailureCost),
		Privacy:           string(profile.Privacy),
		MaxEstimatedCents: profile.MaxEstimatedCents,
		NeedsReasoning:    profile.NeedsReasoning,
		NeedsTools:        profile.NeedsTools,
		NeedsJSON:         profile.NeedsJSON,
		NeedsLongContext:  profile.NeedsLongContext,
	}
}

func parseTaskProfile(preset string, req *taskProfileRequest, candidates []llmkit.Candidate) (llmkit.TaskProfile, error) {
	profile, err := taskProfilePreset(preset)
	if err != nil {
		return llmkit.TaskProfile{}, err
	}
	if err := applyTaskProfileOverride(&profile, req); err != nil {
		return llmkit.TaskProfile{}, err
	}
	if err := validateTaskProfile(profile, candidates); err != nil {
		return llmkit.TaskProfile{}, err
	}
	return profile, nil
}

func applyTaskProfileOverride(profile *llmkit.TaskProfile, req *taskProfileRequest) error {
	if req == nil {
		return nil
	}
	if req.TaskType != nil {
		value, err := nonEmptyTaskProfileString("task_type", *req.TaskType)
		if err != nil {
			return err
		}
		profile.TaskType = value
	}
	if req.Complexity != nil {
		raw, err := nonEmptyTaskProfileString("complexity", *req.Complexity)
		if err != nil {
			return err
		}
		value := llmkit.Complexity(raw)
		if value != llmkit.ComplexitySimple && value != llmkit.ComplexityMedium && value != llmkit.ComplexityHard {
			return fmt.Errorf("unsupported complexity %q", *req.Complexity)
		}
		profile.Complexity = value
	}
	if req.Latency != nil {
		raw, err := nonEmptyTaskProfileString("latency", *req.Latency)
		if err != nil {
			return err
		}
		value := llmkit.LatencyRequirement(raw)
		if value != llmkit.LatencyNone && value != llmkit.LatencyNormal && value != llmkit.LatencyUrgent {
			return fmt.Errorf("unsupported latency %q", *req.Latency)
		}
		profile.Latency = value
	}
	if req.FailureCost != nil {
		raw, err := nonEmptyTaskProfileString("failure_cost", *req.FailureCost)
		if err != nil {
			return err
		}
		value := llmkit.FailureCost(raw)
		if value != llmkit.FailureCostLow && value != llmkit.FailureCostMedium && value != llmkit.FailureCostHigh {
			return fmt.Errorf("unsupported failure_cost %q", *req.FailureCost)
		}
		profile.FailureCost = value
	}
	if req.Privacy != nil {
		raw, err := nonEmptyTaskProfileString("privacy", *req.Privacy)
		if err != nil {
			return err
		}
		value := llmkit.PrivacyLevel(raw)
		if value != llmkit.PrivacyLocalPreferred && value != llmkit.PrivacyCloudAllowed && value != llmkit.PrivacyLocalOnly {
			return fmt.Errorf("unsupported privacy %q", *req.Privacy)
		}
		profile.Privacy = value
	}
	if req.MaxEstimatedCents != nil {
		if *req.MaxEstimatedCents <= 0 {
			return fmt.Errorf("max_estimated_cents must be greater than zero")
		}
		profile.MaxEstimatedCents = *req.MaxEstimatedCents
	}
	if req.NeedsReasoning != nil {
		profile.NeedsReasoning = *req.NeedsReasoning
	}
	if req.NeedsTools != nil {
		profile.NeedsTools = *req.NeedsTools
	}
	if req.NeedsJSON != nil {
		profile.NeedsJSON = *req.NeedsJSON
	}
	if req.NeedsLongContext != nil {
		profile.NeedsLongContext = *req.NeedsLongContext
	}
	return nil
}

func nonEmptyTaskProfileString(field string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s must not be empty", field)
	}
	return trimmed, nil
}

func taskProfilePreset(name string) (llmkit.TaskProfile, error) {
	switch strings.TrimSpace(name) {
	case "":
		return defaultHostTaskProfile(), nil
	case "simple_local":
		profile := defaultHostTaskProfile()
		profile.TaskType = "simple_local"
		return profile, nil
	case "balanced":
		profile := defaultHostTaskProfile()
		profile.TaskType = "balanced"
		profile.Complexity = llmkit.ComplexityMedium
		profile.FailureCost = llmkit.FailureCostMedium
		profile.Privacy = llmkit.PrivacyCloudAllowed
		return profile, nil
	case "high_success":
		profile := defaultHostTaskProfile()
		profile.TaskType = "high_success"
		profile.Complexity = llmkit.ComplexityHard
		profile.FailureCost = llmkit.FailureCostHigh
		profile.Privacy = llmkit.PrivacyCloudAllowed
		profile.NeedsReasoning = true
		return profile, nil
	case "local_only":
		profile := defaultHostTaskProfile()
		profile.TaskType = "local_only"
		profile.Privacy = llmkit.PrivacyLocalOnly
		return profile, nil
	default:
		return llmkit.TaskProfile{}, fmt.Errorf("unsupported task_profile_preset %q", name)
	}
}

func validateTaskProfile(profile llmkit.TaskProfile, candidates []llmkit.Candidate) error {
	if _, err := (llmkit.RoutePolicy{}).Select(profile, candidates); err != nil {
		return fmt.Errorf("task profile %q cannot route to an available model: %w", profile.TaskType, err)
	}
	return nil
}

func defaultHostTaskProfile() llmkit.TaskProfile {
	profile := llmkit.DefaultTaskProfile()
	profile.Source = llmkit.ProfileSourceHost
	profile.TaskType = "host_api_review"
	profile.Complexity = llmkit.ComplexitySimple
	profile.FailureCost = llmkit.FailureCostLow
	profile.Privacy = llmkit.PrivacyLocalPreferred
	return profile
}

func taskProfileFromMetadata(value any) llmkit.TaskProfile {
	switch profile := value.(type) {
	case llmkit.TaskProfile:
		return profile
	case map[string]any:
		return taskProfileFromMap(profile)
	default:
		return defaultHostTaskProfile()
	}
}

func taskProfileFromMap(values map[string]any) llmkit.TaskProfile {
	profile := defaultHostTaskProfile()
	if value, ok := values["task_type"].(string); ok && strings.TrimSpace(value) != "" {
		profile.TaskType = strings.TrimSpace(value)
	}
	if value, ok := values["source"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Source = llmkit.ProfileSource(strings.TrimSpace(value))
	}
	if value, ok := values["complexity"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Complexity = llmkit.Complexity(strings.TrimSpace(value))
	}
	if value, ok := values["latency_requirement"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Latency = llmkit.LatencyRequirement(strings.TrimSpace(value))
	}
	if value, ok := values["failure_cost"].(string); ok && strings.TrimSpace(value) != "" {
		profile.FailureCost = llmkit.FailureCost(strings.TrimSpace(value))
	}
	if value, ok := values["privacy_level"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Privacy = llmkit.PrivacyLevel(strings.TrimSpace(value))
	}
	if value, ok := values["max_estimated_cents"].(float64); ok && value > 0 {
		profile.MaxEstimatedCents = int(value)
	}
	profile.NeedsReasoning, _ = values["needs_reasoning"].(bool)
	profile.NeedsTools, _ = values["needs_tools"].(bool)
	profile.NeedsJSON, _ = values["needs_json"].(bool)
	profile.NeedsLongContext, _ = values["needs_long_context"].(bool)
	return profile
}
