package goagent

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/eruca/goagent/extensions/providers/openaiapi"
	"github.com/eruca/llmkit/llmkit"
)

// DefaultErrorClassifier maps typed provider and network errors to stable
// audit classes without depending on provider-specific error messages.
func DefaultErrorClassifier(err error) llmkit.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return llmkit.ErrorClassTimeout
	}

	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return llmkit.ErrorClassTimeout
	}

	var responseErr *openaiapi.ResponseError
	if errors.As(err, &responseErr) {
		switch responseErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return llmkit.ErrorClassAuth
		case http.StatusRequestTimeout:
			return llmkit.ErrorClassTimeout
		case http.StatusTooManyRequests:
			return llmkit.ErrorClassRateLimited
		}
		if responseErr.StatusCode >= http.StatusInternalServerError && responseErr.StatusCode <= 599 {
			return llmkit.ErrorClassTransient
		}
	}

	return llmkit.ErrorClassUnknown
}
