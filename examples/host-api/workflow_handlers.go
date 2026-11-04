package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/skillkit"
	"github.com/eruca/goagents/workflowkit"
)

type createWorkflowRequest struct {
	ID                string              `json:"id"`
	Input             string              `json:"input"`
	RunMode           string              `json:"run_mode,omitempty"`
	ProjectID         string              `json:"project_id,omitempty"`
	MemoryWriteIntent bool                `json:"memory_write_intent,omitempty"`
	TaskProfilePreset string              `json:"task_profile_preset,omitempty"`
	TaskProfile       *taskProfileRequest `json:"task_profile,omitempty"`
	SkillRefs         []workflowSkillRef  `json:"skill_refs,omitempty"`
}

// workflowSkillRef is the JSON-safe identity of a skill selected for one
// workflow. Digest is optional in a request, but always present after resolve.
type workflowSkillRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest,omitempty"`
}

type approveWorkflowRequest struct {
	Note string `json:"note"`
}

type workflowResponse struct {
	ID            string                 `json:"id"`
	Status        string                 `json:"status"`
	RunMode       string                 `json:"run_mode"`
	SkillRefs     []workflowSkillRef     `json:"skill_refs,omitempty"`
	InputRef      string                 `json:"input_ref,omitempty"`
	OutputRef     string                 `json:"output_ref,omitempty"`
	AgentRunID    string                 `json:"agent_run_id,omitempty"`
	AuditRef      string                 `json:"audit_ref,omitempty"`
	ApprovalRef   string                 `json:"approval_ref,omitempty"`
	WaitingReason string                 `json:"waiting_reason,omitempty"`
	AgentApproval *agentApprovalResponse `json:"agent_approval,omitempty"`
	Completed     []string               `json:"completed_steps,omitempty"`
}

type workflowListResponse struct {
	Workflows []workflowResponse `json:"workflows"`
}

// skillListResponse deliberately exposes only catalog-safe fields. In
// particular, it omits the manifest, instructions, resources, and root paths.
type skillListResponse struct {
	Skills []skillResponse `json:"skills"`
}

type skillResponse struct {
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Digest       string                `json:"digest"`
	Scope        skillkit.Scope        `json:"scope"`
	Availability skillkit.Availability `json:"availability"`
	Reasons      []skillReasonResponse `json:"reasons"`
}

type skillReasonResponse struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
}

type workflowEventsResponse struct {
	WorkflowID  string                  `json:"workflow_id"`
	Status      string                  `json:"status"`
	RunMode     string                  `json:"run_mode"`
	CurrentStep string                  `json:"current_step,omitempty"`
	Completed   []string                `json:"completed_steps,omitempty"`
	Events      []workflowEventResponse `json:"events"`
}

type workflowEventResponse struct {
	Type          string    `json:"type"`
	Name          string    `json:"name,omitempty"`
	Status        string    `json:"status,omitempty"`
	Attempt       int       `json:"attempt,omitempty"`
	OutputRef     string    `json:"output_ref,omitempty"`
	AgentRunID    string    `json:"agent_run_id,omitempty"`
	AuditRef      string    `json:"audit_ref,omitempty"`
	Error         string    `json:"error,omitempty"`
	ApprovalRef   string    `json:"approval_ref,omitempty"`
	WaitingReason string    `json:"waiting_reason,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	EndedAt       time.Time `json:"ended_at,omitempty"`
	FromStatus    string    `json:"from_status,omitempty"`
	ToStatus      string    `json:"to_status,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

type agentRunResponse struct {
	RunID      string                  `json:"run_id"`
	WorkflowID string                  `json:"workflow_id,omitempty"`
	TaskID     string                  `json:"task_id,omitempty"`
	Status     string                  `json:"status"`
	Summary    agentRunSummaryResponse `json:"summary"`
	Events     []agentRunEventResponse `json:"events,omitempty"`
}

// agentRunSummaryResponse and agentRunEventResponse give the runkit records
// the API's snake_case field names; the runkit types have no JSON tags.
type agentRunSummaryResponse struct {
	Status       runkit.Status `json:"status,omitempty"`
	ContentRef   string        `json:"content_ref,omitempty"`
	AbortReason  string        `json:"abort_reason,omitempty"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	LLMCalls     int           `json:"llm_calls"`
	ToolCalls    int           `json:"tool_calls"`
	UsedTools    []string      `json:"used_tools,omitempty"`
}

type agentRunEventResponse struct {
	Sequence   int            `json:"sequence"`
	Type       string         `json:"type"`
	Stage      string         `json:"stage,omitempty"`
	Iteration  int            `json:"iteration"`
	Message    string         `json:"message,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	RecordedAt time.Time      `json:"recorded_at"`
}

func agentRunToResponse(record runkit.RunRecord, events []runkit.RunEvent) agentRunResponse {
	summary := record.Summary
	response := agentRunResponse{
		RunID:      record.RunID,
		WorkflowID: record.WorkflowID,
		TaskID:     record.TaskID,
		Status:     string(record.Status),
		Summary: agentRunSummaryResponse{
			Status: summary.Status, ContentRef: summary.ContentRef, AbortReason: summary.AbortReason,
			InputTokens: summary.InputTokens, OutputTokens: summary.OutputTokens,
			LLMCalls: summary.LLMCalls, ToolCalls: summary.ToolCalls, UsedTools: summary.UsedTools,
		},
		Events: make([]agentRunEventResponse, 0, len(events)),
	}
	for _, event := range events {
		response.Events = append(response.Events, agentRunEventResponse{
			Sequence: event.Sequence, Type: event.Type, Stage: event.Stage, Iteration: event.Iteration,
			Message: event.Message, Metadata: event.Metadata, RecordedAt: event.RecordedAt,
		})
	}
	return response
}

func (s *Server) handleListSkills(w http.ResponseWriter, _ *http.Request) {
	entries := s.skillCatalog.List()
	skills := make([]skillResponse, 0, len(entries))
	for _, entry := range entries {
		report := skillkit.Evaluate(entry, s.skillGateContext)
		reasons := make([]skillReasonResponse, 0, len(report.Reasons))
		for _, reason := range report.Reasons {
			reasons = append(reasons, safeSkillReason(reason))
		}
		skills = append(skills, skillResponse{
			Name:         entry.Ref.Name,
			Description:  entry.Manifest.Description,
			Digest:       entry.Ref.Digest,
			Scope:        entry.Scope,
			Availability: report.State,
			Reasons:      reasons,
		})
	}
	writeJSON(w, http.StatusOK, skillListResponse{Skills: skills})
}

// safeSkillReason keeps Root.ID out of the public API because host
// configuration may use an absolute path as its identifier.
func safeSkillReason(reason skillkit.Reason) skillReasonResponse {
	if reason.Code == "untrusted_root" {
		return skillReasonResponse{Code: reason.Code, Subject: "configured_root"}
	}
	return skillReasonResponse{Code: reason.Code, Subject: reason.Subject}
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req createWorkflowRequest
	var decoded bool
	if s.memory != nil {
		decoded = decodeJSONStrict(w, r, &req)
	} else {
		decoded = decodeJSON(w, r, &req)
	}
	if !decoded {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "id is required")
		return
	}
	runMode, ok := parseRunMode(req.RunMode)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported_run_mode", "supported run_mode values are sync and queued")
		return
	}
	profile, err := parseTaskProfile(req.TaskProfilePreset, req.TaskProfile, s.models)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_profile", err.Error())
		return
	}
	skillRefs, err := s.resolveSkillRefs(req.SkillRefs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_skill_refs", err.Error())
		return
	}
	var trustedMemoryIdentity memoryIdentity
	if s.memory != nil {
		_, identity, ok := s.authorizeWorkflowMemory(w, r, req.ProjectID, memoryRead)
		if !ok {
			return
		}
		if req.MemoryWriteIntent {
			_, writeIdentity, writeOK := s.authorizeWorkflowMemory(w, r, req.ProjectID, memoryWriteExplicit)
			if !writeOK {
				return
			}
			if writeIdentity != identity {
				writeMemoryError(w, http.StatusForbidden, "forbidden", "project memory access denied")
				return
			}
		}
		trustedMemoryIdentity = identity
	}
	// Workflow ids are caller-chosen, so creation must never replace a stored
	// workflow or its input artifact.
	if _, creating := s.creatingWorkflows.LoadOrStore(req.ID, struct{}{}); creating {
		writeWorkflowExists(w)
		return
	}
	defer s.creatingWorkflows.Delete(req.ID)
	if _, err := s.workflows.Get(r.Context(), req.ID); err == nil {
		writeWorkflowExists(w)
		return
	} else if !errors.Is(err, workflowkit.ErrRunNotFound) {
		writeInternalError(w, "workflow_error", err)
		return
	}
	operationCtx := r.Context()
	var pendingShutdown *pendingShutdownTracker
	if runMode == RunModeSync {
		pendingShutdown = newPendingShutdownTracker()
		handle, accepted := s.executions.Begin(req.ID, executionSyncWorkflow, func(ctx context.Context) error {
			return s.finalizeWorkflowShutdownTracked(ctx, req.ID, pendingShutdown)
		})
		if !accepted {
			writeHostDraining(w)
			return
		}
		defer handle.Done()
		operationCtx = withPendingShutdownTracker(operationCtx, pendingShutdown)
	}
	inputRef := "artifact:" + req.ID + ":input"
	if err := putTextArtifact(operationCtx, s.artifacts, inputRef, req.Input); err != nil {
		writeInternalError(w, "artifact_error", err)
		return
	}
	metadata := map[string]any{
		"input_ref":    inputRef,
		"run_mode":     string(runMode),
		"task_profile": profile,
	}
	if len(skillRefs) > 0 {
		metadata["skill_refs"] = skillRefs
	}
	if s.memory != nil {
		metadata[memoryTenantMetadataKey] = trustedMemoryIdentity.TenantID
		metadata[memoryProjectMetadataKey] = req.ProjectID
		metadata[memoryUserMetadataKey] = trustedMemoryIdentity.Subject
		metadata[memoryWriteIntentMetadataKey] = req.MemoryWriteIntent
	}
	run := workflowkit.WorkflowRun{
		ID:       req.ID,
		InputRef: inputRef,
		Metadata: metadata,
	}
	if runMode == RunModeQueued {
		run.Status = workflowkit.StatusPending
		if err := s.createWorkflow(operationCtx, run); err != nil {
			if errors.Is(err, workflowkit.ErrRunExists) {
				writeWorkflowExists(w)
				return
			}
			writeInternalError(w, "workflow_error", err)
			return
		}
		s.signalQueuedWorker()
		writeJSON(w, http.StatusAccepted, workflowToResponse(run, runMode))
		return
	}
	run, err = s.executor.Run(operationCtx, run)
	if errors.Is(err, workflowkit.ErrRunExists) {
		writeWorkflowExists(w)
		return
	}
	if err != nil {
		writeInternalError(w, "workflow_error", err)
		return
	}
	if identity, ok := pendingShutdown.Snapshot(); ok {
		pendingShutdown.Clear(identity.CheckpointID)
	}
	writeJSON(w, http.StatusAccepted, workflowToResponse(run, runMode))
}

// createWorkflow inserts run without replacing a stored workflow when the
// store supports it. The handler already checked that the id is free, so a
// store without Create only races with another host creating the same id.
func (s *Server) createWorkflow(ctx context.Context, run workflowkit.WorkflowRun) error {
	if creator, ok := s.workflows.(workflowkit.CreateStore); ok {
		return creator.Create(ctx, run)
	}
	return s.workflows.Save(ctx, run)
}

func writeWorkflowExists(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "workflow_exists", "a workflow with this id already exists")
}

// resolveSkillRefs accepts caller-supplied names only as lookup inputs. The
// host reuses its immutable catalog and GateContext, then persists the catalog
// digest so a future workflow run cannot silently select a newer skill.
func (s *Server) resolveSkillRefs(requested []workflowSkillRef) ([]map[string]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if s.skillCatalog == nil {
		return nil, errors.New("skill catalog is unavailable")
	}

	resolved := make([]map[string]string, 0, len(requested))
	seen := make(map[skillkit.Ref]struct{}, len(requested))
	for _, ref := range requested {
		if strings.TrimSpace(ref.Name) == "" {
			return nil, errors.New("skill reference name is required")
		}
		entry, err := s.skillCatalog.Resolve(skillkit.Ref{Name: ref.Name, Digest: ref.Digest})
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q: %w", ref.Name, err)
		}
		if report := skillkit.Evaluate(entry, s.skillGateContext); report.State != skillkit.AvailabilityEligible {
			return nil, fmt.Errorf("skill %q is unavailable", entry.Ref.Name)
		}
		if _, exists := seen[entry.Ref]; exists {
			return nil, fmt.Errorf("duplicate skill reference %q", entry.Ref.Name)
		}
		seen[entry.Ref] = struct{}{}
		resolved = append(resolved, map[string]string{
			"name":   entry.Ref.Name,
			"digest": entry.Ref.Digest,
		})
	}
	return resolved, nil
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.queries == nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", "workflow query store is not configured")
		return
	}
	query, err := parseWorkflowQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if s.memory != nil {
		// Listing is scoped to one authorized project of the caller's tenant.
		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		_, identity, ok := s.authorizeWorkflowMemory(w, r, projectID, memoryRead)
		if !ok {
			return
		}
		if query.MetadataEquals == nil {
			query.MetadataEquals = map[string]string{}
		}
		query.MetadataEquals[memoryTenantMetadataKey] = identity.TenantID
		query.MetadataEquals[memoryProjectMetadataKey] = projectID
	}
	runs, err := s.queries.ListWorkflows(r.Context(), query)
	if err != nil {
		writeInternalError(w, "workflow_error", err)
		return
	}
	workflows := make([]workflowResponse, 0, len(runs))
	for _, run := range runs {
		workflows = append(workflows, workflowToResponse(run, RunModeSync))
	}
	writeJSON(w, http.StatusOK, workflowListResponse{Workflows: workflows})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if _, ok := s.authorizeStoredWorkflow(w, r, run, memoryRead); !ok {
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleWorkflowEvents(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if _, ok := s.authorizeStoredWorkflow(w, r, run, memoryRead); !ok {
		return
	}
	writeJSON(w, http.StatusOK, workflowToEventsResponse(run))
}

func (s *Server) handleRequeueWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stored, err := s.workflows.Get(r.Context(), id)
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	// A requeued run executes again under its stored memory scope, and with
	// write intent it writes as its creator, so only the creator may requeue it.
	capability := memoryRead
	writeIntent, _ := stored.Metadata[memoryWriteIntentMetadataKey].(bool)
	if writeIntent {
		capability = memoryWriteExplicit
	}
	identity, ok := s.authorizeStoredWorkflow(w, r, stored, capability)
	if !ok {
		return
	}
	if creator, _ := stored.Metadata[memoryUserMetadataKey].(string); s.memory != nil && writeIntent && identity.Subject != creator {
		writeMemoryError(w, http.StatusForbidden, "forbidden", "only the workflow creator can requeue a workflow with memory write intent")
		return
	}
	run, err := s.workflows.Update(r.Context(), id, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if run.Status != workflowkit.StatusFailed && run.Status != workflowkit.StatusCancelled {
			return run, errWorkflowNotRequeueable
		}
		fromStatus := run.Status
		run.Status = workflowkit.StatusPending
		run.Error = ""
		run.CurrentStep = ""
		run.ApprovalRef = ""
		run.WaitingReason = ""
		run.LeaseOwner = ""
		run.LeaseUntil = time.Time{}
		run.StepRetries = 0
		run.NextAttemptAt = time.Time{}
		if run.Metadata == nil {
			run.Metadata = make(map[string]any)
		}
		run.Metadata["run_mode"] = string(RunModeQueued)
		appendRequeueEvent(run.Metadata, fromStatus, workflowkit.StatusPending, time.Now().UTC())
		return run, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowNotRequeueable) {
			writeError(w, http.StatusBadRequest, "invalid_request", "only failed or cancelled workflows can be requeued")
			return
		}
		writeWorkflowStoreError(w, err)
		return
	}
	s.signalQueuedWorker()
	writeJSON(w, http.StatusAccepted, workflowToResponse(run, RunModeQueued))
}

func (s *Server) handleApproveWorkflow(w http.ResponseWriter, r *http.Request) {
	identity, err := s.approvalAuthenticator.AuthenticateApproval(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeApprovalUnauthorized(w)
		return
	}
	var req approveWorkflowRequest
	if !decodeJSONStrict(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	handle, accepted := s.executions.Begin(id, executionFinalApproval, func(ctx context.Context) error {
		return s.finalizeWorkflowShutdown(ctx, id)
	})
	if !accepted {
		writeHostDraining(w)
		return
	}
	defer handle.Done()
	// Binding the approval to the final-approval request keeps this route from
	// resolving a pending tool approval, which only /agent-approve may answer.
	run, err := s.executor.Approve(r.Context(), id, workflowkit.Approval{
		ApprovalRef: "approval:" + id,
		AuditRef:    "audit:" + id + ":approval",
		Metadata: map[string]any{
			"approved_by":   identity.Subject,
			"approval_note": req.Note,
		},
	})
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if err := s.recordBusinessOutcome(r.Context(), id); err != nil {
		writeInternalError(w, "llm_audit_error", err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleGetAgentRun(w http.ResponseWriter, r *http.Request) {
	record, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, runkit.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent run not found")
			return
		}
		writeInternalError(w, "run_error", err)
		return
	}
	if s.memory != nil {
		// An agent run is visible to whoever may read its workflow.
		workflow, err := s.workflows.Get(r.Context(), record.WorkflowID)
		if errors.Is(err, workflowkit.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent run not found")
			return
		}
		if err != nil {
			writeWorkflowStoreError(w, err)
			return
		}
		if _, ok := s.authorizeStoredWorkflow(w, r, workflow, memoryRead); !ok {
			return
		}
	}
	events, err := s.runs.Events(r.Context(), record.RunID)
	if err != nil {
		writeInternalError(w, "run_error", err)
		return
	}
	writeJSON(w, http.StatusOK, agentRunToResponse(record, events))
}

func workflowToResponse(run workflowkit.WorkflowRun, runMode RunMode) workflowResponse {
	runMode = workflowRunMode(run, runMode)
	return workflowResponse{
		ID:            run.ID,
		Status:        string(run.Status),
		RunMode:       string(runMode),
		SkillRefs:     workflowSkillRefsFromMetadata(run.Metadata),
		InputRef:      run.InputRef,
		OutputRef:     run.OutputRef,
		AgentRunID:    run.AgentRunID,
		AuditRef:      run.AuditRef,
		ApprovalRef:   run.ApprovalRef,
		WaitingReason: run.WaitingReason,
		AgentApproval: agentApprovalFromMetadata(run.Metadata),
		Completed:     append([]string(nil), run.CompletedSteps...),
	}
}

func workflowSkillRefsFromMetadata(metadata map[string]any) []workflowSkillRef {
	switch refs := metadata["skill_refs"].(type) {
	case []map[string]string:
		response := make([]workflowSkillRef, 0, len(refs))
		for _, ref := range refs {
			if ref["name"] == "" || ref["digest"] == "" {
				return nil
			}
			response = append(response, workflowSkillRef{Name: ref["name"], Digest: ref["digest"]})
		}
		return response
	case []any:
		response := make([]workflowSkillRef, 0, len(refs))
		for _, value := range refs {
			ref, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			name, nameOK := ref["name"].(string)
			digest, digestOK := ref["digest"].(string)
			if !nameOK || !digestOK || name == "" || digest == "" {
				return nil
			}
			response = append(response, workflowSkillRef{Name: name, Digest: digest})
		}
		return response
	default:
		return nil
	}
}

func workflowToEventsResponse(run workflowkit.WorkflowRun) workflowEventsResponse {
	events := make([]workflowEventResponse, 0, len(run.StepRecords))
	for _, record := range run.StepRecords {
		events = append(events, stepRecordToEvent(record))
	}
	events = append(events, requeueEventsFromMetadata(run.Metadata)...)
	sort.SliceStable(events, func(i, j int) bool {
		left := workflowEventTime(events[i])
		right := workflowEventTime(events[j])
		if left.IsZero() || right.IsZero() {
			return false
		}
		return left.Before(right)
	})
	return workflowEventsResponse{
		WorkflowID:  run.ID,
		Status:      string(run.Status),
		RunMode:     string(workflowRunMode(run, RunModeSync)),
		CurrentStep: run.CurrentStep,
		Completed:   append([]string(nil), run.CompletedSteps...),
		Events:      events,
	}
}

func stepRecordToEvent(record workflowkit.StepRecord) workflowEventResponse {
	return workflowEventResponse{
		Type:          "step",
		Name:          record.Name,
		Status:        string(record.Status),
		Attempt:       record.Attempt,
		OutputRef:     record.OutputRef,
		AgentRunID:    record.AgentRunID,
		AuditRef:      record.AuditRef,
		Error:         record.Error,
		ApprovalRef:   record.ApprovalRef,
		WaitingReason: record.WaitingReason,
		StartedAt:     record.StartedAt,
		EndedAt:       record.EndedAt,
	}
}

func workflowEventTime(event workflowEventResponse) time.Time {
	if !event.StartedAt.IsZero() {
		return event.StartedAt
	}
	return event.At
}

func appendRequeueEvent(metadata map[string]any, from workflowkit.Status, to workflowkit.Status, at time.Time) {
	event := map[string]any{
		"type":        "workflow_requeued",
		"from_status": string(from),
		"to_status":   string(to),
		"at":          at.Format(time.RFC3339Nano),
	}
	metadata[requeueEventsMetadataKey] = appendMetadataEvent(metadata[requeueEventsMetadataKey], event)
}

func appendMetadataEvent(existing any, event map[string]any) []any {
	events := metadataEventSlice(existing)
	return append(events, event)
}

func metadataEventSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return append([]any(nil), typed...)
	case []map[string]any:
		events := make([]any, 0, len(typed))
		for _, event := range typed {
			events = append(events, event)
		}
		return events
	default:
		return nil
	}
}

func requeueEventsFromMetadata(metadata map[string]any) []workflowEventResponse {
	rawEvents := metadataEventSlice(metadata[requeueEventsMetadataKey])
	events := make([]workflowEventResponse, 0, len(rawEvents))
	for _, raw := range rawEvents {
		event, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if metadataString(event, "type") != "workflow_requeued" {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, metadataString(event, "at"))
		events = append(events, workflowEventResponse{
			Type:       "workflow_requeued",
			FromStatus: metadataString(event, "from_status"),
			ToStatus:   metadataString(event, "to_status"),
			At:         at,
		})
	}
	return events
}

func metadataString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func workflowRunMode(run workflowkit.WorkflowRun, fallback RunMode) RunMode {
	if value, ok := run.Metadata["run_mode"].(string); ok {
		if runMode, valid := parseRunMode(value); valid {
			return runMode
		}
	}
	if fallback == "" {
		return RunModeSync
	}
	return fallback
}

func parseWorkflowQuery(r *http.Request) (workflowkit.WorkflowQuery, error) {
	values := r.URL.Query()
	query := workflowkit.WorkflowQuery{Limit: 50}
	if rawStatus := strings.TrimSpace(values.Get("status")); rawStatus != "" {
		status := workflowkit.Status(rawStatus)
		if !status.IsValid() {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported workflow status %q", rawStatus)
		}
		query.Status = status
	}
	if rawRunMode := strings.TrimSpace(values.Get("run_mode")); rawRunMode != "" {
		runMode, ok := parseRunMode(rawRunMode)
		if !ok {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported run_mode %q", rawRunMode)
		}
		query.MetadataEquals = map[string]string{"run_mode": string(runMode)}
	}
	if rawOrder := strings.TrimSpace(values.Get("order")); rawOrder != "" {
		order := workflowkit.WorkflowOrder(rawOrder)
		if !order.IsValid() || order == "" {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported workflow order %q", rawOrder)
		}
		query.Order = order
	}
	if rawLimit := strings.TrimSpace(values.Get("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("limit must be a positive integer")
		}
		query.Limit = limit
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query, nil
}

func parseRunMode(value string) (RunMode, bool) {
	switch RunMode(strings.TrimSpace(value)) {
	case "", RunModeSync:
		return RunModeSync, true
	case RunModeQueued:
		return RunModeQueued, true
	default:
		return "", false
	}
}
