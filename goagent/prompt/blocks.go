package prompt

import "github.com/eruca/goagents/goagent/ports"

type Mode = ports.PromptMode

const (
	ModeCacheable = ports.PromptModeCacheable
	ModeDynamic   = ports.PromptModeDynamic
)

type Block = ports.PromptBlock

type Compiled = ports.CompiledPrompt
