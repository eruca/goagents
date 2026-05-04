# Policy Example

This example shows the default policy boundary between model-requested tool calls and tool execution.

The read-only tool runs because explicit `read` permission is allowed by default. The first write tool call is denied before its body runs because `write` is denied by default. The final write call succeeds because the run passes `RunRequest.AllowedPermissions` and a typed `RunRequest.PolicyContext`.

Run it with:

```bash
go run ./examples/policy
```

Expected output:

```text
read allowed
write denied
write allowed
```
