package ports

import "context"

type PromptMode string

const (
	PromptModeCacheable PromptMode = "cacheable"
	PromptModeDynamic   PromptMode = "dynamic"
)

type PromptBlock struct {
	Name     string
	Mode     PromptMode
	Priority int
	Content  string
}

type CompiledPrompt struct {
	Blocks  []PromptBlock
	Content string
}

type PromptCompiler interface {
	Compile(ctx context.Context, blocks []PromptBlock) (*CompiledPrompt, error)
}
