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
	select {
	case <-ctx.Done():
		return drain(ctx, service, done, interrupts, options)
	case <-interrupts:
		return drain(ctx, service, done, interrupts, options)
	case err := <-done:
		if err == nil {
			err = errors.New("hostkit: service stopped without an error")
		}
		return cleanup(ctx, service, options.CleanupTimeout, true, err)
	}
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

	var serveErr error
	for {
		select {
		case err := <-done:
			// Done(nil) is the expected serve termination during graceful drain.
			// Disable Done after its one terminal observation to avoid a closed
			// channel spinning this state.
			done = nil
			if err != nil {
				serveErr = err
			}
		case err := <-drainResult:
			if drainCtx.Err() == context.DeadlineExceeded {
				cancelDrain()
				return cleanup(
					parent,
					service,
					options.CleanupTimeout,
					true,
					preferServeError(serveErr, shutdownTimeoutError()),
				)
			}
			cancelDrain()
			if err == nil {
				return cleanup(parent, service, options.CleanupTimeout, false, serveErr)
			}
			return cleanup(
				parent,
				service,
				options.CleanupTimeout,
				true,
				preferServeError(serveErr, err),
			)
		case <-drainCtx.Done():
			cancelDrain()
			return cleanup(
				parent,
				service,
				options.CleanupTimeout,
				true,
				preferServeError(serveErr, shutdownTimeoutError()),
			)
		case <-interrupts:
			cancelDrain()
			return cleanup(
				parent,
				service,
				options.CleanupTimeout,
				true,
				preferServeError(serveErr, shutdownTimeoutError()),
			)
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
		if err != nil {
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

func preferServeError(serveErr, fallback error) error {
	if serveErr != nil {
		return serveErr
	}
	return fallback
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
