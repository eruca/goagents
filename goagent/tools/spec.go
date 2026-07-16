package tools

import "github.com/eruca/goagents/goagent/ports"

type Spec = ports.ToolSpec

type ExecutionMode = ports.ExecutionMode

const (
	ExecutionModeAuto       = ports.ExecutionModeAuto
	ExecutionModeParallel   = ports.ExecutionModeParallel
	ExecutionModeSequential = ports.ExecutionModeSequential
	ExecutionModeExclusive  = ports.ExecutionModeExclusive
)
