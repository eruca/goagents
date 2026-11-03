#!/usr/bin/env bash
# Builds and tests a module outside the repository that depends on
# github.com/eruca/goagents the way a real consumer would: no workspace, no
# replace directives.
#
#   verify-external-consumer.sh            # the working tree, via a local proxy
#   verify-external-consumer.sh v0.2.0     # a published version, via GOPROXY
set -euo pipefail

if (( $# > 1 )); then
  printf 'usage: %s [published-version]\n' "$0" >&2
  exit 2
fi
published_version="${1:-}"

repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
module="github.com/eruca/goagents"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-consumer.XXXXXX")"
workdir="$(cd -P "$workdir" && pwd -P)"
cleanup() {
  # The module cache marks downloads read-only.
  chmod -R u+w "$workdir" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT

export GOMODCACHE="$workdir/modcache"
export GOCACHE="$workdir/buildcache"
export GOWORK=off
export GOFLAGS=-mod=mod

if [[ -z "$published_version" ]]; then
  version="v0.0.0-local"
  proxy="$workdir/proxy/$module/@v"
  stage="$workdir/stage/$module@$version"
  mkdir -p "$proxy" "$stage"
  # Tracked and untracked-but-not-ignored files: what a commit of this tree would publish.
  (cd "$repo_root" && git ls-files -z --cached --others --exclude-standard) |
    (cd "$repo_root" && xargs -0 -I{} sh -c 'test -f "$1" && mkdir -p "$2/$(dirname "$1")" && cp "$1" "$2/$1"' _ {} "$stage")
  cp "$repo_root/go.mod" "$proxy/$version.mod"
  printf '%s\n' "$version" >"$proxy/list"
  printf '{"Version":"%s","Time":"2026-01-01T00:00:00Z"}\n' "$version" >"$proxy/$version.info"
  (cd "$workdir/stage" && zip -qr "$proxy/$version.zip" "$module@$version")
  export GOPROXY="file://$workdir/proxy,$(go env GOPROXY)"
  export GONOSUMDB="$module"
else
  version="$published_version"
fi

consumer="$workdir/consumer"
mkdir -p "$consumer"
cd "$consumer"
go mod init example.invalid/goagents-consumer >/dev/null
go get "$module@$version"

cat >consumer_test.go <<'EOF'
package consumer

import (
	"context"
	"testing"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/eruca/goagents/runkit/sqlitestore"
	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/agentstep"
)

type scriptedLLM struct{}

func (scriptedLLM) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	return &ports.ChatResponse{Content: "consumer answer"}, nil
}

func TestAgentRunsThroughRoutedWorkflowStep(t *testing.T) {
	routed := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: []llmkit.Candidate{{
			Model: llmkit.ModelCapability{
				Alias: "local", Provider: "local", IsLocal: true,
				CapabilityLevel: llmkit.CapabilityBalanced, ContextWindowClass: llmkit.ContextMedium,
				PriceClass: llmkit.PriceFree, LatencyClass: llmkit.LatencyFastClass,
			},
			AccountAlias: "local-account",
		}},
		Providers: map[string]goagentadapter.ProviderClient{"local": scriptedLLM{}},
	})
	agent, err := agentcore.NewAgent(agentcore.WithLLM(routed))
	if err != nil {
		t.Fatal(err)
	}
	store := workflowkit.NewMemoryStore()
	step := agentstep.New("review", agent, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{Input: run.InputRef}
	})
	run, err := workflowkit.NewExecutor(store, []workflowkit.Step{step}).Run(context.Background(), workflowkit.WorkflowRun{ID: "wf-1", InputRef: "hello"})
	if err != nil || run.Status != workflowkit.StatusSucceeded {
		t.Fatalf("workflow = %+v, %v", run, err)
	}
}

func TestDurableStoresOpen(t *testing.T) {
	runs, err := sqlitestore.Open(t.TempDir() + "/runs.db")
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	if _, err := memorystore.New(memorykit.Limits{
		Version: "consumer", MaxKeyRunes: 128, MaxContentRunes: 4096,
		MaxMetadataRunes: 512, MaxSourcesPerMemory: 8, MaxListItems: 100,
	}); err != nil {
		t.Fatal(err)
	}
}
EOF
go mod tidy
go vet ./...
go test ./...
printf '\nexternal consumer verification passed (%s@%s)\n' "$module" "$version"
