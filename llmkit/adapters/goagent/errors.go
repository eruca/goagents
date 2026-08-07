package goagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/llmkit/llmkit"
)

// ErrorStage 无需解析错误文本即可标识 runtime 调用的失败阶段。
type ErrorStage string

const (
	ErrorStageRoute    ErrorStage = "route"
	ErrorStageConfig   ErrorStage = "config"
	ErrorStagePreCall  ErrorStage = "pre_call"
	ErrorStageProvider ErrorStage = "provider"
	ErrorStagePostCall ErrorStage = "post_call"
)

// RuntimeError 暴露稳定的失败元数据，并保留供 errors.Is/errors.As 使用的类型化原因；
// Error 有意不输出底层消息。
type RuntimeError struct {
	Stage              ErrorStage
	Class              llmkit.ErrorClass
	ProviderDispatched bool
	cause              error
}

func (e *RuntimeError) Error() string {
	if e == nil {
		return "goagent runtime error"
	}
	return fmt.Sprintf("goagent runtime error: stage=%s class=%s", e.Stage, e.Class)
}

func (e *RuntimeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newRuntimeError(stage ErrorStage, class llmkit.ErrorClass, dispatched bool, cause error) *RuntimeError {
	if class == "" {
		class = llmkit.ErrorClassUnknown
	}
	return &RuntimeError{
		Stage:              stage,
		Class:              class,
		ProviderDispatched: dispatched,
		cause:              cause,
	}
}

// DefaultErrorClassifier maps typed provider and network errors to stable
// audit classes without depending on provider-specific error messages.
func DefaultErrorClassifier(err error) llmkit.ErrorClass {
	var runtimeErr *RuntimeError
	if errors.As(err, &runtimeErr) {
		return runtimeErr.Class
	}
	if errors.Is(err, context.Canceled) {
		return llmkit.ErrorClassCanceled
	}
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

	var classifiedErr interface{ ProviderErrorClass() string }
	if errors.As(err, &classifiedErr) {
		class := llmkit.ErrorClass(classifiedErr.ProviderErrorClass())
		switch class {
		case llmkit.ErrorClassRequestTooLarge,
			llmkit.ErrorClassResponseTooLarge,
			llmkit.ErrorClassInvalidJSON,
			llmkit.ErrorClassUsageMissing,
			llmkit.ErrorClassRedirectBlocked,
			llmkit.ErrorClassInvalidResponse,
			llmkit.ErrorClassToolCallInvalid,
			llmkit.ErrorClassConfiguration,
			llmkit.ErrorClassBudgetExceeded:
			return class
		}
	}

	return llmkit.ErrorClassUnknown
}
