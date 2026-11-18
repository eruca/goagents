package memorykit

import (
	"sync"
	"testing"
	"time"
)

func TestWorkerStatsZeroSnapshot(t *testing.T) {
	var stats workerStats
	if got := stats.snapshot(); got != (WorkerSnapshot{}) {
		t.Fatalf("snapshot() = %#v, want zero snapshot", got)
	}
}

func TestWorkerStatsRecordsTransitions(t *testing.T) {
	var stats workerStats
	successAt := time.Date(2026, 7, 27, 8, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	stats.recordSuccess(successAt)
	got := stats.snapshot()
	if got.SuccessTotal != 1 || got.FailureTotal != 0 {
		t.Fatalf("success snapshot totals = (%d, %d), want (1, 0)", got.SuccessTotal, got.FailureTotal)
	}
	if !got.LastTransitionTime.Equal(successAt) || got.LastTransitionTime.Location() != time.UTC {
		t.Fatalf("success transition = %v, want %v in UTC", got.LastTransitionTime, successAt)
	}

	failureAt := successAt.Add(time.Minute)
	stats.recordFailure(WorkerErrorProviderFailed, failureAt)
	got = stats.snapshot()
	if got.SuccessTotal != 1 || got.FailureTotal != 1 {
		t.Fatalf("failure snapshot totals = (%d, %d), want (1, 1)", got.SuccessTotal, got.FailureTotal)
	}
	if got.LastErrorClass != WorkerErrorProviderFailed {
		t.Fatalf("last error class = %q, want %q", got.LastErrorClass, WorkerErrorProviderFailed)
	}
	if !got.LastTransitionTime.Equal(failureAt) || got.LastTransitionTime.Location() != time.UTC {
		t.Fatalf("failure transition = %v, want %v in UTC", got.LastTransitionTime, failureAt)
	}

	laterSuccessAt := failureAt.Add(time.Minute)
	stats.recordSuccess(laterSuccessAt)
	got = stats.snapshot()
	if got.SuccessTotal != 2 || got.FailureTotal != 1 {
		t.Fatalf("later success totals = (%d, %d), want (2, 1)", got.SuccessTotal, got.FailureTotal)
	}
	if got.LastErrorClass != WorkerErrorProviderFailed {
		t.Fatalf("later success cleared last error class: %q", got.LastErrorClass)
	}
	if !got.LastTransitionTime.Equal(laterSuccessAt) {
		t.Fatalf("later success transition = %v, want %v", got.LastTransitionTime, laterSuccessAt)
	}
}

func TestWorkerStatsRejectsUnknownErrorClass(t *testing.T) {
	var stats workerStats
	stats.recordFailure(WorkerErrorClass("provider-secret-payload"), time.Now())
	if got := stats.snapshot(); got != (WorkerSnapshot{}) {
		t.Fatalf("invalid error class mutated snapshot: %#v", got)
	}
}

func TestWorkerStatsConcurrentSnapshots(t *testing.T) {
	var stats workerStats
	start := make(chan struct{})
	var writers sync.WaitGroup
	var readers sync.WaitGroup
	for index := 0; index < 100; index++ {
		writers.Add(1)
		go func(index int) {
			defer writers.Done()
			<-start
			at := time.Date(2026, 7, 27, 8, 0, index, 0, time.UTC)
			if index%2 == 0 {
				stats.recordSuccess(at)
				return
			}
			stats.recordFailure(WorkerErrorSourceReadFailed, at)
		}(index)

		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			_ = stats.snapshot()
		}()
	}
	close(start)
	writers.Wait()
	readers.Wait()

	got := stats.snapshot()
	if got.SuccessTotal+got.FailureTotal != 100 {
		t.Fatalf("final transition total = %d, want 100", got.SuccessTotal+got.FailureTotal)
	}
}
