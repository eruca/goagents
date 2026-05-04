package contextkit

import "context"

type Request struct {
	Messages []Message      `json:"messages"`
	Budget   Budget         `json:"budget"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type Result struct {
	Messages    []Message      `json:"messages"`
	Profile     Profile        `json:"profile"`
	Levels      []Level        `json:"levels"`
	Summary     string         `json:"summary,omitempty"`
	Dropped     int            `json:"dropped"`
	Collapses   []Collapse     `json:"collapses,omitempty"`
	AutoCompact *Compact       `json:"auto_compact,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
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
	Collapsed []Message      `json:"collapsed"`
	Recent    []Message      `json:"recent"`
	Budget    Budget         `json:"budget"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type SummarizeResult struct {
	Summary  string         `json:"summary"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func RestoreProjection(messages []Message, collapses []Collapse) []Message {
	if len(collapses) == 0 {
		return cloneMessages(messages)
	}
	byID := make(map[string]Collapse, len(collapses))
	for _, collapse := range collapses {
		if collapse.ID != "" {
			byID[collapse.ID] = collapse
		}
	}

	restored := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if id, ok := collapseIDFromMessage(msg); ok {
			if collapse, found := byID[id]; found {
				restored = append(restored, cloneMessages(collapse.Originals)...)
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
