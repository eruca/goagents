# Event Sink Minimal Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add lightweight Agent runtime events so host applications can observe runs without modifying stages.

**Architecture:** Event types live in `agentcore` because they use `RunID` and describe Agent runtime lifecycle. `Pipeline` emits stage lifecycle events through `RunState.EventSink`. `ReActRunner` and high-value stages emit memory/tool/finalized events. Sink errors are ignored to keep observability side effects from breaking Agent runs.

**Tech Stack:** Go standard library, existing `agentcore` tests, `make verify`.

---

### Task 1: Add Event Types And Agent Wiring

**Files:**
- Create: `agentcore/events.go`
- Modify: `agentcore/run_state.go`
- Modify: `agentcore/agent.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing test**

Add `TestAgentEmitsFinalizedEvent` using a recording sink and asserting a `finalized` event is emitted.

**Step 2: Run test to verify failure**

Run:

```bash
go test ./agentcore -run TestAgentEmitsFinalizedEvent -v
```

Expected: FAIL because event types and `WithEventSink` do not exist.

**Step 3: Implement minimal event API**

Add `EventType`, `Event`, `EventSink`, `WithEventSink`, store the sink on `Agent`, pass it to `RunState`, and emit `finalized` from `ReActRunner` after successful finalization.

**Step 4: Run test**

Run:

```bash
go test ./agentcore -run TestAgentEmitsFinalizedEvent -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/events.go agentcore/run_state.go agentcore/agent.go agentcore/agent_test.go
git commit -m "feat: add event sink wiring"
```

### Task 2: Emit Stage Lifecycle Events

**Files:**
- Modify: `agentcore/pipeline.go`
- Test: `agentcore/pipeline_test.go`

**Step 1: Write failing test**

Add a test asserting each stage emits `stage.started` and `stage.completed`, and failing stages emit `stage.failed`.

**Step 2: Run test**

Run:

```bash
go test ./agentcore -run TestPipelineEmitsStageEvents -v
```

Expected: FAIL because pipeline does not emit events.

**Step 3: Implement lifecycle events**

Have `Pipeline.Run` emit stage started/completed/failed using `state.Emit`.

**Step 4: Run test**

Run:

```bash
go test ./agentcore -run TestPipelineEmitsStageEvents -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/pipeline.go agentcore/pipeline_test.go
git commit -m "feat: emit stage lifecycle events"
```

### Task 3: Emit Tool And Memory Events

**Files:**
- Modify: `agentcore/act_stage.go`
- Modify: `agentcore/memory_stage.go`
- Modify: `agentcore/finalize_stage.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing tests**

Add tests asserting:

- memory load emits `memory.loaded`
- memory save emits `memory.saved`
- tool execution emits tool started/completed/failed

**Step 2: Run tests**

Run:

```bash
go test ./agentcore -run 'TestAgentEmits(Memory|Tool)' -v
```

Expected: FAIL for missing events.

**Step 3: Implement events**

Emit small metadata only. Do not include raw prompts, raw tool inputs, or full user content.

**Step 4: Verify**

Run:

```bash
go test ./agentcore -run 'TestAgentEmits' -v
make verify
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/act_stage.go agentcore/memory_stage.go agentcore/finalize_stage.go agentcore/agent_test.go
git commit -m "feat: emit runtime events"
```

### Task 4: Document EventSink

**Files:**
- Modify: `README.md`

**Step 1: Update docs**

Document `WithEventSink`, event privacy rules, and the fact that sink errors do not fail runs.

**Step 2: Verify**

Run:

```bash
make verify
```

Expected: PASS.

**Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document event sink"
```
