package agentcore

import (
	"encoding/json"
	"time"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
)

type RunRequest struct {
	RunID              RunID
	Input              string
	UserID             string
	SessionID          string
	Metadata           map[string]any
	AllowedPermissions []policy.Permission
	PolicyContext      ports.PolicyContext
}

type RunResult struct {
	RunID   RunID
	Content string
	// Reasoning is the model's thinking behind the final answer, kept apart
	// from Content. Hosts decide whether to show or store it.
	Reasoning        string
	StructuredOutput json.RawMessage
	OutputMetadata   map[string]any
	Usage            Usage
	ExecutionSummary ExecutionSummary
	Interruption     *ToolApprovalInterruption
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	// ReasoningTokens is the part of OutputTokens spent on reasoning, when
	// the provider reports it.
	ReasoningTokens int
}

type ExecutionSummary struct {
	LLMCalls    int
	ToolCalls   int
	UsedTools   []string
	Duration    time.Duration
	AbortReason string
}

type Message struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []ports.ToolCall
	// Reasoning is the model's thinking behind an assistant message. It is
	// sent back to the model only as WithReasoningReplay allows, and never
	// stored in session memory.
	Reasoning string `json:",omitempty"`
}
