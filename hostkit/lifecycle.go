package hostkit

import (
	"context"
	"errors"
	"time"
)

// Service is the lifecycle contract managed by Run.
type Service interface {
	Start(context.Context) error
	Done() <-chan error
	Drain(context.Context) error
	ForceStop(context.Context) error
	Close(context.Context) error
}

// Options bounds graceful draining and forced cleanup.
type Options struct {
	DrainTimeout   time.Duration
	CleanupTimeout time.Duration
}

// Run starts service and synchronously drives it to one terminal Result.
func Run(
	ctx context.Context,
	service Service,
	interrupts <-chan struct{},
	options Options,
) Result {
	if options.DrainTimeout <= 0 || options.CleanupTimeout <= 0 {
		return resultFromError(errors.New("hostkit: lifecycle timeouts must be positive"))
	}
	if service == nil {
		return resultFromError(errors.New("hostkit: nil service"))
	}

	if err := service.Start(ctx); err != nil {
		return cleanup(ctx, service, options.CleanupTimeout, false, err)
	}

	done := service.Done()
	shutdownRequested := false
	for {
		// Done has priority over a shutdown request that was already selected.
		// Keeping the request as state and checking Done again avoids losing an
		// error when both channels were ready in the same select.
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("hostkit: service stopped without an error")
			}
			return cleanup(ctx, service, options.CleanupTimeout, true, err)
		default:
		}
		if shutdownRequested {
			return drain(ctx, service, done, interrupts, options)
		}

		select {
		case err := <-done:
			if err == nil {
				err = errors.New("hostkit: service stopped without an error")
			}
			return cleanup(ctx, service, options.CleanupTimeout, true, err)
		case <-ctx.Done():
			shutdownRequested = true
		case <-interrupts:
			shutdownRequested = true
		}
	}
}

type drainTerminal struct {
	force   bool
	outcome error
}

func drain(
	parent context.Context,
	service Service,
	done <-chan error,
	interrupts <-chan struct{},
	options Options,
) Result {
	drainCtx, cancelDrain := context.WithTimeout(
		context.WithoutCancel(parent),
		options.DrainTimeout,
	)
	drainResult := make(chan error, 1)
	go func() {
		drainResult <- service.Drain(drainCtx)
	}()

	var terminal *drainTerminal
	for {
		// A lower-priority terminal event is first retained as state. This
		// priority phase then consumes an already-observable Done result before
		// cleanup starts, including when Drain and Done became ready together.
		select {
		case err := <-done:
			done = nil
			if err != nil {
				cancelDrain()
				terminal = &drainTerminal{force: true, outcome: err}
			}
		default:
		}
		if terminal != nil {
			cancelDrain()
			return cleanup(
				parent,
				service,
				options.CleanupTimeout,
				terminal.force,
				terminal.outcome,
			)
		}

		select {
		case err := <-done:
			// Done(nil) is expected while draining. A non-nil result is a
			// serve failure and therefore forces cleanup even if Drain succeeds.
			done = nil
			if err != nil {
				cancelDrain()
				terminal = &drainTerminal{force: true, outcome: err}
			}
		case err := <-drainResult:
			cancelDrain()
			if drainCtx.Err() == context.DeadlineExceeded {
				terminal = &drainTerminal{force: true, outcome: shutdownTimeoutError()}
			} else {
				terminal = &drainTerminal{force: err != nil, outcome: err}
			}
		case <-drainCtx.Done():
			cancelDrain()
			terminal = &drainTerminal{force: true, outcome: shutdownTimeoutError()}
		case <-interrupts:
			cancelDrain()
			terminal = &drainTerminal{force: true, outcome: shutdownTimeoutError()}
		}
	}
}

func cleanup(
	parent context.Context,
	service Service,
	timeout time.Duration,
	force bool,
	outcome error,
) Result {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancelCleanup()

	if force {
		err, timedOut := runCleanupHook(cleanupCtx, service.ForceStop)
		if timedOut {
			return cleanupTimeoutResult()
		}
		if err != nil && outcome == nil {
			outcome = err
		}
	}

	err, timedOut := runCleanupHook(cleanupCtx, service.Close)
	if timedOut {
		return cleanupTimeoutResult()
	}
	if err != nil && outcome == nil {
		outcome = err
	}
	return resultFromError(outcome)
}

func runCleanupHook(
	ctx context.Context,
	hook func(context.Context) error,
) (err error, timedOut bool) {
	result := make(chan error, 1)
	go func() {
		result <- hook(ctx)
	}()

	select {
	case err := <-result:
		if ctx.Err() == context.DeadlineExceeded {
			return nil, true
		}
		return err, false
	case <-ctx.Done():
		return nil, true
	}
}

func shutdownTimeoutError() error {
	return Fail(
		CodeShutdownTimeout,
		"shutdown timed out",
		context.DeadlineExceeded,
	)
}

func cleanupTimeoutResult() Result {
	return resultFromError(Fail(
		CodeShutdownCleanupTimeout,
		"shutdown cleanup timed out",
		context.DeadlineExceeded,
	))
}
