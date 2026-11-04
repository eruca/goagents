package artifactkit_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/artifactkit/storetest"
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

func TestFileStoreHandlesConcurrentWritesAndLongRefs(t *testing.T) {
	store, err := artifactkit.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for writer := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 8 {
				if err := store.Put(ctx, artifactkit.Artifact{Ref: "artifact:shared", Content: []byte(fmt.Sprintf("%d-%d", writer, round))}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Put error: %v", err)
	}
	if _, err := store.Get(ctx, "artifact:shared"); err != nil {
		t.Fatalf("Get after concurrent writes: %v", err)
	}

	long := "artifact:" + strings.Repeat("w", 300) + ":input"
	if err := store.Put(ctx, artifactkit.Artifact{Ref: long, Content: []byte("long")}); err != nil {
		t.Fatalf("Put long ref: %v", err)
	}
	got, err := store.Get(ctx, long)
	if err != nil || string(got.Content) != "long" {
		t.Fatalf("Get long ref = %q, %v", got.Content, err)
	}
}
