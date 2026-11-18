package memorykit

import "context"

// RunGate lets several worker processes share one background job so that at
// most one holder, across all processes using the same gate, runs it at a
// time.
//
// TryRun calls run at most once, synchronously, while holding the gate, and
// releases the gate before returning, even when run panics or ctx is canceled.
// The context passed to run is derived from ctx.
//
// ran=false with a nil error means another holder is running right now and
// the caller should skip this round; run was not called. A non-nil error with
// ran=false means the gate itself failed and run was not called. With
// ran=true, the error is run's error, possibly joined with an error from
// releasing the gate.
type RunGate interface {
	TryRun(ctx context.Context, run func(context.Context) error) (ran bool, err error)
}
