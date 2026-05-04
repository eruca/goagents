package agentcore

import "context"

type StageResult int

const (
	StageContinue StageResult = iota
	StageBreak
	StageAbort
)

type Stage interface {
	Name() string
	Run(ctx context.Context, state *RunState) (StageResult, error)
}
