#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -P "$script_dir/.." && pwd -P)"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-week17-consumer.XXXXXX")"

cleanup() {
  local command_status=$?
  local cleanup_status=0

  if [[ -d "$workdir" ]]; then
    # Go module cache 会把下载内容设为只读，删除前只放宽本脚本创建的临时目录。
    if ! chmod -R u+w "$workdir"; then
      cleanup_status=1
    fi
    if ! rm -rf "$workdir"; then
      cleanup_status=1
    fi
  fi
  if (( cleanup_status != 0 )); then
    printf 'week 17 consumer cleanup failed\n' >&2
    return 1
  fi
  return "$command_status"
}
trap cleanup EXIT

workdir="$(cd -P "$workdir" && pwd -P)"
if [[ "$workdir" == "$repo_root" || "$workdir" == "$repo_root/"* ]]; then
  printf 'week 17 consumer workdir must be outside the repository\n' >&2
  exit 1
fi
for command_name in go shasum zip; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    printf 'week 17 consumer requires %s\n' "$command_name" >&2
    exit 1
  fi
done

candidate_version="v0.1.1"
goagent_module="github.com/eruca/goagents/goagent"
llmkit_module="github.com/eruca/goagents/llmkit"
proxy_root="$workdir/proxy"
archive_root="$workdir/archive"
consumer_root="$workdir/consumer"

module_snapshot_sha256() {
  local dir="$1"

  (
    cd "$repo_root/$dir"
    find . -type f -print0 |
      LC_ALL=C sort -z |
      xargs -0 shasum -a 256 |
      shasum -a 256 |
      awk '{print $1}'
  )
}

package_module() {
  local dir="$1"
  local module="$2"
  local version="$3"
  local version_root="$proxy_root/$module/@v"
  local module_archive_root="$archive_root/$module@$version"

  mkdir -p "$version_root" "$module_archive_root"
  cp -R "$repo_root/$dir/." "$module_archive_root/"
  cp "$repo_root/$dir/go.mod" "$version_root/$version.mod"
  printf '%s\n' "$version" >"$version_root/list"
  printf '{"Version":"%s","Time":"2026-08-06T00:00:00Z"}\n' "$version" \
    >"$version_root/$version.info"
  (
    cd "$archive_root"
    zip -qr "$version_root/$version.zip" "$module@$version"
  )
}

package_module goagent "$goagent_module" "$candidate_version"
package_module llmkit "$llmkit_module" "$candidate_version"

mkdir -p "$consumer_root"
cd "$consumer_root"
export GOMODCACHE="$workdir/modcache"
export GOCACHE="$workdir/buildcache"
unset GOSUMDB
export GONOSUMDB="$goagent_module,$llmkit_module"
export GOPROXY="file://$proxy_root,https://proxy.golang.org,direct"

GOWORK=off go mod init example.invalid/goagents-week17-consumer >/dev/null
cat >consumer_test.go <<'EOF'
package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/ports"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	"github.com/eruca/goagents/llmkit/llmkit"
)

type rejectingTransport struct {
	calls int
}

func (t *rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected outbound request")
}

func newProvider(t *testing.T, transport *rejectingTransport, limits openaiapi.Limits) *openaiapi.Client {
	t.Helper()
	provider, err := openaiapi.NewWithLimits(openaiapi.Config{
		BaseURL:    "https://provider.invalid/v1",
		Model:      "consumer-model",
		HTTPClient: &http.Client{Transport: transport},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

type fakeProvider struct {
	response *ports.ChatResponse
	err      error
	calls    int
}

func (p *fakeProvider) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	p.calls++
	return p.response, p.err
}

type classifiedDispatchError struct {
	dispatched bool
}

func (e classifiedDispatchError) Error() string {
	return "classified provider failure"
}

func (e classifiedDispatchError) ProviderErrorClass() string {
	return string(llmkit.ErrorClassTransient)
}

func (e classifiedDispatchError) ProviderDispatched() bool {
	return e.dispatched
}

type cancelingProvider struct {
	cancel context.CancelFunc
	calls  int
}

func (p *cancelingProvider) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	p.calls++
	p.cancel()
	return &ports.ChatResponse{Content: "late success"}, nil
}

func candidate() llmkit.Candidate {
	return llmkit.Candidate{
		Model: llmkit.ModelCapability{
			Alias:              "consumer-model",
			Provider:           "openai-compatible",
			CapabilityLevel:    llmkit.CapabilityBalanced,
			ContextWindowClass: llmkit.ContextMedium,
			PriceClass:         llmkit.PriceLow,
			LatencyClass:       llmkit.LatencyNormalClass,
		},
		AccountAlias: "consumer-account",
	}
}

func fallbackCandidates() []llmkit.Candidate {
	return []llmkit.Candidate{
		{
			Model: llmkit.ModelCapability{
				Alias:              "local-small",
				Provider:           "local",
				IsLocal:            true,
				CapabilityLevel:    llmkit.CapabilitySimple,
				ContextWindowClass: llmkit.ContextMedium,
				PriceClass:         llmkit.PriceFree,
				LatencyClass:       llmkit.LatencyFastClass,
			},
			AccountAlias: "local-account",
		},
		{
			Model: llmkit.ModelCapability{
				Alias:              "cloud-advanced",
				Provider:           "openai-compatible",
				CapabilityLevel:    llmkit.CapabilityAdvanced,
				ContextWindowClass: llmkit.ContextLong,
				PriceClass:         llmkit.PriceHigh,
				LatencyClass:       llmkit.LatencyNormalClass,
			},
			AccountAlias: "cloud-account",
		},
	}
}

func simpleProfile() llmkit.TaskProfile {
	profile := llmkit.DefaultTaskProfile()
	profile.Source = llmkit.ProfileSourceHost
	profile.TaskType = "simple"
	profile.Complexity = llmkit.ComplexitySimple
	profile.FailureCost = llmkit.FailureCostLow
	profile.Latency = llmkit.LatencyNormal
	return profile
}

func fixedProfile(context.Context, ports.ChatRequest) llmkit.TaskProfile {
	return simpleProfile()
}

func TestV010PublicStructShapesRemainSourceCompatible(t *testing.T) {
	// 这些 unkeyed literal 是 v0.1.0 合法源码；patch 版本必须继续可编译。
	_ = ports.ChatRequest{nil, nil}
	_ = agentcore.ThinkStage{nil, nil}
	_ = agentcore.ReActConfig{
		nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, agentcore.OutputFormat{}, nil, nil, 0,
	}
	_ = goagentadapter.FallbackPolicy{2}
}

func TestPublicProviderLimitIsTypedAndPreDispatch(t *testing.T) {
	transport := &rejectingTransport{}
	provider := newProvider(t, transport, openaiapi.Limits{MaxOutputTokens: 4096})

	var _ goagentadapter.ProviderClientWithMaxOutputTokens = provider
	var _ goagentadapter.ProviderRequestDigesterWithMaxOutputTokens = provider
	_, err := provider.ChatWithMaxOutputTokens(context.Background(), ports.ChatRequest{}, 4097)
	if !errors.Is(err, openaiapi.ErrMaxOutputTokensExceeded) {
		t.Fatalf("Chat() error = %v, want max-output-tokens error", err)
	}
	var classified interface{ ProviderErrorClass() string }
	if !errors.As(err, &classified) || classified.ProviderErrorClass() != string(llmkit.ErrorClassBudgetExceeded) {
		t.Fatalf("provider class = %v, want %s", err, llmkit.ErrorClassBudgetExceeded)
	}
	var dispatch interface{ ProviderDispatched() bool }
	if !errors.As(err, &dispatch) || dispatch.ProviderDispatched() {
		t.Fatalf("provider dispatch status = %v, want false", err)
	}
	if transport.calls != 0 {
		t.Fatalf("outbound calls = %d, want zero", transport.calls)
	}
}

func TestPublicPreDispatchHookFailsClosedWithoutRecorder(t *testing.T) {
	const sensitiveMessage = "external-consumer-sensitive-sentinel"
	transport := &rejectingTransport{}
	provider := newProvider(t, transport, openaiapi.Limits{
		MaxOutputTokens:  4096,
		MaxRequestBytes:  128 << 10,
		MaxResponseBytes: 128 << 10,
	})
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	claimErr := errors.New("claim unavailable")
	hookCalls := 0
	var hookRequest goagentadapter.PreDispatchRequest
	client := goagentadapter.NewClientWithPreDispatch(goagentadapter.Config{
		Candidates: []llmkit.Candidate{candidate()},
		Providers: map[string]goagentadapter.ProviderClient{
			"consumer-model": provider,
		},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			return goagentadapter.RouteMetadata{RouteID: "route-consumer", TaskID: "task-consumer", Attempt: 1}
		},
		HealthStore: health,
	}, goagentadapter.PreDispatchConfig{
		MetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.PreDispatchMetadata {
			return goagentadapter.PreDispatchMetadata{
				ProviderCallIndex: 1,
				RunRequestSHA256:  sha256.Sum256([]byte("run request")),
			}
		},
		Hook: func(_ context.Context, request goagentadapter.PreDispatchRequest) error {
			hookCalls++
			hookRequest = request
			return claimErr
		},
	})

	resp, err := client.ChatWithMaxOutputTokens(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: sensitiveMessage}},
	}, 4096)
	if resp != nil || !errors.Is(err, claimErr) {
		t.Fatalf("Chat() response/error = %+v/%v, want nil claim error", resp, err)
	}
	var runtimeErr *goagentadapter.RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Stage != goagentadapter.ErrorStagePreCall || runtimeErr.ProviderDispatched {
		t.Fatalf("runtime error = %+v, want pre-call and not dispatched", runtimeErr)
	}
	if hookCalls != 1 || transport.calls != 0 || len(health.Snapshot().Entries) != 0 {
		t.Fatalf("hook/outbound/health = %d/%d/%d, want 1/0/0", hookCalls, transport.calls, len(health.Snapshot().Entries))
	}
	encoded, marshalErr := json.Marshal(hookRequest)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), sensitiveMessage) {
		t.Fatalf("hook request leaked ChatRequest content: %s", encoded)
	}
	if hookRequest.ProviderRequestSHA256 == ([sha256.Size]byte{}) {
		t.Fatal("provider request digest is empty")
	}
}

func TestPublicCancellationClosesHealthInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancelingProvider{cancel: cancel}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	selected := fallbackCandidates()[0]
	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates:      []llmkit.Candidate{selected},
		Providers:       map[string]goagentadapter.ProviderClient{selected.Model.Alias: provider},
		ProfileProvider: fixedProfile,
		HealthStore:     health,
	})

	resp, err := client.Chat(ctx, ports.ChatRequest{})
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() response/error = %+v/%v, want canceled", resp, err)
	}
	entry := health.Snapshot().Entries[llmkit.ProviderHealthKey(selected.AccountAlias, selected.Model.Alias, selected.Model.Provider)]
	if provider.calls != 1 || entry.InFlight != 0 {
		t.Fatalf("provider calls/in-flight = %d/%d, want 1/0", provider.calls, entry.InFlight)
	}
}

func TestPublicRuntimeFallbackRequiresExplicitPreDispatchClass(t *testing.T) {
	local := &fakeProvider{err: classifiedDispatchError{dispatched: false}}
	cloud := &fakeProvider{response: &ports.ChatResponse{Content: "cloud fallback"}}
	client := goagentadapter.NewRuntimeClient(goagentadapter.Config{
		Candidates:      fallbackCandidates(),
		ProfileProvider: fixedProfile,
		ErrorClassifier: func(error) llmkit.ErrorClass { return llmkit.ErrorClassTransient },
		Providers: map[string]goagentadapter.ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
	}, goagentadapter.WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil || resp == nil || resp.Content != "cloud fallback" {
		t.Fatalf("Chat() response/error = %+v/%v, want cloud fallback", resp, err)
	}
	if local.calls != 1 || cloud.calls != 1 {
		t.Fatalf("provider calls local/cloud = %d/%d, want 1/1", local.calls, cloud.calls)
	}
}

func TestPublicRuntimeNeverFallsBackAfterDispatch(t *testing.T) {
	local := &fakeProvider{err: classifiedDispatchError{dispatched: true}}
	cloud := &fakeProvider{response: &ports.ChatResponse{Content: "unexpected"}}
	client := goagentadapter.NewRuntimeClient(goagentadapter.Config{
		Candidates:      fallbackCandidates(),
		ProfileProvider: fixedProfile,
		ErrorClassifier: func(error) llmkit.ErrorClass { return llmkit.ErrorClassTransient },
		Providers: map[string]goagentadapter.ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
	}, goagentadapter.WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	var runtimeErr *goagentadapter.RuntimeError
	if resp != nil || !errors.As(err, &runtimeErr) || !runtimeErr.ProviderDispatched {
		t.Fatalf("Chat() response/error = %+v/%+v, want dispatched failure", resp, runtimeErr)
	}
	if local.calls != 1 || cloud.calls != 0 {
		t.Fatalf("provider calls local/cloud = %d/%d, want 1/0", local.calls, cloud.calls)
	}
}

func TestPublicProviderRequestLimitIsTypedAndPreDispatch(t *testing.T) {
	transport := &rejectingTransport{}
	provider := newProvider(t, transport, openaiapi.Limits{MaxRequestBytes: 128})
	_, err := provider.Chat(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: strings.Repeat("x", 256)}},
	})
	if !errors.Is(err, openaiapi.ErrRequestTooLarge) || transport.calls != 0 {
		t.Fatalf("Chat() error/calls = %v/%d, want typed pre-dispatch request limit", err, transport.calls)
	}
}

func TestPublicProviderResponseLimitIsTyped(t *testing.T) {
	base := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(base + strings.Repeat(" ", 129)))
	}))
	defer server.Close()
	provider, err := openaiapi.NewWithLimits(
		openaiapi.Config{BaseURL: server.URL, Model: "consumer-model"},
		openaiapi.Limits{MaxResponseBytes: 128},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Chat(context.Background(), ports.ChatRequest{})
	if !errors.Is(err, openaiapi.ErrResponseTooLarge) {
		t.Fatalf("Chat() error = %v, want typed response limit", err)
	}
}

func TestPublicProviderBlocksCrossOriginRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls++
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	provider, err := openaiapi.New(openaiapi.Config{BaseURL: source.URL, Model: "consumer-model"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Chat(context.Background(), ports.ChatRequest{})
	if !errors.Is(err, openaiapi.ErrRedirectBlocked) || targetCalls != 0 {
		t.Fatalf("Chat() error/target calls = %v/%d, want blocked redirect and zero target calls", err, targetCalls)
	}
}

func TestPublicProviderErrorDoesNotLeakResponseBody(t *testing.T) {
	const sentinel = "provider-secret-response-sentinel"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(sentinel))
	}))
	defer server.Close()
	provider, err := openaiapi.New(openaiapi.Config{BaseURL: server.URL, Model: "consumer-model"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Chat(context.Background(), ports.ChatRequest{})
	if err == nil || strings.Contains(err.Error(), sentinel) {
		t.Fatalf("Chat() error = %v, want redacted non-2xx failure", err)
	}
}
EOF

GOWORK=off go get "$goagent_module@$candidate_version" "$llmkit_module@$candidate_version"
if grep -Eq '^[[:space:]]*replace([[:space:]]|$)' go.mod; then
  printf 'week 17 consumer unexpectedly contains replace\n' >&2
  exit 1
fi
GOWORK=off go mod tidy
GOWORK=off go test -count=1 ./...

replacement_graph="$(
  GOWORK=off go list -m -f '{{if .Replace}}{{.Path}}=>{{.Replace.Path}}{{end}}' all |
    sed '/^$/d'
)"
if [[ -n "$replacement_graph" ]]; then
  printf 'week 17 consumer module graph contains replace\n' >&2
  exit 1
fi

for module in "$goagent_module" "$llmkit_module"; do
  resolved="$(GOWORK=off go list -m -f '{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}' "$module")"
  if [[ "$resolved" != "$module|$candidate_version|" ]]; then
    printf 'week 17 consumer resolved %s, want %s|%s|\n' \
      "$resolved" "$module" "$candidate_version" >&2
    exit 1
  fi
done

printf 'week 17 snapshot: goagent=%s llmkit=%s\n' \
  "$(module_snapshot_sha256 goagent)" "$(module_snapshot_sha256 llmkit)"
printf 'Week 18 in-process release candidate verification passed\n'
