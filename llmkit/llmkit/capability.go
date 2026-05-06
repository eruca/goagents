package llmkit

// PriceClass describes the relative cost of using a model.
type PriceClass string

const (
	PriceFree   PriceClass = "free"
	PriceLow    PriceClass = "low"
	PriceMedium PriceClass = "medium"
	PriceHigh   PriceClass = "high"
)

// CapabilityLevel describes the broad capability tier of a model.
type CapabilityLevel string

const (
	CapabilitySimple   CapabilityLevel = "simple"
	CapabilityBalanced CapabilityLevel = "balanced"
	CapabilityAdvanced CapabilityLevel = "advanced"
)

// ContextWindowClass describes relative context-window size.
type ContextWindowClass string

const (
	ContextShort  ContextWindowClass = "short"
	ContextMedium ContextWindowClass = "medium"
	ContextLong   ContextWindowClass = "long"
)

// LatencyClass describes observed or configured model latency.
type LatencyClass string

const (
	LatencySlowClass   LatencyClass = "slow"
	LatencyNormalClass LatencyClass = "normal"
	LatencyFastClass   LatencyClass = "fast"
)

// ModelCapability is the static and current-runtime capability surface needed
// by deterministic routing. Task 4 will own route scoring and selection.
type ModelCapability struct {
	Alias               string             `json:"alias,omitempty"`
	Provider            string             `json:"provider,omitempty"`
	IsLocal             bool               `json:"is_local,omitempty"`
	CapabilityLevel     CapabilityLevel    `json:"capability_level"`
	SupportsTools       bool               `json:"supports_tools,omitempty"`
	SupportsJSON        bool               `json:"supports_json,omitempty"`
	ContextWindowClass  ContextWindowClass `json:"context_window_class"`
	PriceClass          PriceClass         `json:"price_class"`
	LatencyClass        LatencyClass       `json:"latency_class"`
	EstimatedCents      int                `json:"estimated_cents,omitempty"`
	MaxConcurrency      int                `json:"max_concurrency,omitempty"`
	CurrentConcurrency  int                `json:"current_concurrency,omitempty"`
	RecentFailureCount  int                `json:"recent_failure_count,omitempty"`
	RecentFailureRate   float64            `json:"recent_failure_rate,omitempty"`
	RecentLatencyMillis int                `json:"recent_latency_ms,omitempty"`
}

// Matches reports whether the model can legally handle the profile. It only
// applies hard capability filters; preference and scoring are routing concerns.
func (m ModelCapability) Matches(profile TaskProfile) bool {
	if profile.Privacy == PrivacyLocalOnly && !m.IsLocal {
		return false
	}
	if profile.NeedsJSON && !m.SupportsJSON {
		return false
	}
	if profile.NeedsTools && !m.SupportsTools {
		return false
	}
	if profile.NeedsLongContext && contextRank(m.ContextWindowClass) < contextRank(ContextLong) {
		return false
	}
	if capabilityRank(m.CapabilityLevel) < requiredCapabilityRank(profile.Complexity) {
		return false
	}
	if m.MaxConcurrency > 0 && m.CurrentConcurrency >= m.MaxConcurrency {
		return false
	}
	return true
}

func requiredCapabilityRank(complexity Complexity) int {
	switch complexity {
	case ComplexityHard:
		return capabilityRank(CapabilityAdvanced)
	case ComplexityMedium:
		return capabilityRank(CapabilityBalanced)
	default:
		return capabilityRank(CapabilitySimple)
	}
}

func capabilityRank(level CapabilityLevel) int {
	switch level {
	case CapabilityAdvanced:
		return 3
	case CapabilityBalanced:
		return 2
	case CapabilitySimple:
		return 1
	default:
		return 0
	}
}

func contextRank(class ContextWindowClass) int {
	switch class {
	case ContextLong:
		return 3
	case ContextMedium:
		return 2
	case ContextShort:
		return 1
	default:
		return 0
	}
}
