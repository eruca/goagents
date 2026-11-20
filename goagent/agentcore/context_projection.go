package agentcore

import "context"

type ContextProjector interface {
	Project(ctx context.Context, req ContextProjectionRequest) (*ContextProjectionResult, error)
}

type ContextProjectionRequest struct {
	Messages []Message
	Budget   Budget
	Metadata map[string]any
	// SystemPrompt is the compiled system prompt the model call will start
	// with. ThinkStage adds it after projection, so a projector must not add
	// it to Messages; one that sizes the view to a budget should count it.
	SystemPrompt string
}

type ContextProjectionResult struct {
	Messages []Message
	Metadata map[string]any
}

type ContextProjectionStage struct {
	Projector       ContextProjector
	Budget          Budget
	ReasoningReplay ReasoningReplay
}

func (s ContextProjectionStage) Name() string {
	return "context_projection"
}

func (s ContextProjectionStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Projector == nil {
		state.ContextProjection = nil
		return StageContinue, nil
	}
	request := ContextProjectionRequest{
		// The projector sees the messages as ThinkStage will send them, so
		// reasoning that is not replayed does not count against a budget.
		Messages: replayView(state.Messages, s.ReasoningReplay),
		Budget:   s.Budget,
		Metadata: cloneMetadata(state.Metadata),
	}
	if state.CompiledPrompt != nil {
		request.SystemPrompt = state.CompiledPrompt.Content
	}
	projected, err := s.Projector.Project(ctx, request)
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
