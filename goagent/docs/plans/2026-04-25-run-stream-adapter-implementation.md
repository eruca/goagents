# Run Stream Adapter Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add an in-process Agent run stream adapter that exposes ordered runtime events plus one terminal result/error event.

**Architecture:** Implement `Agent.Stream` as a thin wrapper around the existing `RunDetailed` path. The stream adapter installs a temporary fan-out `EventSink` that forwards runtime events into a channel and then emits one terminal stream event with the final `RunResult` and error. It must preserve existing `Run` and `RunDetailed` behavior.

**Tech Stack:** Go, existing `agentcore` event/run contracts, mock LLM tests, `go test`, `make verify`.

---

### Task 1: Add Stream API And Success-Path Contract

**Files:**
- Create: `agentcore/stream.go`
- Test: `agentcore/stream_test.go`
- Modify: `agentcore/agent.go`

**Step 1: Write the failing test**

Create `agentcore/stream_test.go` with a test that runs a simple final-answer agent through `Agent.Stream`:

```go
func TestAgentStreamEmitsEventsAndTerminalResult(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "streamed answer"}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})

	var events []RunStreamEvent
	for event := range stream.Events {
		events = append(events, event)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if result == nil || result.Content != "streamed answer" {
		t.Fatalf("result = %#v", result)
	}
	if len(events) == 0 {
		t.Fatal("no stream events received")
	}
	terminal := events[len(events)-1]
	if !terminal.Done || terminal.Result == nil || terminal.Result.Content != "streamed answer" || terminal.Error != nil {
		t.Fatalf("terminal event = %#v", terminal)
	}
	foundThink := false
	for _, event := range events {
		if event.Event.Type == EventStageStarted && event.Event.Stage == "think" {
			foundThink = true
		}
	}
	if !foundThink {
		t.Fatalf("stream events did not include think stage: %#v", events)
	}
}
```

Expected API shape:

```go
type RunStream struct {
	Events <-chan RunStreamEvent
	done   <-chan runStreamDone
}

type RunStreamEvent struct {
	Event  Event
	Result *RunResult
	Error  error
	Done   bool
}

func (a *Agent) Stream(ctx context.Context, req RunRequest) *RunStream
func (s *RunStream) Wait() (*RunResult, error)
```

**Step 2: Run test to verify it fails**

Run:

```bash
go test ./agentcore -run TestAgentStreamEmitsEventsAndTerminalResult -count=1 -v
```

Expected: FAIL because `Agent.Stream`, `RunStream`, and `RunStreamEvent` do not exist.

**Step 3: Implement minimal stream API**

Add `agentcore/stream.go`:

```go
package agentcore

import "context"

const runStreamBuffer = 64

type RunStream struct {
	Events <-chan RunStreamEvent
	done   <-chan runStreamDone
}

type RunStreamEvent struct {
	Event  Event
	Result *RunResult
	Error  error
	Done   bool
}

type runStreamDone struct {
	result *RunResult
	err    error
}

func (a *Agent) Stream(ctx context.Context, req RunRequest) *RunStream {
	events := make(chan RunStreamEvent, runStreamBuffer)
	done := make(chan runStreamDone, 1)
	stream := &RunStream{Events: events, done: done}
	sink := streamEventSink{
		events: events,
		next:   a.eventSink,
	}

	go func() {
		defer close(events)
		result, err := a.runWithEventSink(ctx, req, true, sink)
		terminal := RunStreamEvent{Result: result, Error: err, Done: true}
		events <- terminal
		done <- runStreamDone{result: result, err: err}
	}()

	return stream
}

func (s *RunStream) Wait() (*RunResult, error) {
	done := <-s.done
	return done.result, done.err
}

type streamEventSink struct {
	events chan<- RunStreamEvent
	next   EventSink
}

func (s streamEventSink) Emit(ctx context.Context, event Event) error {
	s.events <- RunStreamEvent{Event: event}
	if s.next != nil {
		_ = s.next.Emit(ctx, event)
	}
	return nil
}
```

Modify `agentcore/agent.go` so `Run`, `RunDetailed`, and `Stream` share the same run path:

```go
func (a *Agent) run(ctx context.Context, req RunRequest, detailed bool) (*RunResult, error) {
	return a.runWithEventSink(ctx, req, detailed, a.eventSink)
}

func (a *Agent) runWithEventSink(ctx context.Context, req RunRequest, detailed bool, sink EventSink) (*RunResult, error) {
	// existing body of run, with state.EventSink = sink
}
```

**Step 4: Run test to verify it passes**

Run:

```bash
go test ./agentcore -run TestAgentStreamEmitsEventsAndTerminalResult -count=1 -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/stream.go agentcore/stream_test.go agentcore/agent.go
git commit -m "feat: 增加运行流适配器"
```

### Task 2: Cover Abort Semantics And Existing Sink Fan-Out

**Files:**
- Modify: `agentcore/stream_test.go`
- Modify: `agentcore/stream.go` if needed

**Step 1: Write failing tests**

Add `TestAgentStreamReturnsPartialResultOnPolicyDeny`:

```go
func TestAgentStreamReturnsPartialResultOnPolicyDeny(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "wrote file"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{
			ID:    "call_write_file",
			Name:  "write_file",
			Input: json.RawMessage(`{"path":"notes.md"}`),
		}},
		Usage: ports.Usage{InputTokens: 8, OutputTokens: 3},
	}}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "write notes"})
	var terminal RunStreamEvent
	for event := range stream.Events {
		if event.Done {
			terminal = event
		}
	}
	result, err := stream.Wait()
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if toolRan {
		t.Fatal("write tool ran after policy denial")
	}
	if result == nil || terminal.Result == nil {
		t.Fatalf("result=%#v terminal=%#v", result, terminal)
	}
	if result.ExecutionSummary.LLMCalls != 1 || result.ExecutionSummary.ToolCalls != 0 {
		t.Fatalf("summary = %#v", result.ExecutionSummary)
	}
	if terminal.Error == nil || !errors.Is(terminal.Error, ErrPolicyDenied) {
		t.Fatalf("terminal error = %v", terminal.Error)
	}
}
```

Add `TestAgentStreamFansOutToConfiguredEventSink`:

```go
func TestAgentStreamFansOutToConfiguredEventSink(t *testing.T) {
	recorder := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "done"}}}
	agent, err := NewAgent(WithLLM(llm), WithEventSink(recorder))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})
	for range stream.Events {
	}
	if _, err := stream.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if len(recorder.events) == 0 {
		t.Fatal("configured sink received no events")
	}
}
```

If no reusable recording sink exists, define one in `stream_test.go`:

```go
type recordingEventSink struct {
	events []Event
}

func (s *recordingEventSink) Emit(ctx context.Context, event Event) error {
	s.events = append(s.events, event)
	return nil
}
```

**Step 2: Run tests to verify failure or current gap**

Run:

```bash
go test ./agentcore -run 'TestAgentStream(ReturnsPartialResultOnPolicyDeny|FansOutToConfiguredEventSink)' -count=1 -v
```

Expected: PASS if Task 1 implementation already covers both; otherwise FAIL for the missing behavior.

**Step 3: Implement minimal fix if needed**

If the tests fail:

- ensure `Agent.Stream` uses `runWithEventSink(..., true, streamEventSink{next: a.eventSink})`;
- ensure the terminal event is emitted before `events` closes;
- ensure `streamEventSink.Emit` forwards to `next` and ignores `next` errors.

**Step 4: Run focused tests**

Run:

```bash
go test ./agentcore -run 'TestAgentStream' -count=1 -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/stream.go agentcore/stream_test.go
git commit -m "test: 覆盖运行流失败与事件转发"
```

### Task 3: Document Stream Usage

**Files:**
- Modify: `README.md`
- Modify: `docs/plans/2026-04-25-run-stream-adapter-design.md` if final API differs

**Step 1: Update README**

Add a `Run Streams` section after `Observable Runs`:

```markdown
## Run Streams

Use `Agent.Stream` when a host needs in-process lifecycle updates while a run is
executing. The stream emits bounded runtime events and then one terminal event
with the final `RunResult` or abort error.

```go
stream := agent.Stream(ctx, agentcore.RunRequest{Input: "look up the account"})
for event := range stream.Events {
	if event.Done {
		break
	}
	log.Printf("%s %s", event.Event.Type, event.Event.Stage)
}
result, err := stream.Wait()
```

`Stream` uses `RunDetailed` semantics for terminal results. On abort, `Wait`
returns the partial result and error. The stream is in-process and
transport-neutral; HTTP, SSE, WebSocket, and durable audit storage belong in the
host application.
```

Mention that existing `WithEventSink` still receives events.

**Step 2: Run docs-neutral tests**

Run:

```bash
go test ./agentcore -count=1
```

Expected: PASS.

**Step 3: Commit**

```bash
git add README.md docs/plans/2026-04-25-run-stream-adapter-design.md
git commit -m "docs: 说明运行流用法"
```

### Task 4: Full Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
```

Expected: PASS.

**Step 2: Inspect status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree, branch ahead of `origin/main`.

**Step 3: Report**

Summarize:

- stream API added;
- success and policy-denial paths tested;
- existing event sink fan-out preserved;
- README updated;
- `make verify` result.
