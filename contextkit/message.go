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
	Images     []Image        `json:"images,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Status     string         `json:"status,omitempty"`
	Ref        string         `json:"ref,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Image references an image attached to a message. contextkit never reads the
// image: the host resolves the ID, counts its tokens, and decides what a
// summary keeps of it.
type Image struct {
	ID string `json:"id"`
}

// CharLen counts the runes of Content; images add nothing.
func (m Message) CharLen() int {
	return len([]rune(m.Content))
}

// Budget bounds the model-facing view. Each positive limit must hold; zero
// means no limit of that kind. MaxTokens needs a TokenCounter.
type Budget struct {
	MaxChars  int `json:"max_chars"`
	MaxTokens int `json:"max_tokens,omitempty"`
}

// Bounded reports whether the budget sets any limit.
func (b Budget) Bounded() bool {
	return b.MaxChars > 0 || b.MaxTokens > 0
}

// TokenCounter returns the tokens a message costs the model, images included.
type TokenCounter func(Message) int
