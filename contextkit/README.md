# ContextKit

`contextkit` is a standalone Go module for context compression. It belongs in
the broader `goagents` workspace, but it does not import `goagent`. Applications
map their runtime messages into `contextkit.Message`, compress them, and decide
how to use the returned projection.

## Install

```bash
go get github.com/eruca/goagents/contextkit
```

For local development before publishing:

```bash
go mod edit -replace github.com/eruca/goagents/contextkit=/Users/nick/VibeCoding/goagents/contextkit
go get github.com/eruca/goagents/contextkit
```

## Compression Profile

The environment variable is a single deep-compression flag:

```bash
CONTEXT_DEEP_COMPRESSION=1
```

Unset or any value other than `1`, `true`, `yes`, or `on` enables levels 1-3.
Setting `CONTEXT_DEEP_COMPRESSION=1` enables levels 1-5.

`CONTEXT_DEEP_COMPRESSION` names the behavior directly: the normal path still
compresses context, while the flag opts into the deeper, more invasive layers.

## Five Levels

`standard` profile:

1. Tool observation projection: tool messages become structured observations
   containing tool name, status, result reference, and bounded result preview.
2. Message window pruning: system messages and recent conversation are retained.
3. Projection: the returned message slice is a model-facing view, not a mutation
   of the caller's original session.

`deep` profile adds:

4. Reversible collapse: collapsed messages are recorded with a stable collapse
   ID, original IDs, original messages, and summary placeholder.
5. Auto compact: when a `Summarizer` is configured, deep mode uses it to build a
   stronger compact summary and places that summary in the model projection.

No LLM dependency is required. Hosts can provide a deterministic summarizer, an
LLM summarizer, or no summarizer. Without a summarizer, deep mode still returns
collapse and compact metadata using deterministic summaries.

## Window Compressor

```go
compressor := window.New(window.Config{
	Budget:       contextkit.Budget{MaxChars: 4000},
	MinRecent:    8,
	MaxToolChars: 1000,
})

result, err := compressor.Compress(ctx, contextkit.Request{
	Messages: messages,
})
if err != nil {
	panic(err)
}

modelMessages := result.Messages
```

If `Profile` is omitted from `window.Config`, the compressor reads
`CONTEXT_DEEP_COMPRESSION`.

## Deep Summarization

Deep mode can use a host-owned summarizer:

```go
type summarizer struct{}

func (summarizer) Summarize(ctx context.Context, req contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	return &contextkit.SummarizeResult{
		Summary: "Primary request: ...\nCurrent state: ...\nPending tasks: ...",
	}, nil
}

compressor := window.New(window.Config{
	Profile:    contextkit.ProfileDeep,
	MinRecent:  8,
	Summarizer: summarizer{},
})
```

The summarizer receives collapsed messages, recent messages, budget, and
metadata. The returned summary replaces the generic collapse placeholder in the
model-facing projection:

```text
auto compacted earlier context:
Primary request: ...
Current state: ...
Pending tasks: ...
```

`Result.Collapses` still keeps the original collapsed messages, so hosts can
recover or inspect the pre-compact history.

## Restore A Projection

Deep collapse records are reversible:

```go
restored := contextkit.RestoreProjection(result.Messages, result.Collapses)
```

This expands collapse summary messages back to their original messages when the
projection contains `contextkit.collapse_id` metadata.

## Tool Result Budget

```go
budgeted := toolbudget.Apply(rawToolOutput, toolbudget.Config{MaxChars: 1000})
fmt.Println(budgeted.Content)
```

Use this at tool boundaries when a host wants explicit level-1 control before
messages are assembled.

## Tool Observation Projection

`toolprojection` is the recommended Level 1 primitive for agent traces:

```go
projected := toolprojection.Project(contextkit.Message{
	Role:       contextkit.RoleTool,
	ToolName:   "ocr_document",
	ToolCallID: "call-1",
	Status:     "success",
	Ref:        "ocr:run-1",
	Content:    fullToolResult,
}, toolprojection.Config{MaxResultChars: 1000})
```

The projected content is shaped for model context:

```text
tool=ocr_document
status=success
tool_call_id=call-1
ref=ocr:run-1
result=...
```

This removes noisy tool reasoning and raw payloads from the model-facing view
while preserving the useful observation.

## Use With goagent

Keep the dependency direction at the application boundary:

```text
application
  imports github.com/eruca/goagents/goagent
  imports github.com/eruca/goagents/contextkit
  maps goagent messages into contextkit messages
  uses contextkit output as the model-facing projection

github.com/eruca/goagents/goagent does not import github.com/eruca/goagents/contextkit
github.com/eruca/goagents/contextkit does not import github.com/eruca/goagents/goagent
```

Until `goagent` has a native context compression port, use `contextkit` in host
code around memory or model-request preparation. A future `goagent` stage can
call the same compressor before `ThinkStage` without moving the algorithms into
core.

Typical mapping:

```go
func toContextKit(messages []agentcore.Message) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, contextkit.Message{
			Role:    contextkit.Role(msg.Role),
			Content: msg.Content,
		})
	}
	return out
}
```

For large tools such as OCR or retrieval, pair `contextkit` with bounded tool
results:

- return a preview in `tools.Result.ForLLM`
- keep full output in `tools.Result.ForUser`, object storage, or a retrieval index
- expose bounded read/search tools for follow-up access

## Verify

```bash
go test ./...
```
