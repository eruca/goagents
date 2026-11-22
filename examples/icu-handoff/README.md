# icu-handoff

`icu-handoff` writes an ICU shift handoff from a gocms server's patient
context tools (MCP at `POST /api/v1/mcp`, HTTP at `/api/v1/stays/{id}/context/…`). It is the first scenario of gocms' patient
context design (§10): gocms gives the facts, this program prompts the model,
runs one patient at a time, checks the answers and lays them out.

```bash
export GOCMS_AGENT_TOKEN=gca_...                    # gocms agent token
export OPENAI_COMPAT_BASE_URL=http://host:port/v1   # OpenAI-compatible endpoint
export OPENAI_COMPAT_API_KEY=...
export OPENAI_COMPAT_MODEL=qwen3.8-27b

go run ./examples/icu-handoff run --gocms http://127.0.0.1:9002 --ward ICU1 --out ./handoff-out
go run ./examples/icu-handoff run --gocms http://127.0.0.1:9002 --ward ICU1 --out ./dry --dry-run
go run ./examples/icu-handoff check --gocms http://127.0.0.1:9002 --patients S1il,S15mr --as-of "2026-11-22 08:00" --out ./check-out
```

Credentials are read from the environment only and are never written or
printed. `--dry-run` needs only `GOCMS_AGENT_TOKEN`.

## How `run` works

1. **The host lists the ward.** It calls `patients(ward, changes_hours,
   with_data)` over MCP for the census, beds, admissions and discharges.
2. **The host prefetches overviews.** `overview(patient, since, cite=true,
   budget=1000)` for every patient, four at a time (`--overview-concurrency`).
3. **One fresh model run per patient** (`--concurrency`, default 2, bounded
   by the model server's slots), built with `agentcore`:
   - system prompt: [`skills/icu-handoff/SKILL.md`](skills/icu-handoff/SKILL.md)
     (embedded; `--skill` overrides) plus the output format; user message: the
     patient's overview;
   - tools: `labs vitals drugs io scores alerts timeline expand` only, supplied
     per run through `agentcore.WithToolProvider`. Each wraps the
     `mcpkit.Tool` so that `patient`, `as_of` and `with_data` are removed from
     the schema the model sees and **the host sets `patient`** on every call,
     whatever the model sent. With `--tool-spec compact` (default) each tool
     is described in one sentence and keeps only its key parameters, without
     their descriptions (see [`handoff/compact.go`](handoff/compact.go));
     `full` sends gocms' own descriptions;
   - at most `--max-tool-calls` (3) tool calls. A repeated call (same tool,
     same arguments) is answered from the host's cache without asking gocms
     again, and still counts. Once the calls are used, the next model request
     carries **no tools** and ends with `工具调用次数已用完……请直接输出最终的
     JSON 对象。`, so the model has to answer; a call that still slips through
     returns `工具调用次数已用完，请根据已有信息作答。`
     (`WithMaxIterations(max-tool-calls + 2)`);
   - `--reasoning-effort` (default `none`) is sent as `reasoning_effort`;
     `--max-output-tokens` (4096) bounds reasoning plus answer per call;
   - structured output validated against the schema in
     [`handoff/output.go`](handoff/output.go) (a Markdown fence around the JSON
     is tolerated). An invalid answer is retried once with the error appended
     to the user message; a second failure marks the patient "生成失败" and the
     handoff shows the overview instead;
   - every gocms request carries `X-Agent-Run-Id: <handoff id>-<bed>`.
4. **The host verifies every point** against what the tools returned for that
   patient in this handoff (the prefetched overview and every tool result the
   model saw):
   - each reference (in `refs` or in the text) must appear in those returns,
     and gocms' HTTP expand must accept it for this patient (check letters,
     row exists, belongs to the patient);
   - every number in the text must appear verbatim in those returns (see
     below).
   Failing points are marked "未核实", never removed. If at least half of a
   patient's points fail, the run is redone once with the failures listed, and
   the rewrite is kept unless it is worse.
5. **Layout** by bed: ward header (now, shift start, data sync and staleness),
   this shift's admissions, transfers and deaths, then per patient the points
   (with "未核实" marks), "下一班要盯" (labelled as the model's own summary) and
   "拿不准的".

### Number whitelist

`handoff.Tokens` splits text into numeric tokens; a point passes when each
token is also a token of the tool returns:

- gocms references (`L8812345dk`) are removed first;
- a number glued to a preceding ASCII letter is part of a name and skipped:
  `SpO2`, `FiO2`, `q8h`, `IL-6`;
- dates (`2026-11-21`, `11-21`, `11月21日`) are compared whole as `MM-DD`;
  clock times (`05:40`, `5:40`) whole as `HH:mm`;
- `a-b` with an ASCII hyphen that could be a month and day passes as a date or
  when both numbers pass (`62-82` is a range);
- a leading `+` is dropped; `-` is a minus sign only when it follows neither a
  letter nor a digit (`RASS -2` vs `11-22`, `2.8-3.2`);
- anything else separates numbers: `300/0.5` is 300 and 0.5, `62–82` is 62 and
  82; decimals are compared as written (`2.80` does not match `2.8`).

## Output

`--out` receives:

- `handoff.md`: the handoff for people. Patient content; mode 0600.
- `handoff.json`: per patient the structured answer, each point's check
  (issues, missing numbers, failing references), retries and rewrites, and the
  overview when there is no usable answer. Patient content; mode 0600.
- `stats.json`: counts, durations, model and tool usage, verification rates by
  issue code, error codes (`output_invalid`, `output_truncated` when a call
  used its whole output budget, `empty_response`, `max_iterations`,
  `timeout`, `llm_http_<status>`, `llm_<provider class>`, `network`,
  `tool_<gocms code>`, `error_<Go type>`), per-attempt error codes, and model
  usage per round of a run. Failed patients are also logged on stderr by
  position and code. **It never contains patient text, beds or
  references**, so it is the file to read when running against real data.

`check` writes `checklist.md` and `checklist.csv` (one row per overview fact:
text fragment, value, unit, time, flag, reference, UI address, or the
replayable call for aggregates; for manual verification against the UI) and a
content-free `stats.json`.

## Tests

```bash
go test -race ./examples/icu-handoff/...
```

The tests start a fake gocms on `httptest` (the official MCP SDK server plus
the HTTP expand and overview endpoints) and a scripted model that calls tools
with another patient's reference, repeats a call, uses up its tool calls
(the next request must carry no tools), fails the schema
once, invents a number and a reference, and fails twice for one patient. They
assert the pinned patient and run ids at the server, the tool limit, the retry
and rewrite, the "未核实" marks, the layout, and that `stats.json` holds no
patient content.

`TestRunAgainstGocms` runs the whole flow against a live gocms with a model
that copies overview lines verbatim. It is opt-in and must only be pointed at
a server with fictional data:

```bash
ICU_HANDOFF_TEST_GOCMS=http://127.0.0.1:9002 GOCMS_AGENT_TOKEN=gca_... \
  go test -run TestRunAgainstGocms ./examples/icu-handoff/
```
