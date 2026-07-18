//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type cleanupTestReporter struct {
	errors   []string
	cleanups []func()
}

func (r *cleanupTestReporter) Helper() {}

func (r *cleanupTestReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *cleanupTestReporter) Cleanup(cleanup func()) {
	r.cleanups = append(r.cleanups, cleanup)
}

func (r *cleanupTestReporter) runCleanups() {
	for index := len(r.cleanups) - 1; index >= 0; index-- {
		r.cleanups[index]()
	}
}

func TestHostProcessCapturesStdoutAndStderrSeparately(t *testing.T) {
	const sentinel = "host-process-sensitive-sentinel"
	command := exec.Command("/bin/sh", "-c", `
printf 'stdout=%s\n' "$HOST_PROCESS_SENTINEL"
printf 'stderr=%s\n' "$HOST_PROCESS_SENTINEL" >&2
	`)
	command.Env = overrideEnvironment(map[string]string{"HOST_PROCESS_SENTINEL": sentinel})
	process := startCapturedHostCommand(t, command, sentinel)
	cleanupKilledHostProcess(t, process)

	exitCode, err := waitHostProcess(process, time.Second)
	if err != nil || exitCode != 0 {
		t.Fatalf("wait host process = (%d, %v), want (0, nil)", exitCode, err)
	}
	if !process.stdout.ContainsSensitive(sentinel) || !process.stderr.ContainsSensitive(sentinel) ||
		!process.output.ContainsSensitive(sentinel) {
		t.Fatal("captured buffers did not retain the sentinel for raw leakage scanning")
	}
	if got := process.stdout.String(); got != "stdout=[REDACTED]\n" {
		t.Fatalf("stdout = %q, want only redacted stdout", got)
	}
	if got := process.stderr.String(); got != "stderr=[REDACTED]\n" {
		t.Fatalf("stderr = %q, want only redacted stderr", got)
	}
	combined := process.output.String()
	if combined != "stdout=[REDACTED]\nstderr=[REDACTED]\n" &&
		combined != "stderr=[REDACTED]\nstdout=[REDACTED]\n" {
		t.Fatalf("combined output = %q, want both redacted streams", combined)
	}
}

func TestWaitHostExitAcceptsExpectedNonZeroExit(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create process barrier: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	command := exec.Command("/bin/sh", "-c", "read release; exit 7")
	command.Stdin = reader
	process := startCapturedHostCommand(t, command)
	cleanupKilledHostProcess(t, process)

	start := make(chan struct{})
	results := make(chan struct {
		exitCode int
		err      error
	}, 2)
	for range 2 {
		go func() {
			<-start
			exitCode, waitErr := waitHostProcess(process, time.Second)
			results <- struct {
				exitCode int
				err      error
			}{exitCode: exitCode, err: waitErr}
		}()
	}
	close(start)
	if _, err := writer.WriteString("release\n"); err != nil {
		t.Fatalf("release process barrier: %v", err)
	}

	for range 2 {
		result := <-results
		if result.err != nil || result.exitCode != 7 {
			t.Fatalf("concurrent wait = (%d, %v), want (7, nil)", result.exitCode, result.err)
		}
	}
	exitCode, err := waitHostProcess(process, 10*time.Millisecond)
	if err != nil || exitCode != 7 {
		t.Fatalf("repeated wait = (%d, %v), want (7, nil)", exitCode, err)
	}
}

func TestWaitHostExitFailsWhenCleanupKillWasRequired(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create process blocker: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	command := exec.Command("/bin/sh", "-c", "trap '' INT; printf 'ready\n'; read blocked")
	command.Stdin = reader
	process := startCapturedHostCommand(t, command)
	waitForCapturedOutput(t, process.stdout, "ready\n")

	reporter := &cleanupTestReporter{}
	cleanupKilledHostProcessWithTimeout(reporter, process, 20*time.Millisecond)
	reporter.runCleanups()

	if len(reporter.errors) != 1 || !strings.Contains(reporter.errors[0], "required kill") {
		t.Fatalf("cleanup errors = %#v, want one required-kill failure", reporter.errors)
	}
	exitCode, err := waitHostProcess(process, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("wait after cleanup kill: %v", err)
	}
	if process.cmd.ProcessState == nil || exitCode != process.cmd.ProcessState.ExitCode() {
		t.Fatalf("cleanup did not reap process: state=%v exit=%d", process.cmd.ProcessState, exitCode)
	}
}

func TestHostExitErrorIsExactlyOneJSONLine(t *testing.T) {
	const sentinel = "host-exit-sensitive-invalid-duration"
	binary := buildHostBinary(t)
	command := exec.Command(binary)
	command.Env = overrideEnvironment(map[string]string{
		hostShutdownTimeoutEnv: sentinel,
	})
	process := startCapturedHostCommand(t, command, sentinel)
	cleanupKilledHostProcess(t, process)

	exitCode, err := waitHostProcess(process, 5*time.Second)
	if err != nil || exitCode != 2 {
		t.Fatalf("wait host config failure = (%d, %v), want (2, nil)", exitCode, err)
	}
	if got := process.stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	assertExactHostExitJSON(t, process.stderr.String(), "config_failed", "host configuration failed")
	if got := process.output.String(); got != process.stderr.String() {
		t.Fatalf("combined output = %q, want exact stderr %q", got, process.stderr.String())
	}
	for name, buffer := range map[string]*lockedBuffer{
		"stdout": process.stdout,
		"stderr": process.stderr,
		"output": process.output,
	} {
		if buffer.ContainsSensitive(sentinel) {
			t.Fatalf("%s retained leaked sentinel", name)
		}
		if strings.Contains(buffer.String(), sentinel) {
			t.Fatalf("%s exposed sentinel through redacted String", name)
		}
	}
}

func startCapturedHostCommand(t *testing.T, command *exec.Cmd, redactions ...string) *hostProcess {
	t.Helper()
	process := &hostProcess{
		cmd:    command,
		stdout: &lockedBuffer{},
		stderr: &lockedBuffer{},
		output: &lockedBuffer{},
	}
	process.stdout.SetRedactions(redactions...)
	process.stderr.SetRedactions(redactions...)
	process.output.SetRedactions(redactions...)
	command.Stdout = io.MultiWriter(process.stdout, process.output)
	command.Stderr = io.MultiWriter(process.stderr, process.output)
	if err := command.Start(); err != nil {
		t.Fatalf("start host command: %v", err)
	}
	return process
}

func waitForCapturedOutput(t *testing.T, buffer *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buffer.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("captured output = %q, want %q", buffer.String(), want)
}

func assertExactHostExitJSON(t *testing.T, raw, code, message string) {
	t.Helper()
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") {
		t.Fatalf("host exit stderr = %q, want exactly one newline-terminated JSON line", raw)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(raw, "\n")), &entry); err != nil {
		t.Fatalf("decode host exit JSON: %v; raw=%q", err, raw)
	}
	if len(entry) != 4 ||
		entry["level"] != "error" ||
		entry["event"] != "host_exit" ||
		entry["code"] != code ||
		entry["message"] != message {
		t.Fatalf("host exit JSON = %#v, want exact level/event/code/message", entry)
	}
}

func TestSmokeKeychainCleanupRefusesNonSmokeService(t *testing.T) {
	reporter := &cleanupTestReporter{}
	deleteCalls := 0
	cleanup := smokeKeychainCleanupWithDelete(
		reporter,
		localApprovalKeychainService,
		localApprovalKeyID,
		func(string, string) ([]byte, error) {
			deleteCalls++
			return nil, nil
		},
	)

	cleanup()

	if deleteCalls != 0 {
		t.Fatalf("delete calls = %d, want 0", deleteCalls)
	}
	if len(reporter.errors) != 1 || !strings.Contains(reporter.errors[0], "refusing to delete non-smoke") {
		t.Fatalf("cleanup errors = %#v, want refusal", reporter.errors)
	}
}

func TestSmokeKeychainCleanupDeletesExactItemOnce(t *testing.T) {
	reporter := &cleanupTestReporter{}
	service := localApprovalKeychainService + ".smoke.test"
	deleteCalls := 0
	cleanup := smokeKeychainCleanupWithDelete(
		reporter,
		service,
		localApprovalKeyID,
		func(gotService, gotAccount string) ([]byte, error) {
			deleteCalls++
			if gotService != service || gotAccount != "approval-data-key:"+localApprovalKeyID {
				t.Fatalf("delete item = %q/%q", gotService, gotAccount)
			}
			return nil, nil
		},
	)

	cleanup()
	cleanup()

	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}
	if len(reporter.errors) != 0 {
		t.Fatalf("cleanup errors = %#v, want none", reporter.errors)
	}
}

func TestSmokeKeychainCleanupReportsOnlyUnexpectedDeleteErrors(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantErrors int
	}{
		{name: "item not found", output: "The specified item could not be found in the keychain."},
		{name: "unexpected error", output: "permission denied", wantErrors: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reporter := &cleanupTestReporter{}
			cleanup := smokeKeychainCleanupWithDelete(
				reporter,
				localApprovalKeychainService+".smoke.test",
				localApprovalKeyID,
				func(string, string) ([]byte, error) {
					return []byte(test.output), errors.New("delete failed")
				},
			)

			cleanup()

			if len(reporter.errors) != test.wantErrors {
				t.Fatalf("cleanup errors = %#v, want %d", reporter.errors, test.wantErrors)
			}
		})
	}
}
