package openaiapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func TestContextOverflowIsRecognizedWithoutLeakingTheBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"openai code", 400, `{"error":{"message":"secret-ish detail","code":"context_length_exceeded"}}`, true},
		{"vllm", 400, `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens."}`, true},
		{"llama.cpp", 400, `{"error":{"type":"exceed_context_size_error","message":"the request exceeds the available context size"}}`, true},
		{"sglang", 400, `{"message":"The input (40000 tokens) is longer than the model's context length (32768 tokens)."}`, true},
		{"payload too large", 413, `Prompt is too long`, true},
		{"rate limit", 429, `{"error":{"message":"Too many tokens per minute"}}`, false},
		{"other bad request", 400, `{"error":{"message":"unknown parameter"}}`, false},
		{"server error", 500, `maximum context length`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range map[string]func() error{
				"chat": func() error {
					_, err := client.Chat(context.Background(), ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}})
					return err
				},
				"stream": func() error {
					_, err := client.ChatStream(context.Background(), ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "hi"}}}, func(ports.ChatDelta) error { return nil })
					return err
				},
			} {
				err := call()
				var responseErr *ResponseError
				if !errors.As(err, &responseErr) || responseErr.StatusCode != c.status {
					t.Fatalf("err = %v", err)
				}
				if errors.Is(err, ports.ErrContextTooLong) != c.want {
					t.Fatalf("ErrContextTooLong = %v, want %v", !c.want, c.want)
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "maximum") {
					t.Fatalf("error leaks the body: %v", err)
				}
			}
		})
	}
}
