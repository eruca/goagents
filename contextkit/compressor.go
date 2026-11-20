package contextkit

import "context"

type Request struct {
	Messages []Message      `json:"messages"`
	Budget   Budget         `json:"budget"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type Result struct {
	Messages []Message `json:"messages"`
	Profile  Profile   `json:"profile"`
	Levels   []Level   `json:"levels"`
	Summary  string    `json:"summary,omitempty"`
	Dropped  int       `json:"dropped"`
	// OverBudget reports that system messages and the kept conversation
	// still exceed the budget, because the newest turn alone does.
	OverBudget  bool           `json:"over_budget,omitempty"`
	Collapses   []Collapse     `json:"collapses,omitempty"`
	AutoCompact *Compact       `json:"auto_compact,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Applied lists the steps this compression took, cheapest first: see
	// the Step constants.
	Applied []Step `json:"applied,omitempty"`
	// Cleared holds, as they were before, the messages whose tool result or
	// reasoning this compression cleared, so a host can keep them.
	Cleared []Message `json:"cleared,omitempty"`
	// SummaryFallback reports that the Summarizer failed or was paused and
	// the placeholder summary was used instead.
	SummaryFallback bool `json:"summary_fallback,omitempty"`
}

// Step is one stage of compression, applied only while the projection does
// not fit, in this order.
type Step string

const (
	// StepToolBudget truncates a tool result over the per-result limit,
	// offloading its full content when an OffloadStore is set. It applies
	// whether or not the projection fits.
	StepToolBudget Step = "tool_budget"
	// StepClearReasoning clears the reasoning of older assistant messages.
	StepClearReasoning Step = "clear_reasoning"
	// StepClearToolResults clears older tool results, oldest first,
	// keeping the tool call and its reference.
	StepClearToolResults Step = "clear_tool_results"
	// StepWindow drops older steps behind a placeholder summary.
	StepWindow Step = "window"
	// StepSummarize has the Summarizer write that summary.
	StepSummarize Step = "summarize"
	// StepTruncate shortens the longest tool results and reasoning of the
	// kept messages when the newest step alone does not fit.
	StepTruncate Step = "truncate"
)

// OffloadStore keeps the full content of a tool result that compression
// truncates or clears, and returns a reference the model can use to read it
// again through a host tool. Offload should be idempotent for the same
// message: an uncached compression offloads it again.
type OffloadStore interface {
	Offload(ctx context.Context, msg Message) (ref string, err error)
}

type Collapse struct {
	ID          string    `json:"id"`
	StartIndex  int       `json:"start_index"`
	EndIndex    int       `json:"end_index"`
	Summary     Message   `json:"summary"`
	OriginalIDs []string  `json:"original_ids"`
	Originals   []Message `json:"originals,omitempty"`
}

type Compact struct {
	Summary  string         `json:"summary"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type Compressor interface {
	Compress(ctx context.Context, req Request) (*Result, error)
}

type Summarizer interface {
	Summarize(ctx context.Context, req SummarizeRequest) (*SummarizeResult, error)
}

type SummarizeRequest struct {
	// Previous is the summary an earlier Compress produced; fold Collapsed
	// into it rather than summarizing from scratch.
	Previous  string    `json:"previous,omitempty"`
	Collapsed []Message `json:"collapsed"`
	Recent    []Message `json:"recent"`
	// Budget is the whole budget the projection must fit.
	Budget Budget `json:"budget"`
	// SummaryBudget is what the summary may take of it; a longer summary is
	// truncated to fit. Zero limits mean no limit.
	SummaryBudget Budget         `json:"summary_budget"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type SummarizeResult struct {
	Summary  string         `json:"summary"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// RestoreProjection replaces each collapse summary in messages with the
// messages it collapsed, recursively, so chained incremental collapses expand
// back to the full history.
func RestoreProjection(messages []Message, collapses []Collapse) []Message {
	byID := make(map[string]Collapse, len(collapses))
	for _, collapse := range collapses {
		if collapse.ID != "" {
			byID[collapse.ID] = collapse
		}
	}
	return restore(messages, byID, map[string]bool{})
}

func restore(messages []Message, byID map[string]Collapse, expanding map[string]bool) []Message {
	restored := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if id, ok := collapseIDFromMessage(msg); ok && !expanding[id] {
			if collapse, found := byID[id]; found {
				expanding[id] = true
				restored = append(restored, restore(collapse.Originals, byID, expanding)...)
				delete(expanding, id)
				continue
			}
		}
		restored = append(restored, cloneMessage(msg))
	}
	return restored
}

func collapseIDFromMessage(msg Message) (string, bool) {
	if len(msg.Metadata) == 0 {
		return "", false
	}
	id, ok := msg.Metadata["contextkit.collapse_id"].(string)
	return id, ok && id != ""
}

func cloneMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, cloneMessage(msg))
	}
	return out
}

func cloneMessage(msg Message) Message {
	if len(msg.ToolCalls) > 0 {
		msg.ToolCalls = append([]ToolCall(nil), msg.ToolCalls...)
	} else {
		msg.ToolCalls = nil
	}
	if len(msg.Images) > 0 {
		msg.Images = append([]Image(nil), msg.Images...)
	} else {
		msg.Images = nil
	}
	if len(msg.Metadata) == 0 {
		msg.Metadata = nil
		return msg
	}
	copied := make(map[string]any, len(msg.Metadata))
	for k, v := range msg.Metadata {
		copied[k] = v
	}
	msg.Metadata = copied
	return msg
}
