# Prompt Contract Design

## Goal

Make prompt assembly deterministic and easy to reason about before introducing any real LLM provider.

## Context

The Agent already supports static prompt blocks, system prompt providers, skills, module-provided prompts, memory messages, user input, and tool observations. `prompt.Compiler` deterministically orders prompt blocks, and `ThinkStage` sends the compiled prompt as the first system message.

This behavior is central to the library contract. Host applications need to understand what becomes model-facing context and in what order.

## Design

Keep the prompt model small:

- A prompt block is model-facing instruction text.
- Prompt blocks are not tools, memory, policy, or orchestration.
- Prompt blocks should not contain secrets or raw sensitive data unless the host intentionally wants them in model context.
- The default compiler is deterministic and dependency-free.

Pin the prompt block ordering contract:

1. `ModeCacheable` blocks sort before `ModeDynamic` blocks.
2. Lower `Priority` sorts earlier.
3. Same mode and priority sort by `Name`.
4. Empty block content is omitted from compiled content but the block remains in `Compiled.Blocks`.
5. Compiled content joins non-empty block content with `\n`.

Pin Agent prompt assembly:

1. Static `WithPromptBlocks` are combined with provider-supplied prompt blocks.
2. `SystemPromptProvider` blocks are loaded at most once per run.
3. `SkillProvider` skills are rendered as prompt blocks and loaded at most once per run.
4. `WithModule` can provide system prompts, skills, and tools together.
5. The compiled prompt is sent as the first `system` message when non-empty.
6. Memory messages are loaded before the current user input.
7. Tool observations are appended before the next LLM turn unless the result is silent.

## Example Shape

Add `examples/prompt` to print a compact message order from a mock LLM request:

- static prompt block
- module system prompt block
- module skill block
- current user input

The example should print roles and short labels only, not full large prompt text. It should stay deterministic and local.

## Non-Goals

- No template language.
- No token budget or compaction.
- No provider-specific prompt adapters.
- No prompt cache integration.
- No prompt versioning.
- No streaming.
- No secret management layer.

## Success Criteria

- Prompt ordering and assembly behavior is covered by tests.
- `examples/prompt` demonstrates the model-facing message order.
- README and prompt package docs explain the prompt contract.
- `make verify` passes.
