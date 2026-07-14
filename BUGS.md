# Known bugs

Confirmed, not-yet-fixed defects. Each entry says how it was found, what the
impact is, and where a fix would start. Remove entries when fixed (and move
anything durable they taught us into the relevant `docs/NN-*.md` gotchas).

## `WebTransport.getStats()` sampling returns null on Chrome 152

**Found**: 2026-07-14, during the R10 Firefox/Chrome diagnostics session
(docs/14 field findings). Every Copy-diagnostics sample from Chrome 152
(`Chrome/152.0.0.0`) shows `"connection": null` — on **both** surfaces,
including the broadcaster, whose sampler runs on the main thread. This rules
out the R10 P3 nested-transport-worker as the cause: the sampling path broke
on Chrome itself.

**Expected**: Chromium has shipped `WebTransport.getStats()` for years; R9
(docs/13) built the connection-health sampling on it, and it worked on the
Chrome versions current at R9 implementation time.

**Impact**: the overlays' Network sections read `—` on Chrome (RTT, packets
lost, **"Dgrams dropped (in)"** — the receiver-queue overflow signal — and
`atSendCapacity`/`estimatedSendRate` on the broadcaster). We are blind on
exactly the per-leg connection health R9 was built to expose; during the R10
diagnosis this forced attribution through relay-side counters alone.

**Suspicion**: Chrome 152 changed the API surface — `getStats()` removed,
renamed, moved to a different shape, or now rejecting. The sampler
(`gawk-app/src/transport/net-stats.ts`, `sampleConnectionStats`) returns
`null` only when `getStats` is missing (feature-detect) or its promise
rejects; a merely *renamed field* would instead produce a stats object full
of nulls — we get `null`, so the method itself is gone or throwing.

**Fix starts at**: reproduce in Chrome 152 DevTools on any WebTransport
session (`wt.getStats()` in the console), check the current spec/Chromium
status for the stats API shape, then adapt `net-stats.ts` (and its
feature-detection) accordingly. `net-stats.test.ts` pins the mapping; add the
new shape as a fixture once known.
