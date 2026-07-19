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
   - The existing consecutive-open-failure eviction (threshold 10, close
     code 4001) extends to carrier opens — a zombie resilient subscriber is
     evicted the same way a zombie keyframe subscriber is today.
   - **Egress bandwidth cap applies**: carrier bytes pass
     `consumeBandwidth` exactly like keyframe stream bytes do today
     (`sendKeyframe` precedent) — reliable delivery must not become a cap
     bypass.
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
     dwell-gated) when the link cleans up. The existing
     `WindowedQuantileTracker` needs no changes.
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
   Right-click menu: **"Resilient mode (mobile networks)"**, persisted under
   `gawk:resilient-mode`, default off. Toggling tears the session down
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
   record. Each record write carries a `KeyframeWriteTimeout` deadline — a
   stalled write cancels the carrier (a half-written record is unrecoverable
   framing) and the GOP's remaining records are dropped until the next
   rotation. `Subscriber.Close` cancels the current carrier under its own
   mutex so a blocked drain unblocks immediately.
3. **Eviction-streak cadence.** Carrier opens share the keyframe streak, and
   at most one carrier open is attempted per rotation (an open failure marks
   the GOP dead), so the streak grows at GOP cadence — a zombie with both
   stream kinds failing evicts at ~2.5 s (10 combined failures at 500 ms
   GOP), same order as the keyframe-only path.
4. **Bandwidth-cap accounting.** Over-cap carrier records count as
   *datagram* bandwidth drops (same counters as the datagram drain), keeping
   R9's queue_full-by-subtraction intact; `carrier_records_dropped_total` is
   reserved for dead-carrier/open-failure drops.
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
- **Audio** (R15, not started): when audio lands it stays on datagrams with
  silence concealment per its own design; a resilient-mode audio story
  (likely: widen the audio jitter-buffer clamp under the same profile) is a
  one-paragraph amendment to docs/20 at that time, not scoped here.
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
