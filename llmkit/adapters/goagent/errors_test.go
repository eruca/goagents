package goagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/llmkit/llmkit"
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
		{name: "canceled", err: context.Canceled, want: llmkit.ErrorClassCanceled},
		{name: "request too large", err: classifiedProviderError{class: "request_too_large"}, want: llmkit.ErrorClassRequestTooLarge},
		{name: "response too large", err: classifiedProviderError{class: "response_too_large"}, want: llmkit.ErrorClassResponseTooLarge},
		{name: "invalid JSON", err: classifiedProviderError{class: "invalid_json"}, want: llmkit.ErrorClassInvalidJSON},
		{name: "usage missing", err: classifiedProviderError{class: "usage_missing"}, want: llmkit.ErrorClassUsageMissing},
		{name: "redirect blocked", err: classifiedProviderError{class: "redirect_blocked"}, want: llmkit.ErrorClassRedirectBlocked},
		{name: "invalid response", err: classifiedProviderError{class: "invalid_response"}, want: llmkit.ErrorClassInvalidResponse},
		{name: "budget exceeded", err: classifiedProviderError{class: "budget_exceeded"}, want: llmkit.ErrorClassBudgetExceeded},
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

type classifiedProviderError struct {
	class string
}

func (e classifiedProviderError) Error() string {
	return "classified provider error"
}

func (e classifiedProviderError) ProviderErrorClass() string {
	return e.class
}
