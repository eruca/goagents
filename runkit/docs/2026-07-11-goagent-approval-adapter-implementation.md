# Goagent Approval Adapter Implementation Plan

1. Add `runkit/goagentapproval` with narrow `Cipher` and `Resumer` interfaces.
2. Encrypt interrupted `RunCheckpoint` values before passing them to
   `CheckpointStore`.
3. Lease, decrypt, resume once, and close the lease according to the returned
   Agent result.
4. Persist a second interruption before consuming the first lease; fail the
   first lease if any continuation step is uncertain.
5. Verify package tests, the PostgreSQL checkpoint integration tests, race
   tests, and the workspace verification script.
