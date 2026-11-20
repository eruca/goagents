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
	// Overflow is set when the provider rejected the previous request of
	// this model call as too long for the model's context window
	// (ports.ErrContextTooLong). The projector should return a smaller view;
	// the call is retried with it.
	Overflow *ContextOverflow
}

// ContextOverflow describes a request the provider rejected as too long.
type ContextOverflow struct {
	// Attempt counts the rejections of this model call, from 1.
	Attempt int
	// RejectedChars is the size of the rejected request's messages: the
	// characters of their content, reasoning and tool call arguments, system
	// prompt included.
	RejectedChars int
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
	if err := s.project(ctx, state, nil); err != nil {
		return StageAbort, err
	}
	return StageContinue, nil
}

func (s ContextProjectionStage) project(ctx context.Context, state *RunState, overflow *ContextOverflow) error {
	if s.Projector == nil {
		state.ContextProjection = nil
		return nil
	}
	request := ContextProjectionRequest{
		// The projector sees the messages as ThinkStage will send them, so
		// reasoning that is not replayed does not count against a budget.
		Messages: replayView(state.Messages, s.ReasoningReplay),
		Budget:   s.Budget,
		Metadata: cloneMetadata(state.Metadata),
		Overflow: overflow,
	}
	if state.CompiledPrompt != nil {
		request.SystemPrompt = state.CompiledPrompt.Content
	}
	projected, err := s.Projector.Project(ctx, request)
	if err != nil {
		return err
	}
	if projected == nil {
		state.ContextProjection = nil
		return nil
	}
	state.ContextProjection = &ContextProjectionResult{
		Messages: append([]Message(nil), projected.Messages...),
		Metadata: cloneMetadata(projected.Metadata),
	}
	return nil
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
