package hostkit

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

const testDeadline = 2 * time.Second

var defaultRunOptions = Options{
	DrainTimeout:   100 * time.Millisecond,
	CleanupTimeout: 100 * time.Millisecond,
}

type fakeService struct {
	startErr error
	drainErr error
	forceErr error
	closeErr error
	done     chan error

	drainStarted  chan struct{}
	allowDrain    chan struct{}
	forceStarted  chan struct{}
	allowForce    chan struct{}
	forceFinished chan struct{}
	closeStarted  chan struct{}
	allowClose    chan struct{}

	mu                 sync.Mutex
	calls              []string
	doneCalls          int
	ignoreForceContext bool
	forceDeadline      time.Time
	closeDeadline      time.Time
}

func newFakeService() *fakeService {
	return &fakeService{
		done:          make(chan error, 1),
		drainStarted:  make(chan struct{}, 1),
		allowDrain:    make(chan struct{}),
		forceStarted:  make(chan struct{}, 1),
		allowForce:    make(chan struct{}),
		forceFinished: make(chan struct{}, 1),
		closeStarted:  make(chan struct{}, 1),
		allowClose:    make(chan struct{}),
	}
}

func (f *fakeService) Start(context.Context) error {
	f.record("start")
	return f.startErr
}

func (f *fakeService) Done() <-chan error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doneCalls++
	return f.done
}

func (f *fakeService) Drain(ctx context.Context) error {
	f.record("drain")
	f.drainStarted <- struct{}{}
	select {
	case <-f.allowDrain:
		return f.drainErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeService) ForceStop(ctx context.Context) error {
	f.record("force_stop")
	defer func() {
		f.forceFinished <- struct{}{}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		f.mu.Lock()
		f.forceDeadline = deadline
		f.mu.Unlock()
	}
	f.forceStarted <- struct{}{}
	if f.ignoreForceContext {
		<-f.allowForce
		return f.forceErr
	}
	select {
	case <-f.allowForce:
		return f.forceErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeService) Close(ctx context.Context) error {
	f.record("close")
	if deadline, ok := ctx.Deadline(); ok {
		f.mu.Lock()
		f.closeDeadline = deadline
		f.mu.Unlock()
	}
	f.closeStarted <- struct{}{}
	select {
	case <-f.allowClose:
		return f.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeService) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeService) snapshot() ([]string, int, time.Time, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), f.doneCalls, f.forceDeadline, f.closeDeadline
}

func runAsync(
	ctx context.Context,
	service Service,
	interrupts <-chan struct{},
	options Options,
) <-chan Result {
	result := make(chan Result, 1)
	go func() {
		result <- Run(ctx, service, interrupts, options)
	}()
	return result
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitResult(t *testing.T, result <-chan Result) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	select {
	case got := <-result:
		return got
	case <-ctx.Done():
		t.Fatal("timed out waiting for Run result")
		return Result{}
	}
}

func assertResult(t *testing.T, result Result, code Code, cause error) {
	t.Helper()
	if result.Code() != string(code) || result.ExitCode() != exitCode(code) {
		t.Fatalf(
			"Run result = %q/%d, want %q/%d",
			result.Code(),
			result.ExitCode(),
			code,
			exitCode(code),
		)
	}
	if cause != nil && !errors.Is(result.Err(), cause) {
		t.Fatalf("Run error = %v, want cause %v", result.Err(), cause)
	}
}

func assertSuccess(t *testing.T, result Result) {
	t.Helper()
	if result.ExitCode() != 0 || result.Code() != "" || result.Err() != nil {
		t.Fatalf("Run result = %+v, want success", result)
	}
}

func assertCalls(t *testing.T, service *fakeService, want []string) {
	t.Helper()
	got, _, _, _ := service.snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle calls = %v, want %v", got, want)
	}
}

func TestRunDrainSuccess(t *testing.T) {
	service := newFakeService()
	service.done = make(chan error)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, defaultRunOptions)

	waitSignal(t, service.drainStarted, "Drain")
	doneSent := make(chan struct{})
	go func() {
		service.done <- nil
		close(doneSent)
	}()
	waitSignal(t, doneSent, "Done receive")
	close(service.allowDrain)
	waitSignal(t, service.closeStarted, "Close")
	close(service.allowClose)

	assertSuccess(t, waitResult(t, result))
	assertCalls(t, service, []string{"start", "drain", "close"})
}

func TestRunDrainTimeoutForcesAndCloses(t *testing.T) {
	service := newFakeService()
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}

	result := Run(context.Background(), service, interrupts, Options{
		DrainTimeout:   20 * time.Millisecond,
		CleanupTimeout: time.Second,
	})

	assertResult(t, result, CodeShutdownTimeout, nil)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunSecondInterruptForcesWithoutWaitingForDrainTimeout(t *testing.T) {
	service := newFakeService()
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: time.Second,
	})

	waitSignal(t, service.drainStarted, "Drain")
	interrupts <- struct{}{}

	assertResult(t, waitResult(t, result), CodeShutdownTimeout, nil)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunForceStopAndCloseShareCleanupBudget(t *testing.T) {
	service := newFakeService()
	close(service.allowForce)
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: 100 * time.Millisecond,
	})

	waitSignal(t, service.drainStarted, "Drain")
	interrupts <- struct{}{}

	assertResult(t, waitResult(t, result), CodeShutdownCleanupTimeout, nil)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
	_, _, forceDeadline, closeDeadline := service.snapshot()
	if forceDeadline.IsZero() || !forceDeadline.Equal(closeDeadline) {
		t.Fatalf(
			"cleanup deadlines = force %v, close %v; want one shared deadline",
			forceDeadline,
			closeDeadline,
		)
	}
}

func TestRunCleanupTimeoutWinsOverServeFailure(t *testing.T) {
	serveErr := Fail(CodeServeFailed, "serve failed", errors.New("serve cause"))
	service := newFakeService()
	service.done = make(chan error)
	close(service.allowForce)
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: 100 * time.Millisecond,
	})

	waitSignal(t, service.drainStarted, "Drain")
	doneSent := make(chan struct{})
	go func() {
		service.done <- serveErr
		close(doneSent)
	}()
	waitSignal(t, doneSent, "Done receive")
	interrupts <- struct{}{}

	got := waitResult(t, result)
	assertResult(t, got, CodeShutdownCleanupTimeout, nil)
	if errors.Is(got.Err(), serveErr) {
		t.Fatal("serve failure won over cleanup timeout")
	}
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunCleanupDeadlineDoesNotCloseWhileForceStopBlocked(t *testing.T) {
	service := newFakeService()
	service.ignoreForceContext = true
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: 20 * time.Millisecond,
	})

	waitSignal(t, service.drainStarted, "Drain")
	interrupts <- struct{}{}

	assertResult(t, waitResult(t, result), CodeShutdownCleanupTimeout, nil)
	assertCalls(t, service, []string{"start", "drain", "force_stop"})

	// Release the deliberately non-conforming hook so the test itself does not
	// leave its service goroutine behind.
	close(service.allowForce)
	waitSignal(t, service.forceFinished, "ForceStop return")
}

func TestRunStartFailureStillCloses(t *testing.T) {
	startCause := errors.New("start cause")
	startErr := Fail(CodeInitializationFailed, "start failed", startCause)
	service := newFakeService()
	service.startErr = startErr
	close(service.allowClose)

	result := Run(context.Background(), service, make(chan struct{}), defaultRunOptions)

	assertResult(t, result, CodeInitializationFailed, startCause)
	assertCalls(t, service, []string{"start", "close"})
	_, doneCalls, _, _ := service.snapshot()
	if doneCalls != 0 {
		t.Fatalf("Done called %d times after Start failure, want 0", doneCalls)
	}
}

func TestRunStartFailureClassificationSurvivesCloseFailure(t *testing.T) {
	startCause := errors.New("start cause")
	service := newFakeService()
	service.startErr = Fail(CodeInitializationFailed, "start failed", startCause)
	service.closeErr = errors.New("close cause")
	close(service.allowClose)

	result := Run(context.Background(), service, make(chan struct{}), defaultRunOptions)

	assertResult(t, result, CodeInitializationFailed, startCause)
	assertCalls(t, service, []string{"start", "close"})
}

func TestRunUnexpectedDoneErrorForcesThenPreservesServeFailure(t *testing.T) {
	serveCause := errors.New("serve cause")
	serveErr := Fail(CodeServeFailed, "serve failed", serveCause)
	service := newFakeService()
	close(service.allowForce)
	close(service.allowClose)
	service.done <- serveErr

	result := Run(context.Background(), service, make(chan struct{}), defaultRunOptions)

	assertResult(t, result, CodeServeFailed, serveCause)
	assertCalls(t, service, []string{"start", "force_stop", "close"})
}

func TestRunDoneErrorWinsOverReadyInterrupt(t *testing.T) {
	for range 200 {
		serveCause := errors.New("serve cause")
		service := newFakeService()
		close(service.allowDrain)
		close(service.allowForce)
		close(service.allowClose)
		service.done <- Fail(CodeServeFailed, "serve failed", serveCause)
		interrupts := make(chan struct{}, 1)
		interrupts <- struct{}{}

		result := Run(context.Background(), service, interrupts, defaultRunOptions)

		assertResult(t, result, CodeServeFailed, serveCause)
		assertCalls(t, service, []string{"start", "force_stop", "close"})
	}
}

func TestRunDoneErrorWinsOverReadyDrainSuccess(t *testing.T) {
	serveCause := errors.New("serve cause")
	service := newFakeService()
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, defaultRunOptions)

	waitSignal(t, service.drainStarted, "Drain")
	service.done <- Fail(CodeServeFailed, "serve failed", serveCause)
	close(service.allowDrain)

	assertResult(t, waitResult(t, result), CodeServeFailed, serveCause)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunServeFailureWinsOverForceStopFailure(t *testing.T) {
	serveCause := errors.New("serve cause")
	service := newFakeService()
	service.done = make(chan error)
	service.forceErr = errors.New("force cause")
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: time.Second,
	})

	waitSignal(t, service.drainStarted, "Drain")
	doneSent := make(chan struct{})
	go func() {
		service.done <- Fail(CodeServeFailed, "serve failed", serveCause)
		close(doneSent)
	}()
	waitSignal(t, doneSent, "Done receive")
	interrupts <- struct{}{}

	assertResult(t, waitResult(t, result), CodeServeFailed, serveCause)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunUnexpectedNilDoneIsInternalError(t *testing.T) {
	service := newFakeService()
	close(service.allowForce)
	close(service.allowClose)
	service.done <- nil

	result := Run(context.Background(), service, make(chan struct{}), defaultRunOptions)

	assertResult(t, result, CodeInternalError, nil)
	assertCalls(t, service, []string{"start", "force_stop", "close"})
}

func TestRunLifecycleMethodsAreCalledAtMostOnce(t *testing.T) {
	serveCause := errors.New("serve cause")
	service := newFakeService()
	service.done = make(chan error)
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 3)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: time.Second,
	})

	waitSignal(t, service.drainStarted, "Drain")
	doneSent := make(chan struct{})
	go func() {
		service.done <- Fail(CodeServeFailed, "serve failed", serveCause)
		close(doneSent)
	}()
	waitSignal(t, doneSent, "Done receive")
	interrupts <- struct{}{}
	interrupts <- struct{}{}

	assertResult(t, waitResult(t, result), CodeServeFailed, serveCause)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
	_, doneCalls, _, _ := service.snapshot()
	if doneCalls != 1 {
		t.Fatalf("Done called %d times, want 1", doneCalls)
	}
}

func TestRunRejectsNonPositiveTimeoutsWithoutStarting(t *testing.T) {
	tests := []struct {
		name    string
		options Options
	}{
		{
			name: "zero drain timeout",
			options: Options{
				DrainTimeout:   0,
				CleanupTimeout: time.Second,
			},
		},
		{
			name: "negative drain timeout",
			options: Options{
				DrainTimeout:   -time.Nanosecond,
				CleanupTimeout: time.Second,
			},
		},
		{
			name: "zero cleanup timeout",
			options: Options{
				DrainTimeout:   time.Second,
				CleanupTimeout: 0,
			},
		},
		{
			name: "negative cleanup timeout",
			options: Options{
				DrainTimeout:   time.Second,
				CleanupTimeout: -time.Nanosecond,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := newFakeService()
			result := Run(context.Background(), service, make(chan struct{}), tc.options)

			assertResult(t, result, CodeInternalError, nil)
			assertCalls(t, service, nil)
		})
	}
}

func TestRunClassifiedDrainFailureForcesThenPreservesFailure(t *testing.T) {
	drainCause := errors.New("drain cause")
	service := newFakeService()
	service.drainErr = Fail(CodeServeFailed, "drain failed", drainCause)
	close(service.allowDrain)
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}

	result := Run(context.Background(), service, interrupts, defaultRunOptions)

	assertResult(t, result, CodeServeFailed, drainCause)
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunExistingShutdownOutcomeWinsOverClassifiedForceStopFailure(t *testing.T) {
	forceCause := errors.New("force cause")
	service := newFakeService()
	service.forceErr = Fail(CodeServeFailed, "force failed", forceCause)
	close(service.allowForce)
	close(service.allowClose)
	interrupts := make(chan struct{}, 2)
	interrupts <- struct{}{}
	result := runAsync(context.Background(), service, interrupts, Options{
		DrainTimeout:   time.Hour,
		CleanupTimeout: time.Second,
	})

	waitSignal(t, service.drainStarted, "Drain")
	interrupts <- struct{}{}

	got := waitResult(t, result)
	assertResult(t, got, CodeShutdownTimeout, nil)
	if errors.Is(got.Err(), forceCause) {
		t.Fatal("ForceStop failure replaced the existing shutdown outcome")
	}
	assertCalls(t, service, []string{"start", "drain", "force_stop", "close"})
}

func TestRunContextCancellationUsesDrainPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := newFakeService()
	close(service.allowDrain)
	close(service.allowClose)
	result := runAsync(ctx, service, make(chan struct{}), defaultRunOptions)

	cancel()

	assertSuccess(t, waitResult(t, result))
	assertCalls(t, service, []string{"start", "drain", "close"})
}

func TestRunDrainAndSecondInterruptRace(t *testing.T) {
	for range 200 {
		service := newFakeService()
		close(service.allowForce)
		close(service.allowClose)
		interrupts := make(chan struct{}, 2)
		interrupts <- struct{}{}
		result := runAsync(context.Background(), service, interrupts, Options{
			DrainTimeout:   time.Second,
			CleanupTimeout: time.Second,
		})

		waitSignal(t, service.drainStarted, "Drain")
		var release sync.WaitGroup
		release.Add(2)
		go func() {
			defer release.Done()
			close(service.allowDrain)
		}()
		go func() {
			defer release.Done()
			interrupts <- struct{}{}
		}()
		release.Wait()

		got := waitResult(t, result)
		calls, doneCalls, _, _ := service.snapshot()
		switch {
		case reflect.DeepEqual(calls, []string{"start", "drain", "close"}):
			assertSuccess(t, got)
		case reflect.DeepEqual(calls, []string{"start", "drain", "force_stop", "close"}):
			assertResult(t, got, CodeShutdownTimeout, nil)
		default:
			t.Fatalf("lifecycle calls = %v, want graceful or forced sequence", calls)
		}
		if doneCalls != 1 {
			t.Fatalf("Done called %d times, want 1", doneCalls)
		}
	}
}
