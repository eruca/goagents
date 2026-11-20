package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/eruca/goagents/contextkit"
	contextagentadapter "github.com/eruca/goagents/contextkit/agentadapter"
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

// newContextProjector compresses each agent model request to
// config.ContextMaxChars with contextkit, counting the compiled system
// prompt, and then hands the compressed messages to next (the memory
// projector, when memory is enabled). It returns next unchanged when
// compression is off.
func newContextProjector(config Config, next agentcore.ContextProjector) (agentcore.ContextProjector, error) {
	if config.ContextMaxChars <= 0 {
		return next, nil
	}
	return contextagentadapter.NewProjector(contextagentadapter.Config{
		Compressor: window.New(window.Config{
			Budget:       contextkit.Budget{MaxChars: config.ContextMaxChars},
			Profile:      contextkit.ProfileStandard,
			MaxToolChars: config.ContextMaxToolChars,
		}),
		Next: next,
	})
}
