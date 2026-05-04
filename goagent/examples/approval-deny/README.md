# Approval Deny Example

This example shows the approval denial path for `Agent.Stream` and `WithToolApprover`.

The mock model requests a write tool. `RunRequest.AllowedPermissions` allows the write permission through policy, then the host approver rejects the concrete tool call before `ActStage` executes it.

Run it with:

```bash
go run ./examples/approval-deny
```

Expected output includes:

```text
approval=approval.requested tool=write_file reason=<nil>
approval=approval.denied tool=write_file reason=operator rejected
stream=done llm=1 tools=0 abort="approval denied: tool \"write_file\" denied: operator rejected"
err=approval denied
tool_ran=false
```

The example uses `errors.Is(err, agentcore.ErrApprovalDenied)` and confirms the denied tool body never runs.
