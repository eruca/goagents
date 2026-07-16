# Standalone OCRS Design

## Goal

Turn the OCR code from `kairon/rag/internal/ocr` into a standalone Go module that can be imported by other libraries.

## Module Boundary

The module path is `github.com/eruca/goagents/ocrs`. It owns its generic handler and middleware contracts, OCR provider contracts, retry policy, PaddleOCR HTTP client, structured PaddleOCR response parsing, OCR result merging, chunk-aware processing, and endpoint dispatching.

The module must not import `github.com/eruca/rag` packages. Types previously sourced from `rag/core`, `rag/internal/ocr/types`, `rag/internal/retrypolicy`, and `rag/types` move into this module.

## Public API

The root package exposes:

- `Handler[I, O]`, `HandlerFunc[I, O]`
- `Middleware[I, O]`, `MiddlewareFunc[I, O]`, `Chain[I, O]`
- `OCR[T]` with `Handle` and `Close`
- `Provider[I, O]`, `ProviderConfig[I, O]`, `OCRResult`, and `ProcessedChunk`

`paddleocr` exposes the HTTP client, PaddleOCR response DTOs, raw response merge, and structured chunk extraction.

`chunking` exposes PDF chunk orchestration behind a `Splitter` interface. A `PDFCPUSplitter` implementation is included for parity with the original code.

`scheduler` exposes a worker dispatcher for routing OCR calls across providers.

## Error Handling

PaddleOCR HTTP status failures return bounded status errors. Retry decisions treat transient HTTP and network failures as retryable, but do not retry context cancellation, deadline expiry, or exhausted token pools.

The PaddleOCR token pool rotates tokens and marks quota-like status codes as exhausted for the current Asia/Shanghai day.

## Testing

Tests are migrated from the original implementation and adjusted to import local module types. They cover:

- Middleware chaining and `OCR` wrapper lifecycle
- Retry backoff behavior
- PaddleOCR request payload, auth headers, retries, token rotation, and quota exhaustion
- PaddleOCR response parsing and text cleanup
- Response merging
- Chunking orchestration
- Scheduler queue dispatch and close behavior

