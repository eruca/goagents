package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eruca/goagents/mcpkit/officialsdk"
)

// RunIDHeader carries the caller's run id; gocms records it in its audit log
// so the calls of one handoff can be found together.
const RunIDHeader = "X-Agent-Run-Id"

type runIDKey struct{}

// WithRunID returns a context whose outgoing gocms requests carry runID.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

func runIDFrom(ctx context.Context) string {
	runID, _ := ctx.Value(runIDKey{}).(string)
	return runID
}

// Transport adds the agent token and the run id of the request's context to
// every request. The token is never logged.
type Transport struct {
	Base  http.RoundTripper
	Token string
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.Token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.Token)
	}
	if runID := runIDFrom(req.Context()); runID != "" {
		clone.Header.Set(RunIDHeader, runID)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// Gocms reaches one gocms server: MCP for the tools the model and the host
// use, plain HTTP for batch expand and the checklist.
type Gocms struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGocms returns a client whose requests carry token.
func NewGocms(baseURL, token string, timeout time.Duration) *Gocms {
	return &Gocms{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Transport: &Transport{Token: token}, Timeout: timeout},
	}
}

// ConnectMCP opens an MCP session on the gocms endpoint. The session's HTTP
// client has no overall timeout; each tool call is bounded by its context.
func (g *Gocms) ConnectMCP(ctx context.Context) (*officialsdk.Client, error) {
	return officialsdk.ConnectStreamableHTTP(ctx, officialsdk.StreamableHTTPConfig{
		Endpoint:   g.BaseURL + "/api/v1/mcp",
		HTTPClient: &http.Client{Transport: g.HTTP.Transport},
		Name:       "icu-handoff",
		Version:    Version,
	})
}

// ExpandResult is gocms' verdict on one reference.
type ExpandResult struct {
	Ref   string          `json:"ref"`
	Kind  string          `json:"kind,omitempty"`
	URL   string          `json:"url,omitempty"`
	Error *ExpandError    `json:"error,omitempty"`
	Raw   json.RawMessage `json:"fields,omitempty"`
}

// ExpandError is the error gocms gives for a reference it rejects.
type ExpandError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxExpandRefs is the HTTP expand limit per request.
const maxExpandRefs = 50

// Expand checks refs against one patient through the HTTP expand endpoint:
// each must have a valid check code, exist and belong to stay.
func (g *Gocms) Expand(ctx context.Context, stay string, refs []string) (map[string]ExpandResult, error) {
	out := make(map[string]ExpandResult, len(refs))
	for start := 0; start < len(refs); start += maxExpandRefs {
		end := min(start+maxExpandRefs, len(refs))
		body, err := json.Marshal(map[string]any{"refs": refs[start:end], "context": false})
		if err != nil {
			return nil, err
		}
		var resp struct {
			Data struct {
				Result struct {
					Refs []ExpandResult `json:"refs"`
				} `json:"result"`
			} `json:"data"`
		}
		path := "/api/v1/stays/" + url.PathEscape(stay) + "/context/expand"
		if err := g.do(ctx, http.MethodPost, path, body, &resp); err != nil {
			return nil, err
		}
		for _, result := range resp.Data.Result.Refs {
			out[result.Ref] = result
		}
	}
	return out, nil
}

// Overview fetches a patient's overview over HTTP with its structured data.
func (g *Gocms) Overview(ctx context.Context, stay string, query url.Values) (string, json.RawMessage, error) {
	var resp struct {
		Text string          `json:"text"`
		Data json.RawMessage `json:"data"`
	}
	path := "/api/v1/stays/" + url.PathEscape(stay) + "/context/overview?" + query.Encode()
	if err := g.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", nil, err
	}
	return resp.Text, resp.Data, nil
}

// HTTPError is a non-2xx gocms response. Only the status and gocms' error
// code are kept; the message may name a patient.
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("gocms http %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("gocms http %d", e.Status)
}

func (g *Gocms) do(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &HTTPError{Status: resp.StatusCode, Code: e.Error.Code}
	}
	return json.Unmarshal(raw, out)
}
