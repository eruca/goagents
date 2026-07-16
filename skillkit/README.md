# SkillKit

`skillkit` discovers and gates portable `SKILL.md` packages for a host
application. It is a catalog and availability module, not a plugin execution
runtime.

## Boundary

SkillKit can:

- scan explicit, host-configured roots for `<skill-name>/SKILL.md`;
- parse standard fields and the namespaced `metadata.goagents` extension;
- compute a stable package digest over `SKILL.md` and allowlisted resources;
- reject duplicate-name conflicts, invalid manifests, escaping resources, and
  untrusted roots;
- evaluate a skill against host OS, features, and already-authorized tools.
- activate an explicit, gated `name@digest` selection at run start and
  recheck package content before loading its instruction body;
- read an activated allowlisted resource through a path-free
  `skill://name@digest/path` URI.

SkillKit never executes `scripts/`, installs dependencies, performs network
requests, grants tools, or exposes local filesystem paths through catalog
entries.

## Use

```go
package main

import (
	"runtime"

	"github.com/eruca/goagents/skillkit"
)

func activateWorkspaceSkill() error {
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      "workspace",
		Dir:     ".agents/skills",
		Scope:   skillkit.ScopeWorkspace,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		return err
	}

	activation, err := catalog.Activate(skillkit.ActivationRequest{
		Skills: []skillkit.Ref{{Name: "clinical-summary"}},
		GateContext: skillkit.GateContext{
			OS:             runtime.GOOS,
			HostFeatures:   map[string]bool{"artifacts.v1": true},
			AllowedToolIDs: map[string]bool{"artifact.read": true},
		},
	})
	if err != nil {
		return err
	}

	skill := activation.Skills()[0] // Ref always includes its resolved digest.
	uri, err := activation.ResourceURI(skill.Ref, "references/output-schema.md")
	if err != nil {
		return err
	}
	_, err = activation.ReadResource(uri)
	if err != nil {
		return err
	}
	return nil
}
```

The host chooses roots and marks each root as trusted. A declaration such as
`required: [artifact.read]` is only a prerequisite: it does not make the tool
available unless the host has already registered and authorized it for the run.
The host should construct its request-scoped tool registry from the same
authorization decision; SkillKit does not register or remove tools.

## Agent adapter

The optional `agentadapter.Provider` maps a host-resolved activation to
`agentcore.SkillProvider`. Its resolver receives each `RunRequest`, so the
host can keep activation selection request-scoped:

```go
provider := agentadapter.Provider{Resolve: func(ctx context.Context, req agentcore.RunRequest) (*skillkit.Activation, error) {
	return activation, nil
}}
agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithSkillProvider(provider),
)
```

The adapter only injects already activated instruction bodies. It does not
implement `ToolProvider`, inspect `RunRequest.Metadata`, or change the
agent tool registry.

## `SKILL.md` extension

The portable `name`, `description`, `license`, and `metadata` fields remain
unchanged. SkillKit reads only the namespaced extension below:

```yaml
---
name: clinical-summary
description: Produce a bounded clinical summary.
metadata:
  goagents:
    requires:
      os: [darwin, linux]
      host_features: [artifacts.v1]
      tools:
        required: [artifact.read]
        optional: [web.search]
    resources:
      allow: [references/output-schema.md]
---
```

Unknown metadata is preserved. Resource references are forward-slash relative
paths; absolute paths, `..`, duplicate entries, and links escaping the skill
directory are rejected.

Instruction bodies are limited to 128 KiB at discovery. Resource reads return
at most 1 MiB; larger packages must be split into explicit, separately
allowlisted references. A changed package digest fails activation and resource
reads rather than silently loading a newer file.

## Current scope

SkillKit includes catalog discovery, availability evaluation, run-start
activation, bounded resource reads, and `agentcore.SkillProvider` wiring. The
host-api example adds safe catalog exposure, durable workflow `skill_refs`, a
single explicit local-root bootstrap, and a host-side evaluation gate. Remote
registries, dependency installation, request-scoped tool projection, dynamic
activation, and script sandboxing remain separate future slices.

## Verify

```bash
go test ./...
```
