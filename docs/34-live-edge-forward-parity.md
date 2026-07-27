# R29 — Forward parity for live-edge delivery

**Status**: designed 2026-07-27, not started (FP1–FP8).

Broadcaster-side forward error correction for the datagram delta path, so a
live-edge viewer on a lossy link keeps its frames instead of losing whole
GOPs to freeze-on-gap. Two parity chunks per delta frame (`k = 2`), generated
by **both** producers (browser and native), filtered **per subscriber** by the
relay, and paired with a configurable **per-GOP frame-loss allowance**
(default 1) that stops a single unrecovered frame from costing the rest of
the GOP.

Zero relay computation. The relay stays a byte forwarder.

---

## 1. The evidence

This item exists because of a measured session, not a hypothesis. Viewer
session `8591c54d6543752cd2269ec6` (Firefox 154 / macOS, app 0.35.0, broadcast
`b053413bd679`, delivery `datagrams`), read out of R28 telemetry over 553 s /
305 undownsampled samples:

| Signal | Value |
|---|---|
| Frames attempted (`receivedFps` + incomplete drops) | 42.9 fps |
| `framesDroppedIncomplete` | 10.4/s — **24.2% of frames lose ≥1 chunk** |
| `receivedFps` (complete) | 31.7 fps |
| `decoderFps` | 8.0 fps — **18.5% of attempted frames decode** |
| `reorderGapResyncs` | 1.94/s against 1.99 keyframes/s — **~97% of GOPs broken** |
| `framesDroppedLate` | 11 total (negligible) |

The relay's own view of that same subscriber, from `/statusz` on the origin
pod: `dropped: 0`, `sendErrors: 0`, `queueDepth: 0`, `keyframesDropped: 0`.
Broadcast-level `datagramsDropped: 0`. Leg A ingress loss 9 frames in 28,376
(0.03%). **The relay sent every byte.** The loss is entirely in flight on the
relay→viewer path.

### The amplification chain

1. ~3% per-datagram loss on leg B — derived: 2.95 Mbps ÷ 42.9 fps ≈ 8.6 KB per
   frame ≈ 9 datagrams at the 1200 B cap; (1−p)⁹ = 0.758 ⇒ p ≈ 3%.
2. × ~9 chunks/frame ⇒ **24% of frames arrive incomplete** and
   `Reassembler` drops them (`reassembler.ts`, `framesDroppedIncomplete`).
3. × ~20 deltas per 500 ms GOP ⇒ P(clean GOP) = 0.76²⁰ ≈ 0.3%, so effectively
   every GOP contains a hole.
4. Freeze-on-gap (`reorder-buffer.ts`, `waitingForKeyframe = true`) then
   discards **everything after the first hole until the next keyframe**.

A 3% packet-loss link becomes an 81% video-loss experience. The network
destroys 24%; the viewer policy destroys the remaining 57%.

**The policy is not malfunctioning.** Freeze-over-corruption (item 11b,
docs/03) was chosen when gaps were rare, trading a brief freeze for visible
corruption. At 24% frame loss the trade inverts: the viewer sits permanently
at 8 fps, which is worse than the artifacts the policy exists to prevent.

### The loss is i.i.d., and that is what makes per-frame parity work

An i.i.d. model at p = 3% predicts 8.3 decoded fps against a **measured 7.96**,
and 24.0% incomplete against a **measured 24.2%**. A Gilbert-Elliott burst
model with the same 3% mean predicts 16.6 decoded fps — off by 2×. Combined
with R28 TM10 reporting `fpsDipEpisodes: 0` and an incomplete-drop rate whose
p05–p95 spans only 6.2–14.0/s, the loss is stationary and independent.

This matters because per-frame parity is precisely the scheme that burst loss
defeats (a burst inside one frame consumes several of its chunks at once).
The measurement says we are not in that regime. It is also the single
assumption most worth re-checking on other links — see §9.

---

## 2. Goal

A live-edge viewer on a ~3% loss link decodes essentially every frame the
broadcaster sent, at unchanged latency, without switching to Resilient mode
and paying its 0.5–2 s buffer.

Projected against the measured session (Monte-Carlo, 300k GOPs, per-GOP
allowance semantics; "skipped" = frames decoded after an unrecovered
predecessor, i.e. artifact-bearing):

| k | allowance | residual loss/frame | GOP freeze | decoded fps | clean fps | skipped fps | egress |
|---|---|---|---|---|---|---|---|
| 0 | 0 | 23.98% | **99.63%** | 8.3 | 8.3 | 0.00 | 0% |
| 0 | 1 | 23.98% | 97.23% | 14.5 | 8.3 | 1.99 | 0% |
| 1 | 0 | 3.45% | 50.97% | 30.7 | 30.7 | 0.00 | +11% |
| 1 | 1 | 3.45% | 15.56% | 39.4 | 30.7 | 1.02 | +11% |
| **2** | **1** | **0.37%** | **0.26%** | **42.7** | **41.3** | **0.15** | **+22%** |
| 2 | 2 | 0.37% | 0.00% | 42.7 | 41.3 | 0.15 | +22% |

Row `k=2, allowance=1` is the target: GOP freezes 99.63% → 0.26%, decoded
8.3 → 42.7 fps of 42.9 attempted, and only 0.15 fps carrying artifacts.

Note what the table shows about the **allowance alone** (`k=0` rows): it
converts freezes into artifacts but barely moves clean fps, because it does
nothing about the loss itself. Parity is what reduces damage; the allowance
is what stops the residue from being amplified. Neither substitutes for the
other, and the allowance is the cheaper half only in bandwidth, not in value.

---

## 3. Why live-edge only

Resilient and Deep-buffer delivery are structurally immune to this failure.
R19 has the relay re-frame each delta as a length-prefixed record on a per-GOP
**reliable** uni carrier stream (`hub.go`, "reliable and in-order"), so QUIC
retransmits anything lost, the reassembler never sees an incomplete frame,
and freeze-on-gap never fires. They answer "don't lose frames" with "wait for
them", which is exactly the trade live-edge refuses.

So parity is **redundant** in those modes and would be pure waste: the relay
suppresses it for any subscriber not on datagram delivery. This is also why
FEC is the right tool here and was the wrong one for R19 — R19 weighed FEC and
chose carriers because it could afford latency. Live-edge cannot, and a
per-frame code costs none.

---

## 4. Mechanism

### 4.1 RAID-6 style P/Q over GF(2⁸)

For a delta frame split into `n` data chunks `d₀…dₙ₋₁` (zero-padded to the
common chunk payload length):

```
P = d₀ ⊕ d₁ ⊕ … ⊕ dₙ₋₁
Q = (g⁰·d₀) ⊕ (g¹·d₁) ⊕ … ⊕ (gⁿ⁻¹·dₙ₋₁)      g = 2 in GF(2⁸), poly 0x11D
```

This is MDS for `k ≤ 2` — **any** two erasures among the `n+2` transmitted
chunks reconstruct the frame — while needing only 256-entry log/exp tables
and roughly a hundred lines per mirror. A general Reed-Solomon implementation
in both TypeScript and Go would be several times the code and the review
surface for no additional recovery at `k = 2`.

**The prefix property is load-bearing**: `P` alone *is* the `k = 1` code. A
subscriber at `k = 1` takes parity index 0 and nothing else, from the same
computation. This is what makes per-subscriber `k` free on the producer side
(§5) — one computation at the maximum `k`, each consumer taking a prefix.

**Hard bound**: `g^i` has period 255, so the Q coefficients repeat and the code
stops being MDS once `n > 255`. Parity is therefore **not emitted for frames
with `n > 255` chunks** (≈ 300 KB — unreachable for a delta at any sane
bitrate; keyframes are excluded anyway, §4.3). This is an explicit guard, not
an assumption.

**Small frames**: parity symbol count is `min(k, n)`. At `n = 1`, `P` is a
byte-for-byte duplicate of the single chunk, which is genuine (if inelegant)
protection against its loss; a second symbol would add nothing, so it is not
sent.

### 4.2 Wire format — `0x0E TypeParityChunk`

13-byte header, big-endian, mirroring the `VideoChunk` conventions:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version |
| 1 | 1 | type = `0x0E` |
| 2 | 4 | `frameId` (uint32) |
| 6 | 1 | `parityIndex` (0 = P, 1 = Q) |
| 7 | 2 | `chunkCount` — `n`, the data chunk count (uint16) |
| 9 | 4 | `frameBytes` — total encoded frame length (uint32) |
| 13 | … | parity payload |

The header is deliberately **13 bytes, not 20**: a parity payload is as long
as the longest data chunk (`MAX_CHUNK_PAYLOAD` = 1180), so a 21-byte header
carrying a timestamp would produce a 1201-byte datagram and breach the 1200
cap. `frameBytes` is what lets the receiver recover the *last* chunk, whose
length is shorter than the rest.

The consequence of dropping the timestamp is recorded rather than hidden:
**reconstruction requires at least one surviving data chunk**, from which the
assembly already holds `timestampUs` and the keyframe flag. That is satisfied
whenever recovery is possible at all except the degenerate `n ≤ k` case, which
is not recovered by design.

Golden vectors in all three mirrors (`wire_test.go`, `wire.test.ts`,
`gawk-broadcast`'s `wirecheck`), including a full-payload 1200 B boundary case
— the docs/24 finding-15 lesson.

### 4.3 Delta frames only

Keyframes already ride per-subscriber **reliable** uni streams (R8), so they
are not exposed to datagram loss and need no parity. Parity applies to the
datagram delta path and nothing else. This also keeps `n` small and bounded,
which is what §4.1's 255 guard relies on.

### 4.4 `0x0F TypeRelayCapabilities` — the version-skew gate

A new broadcaster against an **old** relay would be a real hazard: `fanOutLocked`
forwards every datagram blindly, so parity chunks would reach viewers that
cannot parse them and would land in `badDatagrams`.

The relay therefore advertises support, and a producer emits parity only when
told to. This needs a **new message** rather than an extra field on
`BroadcastAnnounce`, for two reasons that are already settled elsewhere in the
codebase: the wire parsers are strict, so appending bytes to an existing
message breaks old readers; and the WebTransport JS API cannot read HTTP
response headers, so it cannot ride the connect response.

`0x0F` is a small flags message sent once per session at session start on both
routes, with a reserved extensible flags field. Its first bit is
`CapParityChunks`. A producer that sees no capabilities message assumes no
support and sends no parity — so old relay + new broadcaster is byte-identical
to today.

The reverse direction is free: parity is opt-in per subscriber (§5), so a
viewer that never asks never receives, and a new relay serving old viewers is
byte-identical to today.

---

## 5. Where parity is computed: broadcaster, not relay

**Decision: the broadcaster generates `k = 2`; the relay forwards a per-subscriber
prefix.** The relay computes nothing.

The alternative — relay-side generation — was evaluated and rejected on three
counts:

1. **The relay does not reassemble delta frames today.** `relayVideoChunk`
   parses the header for ingress accounting and forwards immediately
   (`hub.go:2040`). Parity would require accumulating `n` chunks per broadcast,
   plus completion tracking and bounded eviction for frames whose chunks are
   lost on leg A — new state and a new failure surface on the hot path.
2. **`Registry.mu` is process-wide.** It guards the whole `hubs` map
   (`hub.go:487`) and `fanOutLocked` runs under it, so GF arithmetic there
   would serialize parity work for *every* broadcast on the pod against its
   hottest lock. Workable (compute in the publisher's read goroutine, outside
   the lock) but a constraint to design around rather than discover.
3. **It breaks the cascade.** In cluster mode an origin does not know an edge's
   subscribers' `k`, so parity would have to be computed at each serving pod —
   the R19 pattern, but now over a leg that has already suffered origin→edge
   loss, where an edge can only generate parity for frames it received intact.

Broadcaster-side generation avoids all three: parity chunks are just more
datagrams the relay forwards blindly, the cascade works unchanged, and the
"relay stays a byte forwarder" property R19 protected is preserved.

**The cost lands on broadcaster uplink**: +22% on leg A (2.95 → 3.6 Mbps for
the measured session). That is the right leg to spend on — it is the
well-provisioned one, measured at 0.03% loss, and on the reference fleet's
1 gbps symmetric uplink it is noise. It does **not** compete with the encoder:
parity is XOR/GF over already-encoded bytes, not encode work, which matters
because the measured broadcaster is already encoder-overloaded (capture 46 →
encode 34 fps).

### 5.1 Per-subscriber filtering

Per-subscriber is **cheaper than a common setting**, not more expensive:

- **CPU**: parity is per-*frame* work, shared by all subscribers exactly as
  data chunks are (`fanOutLocked` hands every subscriber the same slice — no
  per-subscriber copy). Cost is driven by `max(kᵢ)`, not by subscriber count.
- **Egress**: a common `k` charges every viewer; per-subscriber charges only
  the ones that asked. On a hot broadcast with 1000 viewers of which 10% are
  lossy, common `k=2` adds ~86k datagram sends/s and per-subscriber ~8.6k/s.

The marginal cost is a `parityK uint8` on `Subscriber` and one comparison per
subscriber per parity symbol in the existing loop.

Negotiation follows the R19/`?delivery=` precedent: `?parity=0|1|2` on
`/subscribe/{id}`, acked in-band, **mode change = deliberate reconnect**. The
relay clamps to 0 for any subscriber not on datagram delivery (§3) and for
any broadcast whose publisher is not emitting parity.

### 5.2 The default level — settled

**Owner decision (2026-07-27): `k = 2` is the default, configurable through
the relay chart; `k = 1` and `k = 0` are viewer opt-outs in the right-click
menu.** This overrides the design's initial `k = 1` recommendation, which
optimised for fleet egress; the owner's call is that quality is the point and
+22% on a fleet with 1 gbps symmetric headroom is affordable.

Consequences worth stating plainly, because they follow from the default and
not from the mechanism:

- The default is **quality-first**: a viewer gets full protection without
  finding a menu, which is the population that needs it most (§5.2's original
  argument against opt-in).
- Every live-edge viewer costs +22% egress until it opts down. The chart knob
  (`parity.defaultLevel`, §5.3) is what makes that reversible fleet-wide
  without a client release, and `egress_bytes_total{kind="parity"}` (§7.2) is
  what makes the cost measurable rather than modelled.
- The menu offers **down**, never up: a viewer cannot request more than the
  fleet default, because it cannot conjure parity the broadcaster did not
  emit. Requesting `k` above the publisher's level is clamped and shows as
  `parityActive < parityRequested` (§7.1).

### 5.3 Chart configuration

`parity.defaultLevel` (0 | 1 | 2, default **2**) on the relay chart, plumbed
as `-parity-default` / `GAWK_PARITY_DEFAULT` and **through `registryOptions`
in `cmd/gawk-server/main.go`** — the R2 post-implementation review found knobs
wired only into the test helper, and this doc's acceptance criteria (FP4)
assert the production path explicitly.

The relay's advertised capability (§4.4) carries this level to producers, so
`parity.defaultLevel: 0` disables the feature fleet-wide from one value: the
broadcaster emits nothing, the relay forwards nothing, and the wire is
byte-identical to pre-R29.

---

## 6. The per-GOP frame-loss allowance

Parity reduces how *often* a frame is unrecoverable. The allowance bounds what
one unrecoverable frame *costs*.

**Semantics**: a budget of `N` unrecovered delta frames per GOP (default
`N = 1`, configurable). Within budget, the missing frame is skipped and decode
continues; on exceeding it, the existing freeze-to-next-keyframe behaviour
applies unchanged. The budget resets at every keyframe.

This is deliberately a **per-GOP budget, not a consecutive-run tolerance**.
A budget is what the viewer can actually count (it already tracks position
within a GOP), it degrades predictably as loss rises, and it caps
artifact exposure per GOP at a number the operator chose.

Constraints:

- **Live-edge only.** Resilient/Deep-buffer never see holes, so the allowance
  is inert there and must not be applied — a hole in a carrier stream means
  something else went wrong and freezing is still correct.
- **Honesty about cost.** Decoding past a missing reference produces artifacts
  that persist until the next keyframe. At the target configuration this is
  0.15 fps of 42.9 (§2), but it is not zero and must be visible in stats
  (§7) rather than silently traded away.
- **`gapResyncs` changes meaning.** A skipped-within-allowance frame is not a
  resync. It gets its own counter so the existing signal keeps meaning what
  docs/13's playbook says it means.
- Default `N = 1` is a `PlayoutProfile`-adjacent constant with a viewer
  setting, persisted; `N = 0` reproduces today's behaviour byte-identically,
  which is what makes the change safely revertible at runtime.

---

## 7. Observability

### 7.1 Viewer stats — requested vs. active

The distinction is mandatory, following R19's "reliable requested / datagrams
served" precedent: what a viewer *asked for* and what it is *getting* diverge
whenever the broadcaster does not emit parity, the relay predates R29, or the
subscriber was clamped for delivery mode. A single "parity: 2" row that cannot
tell those apart would be worse than no row.

New `ViewerStats` fields:

| Field | Meaning |
|---|---|
| `parityRequested` | 0 \| 1 \| 2 — what this viewer asked for |
| `parityActive` | 0 \| 1 \| 2 — what is actually arriving |
| `parityChunksReceived` | counter |
| `framesRecoveredByParity` | counter — the headline "is it working" signal |
| `parityRecoveryFailures` | frames where parity was present but insufficient |
| `lossAllowanceFrames` | configured `N` |
| `framesSkippedWithinAllowance` | the artifact-exposure counter (§6) |

Overlay: a **Parity** group in the Delivery section — requested/active,
frames recovered, and the allowance with its skip count. `parityActive = 0`
while `parityRequested > 0` is the signature operators need to see.

Broadcaster stats gain `parityLevel` (emitted) and `parityBytesSent`, on both
producers (`BroadcastStats` and `engine.Stats`).

### 7.2 Relay

`/statusz` `subscriberDetails[]` gains `parityK`; per-broadcast
`parityDatagramsForwarded` / `paritySuppressed`, and
`egress_bytes_total{kind="parity"}` so the fleet cost of the default (§5.2) is
measurable rather than modelled. A docs/13 playbook row for the
`parityActive = 0 while requested > 0` signature.

### 7.3 Telemetry (R28)

All of §7.1 flows to `gawk-telemetry` as **typed known fields** in the rollup
(D15: known fields typed, unknown survive verbatim — a new field must never
reject a batch from a newer client). `parityRequested`/`parityActive`/
`lossAllowanceFrames` join `broadcasterConfig`-style config keys; the counters
join the series.

One new `diagnose()` rule, **`parity-ineffective`**: high
`framesDroppedIncomplete` while `parityActive > 0`. Its action text has to
distinguish the two causes, because they call for opposite responses —
loss above what `k` covers (escalate `k`) versus **bursty** loss defeating a
per-frame code (escalate is useless; the answer is Resilient mode). The
discriminator is `framesRecoveredByParity` relative to
`parityChunksReceived`: a per-frame code failing on bursts recovers almost
nothing, whereas one merely under-provisioned still recovers plenty.

This rule is also how §1's i.i.d. assumption gets continuously re-tested on
real links instead of resting on one session.

---

## 8. Chunks

| Chunk | Work | Acceptance criteria | Status |
|-------|------|---------------------|--------|
| **FP1** | **Wire + codec, three mirrors (pure, test-first)**: GF(2⁸) log/exp tables and P/Q encode + 1-and-2-erasure decode in `gawk-server/wire`, `gawk-app/src/transport/wire.ts`, and `gawk-broadcast`'s `wirecheck`; `0x0E TypeParityChunk` (13 B) and `0x0F TypeRelayCapabilities` encode/parse | Golden vectors byte-identical across all three mirrors, incl. a **full-payload 1200 B boundary** parity datagram with its length pinned as hex (docs/24 finding 15); property test: for every `n ∈ [1, 255]` and every pair of erasure positions among `n+2`, decode reconstructs the original bytes exactly; `n > 255` refuses to produce parity; `n = 1` produces exactly one symbol; strict-parse rejection tests for truncated/oversize/bad-index parity headers; malformed input never panics | not started |
| **FP2** | **Browser producer**: `packetizer.ts` `packetizeFrame` gains parity emission behind the capability gate; `broadcaster.ts` sends parity datagrams after the frame's data chunks; broadcaster setting (default per §5.2) + `BroadcastStats.parityLevel`/`parityBytesSent` | Unit tests over the pure packetizer: `k=0` output byte-identical to pre-R29; `k=2` appends exactly 2 datagrams with correct `frameId`/`chunkCount`/`frameBytes`; parity emitted for deltas and **never** for keyframes; no parity when the capability bit is unset; existing `packetize-reassemble.test.ts` green unmodified; worker and main-thread broadcast paths both covered | not started |
| **FP3** | **Native producer**: mirror in `gawk-broadcast/internal/engine/send.go` (the `packetizer.ts` counterpart); CLI flag + GUI setting + env var; `engine.Stats` parity fields | Go tests mirroring FP2's assertions; a **cross-producer golden test** asserting the browser and native producers emit byte-identical parity for the same frame bytes (the `wirecheck` pattern — this is what stops the two producers drifting); `gofmt`/`vet` clean; GUI builds | not started |
| **FP4** | **Relay per-subscriber filter**: `?parity=` negotiation + in-band ack; `parityK` on `Subscriber`; suppression in `fanOutLocked` for `parityIndex ≥ parityK`, for non-datagram delivery, and when the publisher emits none; `0x0F` capability advertisement to publishers and subscribers; `/statusz` + Prometheus counters | Relay computes no parity (asserted by absence — no GF code imported by `internal/hub`); a `k=1` subscriber receives P and never Q while a `k=2` peer on the same broadcast receives both, from one fan-out; resilient/DVR subscribers receive none; parity never enters the DVR ring or the carrier path; **`-parity` disabled ⇒ `/statusz` and metrics byte-identical to pre-R29** (diff-asserted, the R28 pattern); no measurable added time under `Registry.mu` (benchmark) | not started |
| **FP5** | **Viewer reconstruction**: parity buffering + recovery in `reassembler.ts`; requested-vs-active detection; `ViewerStats` fields; overlay Parity group; `?parity=` request + "⋮" menu control (reconnect on change, R19 precedent) | Test-first: a frame missing 1 of `n` chunks reconstructs and decodes; missing 2 reconstructs at `k=2` and fails cleanly at `k=1` (counted, never thrown); parity for an already-complete frame is discarded without allocation; late parity after the frame is evicted is a no-op; `parityActive` reads 0 when none arrives despite `parityRequested > 0`; **parity-off path byte-identical to pre-R29**; menu control reachable on touch (docs/24 finding 9 — the "⋮" button, not only `contextmenu`) | not started |
| **FP6** | **Per-GOP loss allowance**: budget in `reorder-buffer.ts`, reset at keyframe, live-edge only; viewer setting persisted; `framesSkippedWithinAllowance` + `lossAllowanceFrames` stats | Test-first: `N=0` byte-identical to pre-R29 (the whole existing reorder-buffer suite green unmodified); `N=1` skips one unrecovered frame and continues, freezes on the second, and resets at the next keyframe; the allowance is **inert under resilient/DVR delivery** (test pins the absence); a skipped frame increments the new counter and **not** `gapResyncs`; interaction with `hasBufferedKeyframeAbove` and the restart/backwards-keyframe paths pinned | not started |
| **FP7** | **Telemetry + diagnose**: typed rollup fields for §7.1/§7.2; `parity-ineffective` rule with the burst-vs-underprovisioned discriminator; dashboard surfacing; docs/13 playbook row | Rule fires on a synthetic under-provisioned session and on a synthetic bursty session with **different** action text; does not fire on a healthy parity session; a session predating R29 (no parity fields) still diagnoses without the rule and without `schemaAnomalies`; 32 KB default-response ceiling still met (R28 acceptance criterion) | not started |
| **FP8** | **Verification**: Go loss-injection test reusing the `resilient_loss_test.go` UDP forwarder; R20 tier-1 e2e parity pass; browser + native manual pass; measure against the §1 baseline and evaluate the kill criteria | Go test: 3% relay→viewer loss with a no-parity control on the same forwarder — `k=2` recovers ≥95% of frames the control loses; e2e tier-1 asserts `parityActive == 2` and `framesRecoveredByParity > 0` from Copy-diagnostics; manual pass on a real lossy link reproduces §2's projected row within tolerance; **kill criteria (§9) explicitly evaluated with a recorded verdict** | not started |

Ordering: **FP1 → (FP2 ∥ FP3 ∥ FP4) → FP5 → FP6 → FP7 → FP8.**

FP1 is the gate for everything and is pure/testable in isolation. FP6 is
sequenced after FP5 deliberately: the allowance is only worth tuning once
residual loss is at parity-corrected levels, and shipping it earlier would
tune it against the wrong distribution. FP2 and FP3 are the "both producers"
requirement and must land together or the cross-producer golden test in FP3
has nothing to compare against.

---

## 9. Kill criteria (pre-registered)

Evaluate at FP8. A documented rejection is a valid completion.

1. **Burstiness** — if measured `framesRecoveredByParity` is < ~50% of
   `framesDroppedIncomplete`-avoided on real lossy links, the loss is bursty
   and a per-frame code is the wrong shape. Response: confine R29 to links
   that measure i.i.d. and route bursty viewers to Resilient mode. Do **not**
   respond by raising `k` — §1's burst model shows even `k=4` leaves 37% of
   GOPs damaged.
2. **Cost** — if the fleet egress increase at the chosen default (§5.2)
   exceeds its measured quality gain on the `/v1/fleet` aggregate, drop the
   default to `k=0` and keep it opt-in.
3. **Artifacts** — if the allowance's artifact exposure is judged worse than
   the freezes it replaces in side-by-side viewing, ship `N = 0` as the
   default and keep parity alone (which is still the majority of the win: 41.3
   clean fps at `k=2, N=0` versus 8.3 today).

---

## 10. Non-goals

- **Adaptive/automatic `k` selection.** Deferred: it needs a viewer→relay
  control path and thresholds that only FP8's measurements can set. The R19
  suggest-banner precedent — ship the manual control, learn the thresholds,
  automate later.
- **Relay-side parity generation.** Rejected in §5; do not re-propose without
  new evidence about `Registry.mu` and the cascade.
- **Parity on keyframes.** They are already reliable (§4.3).
- **Parity in resilient / Deep-buffer modes.** Redundant by construction (§3).
- **Cross-frame interleaving** for burst protection. It works, and it costs
  latency — which is the one thing live-edge cannot spend. If bursts turn out
  to dominate, the answer is Resilient mode, not a slower live-edge.
- **`k > 2`.** Beyond the P/Q scheme's MDS range; needs general Reed-Solomon
  in three mirrors, and §9's kill criterion 1 says burstiness — not
  insufficient `k` — is the likelier failure.
- **Relay-driven producer suppression** (telling the broadcaster to stop
  emitting parity when no subscriber wants it, via R18's relay→publisher push
  channel). Cheap and proven machinery, worth doing, but it is an
  optimization on top of a working feature — not v1.
