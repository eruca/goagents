# Module Example

This example wires a business module into the Agent.

A module is host-side glue. It groups the prompts, skills, and tools a host application already owns so the Agent can receive them through one option. It is not a dynamic plugin system.

The module provides:

- system prompt blocks
- skills
- request-scoped tools

Run it with:

```bash
go run ./examples/module
```
