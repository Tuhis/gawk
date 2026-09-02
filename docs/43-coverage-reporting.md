# R41 — Test coverage measurement and badges (docs/43)

**Status**: designed + implemented 2026-09-02. Chunks **CV1–CV6** (`CV` =
CoVerage; two-letter prefix per the R21+ convention — collides with nothing).
Single PR, because the parts do not stand alone: a measurement with nowhere to
publish is a log line nobody reads, and a badge with no gate is decoration.

**How to read this document**: §1 is the why, §2 the locked decisions, §3 the
implementation as built, §4 the chunk table with acceptance criteria, §5 what
the numbers do and do not mean, §6 operations.

---

## 1. Purpose

Eight test suites across four toolchains run on every relevant PR, and until
now the only thing anyone could say about their coverage was "there are a lot
of tests". That is not nothing — `CODE-REVIEW.md` makes bug fixes test-first,
and the suites are the reason the relay can be refactored at all — but it left
two questions unanswerable:

- **Which component is thin?** The answer turned out to be worth knowing:
  `gawk-telemetry/ui` measures 18.7% while `gawk-admin/ui` measures 91.5%. Both
  ship in an image; only one of them is tested like it.
- **Did this PR make it worse?** Nothing forced the question, so nothing
  answered it. A new package with no tests at all passed CI exactly like a
  well-covered one.

R41 answers both, in the shape the rest of this repository already uses:
measured in the job that owns the code, gated where a human can still act on
it, published from `main` only.

## 2. Decisions

**D1 — Self-owned `badges` branch, not Codecov.** The numbers live in an orphan
branch of this repository and are served to shields.io through
`raw.githubusercontent.com`. No third-party account, no token beyond
`GITHUB_TOKEN`, and no coverage report — which is a file-by-file map of the
source tree — leaving GitHub. This follows the deployment rule that already
governs CI here: *CI is publish-only, and no credentials for anything else live
in GitHub Actions* (`CLAUDE.md`). The cost is real and accepted: no PR
diff-coverage comments, no per-file sunburst. What a reviewer gets instead is
the floor gate (D3), which is the part that changes an outcome.

**D2 — Counts, not percentages, are the stored artifact.** Every job emits
`{component, label, covered, total, pct, unit}`. The aggregate badge is
`Σcovered / Σtotal`, so a 744-line UI cannot outvote a 5,191-statement relay.
Averaging eight percentages would have been simpler and wrong.

**D3 — A floor per component, enforced in that component's own job.** Floors
live in `coverage-floors.json` at the repo root and are set from a measured
baseline, rounded down to the whole percent and then dropped by one so a single
nondeterministic test cannot red the tree. The gate runs inside the component
job for two reasons: the annotation then lands on the component that owns the
code, and the publishing job runs on `main` only — a gate living there would
never see a pull request, which is the only place it can still change what
gets merged. A `null` floor is the documented escape hatch for a component
whose baseline is not yet established; it reports and does not gate.

Rejected: **no-regression-versus-main**. It needs the base value fetched, a
tolerance to keep flaky suites from failing unrelated PRs, and it fails PRs for
a number that moved because someone deleted dead code. A floor is cruder and
harder to argue with.

**D4 — The badge writer merges; it never republishes wholesale.** `ci.yml` is
path-filtered, so a push touching only `gawk-app/` runs exactly one coverage
job. The writer folds the records it was handed into `data.json` and
regenerates every badge from the merged set, so the five components that did
not run keep the numbers they last measured — still true of code that did not
move. Publishing "what this run produced" would blank five badges on nearly
every push. This is the single most load-bearing property in the change and it
has a test (`tools/coverage/test_coverage.py`).

**D5 — Two writers, one branch, one concurrency group.**
`gawk-broadcast-windows` lives in its own workflow with its own triggers, and
artifacts do not cross workflows — so `broadcast-windows.yml` gets its own copy
of the publishing job. Both use the shared `concurrency: coverage-badges`
group, which turns the release-commit race into a queue; the writer's
fetch-rebuild-retry loop covers what a queue cannot (a re-run, a dispatch, a
push from outside the group).

**D6 — Telemetry is measured in its DEPLOYED configuration.** The module has
two builds: untagged (what a fresh clone gets, no cgo) and `-tags duckdb` (what
the image ships). They overlap on `internal/sqlengine` with different files
compiled, so their profiles cannot be merged block-for-block; measuring the
untagged one alone would report the shipped query engine as dead code. The
coverage profile therefore comes from the tagged run, which this change widens
from `./internal/sqlengine/...` to `./...`. The untagged gate is untouched and
still proves the fresh-clone property. **Cost**: the telemetry job now runs its
suite twice, once per configuration.

**D7 — The artifact is uploaded before the gate, not after.** A push that lands
below a floor must be red — but the badge should then show the number that is
actually true, rather than leaving a stale higher one standing because the run
that would have corrected it failed.

**D8 — One Python tool, tested.** Four report formats, one normaliser
(`tools/coverage/coverage.py`), with `python3 -m unittest` tests that pin the
parsers, the weighted aggregate, the colour thresholds and D4's partial-update
rule. The alternative — a `jq`/`awk` incantation per job — is eight copies of
the arithmetic that every floor gate depends on. Python because `ci.yml`
already provisions it for the licence gates, and because the Go parser has to
agree with `go tool cover` exactly (it does: 94.6% on `gawk-server/wire` from
both).

## 3. Implementation

```
tools/coverage/coverage.py        normalise | check | badges
tools/coverage/test_coverage.py   its tests (the `coverage-tool` CI job)
coverage-floors.json              the gate's thresholds
.github/actions/coverage/         normalise → upload → gate, used by 8 jobs
.github/actions/publish-coverage-badges/   the badges-branch writer
```

Per component, what produces the number:

| Component | Command | Unit |
|---|---|---|
| `gawk-server` | `go test -race -covermode=atomic -coverprofile` | statements |
| `gawk-broadcast` | same | statements |
| `gawk-admin` | same, in the run that HAS Postgres and envtest | statements |
| `gawk-telemetry` | `go test -tags duckdb -covermode=atomic -coverprofile ./...` (D6) | statements |
| `gawk-app` | `vitest run --coverage` (v8 provider) | lines |
| `gawk-telemetry-ui` | same | lines |
| `gawk-admin-ui` | same | lines |
| `gawk-broadcast-windows` | `cargo llvm-cov --no-report` ×2 + `report` | lines |

`-covermode=atomic` is not decoration: the default `set` mode writes its
counters non-atomically from every goroutine a suite starts, which `-race`
correctly reports as a data race.

The vitest configs spell out `coverage.include` (`src/**/*.{ts,tsx}`). Left to
the provider's default, v8 coverage reports only files a test actually
imported — a module with no test at all would be absent from the
*denominator* rather than counted as uncovered, which is the one thing a
coverage number must not do.

The `badges` branch holds `data.json` (the counts) plus one shields-endpoint
file per component and `total.json`. It is an orphan branch carrying no
workflow files, and it is never merged.

## 4. Chunks and acceptance criteria

| Chunk | Deliverable | Accepted when |
|---|---|---|
| **CV1** | `tools/coverage/coverage.py` + tests | `python3 -m unittest discover -s tools/coverage` passes; the Go parser's total equals `go tool cover -func` on the same profile |
| **CV2** | Coverage collection in all eight jobs | every job prints its percentage and uploads a `coverage-<component>` artifact; a job whose report is missing fails rather than reporting 0% |
| **CV3** | `coverage-floors.json` + the gate | a floor set above the measured value fails **only** that component's job, naming component, measured and floor; a component absent from the file is an error, not a pass |
| **CV4** | The `badges` branch writer | a push touching one component updates that badge and `total.json` and leaves the others byte-identical; two concurrent pushes both land; a re-run with identical numbers makes no commit |
| **CV5** | README badges | the aggregate renders in `README.md`; each component README carries its own, and `gawk-telemetry`/`gawk-admin` carry a second for their SPA |
| **CV6** | This document, the ROADMAP row, the docs index | present, and `docs/README.md` lists this doc under Testing |

## 5. What the numbers mean — and what they do not

Read the badges with three caveats, all structural:

- **Units are not commensurable.** Go statements, TypeScript lines and Rust
  lines are summed into one aggregate because a single number is what a badge
  can show. It is a size-weighted mean of three different measures, and it is
  honest only to about a percentage point.
- **Some code CI structurally cannot reach.** `gawk-broadcast`'s Gio GUI needs
  a display, the Windows capture and Media Foundation paths need Windows and a
  GPU, and the on-hardware registers in `docs/38` §10 and `docs/39` §6 exist
  precisely because CI cannot stand in for them. Those files are in the
  denominator. The floors are set against a baseline that already includes
  them, so this depresses the number without ever failing a build — and raising
  it by *excluding* the untestable paths would make the badge flatter and less
  true.
- **E2E is not counted.** `e2e/` and the kind tier prove things no unit test
  can, and contribute nothing to these percentages. A component's badge is
  about its own suite.

A coverage number is a floor on how much of the code was executed, not a claim
about how well it was checked. This is why the gate is a floor and not a
target.

## 6. Operations

**Raising a floor** is an ordinary PR editing `coverage-floors.json`. Do it in
the PR that earns the coverage. **Lowering one** is allowed — a deliberate
deletion of tested code can genuinely move it — but the reason belongs in the
commit body.

**Bootstrapping the branch**: the writer creates `badges` on its first run if
it does not exist. Until that run lands on `main`, every badge renders as
shields.io's "invalid response" — expected between merge and the first main
push, and self-correcting.

**If a badge looks stuck**: the component's job was skipped by the path filter,
which is correct behaviour (D4). Confirm against `data.json` on the `badges`
branch, which records what each component last measured.

**If the writer cannot push** after five attempts it fails the job. Nothing
else breaks: the badges keep serving their previous values, and the next push
to `main` republishes.
