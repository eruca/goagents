# OCRS

`ocrs` is a small Go module for running OCR providers behind reusable handler
contracts. It was extracted from an internal RAG OCR package so other Go
libraries can use the PaddleOCR client, structured result parsing, chunked PDF
processing, retry middleware, and provider dispatching without depending on the
RAG codebase.

## Install

```bash
go get github.com/eruca/goagents/ocrs
```

For local development before publishing:

```bash
go mod edit -replace github.com/eruca/goagents/ocrs=/Users/nick/VibeCoding/goagents/ocrs
go get github.com/eruca/goagents/ocrs
```

## Packages

- `ocrs`: generic `Handler`, `Middleware`, `OCR`, provider contracts, and common result types.
- `paddleocr`: PaddleOCR HTTP API client, response DTOs, structured chunk parsing, and chunk result merging.
- `chunking`: PDF chunk orchestration and a `pdfcpu` splitter.
- `scheduler`: worker dispatcher for routing calls across OCR providers.
- `retrypolicy`: generic retry middleware with bounded exponential backoff.

## Use With goagent

`ocrs` is an OCR capability module. `goagent` is an agent runtime framework.
Keep the dependency direction at the application boundary:

```text
your application
  imports github.com/eruca/goagents/goagent
  imports github.com/eruca/goagents/ocrs
  wraps ocrs as one or more goagent tools

github.com/eruca/goagents/goagent does not import github.com/eruca/goagents/ocrs
github.com/eruca/goagents/ocrs does not import github.com/eruca/goagents/goagent
```

This keeps `goagent` generic and keeps `ocrs` usable by non-agent programs.
The application decides which OCR actions are exposed to the model, how files
are authorized, and how much OCR output is returned to the LLM context.

Recommended tool shape:

```go
type OCRDocumentTool struct {
	Handler ocrs.Handler[[]byte, ocrs.OCRResult]
}

func (t OCRDocumentTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "ocr_document",
		Description: "Extracts structured text from an approved PDF or image document.",
		Permission:  policy.PermissionRead,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"document_id":{"type":"string"}
				},
				"required":["document_id"]
			}`),
		},
	}
}

func (t OCRDocumentTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	var req struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	// Resolve document_id through your application storage or request metadata.
	// Avoid letting the model pass arbitrary filesystem paths.
	data, err := loadDocumentBytes(ctx, req.DocumentID, env)
	if err != nil {
		return nil, err
	}

	result, err := t.Handler.Handle(ctx, data)
	if err != nil {
		return nil, err
	}
	title, chunks, err := paddleocr.ParseStructuredChunks(result.Raw)
	if err != nil {
		return nil, err
	}

	preview := firstChunkTexts(chunks, 3)
	return &tools.Result{
		ForLLM: fmt.Sprintf("OCR title: %s\nChunks: %d\nPreview:\n%s", title, len(chunks), preview),
		ForUser: string(result.Raw),
	}, nil
}
```

Register that tool with a normal `goagent` registry:

```go
client := paddleocr.NewClient("paddleocr", apiURL, token, 10*time.Minute)
handler := chunking.NewCutAwareHandler(chunking.CutAwareConfig{
	PagesPerChunk:    20,
	ChunkConcurrency: 2,
	Splitter:         chunking.PDFCPUSplitter{},
	Inner:            client,
	Merge:            paddleocr.MergeChunkResponses,
})

registry := tools.NewRegistry()
registry.Register(OCRDocumentTool{Handler: handler})

agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithToolRegistry(registry),
)
```

Use `document_id` or another host-owned identifier instead of raw paths when
possible. OCR output can be much larger than an LLM context window, so return a
bounded preview in `ForLLM` and keep full structured JSON in `ForUser`, object
storage, or a retrieval index.

## PaddleOCR Client

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/eruca/goagents/ocrs/paddleocr"
)

func main() {
	apiURL := os.Getenv("PADDLEOCR_API_URL")
	token := os.Getenv("PADDLEOCR_TOKEN")
	pdfBytes, err := os.ReadFile("document.pdf")
	if err != nil {
		panic(err)
	}

	client := paddleocr.NewClient("paddleocr", apiURL, token, 10*time.Minute)
	result, err := client.Handle(context.Background(), pdfBytes)
	if err != nil {
		panic(err)
	}

	title, chunks, err := paddleocr.ParseStructuredChunks(result.Raw)
	if err != nil {
		panic(err)
	}
	fmt.Println(title, len(chunks))
}
```

`PaddleOCR` responses are returned as `ocrs.OCRResult`. `Raw` contains the
original provider JSON so callers can either parse provider-specific details or
use `paddleocr.ParseStructuredChunks`.

## Multiple Tokens

```go
client := paddleocr.NewClientWithTokens(
	"paddleocr",
	apiURL,
	[]string{"token-a", "token-b"},
	10*time.Minute,
)
```

The token pool rotates tokens and marks quota-like HTTP statuses as exhausted
for the current Asia/Shanghai day.

## Chunk Large PDFs

```go
handler := chunking.NewCutAwareHandler(chunking.CutAwareConfig{
	PagesPerChunk:    20,
	ChunkConcurrency: 2,
	Splitter:         chunking.PDFCPUSplitter{},
	Inner:            client,
	Merge:            paddleocr.MergeChunkResponses,
})

result, err := handler.Handle(ctx, pdfBytes)
```

Non-PDF input bypasses chunking. If PDF splitting fails, `CutAwareHandler`
falls back to direct OCR on the original bytes.

## Examples

```bash
go run ./examples/parse
go run ./examples/chunked

PADDLEOCR_API_URL=https://example.com/paddleocr \
PADDLEOCR_TOKEN=token \
OCR_FILE=/path/to/document.pdf \
go run ./examples/paddleocr
```

`examples/paddleocr` exits successfully with a skip message when required
environment variables are missing.

## Verify

```bash
go test ./...
```
