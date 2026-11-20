package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/eruca/goagents/contextkit"
	contextagentadapter "github.com/eruca/goagents/contextkit/agentadapter"
	"github.com/eruca/goagents/contextkit/archive"
	"github.com/eruca/goagents/contextkit/window"
	"github.com/eruca/goagents/goagent/agentcore"
)

const (
	contextMaxCharsEnv     = "HOST_API_CONTEXT_MAX_CHARS"
	contextMaxToolCharsEnv = "HOST_API_CONTEXT_MAX_TOOL_CHARS"
)

// parseContextLimit reads a non-negative character limit; empty means 0.
func parseContextLimit(getenv func(string) string, name string) (int, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return limit, nil
}

// contextScope keeps each workflow's archived context apart.
func contextScope(metadata map[string]any) string {
	id, _ := metadata["workflow_id"].(string)
	return id
}

// newContextCompression returns the projector that compresses each agent
// model request to config.ContextMaxChars, the compiled system prompt
// included, before handing the messages to nextProjector (the memory
// projector, when memory is enabled), and the tool provider that lets the
// model read back what compression removed, followed by nextTools. Removed
// content is kept in a process-local archive per workflow. It returns the
// next projector and tools unchanged when compression is off.
func newContextCompression(config Config, nextProjector agentcore.ContextProjector, nextTools agentcore.ToolProvider) (agentcore.ContextProjector, agentcore.ToolProvider, error) {
	if config.ContextMaxChars <= 0 {
		return nextProjector, nextTools, nil
	}
	store := archive.New(archive.Config{})
	projector, err := contextagentadapter.NewProjector(contextagentadapter.Config{
		Compressor: window.New(window.Config{
			Budget:       contextkit.Budget{MaxChars: config.ContextMaxChars},
			Profile:      contextkit.ProfileStandard,
			MaxToolChars: config.ContextMaxToolChars,
			Offload:      store,
		}),
		Next:  nextProjector,
		Scope: contextScope,
	})
	if err != nil {
		return nil, nil, err
	}
	tools, err := contextagentadapter.NewContextTools(contextagentadapter.ContextToolsConfig{
		Archive: store, Scope: contextScope, Next: nextTools,
	})
	if err != nil {
		return nil, nil, err
	}
	return projector, tools, nil
}
