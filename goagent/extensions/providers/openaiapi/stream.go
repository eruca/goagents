package openaiapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"

	"github.com/eruca/goagents/goagent/ports"
)

var _ ports.StreamingLLMClient = (*Client)(nil)

// ChatStream sends a streaming chat completions request and calls onDelta with
// each piece of generated text as server-sent events arrive. It returns the
// complete response, including tool calls and usage, assembled from the
// stream. Limits.MaxResponseBytes bounds the whole stream. A server that
// ignores the stream flag and answers with JSON is handled as one delta.
func (c *Client) ChatStream(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	payload, err := c.requestPayload(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "application/json" {
		data, err := readResponseBody(resp, c.limits.MaxResponseBytes)
		if err != nil {
			return nil, err
		}
		result, err := parseChatResponse(data)
		if err != nil {
			return nil, err
		}
		if result.Content != "" {
			if err := onDelta(ports.ChatDelta{Content: result.Content}); err != nil {
				return nil, err
			}
		}
		return result, nil
	}

	var body io.Reader = resp.Body
	if c.limits.MaxResponseBytes > 0 {
		body = &boundedReader{reader: resp.Body, remaining: c.limits.MaxResponseBytes}
	}
	assembler := streamAssembler{}
	if err := readServerSentEvents(body, func(data []byte) error {
		return assembler.add(data, onDelta)
	}); err != nil {
		// A cancelled request surfaces as a read error; report the cancellation.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return assembler.response()
}

// ProviderStreamRequestSHA256 returns the digest of the exact JSON body
// ChatStream sends.
func (c *Client) ProviderStreamRequestSHA256(req ports.ChatRequest) ([sha256.Size]byte, error) {
	payload, err := c.requestPayload(req, true)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}

var errStreamDone = errors.New("stream done")

// maxStreamToolCalls bounds tool call indices so a malformed stream cannot
// force a large allocation. A gap in indices fails as an invalid tool call.
const maxStreamToolCalls = 128

// boundedReader fails with ErrResponseTooLarge once more than remaining bytes
// arrive, so an oversized stream is reported as such rather than as a
// truncated event.
type boundedReader struct {
	reader    io.Reader
	remaining int
}

func (r *boundedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.remaining -= n
	if r.remaining < 0 {
		return 0, newProviderError("response_too_large", true, ErrResponseTooLarge)
	}
	return n, err
}

// readServerSentEvents calls handle with the data of each event until the
// stream ends or sends [DONE].
func readServerSentEvents(body io.Reader, handle func([]byte) error) error {
	reader := bufio.NewReader(body)
	var data bytes.Buffer
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		event := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		defer data.Reset()
		if bytes.Equal(event, []byte("[DONE]")) {
			return errStreamDone
		}
		return handle(event)
	}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if dispatchErr := dispatch(); dispatchErr != nil {
					return streamEnd(dispatchErr)
				}
			case bytes.HasPrefix(line, []byte("data:")):
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
				data.WriteByte('\n')
			}
		}
		if errors.Is(err, io.EOF) {
			return streamEnd(dispatch())
		}
		var classified *providerError
		if errors.As(err, &classified) {
			return err
		}
		if err != nil {
			return newProviderError("invalid_response", true, ErrInvalidResponse)
		}
	}
}

func streamEnd(err error) error {
	if errors.Is(err, errStreamDone) {
		return nil
	}
	return err
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// streamAssembler rebuilds the complete response from stream chunks: content
// is concatenated and tool calls are merged by index, since a call's ID and
// name arrive once and its arguments arrive in pieces.
type streamAssembler struct {
	content   strings.Builder
	toolCalls []chatToolCall
	usage     *chatUsage
}

func (a *streamAssembler) add(data []byte, onDelta func(ports.ChatDelta) error) error {
	var chunk streamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return newProviderError("invalid_json", true, ErrInvalidJSON)
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	delta := chunk.Choices[0].Delta
	for _, call := range delta.ToolCalls {
		if call.Index < 0 || call.Index >= maxStreamToolCalls {
			return newProviderError("tool_call_invalid", true, ErrInvalidResponse)
		}
		for len(a.toolCalls) <= call.Index {
			a.toolCalls = append(a.toolCalls, chatToolCall{Type: "function"})
		}
		merged := &a.toolCalls[call.Index]
		if call.ID != "" {
			merged.ID = call.ID
		}
		if call.Function.Name != "" {
			merged.Function.Name = call.Function.Name
		}
		merged.Function.Arguments += call.Function.Arguments
	}
	if delta.Content == "" {
		return nil
	}
	a.content.WriteString(delta.Content)
	return onDelta(ports.ChatDelta{Content: delta.Content})
}

func (a *streamAssembler) response() (*ports.ChatResponse, error) {
	if a.usage == nil {
		return nil, newProviderError("usage_missing", true, ErrUsageMissing)
	}
	return chatResponse(a.content.String(), a.toolCalls, *a.usage)
}
