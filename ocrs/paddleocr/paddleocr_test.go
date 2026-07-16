package paddleocr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/ocrs/retrypolicy"
)

func TestHandleSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Fatalf("unexpected auth header: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request payload failed: %v", err)
		}
		file, _ := payload["file"].(string)
		if file == "" {
			t.Fatalf("expected encoded file in payload")
		}

		resp := OCRResponse{
			LogID:     "log-1",
			ErrorCode: 0,
			ErrorMsg:  "Success",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient("paddleocr", srv.URL, "test-token", 10*time.Minute)
	got, err := c.Handle(context.Background(), []byte("pdf-bytes"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Provider != "paddleocr" {
		t.Fatalf("unexpected provider: %s", got.Provider)
	}

	var resp OCRResponse
	if err := json.Unmarshal(got.Raw, &resp); err != nil {
		t.Fatalf("unmarshal raw failed: %v", err)
	}
	if resp.LogID != "log-1" {
		t.Fatalf("unexpected log id: %s", resp.LogID)
	}
}

func TestHandleHTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient("paddleocr", srv.URL, "test-token", 10*time.Minute)
	_, err := c.Handle(context.Background(), []byte("pdf-bytes"))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleRetryThenSuccess(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		resp := OCRResponse{LogID: "ok", ErrorCode: 0, ErrorMsg: "Success"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient("paddleocr", srv.URL, "test-token", 10*time.Minute,
		retrypolicy.WithMaxTry(3),
		retrypolicy.WithInitialBackoff(time.Millisecond),
		retrypolicy.WithMaxBackoff(time.Millisecond),
		retrypolicy.WithMultiplier(1),
	)

	got, err := c.Handle(context.Background(), []byte("pdf-bytes"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Provider != "paddleocr" {
		t.Fatalf("unexpected provider: %s", got.Provider)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestHandleSwitchTokenOnQuotaStatus(t *testing.T) {
	t.Parallel()

	var firstTokenSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "token token-a" {
			firstTokenSeen = true
			http.Error(w, "quota", http.StatusTooManyRequests)
			return
		}
		if auth == "token token-b" {
			resp := OCRResponse{LogID: "ok", ErrorCode: 0, ErrorMsg: "Success"}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "unexpected token", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClientWithTokens(
		"paddleocr",
		srv.URL,
		[]string{"token-a", "token-b"},
		10*time.Minute,
		retrypolicy.WithMaxTry(3),
		retrypolicy.WithInitialBackoff(time.Millisecond),
		retrypolicy.WithMaxBackoff(time.Millisecond),
		retrypolicy.WithMultiplier(1),
	)

	got, err := c.Handle(context.Background(), []byte("pdf-bytes"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !firstTokenSeen {
		t.Fatalf("expected token-a to be attempted first")
	}
	if got.Provider != "paddleocr" {
		t.Fatalf("unexpected provider: %s", got.Provider)
	}
}

func TestTokenPoolExhausted(t *testing.T) {
	t.Parallel()

	p := newTokenPool([]string{"a"}, 1)
	_, err := p.acquire()
	if err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	_, err = p.acquire()
	if err == nil {
		t.Fatalf("expected quota exhausted error")
	}
	if err != ErrAllTokensExhaustedToday {
		t.Fatalf("unexpected error: %v", err)
	}
}
