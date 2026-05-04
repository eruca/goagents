package llmkit

// Complexity describes how much model capability a task needs.
type Complexity string

const (
	ComplexitySimple Complexity = "simple"
	ComplexityMedium Complexity = "medium"
	ComplexityHard   Complexity = "hard"
)

// LatencyRequirement describes how strongly routing should prefer lower
// latency. It is a task-side requirement, not a model measurement.
type LatencyRequirement string

const (
	LatencyNone   LatencyRequirement = "none"
	LatencyNormal LatencyRequirement = "normal"
	LatencyUrgent LatencyRequirement = "urgent"
)

// FailureCost describes the user-visible cost of choosing a weak model.
type FailureCost string

const (
	FailureCostLow    FailureCost = "low"
	FailureCostMedium FailureCost = "medium"
	FailureCostHigh   FailureCost = "high"
)

// PrivacyLevel describes whether a task may leave the host machine.
type PrivacyLevel string

const (
	PrivacyLocalPreferred PrivacyLevel = "local_preferred"
	PrivacyCloudAllowed   PrivacyLevel = "cloud_allowed"
	PrivacyLocalOnly      PrivacyLevel = "local_only"
)

// ProfileSource records whether the host provided the task profile or llmkit
// filled in defaults.
type ProfileSource string

const (
	ProfileSourceDefault ProfileSource = "default"
	ProfileSourceHost    ProfileSource = "host"
)

// TaskProfile is the host-provided or default task description used by later
// routing steps.
type TaskProfile struct {
	TaskType         string             `json:"task_type,omitempty"`
	Source           ProfileSource      `json:"source,omitempty"`
	Complexity       Complexity         `json:"complexity"`
	Latency          LatencyRequirement `json:"latency_requirement"`
	FailureCost      FailureCost        `json:"failure_cost"`
	Privacy          PrivacyLevel       `json:"privacy_level"`
	NeedsReasoning   bool               `json:"needs_reasoning,omitempty"`
	NeedsTools       bool               `json:"needs_tools,omitempty"`
	NeedsJSON        bool               `json:"needs_json,omitempty"`
	NeedsLongContext bool               `json:"needs_long_context,omitempty"`
}

// DefaultTaskProfile returns the conservative baseline profile used when the
// host does not provide a more specific task profile.
func DefaultTaskProfile() TaskProfile {
	return TaskProfile{
		Source:      ProfileSourceDefault,
		Complexity:  ComplexityMedium,
		Latency:     LatencyNormal,
		FailureCost: FailureCostMedium,
		Privacy:     PrivacyCloudAllowed,
	}
}
