# Adversarial Review — R19 Resilient Viewer Mode (`docs/24`)

**Date:** 2026-07-22
**Scope:** the R19 opt-in "Resilient mode" for lossy viewer links — relay carrier
delivery (`gawk-server/internal/hub`, `internal/transport`), the carrier wire
framing (`gawk-server/wire`, `gawk-app/src/transport/wire.ts`), viewer ingestion
(`connection.ts`), the resilient reorder/playout profiles
(`reorder-buffer.ts`, `playout.ts`, `resilient.ts`, `live-edge.ts`), the mode
toggle lifecycle (`features/viewer/*`), and observability.
**Goal of the review:** decide whether the *experimental* feature is ready to be
called *production-grade*, and if not, what blocks it. The bar the mode is held
to is its own stated purpose: **mask transient network stalls and keep video
smooth on a lossy-but-adequate-bandwidth mobile link, trading latency for
smoothness (0.5–2 s behind live).**

**Method.** Eight independent adversarial finder passes across relay / wire /
viewer / product dimensions, each finding then checked by an independent skeptic
pass instructed to *refute* it. Every finding cited below was traced to the
actual code; the two highest-severity ones whose automated verification was cut
short (`PLAYOUT-1`, `LIFECYCLE-1`) were re-verified by hand — one confirmed, one
refuted. `go vet` and `go test -race ./internal/hub/...` were run and are clean.

---

## Verdict

**Not production-grade yet — keep it experimental / default-off for one more
cycle.** The engineering underneath is genuinely good: the relay drain's
concurrency and state machine are race-free and hold up to adversarial reading,
the wire framing is solid and DoS-bounded, cross-broadcast isolation holds, and
the scale-out (R17) invariants are correct. But three things block GA, and the
first is the headline capability itself:

1. **The advertised "adaptive up to ~2 s" buffer is structurally unreachable
   (`PLAYOUT-1`, high).** The jitter signal that drives the resilient playout
   controller is a histogram hard-capped at **500 ms**, so the offset can never
   climb above **~534 ms** no matter how bad the link gets — the `maxMs: 2000`
   in the resilient profile is inert. The mode is, in practice, a ~500 ms
   near-fixed buffer wearing an adaptive costume. It cannot absorb the ≥500 ms
   retransmit stalls and multi-hundred-ms handover spikes it exists to mask.
2. **Its own audience can't reach it (`PRODUCT-2`, high/product-fit).** The
   mobile-targeted toggle lives only in a desktop right-click context menu, with
   no touch/kebab affordance and auto-detect deferred.
3. **It can't be shown to work and its failures are invisible (`PRODUCT-1`
   medium, `PRODUCT-3` medium).** No CI/loss coverage exercises the carrier path
   at all, and the mode's dominant real-world failure is mis-bucketed in metrics
   as a generic slow-viewer drop.

None of these is a data-corruption or crash bug; the carrier *does* reliably
deliver deltas. The problem is that the feature under-delivers on its promise in
exactly the deep-loss conditions it was built for, and can neither be reached by
its users nor observed by its operators. Fix `PLAYOUT-1`, `PRODUCT-2`,
`PRODUCT-3`, and land real loss coverage (`PRODUCT-1`), and this becomes a
credible GA feature.

---

## Findings (confirmed, severity-ranked)

| # | ID | Sev | Area | One-line |
|---|----|-----|------|----------|
| 1 | PLAYOUT-1 | **High** | correctness | Jitter histogram caps at 500 ms → resilient offset can never exceed ~534 ms; `[150, 2000]` upper half is dead |
| 2 | PRODUCT-2 | **High** | product-fit | Mobile toggle only in a desktop context menu; undiscoverable/unreachable on touch; auto-detect deferred |
| 3 | PRODUCT-1 | Medium | test-coverage | Zero CI/integration/browser/loss coverage of the carrier path — regressions ship silently |
| 4 | PRODUCT-3 | Medium | observability | Reliable-sub queue overflow (the mode's main failure) mis-bucketed as generic `queue_full`; invisible to operators |
| 5 | BACKPRESSURE-2 | Medium | calibration | Carrier write deadline is 1 s (2× the 500 ms GOP); a stuck record blocks the whole subscriber drain up to 1 s |
| 6 | INGEST-1 | Medium | resource-leak | `readServerStreams` `tasks[]` grows for the whole session; ~doubled by resilient carriers; bites hours-long mobile sessions |
| 7 | BW-CHARGE (BACKPRESSURE-1 / DRAIN-1) | Medium | correctness | Egress cap charged *before* the drop/write decision → dropped records phantom-debit the shared cap |
| 8 | PLAYOUT-3 | Low | tuning | 60 s jitter window makes the offset react on a minute timescale; compounds PLAYOUT-1 |
| 9 | BACKPRESSURE-3 | Low | calibration | Drop-**newest** on a reliable in-order carrier punches mid-GOP holes → keyframe resync |
| 10 | BACKPRESSURE-4 | Low | cap-bypass | 2-byte carrier prologue egresses outside the bandwidth cap |
| 11 | WIRE-1 | Low | test-coverage | No exact-1200-byte carrier record round-trip test (the hot full-delta case) |
| 12 | LIFECYCLE-2 | Low | ui-consistency | Interpolation menu entry keyed on *stored* (not effective) playout mode → unreachable while resilient governs pacing |

Default-off caveats: the bandwidth cap is **unlimited by default**
(`-max-bandwidth` unset ⇒ `limiter == nil`), so #7 and #10 are inert unless an
operator sets a cap; and audio is experimental/default-off, which bounds today's
exposure of anything audio-adjacent.

---

### 1. PLAYOUT-1 — the adaptive buffer can never exceed ~534 ms *(High)*

**Where:** `gawk-app/src/transport/live-edge.ts:74` (`QUANTILE_RANGE_MS = 500`),
consumed by `reorder-buffer.ts:146` → `arrivalJitterMs()` → `playout.ts`
`RESILIENT_PLAYOUT_PROFILE` (`transport/playout.ts:79-85`,
`update()` :104-129).

**What.** The resilient controller's target is
`clamp(arrivalJitter + HEADROOM_MS(34), [150, 2000])`, where `arrivalJitter` is
`WindowedQuantileTracker.quantile(0.95) − WindowedMinTracker.min` (p95 − min of
`received − timestamp`). But `WindowedQuantileTracker` is a fixed-range histogram:
`numBins = ceil(rangeMs / binMs) + 1` with `rangeMs = QUANTILE_RANGE_MS = 500`,
and every sample is clamped into `[bucket.min, bucket.min + 500 ms]`
(`live-edge.ts:99, 111`). So `p95 − min ≤ ~500 ms` **by construction**, and the
controller target tops out at `500 + 34 = 534 ms`. `RESILIENT_PLAYOUT_PROFILE`'s
`maxMs: 2000` is unreachable from the signal — the entire upper half of the
"adaptive up to 2 s" envelope is dead code.

**Failure scenario.** A viewer on LTE hits a handover: a burst of deltas is held
by QUIC retransmit for ~800 ms, then delivered. This is precisely what resilient
mode should absorb into its buffer. The measured p95 − min saturates at 500 ms,
the controller nudges the offset toward 534 ms at 100 ms/s, and the buffer never
provisions the ~800 ms the retransmit needed. Frames past the offset are late →
the reorder buffer declares a gap → freeze-to-keyframe. The mode advertised as
"smooth to 2 s behind live" under-buffers exactly the deep-loss condition it
exists for.

**Why it matters most.** This is the load-bearing claim of the whole feature.
Decisions 2 and 7 justify carriers by saying the wider adaptive buffer will
absorb retransmit jitter "up to ~2 s"; that buffer does not exist. Everything
else could be perfect and the mode would still under-deliver.

**Fix (measurement, not control law — the slew is sound).** Parameterize the
histogram range and window into the playout profile:
- Resilient profile: `quantileRangeMs ≈ 2500`, `windowMs ≈ 5–10 s` (vs the
  default 500 ms / 60 s). The 60 s window is the second half of the problem
  (see PLAYOUT-3).
- Consider an **immediate step-up** (not slew) on a large upward jump in
  measured jitter — buffering is cheap and the doc itself says "up fast" (only
  the *down* direction needs to be slew-limited to avoid a visible skip).
- Add a controller unit test asserting the offset can actually reach, say,
  1500 ms given a 1500 ms jitter signal in the resilient profile.

---

### 2. PRODUCT-2 — the mobile feature is unreachable on mobile *(High, product-fit)*

**Where:** `gawk-app/src/features/viewer/ViewerScreen.tsx` — toggle at :383-388,
opened only by `onContextMenu` (:418-421); the always-visible control bar
(:530-570) has audio/stats/fullscreen/leave icons but no menu/kebab/overflow
button.

**What.** "Resilient mode (mobile networks)" is only in the right-click context
menu. On touch devices — the exact audience — there is no reliable long-press →
context-menu affordance, no kebab button, and auto-detect is explicitly deferred
(Decision 11). A phone viewer on a bad link has no discoverable, reliable path to
the one feature built for them.

**Failure scenario.** A user on a train opens a stream on their phone, it
stutters, and there is no visible control to enable the mode that would fix it.
The feature's entire target population cannot use it.

**Fix.** Add a visible overflow/kebab button to the control bar that opens the
same menu (works for mouse and touch), and/or ship the deferred Decision-11
"Connection looks lossy — enable Resilient mode?" suggest-banner (its input
signals — `framesDroppedIncomplete`, `reorderGapResyncs`, `arrivalJitterMs` —
already exist). This is product work, not a code defect, but it gates the
"production-grade for mobile" claim.

---

### 3. PRODUCT-1 — no automated coverage of the carrier path under loss *(Medium)*

**Where:** `e2e/run.mjs` and the R20 tier-1/tier-2 harness.

**What.** The only automated end-to-end/browser test subscribes in **default
datagram mode over a zero-loss loopback** (`#/view/${id}`, no
`?delivery=reliable`), and nothing anywhere injects loss. The client carrier
reader, the end-to-end `?delivery=reliable` negotiation, and the resilient
reorder/playout profile are covered only by unit tests against in-memory fakes,
plus one manual phone session (docs/24 X6, not CI-reachable). A regression in the
carrier reader or the negotiation would ship green.

Note: the *relay-side* stall/cancel path **is** unit-tested (`fakeCarrierStream`
+ `carBlock` in `hub_test.go` exercise deadline/cancel), so this is specifically
an integration + loss-condition gap, not "no tests."

**Fix.** Add a relay-integration test (Go) that drives a subscriber with
`?delivery=reliable`, injects delta loss (drop N% of publisher datagrams), and
asserts carrier records arrive and reorder correctly with zero gap resyncs — the
`relay_integration_test.go` fixture publisher already exists. Optionally add a
`tc netem`-shaped loopback cell to the R20 harness for the browser carrier
reader.

---

### 4. PRODUCT-3 — the mode's dominant failure is invisible in metrics *(Medium)*

**Where:** `gawk-server/internal/hub/hub.go:1818-1824` (`enqueueLocked`).

**What.** When a reliable subscriber's carrier can't keep up (sustained
congestion, repeated write stalls), its bounded 256-deep delta queue overflows in
`enqueueLocked`, which increments the **generic** `s.dropped` counter — surfaced
in Prometheus as `gawk_broadcast_datagrams_dropped{reason=queue_full}`,
byte-identical to a normal slow *datagram* viewer. The reliable-specific
`carrierRecordsDropped` is **not** incremented for this case. Yet this overflow
punches a hole in the supposedly-reliable in-order delta stream and forces the
viewer to freeze-to-keyframe — the exact stutter resilient mode exists to
prevent. An operator watching the dashboards cannot distinguish "resilient mode
is failing this viewer" from "a normal viewer has a slow link."

**Fix.** Give the reliable path its own overflow accounting — e.g. increment a
`reliable`-flagged reason (`reason=carrier_queue_full`) or a dedicated
`gawk_broadcast_reliable_queue_overflow_total` in the enqueue path when
`s.reliable`, so the mode's real failure mode is observable. Keep the datagram
`queue_full` derivation intact for normal subs (R9 rule).

---

### 5. BACKPRESSURE-2 — 1 s carrier write deadline vs a 500 ms GOP *(Medium)*

**Where:** `hub.go:2005` (`writeCarrier` sets the per-record deadline to
`opts.KeyframeWriteTimeout`), default `time.Second` (`hub.go:516`); GOP is
500 ms.

**What.** The carrier reuses `KeyframeWriteTimeout` (1 s) as its **per-record**
write deadline, and rotation is only observed *between* dequeues
(`drainReliable`, `hub.go:1882`). So a single flow-control-blocked record write
freezes the entire subscriber's delta drain for up to 1 s — two GOPs — before it
abandons. During that block the queue fills and (see BACKPRESSURE-3) drops.

Corrected scope (verifier): the *dropped* window is ~one GOP tail (GOP-B deltas
are delivered late, not lost, because `carDead` resets on the next rotation at
`hub.go:1887`), and at 30–60 fps a 1 s block accrues ~30–60 deltas against the
256-deep queue, so it does **not** overflow the queue on its own. The defect is
the **1 s stall latency**, not queue overflow — a stall the mode exists to hide
that lasts twice the GOP it is scoped to.

**Fix.** Add a carrier-specific write timeout (~1 GOP, e.g. 500 ms) instead of
reusing the 1 s keyframe timeout, and/or check `carRotations` immediately before
the blocking `Write` so a superseded GOP tail is abandoned at rotation rather
than at the deadline.

---

### 6. INGEST-1 — `tasks[]` grows unboundedly for the session *(Medium)*

**Where:** `gawk-app/src/transport/connection.ts:158` (`const tasks = []`),
`:164` (`tasks.push(...)`), drained only at `:175`
(`await Promise.allSettled(tasks)` in `finally`).

**What.** `readServerStreams` runs once per WebTransport session and pushes one
settled `Promise<void>` per accepted server stream into a session-scoped array
that is never pruned. Each keyframe stream and each carrier stream is one entry:
resilient mode roughly **doubles** the rate (keyframe + carrier per GOP ≈ 4/s).
The retained object is a settled void promise (~60–100 B — the ~236 KB keyframe
`chunks` are released with each async scope), so this is a slow leak, not
payload-scale: ~4/s × 3600 = ~14 k entries/hr ≈ ~1 MB/hr, monotonic.

**Why it matters.** The target audience is mobile viewers who leave a stream on
for hours — exactly the population that hits the tail of an unbounded array.

**Fix.** Prune settled tasks — e.g. keep a `Set`, add on accept, remove in the
task's `.finally()` — so the working set stays proportional to in-flight streams
(~2), not to session length. Pre-existing (keyframe streams already leaked at
~2/s), but resilient mode doubles it and this review is the moment to fix it.

---

### 7. BW-CHARGE — egress cap charged for records that are then dropped *(Medium)*

**Where:** `hub.go:1877` (`consumeBandwidth(n)`) precedes the `carDead`/`closed`
drop (`:1889`), the `openCarrier` failure drop (`:1893`), and `writeCarrier`
failure (`:1906`). (Reported independently as BACKPRESSURE-1 and DRAIN-1.)

**What.** `consumeBandwidth` debits a single process-wide token bucket shared by
**all** broadcasts. In `drainReliable` it is charged *before* the code knows
whether the record will actually be written. When `carDead` is latched (a whole
GOP tail after an open/write failure), every one of those records still debits
the global cap despite zero bytes hitting the wire — "phantom debit" that
throttles *unrelated* broadcasts. This directly contradicts the code's own
comment at `hub.go:1872-1875` ("charged per record as written").

**Scope.** Inert by default (cap unlimited ⇒ `limiter == nil`). Worst case is a
GOP-sized burst of phantom debit per stuck GOP; steady-state is smaller. The
non-reliable drain charges-then-sends too, but there a send is at least
*attempted*; the `carDead` branch charges for records it *knows* it will not
write.

**Fix.** Charge `consumeBandwidth` only on a successful write (move it after
`writeCarrier` succeeds), or refund on the drop branches. Same fix aligns the
code with its comment.

---

### 8. PLAYOUT-3 — 60 s jitter window reacts on a minute timescale *(Low)*

**Where:** `live-edge.ts:14` (`LIVE_EDGE_WINDOW_MS = 60_000`), used for both the
p95 and min trackers feeding the resilient offset.

**What.** With a 60 s window, a seconds-scale burst sits below the p95 of a
minute of samples and barely moves the offset, while a sustained loss episode
(>~3 s, i.e. >5 % of the window) keeps p95 — and the offset — elevated for
roughly a full minute *after* the link cleans up, then the 15 s dwell + 10 ms/s
down-slew add more. The controller is sluggish in both directions. Compounds
PLAYOUT-1; fixed by the same "shorten the resilient window to ~5–10 s" change.

---

### 9. BACKPRESSURE-3 — drop-newest on a reliable in-order carrier *(Low)*

**Where:** `hub.go:1818` (`enqueueLocked` drop-newest), reused verbatim for
reliable subs.

**What.** The datagram queue's drop-newest-on-overflow policy is correct for
live-edge datagrams but semantically odd for a reliable in-order carrier: under
overflow it drops the frames nearest *live* and reliably delivers the stale
backlog in order, leaving a mid-stream gap. Narrowed by verification: the viewer
is **not** trapped decoding an unbounded stale backlog — `jumpToKeyframe`
(`reorder-buffer.ts:335`) resyncs to the freshest buffered keyframe — so the
cost is one keyframe resync, not a permanent stall. Still, a drop-**oldest** (or
a carrier-backlog cap that sheds stale GOPs) would preserve the near-live frames
a reliable mode is supposed to protect. Consider revisiting the overflow policy
for `s.reliable` subscribers.

---

### 10. BACKPRESSURE-4 — carrier prologue bypasses the bandwidth cap *(Low)*

**Where:** `hub.go:1986-1989` — `openCarrier` writes the 2-byte prologue and adds
it to `egressCarrierBytes` but never calls `consumeBandwidth`.

**What.** A 2-byte-per-GOP (~4 B/s) egress-cap bypass, inconsistent with the
per-record charging invariant. Cosmetic; inert by default. Fold the prologue into
`consumeBandwidth` when the charge ordering is fixed for #7.

---

### 11. WIRE-1 — no exact-1200-byte carrier record test *(Low)*

**Where:** `gawk-server/wire/wire_test.go:1075` (and the TS/`wirecheck` mirrors).

**What.** The carrier round-trip tests only exercise small datagrams (23-byte
golden VideoChunk) and only assert *rejection* of `MaxDatagramSize+1`. The
inclusive upper boundary — a record whose datagram is exactly
`MaxDatagramSize` (1200), the common full-delta-chunk case — is never
round-tripped. The code handles it correctly today (`AppendCarrierRecord` accepts
`len ≤ 1200`), but nothing guards against a future off-by-one. `DecoderConfig`
and `AudioFrame` *do* test their exact-1200 boundary; add the symmetric vector
for carrier records in all three mirrors.

---

### 12. LIFECYCLE-2 — interpolation menu keyed on stored, not effective, mode *(Low)*

**Where:** `features/viewer/ViewerScreen.tsx` — the "Frame interpolation" entry is
offered only when stored `playoutMode === 'adaptive'`, but resilient mode forces
the *effective* mode to adaptive (with interpolation active in the pipeline).

**What.** A resilient viewer whose stored playout is `'off'`/`'fixed'` has
interpolation running with no menu control to turn it off. Minor UX
inconsistency. Gate the interpolation entry on the *effective* mode (or on
`resilientMode || playoutMode === 'adaptive'`).

---

## What held up (credit where due)

These were probed adversarially and are genuinely solid — worth stating so the
hardening pass doesn't churn them:

- **Relay drain concurrency & state machine** (`hub.go`). Lock ordering is
  inversion-free (`carMu`, `kfMu`, `registry.mu` never nested);
  `carSeen`/`carDead`/`scratch` are drain-goroutine-local; `carRotations` is
  atomic. First-carrier bootstrap, rotation coalescing (`carRotations` jumping
  by >1 loses/duplicates nothing — every dequeued delta is written exactly once),
  and `Close`-unblocks-a-stalled-write all check out. At most ~2 carriers are
  ever held; drain goroutines always terminate. `go test -race` clean.
- **Cross-broadcast isolation.** `fanOutLocked` → `enqueueLocked` is a
  non-blocking `select`-default and keyframe/carrier opens are non-blocking
  (`OpenUniStream`), so a stuck reliable carrier blocks only its own
  per-subscriber drain goroutine — never a normal viewer or the publisher.
- **Wire framing** (`wire.go`, `wire.ts`). uint16 length vs 1200-byte max is
  sound (a full delta chunk is exactly 1200 B; oversize is rejected at ingress
  before enqueue). `CarrierRecordParser.pending` is DoS-bounded (< ~1202 B). A
  corrupt length prefix cleanly abandons one stream and resyncs at the next
  keyframe. Endianness consistent. The `010a` prologue + `0017`+chunk golden
  vectors are byte-identical across all three mirrors (shared
  `goldenVideoChunkHex`), so drift is structurally prevented.
- **Viewer ingestion.** End-to-end carrier-record path traced (in-process and
  across the nested transport worker); records are fresh full-buffer copies,
  safe to transfer; rotation FIN / reset / framing-corruption are classified
  correctly; the prologue peek across a 1-byte first read is safe.
- **Scale-out (R17) interop.** `?delivery=reliable` is never propagated on the
  pod-to-pod pull; `SubscribeInternal` hard-codes `reliable=false`; the reliable
  conversion happens at the pod serving the subscriber; a 4002/4003 re-home
  preserves the mode (`ViewerSession` reuses `connectOpts`). Metric cardinality
  is disciplined (no per-subscriber labels).
- **Mode-toggle lifecycle.** Worker `resilient` command is posted before `start`
  and is idempotent; StrictMode double-invoke leaks no worker/session; stored
  playout mode survives a resilient on/off round-trip; mode-off is byte-identical
  on the wire.

## Refuted / non-issues (recorded for rigor)

- **LIFECYCLE-1 (claimed: mid-session toggle desyncs audio for minutes) —
  REFUTED.** The finder claimed the persisted `AudioSink` is never re-anchored on
  a toggle because `onAudioReset` only fires on a broadcaster restart. In fact
  `onAudioReset` fires on **every** pipeline (re)connect
  (`viewer-session.ts:172`), and `handleAudioReset`
  (`useViewerConnection.ts:225`) flushes the sink, resets the
  `AudioRateController`, and resets AV sync — so the toggle-reconnect re-anchors
  audio to the new deep video playhead. (This finding's automated verifier hit
  the session limit; refuted by hand.)
- **DRAIN-2 (claimed: a stream-wedged resilient viewer is never evicted) —
  REFUTED.** Carrier opens feed the shared `kfConsecOpenFailed` streak
  (`openCarrier`, `hub.go:1961`), and a truly wedged session also stalls its
  parallel keyframe writes → `kfConsecSlow` → `evictStalled`. So the zombie **is**
  evicted (close 4001). Residual nit only: carrier *write* stalls (as opposed to
  *open* failures) don't themselves increment any eviction streak — eviction of a
  write-wedged-but-open-healthy carrier relies on the keyframe path also failing,
  which in practice it does (shared session flow control).
- **INGEST-2 (inner stream readers not tied to the abort signal) — nit.**
  Teardown correctly relies on `wt.close()` erroring accepted receive streams
  (spec-defined); wiring the inner readers to the signal is optional
  belt-and-suspenders, not a leak.

## Review self-caveat

One of the eight finder passes (the reorder-buffer dimension) returned a
placeholder stub instead of real findings, so the reorder buffer was covered
indirectly (via PLAYOUT-1/3, BACKPRESSURE-3, and manual reading) rather than by a
dedicated pass. Manual reading found the reorder buffer internally consistent:
the 256-frame resilient cap comfortably exceeds a 2 s hold (~120 frames @ 60 fps),
`releasableAt` and eviction-order don't fight the playout hold, and with reliable
in-order carriers the `DELTA_GAP_GRACE` path is rarely exercised *within* a
carrier (good). Its resilient shortfalls are all upstream — the buffer only ever
holds what the (capped) playout offset asks for. A dedicated reorder pass is a
reasonable follow-up but is not expected to change the verdict.

---

## Recommended hardening order

Fixes should be **test-first** per `CODE-REVIEW.md` (a failing test before each
fix). Suggested order:

1. **PLAYOUT-1 + PLAYOUT-3** — parameterize the jitter histogram range/window
   into the playout profile (resilient: ~2500 ms range, ~5–10 s window); consider
   an immediate up-step on a large jitter jump. *This is the fix that makes the
   feature do what it claims.* Add a controller test that reaches ~1500 ms.
2. **PRODUCT-3** — reliable-specific queue-overflow metric, so the mode's failure
   is observable before/while tuning the above.
3. **PRODUCT-1** — a relay-integration test driving `?delivery=reliable` under
   injected loss, asserting zero gap resyncs; the regression net for all of this.
4. **BACKPRESSURE-2** — carrier-specific (~1 GOP) write timeout + rotation check
   before the blocking write.
5. **INGEST-1** — prune settled stream tasks.
6. **BW-CHARGE + BACKPRESSURE-4** — charge egress on successful write only;
   include the prologue.
7. **PRODUCT-2** — visible/touch-reachable control + the deferred suggest-banner
   (product track; gates the "for mobile" claim).
8. **BACKPRESSURE-3, WIRE-1, LIFECYCLE-2** — polish.

Re-run the manual X1/X6 netem + real-phone drills after 1–4; the headline
criterion (≈0 gap resyncs at 5 % loss + 50 ms jitter, ≤ 2 s latency) cannot pass
today while the offset is capped at ~534 ms.
