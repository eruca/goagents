package main

import (
	"context"
	"fmt"
	"github.com/eruca/goagents/memorykit"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbeddingResponseIndicesAndProfile(t *testing.T) {
	raw := `{"data":[{"index":1,"embedding":[0,1,0]},{"index":0,"embedding":[1,0,0]}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, raw) }))
	defer server.Close()
	t.Setenv("MEMORY_EMBEDDING_URL", server.URL)
	t.Setenv("MEMORY_EMBEDDING_MODEL", "test")
	e, err := newHTTPEmbedder("test", 3)
	if err != nil {
		t.Fatal(err)
	}
	request := memorykit.EmbedRequest{ProfileID: "test", Texts: []string{"a", "b"}}
	v, err := e.Embed(context.Background(), request)
	if err != nil || v[0][0] != 1 || v[1][1] != 1 {
		t.Fatal("indices not respected", err)
	}
	raw = `{"data":[{"index":0,"embedding":[1,0,0]},{"index":0,"embedding":[0,1,0]}]}`
	if _, err = e.Embed(context.Background(), request); err == nil {
		t.Fatal("duplicate index accepted")
	}
	request.ProfileID = "other"
	if _, err = e.Embed(context.Background(), request); err == nil {
		t.Fatal("foreign embedding profile accepted")
	}
}
func TestEmbeddingRedirectDoesNotForwardCredentials(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv("MEMORY_EMBEDDING_URL", origin.URL)
	t.Setenv("MEMORY_EMBEDDING_MODEL", "test")
	t.Setenv("MEMORY_EMBEDDING_API_KEY", "fixture-key")
	e, err := newHTTPEmbedder("test", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Embed(context.Background(), memorykit.EmbedRequest{ProfileID: "test", Texts: []string{"a"}}); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked {
		t.Fatal("redirect destination contacted")
	}
}
