package agentcore

import (
	"context"
	"errors"

	"github.com/eruca/goagent/ports"
)

var ErrInputRejected = errors.New("input rejected")

// InputGuardRequest is the host-visible request boundary before memory, context, model, or tools run.
type InputGuardRequest struct {
	RunID         RunID
	Input         string
	Metadata      map[string]any
	PolicyContext ports.PolicyContext
}

// InputGuard screens one raw run request. It must not call tools or mutate external state.
type InputGuard interface {
	ValidateInput(context.Context, InputGuardRequest) error
}

// InputGuardFunc adapts a function to InputGuard.
type InputGuardFunc func(context.Context, InputGuardRequest) error

func (f InputGuardFunc) ValidateInput(ctx context.Context, req InputGuardRequest) error {
	return f(ctx, req)
}

// InputGuardStage runs the host guard once before memory and context are populated.
type InputGuardStage struct {
	Guard InputGuard
}

func (s InputGuardStage) Name() string {
	return "input_guard"
}

func (s InputGuardStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Guard == nil || state.Metadata[inputGuardValidatedKey] == true {
		return StageContinue, nil
	}
	request := InputGuardRequest{
		RunID:    state.RunID,
		Input:    state.Input.Input,
		Metadata: cloneMetadata(state.Input.Metadata),
		PolicyContext: ports.PolicyContext{
			TenantID:  state.Input.PolicyContext.TenantID,
			RequestID: state.Input.PolicyContext.RequestID,
			TraceID:   state.Input.PolicyContext.TraceID,
			Labels:    cloneStringMap(state.Input.PolicyContext.Labels),
		},
	}
	if err := s.Guard.ValidateInput(ctx, request); err != nil {
		state.Emit(ctx, Event{Type: EventInputRejected})
		return StageAbort, ErrInputRejected
	}
	if state.Metadata == nil {
		state.Metadata = make(map[string]any)
	}
	state.Metadata[inputGuardValidatedKey] = true
	state.Emit(ctx, Event{Type: EventInputValidated})
	return StageContinue, nil
}

const inputGuardValidatedKey = "agentcore.input_guard.validated"
