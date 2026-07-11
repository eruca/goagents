# Host API OIDC Approval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Require a verified OIDC JWT before a host-api workflow can be approved, and derive the recorded approver solely from the token subject.

**Architecture:** `examples/host-api/approval_auth.go` owns a narrow approval-authentication interface and a `coreos/go-oidc` implementation. The HTTP handler receives a verified identity from that boundary, never a caller-supplied approver field. The CLI builds the verifier from issuer/audience environment variables; programmatic hosts inject their own verifier through `Config`.

**Tech Stack:** Go 1.26, `github.com/coreos/go-oidc/v3`, `net/http`, existing workflowkit persistence.

## Global Constraints

- Accept only an OIDC-verified bearer token on `POST /workflows/{id}/approve`.
- Verify issuer, audience, signature, and expiry through OIDC discovery/JWKS.
- Use the verified `sub` claim as `approved_by`; never persist a bearer token.
- Missing, malformed, invalid, expired, wrong-issuer, or wrong-audience tokens return generic `401 unauthorized`.
- A nil authenticator is fail-closed for approvals; no insecure default exists.
- Preserve existing workflow behavior and unrelated endpoints.

---

### Task 1: Add the OIDC authentication boundary

**Files:**

- Create: `examples/host-api/approval_auth.go`
- Modify: `examples/host-api/go.mod`
- Test: `examples/host-api/approval_auth_test.go`

**Consumes:** OIDC issuer URL and audience supplied by the host process.

**Produces:**

```go
type ApprovalAuthenticator interface {
    AuthenticateApproval(context.Context, string) (ApprovalIdentity, error)
}
type ApprovalIdentity struct { Subject string }
func NewOIDCApprovalAuthenticator(context.Context, issuer, audience string) (*OIDCApprovalAuthenticator, error)
```

- [x] **Step 1: Write failing verifier tests**

Create a local OIDC discovery/JWKS test server, mint RS256 tokens with a
standard-library test helper, and assert that a valid token returns its `sub`.
Add separate assertions that an expired token, wrong audience, missing subject,
and a malformed bearer value return `ErrApprovalUnauthorized`.

- [x] **Step 2: Run the targeted test and observe failure**

Run: `go test ./... -run TestOIDCApprovalAuthenticator -count=1`

Expected: compilation fails because `NewOIDCApprovalAuthenticator` and the
approval-authentication types do not exist.

- [x] **Step 3: Implement the smallest verifier**

Add a constructor that rejects blank issuer/audience, calls
`oidc.NewProvider`, and builds `provider.Verifier(&oidc.Config{ClientID: audience})`.
Implement bearer extraction with an exact `Bearer ` prefix, call
`verifier.Verify`, reject an empty `IDToken.Subject`, and map every client
authentication failure to `ErrApprovalUnauthorized` without returning token
contents.

- [x] **Step 4: Add the dependency and verify**

Run: `go get github.com/coreos/go-oidc/v3@v3.20.0 && go mod tidy && go test ./... -run TestOIDCApprovalAuthenticator -count=1`

Expected: all OIDC verifier tests pass.

### Task 2: Protect workflow approval and remove client-supplied identity

**Files:**

- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/main.go`
- Modify: `examples/host-api/server_test.go`
- Modify: `examples/host-api/README.md`
- Modify: `examples/host-api/openapi.yaml`
- Modify: `docs/host-api-contract.md`

**Consumes:** `ApprovalAuthenticator` from Task 1.

**Produces:** a fail-closed `POST /workflows/{id}/approve` endpoint that writes
the verified subject to workflow approval metadata.

- [x] **Step 1: Write failing endpoint tests**

Add tests that the approval endpoint returns `401` with no authenticator or
no bearer token, that an OIDC token finalizes a waiting workflow, and that a
body field named `approved_by` cannot override the token subject.

- [x] **Step 2: Run the endpoint tests and observe failure**

Run: `go test ./... -run 'TestHostAPIOIDCApproval|TestHostAPIRejectsUnauthenticatedApproval' -count=1`

Expected: the old endpoint accepts the body field and does not return `401`.

- [x] **Step 3: Implement fail-closed handler wiring**

Add `ApprovalAuthenticator` to `Config` and `Server`. If it is nil, install a
rejecting implementation. Change `approveWorkflowRequest` to contain only
`Note`; use a strict decoder for this request. Authenticate before calling
`executor.Approve`, return `401 unauthorized` plus `WWW-Authenticate: Bearer`
on failure, and write `identity.Subject` into approval metadata.

In `main.go`, require `HOST_API_OIDC_ISSUER` and `HOST_API_OIDC_AUDIENCE`,
construct the verifier before starting the server, and pass it in `Config`.

- [x] **Step 4: Update the HTTP contract and verify**

Document the bearer requirement, the two environment variables, `401`, and
the request body containing only `note`. Remove `approved_by` from OpenAPI and
the prose contract. Run:

`go test ./... && go test -race ./... && gofmt -w approval_auth.go approval_auth_test.go main.go server.go server_test.go && git diff --check`

Expected: all host-api tests and race tests pass with no whitespace errors.

### Task 3: Workspace regression verification

**Files:** none beyond Tasks 1–2.

- [x] **Step 1: Run host-api smoke with required OIDC configuration documented**

Run: `go test ./...` from `examples/host-api`.

- [x] **Step 2: Run workspace verification**

Run: `bash ./scripts/verify-all.sh && git diff --check` from repository root.

Expected: the full multi-module suite passes; no unrelated files are staged or
committed.
