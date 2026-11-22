package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type classedError struct{}

func (classedError) Error() string              { return "provider said 血钾" }
func (classedError) ProviderErrorClass() string { return "invalid_json" }

func TestErrorCode(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("run: %w", err) }
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{wrap(agentcore.ErrMaxIterations), "max_iterations"},
		{wrap(agentcore.ErrEmptyResponse), "empty_response"},
		{&truncatedError{err: wrap(agentcore.ErrEmptyResponse)}, "output_truncated"},
		{&truncatedError{err: wrap(agentcore.ErrOutputInvalid)}, "output_truncated"},
		{wrap(agentcore.ErrOutputInvalid), "output_invalid"},
		{wrap(context.DeadlineExceeded), "timeout"},
		{wrap(&openaiapi.ResponseError{StatusCode: 503}), "llm_http_503"},
		{wrap(&openaiapi.ResponseError{StatusCode: 400, ContextTooLong: true}), "llm_context_too_long"},
		{wrap(classedError{}), "llm_invalid_json"},
		{wrap(fmt.Errorf("%w: tool %q not registered", tools.ErrToolNotFound, "x")), "tool_not_found"},
		{wrap(&ToolError{Code: "not_found"}), "tool_not_found"},
		{wrap(&url.Error{Op: "Post", URL: "http://x", Err: errors.New("refused")}), "network"},
		{wrap(errors.New("血钾 2.8")), "error_errors.errorString"},
	}
	for _, tc := range cases {
		got := errorCode(tc.err)
		if got != tc.want {
			t.Errorf("errorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
		for _, r := range got {
			if r > 127 {
				t.Errorf("code %q is not ASCII", got)
			}
		}
	}
}

func TestCompactSpec(t *testing.T) {
	full := ports.ToolSpec{Name: "labs", Description: strings.Repeat("长", 100), Schema: ports.ToolSchema{JSONSchema: json.RawMessage(
		`{"type":"object","required":["items"],"properties":{"items":{"type":"array","description":"项目"},"hours":{"type":"integer","maximum":720,"description":"小时"},"cursor":{"type":"string"},"budget":{"type":"integer"}}}`)}}
	compact, err := compactSpec(full)
	if err != nil {
		t.Fatal(err)
	}
	if compact.Description != compactTools["labs"].description {
		t.Fatalf("description = %q", compact.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(compact.Schema.JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	if len(props) != 2 || props["items"] == nil || props["hours"] == nil {
		t.Fatalf("properties = %v", props)
	}
	if hours := props["hours"].(map[string]any); hours["description"] != nil || hours["maximum"] != float64(720) {
		t.Fatalf("hours = %v", hours)
	}
	if len(compact.Schema.JSONSchema) >= len(full.Schema.JSONSchema) {
		t.Fatal("compact schema is not smaller")
	}
	// A tool without a compact form keeps its spec.
	other := ports.ToolSpec{Name: "search", Description: "d", Schema: full.Schema}
	if got, _ := compactSpec(other); got.Description != "d" {
		t.Fatal("unknown tool changed")
	}
	for _, name := range ModelTools {
		if _, ok := compactTools[name]; !ok {
			t.Errorf("no compact form for %s", name)
		}
	}
}
