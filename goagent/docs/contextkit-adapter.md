# ContextKit Adapter

`goagent` does not import `contextkit`. Use the ready adapter
`contextkit/agentadapter`, which implements `agentcore.ContextProjector` on top
of a `contextkit` compressor:

```go
projector, err := agentadapter.NewProjector(agentadapter.Config{
	Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 48000}}),
})
agent, err := agentcore.NewAgent(agentcore.WithContextProjector(projector), ...)
```

It maps messages with their tool calls and replayed reasoning, counts the
compiled system prompt against the budget, and remembers earlier compressions
so a run's summaries are built incrementally. See
[`contextkit/README.md`](../../contextkit/README.md#use-with-goagent).

Do not hand-write a mapping that copies only `Role` and `Content`: an
assistant message's `ToolCalls` would be lost, and the provider would reject
the tool replies that answer them.
