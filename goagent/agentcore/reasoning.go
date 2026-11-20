package agentcore

// ReasoningReplay decides which of the model's earlier reasoning is sent back
// to it. Reasoning never reaches session memory or a run's final Content,
// whatever the mode.
type ReasoningReplay int

const (
	// ReasoningReplayNone sends no reasoning back. Most providers either
	// ignore earlier reasoning or reject it (DeepSeek), and a finished
	// step's reasoning only costs context.
	ReasoningReplayNone ReasoningReplay = iota
	// ReasoningReplayCurrentTurn sends back the reasoning of the assistant
	// messages after the last user message: the tool-call loop the model is
	// in, for providers that want it to continue its reasoning. The
	// provider must be set up to send it, for example openaiapi's
	// SendReasoning.
	ReasoningReplayCurrentTurn
)

// WithReasoningReplay sets which reasoning is sent back to the model; the
// default is ReasoningReplayNone.
func WithReasoningReplay(mode ReasoningReplay) Option {
	return func(a *Agent) {
		a.reasoningReplay = mode
	}
}

// replayView copies messages, keeping Reasoning only where mode replays it.
func replayView(messages []Message, mode ReasoningReplay) []Message {
	lastUser := -1
	if mode == ReasoningReplayCurrentTurn {
		for index, msg := range messages {
			if msg.Role == "user" {
				lastUser = index
			}
		}
	}
	view := append([]Message(nil), messages...)
	for index := range view {
		keep := mode == ReasoningReplayCurrentTurn && index > lastUser && view[index].Role == "assistant"
		if !keep {
			view[index].Reasoning = ""
		}
	}
	return view
}
