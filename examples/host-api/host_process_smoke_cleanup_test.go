//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type cleanupTestReporter struct {
	errors []string
}

func (r *cleanupTestReporter) Helper() {}

func (r *cleanupTestReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
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
