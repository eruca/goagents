# ContextKit Design

## Goal

`contextkit` is a standalone Go module for context compression. It lives next to
`goagent` and `ocrs` and can be used by agent hosts or non-agent programs.

## Name And Environment

The module name is `contextkit` and the module path is `github.com/eruca/goagents/contextkit`.

The runtime profile is controlled by `CONTEXT_DEEP_COMPRESSION`:

- unset or disabled: enable levels 1-3.
- `1`: enable levels 1-5.

This name is intentionally tied to the behavior rather than the implementation
package name.

## Five Levels

1. Tool result budget: truncate large tool-visible content before it enters the
   model projection.
2. Message window pruning: keep system messages and recent conversational
   messages under a deterministic budget.
3. Projection: return a compressed model-view without destroying the original
   host session.
4. Reversible collapse: in deep mode, record collapsed original messages with a
   stable collapse ID, original IDs, originals, and summary placeholder.
5. Auto compact: in deep mode, call a host-owned summarizer when configured and
   place the stronger summary into the model projection.

## Boundaries

`contextkit` does not import `goagent`. Applications map `goagent` messages into
`contextkit.Message`, call a compressor, then use the returned projection for
model requests or memory compaction.

Future `goagent` core work may add a small runtime port before `ThinkStage`, but
the concrete algorithms remain in this module.
