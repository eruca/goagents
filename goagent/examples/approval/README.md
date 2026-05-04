# Approval Example

This example combines `Agent.Stream` with `WithToolApprover`.

The mock model requests a write tool. The run allows write permission through `RunRequest.AllowedPermissions`, then the host approver approves the concrete tool call before `ActStage` executes it.

Run it with:

```bash
go run ./examples/approval
```

Expected output includes:

```text
approval=approval.requested tool=update_draft reason=<nil>
approval=approval.completed tool=update_draft reason=operator approved
tool=update_draft ref=draft:demo
stream=done llm=2 tools=1
Final answer: draft updated after approval.
```
