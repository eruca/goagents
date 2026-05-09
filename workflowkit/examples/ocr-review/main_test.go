package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/eruca/ocrs"
	"github.com/eruca/workflowkit"
)

func TestRunExample(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"workflow=wf-ocr status=waiting_approval",
		"ocr=artifact:ocr-result",
		"context=artifact:context-projection",
		"approval=approval:wf-ocr",
		"agent_run=",
		"workflow=wf-ocr status=succeeded",
		"output=artifact:ocr-review-final",
		"audit=audit:ocr-review-approved",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestOCRStepStoresRawPayloadBehindArtifactRefAndMetadataPreviewIsBounded(t *testing.T) {
	artifacts := newArtifactStore()
	longChunk := strings.Repeat("clinical-note-", 20)
	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(context.Context, []byte) (ocrs.OCRResult, error) {
		raw, err := json.Marshal(ocrPayload{
			Title:  "Discharge Summary",
			Chunks: []string{longChunk},
		})
		if err != nil {
			return ocrs.OCRResult{}, err
		}
		return ocrs.OCRResult{Provider: "mockocr", Raw: raw}, nil
	})

	result, err := (ocrStep{handler: handler, artifacts: artifacts}).Run(context.Background(), workflowkit.WorkflowRun{
		ID:       "wf-ocr-boundary",
		InputRef: "artifact:document-input",
	})
	if err != nil {
		t.Fatalf("ocr step returned error: %v", err)
	}

	if result.OutputRef != "artifact:ocr-result" {
		t.Fatalf("output ref = %q, want artifact:ocr-result", result.OutputRef)
	}
	if !strings.Contains(artifacts.Get(result.OutputRef), longChunk) {
		t.Fatalf("raw OCR payload was not stored behind output ref")
	}
	preview, _ := result.Metadata["ocr_preview"].(string)
	if len(preview) > 96 {
		t.Fatalf("metadata preview length = %d, want <= 96", len(preview))
	}
	if preview == longChunk {
		t.Fatalf("metadata preview should be bounded, not full raw payload")
	}
	if _, ok := result.Metadata["raw"]; ok {
		t.Fatalf("metadata must not include raw OCR payload: %+v", result.Metadata)
	}
}
