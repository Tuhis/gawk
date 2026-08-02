# The gawk-telemetry dashboard

A tour of the read-side UI. How to reach it (port-forward, never public)
is in the [README](../README.md); the design decisions are in
[docs/33](../../docs/33-telemetry-and-diagnostics.md) and
[docs/36](../../docs/36-telemetry-ui-history.md).

## The landing view

The live page answers *"is anything wrong right now?"* and nothing else.
It lists every live broadcast with its broadcaster **and** each viewer,
with anything obviously wrong highlighted before you click anything.
Severity — not recency — sorts the live group; recently ended broadcasts
sit in a separate recessed group below, carrying their stored verdicts in
the past tense.

Four states: **ok · warn · bad · unknown**. A viewer whose telemetry
stopped reads *stale*; one that never reported reads *unknown*. Neither is
ever `ok` — painting an absence of evidence as green is the one thing an
ops dashboard must not do.

No external asset fetch: the page works on a port-forward from a laptop
with no network, and a test asserts that against the built bundle.

## The sections

Every section is addressable, so a link is a whole answer:

| Route | Answers |
|---|---|
| `#/` | the live fleet |
| `#/session/<id>` | everything known about one session, from disk, full resolution, from its first sample — for a live session too |
| `#/broadcast/<key>` | every participant on one absolute axis, one crosshair, one zoom: *did they all dip, or just that one viewer?* |
| `#/history` | sessions and broadcasts over a range, filtered/sorted/paged server-side |
| `#/explore` | any recorded field, for one or more sessions, on one axis |
| `#/fleet` | broadcasts as stripes on a shared axis, plus bucketed trends and a cohort A/B |
| `#/rules` | every playbook rule with its thresholds, and a per-session trace of what each one read |
| `#/sql` | ad-hoc queries over the partitions (below) |

Two habits the whole surface keeps:

- **Absolute time is the axis for anything historical**, with the timezone
  shown. Relative time (`3m ago`) is the annotation. The header also names
  the gap if your browser's clock disagrees with the service's.
- **Absence is never green and never blank.** A range before the oldest
  stored partition says so rather than rendering as empty; a session whose
  raw window was pruned shows its permanent rollup and says why the charts
  are empty.

## The SQL console

`#/sql`, and the MCP `query_sql` tool, run read-only DuckDB over the
NDJSON partitions. **Whether they can answer depends on how the binary was
built**, and they say which:

- The shipped image is compiled `-tags duckdb` with cgo — that is what the
  default-on flag actually delivers.
- `go build ./...` on a fresh clone is cgo-free and links no DuckDB at
  all. The console then explains that this build has no engine — a
  different message from a query error, rendered as one.

The engine refuses anything that is not a read (`SELECT`, `WITH`,
`DESCRIBE`, `SUMMARIZE`, `SHOW`, `EXPLAIN`, `FROM`, `PIVOT`) and refuses
more than one statement per query. External file access has to stay on for
it to read the partitions at all, so that allowlist — not a read-only
connection — is what stops a `COPY … TO` over the data directory.

## Notes

An operator note pinned to a session, a broadcast, or a moment on a
timeline is the one thing this service lets you write. Notes live beside
the rollups, are **permanent**, and are never mixed into a session file —
a raw partition stays exactly what a client sent. An annotation outliving
the samples it describes is the normal case and the point.

## Finding one stream

Rows are labelled with the **obfuscated** broadcast key, because that is
the only identity telemetry is ever told: the six-character code is a join
credential, and the client is structurally incapable of reporting one (the
session token's HMAC binds the obfuscated key, so a batch carrying
anything else is rejected before a byte is written).

So the lookup runs one-way and server-side. Set `-stats-key` to the fleet
stats key and a **Find** box appears: type the code you already hold, the
service computes the digest the relay would have published for it, and the
page highlights that row. `POST /v1/resolve`, never a query string — a
join credential must not land in browser history, a `Referer`, or a proxy
log. The code is never stored and never logged.

**It is off by default and that is deliberate.** A service holding the
stats key can enumerate join codes for the broadcasts it has stored —
31⁶ HMACs is minutes of work — so turning it on is an explicit act, the
same posture `query_sql` takes. Leave it unset and the endpoint answers
501 and the box does not render.

## Tab visibility

Every session reports `documentHidden` and a cumulative
`documentHiddenMs`. A hidden tab stops firing rAF, so a viewer's
`renderedFps` falls to 0 while decode carries on — a difference visible in
no other number.

The two roles are handled **oppositely**, and it matters:

- A **viewer**'s hidden window is not evidence of anything wrong; nothing
  was supposed to be rendered. Hidden samples are excluded from the
  viewer's presentation series (`renderedFps`, `renderCadenceP95Ms`) in
  the permanent rollup, and from nothing else — received, decoded, jitter
  and latency all kept being measured by the worker.
- A **broadcaster**'s hidden tab is throttled by the browser, so it
  genuinely sends fewer frames and every viewer sees it. Nothing is
  filtered; the fact is recorded so the collapse can be *explained*.

A session spent wholly in the background has **no** presentation series
rather than a zero, and clients predating the signal are never dropped
from a series for failing to report it.
