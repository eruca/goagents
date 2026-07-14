package goagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/eruca/goagent/extensions/providers/openaiapi"
	"github.com/eruca/llmkit/llmkit"
)

func TestDefaultErrorClassifier(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want llmkit.ErrorClass
	}{
		{name: "deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: llmkit.ErrorClassTimeout},
		{name: "network timeout", err: fmt.Errorf("wrapped: %w", timeoutProviderError{}), want: llmkit.ErrorClassTimeout},
		{name: "unauthorized", err: &openaiapi.ResponseError{StatusCode: http.StatusUnauthorized}, want: llmkit.ErrorClassAuth},
		{name: "forbidden", err: &openaiapi.ResponseError{StatusCode: http.StatusForbidden}, want: llmkit.ErrorClassAuth},
		{name: "request timeout", err: &openaiapi.ResponseError{StatusCode: http.StatusRequestTimeout}, want: llmkit.ErrorClassTimeout},
		{name: "rate limited", err: &openaiapi.ResponseError{StatusCode: http.StatusTooManyRequests}, want: llmkit.ErrorClassRateLimited},
		{name: "internal server error", err: &openaiapi.ResponseError{StatusCode: http.StatusInternalServerError}, want: llmkit.ErrorClassTransient},
		{name: "service unavailable", err: &openaiapi.ResponseError{StatusCode: http.StatusServiceUnavailable}, want: llmkit.ErrorClassTransient},
		{name: "bad request", err: &openaiapi.ResponseError{StatusCode: http.StatusBadRequest}, want: llmkit.ErrorClassUnknown},
		{name: "canceled", err: context.Canceled, want: llmkit.ErrorClassUnknown},
		{name: "unknown", err: errors.New("provider failed"), want: llmkit.ErrorClassUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DefaultErrorClassifier(test.err); got != test.want {
				t.Fatalf("DefaultErrorClassifier() = %q, want %q", got, test.want)
			}
		})
	}
}

type timeoutProviderError struct{}

func (timeoutProviderError) Error() string   { return "provider timeout" }
func (timeoutProviderError) Timeout() bool   { return true }
func (timeoutProviderError) Temporary() bool { return true }
