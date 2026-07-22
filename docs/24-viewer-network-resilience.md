# R19 — Resilient Viewer Mode: Reliable Delivery + Extended Buffering for Lossy Networks

Design doc for [ROADMAP R19](../ROADMAP.md#r19--resilient-viewer-mode-for-lossy-networks)
(designed 2026-07-18; **X2–X5 implemented 2026-07-18**, automated gates green;
X1 measurement baseline + browser spike and X6 verification/tuning done
2026-07-19 — the lossy-network behaviour this mode exists for is **not
CI-reachable** (the R20 E2E runs a clean loopback link with zero loss and
default datagram delivery); see "Implementation status" below). Adds an **opt-in, per-viewer "Resilient
mode"** for viewers on lossy networks (LTE/5G mobile connections): the relay
delivers that viewer's delta frames over **reliable WebTransport uni streams**
instead of unreliable datagrams (QUIC retransmission recovers loss for free),
and the viewer runs an **extended adaptive jitter buffer** (up to ~2 s) that
absorbs the retransmit delays. Smooth video on a bad connection, at the price
of latency — a price the viewer explicitly chose to pay.

Three user decisions anchor the scope (2026-07-18):

1. **Mechanism**: reliable stream delivery + extended adaptive buffering
   (Option A below) — chosen over buffer-only, relay-cache + NACK, and FEC.
   QUIC's built-in ARQ does the loss recovery; we don't hand-roll one.
2. **Latency budget**: adaptive up to **~2 s** — the resilient playout
   controller buffers only what measured jitter/loss actually requires, so a
   good mobile link still sits well under 1 s. The live-edge sub-500 ms
   philosophy is untouched as the default mode.
3. **Activation**: **manual toggle first** — a new, separate right-click
   toggle, default off, persisted (per the every-new-behavior-gets-its-own-
   toggle rule). Auto-detection is a designed-but-deferred follow-up
   (Decision 11): a mode switch requires a reconnect, so full-auto needs an
   oscillation story we should design from field data, not guesses.

**Numbering**: `docs/23` is reserved for R18 (live viewer count, not yet
picked up) — hence R19 and `docs/24`. R17 relay scale-out (`docs/22`,
merged 2026-07-18) holds wire type 0x09 and close codes 4002/4003; both
allocations are respected in Decision 3.

## Goal

A viewer on a train, on hotel Wi-Fi, or on a phone with two bars enables
"Resilient mode" and gets **smooth, artifact-free video** — no
freeze-until-keyframe stutter, no corruption, no keyframe-slideshow
degradation — at 0.5–2 s behind live instead of ~150 ms. Everyone else's
experience is byte-identical: the default mode, the wire seen by other
viewers, the broadcaster (browser and native), and the relay's behavior for
non-resilient subscribers all remain unchanged.

## Background: why lossy links are choppy today

The transport is datagram-first by design (drops over stalls), and every
defense added since assumes loss is *rare*:

- **A single lost delta datagram costs up to ~500 ms of freeze.** A delta
  whose predecessor never arrives can't be decoded; the reorder buffer
  (`gawk-app/src/transport/reorder-buffer.ts`) waits `DELTA_GAP_GRACE_MS`
  (60 ms) then freezes until the next keyframe (500 ms GOP). On a LAN this
  is a rare blip; at mobile-typical 1–5 % bursty loss it fires several times
  per second — the stream becomes a slideshow of freezes exactly when the
  GOP self-healing was supposed to make loss cheap.
- **The adaptive playout offset is sized for LAN jitter.** The R12
  `PlayoutController` (`transport/playout.ts`) clamps at
  `MAX_PLAYOUT_OFFSET_MS = 350` — deliberately inside
  `MAX_BUFFERED_FRAMES = 64` (~1.07 s @ 60 fps) and under
  `KEYFRAME_WAIT_MS = 1000`. Cellular jitter spikes (hundreds of ms on
  handover or scheduler stalls) blow straight through it.
- **Keyframes are already reliable** (R8): they travel on per-subscriber uni
  streams with store-and-forward fan-out. That machinery — and the fact that
  the viewer already merges stream-delivered frames with datagram deltas in
  the reorder buffer — is exactly what this design generalizes.
- **docs/12 Decision 1 said "deltas stay on datagrams — permanently."** Its
  premises: no jitter budget ("a delta is superseded within ~33 ms, so its
  loss is cheap and waiting for it is never worth it") and the only
  alternative considered was *one ordered stream for everything* (true
  head-of-line blocking). Both premises are deliberately inverted here, for
  this opt-in mode only: a viewer with a ~2 s budget has asked to wait, and
  per-GOP streams bound head-of-line effects to one retransmit inside one
  GOP (Decision 4). The default mode keeps the founding choice untouched.
- What loss recovery costs on the links we target: QUIC loss detection +
  retransmit ≈ 1–1.5× RTT; mobile RTT is typically 30–80 ms. A ~100–150 ms
  recovery hiccup vanishes inside a 500–2000 ms buffer. Resilient mode
  converts *random loss* into *bounded delivery jitter* — precisely the
  quantity the adaptive offset already measures (arrival p95 − min) and
  absorbs.

**Constraint — sufficient throughput assumed.** Reliable delivery cannot
create bandwidth: if the link sustains less than the stream bitrate, no mode
helps (the drop policy in Decision 5 degrades at GOP granularity rather than
stalling forever). The target is the common mobile profile: adequate
throughput, ugly loss/jitter.

## Direction survey (settled 2026-07-18)

| Option | Sketch | Verdict |
|--------|--------|---------|
| **A — Reliable streams + extended adaptive buffer** | Relay sends the opted-in viewer's deltas over reliable uni streams (keyframes already are); viewer buffers adaptively up to ~2 s | **Chosen** — QUIC ARQ for free, reuses the R8 stream machinery + R12 controller, zero broadcaster changes |
| B — Bigger buffer only (client-only) | Raise the adaptive ceiling + reorder capacity; no server change | Rejected as primary: absorbs jitter but not loss — lost deltas still freeze-until-keyframe, so truly lossy links stay choppy. **Subsumed**: it ships inside A as the graceful fallback against an old relay (Decision 8) |
| C — Relay frame cache + NACK | Relay keeps a rolling frame cache; viewer NACKs missing frames over the viewer→relay datagram path (TimeSync precedent); relay retransmits | Rejected: hand-rolled ARQ — new wire messages, relay cache memory, loss-of-the-NACK handling, retransmit pacing — to rebuild what QUIC streams already do better (loss detection well under an app-level gap timer) |
| D — FEC parity datagrams | Broadcaster adds parity chunks; viewers reconstruct without round trips | Rejected: best theoretical recovery latency, but the most new machinery (coding in TS *and* Go, overhead tuning per loss profile), spends bandwidth for every viewer on parity most don't need, and docs/17 already surveyed + deferred it. Revisit only if A's field data shows RTT-scale recovery is not enough (recorded trigger) |

## Decisions

1. **Opt-in, per-subscriber reliable delivery — supersedes docs/12
   Decision 1 for this mode only.** The relay keeps serving datagrams to
   every normal subscriber and switches to reliable delivery for
   subscribers that requested it at subscribe time. Nothing changes for the
   broadcaster (either implementation): the lossy leg being fixed is
   **relay→viewer**; the broadcaster→relay leg runs from the gaming PC over
   good uplinks, is already measured by the ingress-loss window
   (`hub/ingress.go`), and stays exactly as is. docs/15 Decision 6 (no
   keyframe-request back-channel) is untouched — nothing here talks back to
   the broadcaster, and no keyframes are produced on request.

2. **Transport shape: a reliable carrier of verbatim delta datagrams — the
   relay stays a byte forwarder.** The relay never reassembles frames
   (deltas exist at the relay only as ~1200 B `VideoChunk` datagram chunks),
   and it must stay that way — relay-side reassembly would duplicate the
   viewer's reassembler in Go and break the byte-forwarder principle for one
   subscriber class. Instead, a **carrier stream** conveys the *same bytes*
   reliably: for a resilient subscriber, each delta datagram that would have
   been sent via `SendDatagram` is instead appended to the subscriber's
   current carrier uni stream as a length-prefixed record. The viewer feeds
   each record into the **existing datagram path** (reassembler → dedup →
   watermark → reorder buffer) unchanged — loss simply stops happening.
   Keyframes keep their existing reliable stream path *completely
   unchanged*, including store-and-forward, supersede, and the 4001
   eviction backstop.
   - *Rejected alternative — whole-frame `StreamFrame` emission for deltas*:
     requires the relay to reassemble frames from chunks (new mechanism,
     ingress-loss edge cases, byte-forwarder violation) and the viewer's
     stream reader to grow a delta branch, for no benefit over the carrier —
     the viewer reassembles either way, and its reassembler is already
     battle-tested.

3. **Wire: no new datagram messages; one new stream-kind discriminator +
   record framing, golden-vectored.** Uni streams carry no metadata, and the
   keyframe stream's first two bytes are `version ‖ TypeStreamFrame`
   (`0x01 0x04`) — a carrier stream must be distinguishable, and a bare
   leading length prefix would be ambiguous. The carrier therefore opens
   with a two-byte prologue `version ‖ TypeReliableCarrier`, followed by
   records of `uint16 length (BE) ‖ verbatim datagram bytes` (each record is
   a complete, already-golden-vectored `VideoChunk` datagram ≤ ~1220 B —
   uint16 is ample). The type byte is **allocated at implementation time as
   the next genuinely free value** (0x07/0x08 are reserved by R15 audio,
   0x09 is R17's resume token — **allocated as 0x0A, `TypeReliableCarrier`,
   2026-07-18**), mirrored in all
   three wire implementations
   (`gawk-server/wire/wire_test.go`, `gawk-app/.../wire.test.ts`,
   `gawk-broadcast/internal/wirecheck`) with golden vectors for the
   prologue + record framing. No datagram format, no existing message, and
   no close-code semantics change (4000 terminal / 4001 non-terminal stay
   as-is; 4002/4003 are R17's).

4. **Carrier granularity: one stream per GOP, rotated at keyframe fan-out.**
   Three candidates, decided in favor of per-GOP with X1 confirming:
   - *Per-GOP (chosen)*: the relay rotates a resilient subscriber's carrier
     when it fans out a keyframe to them (~2 streams/s at the 500 ms GOP).
     In-order within the carrier matches decode order; head-of-line effects
     are confined to one GOP and absorbed by the buffer; and **rotation is
     the drop point** — the mechanism that preserves drops-over-stalls
     (Decision 5). Stream churn is negligible (the keyframe path already
     opens one stream per GOP per subscriber; this doubles it).
   - *Per-frame streams*: no head-of-line coupling at all, but ~60 streams/s
     per subscriber sustained — browser stream-credit behavior at that rate
     is unproven (the R10 zombie finding showed credit exhaustion is real),
     and it buys nothing the buffer doesn't already absorb.
   - *One long-lived stream*: no rotation churn, but no drop point either —
     under sustained undersupply the stream falls arbitrarily far behind
     with no way to shed except session teardown. Rejected.
   - Rotation is keyed on keyframe fan-out and is deliberately best-effort:
     a delta chunk of GOP *k* that reaches the hub before keyframe *k*
     finishes store-and-forward ingest may land on carrier *k−1*. Harmless —
     the viewer's reorder buffer sorts by frameId regardless; carrier
     assignment only affects drop granularity. Exact trigger point is an X2
     implementation detail.

5. **Relay fan-out: same queue, different drain; drops-over-stalls at GOP
   granularity.** The resilient subscriber reuses the existing bounded
   per-subscriber queue (`QueueDepth` 256, drop-newest on overflow — the
   enqueue side is untouched); its `drain()` writes carrier records instead
   of calling `SendDatagram`. Datagram sends to resilient subscribers are
   suppressed entirely (no double-send). Backpressure policy:
   - A carrier whose GOP has been superseded (rotation happened) but which
     still hasn't drained after a write deadline (reuse
     `KeyframeWriteTimeout`) is `CancelWrite`-ed — the viewer loses the tail
     of a stale GOP and resyncs at the keyframe it already reliably has.
     **≤ 2 carriers open per subscriber** (current + draining predecessor).
     **Amended 2026-07-22** (finding 12 below): the deadline is *not*
     `KeyframeWriteTimeout` but `CarrierWriteTimeout` (500 ms — one GOP),
     because a record write blocks the whole subscriber's delta drain while
     a keyframe write blocks nobody. Still no new knob.
   - The existing consecutive-open-failure eviction (threshold 10, close
     code 4001) extends to carrier opens — a zombie resilient subscriber is
     evicted the same way a zombie keyframe subscriber is today.
   - **Egress bandwidth cap applies**: carrier bytes pass
     `consumeBandwidth` exactly like keyframe stream bytes do today
     (`sendKeyframe` precedent) — reliable delivery must not become a cap
     bypass. Charged once per record, for the record plus the prologue of
     the carrier that record opens, and only after the drain has decided
     the record is actually going out (finding 13).
   - **No new knobs in v1**: `QueueDepth`, `KeyframeWriteTimeout`,
     subscriber caps and the bandwidth cap cover the resilient path with
     their existing values (any new knob would cross `registryOptions` in
     `cmd/gawk-server/main.go` + `GAWK_*` env + Helm per the R2 rule —
     recorded here so X2 doesn't forget; revisit-if trigger: field data
     showing resilient subscribers need different queue depths).

6. **Subscribe-time negotiation via a query param.** The viewer opts in with
   `CONNECT /subscribe/{id}?delivery=reliable` — the publish-secret
   precedent (`r.URL.Query().Get` in `handleSubscribe`,
   `internal/transport/server.go`), because the WebTransport JS API cannot
   set headers. The flag threads into `Registry.Subscribe` → a per-
   `Subscriber` mode. Unknown values fall back to datagram delivery.
   Changing mode mid-session = reconnect (Decision 9); a datagram-mode
   subscriber never silently morphs.

7. **Viewer resilient profile: wider buffer, RTT-scale gap patience, a
   2 s adaptive clamp.** All named constants, applied only while resilient
   mode is active (defaults untouched otherwise); values are provisional
   until X6's measurement pass:
   - `RESILIENT_MAX_BUFFERED_FRAMES = 256` (vs 64): a 2 s budget at 60 fps
     is 120 frames before headroom. Memory stays trivial — these are
     *encoded* frames (~2 MB at 8 Mbps for a full 2 s).
   - `RESILIENT_DELTA_GAP_GRACE_MS = 250` (vs 60): within one carrier,
     records arrive in order — a missing predecessor on the *same* carrier
     is genuinely gone and waiting is still pointless. But across a
     rotation, the draining predecessor carrier can trail the new one by a
     retransmit (~RTT), so cross-carrier stragglers are real and worth
     RTT-scale patience.
   - `RESILIENT_KEYFRAME_WAIT_MS = 2000` (vs 1000): a ~236 KB
     store-and-forwarded keyframe on a throttled link deserves the same
     patience the rest of the budget gets.
   - **`PlayoutController` resilient clamp `[150, 2000]` ms, seed 500,
     slew up 100 ms/s / down 10 ms/s** (vs `[50, 350]`, seed 150, 50/5).
     The formula is unchanged — `clamp(arrivalP95 − min + headroom)` — only
     the profile widens: retransmit stalls inflate arrival p95, so the
     offset grows exactly when loss is happening and shrinks (slowly,
     dwell-gated) when the link cleans up. ~~The existing
     `WindowedQuantileTracker` needs no changes.~~ **Wrong — superseded
     2026-07-22** (finding 8 below): its 500 ms histogram range capped the
     measured jitter, pinning this clamp at ~534 ms. The profile now carries
     `quantileRangeMs` / `jitterWindowMs` / `stepUpAboveMs` too.
   - Resilient mode **implies adaptive pacing with this profile** while
     active. The existing playout-mode setting (`off`/`fixed`/`adaptive`)
     keeps its stored value and its semantics untouched; the right-click
     menu annotates the paced/smooth entries as governed by Resilient mode
     while it's on, and they regain effect the moment it's off. (Own-toggle
     rule respected: no existing toggle changes meaning; one new toggle
     appears.)
   - Everything downstream is unchanged by construction: the
     `PacedPresentationSink`, R12 interpolation, the R16 tee, decoder-queue
     bounds, and every drop/resync policy keep operating on exactly the
     same inputs — they never learn which transport delivered the bytes.

8. **Graceful degradation against an old relay.** A relay that predates X2
   ignores the query param and serves datagrams; the viewer detects the
   absence of carrier streams and reports mode
   `reliable requested / datagrams served` in stats while keeping the
   resilient buffering profile — which alone is Option B, still a real
   improvement on jittery (if not lossy) links. No version negotiation
   protocol; observability over ceremony.

9. **Activation: one new toggle, mode change = clean reconnect.**
   ~~Right-click menu:~~ **"Resilient mode (mobile networks)"** — reached
   from the viewer menu, persisted under
   `gawk:resilient-mode`, default off. **Right-click-only was wrong —
   amended 2026-07-22** (finding 9 below): touch devices have no such
   gesture, so the mode built for phones was unreachable on phones. The
   control bar now carries a visible overflow ("⋮") button that opens the
   same menu for every pointer type. Toggling tears the session down
   deliberately and reconnects with/without `?delivery=reliable` through the
   existing `ViewerSession` machinery (a fresh start, not the backoff path);
   reconnects preserve the mode. The worker learns the mode via a command in
   the session-start message (alongside the existing `playout` command
   plumbing — `workerViewerController` → `viewer.worker.ts`). With the
   toggle off, every code path is byte-identical to today.

10. **Observability.** Viewer (`ViewerStats` + Delivery section of the
    overlay): delivery mode row (`datagrams (live-edge)` /
    `reliable (resilient)` / the Decision 8 fallback string), resilient
    playout offset (the existing Playout row shows the live value), carrier
    streams opened / records received, carrier-drop resyncs. All fields ride
    Copy diagnostics. Relay: per-subscriber `reliable` flag + carrier
    counters in `/statusz` `subscriberDetails`; Prometheus gains
    `gawk_broadcast_reliable_subscribers` and carrier byte/drop counters via
    the existing snapshot-collector path (`Registry.Stats()` stays the one
    source of truth).

11. **Auto-detection: designed sketch, deferred implementation.** The
    signals already exist per-viewer: sustained `framesDroppedIncomplete`
    rate, `reorderGapResyncs` rate, `arrivalJitterMs`. The deferred design
    is a **suggest-banner** ("Connection looks lossy — enable Resilient
    mode?") with hysteresis (e.g. ≥3 gap resyncs within 30 s, banner at most
    once per session), never a silent flip: a mode change is a visible
    reconnect, and an auto-flipping transport that oscillates on a
    marginal link is worse than either steady state. Full-auto switching is
    explicitly out of v1; X6's field data feeds the eventual thresholds.

## End-to-end path (resilient subscriber)

```
Broadcaster (any, unchanged):
  keyframes → publisher uni streams;  deltas → VideoChunk datagrams

Relay:
  ingest unchanged (S&F keyframes, verbatim datagram dispatch)
  resilient Subscriber:
    keyframe fan-out  → existing per-sub keyframe stream (unchanged)
                      → rotate carrier (open new; predecessor drains,
                        CancelWrite on deadline; ≤2 open)
    delta datagram    → existing bounded queue → drain writes
                        uint16 len ‖ datagram onto current carrier
    (SendDatagram suppressed; bandwidth cap charged per record)

Viewer (worker pipeline):
  incoming uni stream → peek bytes 0–1:
    0x01 0x04 (StreamFrame)      → existing keyframe path, unchanged
    0x01 TypeReliableCarrier     → read records → existing datagram
                                   handler (reassembler → reorder buffer)
  reorder buffer (resilient profile) → adaptive playout [150, 2000] ms
  → PacedPresentationSink / interpolation / tee — all unchanged
```

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| X1 | **Measurement baseline + spike** — reproducible lossy-link harness (`tc netem` loss 1/3/5/10 % × jitter 0/50/100 ms on the viewer leg, docs/12 precedent); record default-mode failure signatures (gap resyncs/min, renderCadence p95, freeze visibility vs loss rate); browser spike for the carrier reader (sustained uni-stream accept + in-worker read throughput on Chrome, Firefox, iOS Safari — the primary resilient audience is a phone) | Findings table in this doc: default-mode signature per netem cell; measured stream-accept behavior confirming per-GOP rotation (~4 streams/s incl. keyframes) is comfortably inside browser limits on all three engines (else the granularity decision is revisited **here, before X2**); harness commands recorded verbatim | ✅ done 2026-07-19 (owner verification on real browsers + netem; not CI-reachable) — see "Implementation status" for the ordering deviation |
| X2 | **Wire + relay** — carrier prologue + record framing (type byte allocated per Decision 3) with golden vectors in all three mirrors; `?delivery=reliable` parsing in `handleSubscribe` → per-`Subscriber` mode; carrier writer drain, per-GOP rotation, ≤2-open + deadline `CancelWrite`, eviction-streak reuse, datagram suppression; bandwidth-cap accounting; `/statusz` + metrics per Decision 10 | Golden vectors byte-identical Go↔TS↔wirecheck; hub tests: resilient sub receives every enqueued delta byte-identically and in order across a rotation; datagram path proven silent for resilient subs; mixed audience (resilient + normal subs on one broadcast) each get their own delivery untouched; stalled carrier `CancelWrite`-ed after deadline and playback story resumes at next GOP; carrier opens feed the eviction streak; cap counts carrier bytes (test: over-cap drops recorded under the right reason); zero new knobs confirmed or any new knob traced flag→env→`registryOptions`→Helm | ✅ implemented 2026-07-18 — all criteria covered by `hub_test.go` + `server_test.go` + wire tests; **zero new knobs** (`QueueDepth`/`KeyframeWriteTimeout`/caps reused) |
| X3 | **Viewer ingestion + resilient reorder profile** — stream-kind peek in the uni-stream reader (`connection.ts`), carrier record loop feeding the existing datagram handler (nested transport worker + in-process transport both); `RESILIENT_MAX_BUFFERED_FRAMES` / `RESILIENT_DELTA_GAP_GRACE_MS` / `RESILIENT_KEYFRAME_WAIT_MS` applied via mode, defaults untouched | Unit tests: carrier records traverse reassembler → reorder identically to datagram delivery (same bytes in, same frames out); keyframe streams still parse unchanged; interleaved carrier + keyframe arrival resolves by frameId; a `CancelWrite`-truncated carrier yields exactly one resync at the next keyframe (no corruption feed); profile constants active only in resilient mode (default-mode tests all pass unmodified); malformed prologue/record counts bad + closes stream without wedging the read loop | ✅ implemented 2026-07-18 — `readServerStreams` dispatches by prologue; records enter `cb.onDatagram` (both transports by construction: the nested worker runs the same `LocalViewerTransport`); resilient constants live in `transport/resilient.ts` |
| X4 | **Mode toggle + playout profile** — "Resilient mode (mobile networks)" context-menu toggle, `gawk:resilient-mode` persistence, deliberate reconnect with/without the query param via `ViewerSession`; worker mode command; `PlayoutController` resilient profile (`[150, 2000]`, seed 500, slew 100/10) selected by mode; paced/smooth menu-entry annotation while active | Toggle flips reconnect with the correct subscribe URL (test at the session/URL seam); controller tests for the resilient profile (clamp, seed-until-warmup, asymmetric slew — same test shape as R12 T3); mode off ⇒ behavior byte-identical to today (existing suites untouched); playout-mode setting value survives resilient on/off round-trip; Decision 8 fallback state reported when no carrier appears | ✅ implemented 2026-07-18 — mode rides `ConnectOptions.deliveryMode` into the subscribe URL; worker `resilient` command sent before `start`; `PlayoutController` takes a profile (`RESILIENT_PLAYOUT_PROFILE`) |
| X5 | **Observability** — Delivery-mode overlay row + carrier counters + resilient offset surfaced (viewer); Copy-diagnostics fields; relay `/statusz` `subscriberDetails.reliable` + carrier counters; Prometheus `gawk_broadcast_reliable_subscribers` + carrier families via the snapshot collector | Overlay renders mode truthfully in all three states (datagrams / reliable / requested-but-datagrams); diagnostics JSON round-trips the new fields; metrics visible on `/metrics` with bounded cardinality (no per-subscriber labels — R9 rule); docs/13 bottleneck playbook gains a resilient-mode signature row | ✅ implemented 2026-07-18 — overlay "Delivery mode" + carrier rows; `ViewerStats` fields ride Copy diagnostics; Prometheus `gawk_broadcast_reliable_subscribers`, `carrier_streams_total`, `carrier_records_total`, `carrier_records_dropped_total`, `egress_bytes_total{kind="carrier"}`; playbook row added |
| X6 | **Verification + tuning pass** — the X1 harness re-run in resilient mode; real-phone LTE/5G session against the homelab; every provisional constant (Decision 7) confirmed or amended; findings + auto-detect thresholds recorded in this doc; README gotchas + ROADMAP/CLAUDE status sync | **Headline criterion**: at 5 % loss + 50 ms jitter, resilient mode plays with ≈0 gap resyncs/min and renderCadence p95 within the R12 clean-link envelope at ≤ 2 s capture→render latency, where default mode measurably stutters (X1 baseline) — measured with the existing latency + cadence instruments, no new measurement code; phone-on-LTE session verdict recorded; constants table updated with final values; auto-detect banner thresholds proposed from data | ✅ done 2026-07-19 (owner verification; X1 harness + real-phone LTE session — not CI-reachable) |

## Implementation status & findings (2026-07-18)

X2–X5 were implemented together on 2026-07-18 with all automated gates green
(both Go modules `-race`, the full vitest suite, tsc, lint, production
build). Notes and deviations:

1. **Ordering deviation — X2 landed before X1.** X1's netem baseline and the
   three-engine browser spike need real browsers and a lossy-link rig, which
   the implementation environment didn't have. The carrier granularity
   therefore follows the design default (per-GOP, Decision 4) without X1's
   empirical confirmation. The risk is contained: the rotation trigger is a
   single hook in `Subscriber.sendKeyframe` and the drop point is isolated in
   `drainReliable`, so if X1/X6 find per-GOP rotation problematic on any
   engine, revisiting the granularity touches only the relay drain. **X1
   remains a gate for X6's verdict, not for merging the dormant code** — with
   the toggle off every path is byte-identical to before.
2. **Carrier concurrency shape.** The implementation is stricter than the
   "≤ 2 open carriers" bound: the drain goroutine owns all carrier stream
   I/O sequentially (open/write/close), so at most one carrier is ever being
   written; a rotation gracefully closes the predecessor before the next
   record. Each record write carries a deadline — a stalled write cancels the
   carrier (a half-written record is unrecoverable framing) and the GOP's
   remaining records are dropped until the next rotation. `Subscriber.Close`
   cancels the current carrier under its own mutex so a blocked drain unblocks
   immediately. **Amended 2026-07-22** (finding 12 below): that deadline was
   `KeyframeWriteTimeout` and is now `CarrierWriteTimeout` (500 ms), shared by
   the lazy open's prologue and the record write so one dequeued delta stalls
   the drain for at most one GOP total.
3. **Eviction-streak cadence.** Carrier opens share the keyframe streak, and
   at most one carrier open is attempted per rotation (an open failure marks
   the GOP dead), so the streak grows at GOP cadence — a zombie with both
   stream kinds failing evicts at ~2.5 s (10 combined failures at 500 ms
   GOP), same order as the keyframe-only path.
4. **Bandwidth-cap accounting.** Over-cap carrier records count as
   *datagram* bandwidth drops (same counters as the datagram drain), keeping
   R9's queue_full-by-subtraction intact; `carrier_records_dropped_total` is
   reserved for dead-carrier/open-failure drops. **Amended 2026-07-22**
   (finding 11 below): a reliable subscriber's *queue* overflow — dropped
   before it could become a record — is neither of those, and now has its
   own `reason="carrier_queue_full"` slice of the same drop total.
5. **Delivery-mode ground truth.** The viewer reports
   `reliable-requested` (Decision 8) until the first carrier stream is
   actually observed — which also covers the first instants after connect,
   before the prime keyframe rotates carrier #1 in.
6. **Prologue peek.** The uni-stream reader accumulates the two stream-kind
   bytes before dispatching (they may span reads); unknown kinds are counted
   as malformed and cancelled without wedging the accept loop.
7. **CI finding (2026-07-19): carrier tests must deadline-bound their reads
   and tolerate delta loss.** `TestSubscribeReliableDeliversCarrierRecords`
   hung for the full 10-minute package timeout on a GitHub runner (run
   29663246454): a delta datagram was lost publisher→relay (runners cap
   `net.core.rmem_max` at 2 MiB vs the ~7 MiB quic-go asks for — the
   warning is right there in the job log), so the carrier legitimately sat
   one record short with the stream open, and the test's carrier `Read`
   had no deadline (the accept-context timeout bounds only
   `AcceptUniStream`). Reproduced locally by skipping one delta send —
   identical stack. The test now sets `SetReadDeadline` on every accepted
   stream, resends the deltas at 250 ms until both are observed, and
   matches records by byte equality instead of position (resends
   duplicate; the relay forwards verbatim). CI additionally raises the UDP
   buffer sysctls in every job that pushes QUIC over loopback.
8. **Post-review fix (2026-07-22): Decision 7's adaptive buffer topped out
   at ~534 ms — `PLAYOUT-1` (high) from
   `docs/reviews/resilient-mode-review.md`.** Decision 7 above says "the
   existing `WindowedQuantileTracker` needs no changes"; that is the one
   sentence in this design that was wrong, and it made the headline
   capability structurally unreachable. The tracker is a **fixed-range
   histogram** (`live-edge.ts`, `QUANTILE_RANGE_MS = 500`) whose samples
   clamp into the top bin, so the arrival jitter feeding the controller
   saturates at ~500 ms and `clamp(jitter + 34, [150, 2000])` could never
   exceed **534 ms** — the entire upper half of the envelope was dead, and
   the ≥ 500 ms retransmit stalls the mode exists to absorb were exactly
   the ones it under-buffered. The clamp bites hardest in the very shape
   this mode targets: a stall holds a burst of deltas, then delivers them
   at once, so the held frames and the fresh ones share an arrival bucket
   and the deep ones clamp against its 500 ms wall. Amendments to
   Decision 7, all inside `PlayoutProfile` so nothing changes with the mode
   off:
   - **`quantileRangeMs`** — 500 default, **2500** resilient (> `maxMs`, so
     p95 stays honest to the top of the clamp). A profile must be able to
     measure its whole clamp range; a test now asserts that invariant for
     both profiles so it can't rot back.
   - **`jitterWindowMs`** — 60 s default, **8 s** resilient. The 60 s p95
     window is `PLAYOUT-3` (low) from the same review: a seconds-scale
     handover spike sits under the p95 of a minute of clean samples and
     barely moves the offset. The **min tracker keeps its 60 s window** —
     it is also `releasableAt`'s anchor, and `offset ≈ p95 − min₆₀` is what
     makes the release schedule land at the measured p95; measuring the two
     terms over different baselines would make the schedule and the offset
     disagree. Down-direction memory stays where it belongs, in the dwell +
     slew, not in how fast a bad episode ages out.
   - **`stepUpAboveMs`** — `Infinity` default (unchanged behavior),
     **150 ms** resilient: a rise larger than that is taken in one step
     instead of slewed. Slewing 1 s of new buffer in at 100 ms/s spends 10 s
     stuttering on the way, which is the stutter the mode exists to remove;
     the alternative to one pause is frames missing their slot and freezing
     to the next keyframe. **Down is still never stepped** (a step down is a
     visible skip) and small rises still slew. This supersedes the
     always-slew reading of Decision 7's "slew up 100 ms/s", whose values
     were provisional pending X6 in any case.

   Cost check: the resilient histogram is **smaller** than the default one
   (626 bins × 9 buckets = 5,634 cells vs 126 × 61 = 7,686) — the shorter
   window more than pays for the wider range. Geometry is fixed per
   histogram, so unlike the other resilient bounds it can't be read per use;
   the reorder buffer rebuilds its tracker when the active profile changes,
   which in production never fires (a mode flip is a reconnect, Decision 9).
   **Re-run the X6 headline criterion after this** — it could not have been
   met at a 534 ms ceiling, so the ✅ on X6 covers the mode's behavior on the
   links it was measured on, not the deep-stall claim.

9. **Post-review fix (2026-07-22): the mobile mode was unreachable on
   mobile — `PRODUCT-2` (high, product-fit) from
   `docs/reviews/resilient-mode-review.md`.** Decision 9 put the toggle in
   the right-click menu and stopped there. The control bar
   (`ViewerScreen.tsx`) carried stats/fullscreen/leave icons and no way into
   that menu, and a right-click is the one gesture a touch device does not
   have: on the phones this whole mode exists for, "Resilient mode (mobile
   networks)" — plus the playout modes, mute and Copy link — had no reliable
   entry point at all. Long-press → `contextmenu` is an Android-Chrome
   behavior, not a guarantee (iOS Safari does not fire it), so the feature's
   entire target population depended on a gesture half of them lack.
   Fix, viewer-UI only:
   - **Overflow button in the control bar** (`MoreIcon`, `aria-haspopup`,
     labelled "More options") opening the *same* `ContextMenu` with the same
     items — no second menu to keep in sync. Right-click keeps working
     unchanged.
   - **`ContextMenu` grew an `anchor` prop.** Its coordinates were always a
     pointer position it grows down-right from; anchored that way to a
     button in the *bottom* bar, the viewport clamp pushes the menu straight
     back up over the button that opened it (caught in a real browser —
     Playwright reported the menu intercepting pointer events on the
     button). `anchor: 'bottom-right'` treats (x, y) as the corner the menu
     grows *up-left* from, so it opens above the bar, right-aligned to the
     button.
   - **`ContextMenu` grew an `anchorRef` prop**, and this is the subtle one.
     The menu closes on any outside `pointerdown`, which includes the
     button's own — so the ensuing click re-opens what the pointerdown just
     closed and the button can never dismiss its own menu. Whether that is
     even *visible* depends on when React flushes the close between the two
     listeners: jsdom (`fireEvent` inside `act`) defers it and the naive
     "was it open at pointerdown?" guard passes, while Chrome runs a
     microtask checkpoint between listeners, flushes, and the guard reads
     the already-closed state. Excluding the anchor from "outside" removes
     the race instead of timing it; the click then sees a still-open menu
     and closes it.
   - **Placement measures the layout box from a neutral corner.** Two
     browser-only traps, both invisible in jsdom (which measures everything
     as 0): the open animation starts at `scale(0.97)`, so
     `getBoundingClientRect()` under-reports by 3 % and drifts the menu onto
     its anchor (now `offsetWidth`/`offsetHeight`); and a fixed element's
     shrink-to-fit width is computed against the space between its `left`
     and the viewport edge, so measuring it where it was *asked* to appear
     can return a squeezed size that then feeds the placement math (now
     measured at the pad corner, hidden until placed).
   - **Touch ergonomics:** `.item` padding grows under
     `@media (pointer: coarse)` only (a mis-tap on a delivery-mode entry
     costs a reconnect), and the menu gains `max-width: calc(100vw - 16px)`
     so R19's long "— governed by Resilient mode" labels wrap instead of
     running off a 360 px screen. Mouse surfaces render byte-identically.

   The control bar itself was already touch-safe: `useAutoHide` reveals on
   `pointerdown`, so a tap brings the bar (and the button) back. Verified in
   headless Chrome at 1280×800, 390×844 (touch taps, not mouse clicks) and
   320×568: the button renders, the menu opens above it and fully on screen,
   a second tap dismisses, an outside tap dismisses, right-click still
   opens, and toggling Resilient mode from the button persists and
   reconnects.
   **Still deferred: Decision 11's suggest-banner.** This fix makes the mode
   *reachable*; a viewer who does not know it exists still has to open the
   menu. The banner stays deferred deliberately — its hysteresis thresholds
   were to come from X6 field data, and X6 ran against the ~534 ms buffer
   finding 8 fixed, so that data has to be re-taken before it can set
   thresholds that flip a transport mode.

10. **Post-review fix (2026-07-22): the carrier path had no automated
    coverage under loss — `PRODUCT-1` (medium) from
    `docs/reviews/resilient-mode-review.md`.** X2/X3's tests are real, but
    they run against in-memory fakes (`hub_test.go`'s `fakeCarrierStream`) or
    a **zero-loss** loopback (`TestSubscribeReliableDeliversCarrierRecords`),
    and the R20 browser harness subscribed in default datagram mode only. So
    nothing automated exercised the one claim the mode is built on — *the
    deltas a datagram viewer loses, a reliable viewer still gets* — and
    nothing exercised the browser's carrier reader or the
    `?delivery=reliable` negotiation end to end. A regression that silently
    degraded carriers back to lossy delivery would have shipped green (it
    now does not: see the mutants below). Two additions, no production code
    touched:
    - **Relay integration test under injected loss**
      (`gawk-server/internal/transport/resilient_loss_test.go`). A userspace
      UDP forwarder sits in front of the relay and drops 15 % of the packets
      travelling **relay → subscriber** — R19's actual failure geometry;
      the publisher stays wired straight to the relay, so ingress is clean
      and every absence downstream is attributable to the injected loss.
      Two subscribers behind the same forwarder, one `?delivery=reliable`
      and one plain, make the datagram viewer a same-conditions **control**
      rather than a separate experiment. Two GOPs, so a carrier rotation —
      R19's designated drop point — happens while the link is lossy.
      Asserted: every relayed delta arrives as a record, byte-identical,
      no duplicates, no holes; `CarrierRecordsDropped == 0`; and the control
      loses strictly more than the carrier did. Typical run: carrier 96/96,
      control missing 15–17 of 96, ~1.1 s wall under `-race`.
      **Ordering across carriers is asserted by content, not accept order** —
      webtransport-go does not deliver accepted streams in the order the
      peer opened them (docs/22 finding 9), so the runs are sorted by their
      first record and the assertion is that the concatenation is still
      strictly ascending, which an interleaving carrier cannot satisfy.
      `tc netem` was not an option (CI runners are unprivileged containers
      with no NET_ADMIN) and is not needed for a downlink-only drop model.
    - **Resilient viewer pass in the R20 tier-1 harness** (`e2e/run.mjs`,
      docs/25 finding 16). The Go test owns behaviour-under-loss; only a real
      browser can show that the production viewer *negotiates* the mode and
      that its own carrier reader turns those uni streams back into frames.
      A second viewer scenario seeds `gawk:resilient-mode` before app boot
      and asserts, flow-shaped, that `deliveryMode` reads `reliable` (not
      the `reliable-requested` degradation), that carriers rotate, and that
      records keep arriving between the two diagnostics captures. It reuses
      the running relay/publisher/preview, so it costs one browser session.
      No loss is injected there — deliberately, since it would trade the
      Go test's determinism for CI flake.

    **Test-the-test** (per `CODE-REVIEW.md`; each mutant reverted after):
    negotiation regressed to datagram delivery → red on
    `ReliableSubscribers = 0`; carrier degraded to unreliable datagrams
    (`isAudioDatagram` → always true) → red on "missing 96 of 96 relayed
    deltas" + `CarrierStreams = 0`; carrier silently dropping every 8th
    record → red on "missing 12 of 96"; **loss injection disabled → red**
    ("the lossy link dropped nothing — the test proves nothing"), which is
    what keeps the test from passing vacuously if the forwarder ever stops
    dropping. Browser side: seeding the toggle off → red on all four
    resilient assertions.

    What this still does not cover: the resilient *playout* profile end to
    end (finding 8's buffer envelope is unit-tested only), and real cellular
    behaviour — X6's re-run remains a manual, owner-verified drill.

11. **Post-review fix (2026-07-22): the mode's dominant failure was
    invisible in metrics — `PRODUCT-3` (medium) from
    `docs/reviews/resilient-mode-review.md`.** When a reliable subscriber's
    carrier drain falls behind (sustained congestion, repeated write
    stalls), its bounded queue overflows in `enqueueLocked` — and that
    incremented only the generic `dropped` counter, surfacing as
    `gawk_broadcast_datagrams_dropped_total{reason="queue_full"}`,
    byte-identical to a normal slow *datagram* viewer. But the two are not
    the same failure: the hole this punches lands in a stream the viewer
    treats as reliable and in-order, so it freezes to the next keyframe —
    exactly the stutter the mode exists to prevent — while the datagram
    viewer's queue_full is business as usual. An operator could not tell
    "resilient mode is failing this viewer" from "this viewer's link is
    slow", which is the question the whole feature's dashboards exist to
    answer. Decision 4's bandwidth-cap accounting had made the analogous
    call correctly (over-cap records count as *datagram* bandwidth drops);
    the queue overflow simply had no bucket of its own. Fix, relay only,
    no wire/viewer/broadcaster change:
    - **Its own reason bucket**: `enqueueLocked` increments a new
      per-subscriber `carrierQueueOverflow` when `s.reliable`, exposed as
      `…_datagrams_dropped_total{reason="carrier_queue_full"}` on both the
      per-broadcast and relay-lifetime families.
    - **The drop budget stays whole.** The overflow still counts in
      `dropped`, so `carrier_queue_full` is a *slice* of the datagram-drop
      total, not a second budget; `queue_full` remains derived by
      subtraction (R9), now minus the carrier slice as well as the
      bandwidth one — so a normal viewer's drops read exactly as before.
      The subtraction is floored at zero: its terms are atomics read at
      slightly different instants and a Prometheus counter must not wrap.
    - **`carrierRecordsDropped` is deliberately untouched.** These deltas
      are dropped at the queue, before they ever become records; folding
      them in would make "records dropped" mean two different things and
      would move the goalposts of finding 10's `CarrierRecordsDropped == 0`
      assertion. The two counters answer "did the carrier fail?" and "did
      the drain keep up?" separately; `/statusz` carries both, per broadcast
      and per subscriber (`subscriberDetails[].carrierQueueOverflow`).

    Tests (test-first): the reproduction is
    `internal/metrics` `TestReliableQueueOverflowHasItsOwnDropReason` — a
    wedged reliable subscriber on one broadcast and a wedged datagram
    subscriber on another, asserting each lands in its own bucket and
    neither leaks into the other; before the fix it reported all 16 carrier
    overflows under `queue_full` and no `carrier_queue_full` series at all.
    `internal/hub` `TestReliableQueueOverflowCountedApartFromDatagramDrops`
    covers the `/statusz` surface: the per-subscriber split, the total
    staying whole, `CarrierRecordsDropped` staying 0, and the fold on close.

12. **Post-review fix (2026-07-22): a stalled record froze the drain for two
    GOPs — `BACKPRESSURE-2` (medium) from
    `docs/reviews/resilient-mode-review.md`.** Decision 5's "reuse
    `KeyframeWriteTimeout`" was the cheap call at design time and the wrong
    one at runtime, because the two writes are not comparable work:
    - a **keyframe** is ~236 KB on a stream of its own, written by a
      per-keyframe goroutine (`writeKeyframe`) that blocks nothing else, so
      1 s of patience costs that subscriber at most that keyframe;
    - a **carrier record** is ~20 B–1.2 kB written by `drainReliable`, the
      single goroutine that owns the subscriber's *entire* delta path — the
      audio sideband included. Its deadline is not "how long we wait for this
      record", it is **how long every delta behind it is frozen**.

    So the mode built to hide network stalls could manufacture a 1 s one of
    its own — twice the 500 ms GOP it is scoped to — every time a peer's
    stream flow-control window closed. The queue survives it (at 30–60 fps a
    1 s block accrues ~30–60 deltas against a 256-deep queue, so this is a
    latency defect, not the finding-11 overflow), and the GOP tail is
    recovered late rather than lost, but the freeze is exactly the symptom
    the feature exists to remove.
    Fix, relay only, no wire/viewer/broadcaster change and **still no new
    knob**:
    - **`CarrierWriteTimeout` = 500 ms**, a constant beside the eviction
      thresholds (it encodes the relay's drops-over-stalls policy, not fleet
      capacity). One GOP is the natural bound: it is also when the next
      keyframe rotates the carrier and hands the viewer a clean resync point,
      so a record still unsent by then has missed its ride regardless.
      Nor is anything gained by waiting: the write blocks because the peer's
      *stream* flow-control window is shut, and that stream is in-order, so
      while it is blocked the viewer is receiving nothing on it either —
      patience extends the freeze instead of shortening it, and delays the
      next GOP's carrier behind it. (Nor is this the "merely slow" case: a
      record is ≤ ~1.2 kB, so 500 ms of blocking means < 2.4 kB/s on that
      stream — a link that cannot carry video at all. The deeper playout
      buffer finding 8 unlocked does not change this; a buffer can only play
      frames it has, and these are stuck behind a wedged stream.)
    - **One budget per dequeued record.** The deadline is computed once in
      `drainReliable` and shared by the lazy `openCarrier` prologue write and
      the record write, so a record can't stall the drain for 2× the bound by
      parking in both.
    - **An operator's tighter setting still wins**:
      `Options.carrierWriteTimeout()` returns
      `min(CarrierWriteTimeout, KeyframeWriteTimeout)` — a fleet that
      abandons a whole keyframe after 200 ms has no business waiting 500 ms
      on one delta record. Patience may shrink with the fleet's setting,
      never grow past a GOP.
    - `retireCarrier`'s `Close` deadline moves with it: retiring also runs on
      the drain goroutine.

    **Not done: the review's alternative half** ("check `carRotations`
    immediately before the blocking `Write`"). It cannot bound an
    *already-blocked* write, which is the defect; and dropping the in-hand
    record on a rotation would discard deltas the current code delivers on
    the next carrier in the healthy case (the queue legitimately holds a few
    records at rotation time under normal jitter). Cancelling from the
    rotation side would bound it, but at 500 ms the deadline and the rotation
    coincide by construction, without cross-goroutine cancellation of a
    stream that may merely be slow.

    Tests (test-first): `TestStalledCarrierAbandonedOnItsOwnDeadline` parks a
    carrier write on a registry configured with a *patient* 5 s
    `KeyframeWriteTimeout` and requires the abandon within 2 s — it timed out
    at 2 s before the fix and passes in ~0.5 s after — then shows the drain
    free again on the next rotation.
    `TestCarrierWriteTimeoutTakesTheTighterBound` pins the `min` rule and the
    "shorter than the default keyframe timeout" invariant so neither can rot
    back. **Test-the-test** (each mutant reverted): deadline reverted to
    `KeyframeWriteTimeout` → red on the 2 s bound; the `min` rule removed →
    red on the 200 ms operator case; `CarrierWriteTimeout` raised to 2 s →
    red on the invariant guard. Existing coverage unchanged and green,
    including the finding-10 loss-injection integration test.

    Residual, unchanged by this fix: the reliable drain is still one
    goroutine, so a stalled record delays the audio datagrams queued behind
    it — now for ≤ 500 ms rather than ≤ 1 s (docs/20 field finding 5's known
    residual).

13. **Post-review fix (2026-07-22): the egress cap was charged for records
    the drain had already decided not to write — `BW-CHARGE` (medium,
    reported independently as `BACKPRESSURE-1`/`DRAIN-1`) from
    `docs/reviews/resilient-mode-review.md`, together with the low-severity
    `BACKPRESSURE-4` the review pairs with it.** `drainReliable` charged
    `consumeBandwidth` at the *top* of the loop, before the rotation check,
    the `carDead`/`closed` check and the carrier open — so every record the
    loop then dropped had already debited the cap. That cap is one
    process-wide token bucket shared by **every broadcast on the pod**, so
    the debit is not paid by the viewer whose record died: it is paid by
    unrelated broadcasts, whose datagrams get bandwidth-dropped to fund
    bytes that never existed. The failure shape is a whole GOP tail — once
    `openCarrier` fails, `carDead` drops every remaining record of that GOP
    at the top of the loop, and each one used to pay full freight on the way
    out. It also contradicted the code's own comment ("charged per record as
    written"). Mirror-image on the other side: `openCarrier`'s 2-byte
    prologue was counted into `egressCarrierBytes` but never charged
    (`BACKPRESSURE-4`) — a small, permanent hole in the cap and in the
    invariant that makes the accounting reviewable at all. Both are inert
    on a default fleet (`-max-bandwidth` unset ⇒ `limiter == nil`), which is
    why this ranks below findings 8–12. Fix, relay only, no
    wire/viewer/broadcaster change and no new knob:
    - **One charge per record, taken below the drop decisions**, for exactly
      the bytes that record is about to put on the wire: `len(record)`, plus
      `wire.CarrierPrologueSize` when it is the record whose lazy open
      starts this GOP's carrier. The rotation/`carDead`/`closed` checks and
      the record marshalling now all run *before* the charge.
    - **The prologue is charged by the record that opens the carrier**,
      rather than inside `openCarrier`. One charge site keeps the invariant
      statable in a sentence, and an over-cap drop at that point is decided
      *before* a stream exists — where `openCarrier` would already have
      written the prologue and set `carCurrent` before it could refuse.
    - Charging and dropping use the same `n`, so `bandwidthDroppedBytes`
      keeps meaning "bytes we intended to send and could not" (a
      cap-refused first record of a GOP now counts its prologue too).
    - Moving the rotation check above the charge is a small bonus: an
      over-cap record now still retires the previous GOP's carrier instead
      of leaving it open until some later record passes the cap.

    **Deliberately not "charge only on successful write"** (the review's
    first suggestion). A cap that debits after the bytes are gone cannot
    refuse anything — the datagram path's check-then-send ordering is what
    makes `-max-bandwidth` a limit rather than a meter, and reliable
    delivery must not get a weaker rule than datagrams. The residual
    imprecision is therefore bounded and intended: a record whose
    `openCarrier` or `writeCarrier` *fails* stays charged, exactly as a
    datagram whose `SendDatagram` fails does. That is one record per dead
    GOP (at most one open attempt per rotation), not the GOP tail.

    Tests (test-first, `internal/hub`):
    `TestDeadCarrierRecordsDoNotDebitEgressCap` fails every carrier open and
    pushes a five-record GOP tail through a frozen budget — 115 bytes
    debited before the fix, 25 after (the one record that reached an open
    attempt, prologue included) — and asserts the drops do not masquerade as
    bandwidth drops. `TestCarrierPrologueChargedOncePerGOP` measures the
    debit per record across a rotation (prologue + record, record, prologue
    + record) and closes the loop against `EgressCarrierBytes`, so the cap
    and the operator-visible egress counter can't drift apart.
    `TestCarrierBandwidthCapDropsRecords`'s expectation moves by the
    prologue's two bytes — the one deliberate behaviour change to an
    existing assertion. **Test-the-test** (mutant reverted afterwards):
    putting the charge back at the top of the loop while *keeping* the
    prologue in it reddens both new tests — 125 bytes for the dead GOP, and
    no prologue charged after a rotation, because above the rotation check
    `currentCarrier()` still returns the previous GOP's carrier. The two
    halves of this fix are not independent: the prologue can only be charged
    correctly from below the rotation check.

14. **Post-review fix (2026-07-22): a reliable subscriber's queue overflow
    dropped the *newest* record — `BACKPRESSURE-3` (low) from
    `docs/reviews/resilient-mode-review.md`.** `enqueueLocked`'s bounded queue
    is shared by both drains, and its overflow policy — a non-blocking send
    that sheds the *newcomer* — is correct for the datagram drain: a datagram
    viewer is live-edge and loss-tolerant, and the queue already holds
    fresher-or-equal frames, so dropping the arrival is right. Reused verbatim
    for a reliable subscriber it is backwards. The carrier delivers records in
    order and the viewer treats them as one contiguous stream, so a queue
    overflow forces a keyframe resync (`jumpToKeyframe`) either way — but
    drop-newest reliably delivers the *stale* backlog in order and throws away
    the near-live frames, stranding the viewer as far behind as the queue is
    deep (256 records ≈ 4–8 s at 30–60 fps) at exactly the moment resilient
    mode is supposed to keep it near live. The cost of the hole is one resync
    regardless of which end is dropped; the choice only decides how far behind
    live the viewer lands after it.

    Fix, relay only, test-first, no wire/viewer/broadcaster change and no new
    knob: for `s.reliable` subscribers the overflow path evicts the queue's
    **oldest** record (a non-blocking receive) before enqueuing the newcomer,
    so the queue trends toward live. The datagram path keeps drop-newest,
    byte-identical. Safety of the two-step evict-then-enqueue rests on an
    invariant already true in the code: `enqueueLocked` is the *sole* sender to
    the queue and always runs under `registry.mu` (via `fanOutLocked`), while
    the drain goroutine only ever *removes* — so once a head is evicted, room
    for the newcomer is guaranteed and no concurrent sender can re-fill it. The
    second send stays a non-blocking `select` regardless (fan-out must never
    block under the lock); its `default` is unreachable given the invariant but
    falls back to dropping the newcomer if that invariant ever breaks. Exactly
    one datagram is shed per overflow either way, so the finding-11
    accounting is unchanged: one `dropped` + one `carrierQueueOverflow` per
    reliable overflow, `queue_full` still derivable by subtraction.

    Test (test-first): `TestReliableQueueOverflowDropsOldest` parks the carrier
    write so the drain holds one delta and the queue fills, fans out
    `2×queueDepth` more deltas, then releases the carrier and asserts the
    delivered records are the in-flight delta plus the **newest** `queueDepth`
    survivors in order — the newest delta present, the oldest overflowed one
    absent. It fails on the old drop-newest code (it delivered the oldest
    `queueDepth`, newest absent) and passes after the evict-oldest change; the
    existing `TestReliableQueueOverflowCountedApartFromDatagramDrops` (drop
    count / bucketing) and `TestReliableSubscriberCarrierDelivery` (in-order
    verbatim delivery) stay green, as does `go test -race ./internal/hub/...`.

    Not done: the review's alternative "carrier-backlog cap that sheds stale
    GOPs". Drop-oldest already preserves the near-live frames the finding is
    about with a one-line policy change and no new state; a GOP-granular
    backlog cap is a larger mechanism for a low-severity edge that only bites a
    subscriber already deep in sustained overflow (a link that cannot keep up
    at all), and is a reasonable future refinement, not a blocker.

15. **Post-review fix (2026-07-22): the carrier's most common record size was
    never round-tripped — `WIRE-1` (low) from
    `docs/reviews/resilient-mode-review.md`.** The carrier framing tests in
    all three mirrors exercised the 23-byte golden VideoChunk and asserted
    *rejection* of `MaxDatagramSize + 1`, but never carried a record whose
    datagram is exactly `MaxDatagramSize`. That is not an exotic edge: a full
    delta chunk is exactly 1200 bytes, so it is the record the carrier frames
    most of the time, and the uint16 length prefix's inclusive upper boundary
    is precisely where an off-by-one lives. `DecoderConfig` and `AudioFrame`
    both pin their exact-1200 boundary; the carrier was the asymmetry.
    Test-only, no production code touched — `AppendCarrierRecord` /
    `CarrierRecordParser` handle 1200 correctly today; this is a guard, added
    to all three mirrors by the same rule that mirrors the golden vectors:
    - `TestCarrierRecordMaxSizeDatagram` (`gawk-server/wire/wire_test.go`) and
      `TestCarrierRecordAtMaxDatagramSize`
      (`gawk-broadcast/internal/wirecheck/wirecheck_test.go`) build the
      boundary datagram as a **real full delta chunk** (`AppendVideoChunk`
      with `MaxChunkPayload`), so the vector also re-pins
      `MaxChunkPayload + VideoChunkHeaderSize == MaxDatagramSize`.
    - The relay-side test frames a second record behind the max-size one: the
      boundary must not swallow or truncate what follows on the same stream.
    - The viewer test (`wire.test.ts`) drives the production path — the
      incremental `CarrierRecordParser` — with the read split landing on
      either side of the record boundary and on it exactly.
    - The 2-byte length prefix at the maximum (`04b0`) is pinned as a hex
      constant in all three, the only part of a 1202-byte record worth
      stating as a vector.

    **Test-the-test** (each mutant reverted afterwards): flipping
    `> MaxDatagramSize` to `>=` in `AppendCarrierRecord` reddens the relay and
    wirecheck tests; the same flip in `ParseCarrierRecord`, in
    `encodeCarrierRecord` and in `CarrierRecordParser.push` reddens the
    corresponding mirror. The point of the finding is the *rest* of the
    result: with the append-side mutant in place the **entire pre-existing
    wire suite stays green**, in every mirror — which is exactly the gap
    reported.

16. **Post-review fix (2026-07-22): the frame-interpolation menu entry was
    keyed on the *stored* playout mode, not the effective one — `LIFECYCLE-2`
    (low) from `docs/reviews/resilient-mode-review.md`.** Decision 7 gave
    resilient mode a deliberate split: the stored playout mode keeps its value
    untouched (so it regains effect the moment the mode turns off) while the
    *effective* mode is forced to adaptive with the resilient profile —
    `playout.ts` exposes exactly that pair (`getStoredPlayoutMode` /
    `getPlayoutMode`). The pipeline honours the split correctly: `viewer.ts`
    reports `stats.interpolation` non-null when `getPlayoutMode() ===
    'adaptive'`, so a resilient viewer *is* interpolating regardless of what
    it stored. `ViewerScreen`'s menu, though, gated the "Frame interpolation
    (experimental)" entry on the local `playoutMode` state — the stored value.
    A resilient viewer whose stored mode is `'off'` or `'fixed'` therefore had
    an experimental feature running with **no control to turn it off**, which
    is precisely the population that most wants the option: R19 exists for
    phones, and interpolation is the most GPU-expensive thing in the viewer.

    Fix, viewer UI only, test-first, one condition: the entry is gated on
    `resilientMode || playoutMode === 'adaptive'` — the effective mode,
    computed from the same two inputs `playout.ts` uses. Kept deliberately as
    a local computation rather than switching the gate to `stats.playoutMode`:
    the local state changes the instant the user toggles, while a stats sample
    is up to a tick stale, so a viewer who turns pacing off would keep seeing
    an interpolation entry for ~1 s. The `stats?.interpolation != null`
    half of the condition stays — it is what still excludes the main-thread
    path and non-WebGL2 sinks. No annotation was added to the entry (unlike
    the two pacing entries, which resilient mode *overrides*): interpolation
    remains genuinely user-controlled in resilient mode, so the toggle means
    what it says.

    Tests (test-first, `ViewerScreen.test.tsx`): the reproducer renders with
    `gawk:playout-mode='off'` + `gawk:resilient-mode='1'`, asserts the split
    at the source (`getStoredPlayoutMode() === 'off'`,
    `getPlayoutMode() === 'adaptive'`), and requires the entry to be present,
    checked, and to actually flip `gawk:interpolation` without disturbing the
    stored playout mode. A negative control (stored `'fixed'`, resilient off)
    pins that a stale `interpolation: 'on'` sample cannot resurrect the entry.
    **Test-the-test** (mutants reverted): dropping the mode half of the
    condition entirely — gating on `stats?.interpolation != null` alone —
    reddens the negative control, and the original stored-mode gate reddens
    the reproducer; the whole pre-existing viewer suite stays green under
    both.

Ordering: X1 → X2 → X3 form the minimal reliable path (verifiable with the
harness before any UI exists, via a URL-level override); X4 makes it a
product feature; X5 rides alongside; X6 last. Nothing here blocks or is
blocked by R17 scale-out (merged 2026-07-18) — see interop note below; X2
lands beside the drain path that merge touched and allocates its type byte
after R17's 0x09.

**Scale-out interop (R17)**: reliable conversion happens at the
pod serving the subscriber. Origin→edge internal subscriptions stay
datagram-based (in-cluster links are effectively lossless; the lossy leg is
the last mile) and the `delivery` param is never propagated upstream. An
edge can only reliably deliver what it received — origin→edge ingress loss
is out of scope by the same argument as broadcaster→relay loss.

## Verification plan (manual, after X6)

1. **Harness drills** (X1 baseline vs X6 resilient, same netem matrix):
   default mode shows the documented stutter signature; resilient mode meets
   the headline criterion; overlay Delivery section tells the truth in both
   modes (mode row, offset climbing under induced loss, carrier counters).
2. **Real phone, real cell network**: iPhone (Safari 26.4+, the R16
   audience) and an Android Chrome viewer on LTE/5G against the homelab
   relay — subjective smoothness + Copy-diagnostics capture; latency
   observed via the existing capture→render row (expect 0.5–1.5 s under
   real conditions).
3. **Mode round-trip**: toggle on → visible reconnect → smooth; toggle off →
   back to live-edge (~150 ms); persisted across reload; R12 toggles regain
   effect when resilient turns off.
4. **Mixed audience**: one broadcast, one resilient + two default viewers —
   default viewers byte-identical (overlays + relay metrics confirm), relay
   `/statusz` shows the split.
5. **Degradation**: resilient viewer against a pre-X2 relay build → Decision
   8 fallback state; throttle below stream bitrate → GOP-granular dropping,
   no stall, recovery when throughput returns; broadcaster restart
   mid-session → existing restart/resync behavior unchanged in resilient
   mode.
6. Record findings + final constants in this doc.

## Non-goals

- **FEC** — rejected in the survey; revisit-if trigger recorded there.
- **NACK/ARQ over datagrams** — rejected in the survey (QUIC streams do it).
- **Auto-switching transport modes** — deferred with a design sketch
  (Decision 11); v1 is manual + persisted.
- **Broadcaster changes of any kind** — browser and native broadcasters are
  untouched; no broadcaster-side toggle (this is a per-viewer choice about
  their own network).
- **Per-viewer quality adaptation / simulcast** — unchanged non-goal (R4).
- **Audio** (R15): *(landed 2026-07-19, and the guess below was right —
  docs/20 Decision 12 is that amendment.* Audio rides the reliable carrier
  automatically, since the relay converts the subscriber's whole datagram
  stream and audio is just another datagram type; the audio jitter buffer
  became profile-carrying and adopts this mode's [150, 2000] ms / seed 500
  envelope. Post-2026-07-20 that envelope sizes audio's own fallback depth
  floor rather than anything video-side — A/V sync is video-master.)*
- **DVR/rewind** — the buffer is a delivery buffer, not a seek buffer.

## Rejected

- **Buffer-only, relay-cache+NACK, FEC as the primary mechanism** — survey
  table above.
- **Whole-frame `StreamFrame` emission for deltas** (relay-side reassembly)
  — Decision 2.
- **Per-frame carrier streams / one long-lived carrier** — Decision 4.
- **A latency slider** — same reasoning as R12 (adaptivity is what makes a
  slider unnecessary); the clamp profile is the contract.
- **Repurposing the existing paced/smooth toggles** — resilient mode is its
  own toggle; existing toggles keep their semantics (project rule).
- **Server-pushed mode switching** (relay tells a struggling viewer to go
  reliable) — the viewer owns its latency/smoothness trade; the relay can't
  know what the human prefers.
