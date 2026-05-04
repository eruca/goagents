# Tools Example

This example shows the smallest useful tool contract for host applications.

Tools are host-owned typed actions. The Agent may request them, policy may approve them, and the executor runs them with schema validation and timeouts.

The example includes:

- `Spec.Permission`: declares whether a tool is read, write, or exec so policy can approve it.
- `Spec.Schema`: validates model-supplied JSON before the tool body runs.
- `Spec.Timeout`: bounds tool and middleware execution.
- `Result.ForLLM`: model-visible observation for the next LLM turn.
- `Result.ForUser`: host-visible output for UI or caller surfaces.
- `Result.Silent`: suppresses a successful result from model context.
- `Result.IsError`: marks a recoverable domain error that the model can correct on a later turn.

Return a Go error for executor failures that should abort the current run path. Return `Result{IsError: true, ForLLM: "..."}` for recoverable domain errors such as invalid business state or correctable arguments.

Run it with:

```bash
go run ./examples/tools
```
