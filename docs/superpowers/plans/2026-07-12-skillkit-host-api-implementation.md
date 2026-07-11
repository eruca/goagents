# Skillkit Host API Implementation Plan

> For agentic workers: use superpowers:executing-plans and track each checkbox.

**Goal:** Expose safe skill discovery in host-api, persist resolved name@digest refs in workflow metadata, and reactivate the same refs after restart or approval resume.

**Architecture:** Config owns a prebuilt SkillKit catalog and GateContext. HTTP never scans roots or grants tools. Creation resolves and gates refs before storing JSON-safe metadata; routingAgentRunner rebuilds activation from RunRequest or checkpoint metadata and passes it through agentadapter.Provider.

**Tech Stack:** Go 1.26.1, examples/host-api, workflowkit/sqlitestore, skillkit.

## Global Constraints

- GET /skills returns only name, description, digest, scope, availability and stable reasons.
- skill_refs accepts objects with name and optional digest. Persisted refs always include the resolved digest.
- Missing catalog, unknown ref, unavailable requirement, changed or missing digest all fail closed.
- No agentcore change, script execution, dynamic activation or tool-registry projection.
- Workflow metadata stores only an array of maps with name and digest.

---

### Task 1: Safe GET /skills

Files:

- Modify: examples/host-api/server.go
- Modify: examples/host-api/server_test.go
- Modify: examples/host-api/go.mod and go.sum

- [ ] Write TestListSkillsReturnsSafeAvailability with eligible, unavailable and invalid catalog entries. Assert no root path, manifest body or resource path appears.
- [ ] Run: cd examples/host-api && go test ./... -run TestListSkillsReturnsSafeAvailability -count=1. Expected RED: route absent.
- [ ] Add Catalog and GateContext to Config and Server, a safe response DTO, and GET /skills. A nil catalog returns an empty list and never scans disk.
- [ ] Repeat the focused test. Expected GREEN.

### Task 2: Validate and persist workflow skill_refs

Files:

- Modify: examples/host-api/server.go
- Modify: examples/host-api/server_test.go

- [ ] Write TestCreateWorkflowPersistsResolvedSkillRefs. A digest-less unique ref resolves to its full digest; after reopening SQLite, GET workflow returns the same ref. Cover missing catalog, unknown ref and unavailable required tool as 400 responses.
- [ ] Run: cd examples/host-api && go test ./... -run TestCreateWorkflowPersistsResolvedSkillRefs -count=1. Expected RED: request and response lack skill_refs.
- [ ] Add request and response DTO fields plus resolveSkillRefs. Resolve and Evaluate each ref, then store only complete refs under metadata key skill_refs.
- [ ] Repeat the focused test. Expected GREEN.

### Task 3: Rebuild activation for run and resume

Files:

- Modify: examples/host-api/server.go
- Modify: examples/host-api/server_test.go

- [ ] Write TestWorkflowSkillRefsActivateSameDigestAfterRestart. Create a waiting-approval workflow with a Skill, reopen the host, resume it, and assert the model prompt receives the Skill body from the persisted digest. A missing digest must abort before a model call.
- [ ] Run: cd examples/host-api && go test ./... -run TestWorkflowSkillRefsActivateSameDigestAfterRestart -count=1. Expected RED: routingAgentRunner ignores skill_refs.
- [ ] Add catalog and gate context to routingAgentRunner. Decode refs from normal requests and checkpoints; activate them and use agentadapter.Provider when building each Agent. Keep no-ref execution unchanged.
- [ ] Repeat the focused test. Expected GREEN.

### Task 4: Documentation, process smoke and semantic commit

- [ ] Update examples/host-api/README.md with GET /skills, workflow skill_refs, fixed-digest replay and failure-closed semantics.
- [ ] Run module tests, race tests, host-process smoke, bash ./scripts/verify-all.sh and git diff --check.
- [ ] Commit only host API files, dependency metadata and this plan as feat(host-api): 持久化并激活技能引用.

## Plan Self-Review

- The host exposes catalog state but never package content or paths.
- The persisted digest, not a current name lookup, controls restart and resume.
- Tool projection remains an explicit later host policy slice because current agentcore ToolProvider only appends tools.
