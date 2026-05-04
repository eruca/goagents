# Standalone OCRS Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build `github.com/eruca/ocrs` as a standalone OCR module based on the original `kairon/rag/internal/ocr` code.

**Architecture:** Move shared handler/middleware and OCR provider contracts into the root package, then port PaddleOCR, retry, chunking, and scheduler code to depend only on this module. Keep behavior compatible with the reference implementation while making import paths public and reusable.

**Tech Stack:** Go 1.26.1, standard library HTTP/client concurrency, `github.com/pdfcpu/pdfcpu` for optional PDF splitting.

---

### Task 1: Module And Core Contracts

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/ocrs/go.mod`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/handler.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/ocr.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/types.go`
- Test: `/Users/nick/VibeCoding/goagents/ocrs/ocr_test.go`

**Steps:**

1. Write failing tests for `OCR.Handle`, `OCR.Close`, and middleware wrapping order.
2. Run `go test ./...` and verify the tests fail because the module and types do not exist.
3. Implement the root contracts and wrapper.
4. Run `go test ./...` and verify root tests pass.

### Task 2: Retry Policy

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/ocrs/retrypolicy/retrypolicy.go`
- Test: `/Users/nick/VibeCoding/goagents/ocrs/retrypolicy/retrypolicy_test.go`

**Steps:**

1. Write failing retry tests for retry-then-success, non-retryable errors, and context cancellation while waiting.
2. Run package tests and verify failure.
3. Implement retry config, options, normalization, and middleware.
4. Run package tests and verify pass.

### Task 3: PaddleOCR Provider

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/ocrs/paddleocr/types.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/paddleocr/paddleocr.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/paddleocr/extract.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/paddleocr/merge.go`
- Test: `/Users/nick/VibeCoding/goagents/ocrs/paddleocr/*_test.go`

**Steps:**

1. Write failing tests for HTTP success, HTTP error, retry, token switching, token exhaustion, parsing, cleanup, and merge behavior.
2. Run package tests and verify failure.
3. Implement client, DTOs, token pool, parse, cleanup, and merge.
4. Run package tests and verify pass.

### Task 4: Chunking And Scheduler

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/ocrs/chunking/chunking.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/chunking/pdfcpu_splitter.go`
- Create: `/Users/nick/VibeCoding/goagents/ocrs/scheduler/scheduler.go`
- Test: `/Users/nick/VibeCoding/goagents/ocrs/chunking/chunking_test.go`
- Test: `/Users/nick/VibeCoding/goagents/ocrs/scheduler/scheduler_test.go`

**Steps:**

1. Write failing tests for direct non-PDF processing, invalid config, chunk ordering, first chunk error, scheduler dispatch, and close behavior.
2. Run tests and verify failure.
3. Implement chunking and scheduler.
4. Run tests and verify pass.

### Task 5: Verification

**Files:**
- All module files under `/Users/nick/VibeCoding/goagents/ocrs`

**Steps:**

1. Run `gofmt -w`.
2. Run `go mod tidy`.
3. Run `go test ./...`.
4. Report changed files and verification evidence.

