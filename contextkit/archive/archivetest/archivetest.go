// Package archivetest checks that a contextkit.Archive behaves like
// archive.Memory: scoped, idempotent references, bounded scopes, and the same
// search ranking.
package archivetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/archive"
)

// Factory returns an empty-enough archive that keeps at most
// maxMessagesPerScope messages per scope. Scopes the suite uses are unique to
// each call, so a shared database needs no cleanup between tests.
type Factory func(t *testing.T, maxMessagesPerScope int) contextkit.Archive

// Run runs the conformance suite against archives from newArchive.
func Run(t *testing.T, newArchive Factory) {
	t.Run("offload is idempotent and scoped", func(t *testing.T) { offloadIsIdempotentAndScoped(t, newArchive(t, 100)) })
	t.Run("search ranks like archive.Memory", func(t *testing.T) { searchRanksLikeMemory(t, newArchive(t, 100)) })
	t.Run("scopes are bounded", func(t *testing.T) { scopesAreBounded(t, newArchive(t, 2)) })
}

func uniqueScope(t *testing.T, name string) context.Context {
	t.Helper()
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	return contextkit.WithArchiveScope(context.Background(), name+"-"+hex.EncodeToString(suffix[:]))
}

func offloadIsIdempotentAndScoped(t *testing.T, store contextkit.Archive) {
	a, b := uniqueScope(t, "a"), uniqueScope(t, "b")
	msg := contextkit.Message{
		Role: contextkit.RoleAssistant, Content: "calling read", Reasoning: "need the file",
		ToolCalls: []contextkit.ToolCall{{ID: "c1", Name: "read", Arguments: `{"path":"a.go"}`}},
		Metadata:  map[string]any{"step": "one"},
	}
	first, err := store.Offload(a, msg)
	if err != nil {
		t.Fatal(err)
	}
	if want := archive.Ref(contextkit.ArchiveScope(a), msg); first != want {
		t.Fatalf("ref = %s, want archive.Ref %s", first, want)
	}
	again, _ := store.Offload(a, msg)
	other, _ := store.Offload(b, msg)
	if first != again || first == other {
		t.Fatalf("refs = %s %s %s", first, again, other)
	}
	got, err := store.Read(a, first)
	if err != nil || got.Content != msg.Content || got.Reasoning != msg.Reasoning || len(got.ToolCalls) != 1 ||
		got.ToolCalls[0] != msg.ToolCalls[0] || got.Metadata["step"] != "one" {
		t.Fatalf("read = %+v, %v", got, err)
	}
	if _, err := store.Read(b, first); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read across scopes = %v", err)
	}
	if _, err := store.Read(context.Background(), first); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read without scope = %v", err)
	}
	if _, err := store.Offload(context.Background(), msg); err == nil {
		t.Fatal("offload without a scope was accepted")
	}
}

func searchRanksLikeMemory(t *testing.T, store contextkit.Archive) {
	ctx, other := uniqueScope(t, "search"), uniqueScope(t, "other")
	reference := archive.New(archive.Config{})
	refCtx := contextkit.WithArchiveScope(context.Background(), contextkit.ArchiveScope(ctx))
	contents := []string{
		"the build passed on main",
		"the lease expires after 30 seconds",
		"the build failed on the feature branch",
		"数据库端口是 55432",
		"the lease is held by replica B",
		"the build passed on main",
	}
	for i, content := range contents {
		msg := contextkit.Message{Role: contextkit.RoleTool, ToolCallID: fmt.Sprint(i % 5), ToolName: "shell", Content: content}
		if _, err := store.Offload(ctx, msg); err != nil {
			t.Fatal(err)
		}
		_, _ = reference.Offload(refCtx, msg)
	}
	if _, err := store.Offload(other, contextkit.Message{Role: contextkit.RoleTool, Content: "the lease is renewed"}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"when does the lease expire", "build main", "数据库用哪个端口", "shell", "kubernetes", ""} {
		for _, limit := range []int{0, 2, 10} {
			got, err := store.Search(ctx, query, limit)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := reference.Search(refCtx, query, limit)
			if len(got) != len(want) {
				t.Fatalf("%q limit %d: %d hits, archive.Memory %d", query, limit, len(got), len(want))
			}
			for i := range got {
				if got[i].Ref != want[i].Ref || got[i].Message.Content != want[i].Message.Content || math.Abs(got[i].Score-want[i].Score) > 1e-9 {
					t.Fatalf("%q limit %d hit %d = %s %.6f, archive.Memory %s %.6f", query, limit, i, got[i].Ref, got[i].Score, want[i].Ref, want[i].Score)
				}
				if got[i].Message.Content == "the lease is renewed" {
					t.Fatal("search crossed scopes")
				}
			}
		}
	}
	if hits, _ := store.Search(context.Background(), "lease", 3); len(hits) != 0 {
		t.Fatalf("search without scope = %+v", hits)
	}
}

func scopesAreBounded(t *testing.T, store contextkit.Archive) {
	ctx := uniqueScope(t, "bounded")
	var refs []string
	for i := 0; i < 3; i++ {
		ref, err := store.Offload(ctx, contextkit.Message{Role: contextkit.RoleUser, Content: fmt.Sprint("message ", i)})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if _, err := store.Read(ctx, refs[0]); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatal("oldest message was not evicted")
	}
	for _, ref := range refs[1:] {
		if _, err := store.Read(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if hits, _ := store.Search(ctx, "message", 10); len(hits) != 2 {
		t.Fatalf("hits after eviction = %+v", hits)
	}
}
