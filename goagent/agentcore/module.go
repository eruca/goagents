package agentcore

import (
	"context"

	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

type SystemPromptProvider interface {
	SystemPrompt(ctx context.Context, req RunRequest) ([]prompt.Block, error)
}

// ToolProvider returns request-scoped tools.
// Providers used with approval checkpoints must recreate the same valid tools for the checkpoint request.
type ToolProvider interface {
	Tools(ctx context.Context, req RunRequest) ([]tools.Tool, error)
}

type Module interface {
	SystemPromptProvider
	SkillProvider
	ToolProvider
}
