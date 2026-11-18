package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/eruca/goagents/artifactkit"
)

// artifactPutRecorder records the refs written through the Server's artifact
// store, since input refs carry a random suffix a test cannot predict.
type artifactPutRecorder struct {
	artifactkit.Store
	mu   sync.Mutex
	refs []string
}

func (r *artifactPutRecorder) Put(ctx context.Context, artifact artifactkit.Artifact) error {
	r.mu.Lock()
	r.refs = append(r.refs, artifact.Ref)
	r.mu.Unlock()
	return r.Store.Put(ctx, artifact)
}

func (r *artifactPutRecorder) wrote(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ref := range r.refs {
		if strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

func recordArtifactPuts(server *Server) *artifactPutRecorder {
	recorder := &artifactPutRecorder{Store: server.artifacts}
	server.artifacts = recorder
	return recorder
}

func TestWorkflowInputRefsAreUniquePerRequest(t *testing.T) {
	first, second := newWorkflowInputRef("wf-1"), newWorkflowInputRef("wf-1")
	if first == second {
		t.Fatalf("two input refs for one workflow are equal: %q", first)
	}
	for _, ref := range []string{first, second} {
		if !isWorkflowInputRef("wf-1", ref) {
			t.Fatalf("isWorkflowInputRef(wf-1, %q) = false", ref)
		}
	}
	for _, ref := range []string{
		"artifact:wf-1:input", "artifact:wf-1:input:", "artifact:wf-2:input:abc",
		"artifact:wf-1:input:a:b", "artifact:wf-1:input:a b", "artifact:wf-1:output:abc",
	} {
		if isWorkflowInputRef("wf-1", ref) {
			t.Fatalf("isWorkflowInputRef(wf-1, %q) = true", ref)
		}
	}
}

func stringMetadata(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}
