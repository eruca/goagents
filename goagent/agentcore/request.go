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
	RunID            RunID
	Content          string
	StructuredOutput json.RawMessage
	OutputMetadata   map[string]any
	Usage            Usage
	ExecutionSummary ExecutionSummary
	Interruption     *ToolApprovalInterruption
}

type Usage struct {
	InputTokens  int
	OutputTokens int
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
}
