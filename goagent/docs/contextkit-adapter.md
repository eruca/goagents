# ContextKit Adapter Sketch

`goagent` intentionally does not import `contextkit`. Applications that need
context compression should wire both modules at the host boundary:

```text
application
  imports github.com/eruca/goagents/goagent
  imports github.com/eruca/goagents/contextkit
  maps agentcore.Message to contextkit.Message
  returns agentcore.ContextProjectionResult
```

For local development before `contextkit` is published:

```bash
go mod edit -replace github.com/eruca/goagents/contextkit=/Users/nick/VibeCoding/goagents/contextkit
go get github.com/eruca/goagents/contextkit
```

Adapter shape:

```go
package app

import (
	"context"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/window"
	"github.com/eruca/goagents/goagent/agentcore"
)

type ContextKitProjector struct {
	Compressor interface {
		Compress(context.Context, contextkit.Request) (*contextkit.Result, error)
	}
}

func NewContextKitProjector(maxChars int) ContextKitProjector {
	return ContextKitProjector{
		Compressor: window.New(window.Config{
			Budget:    contextkit.Budget{MaxChars: maxChars},
			MinRecent: 8,
		}),
	}
}

func (p ContextKitProjector) Project(ctx context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	compressed, err := p.Compressor.Compress(ctx, contextkit.Request{
		Messages: toContextKit(req.Messages),
	})
	if err != nil {
		return nil, err
	}
	return &agentcore.ContextProjectionResult{
		Messages: fromContextKit(compressed.Messages),
		Metadata: map[string]any{
			"projector": "contextkit",
		},
	}, nil
}

func toContextKit(messages []agentcore.Message) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, contextkit.Message{
			Role:       contextkit.Role(msg.Role),
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		})
	}
	return out
}

func fromContextKit(messages []contextkit.Message) []agentcore.Message {
	out := make([]agentcore.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, agentcore.Message{
			Role:       string(msg.Role),
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		})
	}
	return out
}
```

Use it with `goagent`:

```go
agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithContextProjector(NewContextKitProjector(4000)),
)
```

The runnable `examples/context-projection` example uses a deterministic local
projector so the normal smoke suite does not require a cross-module dependency.
