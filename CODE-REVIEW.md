# Code & Review Guidelines

Distilled from the post-R1 code review (2026-07-13, see
[docs/06](docs/06-multi-broadcaster.md#post-implementation-review-2026-07-13)),
which found six issues in an implementation whose tests were all green.
Every rule below exists because its absence cost us a real bug; the R1
example is cited so the rule stays concrete. Applies to human and AI
contributors alike.

## Fixing bugs: test-first, always

Write a test that reproduces the bug, **run it and watch it fail**, then
fix, then watch it pass. Never land a fix whose test didn't first fail —
a test written after the fix proves nothing about the bug.

- If the bug is genuinely unreachable from the existing harnesses, say so
  explicitly in the PR and agree on the fallback *before* fixing. Adding a
  small seam is usually better than skipping the test: R1's post-upgrade
  race got a nil-in-production `testHookPostUpgradeSubscribe` hook; the
  ViewPage effect bug justified adding `@testing-library/react`.
- Pure-quality refactors don't need new tests, but must stay green under
  the full gates (below).

## Coding guidelines

**Error paths must release what they acquired.** Any `start()`/`connect()`
that fails midway must tear down everything it built before rethrowing —
in this codebase a leaked WebTransport session is a *zombie publisher*
that holds a broadcast ID hostage until the tab closes. Route success and
failure through one shared teardown (`BroadcastPipeline.teardown()`).

**Type your failure phases.** When callers behave differently depending on
*where* an operation failed (retry vs. surface), encode that in the error
(`BroadcastStartError.phase: 'connect' | 'capture'`) instead of letting
them guess from messages. R1's reclaim fallback treated "server rejected
the dial" and "user cancelled the share picker" identically — silently
minting a new broadcast on a cancelled picker.

**One event, one authoritative signal.** When several async signals report
the same underlying event (datagram read loop ending vs. `wt.closed`),
their settle order is unspecified. Pick the signal that carries the
semantics (only `wt.closed` has the close code) and make the others defer
to it — never let whichever-fires-first decide behavior. See the README
gotcha and `transport/viewer-transport.ts` (`handleReadLoopEnd`).

**Shared constants have exactly one definition per language.** Go's
`wire` package and `wire.ts` are the two homes; nothing else hardcodes
wire values. R1 shipped `CLOSE_CODE_BROADCAST_ENDED` in `wire.ts` and then
compared against a literal `4000` twice, plus a third copy of the
broadcast-ID alphabet in `ViewPage.tsx`.

**React effects that *act* (dial, join, navigate) fire on explicit events
only** — mount, or a real DOM/browser event — never on re-renders driven by
state or recreated callbacks. Read the latest handler through a ref; don't
put it in the deps. Reading state a component renders from inside an
effect without depending on it is a stale-closure bug, not a lint nit:
R1's auto-join effect rejoined a stale broadcast while the user typed a
new code.

**Counters and stats survive their owner's deletion.** Fold per-child
counters into the aggregate *under the lock, before* deleting the parent
(`handleGraceExpiry` folds live subscribers' drop counts before dropping
the hub). Anything counted after deletion is silently lost.

**New config knobs land fully plumbed in the same change:** flag + `GAWK_*`
env in `config.go`, chart `values.yaml`, chart `templates/deployment.yaml`,
the flag's docs, **and the production call site** (`registryOptions` in
`cmd/gawk-server/main.go`). R1 shipped `-broadcast-grace` without the Helm
half — deploys silently ran the default with no values-level override. R2
shipped all six hardening knobs parsed, charted and documented but wired
only into the transport *test helper* — `-max-bandwidth` was a complete
no-op in production and every test stayed green. Walk each knob from flag
to the object that consumes it; a design doc's file list is not proof of
completeness.

## Review checklist

Work from the design doc, not just the diff. For gawk, milestone docs
(`docs/NN-*.md`) are contracts with **locked decisions** and per-chunk
**acceptance criteria** tables.

1. **Locked decisions**: re-read them and check the code does what they
   say. R1's design said, twice, that the announce read "must not gate
   media start"; the implementation awaited it before capture and could
   hang forever. Green tests don't catch contract violations nobody tested.
2. **Acceptance criteria → tests**: walk every criterion and point at the
   test that covers it. Missing ones (R1: the generation-race test, the
   pipeline URL tests, network-level isolation, the Helm values) are
   findings even when the code looks right.
3. **Walk the failure paths by hand**: for each `await` in a setup
   sequence, ask "what is live if *this* rejects, and who cleans it up?"
   The happy path is what the author tested; the leak was in the catch.
4. **Hunt settle-order races**: any place two promises/goroutines react to
   one event, ask which order the test exercised — and what happens in the
   other order. "Manual verify passed in Chrome" is one ordering, not both.
5. **Check both sides of the wire**: a constant, format, or close-code
   added on one side must be *used* (not re-hardcoded) on the other.
6. **Error mapping at boundaries**: every distinct sentinel error crossing
   a transport boundary needs a deliberate status/close-code, not a
   catch-all (R1 closed a GC'd-broadcast race with the "subscriber limit"
   code).
7. **Ops surface**: flags, Helm values, `/statusz` fields, log fields —
   shipped together, or the gap called out explicitly.

## Gates (all must pass before merge)

```sh
cd gawk-server            && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-broadcast         && test -z "$(gofmt -l .)" && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-telemetry         && test -z "$(gofmt -l .)" && go vet ./... && CGO_ENABLED=1 go test -race ./...   # plus -tags duckdb
cd gawk-app               && npm test && npm run lint && npm run build
cd gawk-telemetry/ui      && npm ci && npm run lint && npm test && npm run build
cd gawk-broadcast-windows && cargo xwin clippy --all-targets --target x86_64-pc-windows-msvc -- -D warnings
helm lint gawk-server/deploy/charts/gawk-server gawk-app/deploy/charts/gawk-app
helm lint gawk-telemetry/deploy/charts/gawk-telemetry --set telemetryKey=00
```

See `CONTRIBUTING.md`'s "Running the gates before you push" for the exact,
kept-current commands (including `go mod tidy` and license-notice checks) —
the block above is a summary, not a second source of truth.

`gofmt -l` prints nothing when clean — it joined the gates after R2 landed
unformatted files (`go vet` does not check formatting, so nothing failed).

Chart template changes additionally get a `helm template` render check of
the touched values (default + one override).

**`npm run build` is the only real frontend typecheck.** gawk-app's root
`tsconfig.json` is solution-style (references only), so a bare
`npx tsc --noEmit` type-checks *nothing* and passes vacuously; `tsc -b`
inside `npm run build` is what actually checks the code — including test
files. This exact gap let a `vi.fn()` typing error through local
verification and broke the main build (run 29212215376). Vitest doesn't
typecheck either (it strips types), so green tests prove nothing about
types.
