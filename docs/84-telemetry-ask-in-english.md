# R82 — Ask in English: SQL drafted from a question in the telemetry console

**Status**: designed 2026-11-01 after the NQ0 measurement spike (§12;
harness, question set and graded results in
[`assets/84/`](assets/84/results/grades.md)). Owner decision 2026-11-01:
**data mode B** (OD1, §2); OD2–OD5 decided 2026-11-02. NQ0b (a bug fix the
spike found) done 2026-11-02. NQ1–NQ4 implemented 2026-11-05 (§13); open:
NQ5, the owner pass.

**Relationship to earlier work**: R31 ([docs/36](36-telemetry-ui-history.md)
UD18) built the `#/sql` console over the DuckDB engine, with a read-only
statement allowlist, a 5,000-row and 30 s budget, and one isolated worker at
a time. R28 ([docs/33](33-telemetry-and-diagnostics.md) D11, §7) made
arbitrary SQL a deliberate act and put **no hosted SaaS export** on the list
of things telemetry never does. This milestone puts a question box above the
console's editor: the operator writes English, a model drafts the SQL, the
operator reads it and presses Run. It changes the `gawk-telemetry` read
surface only. Nothing on the wire, the relay or the clients changes.

---

## 1. Why, and what "done" means

The console answers anything the views can express, but only for an
operator who knows DuckDB and the store's shape: that times are epoch
milliseconds, that `series.capToRenderMs.median` is glass-to-glass latency,
that `browser` holds `Firefox 159` and also `gawk-broadcast-windows`. Most
questions the owner asks are not exact: *how does user experience compare
between Linux and Windows?* The answer is SQL, but you have to know which
five columns to read.

The model only drafts. **It never runs a query and never sees a row.** The
operator runs the draft, as they would run SQL they wrote themselves, under
the same allowlist and budget.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| A question in the console yields SQL in the editor and one sentence saying how the question was read | NQ3 component tests; NQ5 owner pass |
| Nothing from the store reaches the model: no rows, no row counts, no values | NQ2 test: the provider fake records every request, and none contains a value from a seeded fixture store (§4.2) |
| The model can check its draft against the real schema | NQ2: `check_sql` runs `EXPLAIN` in the shadow catalogue (D2) built from the learned schema; a binder error comes back to the model |
| Anthropic and any OpenAI-compatible server (Ollama, llama.cpp, vLLM) both work | NQ1 tests against recorded fakes of both APIs; NQ5 against a real Anthropic key and a real Ollama |
| Off by default; an install that does not enable it is unchanged | NQ4: chart renders byte-identical with `ask.enabled=false`; `GET /v1/query` reports `ask: false`; the UI shows no box |
| The question set keeps passing as models and the schema change | NQ0's harness re-runnable (`assets/84/harness/`); NQ5 re-runs it on the chosen model, at least 18 of 20 correct |
| Every column the glossary names exists | NQ2 test: each backticked identifier in the glossary resolves against the learned schema or the field catalogue |

---

## 2. Owner decisions (2026-11-01 and 2026-11-02)

| # | Question | Decision |
|---|---|---|
| OD1 | What may leave the homelab? A: the question and the schema. B: also DuckDB's error text from checking a draft. C: also result rows, so the model can look at the data. | **B** (2026-11-01). Rows never leave. A local model is the way to get C later (§6). |
| OD2 | Default Anthropic model | **`claude-sonnet-5-5`**, overridable per deployment with `-ask-model` (2026-11-02). Measured (§12.2): Opus 5.5, Sonnet 5.5 and Haiku 5.5 each got 19 of 20 right with the glossary, at about $0.028, $0.013 and $0.001 a question and a median of 10, 5.5 and 5 s. Haiku's misses were the dangerous kind, a confident wrong number (§12.3), and it was the one model whose answer changed on a question the glossary revision did not touch (q18). |
| OD3 | Who maintains the glossary? | **It lives beside the field catalogue in `internal/schema`**, and a PR that adds or changes a column documents it there (D6) (2026-11-02). |
| OD4 | Should the box also explain a result? That means sending rows to the model, so it is mode C. | **No: mode B only for now** (2026-11-02). A full agent that sees results may be considered later, as its own milestone. |
| OD5 | Is a local model a supported default or a supported option? The reference cluster has no GPU (§12.4). | **An option** (2026-11-02): `-ask-provider=openai` points at an Ollama (or other OpenAI-compatible server) the operator already runs. The chart does not ship one. |

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Server-side only.** A new `POST /v1/query/ask` on the read listener, behind the read listener's auth (basic or OIDC, docs/55). The SPA posts the question there and never talks to a provider. | The UI's no-external-fetch test (docs/36 UD7) stays true. The key stays in the pod. A provider is reachable only from where the read API already is. |
| D2 | **Mode B, enforced in one place, by construction.** The provider sees the system prompt (D5) and the conversation: the question, the model's drafts and `check_sql`'s answers. No other function feeds the conversation. `check_sql` runs `EXPLAIN <draft>` in a **shadow catalogue**, never against the store: an in-memory DuckDB holding an **empty** table per view, created from the learned column list and types (`learn.go`), with `enable_external_access = false` and `lock_configuration = true`. It returns `OK` or DuckDB's error text, truncated to 1,500 bytes. The draft must also pass `Check`. *(Revised 2026-11-02 after review on #677: the first version ran `EXPLAIN` against the real views.)* | OD1. Binding is not always data-free on the real views. A dynamic `PIVOT … ON col` with no `IN` list runs `SELECT DISTINCT col` to find its output columns, so a binder error's "candidate bindings" listed `"Linux"` on the deployment, and `OK` versus `ERROR` on `SELECT "Windows" FROM (PIVOT …)` answered whether a value exists. Table functions such as `read_json_auto` sniff files at bind time and can quote them in errors. The shadow catalogue has no row and no file, so there is nothing to leak whatever the syntax; refusing `PIVOT` by keyword would leave the next construct open. §12.6: on DuckDB 1.4.1, the deployment's version, it listed only column names for the same `PIVOT`, refused every file function, kept its configuration locked, and bound all 169 SQL statements the spike's models wrote. |
| D3 | **It drafts, it does not run.** The answer is `{interpretation, sql, checks, model}`. The UI puts the SQL in the editor and the interpretation above it. Running is the operator's Run button, under UD18's allowlist and budget, unchanged. | A wrong reading becomes visible before anything runs, and the allowlist stays the only thing between a statement and the engine. In the spike, the dangerous failure was SQL that ran and returned a confident, impossible number (§12.3). Only a human reading the result catches that. |
| D4 | **Two provider kinds behind one interface.** `anthropic`: the Messages API through `anthropic-sdk-go`, with tool use for `check_sql`, prompt caching on the system prompt, and `fallbacks: "default"` for refusals (not sent for Haiku, which has no
server-side fallback). `openai`: hand-written HTTP to `POST {base}/v1/chat/completions` with function tools, which Ollama, llama.cpp's server, vLLM and LM Studio all serve. | One OpenAI-compatible client covers every local server worth naming, so no provider-specific code is needed for each. The Anthropic SDK (`anthropic-sdk-go/v2`) is Apache-2.0; the licence gate (`ci.yml`) passed with it in NQ1 (§13). The hand-written client keeps the module free of a second SDK. |
| D5 | **The prompt is generated from what the server already serves, plus a glossary.** The system prompt holds the rules (one read-only statement, check before answering, state the interpretation, time or category column first), today's date, each view's `ViewDoc`, the **learned** column list and types (`learn.go`), the field catalogue (`internal/schema`, skipping legacy fields), and the glossary (D6). It is byte-stable between questions, so it caches. | No second copy of the schema (docs/36 D15's lesson). The learned list is what the views actually bind to today. Cached, the 17k-token prefix costs a tenth of its price after the first question in five minutes (§12.2). |
| D5a | **Two prompt sizes.** `full` is D5 as written (about 17.5k tokens). `slim` describes only `rollups`' columns, names the other views without their columns, and keeps the glossary (about 3.2k). `-ask-context` picks one; the default is `full` for `anthropic` and `slim` for `openai`. | §12.5: slim turned one correct answer in twenty into a partial one for both Sonnet and Haiku, and is a fifth as long to read. On a CPU, reading the prompt is the whole wait (§12.4). |
| D6 | **A curated glossary, checked against the schema.** `internal/schema/glossary.md`, embedded. It explains how fields fit together: role values, what `browser` holds (including the native distributions), `os` values, epoch-ms times, partition pruning, which counter means loss and which denominator to use, where viewer counts live, the list-typed `verdict.findings`. **It states only enumerations the code defines, never values sampled from the store.** A test fails when a backticked identifier in it no longer resolves. | §12.2: without it, Sonnet got 14 of 20 right, with or without the catalogue. All six misses came from not knowing domain facts, not from writing bad SQL. With it, 19 of 20. Sampling `SELECT DISTINCT os` into the prompt would teach the same facts, but it is data leaving the homelab, which OD1 rules out. |
| D7 | **Bounded.** At most 4 `check_sql` calls a question, 120 s overall, one question at a time per operator, and 30 questions per operator per hour (both configurable). `check_sql` runs in D2's shadow catalogue, not in the console's query worker, so it never queues behind a console query or adds to the worker's budget. | An `EXPLAIN` over empty tables is cheap, but each question still costs provider tokens and the operator's attention. In the spike, 155 of 164 Anthropic answers checked once and none more than three times (§12.2); the cap is for models that loop. |
| D8 | **Off by default, every knob plumbed.** Flags `-ask-provider` (`none`, `anthropic`, `openai`; default `none`), `-ask-model` (default `claude-sonnet-5-5` for `anthropic`; required for `openai`), `-ask-base-url`, `-ask-api-key-file`, `-ask-context`, `-ask-timeout`, `-ask-max-checks`, `-ask-rate`. Each has a `GAWK_TELEMETRY_ASK_*` env var and a Helm value under `ask:`, with the key from a Secret (`ask.apiKeyRef`). `GET /v1/query` gains `ask: {enabled, provider, model}`. The box needs both an engine and a provider, and says which one is missing when either is. | The repository's rule that a knob reaches production or does not exist. UD18's split between "no engine here" and "your query was wrong" carries over to "no model configured" versus "the model could not answer". |
| D9 | **Logged without results.** Each ask logs the operator (the OIDC subject or the basic-auth user), the provider and model, the question, the final SQL, check count, tokens and duration, at `info`. Nothing is stored. | The question is the operator's own text and may name a session ID; it goes to the same log as everything else on the read listener. There are no rows to log. |
| D10 | **MCP is unchanged.** `query_sql` stays as it is. | An MCP client such as Claude Code already does English → SQL **and** sees the rows. That is the operator choosing mode C for themselves, with their own client and key. This milestone is the in-console path for B. |

---

## 4. What changes where

### 4.1 Server (`gawk-telemetry/`)

- `internal/ask/` (new): `Provider` interface (`Draft(ctx, system, question,
  check func(sql) string) (Answer, error)`); `anthropic.go` (D4, SDK);
  `openai.go` (D4, hand-written); `prompt.go` (D5, built once per learned
  schema change and cached); `ask.go` (the loop, D2's single conversation
  feed, D7's bounds).
- `internal/schema/glossary.md` + `glossary.go` (D6, embedded, with the
  identifier test).
- `internal/readapi/query.go`: `POST /v1/query/ask`, and the `ask` field on
  `QueryStatus` (D8).
- `internal/sqlengine`: an `Explain(sql)` that runs `Check` and then
  `EXPLAIN` in D2's shadow catalogue, rebuilt when the learned schema
  changes, returning `OK` or the error text and nothing else. It never
  touches the store or the query worker.
- `cmd/gawk-telemetry/main.go`: the flags and env vars (D8).
- `deploy/charts/gawk-telemetry`: `ask.*` values, the Secret ref, and the
  env wiring; off renders byte-identically.

### 4.2 The mode B test

A fixture store with marker values that cannot come from the schema (a
browser called `ZZMARKER 1`, a session ID `feedfacefeedface…`, a
`durationMs` of `987654321`). The test drives three questions through a
recording fake provider whose drafts include a valid query, a binder error,
a query that would return the markers if it ran, a dynamic `PIVOT` on the
marked column with a deliberately wrong column name, a `SELECT
"ZZMARKER 1" FROM (PIVOT …)` whose `OK` would confirm the value, and a
`read_json_auto` over the data directory. It asserts that no recorded
request contains a marker, and that the two `PIVOT` drafts get the same
answer as for an unmarked value. The test belongs to NQ2 and is written
first.

### 4.3 UI (`gawk-telemetry/ui/`)

- `SqlView.tsx`: an **Ask** field above the editor, shown only when
  `status.ask.enabled`. Enter asks; the editor gets the SQL and a line above
  it reads *"Read as: …"* with the model's name. Asking again replaces the
  draft; the previous editor contents are kept in the existing history. It
  never runs the query by itself.
- `api/client.ts`: `askQuery(question)`.

## 5. Risks and limits

- **A draft can run, look plausible and be wrong.** In the spike, Haiku
  divided by `decodedFrames` alone and reported a macOS "average loss ratio"
  of 484 (§12.3). Mode B means no model can catch that. The guards are the
  glossary's denominator rule (D6) and the interpretation line, but the
  operator still has to read the result.
- **The data itself can mislead.** `series.receivedFps.median` is 0 for
  most viewer sessions (6,379 of 6,398 have samples; the median of their
  medians is 0, the 90th percentile 30), and per-session medians of
  `capToRenderMs` run to seconds. This spike did not establish why. Every
  model reported those numbers faithfully. Which fields can be trusted for
  which question is a telemetry question, not an AI one; NQ2 settles it
  with the owner before the glossary says anything about it.
- **The question leaves the homelab** in Anthropic mode. It is the
  operator's own text; the console says so under the box when the provider
  is not in-cluster.
- **Column names are not all the code's.** Public ingest keeps an unknown
  stats key verbatim (docs/36 D15), and the learner records it as a field of
  `sessions.stats`. Fed to the model, a client-written "column name" could
  be an instruction, in every question's prompt and in binder errors. The
  shadow catalogue, and so the prompt, keeps only identifier-shaped names,
  under `sessions.stats` only the fields the schema tables define, and at
  most 128 fields a struct and four levels (PR #706 review). A draft that
  reads an uncatalogued stats key fails its check, and the console still
  runs it.
- **A draft is SQL the operator runs with the console's own reach.** The
  console's engine keeps external file access on (it reads the NDJSON), so
  any statement run from it — written by hand or drafted — can read files
  the pod can: the mounted Ask key, `/proc/1/environ` with the telemetry
  key. That is not new with R82 (a read-listener operator could always
  type it), but a drafted statement is one the operator did not write.
  The guards are the operator reading the draft before Run, and the
  read listener's auth. Narrowing the console's file access to the data
  directory (DuckDB's `allowed_directories`) is the fix and its own change.
- **Interpretations differ.** "Last 5 days" was read as the trailing 120
  hours by the glossary runs and as from midnight five days ago by others
  (67 versus 74 sessions). Both were stated; the line above the editor is
  where the operator sees it.
- **Local models are slow on CPU and less accurate at 7B** (§12.4). With
  the full prompt there was no answer in 30 minutes; slim answered in about
  30 s once warm but got 3 of 5 right, and never used `check_sql`.
- **The engine failed on NaN and ±Inf results** (fixed in NQ0b): the
  worker could not encode them and exited with no message, so a drafted
  ratio without `nullif` answered "worker failed". They now come back as
  the strings `"NaN"`, `"Infinity"` and `"-Infinity"`.

## 6. Out of scope

Running the query for the operator. Explaining or charting a result in
words (OD4, mode C). Sampling values from the store into the prompt.
Conversation (follow-up questions refining a draft) — each ask stands
alone in v1. An Ollama in the chart. Questions over `annotations`.

---

## 9. Chunks

Every letter is claimed; this milestone's prefix is **NQ**.

| Chunk | What | Acceptance |
|---|---|---|
| **NQ0** ✅ | **Measurement spike**: `docs/assets/84/harness/` (20 questions with reference SQL and grading notes, prompt builder, mode B check loop, both provider kinds), run against the reference deployment. | §12 filled; `results/grades.md` from the runs; the harness re-runs with `q.sh` pointed at any read listener. |
| **NQ0b** ✅ | **Prerequisite fix**: `BUGS.md`'s NaN/±Inf worker failure, test first. Not part of this feature; the console has the bug today. | `SELECT 1/0`, `'nan'::DOUBLE` and `'inf'::DOUBLE` return a result through the isolated engine and through `/v1/query`; the `BUGS.md` entry is removed. Done 2026-11-02: non-finite floats (also inside LISTs and STRUCTs) answer `"NaN"`, `"Infinity"` or `"-Infinity"`, distinct from NULL; a `MAP` result, which failed the same way, answers an object keyed by each key's text; a result the worker still cannot encode says why. |
| **NQ1** ✅ | Providers: D4. | Tests against recorded request/response fixtures of both APIs: tool call round trip, the 4-check cap, a refusal, a 429 retry, a timeout, a malformed tool argument. The licence gate passes with `anthropic-sdk-go`. |
| **NQ2** ✅ | The ask loop, the prompt and the glossary: D2, D3, D5–D7, D9. `sqlengine.Explain`. | Tests first: §4.2's mode B test; the glossary identifier test; the prompt is byte-stable between two questions; a binder error reaches the model and a fixed draft is returned; the per-operator rate limit; `Explain` refuses what `Check` refuses, never returns rows, and runs with file access off and its configuration locked; the shadow catalogue's columns and types equal the learned schema's, and it is rebuilt when that changes. |
| **NQ3** ✅ | UI: §4.3. | Component tests: the box hidden without `ask.enabled`, the "no engine" and "no model" messages distinct, the draft lands in the editor with its interpretation, nothing runs on its own, an error from the model renders as prose. |
| **NQ4** ✅ | Flags, env and chart: D8. | `main_test` covers every flag and env var; the chart renders byte-identically with `ask` off and renders the Secret ref when on; `GET /v1/query` reports the provider. |
| **NQ5** | **Owner pass** on the reference deployment: the 20 questions through the console with the chosen Anthropic model, then a handful against an Ollama on hardware with a GPU. | The chosen model gets at least 18 of 20 right (graded as in §12.1); the owner's own new questions recorded in §12 with their outcomes. |

---

## 12. Results (NQ0, 2026-11-01)

Harness and grading in [`assets/84/`](assets/84/results/grades.md). The
store: the reference deployment's 6,881 rollups (2026-07-27 to
2026-11-01), plus the `sessions` and `relay` partitions, through the
deployed `gawk-telemetry` 1.14.2's `/v1/query`. Mode B as D2 has it: the
model's only tool was `check_sql`, which ran `EXPLAIN` on the deployment.
The harness then ran the final SQL itself and recorded the rows, for grading
only; they were never shown to a model and are not committed.

### 12.1 Questions and grading

Twenty questions (`harness/questions.json`): the owner's four, verbatim,
and sixteen more of the same kind, from exact (*how many viewer sessions
reconnected more than 3 times?*) to vague (*are Safari viewers worse off
than Chrome viewers?*). Each has reference SQL that runs, and a grading
note. **Correct** means the SQL answers a sensible reading of the question
and the numbers are right for that reading. **Partial** means the answer is
usable but has a flaw a careful reader would catch (a missing role filter,
an unusable chart axis). **Wrong** means the result does not answer the
question or is not true.

Context levels: **schema** (view docs and the learned columns of
`rollups`, `sessions` and `relay`, about 9k tokens), **catalogue** (+ the
field catalogue, about 16k), **glossary** (+ D6's glossary, about 17k),
**slim** (only `rollups`' columns + the glossary).

### 12.2 Accuracy, cost and time

| Model | Context | Correct | Partial | Wrong | $ / question | Median s (p90) |
|---|---|---|---|---|---|---|
| Claude Opus 5.5 | glossary | 19 | 1 | 0 | 0.028 | 10.2 (14.6) |
| Claude Sonnet 5.5 | glossary | 19 | 1 | 0 | 0.013 | 5.5 (8.4) |
| Claude Haiku 5.5 | glossary | 19 | 0 | 1 | 0.001 | 4.8 (6.9) |
| Claude Haiku 5.5 | glossary, revised (§12.3) | 19 | 1 | 0 | 0.001 | 4.4 (8.4) |
| Claude Sonnet 5.5 | slim | 18 | 2 | 0 | 0.009 | 6.0 (8.6) |
| Claude Haiku 5.5 | slim | 18 | 1 | 1 | 0.001 | 4.3 (9.8) |
| Claude Sonnet 5.5 | catalogue | 14 | 3 | 3 | 0.013 | 6.8 (9.3) |
| Claude Sonnet 5.5 | schema | 14 | 3 | 3 | 0.012 | 7.3 (11.1) |

Effort `medium` throughout. Cost is from the API's token counts at list
prices, with the system prompt cached. Each question made two requests, and
the cache write paid once per five minutes is not included. Haiku's
cache-read rate was taken as a tenth of its input price. The prompt was
about 17.5k tokens a request with the glossary, 9k with the schema alone
and 3.2k slim. Times include `check_sql`'s round trip to the deployment,
whose engine runs one query at a time and was shared by up to five runs.
Every answer and its grade: [`results/grades.md`](assets/84/results/grades.md).

`check_sql` mattered less than expected. Of 164 Anthropic answers, 155
checked once and 9 needed a second or third check, 8 of them Haiku's (four
on the slim prompt, where it had to guess `relay`'s and `sessions`'
columns). The models write DuckDB that binds. What they lack is knowledge of what the values mean,
which no check can supply.

### 12.3 What went wrong

- **No glossary, no domain facts.** All six misses at the schema and
  catalogue levels came from domain knowledge. *By browser family* grouped
  by `Firefox 158`, `Firefox 159` and so on (q07, twice), and Safari against
  Chrome compared by version (q20, partial, twice). The Windows native
  broadcaster was looked for in `app.surface` or in native-only config
  fields and found nothing (q09, twice: 0 rows). *Native broadcasters* was
  read as sessions whose surface is `native` (q17, schema: 0 rows). Viewer
  comparisons left out `role = 'viewer'` and included broadcasters (q04,
  q16, partial, twice each). The catalogue fixed q17 (its uplink counters
  are broadcaster-only) and nothing else.
- **A ratio with the wrong denominator.** Haiku with the glossary divided
  `framesDroppedIncomplete` by `decodedFrames` alone. Sessions that decoded
  almost nothing produced a macOS "average loss ratio" of 484 (q03). It
  ran, it looked like an answer, and in mode B nothing but the operator can
  see it is impossible. Sonnet and Opus added the numerator to the
  denominator.
- **Time on a chart axis.** `sessions.tMs` is milliseconds since the session
  started. Sonnet with the glossary plotted it as an epoch (`1970-01-01`
  ticks, partial). Opus plotted `receivedAtMs`, which is the batch's
  arrival, so 2,821 samples sat on 644 x values (partial). With
  the catalogue, Sonnet added `startedAtMs`, which is null on sample lines,
  and got no axis at all (wrong).
- **Revising the glossary fixed both, and was re-measured.** Three
  additions: a loss ratio's denominator includes its numerator; what
  `tMs`, `receivedAtMs` and `startedAtMs` are; and the `browser` and `os`
  values taken from the web app's `describeClient` rather than from the
  data (`Opera`, `ChromeOS`, `unknown`, iPads as `macOS`). Re-run, Haiku
  got q03 right (0.395 on macOS), and Sonnet and Opus got q19 right
  (`tMs / 1000.0` seconds). Haiku's full re-run kept 19 of 20 but lost q18,
  which it had right the first time: it reported one broadcast's peak
  `viewersGlobal` (4) instead of the fleet-wide sum (8). Nothing it reads
  about q18 changed, so this is run-to-run variance; a glossary line saying
  to sum across broadcasts is the obvious next fix. The first-run rows above
  used the first glossary.
- **One reference was wrong.** q18's first reference SQL also took a single
  broadcast's peak. The Sonnet and Opus answers summed per minute across
  broadcasts, which is right. The reference and its grading note were
  corrected during grading (`questions.json` records it). No grade relied on
  the first reference: answers were graded against the note's reading of
  the question, not by matching the reference's numbers.

### 12.4 Local model

The reference cluster's three nodes have no GPU (24, 32 and 16 cores). The
spike ran Ollama 0.12.6 with `qwen2.5-coder:7b` (Q4, 32k context, CPU only)
in Docker on the 8-core development host, which other work was sharing
(load average about 9). The timings below are an upper bound for that
hardware class, not a measurement of the cluster's nodes.

- **Full context (17.5k tokens): no answer.** The first request ran into the
  harness's 30-minute timeout before the model wrote a token.
- **Slim context (3.2k tokens, revised glossary), five questions**: the
  first took 24 minutes. After that Ollama reused the cached prompt and
  read only the question: 19 to 46 s each. It got 3 of 5 right (q01, q07,
  q11). It skipped the role filter on q03, which averaged a 0/0 to NaN, and
  that crashed the deployment's query worker (fixed in NQ0b; the bug was the
  engine's, not the model's). On q09 it parsed a version out of `browser`
  instead of reading `appVersion`. Claude on the same five slim questions
  got all five right (Sonnet) and four (Haiku).
- **It never called `check_sql`.** The model always answered in one turn,
  so D2's check loop bought it nothing.

So a local model is workable only with D5a's slim prompt, a warm cache, and
hardware better than a shared CPU. Its accuracy at 7B is well below
Claude's. That is why OD5 makes it an option the operator points at their
own hardware, not a default. NQ5 measures it again on a GPU and tries a
larger model there.

### 12.5 Slim context

Slim describes only `rollups`' columns and keeps the glossary (3.2k tokens
against 17.5k). The runs used the first glossary. Sonnet: 18 correct, 2
partial. Haiku: 18 correct, 1 partial, 1 wrong. The answers that needed the
raw views still found them. q18 went to `relay`, and q19 to `sessions` with
a date filter, using the glossary's mentions of those columns and
`check_sql` (Haiku needed three checks for q18). What slim cost was q18 for
Sonnet: without `relay`'s columns, it counted overlapping viewer rollups,
which gives 12 where the relay's own count says 8. For Haiku it was q19's
axis (`tMs` as an epoch, the mistake the revised glossary addresses), which
it had right with the full context. Haiku's slim q03 was the
same wrong ratio as with the full context, and the revised glossary fixes
that (§12.3).

### 12.6 Checking without reading: the shadow catalogue (2026-11-02)

The review on #677 pointed out that `EXPLAIN` against the real views is not
data-free. Confirmed on the deployment: `EXPLAIN SELECT nosuchcol FROM
(PIVOT rollups ON os USING count(*))` answered with candidate bindings
including `"Linux"`, a value, and `EXPLAIN SELECT "Windows" FROM (PIVOT …)`
succeeded. The spike's harness had the same flaw. None of its 169 final
statements used `PIVOT` or a file function. Intermediate drafts were not
recorded, so that cannot be shown for them, but 155 of the 164 Anthropic
answers checked only once, which means their checked draft was the final
statement; the other 9 are the only drafts unaccounted for.

`harness/shadow.py` builds D2's shadow catalogue in DuckDB 1.4.1 from the
spike's schema snapshot and re-checks everything:

| Probe | Real views | Shadow catalogue |
|---|---|---|
| `SELECT nosuchcol FROM (PIVOT rollups ON os …)` | candidates include `"Linux"` | column names only |
| `SELECT "Windows" FROM (PIVOT rollups ON os …)` | `OK` | binder error, as for any name |
| `read_csv`, `read_json_auto`, `glob` on a path | allowed by `Check`; would read at bind (not probed on the deployment) | `Permission Error`, file access disabled |
| `SET enable_external_access = true` | (refused by `Check`) | refused: configuration locked |
| the spike's 169 final statements | 168 ran; one (the local model's q03, §12.4) bound but failed at run time on the NaN result (fixed in NQ0b) | all bind |

---

## 13. Implementation (NQ1–NQ4, 2026-11-05)

Where the build differs from, or adds to, §3 and §4, and which test holds
each acceptance row of §9.

| Chunk | Where | Verified by |
|---|---|---|
| NQ1 | `internal/ask/anthropic.go`, `openai.go` | `anthropic_test.go`, `openai_test.go`: a tool call round trip against recorded shapes of both APIs (the thinking block replayed with its signature, every tool result in one user turn, the system prompt cached, `fallbacks: "default"` with its beta header, the tool `strict`), the 4-check cap, a refusal with its category, a 429 retried, a 400 not retried, a timeout, a malformed tool argument, arguments sent as an object (Ollama). `gen-notices.py --check` passes with the SDK. |
| NQ2 | `internal/ask/ask.go`, `prompt.go`; `internal/sqlengine/shadow*.go`; `internal/schema/glossary.*` | `modeb_duckdb_test.go` is §4.2's test, against the real engine: three questions, six drafts that would leak if checked against the store, no marker in any recorded request beyond what the model wrote itself, and the `PIVOT` verdict the same for a stored value as for an absent one (checked to fail with a checker that runs the drafts). `shadow_duckdb_test.go`: the shadow tables' columns and types equal each real view's `DESCRIBE`; a column the store gains binds at the next check; the configuration is locked; file functions are refused; a check does not queue behind a console query; `Explain` refuses what `Check` refuses without starting a worker. `ask_test.go`: a binder error reaches the model and its fix is returned; the prompt is byte-stable between questions and follows the learned schema; the per-operator hourly limit and one question in flight; the D9 log line. `glossary_test.go`: every backticked name resolves against the stored shape (`storedshape`'s golden) or the catalogue, every value it names is one the code defines, and a doctored glossary fails. |
| NQ3 | `ui/src/views/SqlView.tsx`, `api/client.ts` | `SqlView.ask.test.tsx`: hidden with no provider and on an older server; "no model" and "no engine" say different things; the draft lands in the editor with its *Read as:* line and the model's name; nothing runs; a draft that does not bind says so; a model error renders as prose. |
| NQ4 | `cmd/gawk-telemetry/main.go`, the chart's `ask:` values | `ask_test.go` (main): every flag and env var, flag over env, the defaults, each unusable configuration refused at start, no box without an engine. CI's *telemetry chart Ask box* step: nothing renders with the box off (also checked byte-identical against the previous chart locally), the key mounts from its Secret, a bad provider fails the render. `readapi/ask_test.go`: `GET /v1/query` reports `ask: {enabled, provider, model, …}`. |

Decisions taken while building, none of which changes an owner decision:

- **The check runs in a worker of its own.** D2 left open where the shadow
  catalogue lives. The service never opens DuckDB itself (`docs/gotchas.md`,
  the allocator), so each check re-executes the binary like a console
  query, but through its own one-at-a-time slot (D7: it never waits behind
  a console query), with no data directory, and with an **empty
  environment**: nothing in the service's environment is within reach of a
  function that reads one. The parent reads the learned schema and hands
  the worker only the `CREATE TABLE`s and the statement. Building the
  catalogue per check also makes "rebuilt when the learned schema
  changes" automatic.
- **Partition keys come after the file columns, in name order.** The real
  `sessions` view ends `broadcast VARCHAR, date DATE`; the first shadow
  build had them the other way round, and the `DESCRIBE` comparison caught
  it.
- **The final SQL's verdict goes to the operator, not the model.** The
  answer carries `check`: `OK`, or the error Run would meet. When the model's
  last check was of the final statement it is reused; otherwise the final
  statement is checked once more and the verdict is never sent back. The
  console shows a draft that does not bind under its *Read as:* line.
- **A draft the console would refuse is not offered.** The final SQL must
  pass `Check`; a `COPY` or two statements come back as "no usable draft".
- **`GET /v1/query`'s `ask` is an object, not a boolean.** NQ4's table says
  `ask: false`; D8 says `{enabled, provider, model}`. The object won, with
  `provider: "none"` when off, `reason` when a configured box cannot work,
  and `external`/`endpoint` for the line under the box. A host is in-cluster
  when it is loopback, private, single-label or ends in `.svc` or
  `.cluster.local`; a bare `name.namespace` reads as outside, the safe
  default, so spell an in-cluster Ollama with `.svc`.
- **Operators are keyed `oidc:<sub>` or `basic:<user>`,** or `anonymous`
  on an open listener (where every operator shares one budget).
- **The key is a file from a Secret** (`ask.apiKeyRef`, mounted read-only),
  never an env var, because the SQL workers start with the service's
  environment. §5 has the limit of that.
- **The SDK is v2** (`github.com/anthropics/anthropic-sdk-go/v2`, Apache-2.0,
  not MIT as D4 said) with its environment lookup off
  (`WithoutEnvironmentDefaults`), so `ANTHROPIC_*` variables on a pod change
  nothing. It sends the key as a bearer token and API version `2026-10-21`.
  It brings 18 transitive modules into the image (`THIRD-PARTY-NOTICES.md`);
  the cgo-free build links them too, since `internal/ask` has no build tag.
- **The glossary gained the line §12.3 called the obvious next fix**: the
  fleet's viewers at a moment are one `viewersGlobal` per broadcast, summed.
  It has not been re-measured; NQ5's run of the question set covers it.
- **The glossary test reads the code that defines its values** —
  `gawk-app/src/lib/telemetry.ts` (`describeClient`), the desktop engine's
  distributions and the iOS identity — and the telemetry CI job now runs on
  changes to those three files. The removed Go broadcaster's
  `gawk-broadcast` is allowed by name: no code writes it, and old rollups
  keep it.
- **Retry tests count attempts at the client.** Go's transport resent a
  POST on a reused connection one run in a few under `-race`, so a fake
  server's request count is not the client's (`docs/gotchas.md`).

