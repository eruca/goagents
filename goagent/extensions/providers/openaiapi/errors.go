package openaiapi

import "fmt"

// ResponseError preserves the HTTP status needed by host-side error policy.
type ResponseError struct {
	StatusCode int
	Body       string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("openai-compatible request failed: status %d: %s", e.StatusCode, e.Body)
}
