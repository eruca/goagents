# Goagent Approval Adapter Design

## Goal

Connect `agentcore`'s tool-approval interruption to the existing runkit
checkpoint state machine without moving encryption, caller authorization, or
Agent execution into the persistence store.

## Boundary

`runkit/goagentapproval` is a host adapter, not a policy service:

- the host authenticates the operator before it calls `ApproveAndResume` or
  `Reject`;
- the host implements `Cipher` and owns encryption keys;
- `CheckpointStore` continues to store only ciphertext and lifecycle data;
- `agentcore.Agent` remains responsible for exact decision validation and tool
  execution.

## Resume flow

```text
Agent interrupted
  -> host encrypts and SavePending
  -> operator approves or rejects
      -> reject: checkpoint becomes rejected; Agent is not called
      -> approve: atomic approve-and-lease
          -> decrypt and ResumeDetailed once
              -> final: consume old lease
              -> paused again: encrypt/save next checkpoint, then consume old lease
              -> failure: fail old lease, never automatically replay it
```

The resume command includes a host-generated next checkpoint ID and expiry in
advance. This avoids a post-tool-execution gap where a second interruption
would exist only in process memory. If the next checkpoint cannot be saved,
the original lease becomes `failed`, because the executed tool may already
have an external effect. The terminal record carries only a bounded failure
code, not raw decrypted state or a runtime error string.

Saving the next checkpoint and consuming the old lease are separate store
operations: a crash between them can leave the old lease `leased` alongside a
new `pending` checkpoint. That is deliberate fail-closed behavior: the old
lease is never made claimable again, so it cannot replay the same tool call.

## Verification

The adapter tests cover encrypted storage, normal resume consumption, the
ordering of a repeated interruption, terminal failure when the next checkpoint
cannot persist, explicit rejection, decryption failure, and a real Agent pause
and resume that executes its approved tool exactly once.

For a local macOS host, `approvalcrypto` uses a Keychain-held AES-256-GCM data
key. The adapter supplies checkpoint identity as AEAD associated data, which
binds ciphertext to its checkpoint, tenant, and agent definition even though
those fields remain queryable in the checkpoint store.
