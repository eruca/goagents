#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
  printf 'usage: %s <hostkit|workflowkit|runkit> <git-ref> <version|pseudo>\n' "$0" >&2
  exit 2
fi

module_name="$1"
ref="$2"
expected_version="$3"
module_path="github.com/eruca/goagents/$module_name"

case "$module_name" in
  hostkit|workflowkit|runkit)
    ;;
  *)
    printf 'unsupported release module: %s\n' "$module_name" >&2
    exit 2
    ;;
esac

if [[ -z "$ref" || -z "$expected_version" ]]; then
  printf 'git ref and expected version are required\n' >&2
  exit 2
fi
if [[ "$expected_version" == "pseudo" && ! "$ref" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'pseudo-version verification requires a 40-character commit SHA\n' >&2
  exit 2
fi

script_dir="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -P "$script_dir/.." && pwd -P)"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/goagents-release-consumer.XXXXXX")"
cleanup() {
  local command_status=$?
  local cleanup_status=0

  if [[ -d "$workdir" ]]; then
    # Go module cache directories are intentionally read-only after extraction.
    if ! chmod -R u+w "$workdir"; then
      cleanup_status=1
    fi
    if ! rm -rf "$workdir"; then
      cleanup_status=1
    fi
  fi
  if (( cleanup_status != 0 )); then
    printf 'release consumer cleanup failed\n' >&2
    return 1
  fi
  return "$command_status"
}
trap cleanup EXIT

# Resolve TMPDIR symlinks before enforcing the repository boundary.
workdir="$(cd -P "$workdir" && pwd -P)"
if [[ "$workdir" == "$repo_root" || "$workdir" == "$repo_root/"* ]]; then
  printf 'release consumer workdir must be outside the repository\n' >&2
  exit 1
fi

cd "$workdir"
export GOMODCACHE="$workdir/modcache"
export GOCACHE="$workdir/buildcache"
GOWORK=off go mod init "example.invalid/goagents-$module_name-consumer" >/dev/null

case "$module_name" in
  hostkit)
    cat > consumer_test.go <<'EOF'
package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/eruca/goagents/hostkit"
)

type service struct {
	done chan error
}

func (s *service) Start(context.Context) error     { return nil }
func (s *service) Done() <-chan error               { return s.done }
func (s *service) Drain(context.Context) error      { return nil }
func (s *service) ForceStop(context.Context) error  { return nil }
func (s *service) Close(context.Context) error      { return nil }

func TestPublicLifecycleContract(t *testing.T) {
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}
	result := hostkit.Run(context.Background(), &service{done: make(chan error, 1)}, interrupts, hostkit.Options{
		DrainTimeout:   time.Second,
		CleanupTimeout: time.Second,
	})
	if result.ExitCode() != 0 || result.Err() != nil {
		t.Fatalf("result = code %d, err %v", result.ExitCode(), result.Err())
	}
}
EOF
    ;;
  workflowkit)
    cat > consumer_test.go <<'EOF'
package consumer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/sqlitestore"
)

func TestTransactionalUpdate(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	run := workflowkit.WorkflowRun{
		ID:        "wf-consumer",
		Status:    workflowkit.StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		current.Status = workflowkit.StatusFailed
		current.Error = "consumer_failure"
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != workflowkit.StatusFailed || updated.Error != "consumer_failure" {
		t.Fatalf("updated = %#v", updated)
	}
}
EOF
    ;;
  runkit)
    cat > consumer_test.go <<'EOF'
package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
)

func TestPendingCheckpointFailureCapability(t *testing.T) {
	ctx := context.Background()
	store := runkit.NewMemoryCheckpointStore()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-consumer",
		RunID:          "run-consumer",
		TenantID:       "tenant-consumer",
		DefinitionHash: "definition-consumer",
		Ciphertext:     []byte("opaque"),
		ExpiresAt:      time.Now().UTC().Add(time.Hour),
	}
	if err := store.CreateCheckpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	var failureStore runkit.PendingCheckpointFailureStore = store
	if err := failureStore.FailPendingCheckpoint(ctx, runkit.PendingCheckpointFailure{
		CheckpointID:   checkpoint.ID,
		RunID:          checkpoint.RunID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		FailureCode:    "host_shutdown_timeout",
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runkit.CheckpointFailed || stored.FailureCode != "host_shutdown_timeout" {
		t.Fatalf("stored = %#v", stored)
	}
}
EOF
    ;;
esac

GOWORK=off go get "$module_path@$ref"
if grep -Eq '^[[:space:]]*replace([[:space:]]|$)' go.mod; then
  printf 'clean consumer unexpectedly contains replace\n' >&2
  exit 1
fi
GOWORK=off go mod tidy
GOWORK=off go test -count=1 ./...
GOWORK=off go list -m all >/dev/null

replacement_graph="$(
  GOWORK=off go list -m \
    -f '{{if .Replace}}{{.Path}}=>{{.Replace.Path}}{{end}}' all |
    sed '/^$/d'
)"
if [[ -n "$replacement_graph" ]]; then
  printf 'clean consumer module graph contains replace\n' >&2
  exit 1
fi

internal_graph="$(
  GOWORK=off go list -m -f '{{.Path}}|{{.Version}}' all |
    awk -F '|' -v prefix='github.com/eruca/goagents/' -v target="$module_path" \
      'index($1, prefix) == 1 && $1 != target' |
    LC_ALL=C sort
)"
expected_internal_graph=""
if [[ "$module_name" == "runkit" ]]; then
  expected_internal_graph='github.com/eruca/goagents/goagent|v0.1.0'
fi
if [[ "$internal_graph" != "$expected_internal_graph" ]]; then
  printf 'unexpected internal module graph for %s\n' "$module_path" >&2
  exit 1
fi

resolved="$(GOWORK=off go list -m -f '{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}' "$module_path")"
IFS='|' read -r resolved_path resolved_version replacement <<<"$resolved"
if [[ "$resolved_path" != "$module_path" || -n "$replacement" ]]; then
  printf 'unexpected module resolution for %s\n' "$module_path" >&2
  exit 1
fi

if [[ "$expected_version" == "pseudo" ]]; then
  if [[ ! "$resolved_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9]{14}|-0\.[0-9]{14})-[0-9a-f]{12}$ ]]; then
    printf 'resolved version is not a pseudo-version for %s\n' "$module_path" >&2
    exit 1
  fi
  if [[ "${resolved_version##*-}" != "${ref:0:12}" ]]; then
    printf 'resolved pseudo-version revision does not match candidate SHA for %s\n' \
      "$module_path" >&2
    exit 1
  fi
elif [[ "$resolved_version" != "$expected_version" ]]; then
  printf 'resolved version mismatch for %s: got %s, want %s\n' \
    "$module_path" "$resolved_version" "$expected_version" >&2
  exit 1
fi

printf 'release consumer: %s@%s PASS\n' "$module_path" "$resolved_version"
