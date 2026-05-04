package agentcore

import (
	"context"

	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

type SystemPromptProvider interface {
	SystemPrompt(ctx context.Context, req RunRequest) ([]prompt.Block, error)
}

type ToolProvider interface {
	Tools(ctx context.Context, req RunRequest) ([]tools.Tool, error)
}

type Module interface {
	SystemPromptProvider
	SkillProvider
	ToolProvider
}
