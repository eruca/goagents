package agentcore

import "context"

type Pipeline struct {
	stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: append([]Stage(nil), stages...)}
}

func (p *Pipeline) Run(ctx context.Context, state *RunState) (StageResult, error) {
	for _, stage := range p.stages {
		state.Emit(ctx, Event{Type: EventStageStarted, Stage: stage.Name()})
		result, err := stage.Run(ctx, state)
		if err != nil {
			state.Emit(ctx, Event{Type: EventStageFailed, Stage: stage.Name(), Message: err.Error()})
			return StageAbort, err
		}
		state.Emit(ctx, Event{Type: EventStageCompleted, Stage: stage.Name()})
		if result != StageContinue {
			return result, nil
		}
	}
	return StageContinue, nil
}
