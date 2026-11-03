package openaiapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func sseServer(t *testing.T, events []string, check func(body []byte)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if check != nil {
			check(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func collectDeltas(deltas *[]string) func(ports.ChatDelta) error {
	return func(delta ports.ChatDelta) error {
		*deltas = append(*deltas, delta.Content)
		return nil
	}
}

func TestChatStreamDeliversDeltasAndAssemblesResponse(t *testing.T) {
	var sent map[string]any
	server := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		`[DONE]`,
	}, func(body []byte) {
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	})
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}},
	}, collectDeltas(&deltas))
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" {
		t.Fatalf("deltas = %q", deltas)
	}
	if resp.Content != "Hello" || resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("response = %+v", resp)
	}
	options, _ := sent["stream_options"].(map[string]any)
	if sent["stream"] != true || options["include_usage"] != true {
		t.Fatalf("request = %v, want stream with usage", sent)
	}
}

func TestChatStreamMergesToolCallArgumentsByIndex(t *testing.T) {
	server := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go\"}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`[DONE]`,
	}, nil)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}

	var deltas []ports.ChatDelta
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, func(delta ports.ChatDelta) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", resp.ToolCalls)
	}
	// Pieces of each call, joined by index, rebuild the response's calls.
	streamed := assembleToolCallDeltas(t, deltas)
	if !reflect.DeepEqual(streamed, resp.ToolCalls) {
		t.Fatalf("streamed tool calls = %+v, response = %+v", streamed, resp.ToolCalls)
	}
	if deltas[0].ToolCall == nil || deltas[0].ToolCall.ID != "call_1" || deltas[0].ToolCall.Name != "lookup" {
		t.Fatalf("first delta = %+v, want the call header before its arguments", deltas[0])
	}
	first, second := resp.ToolCalls[0], resp.ToolCalls[1]
	if first.ID != "call_1" || first.Name != "lookup" || string(first.Input) != `{"q":"go"}` ||
		second.ID != "call_2" || second.Name != "read" || string(second.Input) != `{}` {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
}

func TestChatStreamStopsWhenDeltaHandlerFails(t *testing.T) {
	server := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"one"}}]}`,
		`{"choices":[{"delta":{"content":"two"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	}, nil)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("consumer gone")
	calls := 0
	_, err = client.ChatStream(context.Background(), ports.ChatRequest{}, func(ports.ChatDelta) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want handler error after first delta", err, calls)
	}
}

func TestChatStreamRequiresUsage(t *testing.T) {
	server := sseServer(t, []string{`{"choices":[{"delta":{"content":"hi"}}]}`, `[DONE]`}, nil)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	if _, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collectDeltas(&deltas)); !errors.Is(err, ErrUsageMissing) {
		t.Fatalf("err = %v, want ErrUsageMissing", err)
	}
}

func TestChatStreamEnforcesResponseByteLimit(t *testing.T) {
	events := []string{}
	for range 50 {
		events = append(events, `{"choices":[{"delta":{"content":"0123456789"}}]}`)
	}
	server := sseServer(t, events, nil)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model", Limits: Limits{MaxResponseBytes: 512}})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	if _, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collectDeltas(&deltas)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
}

func TestChatStreamAcceptsJSONResponseFromNonStreamingServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"whole"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collectDeltas(&deltas))
	if err != nil || resp.Content != "whole" || strings.Join(deltas, "|") != "whole" {
		t.Fatalf("response = %+v, deltas = %q, err = %v", resp, deltas, err)
	}
}

func TestChatStreamParsesCRLFAndMultiLineEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keep-alive\r\n\r\n"+
			"data: {\"choices\":[{\"delta\":\r\ndata: {\"content\":\"ok\"}}]}\r\n\r\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\r\n\r\n")
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collectDeltas(&deltas))
	if err != nil || resp.Content != "ok" {
		t.Fatalf("response = %+v, err = %v", resp, err)
	}
}

func TestChatStreamReportsHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var responseErr *ResponseError
	_, err = client.ChatStream(context.Background(), ports.ChatRequest{}, func(ports.ChatDelta) error { return nil })
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want ResponseError 429", err)
	}
}

func TestProviderStreamRequestSHA256MatchesDispatchedBody(t *testing.T) {
	var dispatched []byte
	server := sseServer(t, []string{`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`}, func(body []byte) {
		dispatched = body
	})
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	req := ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}, MaxOutputTokens: 64}
	want, err := client.ProviderStreamRequestSHA256(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ChatStream(context.Background(), req, func(ports.ChatDelta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(dispatched) != want {
		t.Fatal("stream request digest does not match the dispatched body")
	}
}

func TestChatStreamReportsCancellationMidStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = client.ChatStream(ctx, ports.ChatRequest{}, func(ports.ChatDelta) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func assembleToolCallDeltas(t *testing.T, deltas []ports.ChatDelta) []ports.ToolCall {
	t.Helper()
	var calls []ports.ToolCall
	for _, delta := range deltas {
		if delta.Content != "" {
			t.Fatalf("unexpected content delta %q", delta.Content)
		}
		piece := delta.ToolCall
		if piece == nil {
			continue
		}
		for len(calls) <= piece.Index {
			calls = append(calls, ports.ToolCall{})
		}
		call := &calls[piece.Index]
		if piece.ID != "" {
			call.ID = piece.ID
		}
		if piece.Name != "" {
			call.Name = piece.Name
		}
		call.Input = append(call.Input, piece.Arguments...)
	}
	return calls
}

func TestChatStreamJSONFallbackStreamsToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"go\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []ports.ChatDelta
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, func(delta ports.ChatDelta) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if streamed := assembleToolCallDeltas(t, deltas); !reflect.DeepEqual(streamed, resp.ToolCalls) || len(streamed) != 1 {
		t.Fatalf("streamed = %+v, response = %+v", streamed, resp.ToolCalls)
	}
}
