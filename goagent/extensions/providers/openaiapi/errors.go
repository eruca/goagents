package openaiapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/eruca/goagents/goagent/ports"
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

// ResponseError reports a non-2xx HTTP status. It keeps only the status code
// and whether the body said the request does not fit the model's context
// window, so provider response bodies never reach host errors or logs.
type ResponseError struct {
	StatusCode int
	// ContextTooLong is set when the error body names a context window
	// overflow; the error then matches ports.ErrContextTooLong.
	ContextTooLong bool
}

// Unwrap lets errors.Is(err, ports.ErrContextTooLong) match an overflow.
func (e *ResponseError) Unwrap() error {
	if e != nil && e.ContextTooLong {
		return ports.ErrContextTooLong
	}
	return nil
}

// maxErrorBodyBytes bounds how much of an error body is read to classify it.
const maxErrorBodyBytes = 16 << 10

// contextOverflowMarkers are the phrases OpenAI-compatible servers use when a
// prompt does not fit the context window, lowercased.
var contextOverflowMarkers = [][]byte{
	[]byte("context_length_exceeded"),            // OpenAI error code
	[]byte("maximum context length"),             // OpenAI, vLLM, DeepSeek
	[]byte("longer than the model's context"),    // vLLM, SGLang
	[]byte("exceed_context_size_error"),          // llama.cpp error type
	[]byte("exceeds the available context size"), // llama.cpp
	[]byte("context window"),                     // "exceeds the context window"
	[]byte("prompt is too long"),                 // Anthropic-compatible gateways
	[]byte("input length exceeds the context length"),
	[]byte("too many tokens"),
}

// contextTooLong reads the start of an error body and reports whether it
// describes a context window overflow. The body itself is discarded.
func contextTooLong(resp *http.Response) bool {
	// 429 bodies such as "too many tokens per minute" are rate limits.
	if resp.StatusCode < 400 || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	body = bytes.ToLower(body)
	for _, marker := range contextOverflowMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("openai-compatible request failed: status %d", e.StatusCode)
}

func (e *ResponseError) ProviderDispatched() bool {
	return true
}
