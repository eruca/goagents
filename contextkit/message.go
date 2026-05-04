package contextkit

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	ID         string         `json:"id,omitempty"`
	Role       Role           `json:"role"`
	Content    string         `json:"content"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Status     string         `json:"status,omitempty"`
	Ref        string         `json:"ref,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

func (m Message) CharLen() int {
	return len([]rune(m.Content))
}

type Budget struct {
	MaxChars int `json:"max_chars"`
}
