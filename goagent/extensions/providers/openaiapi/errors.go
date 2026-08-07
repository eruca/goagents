package openaiapi

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfig           = errors.New("openai-compatible provider configuration is invalid")
	ErrMaxOutputTokensExceeded = errors.New("openai-compatible max output tokens exceeds limit")
	ErrRequestTooLarge         = errors.New("openai-compatible request exceeds byte limit")
	ErrResponseTooLarge        = errors.New("openai-compatible response exceeds byte limit")
	ErrInvalidJSON             = errors.New("openai-compatible response contains invalid JSON")
	ErrUsageMissing            = errors.New("openai-compatible response is missing usage")
	ErrRedirectBlocked         = errors.New("openai-compatible redirect is blocked")
	ErrInvalidResponse         = errors.New("openai-compatible response schema is invalid")
)

type providerError struct {
	class      string
	dispatched bool
	cause      error
}

func (e *providerError) Error() string {
	if e == nil || e.cause == nil {
		return "openai-compatible provider error"
	}
	return e.cause.Error()
}

func (e *providerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ProviderErrorClass 暴露白名单错误类别，不泄露响应数据。
func (e *providerError) ProviderErrorClass() string {
	if e == nil {
		return "unknown"
	}
	return e.class
}

// ProviderDispatched 表示出站请求是否可能已经执行。
func (e *providerError) ProviderDispatched() bool {
	return e != nil && e.dispatched
}

func newProviderError(class string, dispatched bool, cause error) error {
	return &providerError{class: class, dispatched: dispatched, cause: cause}
}

// ResponseError 只保留 Host 错误策略所需的 HTTP 状态码。
type ResponseError struct {
	StatusCode int
	// Body 仅为源码兼容保留，不再填充或输出。
	Body string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("openai-compatible request failed: status %d", e.StatusCode)
}

func (e *ResponseError) ProviderDispatched() bool {
	return true
}
