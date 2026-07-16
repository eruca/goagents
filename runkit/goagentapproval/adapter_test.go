package goagentapproval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/runkit"
)

func TestAdapterStoresOpaqueCheckpointAndCompletesApprovedResume(t *testing.T) {
	store := runkit.NewMemoryCheckpointStore()
	cipher := &testCipher{}
	resumer := &testResumer{result: &agentcore.RunResult{}}
	adapter, err := New(store, cipher, resumer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}

	stored, err := store.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if !strings.HasPrefix(string(stored.Ciphertext), "encrypted:") {
		t.Fatalf("checkpoint was not encrypted: %q", stored.Ciphertext)
	}

	result, err := adapter.ApproveAndResume(context.Background(), ResumeRequest{
		Approval: approvalLeaseRequest(pending, now),
		Resolutions: []agentcore.ToolApprovalResolution{{
			Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true,
		}},
		Next: NextCheckpoint{ID: "checkpoint-2", ExpiresAt: now.Add(2 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("ApproveAndResume: %v", err)
	}
	if result != resumer.result || resumer.calls != 1 {
		t.Fatalf("result = %#v, calls = %d", result, resumer.calls)
	}
	if resumer.checkpoint.RunID != pending.Checkpoint.RunID || len(resumer.resolutions) != 1 {
		t.Fatalf("resume input = %#v, %#v", resumer.checkpoint, resumer.resolutions)
	}
	stored, err = store.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint after resume: %v", err)
	}
	if stored.Status != runkit.CheckpointConsumed {
		t.Fatalf("status = %q, want consumed", stored.Status)
	}
	if _, err := store.GetCheckpoint(context.Background(), "checkpoint-2", pending.TenantID); !errors.Is(err, runkit.ErrCheckpointNotFound) {
		t.Fatalf("unexpected next checkpoint = %v", err)
	}
}

func TestAdapterBindsCiphertextToCheckpointIdentity(t *testing.T) {
	store := runkit.NewMemoryCheckpointStore()
	cipher := &bindingCipher{}
	adapter, err := New(store, cipher, &testResumer{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}
	want := []byte(`{"checkpoint_id":"checkpoint-1","tenant_id":"tenant-1","definition_hash":"agent-v1"}`)
	if !bytes.Equal(cipher.encryptAAD, want) {
		t.Fatalf("encrypt AAD = %q, want %q", cipher.encryptAAD, want)
	}
}

func TestAdapterPersistsNextCheckpointBeforeConsumingCurrentLease(t *testing.T) {
	baseStore := runkit.NewMemoryCheckpointStore()
	store := &recordingStore{CheckpointStore: baseStore}
	cipher := &testCipher{}
	next := testAgentCheckpoint("run-1", "call-2")
	resumer := &testResumer{
		result: &agentcore.RunResult{Interruption: &agentcore.ToolApprovalInterruption{Checkpoint: next}},
		err:    agentcore.ErrApprovalPending,
	}
	adapter, err := New(store, cipher, resumer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}

	result, err := adapter.ApproveAndResume(context.Background(), ResumeRequest{
		Approval:    approvalLeaseRequest(pending, now),
		Resolutions: []agentcore.ToolApprovalResolution{{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true}},
		Next:        NextCheckpoint{ID: "checkpoint-2", ExpiresAt: now.Add(2 * time.Hour)},
	})
	if !errors.Is(err, agentcore.ErrApprovalPending) || result != resumer.result {
		t.Fatalf("ApproveAndResume = %#v, %v", result, err)
	}
	if got, want := strings.Join(store.operations, ","), "create:checkpoint-1,approve:checkpoint-1,create:checkpoint-2,complete:checkpoint-1"; got != want {
		t.Fatalf("operations = %q, want %q", got, want)
	}
	current, err := baseStore.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil || current.Status != runkit.CheckpointConsumed {
		t.Fatalf("current = %#v, %v", current, err)
	}
	nextStored, err := baseStore.GetCheckpoint(context.Background(), "checkpoint-2", pending.TenantID)
	if err != nil || nextStored.Status != runkit.CheckpointPending {
		t.Fatalf("next = %#v, %v", nextStored, err)
	}
}

func TestAdapterFailsLeaseInsteadOfReplayingWhenNextCheckpointCannotPersist(t *testing.T) {
	baseStore := runkit.NewMemoryCheckpointStore()
	store := &failNextCreateStore{CheckpointStore: baseStore, checkpointID: "checkpoint-2"}
	cipher := &testCipher{}
	next := testAgentCheckpoint("run-1", "call-2")
	resumer := &testResumer{
		result: &agentcore.RunResult{Interruption: &agentcore.ToolApprovalInterruption{Checkpoint: next}},
		err:    agentcore.ErrApprovalPending,
	}
	adapter, err := New(store, cipher, resumer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}

	_, err = adapter.ApproveAndResume(context.Background(), ResumeRequest{
		Approval:    approvalLeaseRequest(pending, now),
		Resolutions: []agentcore.ToolApprovalResolution{{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true}},
		Next:        NextCheckpoint{ID: "checkpoint-2", ExpiresAt: now.Add(2 * time.Hour)},
	})
	if !errors.Is(err, ErrNextCheckpointPersist) {
		t.Fatalf("ApproveAndResume error = %v, want ErrNextCheckpointPersist", err)
	}
	stored, err := baseStore.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil || stored.Status != runkit.CheckpointFailed || stored.FailureCode != "next_checkpoint_persist_failed" {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	_, err = baseStore.ApproveAndLease(context.Background(), approvalLeaseRequest(pending, now.Add(time.Minute)))
	if !errors.Is(err, runkit.ErrCheckpointNotClaimable) {
		t.Fatalf("failed checkpoint was leaseable: %v", err)
	}
	if resumer.calls != 1 {
		t.Fatalf("resumer calls = %d, want 1", resumer.calls)
	}
}

func TestAdapterRejectNeverResumesAgent(t *testing.T) {
	store := runkit.NewMemoryCheckpointStore()
	resumer := &testResumer{}
	adapter, err := New(store, &testCipher{}, resumer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}
	if err := adapter.Reject(context.Background(), approvalLeaseRequest(pending, now)); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	stored, err := store.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil || stored.Status != runkit.CheckpointRejected || resumer.calls != 0 {
		t.Fatalf("stored = %#v, err = %v, calls = %d", stored, err, resumer.calls)
	}
}

func TestAdapterFailsLeaseWhenCheckpointCannotDecrypt(t *testing.T) {
	store := runkit.NewMemoryCheckpointStore()
	resumer := &testResumer{}
	adapter, err := New(store, &testCipher{decryptErr: errors.New("key unavailable")}, resumer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()
	pending := testPendingCheckpoint("checkpoint-1", now.Add(time.Hour))
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}

	_, err = adapter.ApproveAndResume(context.Background(), ResumeRequest{
		Approval:    approvalLeaseRequest(pending, now),
		Resolutions: []agentcore.ToolApprovalResolution{{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true}},
		Next:        NextCheckpoint{ID: "checkpoint-2", ExpiresAt: now.Add(2 * time.Hour)},
	})
	if !errors.Is(err, ErrCheckpointDecrypt) {
		t.Fatalf("ApproveAndResume error = %v, want ErrCheckpointDecrypt", err)
	}
	stored, err := store.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil || stored.Status != runkit.CheckpointFailed || stored.FailureCode != "checkpoint_decode_failed" || resumer.calls != 0 {
		t.Fatalf("stored = %#v, err = %v, calls = %d", stored, err, resumer.calls)
	}
}

func TestAdapterResumesRealAgentExactlyOnce(t *testing.T) {
	toolRuns := 0
	llm := &scriptedLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"draft":"ready"}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(countingWriteTool{runs: &toolRuns})
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(alwaysPendingApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), agentcore.RunRequest{
		Input:              "update draft",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, agentcore.ErrApprovalPending) || paused == nil || paused.Interruption == nil {
		t.Fatalf("RunDetailed = %#v, %v", paused, err)
	}
	if toolRuns != 0 {
		t.Fatalf("tool ran before approval: %d", toolRuns)
	}

	store := runkit.NewMemoryCheckpointStore()
	adapter, err := New(store, &testCipher{}, agent)
	if err != nil {
		t.Fatalf("New adapter: %v", err)
	}
	now := time.Now().UTC()
	pending := PendingCheckpoint{
		ID:             "checkpoint-real-agent",
		TenantID:       "tenant-1",
		DefinitionHash: "agent-v1",
		ExpiresAt:      now.Add(time.Hour),
		Checkpoint:     paused.Interruption.Checkpoint,
	}
	if err := adapter.SavePending(context.Background(), pending); err != nil {
		t.Fatalf("SavePending: %v", err)
	}
	result, err := adapter.ApproveAndResume(context.Background(), ResumeRequest{
		Approval:    approvalLeaseRequest(pending, now),
		Resolutions: []agentcore.ToolApprovalResolution{{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true}},
		Next:        NextCheckpoint{ID: "checkpoint-real-agent-next", ExpiresAt: now.Add(2 * time.Hour)},
	})
	if err != nil || result == nil || result.Content != "done" {
		t.Fatalf("ApproveAndResume = %#v, %v", result, err)
	}
	if toolRuns != 1 || llm.calls != 2 {
		t.Fatalf("tool runs = %d, LLM calls = %d", toolRuns, llm.calls)
	}
	stored, err := store.GetCheckpoint(context.Background(), pending.ID, pending.TenantID)
	if err != nil || stored.Status != runkit.CheckpointConsumed {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
}

type testCipher struct{ decryptErr error }

func (testCipher) Encrypt(_ context.Context, plaintext, _ []byte) ([]byte, error) {
	return append([]byte("encrypted:"), plaintext...), nil
}

func (c testCipher) Decrypt(_ context.Context, ciphertext, _ []byte) ([]byte, error) {
	if c.decryptErr != nil {
		return nil, c.decryptErr
	}
	return append([]byte(nil), ciphertext[len("encrypted:"):]...), nil
}

type bindingCipher struct{ encryptAAD []byte }

func (c *bindingCipher) Encrypt(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	c.encryptAAD = append([]byte(nil), aad...)
	return append([]byte("encrypted:"), plaintext...), nil
}

func (bindingCipher) Decrypt(_ context.Context, ciphertext, _ []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext[len("encrypted:"):]...), nil
}

type testResumer struct {
	result      *agentcore.RunResult
	err         error
	calls       int
	checkpoint  agentcore.RunCheckpoint
	resolutions []agentcore.ToolApprovalResolution
}

type scriptedLLM struct {
	responses []*ports.ChatResponse
	calls     int
}

func (l *scriptedLLM) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	if l.calls >= len(l.responses) {
		return nil, errors.New("unexpected LLM call")
	}
	response := l.responses[l.calls]
	l.calls++
	return response, nil
}

type countingWriteTool struct{ runs *int }

func (t countingWriteTool) Spec() tools.Spec {
	return tools.Spec{Name: "write", Permission: policy.PermissionWrite}
}

func (t countingWriteTool) Execute(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
	*t.runs++
	return &tools.Result{ForLLM: "written"}, nil
}

type alwaysPendingApprover struct{}

func (alwaysPendingApprover) ApproveTool(context.Context, agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Pending: true}
}

func (r *testResumer) ResumeDetailed(_ context.Context, checkpoint agentcore.RunCheckpoint, resolutions []agentcore.ToolApprovalResolution) (*agentcore.RunResult, error) {
	r.calls++
	r.checkpoint = checkpoint
	r.resolutions = append([]agentcore.ToolApprovalResolution(nil), resolutions...)
	return r.result, r.err
}

type recordingStore struct {
	runkit.CheckpointStore
	operations []string
}

type failNextCreateStore struct {
	runkit.CheckpointStore
	checkpointID string
}

func (s *failNextCreateStore) CreateCheckpoint(ctx context.Context, checkpoint runkit.ApprovalCheckpoint) error {
	if checkpoint.ID == s.checkpointID {
		return errors.New("checkpoint store unavailable")
	}
	return s.CheckpointStore.CreateCheckpoint(ctx, checkpoint)
}

func (s *recordingStore) CreateCheckpoint(ctx context.Context, checkpoint runkit.ApprovalCheckpoint) error {
	s.operations = append(s.operations, "create:"+checkpoint.ID)
	return s.CheckpointStore.CreateCheckpoint(ctx, checkpoint)
}

func (s *recordingStore) ApproveAndLease(ctx context.Context, request runkit.ApprovalLeaseRequest) (runkit.ApprovalCheckpoint, error) {
	s.operations = append(s.operations, "approve:"+request.CheckpointID)
	return s.CheckpointStore.ApproveAndLease(ctx, request)
}

func (s *recordingStore) CompleteLease(ctx context.Context, completion runkit.CheckpointLeaseCompletion) error {
	s.operations = append(s.operations, "complete:"+completion.CheckpointID)
	return s.CheckpointStore.CompleteLease(ctx, completion)
}

func (s *recordingStore) FailLease(ctx context.Context, completion runkit.CheckpointLeaseCompletion) error {
	s.operations = append(s.operations, "fail:"+completion.CheckpointID)
	return s.CheckpointStore.FailLease(ctx, completion)
}

func testPendingCheckpoint(id string, expiresAt time.Time) PendingCheckpoint {
	return PendingCheckpoint{
		ID:             id,
		TenantID:       "tenant-1",
		DefinitionHash: "agent-v1",
		ExpiresAt:      expiresAt,
		Checkpoint:     testAgentCheckpoint("run-1", "call-1"),
	}
}

func testAgentCheckpoint(runID, callID string) agentcore.RunCheckpoint {
	return agentcore.RunCheckpoint{
		Version: 1,
		RunID:   runID,
		PendingCalls: []ports.ToolCall{
			{ID: callID, Name: "write"},
		},
	}
}

func approvalLeaseRequest(pending PendingCheckpoint, now time.Time) runkit.ApprovalLeaseRequest {
	return runkit.ApprovalLeaseRequest{
		CheckpointID:   pending.ID,
		TenantID:       pending.TenantID,
		DefinitionHash: pending.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	}
}
