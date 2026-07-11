.PHONY: test race example smoke verify

test:
	go test ./...

race:
	go test -race ./...

example:
	go run ./examples/basic

smoke:
	go run ./examples/artifacts
	go run ./examples/context-projection
	go run ./examples/stream
	go run ./examples/stream-sse --once
	go run ./examples/audit-log --once
	go run ./examples/approval
	go run ./examples/approval-deny
	go run ./examples/approval-resume
	go run ./examples/structured-output
	go run ./examples/openai-compatible

verify: test race example smoke
