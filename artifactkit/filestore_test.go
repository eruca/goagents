package artifactkit_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/eruca/artifactkit"
	"github.com/eruca/artifactkit/storetest"
)

func TestFileStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) artifactkit.Store {
		store, err := artifactkit.NewFileStore(filepath.Join(t.TempDir(), "artifacts"))
		if err != nil {
			t.Fatalf("NewFileStore returned error: %v", err)
		}
		return store
	})
}

func TestFileStorePersistsArtifactsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "artifacts")

	store, err := artifactkit.NewFileStore(root)
	if err != nil {
		t.Fatalf("NewFileStore returned error: %v", err)
	}
	if err := store.Put(ctx, artifactkit.Artifact{
		Ref:         "artifact:wf-1:agent-output",
		Content:     []byte("durable draft"),
		ContentType: "text/plain",
		Metadata: map[string]any{
			"workflow_id": "wf-1",
		},
	}); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	reopened, err := artifactkit.NewFileStore(root)
	if err != nil {
		t.Fatalf("reopen NewFileStore returned error: %v", err)
	}
	got, err := reopened.Get(ctx, "artifact:wf-1:agent-output")
	if err != nil {
		t.Fatalf("Get after reopen returned error: %v", err)
	}
	if string(got.Content) != "durable draft" || got.ContentType != "text/plain" {
		t.Fatalf("artifact after reopen = %+v", got)
	}
	if got.Metadata["workflow_id"] != "wf-1" {
		t.Fatalf("metadata after reopen = %+v", got.Metadata)
	}
}
