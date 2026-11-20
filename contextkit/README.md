# ContextKit

`contextkit` is a standalone Go module for context compression. It belongs in
the broader `goagents` workspace, but it does not import `goagent`. Applications
map their runtime messages into `contextkit.Message`, compress them, and decide
how to use the returned projection.

## Install

```bash
go get github.com/eruca/goagents@latest
```

Then import `github.com/eruca/goagents/contextkit`.

## Compression Profile

The environment variable is a single deep-compression flag:

```bash
CONTEXT_DEEP_COMPRESSION=1
```

Unset or any value other than `1`, `true`, `yes`, or `on` enables levels 1-3.
Setting `CONTEXT_DEEP_COMPRESSION=1` enables levels 1-5.

`CONTEXT_DEEP_COMPRESSION` names the behavior directly: the normal path still
compresses context, while the flag opts into the deeper, more invasive layers.
The compressor does not read it on its own: pass
`Profile: contextkit.ProfileFromEnv()` to `window.Config` to follow it. An
empty `Profile` means `ProfileStandard`.

## Compression Steps

`window.Compressor` fits a conversation into its budget in steps, cheapest
first, and stops as soon as the projection fits, in the spirit of Claude
Code's context management. `Result.Applied` lists the steps a compression
took.

| Step | What it does | When |
|---|---|---|
| `tool_budget` | Projects each tool message (`tool=… status=… ref=… result=…`) and truncates a result over `MaxToolChars`, after handing its full content to `Offload` when one is set | always |
| `clear_reasoning` | Clears the reasoning of all but the newest `KeepRecentReasoning` (default 1) assistant messages | over budget |
| `clear_tool_results` | Clears tool results oldest first, keeping the newest `KeepRecentToolResults` (default 3). Once it has to start, it clears down to `ClearTarget` (default 70%) of the budget, so the following calls keep the same cleared prefix, which keeps provider prompt caches valid. A cleared result keeps its tool call ID and reference: `result=[older result cleared: N chars; read ref … to see it again]` | still over |
| `window` | Drops older steps behind one summary message (see Window Compressor) | still over |
| `summarize` | In deep mode, the `Summarizer` writes that summary. The window is then cut to `CompactTarget` (default half) of its space, so the following calls fit without summarizing again | deep, still over |
| `truncate` | When the newest step alone does not fit, halves its longest tool results and reasoning until it does. User messages and assistant answers are never cut | last resort |

`Result.Cleared` holds the originals of everything cleared or truncated, and
deep-mode `Collapses` hold dropped messages, so nothing is lost to the host.

The `standard` profile stops at the placeholder summary; `deep` adds
reversible collapses and the `Summarizer`. No LLM dependency is required:
without a summarizer, deep mode still records the collapse with the
placeholder summary.

A Summarizer that fails does not fail the compression: the placeholder is
used and `Result.SummaryFallback` is set. After `SummarizerFailureLimit`
(default 3) consecutive failures it is paused for `SummarizerCooldown`
(default 5 minutes). A canceled context still fails the compression.

## Summary Catalog And Archive

The summary of dropped steps carries a catalog of them, newest last, so the
model knows what happened and where to find it:

```text
compressed earlier context: 14 messages omitted
earlier steps (newest last; read a ref to see it again):
- (3 earlier entries not listed)
- user: "total the invoices"
- ocr {"page": 4} → 2380 chars ref=ctx:9f2c1a7e5b3d0c44
- assistant: "Pages 1-4 read; totals so far 1000 yuan" ref=ctx:51e0…
```

The catalog keeps the newest `CatalogEntries` (default 12) entries across
compressions and takes at most a tenth of the budget, so a small budget keeps
conversation instead of a list of what it dropped.

An `Archive` (`contextkit.Archive`: `Offload`, `Read`, `Search`) set as
`Offload` keeps everything compression removes: truncated and cleared tool
results, cleared reasoning, and dropped steps. `contextkit/archive.Memory` is
a process-local implementation with bounded scopes and a lexical, IDF-weighted
search over words and Chinese/Japanese bigrams. Archives are scoped by
`contextkit.WithArchiveScope`, so one conversation never reads another's
content.

With goagent, give the projector a `Scope` (for example the session or
workflow ID from run metadata) and add `agentadapter.ContextTools`, which
offers the model `context_read` (read back by ref) and `context_search`
(find archived content). The model pulls what it needs instead of the host
pushing guesses into the context; results are marked as untrusted data, and
content read back is not truncated or archived again.

```go
store := archive.New(archive.Config{})
projector, _ := agentadapter.NewProjector(agentadapter.Config{
	Compressor: window.New(window.Config{Budget: budget, MaxToolChars: 2000, Offload: store}),
	Scope:      sessionID,
})
tools, _ := agentadapter.NewContextTools(agentadapter.ContextToolsConfig{Archive: store, Scope: sessionID})
agent, _ := agentcore.NewAgent(agentcore.WithContextProjector(projector), agentcore.WithToolProvider(tools), ...)
```

An `OffloadStore` alone keeps full tool results out of the context:

```go
type OffloadStore interface {
	Offload(ctx context.Context, msg contextkit.Message) (ref string, err error)
}
```

The host stores the content, for example as an artifact, and gives the model
a tool to read a reference back. A failing store falls back to truncation.
Offload should be idempotent, since an uncached compression offloads a result
again.

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

The window is chosen from the newest message backwards. It always holds the
last `MinRecent` messages (default 1) and extends over earlier steps while
system messages, the kept steps, and the summary of the rest fit the budget.
A step starts at a user message, or, within the last turn, at an assistant
message, which an agent run extends with tool calls; a tool reply always stays
in the step of the call it answers. When the window starts inside the last
turn, the turn's user message is kept too, ahead of the window, so the model
keeps the request it is working on. A conversation that fits whole is kept
whole, with no summary. Without a budget nothing is dropped. When the newest
step alone is too large it is kept anyway and `Result.OverBudget` is set.

The summary counts against the budget, and `Result.OverBudget` reports
whether the whole projection, summary included, exceeds it. Without a
`Summarizer` the window leaves room for the placeholder it writes; the
placeholder names at most the five latest dropped message IDs. With a
`Summarizer`, `Config.SummaryReserve` (default: a fifth of each budget limit)
is kept for the summary, passed to the summarizer as
`SummarizeRequest.SummaryBudget`, and a longer summary is truncated to fit.

`CharLen` counts a message's content and its tool calls' names and arguments.

## Token Budget

A model's window is measured in tokens, and the ratio of characters to tokens
varies with the language. Set `Budget.MaxTokens` and give the compressor the
host's own counter:

```go
compressor := window.New(window.Config{
	Budget:      contextkit.Budget{MaxTokens: 52000},
	MinRecent:   2,
	CountTokens: func(msg contextkit.Message) int { return tokenizer.Count(msg) },
})
```

Each positive limit in a `Budget` must hold, so `MaxChars` and `MaxTokens` can
be combined. `CountTokens` runs only when `MaxTokens` is set; it sees tool
messages after projection. A `MaxTokens` budget without `CountTokens` fails
with `window.ErrTokenCounterRequired` rather than estimating tokens from
characters. A `Request.Budget` that sets any limit replaces the configured
budget whole; limits it leaves at zero do not fall back to the config.

## Images

`Message.Images` holds references to images attached to a message. contextkit
never reads them: `CharLen` ignores them, so a budget that must account for
images needs `MaxTokens` and a `CountTokens` that prices them. When a message
with images is dropped:

- the standard placeholder counts them, for example
  `compressed earlier context: 3 messages omitted (2 images)`, and carries the
  count across incremental compressions;
- a deep `Summarizer` receives them in `SummarizeRequest.Collapsed`, so the
  host can have a vision model describe them in the summary or note them;
- collapse records keep them, so `RestoreProjection` returns them.

Messages are copied at every boundary; editing what a summarizer or a result
holds does not change the caller's messages.

Compression is incremental: keep `result.Messages` as the session transcript
and append new messages to it. The next `Compress` recognizes its own summary,
summarizes only the newly dropped messages together with it, and carries the
total dropped count forward. A summary is recognized by its metadata or, when
a host stored only role and content, by its header (`compressed earlier
context: N messages omitted` or `summary of earlier context (N messages
omitted):`), so summaries do not pile up. Passing the full raw history each
time works too, but re-summarizes everything.

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

The summarizer receives the previous summary (`Previous`, empty on the first
compression), the newly collapsed messages, recent messages, budget, and
metadata. It should fold the collapsed messages into the previous summary. The
returned summary replaces the generic placeholder in the model-facing
projection:

```text
summary of earlier context (12 messages omitted):
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
projection contains `contextkit.collapse_id` metadata. Pass every collapse
recorded across incremental compressions: each collapse keeps the previous
summary as one of its originals, so the expansion recurses back to the full
history.

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

`contextkit/agentadapter` is a goagent `ContextProjector` backed by a
compressor, so every model request of a run uses the compressed view:

```go
projector, err := agentadapter.NewProjector(agentadapter.Config{
	Compressor: window.New(window.Config{
		Budget:       contextkit.Budget{MaxTokens: 52000},
		CountTokens:  countTokens,
		Profile:      contextkit.ProfileDeep,
		MaxToolChars: 2000,
		Summarizer:   summarizer,
	}),
	Next: memoryProjector, // optional, for example memorykit's projector
})
agent, err := agentcore.NewAgent(agentcore.WithContextProjector(projector), ...)
```

- Messages map to contextkit with their tool calls, and tool replies are
  named after the call they answer; `ToContextKit` and `FromContextKit` do
  the mapping.
- goagent projects the whole run before every model call. The projector
  remembers recent compressions (`CacheSize`, default 32) by a hash of the
  messages they covered, and compresses a remembered projection plus the new
  messages, so a `Summarizer` folds in only newly dropped messages. In a test
  run of eight tool steps with large outputs, the last model request shrank
  from 80,019 to 2,364 characters under a 3,000-character budget, and each
  message was summarized once.
- `Next` receives the compressed messages; putting a memory projector there
  keeps recalled memory out of the summaries.
- `UseAgentBudget` compresses to the run's `Budget.MaxInputTokens` (the
  compressor then needs `CountTokens`).
- The budget covers the compiled system prompt: goagent passes it as the
  request's `SystemPrompt`, the projector counts it as a leading system
  message, and leaves it out of the projection because goagent adds it to the
  model call. Tool schemas and whatever `Next` adds are not counted.
- `OnCompress` receives each `Result`, for example to keep deep-mode
  `Collapses`. The result metadata reports `contextkit.dropped`,
  `contextkit.over_budget`, and `contextkit.reused_messages`.
- When the provider rejects a call as too long for the model's context window
  (`ports.ErrContextTooLong`), goagent projects again with the request's
  `Overflow` set and retries, at most twice. The projector compresses to 70%
  of the rejected request's characters and keeps that as a character limit
  for the conversation (by `Scope`; one shared limit without it), so later
  calls stay under it instead of overflowing again. Limits only go down, and
  the projection metadata reports one in use as `contextkit.overflow_limit`.
  The limit replaces the compressor's own budget for that conversation; with
  `UseAgentBudget` the agent's token budget still applies beside it.

For large tools such as OCR or retrieval, pair `contextkit` with bounded tool
results:

- return a preview in `tools.Result.ForLLM`
- keep full output in `tools.Result.ForUser`, object storage, or a retrieval index
- expose bounded read/search tools for follow-up access

## Verify

```bash
go test ./...
```
