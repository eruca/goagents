package archive

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/eruca/goagents/contextkit"
)

func scoped(name string) context.Context {
	return contextkit.WithArchiveScope(context.Background(), name)
}

func TestOffloadIsIdempotentAndScoped(t *testing.T) {
	archive := New(Config{})
	msg := contextkit.Message{Role: contextkit.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "the lease expires after 30 seconds"}
	first, err := archive.Offload(scoped("run-a"), msg)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := archive.Offload(scoped("run-a"), msg)
	other, _ := archive.Offload(scoped("run-b"), msg)
	if first != again || first == other {
		t.Fatalf("refs = %s %s %s", first, again, other)
	}
	got, err := archive.Read(scoped("run-a"), first)
	if err != nil || got.Content != msg.Content {
		t.Fatalf("read = %+v, %v", got, err)
	}
	if _, err := archive.Read(scoped("run-b"), first); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read across scopes = %v", err)
	}
	if _, err := archive.Read(context.Background(), first); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read without scope = %v", err)
	}
	if _, err := archive.Offload(context.Background(), msg); err == nil {
		t.Fatal("offload without a scope was accepted")
	}
	got.Content = "changed"
	if again, _ := archive.Read(scoped("run-a"), first); again.Content != msg.Content {
		t.Fatal("read returned shared state")
	}
}

func TestSearchRanksRareTermsAndStaysInScope(t *testing.T) {
	archive := New(Config{})
	ctx := scoped("run")
	for i, content := range []string{
		"the build passed on main",
		"the lease expires after 30 seconds",
		"the build failed on the feature branch",
		"数据库端口是 55432",
	} {
		if _, err := archive.Offload(ctx, contextkit.Message{Role: contextkit.RoleTool, ToolCallID: fmt.Sprint(i), Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = archive.Offload(scoped("other"), contextkit.Message{Role: contextkit.RoleTool, Content: "the lease is renewed"})

	hits, err := archive.Search(ctx, "when does the lease expire", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Message.Content != "the lease expires after 30 seconds" {
		t.Fatalf("hits = %+v", hits)
	}
	for _, hit := range hits {
		if hit.Message.Content == "the lease is renewed" {
			t.Fatal("search crossed scopes")
		}
	}
	if hits, _ := archive.Search(ctx, "数据库用哪个端口", 3); len(hits) != 1 || hits[0].Message.Content != "数据库端口是 55432" {
		t.Fatalf("chinese hits = %+v", hits)
	}
	if hits, _ := archive.Search(ctx, "kubernetes", 3); len(hits) != 0 {
		t.Fatalf("unrelated hits = %+v", hits)
	}
}

func TestArchiveIsBounded(t *testing.T) {
	archive := New(Config{MaxMessagesPerScope: 2, MaxScopes: 2})
	ctx := scoped("a")
	var refs []string
	for i := 0; i < 3; i++ {
		ref, _ := archive.Offload(ctx, contextkit.Message{Role: contextkit.RoleUser, Content: fmt.Sprint("message ", i)})
		refs = append(refs, ref)
	}
	if _, err := archive.Read(ctx, refs[0]); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatal("oldest message was not evicted")
	}
	if _, err := archive.Read(ctx, refs[2]); err != nil {
		t.Fatal(err)
	}
	_, _ = archive.Offload(scoped("b"), contextkit.Message{Content: "b"})
	_, _ = archive.Offload(scoped("c"), contextkit.Message{Content: "c"})
	if _, err := archive.Read(ctx, refs[2]); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatal("least recently used scope was not evicted")
	}
	archive.DeleteScope("c")
	if hits, _ := archive.Search(scoped("c"), "c", 1); len(hits) != 0 {
		t.Fatal("deleted scope still searchable")
	}
}
