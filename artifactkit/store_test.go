package artifactkit

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryStorePutGetAndCopySemantics(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	artifact := Artifact{
		Ref:         "artifact:wf-1:input",
		Content:     []byte("draft content"),
		ContentType: "text/plain",
		Metadata: map[string]any{
			"source": "test",
		},
	}
	if err := store.Put(ctx, artifact); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	artifact.Content[0] = 'X'
	artifact.Metadata["source"] = "mutated"

	got, err := store.Get(ctx, "artifact:wf-1:input")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if string(got.Content) != "draft content" {
		t.Fatalf("content = %q, want original draft content", string(got.Content))
	}
	if got.ContentType != "text/plain" {
		t.Fatalf("content type = %q, want text/plain", got.ContentType)
	}
	if got.Metadata["source"] != "test" {
		t.Fatalf("metadata source = %v, want test", got.Metadata["source"])
	}

	got.Content[0] = 'Y'
	got.Metadata["source"] = "changed-after-get"
	again, err := store.Get(ctx, "artifact:wf-1:input")
	if err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if string(again.Content) != "draft content" || again.Metadata["source"] != "test" {
		t.Fatalf("store leaked mutable artifact state: %+v", again)
	}
}

func TestMemoryStoreRequiresArtifactRef(t *testing.T) {
	store := NewMemoryStore()

	err := store.Put(context.Background(), Artifact{Content: []byte("missing ref")})
	if err == nil {
		t.Fatal("Put error = nil, want missing ref error")
	}
}

func TestMemoryStoreGetMissingReturnsNotFound(t *testing.T) {
	store := NewMemoryStore()

	_, err := store.Get(context.Background(), "artifact:missing")
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("Get missing error = %v, want ErrArtifactNotFound", err)
	}
}

func TestMemoryStoreHonorsContextCancellation(t *testing.T) {
	store := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Put(ctx, Artifact{Ref: "artifact:cancelled", Content: []byte("x")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put cancelled error = %v, want context.Canceled", err)
	}
	if _, err := store.Get(ctx, "artifact:cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get cancelled error = %v, want context.Canceled", err)
	}
}
