package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/eruca/contextkit"
	"github.com/eruca/contextkit/toolprojection"
	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/ocrs"
	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/agentstep"
)

type artifactStore struct {
	values map[string]string
}

func newArtifactStore() *artifactStore {
	return &artifactStore{values: make(map[string]string)}
}

func (s *artifactStore) Put(ref string, value string) string {
	s.values[ref] = value
	return ref
}

func (s *artifactStore) Get(ref string) string {
	return s.values[ref]
}

type ocrPayload struct {
	Title  string   `json:"title"`
	Chunks []string `json:"chunks"`
}

type ocrStep struct {
	handler   ocrs.Handler[[]byte, ocrs.OCRResult]
	artifacts *artifactStore
}

func (s ocrStep) Name() string {
	return "ocr"
}

func (s ocrStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	result, err := s.handler.Handle(ctx, []byte(run.InputRef))
	if err != nil {
		return workflowkit.StepResult{}, err
	}
	var payload ocrPayload
	if err := json.Unmarshal(result.Raw, &payload); err != nil {
		return workflowkit.StepResult{}, err
	}
	ref := s.artifacts.Put("artifact:ocr-result", string(result.Raw))
	preview := strings.Join(payload.Chunks, " ")
	if len(preview) > 96 {
		preview = preview[:96]
	}
	return workflowkit.StepResult{
		Status:    workflowkit.StatusRunning,
		OutputRef: ref,
		Metadata: map[string]any{
			"ocr_provider": result.Provider,
			"ocr_title":    payload.Title,
			"chunk_count":  len(payload.Chunks),
			"ocr_preview":  preview,
		},
	}, nil
}

type contextStep struct {
	artifacts *artifactStore
}

func (s contextStep) Name() string {
	return "context_projection"
}

func (s contextStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	msg := contextkit.Message{
		Role:       contextkit.RoleTool,
		ToolName:   "ocr_document",
		ToolCallID: "call-ocr-1",
		Status:     "success",
		Ref:        run.OutputRef,
		Content:    fmt.Sprint(run.Metadata["ocr_preview"]),
	}
	projected := toolprojection.Project(msg, toolprojection.Config{MaxResultChars: 80})
	ref := s.artifacts.Put("artifact:context-projection", projected.Content)
	return workflowkit.StepResult{
		Status:    workflowkit.StatusRunning,
		OutputRef: ref,
		Metadata: map[string]any{
			"context_ref":       ref,
			"context_projected": projected.Metadata["contextkit.tool_projected"],
		},
	}, nil
}

type mockAgent struct {
	runID agentcore.RunID
}

func (a mockAgent) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	runID := a.runID
	if runID.IsZero() {
		runID = agentcore.NewRunID()
	}
	return &agentcore.RunResult{
		RunID:   runID,
		Content: "OCR review requires operator confirmation before finalizing",
	}, nil
}

type finalizeStep struct {
	artifacts *artifactStore
}

func (s finalizeStep) Name() string {
	return "finalize"
}

func (s finalizeStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	ref := s.artifacts.Put("artifact:ocr-review-final", "approved OCR review")
	return workflowkit.StepResult{
		Status:    workflowkit.StatusSucceeded,
		OutputRef: ref,
	}, nil
}

func run(ctx context.Context, out io.Writer) error {
	artifacts := newArtifactStore()
	ocrHandler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		raw, err := json.Marshal(ocrPayload{
			Title: "Discharge Summary",
			Chunks: []string{
				"Patient admitted for fever and dyspnea.",
				"OCR extracted medication and follow-up instructions.",
			},
		})
		if err != nil {
			return ocrs.OCRResult{}, err
		}
		return ocrs.OCRResult{Provider: "mockocr", Raw: raw}, nil
	})

	agent := mockAgent{runID: agentcore.NewRunID()}
	agentStep := agentstep.New("agent_review", agent, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{
			Input:     "review " + fmt.Sprint(run.Metadata["context_ref"]),
			SessionID: run.ID,
		}
	}, agentstep.WithResultMapper(func(run workflowkit.WorkflowRun, result *agentcore.RunResult) workflowkit.StepResult {
		return workflowkit.StepResult{
			Status:        workflowkit.StatusWaitingApproval,
			AgentRunID:    result.RunID.String(),
			ApprovalRef:   "approval:" + run.ID,
			WaitingReason: result.Content,
			Metadata: map[string]any{
				"agent_content_preview": result.Content,
			},
		}
	}))

	store := workflowkit.NewMemoryStore()
	executor := workflowkit.NewExecutor(store, []workflowkit.Step{
		ocrStep{handler: ocrHandler, artifacts: artifacts},
		contextStep{artifacts: artifacts},
		agentStep,
		finalizeStep{artifacts: artifacts},
	})

	workflow, err := executor.Run(ctx, workflowkit.WorkflowRun{
		ID:       "wf-ocr",
		Status:   workflowkit.StatusPending,
		InputRef: "artifact:document-input",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s ocr=%s context=%s approval=%s agent_run=%s reason=%q\n",
		workflow.ID,
		workflow.Status,
		"artifact:ocr-result",
		workflow.Metadata["context_ref"],
		workflow.ApprovalRef,
		workflow.AgentRunID,
		workflow.WaitingReason,
	)

	workflow, err = executor.Approve(ctx, workflow.ID, workflowkit.Approval{
		AuditRef: "audit:ocr-review-approved",
		Metadata: map[string]any{
			"approved": true,
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s output=%s audit=%s records=%d\n",
		workflow.ID,
		workflow.Status,
		workflow.OutputRef,
		workflow.AuditRef,
		len(workflow.StepRecords),
	)
	return nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		panic(err)
	}
}
