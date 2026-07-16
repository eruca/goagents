//go:build provideracceptance

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	"github.com/eruca/goagents/llmkit/llmkit"
)

type realProviderConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

type realProviderProbeTool struct {
	marker string
}

func (realProviderProbeTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "verification_probe",
		Description: "Returns the exact verification marker required by the user request.",
		Permission:  policy.PermissionRead,
		Timeout:     5 * time.Second,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
	}
}

func (tool realProviderProbeTool) Execute(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
	return &tools.Result{ForLLM: tool.marker, ForUser: "probe completed"}, nil
}

type realProviderCaptureRecorder struct {
	outcome *llmkit.TaskOutcome
}

type realProviderResponseCapture struct {
	provider ports.LLMClient
	content  string
}

func (capture *realProviderResponseCapture) Chat(ctx context.Context, request ports.ChatRequest) (*ports.ChatResponse, error) {
	response, err := capture.provider.Chat(ctx, request)
	if response != nil {
		capture.content = response.Content
	}
	return response, err
}

func (*realProviderCaptureRecorder) RecordRoute(context.Context, llmkit.RouteTrace) error {
	return nil
}

func (recorder *realProviderCaptureRecorder) RecordOutcome(_ context.Context, outcome llmkit.TaskOutcome) error {
	recorder.outcome = &outcome
	return nil
}

func TestRealProviderMVPAcceptance(t *testing.T) {
	config := requireRealProviderConfig(t)
	t.Logf("provider=openai_compatible model=%s", config.Model)
	provider := newRealProviderClient(t, config.BaseURL, config.Model, config.APIKey)

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		agent := newRealProviderAgent(t,
			agentcore.WithLLM(provider),
			agentcore.WithPromptBlocks([]prompt.Block{{
				Name:    "mvp-text-smoke",
				Mode:    prompt.ModeCacheable,
				Content: "Answer directly in one short sentence.",
			}}),
		)
		result, err := agent.RunDetailed(ctx, agentcore.RunRequest{
			Input: "Reply with a short confirmation that the provider is reachable.",
		})
		if err != nil {
			t.Fatalf("real provider text request failed with %T", err)
		}
		if result == nil || strings.TrimSpace(result.Content) == "" || result.ExecutionSummary.LLMCalls != 1 {
			t.Fatalf("real provider text result did not contain one completed LLM response")
		}
	})

	t.Run("tool_observation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		marker := newRealProviderObservationMarker(t)
		registry := tools.NewRegistry()
		registry.Register(realProviderProbeTool{marker: marker})
		agent := newRealProviderAgent(t,
			agentcore.WithLLM(provider),
			agentcore.WithToolRegistry(registry),
			agentcore.WithPromptBlocks([]prompt.Block{{
				Name: "mvp-tool-smoke",
				Mode: prompt.ModeCacheable,
				Content: "For this request, call verification_probe exactly once before answering. " +
					"After the tool result, return its exact marker unchanged in the final answer.",
			}}),
		)
		result, err := agent.RunDetailed(ctx, agentcore.RunRequest{
			Input:              "Run the verification probe and report its exact marker.",
			AllowedPermissions: []policy.Permission{policy.PermissionRead},
		})
		if err != nil {
			t.Fatalf("real provider tool request failed with %T", err)
		}
		if result == nil || result.ExecutionSummary.LLMCalls != 2 || result.ExecutionSummary.ToolCalls != 1 {
			t.Fatalf("real provider tool result did not complete one tool call between two LLM calls")
		}
		if len(result.ExecutionSummary.UsedTools) != 1 || result.ExecutionSummary.UsedTools[0] != "verification_probe" {
			t.Fatalf("real provider used tools = %v, want verification_probe", result.ExecutionSummary.UsedTools)
		}
		if !strings.Contains(result.Content, marker) {
			t.Fatal("real provider final answer did not contain the tool observation marker")
		}
	})

	t.Run("structured_output_success", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		agent := newRealProviderAgent(t,
			agentcore.WithLLM(provider),
			agentcore.WithOutputFormat(agentcore.OutputFormat{
				Name:         "mvp_provider_status",
				Instructions: "Return only the requested JSON object.",
				JSONSchema: json.RawMessage(`{
					"type":"object",
					"required":["status","source"],
					"properties":{
						"status":{"const":"ok"},
						"source":{"const":"qwen_smoke"}
					},
					"additionalProperties":false
				}`),
			}),
		)
		result, err := agent.RunDetailed(ctx, agentcore.RunRequest{
			Input: "Return status ok from source qwen_smoke.",
		})
		if err != nil {
			t.Fatalf("real provider structured-output request failed with %T", err)
		}
		if result == nil || len(result.StructuredOutput) == 0 || !json.Valid(result.StructuredOutput) {
			t.Fatal("real provider structured output was not locally validated JSON")
		}
	})

	t.Run("structured_output_failure", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		capture := &realProviderResponseCapture{provider: provider}
		agent := newRealProviderAgent(t,
			agentcore.WithLLM(capture),
			agentcore.WithOutputFormat(agentcore.OutputFormat{
				Name:         "mvp_impossible_schema",
				Instructions: "Return any JSON object.",
				JSONSchema:   json.RawMessage(`{"not":{}}`),
			}),
		)
		partial, err := agent.RunDetailed(ctx, agentcore.RunRequest{Input: "Return a JSON object."})
		if !errors.Is(err, agentcore.ErrOutputInvalid) || partial == nil {
			t.Fatal("real provider invalid structured output did not fail closed with a partial run")
		}
		if !json.Valid([]byte(strings.TrimSpace(capture.content))) {
			t.Fatal("real provider schema failure was not caused by valid JSON rejected by the schema")
		}
	})

	t.Run("auth_error_classification", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		assertRealProviderAuthClassification(t, ctx, config.BaseURL, config.Model)
	})

	t.Run("timeout_error_classification", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		assertRealProviderTimeoutClassification(t, ctx, config)
	})
}

func newRealProviderObservationMarker(t *testing.T) string {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate tool observation nonce: %v", err)
	}
	return "MVP_TOOL_OBSERVATION_" + hex.EncodeToString(nonce)
}

func requireRealProviderConfig(t *testing.T) realProviderConfig {
	t.Helper()
	config := realProviderConfig{
		BaseURL: strings.TrimSpace(os.Getenv("OPENAI_COMPAT_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("OPENAI_COMPAT_MODEL")),
		APIKey:  strings.TrimSpace(os.Getenv("OPENAI_COMPAT_API_KEY")),
	}
	if config.BaseURL == "" || config.Model == "" || config.APIKey == "" {
		t.Skip("real Provider acceptance requires OPENAI_COMPAT_BASE_URL, OPENAI_COMPAT_MODEL, and OPENAI_COMPAT_API_KEY")
	}
	return config
}

func newRealProviderClient(t *testing.T, baseURL, model, apiKey string) *openaiapi.Client {
	return newRealProviderClientWithTimeout(t, baseURL, model, apiKey, 60*time.Second)
}

func newRealProviderClientWithTimeout(t *testing.T, baseURL, model, apiKey string, timeout time.Duration) *openaiapi.Client {
	t.Helper()
	client, err := openaiapi.New(openaiapi.Config{
		BaseURL:    baseURL,
		Model:      model,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: timeout},
	})
	if err != nil {
		t.Fatalf("construct real provider client: %v", err)
	}
	return client
}

func newRealProviderAgent(t *testing.T, options ...agentcore.Option) *agentcore.Agent {
	t.Helper()
	agent, err := agentcore.NewAgent(options...)
	if err != nil {
		t.Fatalf("construct real provider agent: %v", err)
	}
	return agent
}

func assertRealProviderAuthClassification(t *testing.T, ctx context.Context, baseURL, model string) {
	t.Helper()
	invalidProvider := newRealProviderClient(t, baseURL, model, "invalid-auth-smoke")
	recorder := &realProviderCaptureRecorder{}
	client := newRealProviderAuditClient(invalidProvider, recorder, "mvp-auth-smoke")
	_, err := client.Chat(ctx, ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "authentication classification smoke"}},
	})
	if err == nil {
		t.Fatal("real provider accepted the deliberately invalid credential")
	}
	if recorder.outcome == nil || recorder.outcome.Success || recorder.outcome.ErrorCode != "provider_error" || recorder.outcome.ErrorClass != llmkit.ErrorClassAuth {
		t.Fatalf("real provider auth outcome class=%q code=%q, want auth_error/provider_error", realProviderOutcomeClass(recorder.outcome), realProviderOutcomeCode(recorder.outcome))
	}
}

func assertRealProviderTimeoutClassification(t *testing.T, ctx context.Context, config realProviderConfig) {
	t.Helper()
	timeoutProvider := newRealProviderClientWithTimeout(t, config.BaseURL, config.Model, config.APIKey, time.Nanosecond)
	recorder := &realProviderCaptureRecorder{}
	client := newRealProviderAuditClient(timeoutProvider, recorder, "mvp-timeout-smoke")
	_, err := client.Chat(ctx, ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "timeout classification smoke"}},
	})
	if err == nil {
		t.Fatal("real provider request unexpectedly completed within the forced timeout")
	}
	if recorder.outcome == nil || recorder.outcome.Success || recorder.outcome.ErrorCode != "provider_error" || recorder.outcome.ErrorClass != llmkit.ErrorClassTimeout {
		t.Fatalf("real provider timeout outcome class=%q code=%q, want timeout/provider_error", realProviderOutcomeClass(recorder.outcome), realProviderOutcomeCode(recorder.outcome))
	}
}

func newRealProviderAuditClient(provider goagentadapter.ProviderClient, recorder *realProviderCaptureRecorder, routeID string) *goagentadapter.Client {
	const modelAlias = "qwen-provider-smoke"
	return goagentadapter.NewClient(goagentadapter.Config{
		Candidates: []llmkit.Candidate{{
			Model: llmkit.ModelCapability{
				Alias:              modelAlias,
				Provider:           "openai_compatible",
				CapabilityLevel:    llmkit.CapabilityAdvanced,
				SupportsTools:      true,
				SupportsJSON:       true,
				ContextWindowClass: llmkit.ContextLong,
				PriceClass:         llmkit.PriceMedium,
				LatencyClass:       llmkit.LatencyNormalClass,
			},
			AccountAlias: "qwen-smoke-account",
		}},
		Providers: map[string]goagentadapter.ProviderClient{modelAlias: provider},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			return goagentadapter.RouteMetadata{
				RouteID: routeID,
				TaskID:  routeID,
				Attempt: 1,
			}
		},
		Recorder:       recorder,
		RecordOutcomes: true,
	})
}

func realProviderOutcomeClass(outcome *llmkit.TaskOutcome) llmkit.ErrorClass {
	if outcome == nil {
		return ""
	}
	return outcome.ErrorClass
}

func realProviderOutcomeCode(outcome *llmkit.TaskOutcome) string {
	if outcome == nil {
		return ""
	}
	return outcome.ErrorCode
}

var _ ports.Tool = realProviderProbeTool{}
