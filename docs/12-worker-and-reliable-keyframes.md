# R8 — Worker Offloading & Reliable Keyframes: Design + Implementation Plan

Design doc for [ROADMAP R8](../ROADMAP.md#r8--worker-offloading--reliable-keyframes).

Two changes that share a delivery window but are independent:

1. **Reliable keyframes.** Keyframes move from the unreliable datagram path
   (where a single lost UDP packet discards the whole frame and freezes the
   picture until the next GOP) onto a **reliable WebTransport unidirectional
   stream**, one stream per keyframe. Delta frames stay on datagrams. This is
   the server-heavy change: the relay must accept publisher-initiated streams,
   store-and-forward each keyframe to every subscriber over its own stream, and
   do so **without a slow subscriber ever coupling back onto the publisher or
   the other subscribers** — the datagram fan-out's "drops over stalls"
   discipline, re-expressed at stream granularity.
2. **Worker offload.** The whole viewer pipeline (WebTransport reads, delta
   reassembly, keyframe stream reads, reorder, decode, and canvas render via
   `OffscreenCanvas`) moves into a dedicated `viewer.worker.ts`, so React
   renders and UI interaction can never starve network I/O or decode (the
   suspected cause of the Firefox `Awaiting keyframe` drops), and decoded
   `VideoFrame`s never cross a thread boundary to be painted.

The preliminary sketch that preceded this doc left the server backpressure
model, the stream wire framing, the cache/late-joiner redesign, the
resource/hardening bounds, and the two-transport **reordering** interaction
unspecified, and proposed a fixed **200 ms playout offset** that runs directly
against the project's locked-in *"favor dropped frames over stalled playback,
play the latest frame"* philosophy. This doc resolves all of them.

## Goals

- **Keyframe integrity**: a keyframe is delivered whole or not at all; a lost
  datagram can never again strand a viewer for the rest of a GOP. Recovery
  after any delta loss is bounded to **≤ 1 GOP (currently 500 ms)** and
  *deterministic*, because the next keyframe is now guaranteed to arrive.
- **Backpressure isolation (the crux)**: a subscriber that stalls on its
  reliable keyframe stream must degrade to "misses this keyframe, recovers at
  the next one" — exactly the datagram drop semantics — and must **never**
  block the publisher's ingestion or another subscriber's fan-out. QUIC
  per-stream flow control makes a naive `Write` to a stalled peer *block*; the
  design must structurally prevent that from touching anyone else.
- **Off-main-thread viewer**: network + decode + render run in a worker;
  decoded frames are drawn to a transferred `OffscreenCanvas` inside the
  worker and never posted to the main thread.
- **Preserve the transport philosophy where it belongs**: deltas stay lossy
  and fast; reliability is spent *only* on the one frame type whose loss costs
  a whole GOP. No global playout delay is added (see Decision 7).
- **Bounded and hardened**: keyframe streams are size-capped and count-capped
  (memory-inflation defense, mirroring `MaxChunkCount`), counted against the
  bandwidth cap, and every new knob is plumbed **all the way to production**
  (config → `registryOptions` → Helm), per the R2 post-implementation finding.
- **Cross-browser**: works Chrome↔Chrome and Firefox↔Chrome; the docs/11
  Firefox AVCC normalization and dynamic-MTU handling carry over unchanged
  (and the keyframe MTU problem disappears entirely on the stream path).

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| S1 | **Wire framing for stream keyframes** — new `TypeStreamFrame` message in `gawk-server/wire` (`wire.go`) + its TS mirror (`transport/wire.ts`); golden hex vectors in `wire_test.go` copied verbatim into `wire.test.ts` | Test-first: Go and TS round-trip a stream-frame header (frameId, timestampUs, configLen, payloadLen, flags) byte-identically against the shared golden vector; oversized `payloadLen`/`configLen` and truncated headers reject with typed errors; existing datagram vectors unchanged | ✅ done |
| S2 | **Server ingestion** — `handlePublish` accepts publisher uni streams concurrently with the datagram loop; hub reads each keyframe stream to EOF into a bounded buffer, validates against `MaxKeyframeBytes`, updates the cache | Publisher datagram loop and stream-accept loop share the session context and both exit on session end with no goroutine leak (race detector clean); a stream exceeding `MaxKeyframeBytes` is `CancelRead`-ed and counted, never allocated in full; a well-formed keyframe stream replaces the cached keyframe+config atomically | ✅ done |
| S3 | **Server fan-out (the crux)** — per-subscriber store-and-forward over `OpenUniStream`; `hub.Conn` gains stream-open; write deadline + supersede-stale + ≤1 in-flight per subscriber; late-joiner priming over a stream; bandwidth accounting; new knobs through `config` → `registryOptions` → Helm | A subscriber whose stream write blocks past `KeyframeWriteTimeout` is `CancelWrite`-ed and counted as a keyframe drop while the publisher ingest and all other subscribers proceed unaffected (test injects a blocking `Conn`); a new keyframe supersedes an in-flight stale one to the same subscriber; a joiner is primed with the cached keyframe over a stream; over-bandwidth fan-out is skipped (not queued); every knob is asserted reachable from `registryOptions` (mirrors `main_test.go`) | ✅ done |
| S4 | **Broadcaster** — keyframes branch onto `wt.createUnidirectionalStream()` carrying config+payload; deltas stay on `packetizeFrame`/datagrams; `packetizer.ts` gains a stream-frame encoder | `handleEncoded` sends `chunk.type==='key'` over a uni stream (config prepended into the stream, not the datagram path) and deltas over datagrams; a write to a torn-down session fails the pipeline as today; unit tests cover the key/delta branch and the stream encoder | ✅ done |
| S5 | **Viewer reorder/playout buffer** — new `transport/reorder-buffer.ts` merging datagram deltas + stream keyframes by `frameId`; dependency-gated release; freeze-on-gap centralized here; bounded reorder hold (Decision 7), **no fixed playout offset** | Test-first (node, no WebCodecs): a keyframe arriving *after* its following deltas releases keyframe-then-contiguous-deltas in order; a missing predecessor **delta** freezes immediately and resyncs at the next keyframe; an in-progress keyframe holds decodable-pending deltas up to `KEYFRAME_WAIT_MS` then drops them; decoder-queue-deep triggers drop-to-keyframe; watermark never emits a delta older than the decode position | ✅ done |
| S6 | **Worker offload** — `viewer.worker.ts` hosting a host-agnostic `ViewerWorkerCore` (S1–S5 pipeline + in-worker reconnect); main-thread shell transfers `OffscreenCanvas` once and relays start/stop + status; `ViewerScreen` rewired; AVCC-normalize moves into the worker | Core is unit-testable synchronously with a fake `WebTransport` (extended for `incomingUnidirectionalStreams`) and a fake render sink — no real worker/DOM needed; `OffscreenCanvas` transferred exactly once for the life of the view; reconnect (backoff, code-4000 terminal, fatal-codec) behaves identically to today's `ViewerSession`; frames never `postMessage`-d to main thread | ✅ done (browser-verified 2026-07-14) |
| S7 | **Observability + verification + docs** — `/statusz` + stats-overlay fields; DevTools packet-loss and profiler verification; README gotchas + ROADMAP + CLAUDE.md sync | `/statusz` reports keyframe-stream in/out/dropped/oversize counters per broadcast and in totals; stats overlay shows keyframe-stream health and reorder holds/drops; manual packet-loss injection shows freezes eliminated and ≤1-GOP recovery; React-profiler churn no longer spikes viewer drops; all automated gates green | ✅ done (browser-verified 2026-07-14) |

**Implementation status (2026-07-14): R8 complete and browser-verified.** The
reliable-keyframe path (S1–S5) **and the worker offload (S6)** are implemented
end-to-end, covered by automated tests, **and confirmed working in the browser
(2026-07-14)**: keyframes travel over per-subscriber reliable uni streams with
store-and-forward fan-out, a write deadline, supersede-stale, and late-joiner
priming server-side; the viewer merges stream keyframes with datagram deltas in
a pure, bounded reorder buffer (`transport/reorder-buffer.ts`) with freeze-on-gap
centralized there; and the whole viewer pipeline runs in a Web Worker rendering
to a transferred `OffscreenCanvas` (with a main-thread fallback). S7's
observability and docs are done — `/statusz` and both stats surfaces expose the
new keyframe-stream and reorder counters, and all automated gates are green
(Go `gofmt`/`vet`/`-race`, frontend `test`/`build`/`lint`, `helm lint`). The
end-to-end browser verification (worker path decoding/rendering the
reliable-keyframe stream) passed on the target build; there is no outstanding
gate.

**S6 (worker offload) is implemented and browser-verified (2026-07-14).** Per
Decision 8 the pipeline core was already host-agnostic, so S6
*moved* code rather than rewriting it:

- `transport/render-sink.ts` — the `RenderSink` seam. `ViewerPipeline` now draws
  each decoded `VideoFrame` to an injected sink and closes it in place (single
  owner) instead of forwarding it; `OffscreenCanvasRenderSink` is the worker
  implementation. When no sink is injected the pipeline keeps the exact
  main-thread `onDecodedFrame` behavior.
- `transport/viewer-worker-core.ts` — `ViewerWorkerCore`, a DOM-free core that
  owns the S1–S5 pipeline **and** the reconnect state machine by *reusing
  `ViewerSession` unchanged* (so backoff / code-4000-terminal / fatal-codec are
  identical by construction). It talks to the outside only through an injected
  `WorkerHost` (a `postMessage`-like event sink + the render sink), and a
  generation guard drops a superseded session's late callbacks. Decoded frames
  never become messages — only small `connected`/`reconnecting`/`codec`/`stats`/
  `error`/`ended` events cross the boundary.
- `transport/viewer.worker.ts` — the thin worker shell: a boot handshake reports
  whether the worker scope actually has `VideoDecoder`/`WebTransport` *before*
  any canvas transfer, then it bridges `postMessage` to the core.
- `features/viewer/workerViewerController.ts` + `useViewerConnection.ts` —
  imperative worker glue (one-shot `transferControlToOffscreen`, StrictMode-safe
  deferred dispose) and the React hook that picks the worker path when supported
  and **falls back to the main-thread `ViewerSession`** otherwise (jsdom/tests,
  or a worker whose boot handshake reports missing codecs — the Firefox risk).
  `ViewerScreen` consumes the hook and never touches a `VideoFrame`.

Tests (`viewer-worker-core.test.ts`, `render-sink.test.ts`) instantiate the core
with a fake host + fake render sink and the existing mocked `connection`/
`decoder` — no real Worker/DOM — and assert the event mapping, the generation
guard, and that decoded frames reach the sink and never the host. The
browser-only gate — the worker path actually decoding and rendering on the
target build — was **verified 2026-07-14** (the `runtime`, not the logic, was
what couldn't be exercised in CI).

Goal → verified-by, for the cross-cutting behaviors:

| Goal | Where | Verified by |
|------|-------|-------------|
| Keyframe delivered whole or not at all | reliable uni stream, S2/S4 | manual packet-loss injection |
| Slow subscriber never blocks publisher or peers | server store-and-forward + per-sub deadline/cancel, S3 | `hub` test with a blocking `Conn`; race detector |
| A stale in-flight keyframe is superseded by the newest | per-subscriber ≤1 in-flight, S3 | `hub` test |
| Late joiner primed with cached keyframe over a stream | `Subscribe` prime path, S3 | `hub` test + manual join-mid-stream |
| Keyframe arriving after its deltas still decodes in order | reorder buffer, S5 | `reorder-buffer.test.ts` |
| A lost **delta** freezes immediately, resyncs ≤1 GOP | reorder buffer gap policy, S5 | `reorder-buffer.test.ts` + manual |
| No fixed playout latency added (live-edge preserved) | Decision 7 (bounded reorder, no offset) | `reorder-buffer.test.ts` + glass-to-glass measure |
| Keyframe streams bounded in size/count/bandwidth | `MaxKeyframeBytes`, in-flight cap, cap counted | `hub` test; `wire` reject tests |
| Every knob reaches production | `registryOptions` mapping, S3 | `main_test.go`-style assertion |
| UI churn can't cause viewer drops | worker isolation, S6 | React-profiler soak |
| Decoded frames never cross threads | in-worker `OffscreenCanvas` render, S6 | code review + transfer-list assertion |

## Background: the two bottlenecks

Cross-browser testing (docs/11) and manual verification surfaced two distinct
failure modes:

1. **`Dropped (Incomplete)` on Chrome** — a keyframe is chunked into ~100–200
   datagrams (`packetizeFrame`); losing any one of them means the
   `Reassembler` evicts the in-progress assembly (`framesDroppedIncomplete`)
   and the viewer freezes on the last good frame until the *next* keyframe.
   At a 500 ms GOP that is a visible half-second stutter per lost keyframe
   packet, and keyframes are the largest, burstiest frames — the most likely
   to lose a packet under congestion.
2. **`Awaiting keyframe` on Firefox** — the decoder falls behind and the
   viewer's `decodeFrame` latches `waitingForKeyframe` (either the decode
   queue crossed `getMaxDecoderQueueSize()` or a delta gap was seen),
   discarding deltas until the next keyframe. Suspected root cause is
   main-thread contention: React renders and the 2D `drawImage` on the main
   thread compete with the datagram read loop and decode callbacks.

Reliable keyframes attack (1) at the source; the worker attacks (2).

## Architecture overview

Today every frame — key and delta — travels as `TypeVideoChunk` datagrams; the
relay caches the last fully-reassembled keyframe and its config to prime late
joiners. After R8:

```
Broadcaster                     Relay (Go)                         Viewer (worker)
-----------                     ----------                         ---------------
encode ─┬─ key  ─► uni stream ─► AcceptUniStream ─► read-to-EOF ──┐
        │        (config+kf)      (bounded buf = cache)           │ store-and-forward
        │                              │                          ▼
        │                         OpenUniStream ─► per-sub write ───► incomingUni ─► keyframe
        │                         (deadline, supersede, ≤1)           streams        ▲
        │                                                                            │ reorder
        └─ delta ─► datagrams ──► fan-out queue (drop-if-full) ────► datagrams ─► Reassembler
                                                                                     │
                                                                          decode ─► OffscreenCanvas
```

Key properties of this split:

- **Reliability is spent only on keyframes.** A lost delta costs one frame and
  a freeze until the next keyframe; a lost keyframe used to cost a whole GOP.
  We make reliable exactly the frame whose loss is expensive, and keep the
  cheap frames cheap and lossy — the datagram rationale is intact for 14 of
  every 15 frames.
- **The publisher is fully decoupled from every subscriber.** The server
  receives a keyframe *completely* before it fans anything out (store-and-
  forward), so a subscriber's flow-controlled stream can never exert
  backpressure on the ingest path. This is the whole answer to the roadmap's
  key design question, and it is developed in Decision 3.
- **The cache gets simpler, not harder.** The "last keyframe" is now just the
  bytes of the last keyframe stream — no datagram reassembly to cache from.

## Decisions

1. **Only keyframes go reliable; deltas stay on datagrams — permanently.**
   The alternative (all frames on streams) would reintroduce head-of-line
   blocking for the common case and throw away the project's founding
   transport choice. The keyframe/delta asymmetry is real: a keyframe is
   self-contained and is the *only* resync point, so its loss is catastrophic
   and its integrity is worth a retransmit; a delta references its predecessor
   and is superseded within ~33 ms, so its loss is cheap and waiting for it is
   never worth it. The datagram `TypeVideoChunk` format is **unchanged**;
   keyframes simply stop being sent on it (the datagram keyframe flag becomes
   vestigial — always 0 — and the viewer no longer looks for keyframes there).

2. **One uni stream per keyframe, carrying config + payload, delimited by
   FIN.** A new wire message `TypeStreamFrame` (see [Wire format](#wire-format))
   with a fixed 24-byte header (version, type, flags, `frameId`, `timestampUs`,
   `configLen`, `payloadLen`) followed by an optional embedded `DecoderConfig`
   datagram and then the raw encoded keyframe bytes. The publisher opens the
   stream, writes header+config+payload, and `Close()`s it (FIN). One keyframe
   per stream means the stream *is* the frame boundary — no in-band length
   framing of multiple frames, no partial-frame ambiguity. Explicit
   `configLen`/`payloadLen` are validated before allocation (defensive parsing,
   R2). Config rides **on the stream** (not the old "config datagram before the
   keyframe" trick), so a delivered keyframe is always self-sufficient to
   decode — including the resolution-ladder-change case, where the following
   keyframe simply carries the new config.

3. **Server fan-out is store-and-forward, and that is what isolates
   backpressure.** For each keyframe:
   - The **ingest goroutine** (publisher side) reads the entire stream into a
     single bounded buffer capped at `MaxKeyframeBytes`. That buffer atomically
     becomes the broadcast's cached keyframe. Ingest never writes to a
     subscriber, so a subscriber can never slow ingest. If the stream exceeds
     the cap or malforms, it is `CancelRead`-ed and counted; the cache is left
     intact.
   - **Fan-out** then, under the registry lock, snapshots the current
     subscriber set and hands each subscriber the immutable keyframe buffer;
     the actual `OpenUniStream` + `Write` happens **outside the lock**, on a
     per-subscriber goroutine. A blocked or slow write touches only that one
     subscriber's goroutine.
   - Store-and-forward costs one keyframe's worth of receive latency before
     fan-out begins (sub-millisecond on the 1 gbps homelab for a few-hundred-KB
     keyframe). That is a deliberate, bounded trade for total publisher/peer
     isolation, and it mirrors how the datagram path already fully buffers each
     datagram before enqueueing.

   Cut-through streaming (forward bytes as they arrive) was rejected: it
   couples the forwarding loop to the slowest subscriber's flow control and
   forces per-subscriber intermediate buffering — exactly the coupling we are
   eliminating.

4. **Per-subscriber stream lifecycle: write deadline, supersede-stale, ≤1
   in-flight.** Each subscriber gets at most **one** outstanding keyframe
   stream. On a new keyframe:
   - If the previous keyframe stream to that subscriber is still in flight, it
     is `CancelWrite`-ed (superseded — the newest keyframe is the only one that
     matters; this is "play the latest" applied to fan-out) and counted as a
     keyframe drop for that subscriber.
   - The new write runs with `SetWriteDeadline(now + KeyframeWriteTimeout)`. On
     deadline/error the stream is `CancelWrite`-ed and counted; the subscriber
     recovers at the next keyframe (≤ 1 GOP). No queue of keyframe streams is
     ever built up — a subscriber that can't keep up simply skips keyframes,
     precisely the datagram drop discipline. `KeyframeWriteTimeout` defaults to
     a small multiple of the GOP (e.g. 1 s) so a transient hiccup still
     completes but a genuinely stuck peer is abandoned fast.

5. **Cache and late-joiner priming move to the stream path.** The hub's
   `cachedKeyframe` becomes the last keyframe stream's bytes (header stripped
   or retained — retained is simplest: prime replays the exact bytes) plus the
   embedded config; the separate `cachedConfig` datagram machinery and the
   datagram `keyframeAssembly` reassembly are removed. On `Subscribe`, the hub
   registers the subscriber and captures a snapshot of the cached keyframe;
   **outside the lock** a prime goroutine opens a uni stream to the new
   subscriber and writes the cached keyframe, subject to the same
   deadline/supersede rules (a live keyframe arriving mid-prime cancels the
   prime). Deltas begin flowing on datagrams immediately; the viewer's reorder
   buffer holds them until the primed keyframe lands. This preserves today's
   "first picture without waiting for the next keyframe" behavior.

6. **Resource bounds and hardening (R2 is not optional here).** New vectors and
   their bounds:
   - `MaxKeyframeBytes` (new knob, default e.g. 8 MiB) caps a single keyframe
     buffer — the stream analogue of `MaxChunkCount`'s memory-inflation
     defense. Ingest refuses to allocate beyond it.
   - At most one in-flight publisher stream is expected; the accept loop bounds
     concurrently-open publisher streams (cancel extras) so a hostile publisher
     can't open thousands.
   - Per subscriber, ≤1 in-flight fan-out stream (Decision 4) bounds concurrent
     server→subscriber streams to `subscribers` at any instant.
   - The **bandwidth limiter must count stream bytes.** Because a reliable
     stream can't be dropped mid-flight, the budget is checked **before**
     opening each subscriber's keyframe stream; over-budget → skip that
     subscriber's keyframe this round (recovers next keyframe), counted as a
     bandwidth drop — same "drop, don't queue" semantics the datagram drain
     already uses.
   - **Every knob crosses `registryOptions` in `cmd/gawk-server/main.go`** and
     is exposed as a flag + `GAWK_*` env + Helm value. The R2 review's critical
     finding was a knob wired only into the test helper; S3's acceptance
     criteria explicitly asserts reachability from `registryOptions`.

7. **Viewer reorder buffer — bounded reorder, NOT a fixed playout offset.**
   The preliminary sketch proposed holding early frames for 200 ms *and*
   "maintaining a fixed ~200 ms offset from the source clock." A constant
   playout delay is exactly what this project has refused since day one:
   *"playing the latest frames is strictly more important than playing every
   frame"*, sub-500 ms glass-to-glass. A fixed 200 ms offset spends 40 % of the
   latency budget to smooth pacing we've explicitly chosen not to smooth. So:
   - **We keep the reorder behavior, drop the fixed offset.** The buffer merges
     the two channels by `frameId` and releases as *soon* as the next needed
     frame is decodable — no artificial hold on the steady path.
   - The buffer distinguishes the **two kinds of "not yet decodable"**, which
     is the subtle part of merging a reliable channel with a lossy one:
     - *Waiting for an in-progress keyframe.* Because keyframes are reliable
       they **will** arrive (delayed only by retransmit — tens of ms on a LAN).
       Decodable-pending deltas after that keyframe are held up to
       `KEYFRAME_WAIT_MS` (a ceiling, not a target; the steady case releases
       in ≪ that). If the keyframe still hasn't arrived, the held deltas are
       undecodable anyway and are dropped. **Amended by the R10 field
       finding (docs/14)**: the original ~200 ms assumed retransmit-scale
       delay, but a ~236 KB keyframe store-and-forwarded to a congested peer
       was measured landing > 500 ms behind its deltas — at 200 ms every GOP
       degenerated into keyframe-only playback. The ceiling is now
       **1000 ms**, aligned with `MAX_BUFFERED_FRAMES` (~1.07 s at 60 fps) as
       the memory bound.
     - *A missing predecessor **delta**.* A lost delta never retransmits, so
       waiting is pointless. If frame `p+2` arrives while `p+1` is absent past
       a tiny `DELTA_GAP_GRACE_MS` (≈ 2 frame intervals, ~60 ms), declare a gap,
       **freeze immediately**, drop the orphans, and resync at the next
       keyframe (≤ 1 GOP, now guaranteed).
   - Decode position = frameId of the last frame handed to the decoder. Release
     rule: jump to any keyframe `K ≥ position` (self-contained; drop everything
     below it), then release contiguous deltas `K+1, K+2, …`. Decoder-queue-deep
     still forces drop-to-keyframe (today's `getMaxDecoderQueueSize()` check,
     relocated here). The existing `viewer.ts` `lastFrameId` freeze-on-gap logic
     is **subsumed** by this buffer.
   - A constant-offset de-jitter playout mode is deferred to **R5 (viewer
     live-edge)**, which is the correct home for any latency-for-smoothness
     trade and which starts with measurement. R8 must not silently pre-empt
     that decision.

8. **Worker offload with a host-agnostic, synchronously-testable core.**
   `viewer.worker.ts` is a thin `onmessage` shell around a `ViewerWorkerCore`
   that takes an injected *host* (`postMessage`-like status sink + a render
   sink that wraps the `OffscreenCanvas` 2D context). The core contains the
   S1–S5 pipeline **and** the reconnect state machine (today's `ViewerSession`
   logic moves in). Rationale:
   - The `OffscreenCanvas` can be `transferControlToOffscreen`-transferred
     **exactly once** and cannot be transferred back, so the worker must be
     persistent for the life of the view and reconnects must happen *inside*
     it — a per-attempt fresh worker would lose the canvas. The main thread
     just relays `start`/`stop` and renders status from posted messages.
   - Decoded `VideoFrame`s are drawn to the `OffscreenCanvas` **in the worker**
     and never `postMessage`-d — no per-frame structured clone/transfer, no
     main-thread paint. Only small status/stats/config/lifecycle messages
     cross the boundary.
   - Tests instantiate `ViewerWorkerCore` directly with the existing fake
     `WebTransport` (extended for `incomingUnidirectionalStreams`) and a fake
     render sink — no real Worker, no real DOM, no `OffscreenCanvas` — keeping
     the current synchronous testing approach (`viewer.test.ts` pattern). The
     docs/11 `normalizeAvccExtradata` interceptor moves into the core unchanged.

9. **Deployment is lock-step; no dual-mode negotiation.** A new broadcaster
   sending keyframes on streams cannot be understood by an old viewer, and vice
   versa. For a coordinated homelab deploy (frontend + relay released together,
   docs/05) that is acceptable and far simpler than capability negotiation. The
   relay still forwards datagram `TypeVideoChunk`s verbatim regardless of the
   keyframe flag, but only the stream path populates the cache; an old
   broadcaster against a new relay would therefore fail to prime late joiners —
   noted, not supported. Capability negotiation is an explicit non-goal.

10. **Observability.** `/statusz` per-broadcast + totals gain keyframe-stream
    counters: `keyframeStreamsIn`, `keyframeBytesIn`, `keyframeStreamsOut`,
    `keyframeStreamsDroppedSlow`, `keyframeStreamsOversize`. Viewer-side reorder
    holds/drops surface in the stats overlay (and are the natural slot for an
    R5 live-edge metric later). This keeps limit tuning data-driven, per the R2
    observability principle.

## Server design detail (embracing the complexity)

The relay is where R8 stops being a UI change and becomes a concurrency
problem. This section specifies the goroutine, lifecycle, and lock model
precisely, because "handle backpressure appropriately" is the whole risk.

### Publisher session: two ingest loops

`handlePublish` today runs a single `for { ReceiveDatagram }` loop. It gains a
concurrent stream-accept loop:

```
handlePublish(after upgrade + StartPublish + announce):
    ctx := r.Context()                       // publisher request context
    go acceptKeyframeStreams(ctx, sess, pub) // NEW
    for { dgram := sess.ReceiveDatagram(ctx); pub.HandleDatagram(dgram) }  // deltas + config-on-datagram gone; deltas only

acceptKeyframeStreams(ctx, sess, pub):
    for {
        rs := sess.AcceptUniStream(ctx)      // publisher-initiated only
        if err != nil { return }             // session gone → loop exits
        go pub.IngestKeyframeStream(ctx, rs)  // bounded; see below
    }
```

- The two loops share the request context; when the publisher session ends,
  `ReceiveDatagram` and `AcceptUniStream` both error and both loops return —
  no separate shutdown signal, no leak. (Race-detector-clean exit is an S2
  acceptance criterion.)
- `AcceptUniStream` on the *server* only yields **publisher-initiated** streams.
  The server's own announce stream is server→publisher (`OpenUniStream`) and
  never appears here — no collision.
- `IngestKeyframeStream` reads with a read deadline and a hard
  `MaxKeyframeBytes` cap (`io.LimitReader` + explicit length check against the
  header's `payloadLen`). On success it hands the assembled buffer to the hub,
  which updates the cache and triggers fan-out. On any error it `CancelRead`s
  and increments `keyframeStreamsOversize`/bad counters. A cap on concurrently
  open publisher streams guards against a hostile publisher.

### Fan-out: lock discipline

The existing hub already has the right shape — a bounded queue per subscriber
drained by a private goroutine, non-blocking enqueue, drops on overflow. The
stream fan-out reuses the *pattern*, not the *queue*:

```
hub.onKeyframe(buf):               // called from IngestKeyframeStream, no lock held
    r.mu.Lock()
    b.cachedKeyframe = buf         // atomic swap of the immutable buffer
    subs := snapshot(b.subs)       // copy the pointer set under lock
    r.mu.Unlock()
    for s := range subs {
        s.sendKeyframe(buf)        // per-subscriber, NON-blocking dispatch
    }

Subscriber.sendKeyframe(buf):
    // supersede any in-flight stream to this subscriber, then start a new
    // goroutine that OpenUniStream + SetWriteDeadline + Write + Close.
    // Bandwidth check happens here, before OpenUniStream.
```

- `cachedKeyframe` is an **immutable** `[]byte` once assembled, so it is shared
  read-only across every subscriber goroutine and the prime path — no copy per
  subscriber, no lock while writing.
- `sendKeyframe` must not block the fan-out loop: it swaps the subscriber's
  current keyframe-stream handle (cancelling the previous under a small
  per-subscriber mutex) and spawns the writer goroutine. The registry lock is
  never held across a stream write.
- The writer goroutine is bound to the subscriber's `done` channel / session
  context so a departing subscriber cancels its in-flight keyframe write.

### The `hub.Conn` interface grows a stream capability

Today:

```go
type Conn interface {
    SendDatagram(payload []byte) error
    CloseWithError(code uint32, reason string) error
}
```

R8 adds a stream-open method returning a minimal writer the hub controls,
keeping the hub testable without webtransport-go:

```go
type Conn interface {
    SendDatagram(payload []byte) error
    OpenKeyframeStream() (KeyframeStream, error) // NEW
    CloseWithError(code uint32, reason string) error
}

type KeyframeStream interface {
    SetWriteDeadline(t time.Time) error
    Write(p []byte) (int, error)
    Close() error
    CancelWrite() // maps to SendStream.CancelWrite(code)
}
```

The `webtransportSessionAdapter` in `transport/server.go` implements
`OpenKeyframeStream` via `sess.OpenUniStream()`. Tests supply a fake `Conn`
whose `OpenKeyframeStream` returns a blocking or erroring writer to exercise
the deadline/supersede/drop paths deterministically — the same way the datagram
tests inject a `Conn` today.

### Why this answers the key design question

> How to manage server-side backpressure when a subscriber stalls on a reliable
> stream, preventing it from blocking the publisher's broadcast stream.

- The publisher stream is read to completion **before** any subscriber write
  exists — structural decoupling, not a timeout race.
- Each subscriber write is an independent goroutine with a write deadline; a
  stall cancels **that** stream only.
- ≤1 in-flight keyframe stream per subscriber + supersede-stale bounds memory
  and always favors the newest keyframe.
- Bandwidth is checked before opening, never mid-stream.

The failure mode of a slow subscriber is identical to the datagram world:
it misses frames (here, keyframes) and recovers at the next one. Nobody else
notices.

## Wire format

Datagram formats (`TypeVideoChunk`, `TypeDecoderConfig`, `TypeBroadcastAnnounce`)
are **unchanged**. One message type is added, used **only on uni streams**:

```
0x04 StreamFrame (24-byte header, then optional config block, then payload):
  byte 0        uint8   version (0x01)
  byte 1        uint8   type = TypeStreamFrame (0x04)
  byte 2        uint8   flags   bit0 = keyframe (1 for now; reserved otherwise)
  byte 3        uint8   reserved (0)
  bytes 4-7     uint32  frameID
  bytes 8-15    uint64  timestampUs
  bytes 16-19   uint32  configLen   (0 = no embedded config in this stream)
  bytes 20-23   uint32  payloadLen  (encoded keyframe byte length)
  bytes 24..    configLen bytes: a complete DecoderConfig datagram (its own
                0x01/0x02 prefix included) so the viewer parses it with the
                existing parseDecoderConfig, and the relay inspects it with
                the existing ParseDecoderConfig — maximal reuse.
  then          payloadLen bytes: raw encoded keyframe (fed straight into
                EncodedVideoChunk{ type:'key' }).
```

- Both lengths are validated before allocation: `24 + configLen + payloadLen`
  must equal the bytes read to EOF and must be ≤ `MaxKeyframeBytes`; otherwise
  the stream is rejected (mirrors the datagram parsers' strict validation).
- The Go `wire` package gains `AppendStreamFrameHeader`/`ParseStreamFrameHeader`
  (+ `MaxKeyframeBytes` const default) and the TS mirror gains
  `encodeStreamFrameHeader`/`parseStreamFrameHeader`. Golden hex vectors are
  added to `wire_test.go` and copied verbatim into `wire.test.ts` — the same
  cross-language byte-compatibility guarantee the existing vectors give
  (per the wire.ts header comment: if a vector fails, the format drifted).

## Frontend design detail

### Worker message protocol (main ↔ worker)

Main → worker: `{type:'start', serverUrl, broadcastId, connectOpts}`,
`{type:'stop'}`, plus the one-time `OffscreenCanvas` transfer at construction.
Worker → main: `{type:'connected'}`, `{type:'reconnecting', attempt, reason}`,
`{type:'config', codec}`, `{type:'stats', …ViewerStats + reorder fields}`,
`{type:'error', message, fatal}`, `{type:'ended'}`. These mirror today's
`ViewerSessionCallbacks` one-for-one, so `ViewerScreen`'s state machine
(connecting/watching/reconnecting/ended/error) is preserved almost verbatim —
it swaps direct `ViewerSession` construction for a worker handle and its
`onDecodedFrame` canvas draw (which moves into the worker).

### Two-channel ingest inside the core

- `wt.datagrams.readable` → `readDatagrams` → `Reassembler` (delta multi-chunk
  assembly, unchanged) → reorder buffer as *delta* input.
- `wt.incomingUnidirectionalStreams` → per-stream reader: read to EOF (bounded
  by `MAX_KEYFRAME_BYTES`), `parseStreamFrameHeader`, split config/payload,
  apply the AVCC normalization to the embedded config, → reorder buffer as
  *keyframe* input.
- Reorder buffer (Decision 7) → `Decoder` → render sink → `OffscreenCanvas`.

The delta chunking/reassembly path is retained because high-motion **delta**
frames can still exceed the (dynamic, Firefox-aware) datagram MTU and need
splitting; only keyframes leave the datagram path.

## Chunked implementation plan

Ordering and independence:

- **S1 → S2 → S3** is the server spine and must land in order; S3 is the
  highest-risk chunk and carries the backpressure crux + the R2 plumbing rule.
- **S4** (broadcaster) pairs with S2/S3 for an end-to-end reliable-keyframe path
  and can be verified against the server before the viewer is rewired (a
  temporary main-thread stream reader is fine for bring-up).
- **S5** (reorder buffer) is pure and test-first, independent of the worker.
- **S6** (worker) is **independent of S1–S5's protocol work** — it relocates
  the pipeline. Structure S5's buffer and the pipeline core host-agnostic from
  the start so S6 *moves* code rather than rewriting it. S6 could even ship
  first (pure latency/jank win, zero server dependency) if we want the worker
  benefit before the stream work is ready.
- **S7** closes observability + verification + docs.

Recommended sequence: **S1, S2, S3, S4, S5, S6, S7** — reliable keyframes
(the correctness fix) first, worker second. Each chunk is a separate task with
test-first bug-fix discipline (CODE-REVIEW.md): any bug found during a chunk
gets a failing test before the fix.

## Risks & open questions

- **WebTransport in a dedicated Worker on Firefox.** Chrome exposes
  WebTransport in `DedicatedWorkerGlobalScope`; Firefox support must be
  verified on the target build. If Firefox lacks it, S6 falls back to running
  the pipeline on the main thread for Firefox only (feature-detect and branch)
  while Chrome gets the worker — the core is host-agnostic, so this is a host
  choice, not a rewrite.
- **Firefox as broadcaster opening uni streams** (`createUnidirectionalStream`)
  must be verified; the reliable path also *sidesteps* Firefox's smaller
  datagram MTU (docs/11 §1) for keyframes, which is a bonus.
- **`KEYFRAME_WAIT_MS` / `DELTA_GAP_GRACE_MS` / `KeyframeWriteTimeout` /
  `MaxKeyframeBytes` tuning** needs real-hardware measurement (same posture as
  R4's thresholds — named in one place, tuned in S7). Start conservative.
- **Glass-to-glass regression check.** Store-and-forward adds one keyframe's
  receive latency at the relay and a small reorder hold at the viewer; S7 must
  confirm the sub-500 ms budget still holds and that no *fixed* latency crept
  in (Decision 7).
- **Keyframe size vs. `MaxKeyframeBytes`.** A native-resolution scene-cut
  keyframe can be large; the default cap must clear realistic worst cases while
  still bounding abuse. Measure typical keyframe sizes in S7 before finalizing.

## Verification

1. **Automated** — `wire` golden-vector round-trips (Go + TS); `hub` tests with
   injected blocking/erroring `Conn` for the deadline/supersede/drop/prime
   paths, race-detector clean; `reorder-buffer.test.ts` for the merge/gap/hold
   state machine; `ViewerWorkerCore` synchronous tests with the extended fake
   `WebTransport`; `registryOptions` reachability assertion for every new knob;
   full `tsc`/lint/build/`go test ./...` green.
2. **Manual packet loss** — Chrome DevTools / `tc netem` induced loss: verify
   keyframe freezes are eliminated, a lost delta resyncs within one GOP, and
   the picture recovers deterministically.
3. **Backpressure** — throttle one subscriber (e.g. paused tab / constrained
   link) and confirm via `/statusz` that only its `keyframeStreamsDroppedSlow`
   grows while the publisher and other subscribers are unaffected.
4. **Profiling** — React profiler under heavy UI interaction shows no viewer
   drop spikes (the worker isolation goal); confirm decoded frames are never
   posted across the thread boundary.

## Non-goals

- **Deltas on streams / any general reliable video path** — Decision 1.
- **A fixed-offset de-jitter playout buffer** — deferred to R5; R8 adds no
  constant latency (Decision 7).
- **Viewer→server keyframe-request channel** — that one-way-to-two-way protocol
  change belongs to R5's open questions, not here.
- **Client/server capability negotiation for the stream path** — lock-step
  deploy instead (Decision 9).
- **MoQ / WebRTC** — unchanged from CLAUDE.md's out-of-scope list.
```
