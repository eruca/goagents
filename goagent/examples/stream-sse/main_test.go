package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEHandlerStreamsRuntimeAndDoneEvents(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/runs/stream", nil)
	rec := httptest.NewRecorder()

	newSSEHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	body := rec.Body.String()
	assertContains(t, body, "event: runtime\n")
	assertContains(t, body, `"type":"approval.requested"`)
	assertContains(t, body, `"type":"approval.completed"`)
	assertContains(t, body, `"type":"tool.completed"`)
	assertContains(t, body, `"type":"finalized"`)
	assertContains(t, body, `"tool":"update_draft"`)
	assertContains(t, body, `"ref":"draft:demo"`)
	assertContains(t, body, "event: done\n")
	assertContains(t, body, `"content":"Final answer: draft updated after approval."`)
	assertContains(t, body, `"llm_calls":2`)
	assertContains(t, body, `"tool_calls":1`)
	assertContains(t, body, `"used_tools":["update_draft"]`)
	assertNotContains(t, body, `"type":"stage.started"`)
	assertNotContains(t, body, `"type":"stage.completed"`)
}

func TestSSEHandlerRejectsNonGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/runs/stream", nil)
	rec := httptest.NewRecorder()

	newSSEHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func assertContains(t *testing.T, s string, want string) {
	t.Helper()
	if !strings.Contains(s, want) {
		t.Fatalf("missing %q in:\n%s", want, s)
	}
}

func assertNotContains(t *testing.T, s string, want string) {
	t.Helper()
	if strings.Contains(s, want) {
		t.Fatalf("found %q in:\n%s", want, s)
	}
}
