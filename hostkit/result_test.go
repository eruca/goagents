package hostkit

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

var exitCases = []struct {
	code Code
	exit int
}{
	{CodeInternalError, 1},
	{CodeConfigFailed, 2},
	{CodeInitializationFailed, 2},
	{CodeListenFailed, 3},
	{CodeServeFailed, 4},
	{CodeShutdownTimeout, 5},
	{CodeShutdownCleanupTimeout, 5},
}

func TestCodeExitMapping(t *testing.T) {
	for _, tc := range exitCases {
		t.Run(string(tc.code), func(t *testing.T) {
			if got := exitCode(tc.code); got != tc.exit {
				t.Fatalf("exitCode(%q) = %d, want %d", tc.code, got, tc.exit)
			}
		})
	}
}

func TestFailurePreservesCauseAndClassification(t *testing.T) {
	cause := errors.New("raw listener error")
	err := Fail(CodeListenFailed, "safe listen failure", cause)

	if got := err.Error(); got != "safe listen failure" {
		t.Fatalf("Error() = %q, want safe message", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Fail error does not unwrap to cause")
	}

	result := resultFromError(err)
	if result.Code() != string(CodeListenFailed) || result.ExitCode() != 3 {
		t.Fatalf("resultFromError(Fail(...)) = %+v, want listen_failed/3", result)
	}
	if result.Err() != err {
		t.Fatal("result error did not retain classified error")
	}
}

func TestFailureExitCodeCannotBeCustomized(t *testing.T) {
	result := resultFromError(Fail(CodeServeFailed, "safe serve failure", errors.New("raw cause")))
	if result.ExitCode() != 4 {
		t.Fatalf("Fail-derived exit code = %d, want 4", result.ExitCode())
	}
}

func TestFailureNormalizesUnclassifiedErrors(t *testing.T) {
	err := errors.New("unclassified failure")
	result := resultFromError(err)

	if result.Code() != string(CodeInternalError) || result.ExitCode() != 1 {
		t.Fatalf("resultFromError(unclassified) = %+v, want internal_error/1", result)
	}
	if !errors.Is(result.Err(), err) {
		t.Fatal("result error does not unwrap to unclassified cause")
	}
}

func TestFailureNormalizesUnknownCode(t *testing.T) {
	cause := errors.New("raw cause")
	result := resultFromError(Fail(Code("unknown"), "safe message", cause))

	if result.Code() != string(CodeInternalError) || result.ExitCode() != 1 {
		t.Fatalf("resultFromError(Fail(unknown, ...)) = %+v, want internal_error/1", result)
	}
	if !errors.Is(result.Err(), cause) {
		t.Fatal("normalized unknown-code error does not retain its cause")
	}
}

func TestResultFromNilIsSuccess(t *testing.T) {
	result := resultFromError(nil)
	if result.ExitCode() != 0 || result.Code() != "" || result.Err() != nil {
		t.Fatalf("resultFromError(nil) = %+v, want zero success Result", result)
	}
}

func TestResultExposesOnlyReadOnlyMethods(t *testing.T) {
	resultType := reflect.TypeOf(Result{})
	for i := 0; i < resultType.NumField(); i++ {
		field := resultType.Field(i)
		if field.IsExported() {
			t.Fatalf("Result exposes writable field %q", field.Name)
		}
	}

	for _, method := range []string{"ExitCode", "Code", "Err"} {
		if _, ok := resultType.MethodByName(method); !ok {
			t.Fatalf("Result does not expose read-only %s method", method)
		}
	}
}

func TestWriteErrorSkipsSuccessfulResult(t *testing.T) {
	var output bytes.Buffer
	if err := WriteError(&output, Result{}); err != nil {
		t.Fatalf("WriteError() error = %v", err)
	}
	if got := output.String(); got != "" {
		t.Fatalf("WriteError() wrote %q, want no bytes", got)
	}
}

func TestWriteErrorWritesSingleJSONLine(t *testing.T) {
	result := resultFromError(Fail(CodeListenFailed, "safe listen failure", errors.New("raw listener error")))
	var output bytes.Buffer

	if err := WriteError(&output, result); err != nil {
		t.Fatalf("WriteError() error = %v", err)
	}
	const want = "{\"level\":\"error\",\"event\":\"host_exit\",\"code\":\"listen_failed\",\"message\":\"safe listen failure\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("WriteError() = %q, want %q", got, want)
	}
}

func TestWriteErrorEscapesSpecialMessageCharacters(t *testing.T) {
	result := resultFromError(Fail(CodeServeFailed, "safe \"message\"\nnext", errors.New("raw\nnested error")))
	var output bytes.Buffer

	if err := WriteError(&output, result); err != nil {
		t.Fatalf("WriteError() error = %v", err)
	}
	const want = "{\"level\":\"error\",\"event\":\"host_exit\",\"code\":\"serve_failed\",\"message\":\"safe \\\"message\\\"\\nnext\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("WriteError() = %q, want %q", got, want)
	}
	if bytes.Count(output.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("WriteError() emitted multiple lines: %q", output.String())
	}
}

func TestWriteErrorDoesNotExposeUnclassifiedCause(t *testing.T) {
	result := resultFromError(errors.New("raw provider secret"))
	var output bytes.Buffer

	if err := WriteError(&output, result); err != nil {
		t.Fatalf("WriteError() error = %v", err)
	}
	const want = "{\"level\":\"error\",\"event\":\"host_exit\",\"code\":\"internal_error\",\"message\":\"internal error\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("WriteError() = %q, want %q", got, want)
	}
}
