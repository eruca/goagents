# hostkit

`hostkit` coordinates the lifecycle of one host-owned `Service`. It uses only
the Go standard library and keeps process policy separate from the components
owned by a concrete host.

## Lifecycle contract

A `Service` synchronously starts, exposes one stable terminal channel, stops
intake during drain, cancels and reconciles active work during force stop, and
releases resources during close:

```go
package example

import (
	"context"
	"fmt"
	"time"

	"github.com/eruca/goagents/hostkit"
)

func run(
	ctx context.Context,
	service hostkit.Service,
	interrupts <-chan struct{},
) hostkit.Result {
	result := hostkit.Run(ctx, service, interrupts, hostkit.Options{
		DrainTimeout:   30 * time.Second,
		CleanupTimeout: 5 * time.Second,
	})
	if result.Err() != nil {
		fmt.Printf("%s: %v\n", result.Code(), result.Err())
	}
	return result
}
```

`Start` must not reuse its context as the execution root. `Done` must return the
same read-only channel on every call and report one terminal result. `Drain`
stops intake and waits for active work. `ForceStop` cancels active work and
performs host-owned reconciliation. `Close` releases resources after active
work can no longer use them.

`Result` has no writable outcome fields. A zero result means success;
`ExitCode()`, `Code()`, and `Err()` expose the normalized outcome. A service
uses `Fail` to classify a safe public message while retaining the original
cause for `errors.Is`.

| Code | Exit |
|---|---:|
| success (empty code) | 0 |
| `internal_error` | 1 |
| `config_failed` | 2 |
| `initialization_failed` | 2 |
| `listen_failed` | 3 |
| `serve_failed` | 4 |
| `shutdown_timeout` | 5 |
| `shutdown_cleanup_timeout` | 5 |

The first interrupt starts `Drain` with `DrainTimeout`. A second interrupt
cancels the remaining drain wait and enters force stop; it does not skip
cleanup. `ForceStop` and `Close` share one `CleanupTimeout` deadline. If force
stop does not return before that deadline, `hostkit` does not call `Close`
concurrently and returns `shutdown_cleanup_timeout`.

`hostkit` does not manage workflows, HTTP servers, operating-system signal
constants, participant graphs, or external side effects. The host adapter owns
those policies and translates signals into the generic interrupt channel.
