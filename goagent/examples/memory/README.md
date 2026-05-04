# Memory Example

This example uses one shared `memory.WindowMemory` and two Agent runs with the same `SessionID`.

The first run saves its final message history. The second run loads that session history before adding the new user input, so the LLM request receives remembered context.

`WindowMemory` is an in-process bounded memory provider. It is useful for embedding and tests, but it does not survive process restart.

Run it with:

```bash
go run ./examples/memory
```
