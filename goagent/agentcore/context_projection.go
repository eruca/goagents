package agentcore

import "context"

type ContextProjector interface {
	Project(ctx context.Context, req ContextProjectionRequest) (*ContextProjectionResult, error)
}

type ContextProjectionRequest struct {
	Messages []Message
	Budget   Budget
	Metadata map[string]any
}

type ContextProjectionResult struct {
	Messages []Message
	Metadata map[string]any
}

type ContextProjectionStage struct {
	Projector ContextProjector
	Budget    Budget
}

func (s ContextProjectionStage) Name() string {
	return "context_projection"
}

func (s ContextProjectionStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Projector == nil {
		state.ContextProjection = nil
		return StageContinue, nil
	}
	projected, err := s.Projector.Project(ctx, ContextProjectionRequest{
		Messages: append([]Message(nil), state.Messages...),
		Budget:   s.Budget,
		Metadata: cloneMetadata(state.Metadata),
	})
	if err != nil {
		return StageAbort, err
	}
	if projected == nil {
		state.ContextProjection = nil
		return StageContinue, nil
	}
	state.ContextProjection = &ContextProjectionResult{
		Messages: append([]Message(nil), projected.Messages...),
		Metadata: cloneMetadata(projected.Metadata),
	}
	return StageContinue, nil
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for k, v := range metadata {
		copied[k] = v
	}
	return copied
}
