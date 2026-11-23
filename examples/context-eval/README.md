# context-eval

Compares context compression strategies on tasks whose answer depends on
facts an agent read early in a long run, against any OpenAI-compatible model.

Each task gives the agent `read_page` over a generated document of invoice
pages, one invoice per page amid filler text. The agent must read every page,
one per call, before answering, so with a small budget the early pages are
compressed away by the time it answers. Three task kinds:

- `lookup`: the amount of page 2's invoice;
- `sum`: the sum of every invoice;
- `max`: the vendor with the largest invoice.

Strategies:

| Strategy | Context handling |
|---|---|
| `none` | no compression: the upper bound and the token cost to beat |
| `drop` | standard compression; the summary catalogs dropped steps without references |
| `summary` | deep compression with `agentadapter.LLMSummarizer` on the same model |
| `pull` | removed content archived; the model gets `context_read` and `context_search` |
| `push` | `pull`, plus the two best archive matches for the request appended to every model call |
| `notes` | `pull`, plus `context_note`, whose notes stay pinned at the top of the context |

```bash
go run ./examples/context-eval \
  --base-url http://127.0.0.1:8084/v1 --model qwen3.8-27b \
  --seeds 2 --pages 8 --page-chars 2500 --budget 8000 --out report.json
```

The report gives, per strategy, correct answers overall and per kind, input
and output tokens, model calls, tool calls, context tool calls, summarizer
calls, context overflows, failed runs, and time.

To test reactive compression, make the document larger than the model's
context window and the budget larger still, so compression only happens when
the provider rejects a call as too long (`ports.ErrContextTooLong`):

```bash
go run ./examples/context-eval --pages 12 --page-chars 60000 --budget 100000000 \
  --strategies none,pull,summary --run-timeout 60m
```

`overflows` counts calls the provider rejected and the agent retried after
compressing again, and runs that still ended on a rejection (`none` has no
projector, so every run that outgrows the window ends there). Results for qwen3.8-27b are in
[`docs/evals/contextkit-strategies-qwen3.8-27b.md`](../../docs/evals/contextkit-strategies-qwen3.8-27b.md).

The test runs every strategy with a deterministic stand-in model that answers
only from what its request shows.
