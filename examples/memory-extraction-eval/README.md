# memory-extraction-eval

Runs memorykit's `agentadapter.JSONCandidateExtractor` on finished agent runs,
rendered as the host-api renders them (`agentadapter.RunSource` within the
memory content limit), against any OpenAI-compatible model, and checks what
it proposes.

Each case is a short run: a request, tool calls with their results, and the
answer. The cases check that extraction

- keeps a fact only a tool result states (`tool-fact`, `chinese`), a decision
  (`decision`), a constraint (`constraint`, `buried` in long tool output),
  and a lesson (`lesson`);
- proposes nothing for a run with nothing durable (`nothing-durable`) and
  skips one-off values next to a durable fact (`transient-and-durable`);
- does not keep what a tool result tries to plant (`injection`);
- keeps what a tool found, not what the model guessed (`guess-vs-fact`).

```bash
go run ./examples/memory-extraction-eval \
  --base-url http://127.0.0.1:8084/v1 --model qwen3.8-27b --trials 3 --out report.json
```

An extraction passes when its output is valid, every expected fact appears in
some candidate's content, no forbidden text appears, and a run with nothing
durable yields no candidates. A wrong kind is reported but does not fail it.
The test checks every case's source still holds its expected facts and the
grading with a scripted model.
