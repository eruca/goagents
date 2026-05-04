# SSE Stream Adapter Example Design

> Date: 2026-04-26
> Goal: Add a host-owned Server-Sent Events example that wraps
> `Agent.Stream` without adding transport responsibilities to `agentcore`.

## Context

`Agent.Stream` is already the in-process runtime stream contract. It emits
bounded runtime events and one terminal event carrying the final result or a
partial abort result. The next boundary proof is to show that a host service can
adapt that stream to HTTP/SSE without changing the runtime.

This example should validate the extension boundary from
`2026-04-26-runtime-extension-boundary-review.md`: transport belongs outside
core, while `Agent.Stream` remains the reusable primitive.

## Design Principles

1. Keep all HTTP and SSE code in `examples/stream-sse`.
2. Do not add transport interfaces, HTTP helpers, or SSE types to `agentcore`.
3. Preserve bounded event payloads; never emit raw prompts, raw tool inputs, or
   full tool output.
4. Make the example testable with `httptest` without starting a long-running
   server.
5. Keep `make smoke` fast by letting the example run a one-shot self-check when
   requested instead of blocking forever.

## Example Shape

Create `examples/stream-sse` as a package `main` with three responsibilities:

- build a deterministic demo agent;
- expose a host-owned HTTP handler for `/runs/stream`;
- start a real server only when run normally.

The handler should:

- accept `GET /runs/stream`;
- set `Content-Type: text/event-stream`;
- start `agent.Stream` with a deterministic `RunRequest`;
- encode selected runtime events as SSE frames;
- encode one terminal `done` frame with final content and summary;
- return HTTP 405 for non-GET requests.

The example should include a write tool and an approver so the stream shows the
transport-relevant lifecycle:

```text
approval.requested -> approval.completed -> tool.completed -> done
```

The tool result should include a small `Ref` and bounded model observation. The
SSE payload should include only event type, stage, tool, ref, reason, final
content, and execution summary.

## SSE Event Contract

Each frame should use a named event:

```text
event: runtime
data: {"type":"approval.requested","stage":"Approval","tool":"update_draft","reason":""}

event: runtime
data: {"type":"approval.completed","stage":"Approval","tool":"update_draft","reason":"operator approved"}

event: runtime
data: {"type":"tool.completed","stage":"Act","tool":"update_draft","ref":"draft:demo"}

event: done
data: {"content":"Final answer: draft updated after approval.","summary":{"llm_calls":2,"tool_calls":1,"used_tools":["update_draft"],"abort_reason":""}}
```

The exact order may include other runtime events between these highlighted
frames, but tests should assert that these frames exist and that the terminal
frame is present.

## Files

- Create `examples/stream-sse/main.go`
  - demo LLM, tool, approver, handler, SSE encoding, `main`.
- Create `examples/stream-sse/main_test.go`
  - `httptest` coverage for SSE headers, highlighted events, terminal payload,
    and method rejection.
- Create `examples/stream-sse/README.md`
  - explains that this is a host adapter around `Agent.Stream`, not a core
    transport feature.
- Modify `README.md`
  - add `examples/stream-sse` to the learning path.
- Modify `Makefile`
  - include `go run ./examples/stream-sse --once` in `smoke`.

## Testing Strategy

Use TDD around the handler:

1. Add `main_test.go` that calls `newSSEHandler()` through `httptest`.
2. Verify the test fails because the handler does not exist.
3. Implement the minimal example.
4. Run `go test ./examples/stream-sse -count=1`.
5. Add README and smoke wiring.
6. Run `go run ./examples/stream-sse --once`.
7. Run `make verify`.

## Non-Goals

- No new `agentcore` API.
- No reusable transport package.
- No durable event store.
- No browser UI.
- No async human approval.
- No pause/resume.
- No plan approval.
- No MCP adapter.
