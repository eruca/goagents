package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/llmkit/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/approvalcrypto"
	"github.com/eruca/goagents/runkit/goagentapproval"
)

const (
	localApprovalTenant          = "local"
	hostAgentDefinitionHash      = "host-api-record-review-v1"
	localApprovalKeychainService = "goagents.host-api.approvals"
	localApprovalKeyID           = "local-v1"
	agentApprovalMetadataID      = "agent_approval_checkpoint_id"
	agentApprovalMetadataTools   = "agent_approval_tools_json"
	agentApprovalCompletedID     = "agent_approval_completed_checkpoint_id"
	agentApprovalCompletedTools  = "agent_approval_completed_tools_json"
	recordReviewToolName         = "record_review"
	agentApprovalLifetime        = time.Hour
)

const (
	agentApprovalKeychainServiceEnv = "HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE"
	agentApprovalKeyIDEnv           = "HOST_API_AGENT_APPROVAL_KEY_ID"
)

type agentApprovalKeychainConfig struct {
	service string
	keyID   string
}

func resolveAgentApprovalKeychainConfig(service, keyID string) (agentApprovalKeychainConfig, error) {
	if service == "" && keyID == "" {
		return agentApprovalKeychainConfig{
			service: localApprovalKeychainService,
			keyID:   localApprovalKeyID,
		}, nil
	}
	service = strings.TrimSpace(service)
	keyID = strings.TrimSpace(keyID)
	if service == "" || keyID == "" {
		return agentApprovalKeychainConfig{}, fmt.Errorf("agent approval Keychain service and key ID must be configured together")
	}
	return agentApprovalKeychainConfig{service: service, keyID: keyID}, nil
}

// agentApprovalResponse is the operator-safe projection of a paused agent. It
// intentionally excludes raw tool inputs, prompts, messages, and checkpoint data.
type agentApprovalResponse struct {
	CheckpointID string                     `json:"checkpoint_id"`
	Tools        []agentApprovalPendingTool `json:"tools"`
}

type agentApprovalPendingTool struct {
	Index      int    `json:"index"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
}

type agentApprovalRequest struct {
	Resolutions []agentApprovalResolutionRequest `json:"resolutions"`
}

// agentApprovalResolutionRequest is intentionally narrower than goagent's
// internal resolution type: HTTP callers cannot put free-form text into the
// agent event stream.
type agentApprovalResolutionRequest struct {
	Index      int    `json:"index"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
	Allowed    bool   `json:"allowed"`
}

func (r agentApprovalRequest) coreResolutions() []agentcore.ToolApprovalResolution {
	resolutions := make([]agentcore.ToolApprovalResolution, 0, len(r.Resolutions))
	for _, resolution := range r.Resolutions {
		resolutions = append(resolutions, agentcore.ToolApprovalResolution{
			Index:      resolution.Index,
			ToolCallID: resolution.ToolCallID,
			Tool:       resolution.Tool,
			Allowed:    resolution.Allowed,
		})
	}
	return resolutions
}

// hostAgentApprovalService owns the host-only identities and encryption
// boundary. Runkit receives opaque encrypted bytes and never owns local keys.
type hostAgentApprovalService struct {
	checkpoints     runkit.CheckpointStore
	pendingFailures runkit.PendingCheckpointFailureStore
	runner          goagentapproval.Resumer
	keychain        agentApprovalKeychainConfig

	mu     sync.Mutex
	cipher goagentapproval.Cipher
}

func newHostAgentApprovalService(
	runs runkit.Store,
	cipher goagentapproval.Cipher,
	runner goagentapproval.Resumer,
	keychain agentApprovalKeychainConfig,
) (*hostAgentApprovalService, error) {
	checkpoints, ok := runs.(runkit.CheckpointStore)
	if !ok {
		return nil, fmt.Errorf("host run store does not implement approval checkpoint persistence")
	}
	pendingFailures, ok := runs.(runkit.PendingCheckpointFailureStore)
	if !ok {
		return nil, fmt.Errorf("host run store does not implement pending checkpoint failure")
	}
	return &hostAgentApprovalService{
		checkpoints:     checkpoints,
		pendingFailures: pendingFailures,
		cipher:          cipher,
		runner:          runner,
		keychain:        keychain,
	}, nil
}

// SavePending encrypts a pause before its safe operator-facing metadata is
// attached to a workflow. The artifact tool has not executed at this point.
func (s *hostAgentApprovalService) SavePending(ctx context.Context, workflowID string, checkpoint agentcore.RunCheckpoint) (agentApprovalResponse, error) {
	if strings.TrimSpace(workflowID) == "" || strings.TrimSpace(checkpoint.RunID) == "" || len(checkpoint.PendingCalls) == 0 {
		return agentApprovalResponse{}, fmt.Errorf("workflow id and pending checkpoint are required")
	}
	adapter, err := s.adapter()
	if err != nil {
		return agentApprovalResponse{}, err
	}
	approval := agentApprovalResponse{
		CheckpointID: agentcore.NewRunID().String(),
		Tools:        safePendingTools(checkpoint),
	}
	if err := adapter.SavePending(ctx, goagentapproval.PendingCheckpoint{
		ID:             approval.CheckpointID,
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		ExpiresAt:      time.Now().UTC().Add(agentApprovalLifetime),
		Checkpoint:     checkpoint,
	}); err != nil {
		return agentApprovalResponse{}, err
	}
	return approval, nil
}

func (s *hostAgentApprovalService) adapter() (*goagentapproval.Adapter, error) {
	cipher, err := s.activeCipher()
	if err != nil {
		return nil, err
	}
	return goagentapproval.New(pendingShutdownCheckpointStore{CheckpointStore: s.checkpoints}, cipher, s.runner)
}

func (s *hostAgentApprovalService) ApproveAndResume(ctx context.Context, workflowID string, approval agentApprovalResponse, approverID string, resolutions []agentcore.ToolApprovalResolution, leaseOwner string) (agentApprovalResponse, *agentcore.RunResult, error) {
	adapter, err := s.adapter()
	if err != nil {
		return agentApprovalResponse{}, nil, err
	}
	next := agentApprovalResponse{CheckpointID: agentcore.NewRunID().String()}
	result, err := adapter.ApproveAndResume(ctx, goagentapproval.ResumeRequest{
		Approval: runkit.ApprovalLeaseRequest{
			CheckpointID:   approval.CheckpointID,
			TenantID:       localApprovalTenant,
			DefinitionHash: hostAgentDefinitionHash,
			ApproverID:     approverID,
			AuditRef:       "audit:" + workflowID + ":agent-approval:" + approval.CheckpointID,
			ReasonCode:     "operator_approved",
			LeaseOwner:     leaseOwner,
			LeaseDuration:  agentApprovalLifetime,
			Now:            time.Now().UTC(),
		},
		Resolutions: resolutions,
		Next: goagentapproval.NextCheckpoint{
			ID:        next.CheckpointID,
			ExpiresAt: time.Now().UTC().Add(agentApprovalLifetime),
		},
	})
	if result != nil && result.Interruption != nil {
		next.Tools = safePendingTools(result.Interruption.Checkpoint)
	}
	return next, result, err
}

// Reject records a terminal, fail-closed operator decision without decrypting
// or executing paused agent state.
func (s *hostAgentApprovalService) Reject(ctx context.Context, workflowID string, approval agentApprovalResponse, approverID string) error {
	adapter, err := s.adapter()
	if err != nil {
		return err
	}
	return adapter.Reject(ctx, runkit.ApprovalLeaseRequest{
		CheckpointID:   approval.CheckpointID,
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		ApproverID:     approverID,
		AuditRef:       "audit:" + workflowID + ":agent-approval:" + approval.CheckpointID,
		ReasonCode:     "operator_rejected",
		Now:            time.Now().UTC(),
	})
}

// activeCipher opens Keychain only if a real tool-capable run actually pauses.
// Tests inject a cipher, so they never create or read a machine Keychain item.
func (s *hostAgentApprovalService) activeCipher() (goagentapproval.Cipher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cipher != nil {
		return s.cipher, nil
	}
	keys, err := approvalcrypto.OpenMacOSKeychainKeyProvider(s.keychain.service, s.keychain.keyID)
	if err != nil {
		return nil, err
	}
	cipher, err := approvalcrypto.NewAESGCMCipher(keys)
	if err != nil {
		return nil, err
	}
	s.cipher = cipher
	return s.cipher, nil
}

func (a agentApprovalResponse) workflowMetadata() map[string]any {
	encoded, _ := json.Marshal(a.Tools)
	return map[string]any{
		agentApprovalMetadataID:    a.CheckpointID,
		agentApprovalMetadataTools: string(encoded),
	}
}

func agentApprovalFromMetadata(metadata map[string]any) *agentApprovalResponse {
	return agentApprovalFromMetadataKeys(metadata, agentApprovalMetadataID, agentApprovalMetadataTools)
}

func completedAgentApprovalFromMetadata(metadata map[string]any) *agentApprovalResponse {
	return agentApprovalFromMetadataKeys(metadata, agentApprovalCompletedID, agentApprovalCompletedTools)
}

func agentApprovalFromMetadataKeys(metadata map[string]any, checkpointKey, toolsKey string) *agentApprovalResponse {
	checkpointID, _ := metadata[checkpointKey].(string)
	rawTools, _ := metadata[toolsKey].(string)
	if strings.TrimSpace(checkpointID) == "" || strings.TrimSpace(rawTools) == "" {
		return nil
	}
	var tools []agentApprovalPendingTool
	if err := json.Unmarshal([]byte(rawTools), &tools); err != nil || len(tools) == 0 {
		return nil
	}
	for _, tool := range tools {
		if strings.TrimSpace(tool.ToolCallID) == "" || strings.TrimSpace(tool.Tool) == "" || tool.Index < 0 {
			return nil
		}
	}
	return &agentApprovalResponse{CheckpointID: checkpointID, Tools: tools}
}

func rememberCompletedAgentApprovalMetadata(metadata map[string]any, approval agentApprovalResponse) {
	if metadata == nil || approval.CheckpointID == "" || len(approval.Tools) == 0 {
		return
	}
	encoded, _ := json.Marshal(approval.Tools)
	metadata[agentApprovalCompletedID] = approval.CheckpointID
	metadata[agentApprovalCompletedTools] = string(encoded)
}

func resolutionsMatchCompletedApproval(resolutions []agentcore.ToolApprovalResolution, tools []agentApprovalPendingTool) bool {
	if len(resolutions) != len(tools) || len(tools) == 0 {
		return false
	}
	expected := make(map[int]agentApprovalPendingTool, len(tools))
	for _, tool := range tools {
		if _, exists := expected[tool.Index]; exists {
			return false
		}
		expected[tool.Index] = tool
	}
	for _, resolution := range resolutions {
		tool, ok := expected[resolution.Index]
		if !ok || !resolution.Allowed || resolution.ToolCallID != tool.ToolCallID || resolution.Tool != tool.Tool {
			return false
		}
		delete(expected, resolution.Index)
	}
	return len(expected) == 0
}

func safePendingTools(checkpoint agentcore.RunCheckpoint) []agentApprovalPendingTool {
	tools := make([]agentApprovalPendingTool, 0, len(checkpoint.PendingCalls))
	for index, call := range checkpoint.PendingCalls {
		tools = append(tools, agentApprovalPendingTool{
			Index:      index,
			ToolCallID: call.ID,
			Tool:       call.Name,
		})
	}
	return tools
}

type recordReviewTool struct {
	artifacts artifactkit.Store
}

func (recordReviewTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        recordReviewToolName,
		Description: "Record the approved host review action as a local artifact.",
		Permission:  policy.PermissionWrite,
		Schema: ports.ToolSchema{
			JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		},
	}
}

func (t recordReviewTool) Execute(ctx context.Context, _ json.RawMessage, env tools.Env) (*tools.Result, error) {
	workflowID, _ := env.Metadata["workflow_id"].(string)
	if strings.TrimSpace(workflowID) == "" {
		return nil, fmt.Errorf("workflow_id tool metadata is required")
	}
	ref := "artifact:" + workflowID + ":review-action"
	if err := putTextArtifact(ctx, t.artifacts, ref, "record_review executed after operator approval\n"); err != nil {
		return nil, err
	}
	return &tools.Result{
		ForLLM:  "review action recorded",
		ForUser: "review action recorded",
		Ref:     ref,
	}, nil
}

type pendingReviewApprover struct{}

func (pendingReviewApprover) ApproveTool(context.Context, agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Pending: true}
}

func (r routingAgentRunner) toolOptions(profile llmkit.TaskProfile) []agentcore.Option {
	if !profile.NeedsTools {
		return nil
	}
	registry := tools.NewRegistry()
	registry.Register(recordReviewTool{artifacts: r.artifacts})
	return []agentcore.Option{
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(pendingReviewApprover{}),
	}
}
