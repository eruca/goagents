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

SkillKit never executes `scripts/`, installs dependencies, performs network
requests, grants tools, or exposes local filesystem paths through catalog
entries.

## Use

```go
package main

import (
	"fmt"
	"runtime"

	"github.com/eruca/skillkit"
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

	entry, err := catalog.Resolve(skillkit.Ref{Name: "clinical-summary"})
	if err != nil {
		return err
	}
	report := skillkit.Evaluate(entry, skillkit.GateContext{
		OS:             runtime.GOOS,
		HostFeatures:   map[string]bool{"artifacts.v1": true},
		AllowedToolIDs: map[string]bool{"artifact.read": true},
	})
	if report.State != skillkit.AvailabilityEligible {
		return fmt.Errorf("skill unavailable: %#v", report.Reasons)
	}
	return nil
}
```

The host chooses roots and marks each root as trusted. A declaration such as
`required: [artifact.read]` is only a prerequisite: it does not make the tool
available unless the host has already registered and authorized it for the run.

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

## Current scope

This first slice intentionally stops after catalog discovery and availability
evaluation. Resource resolution, `agentcore.SkillProvider` wiring, durable
workflow `skill_refs`, remote registries, dependency installation, dynamic
activation, and script sandboxing are separate future slices.

## Verify

```bash
go test ./...
```
