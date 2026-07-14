# R10 — Viewer Render Performance: Design + Implementation Plan

Design doc for [ROADMAP R10](../ROADMAP.md#r10--viewer-render-performance).

Firefox viewers show two coupled symptoms that Chrome viewers don't:
a significant **"Dropped (incomplete)"** rate (reassembler-level datagram
loss) and **decoded fps persistently below received fps** (the decoder falls
behind and the resync-to-keyframe policy dumps work). This doc diagnoses
both, records the fix ideas, and implements the two cheapest ones — both
confined to the worker-side **`RenderSink`** seam introduced by R8 S6. Zero
server, wire, broadcaster, or protocol changes.

## Background: the diagnosis

On the worker path (which Firefox takes — it has worker `WebTransport` +
`VideoDecoder`), **one worker thread runs everything**: the datagram read
loop, reassembly, the reorder buffer, decoder feeding, *and* rendering.
`ViewerPipeline.handleDecoded` calls `RenderSink.draw()` synchronously from
the `VideoDecoder` output callback, and the sink is a **2D-canvas
`drawImage(VideoFrame)`** (`transport/render-sink.ts`).

That draw is the problem multiplier on Firefox:

1. **Firefox's 2D-canvas `drawImage(VideoFrame)` is a slow path** — it
   typically performs a synchronous CPU YUV→RGB conversion per frame
   (Chromium GPU-accelerates this; Gecko largely does not, especially when
   the decoder output is a software frame).
2. While the worker thread is stuck in `drawImage`, **the browser's small
   incoming-datagram queue overflows and silently drops datagrams** — one
   lost chunk discards the whole frame, surfacing as "Dropped (incomplete)".
   (This is invisible on Firefox: it lacks `WebTransport.getStats()`, so the
   overlay's "Dgrams dropped (in)" row is `—` there.)
3. The decoder's output callbacks queue behind the same busy thread, so
   decoded fps lags received fps; once `decoderQueueDepth` passes
   `maxDecoderQueueSize` (5), the pipeline requests a reorder-buffer resync
   and waits for the next keyframe — with a 500 ms GOP, a decoder that is
   persistently ~10% too slow cycles through overflow → dump → keyframe
   wait, amplifying a small deficit into a visible fps gap.

Independently, **Firefox's WebCodecs H.264 decode is often software**
(ffvpx), especially on Linux. Note the overlay's "Decode mode" row is *not*
ground truth: `Decoder.isHardwareAccelerated` only records that the
`prefer-hardware` **probe** was accepted, and Firefox treats the hint as
advisory — it can report "Hardware" while decoding in software.

**Why YouTube is fine on the same machine**: `<video>` + MSE is a 5–30 s
buffer over TCP (loss invisible), ABR that quietly degrades, and decode +
presentation entirely inside the browser's native, hardware-backed media
pipeline with zero JavaScript per frame. gawk deliberately trades all of
that for sub-500 ms live edge and runs Firefox's newest, least-optimized
media path (WebCodecs, shipped 2024) with JS in the per-frame loop. The gap
is the price of the latency target; this doc's job is to stop paying more
of it than necessary.

## The ideas (full list, including deferred)

| # | Idea | Attacks | Status |
|---|------|---------|--------|
| 1 | **Coalesced rendering** — latest-frame-wins, one draw per `requestAnimationFrame` tick instead of one per decoded frame | Worker-thread starvation (both symptoms) | **P1, this doc** |
| 2 | **WebGL render sink** — replace 2D `drawImage` with a WebGL textured quad (`texImage2D(VideoFrame)`); 2D stays as fallback | Per-draw CPU cost on Firefox | **P2, this doc** |
| 3 | **Transport/decode worker split** — WebTransport read loops in their own worker, posting transferable buffers to the decode/render worker, so no render/decode pressure can ever starve the datagram queue | Residual datagram drops under load | Deferred — only if P1+P2 measurements say the reader is still starved |
| 4 | **Decode-load reduction & backpressure tuning** — confirm the real decode path via `about:support`; broadcaster picks a lower rung when viewers are software-decoding; possibly a Firefox-only larger decoder queue bound or stickier overload response to break the overflow→dump→wait cycle | Software-decode deficit | Deferred — needs post-P1/P2 measurements first |

Per-viewer quality (simulcast/SVC) remains explicitly out of scope, as in R4.

## Decisions (locked)

1. **WebGL over `bitmaprenderer`.** Both avoid Firefox's software 2D path,
   but WebGL is expected to be faster and more predictable:
   `texImage2D(VideoFrame)` is the classic video-upload path every WebGL
   player exercises, it is synchronous with no per-frame garbage, and
   browsers keep it on the GPU where the platform allows. `bitmaprenderer`
   needs `createImageBitmap(frame)` per frame — a per-frame allocation plus
   an async hop, and in Gecko the ImageBitmap conversion can hit the same
   software conversion the 2D path does, i.e. it may just move the cost, not
   remove it. The `RenderSink` seam keeps a `bitmaprenderer` variant cheap
   to add later **if measurements disagree** — decisions here are
   falsifiable by the overlay funnel, by design.
2. **Latest-frame-wins at display cadence.** The sink draws at most once per
   `requestAnimationFrame` tick (worker rAF: Chrome and Firefox ≥ 105); a
   frame still pending when a newer one decodes is closed unseen. Painting
   faster than the display refreshes is pure waste, and this is the R5
   "latest-frame-first rendering" bullet landing early, at the sink level.
   Where worker rAF is unavailable, fall back to a ~16 ms timer — coalescing
   is the point, the exact clock is not.
   **Consequence**: the overlay's "Rendered fps" now reads
   ≈ min(decoded fps, display Hz) and *lower than decoded fps under load is
   healthy* — it means coalescing is shedding paints, not that frames are
   lost. The funnel's loss signals stay "Dropped (incomplete)" and the
   decoded-vs-received gap.
3. **Worker path only; the 2D sink survives as fallback.** The main-thread
   `ViewerSession` fallback (browsers without worker WebCodecs/WebTransport)
   keeps its existing draw-in-callback path — it isn't the population with
   the problem, and rAF pacing on the main thread has different trade-offs.
   The 2D `OffscreenCanvasRenderSink` also remains as the in-worker fallback
   when WebGL context creation fails.
4. **The active renderer becomes observable.** `ViewerStats` grows a
   `renderer` field (`'webgl' | '2d' | null`), shown in the stats overlay
   and included in Copy-diagnostics JSON — remote "is your Firefox actually
   on the WebGL sink?" must be answerable from data, per R9.
5. **Zero wire/server/broadcaster changes.** Everything lands behind the
   `RenderSink` interface; `ViewerPipeline`, `ViewerWorkerCore`, and the
   worker shell keep their contracts (the shell just constructs the sink via
   a factory now).

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| P1 | **Coalescing sink** — `CoalescingRenderSink` wrapping any inner sink: latest-frame-wins, one inner `draw()` per scheduler tick (worker rAF, ~16 ms timer fallback), superseded frames closed unseen | Unit tests (fake scheduler, fake frames): N draws before a tick → 1 inner draw of the newest frame, N−1 older frames closed exactly once, nothing scheduled twice per tick; a frame arriving after a flush schedules a new tick; `drawnFrames()` delegates to the inner sink so "Rendered fps" counts real paints | ✅ done |
| P2 | **WebGL sink + factory + renderer stat** — `WebGLRenderSink` (textured quad, `texImage2D(VideoFrame)`, resize + viewport tracking, context-lost tolerance); `createRenderSink()` factory (WebGL2 → WebGL → 2D, wrapped in P1's coalescer) used by `viewer.worker.ts`; `ViewerStats.renderer` + overlay "Renderer" row | Unit tests (fake GL): draw uploads the frame via `texImage2D` and closes it (also on context-lost and on exceptions); canvas resize + `viewport` only when frame dimensions change; factory falls back to the 2D sink when WebGL contexts are unavailable and the result is always coalescing-wrapped; stats carry `renderer` on the worker path (`null` main-thread), overlay renders the row; all gates green | ✅ done |
| P3 | **Transport/decode worker split** | (design when picked up — requires P1/P2 measurements first) | deferred |
| P4 | **Decode-load reduction & backpressure tuning** | (design when picked up — requires P1/P2 measurements first) | deferred |

## Verification plan (manual, Firefox on the affected machine)

Before/after comparison using the stats overlay (`Ctrl+Alt+Shift+D`) at the
same broadcaster settings, ~60 s samples via Copy diagnostics:

- "Renderer" row reads **webgl** (fallback check: force WebGL off →
  `2d`, picture still renders).
- "Dropped (incomplete)" rate collapses toward the Chrome baseline —
  confirms the starvation theory (idea 3 stays shelved if so).
- Decoded fps ≈ received fps; "Gap resyncs" stops climbing during steady
  playback.
- Rendered fps ≈ min(decoded fps, display Hz) — coalescing active, no
  visible jank added.
- Cross-check on Chrome: no regression (Chrome was already fine; the WebGL
  sink now serves it too, and "Dgrams dropped (in)" — visible there via
  `getStats()` — should stay ~0).

If "Dropped (incomplete)" stays high after P1+P2 on Firefox, that is the
signal to pick up P3 (reader starvation persists) and/or P4 (pure decode
deficit) — re-measure, then design.

## Non-goals

- Main-thread fallback path rendering changes (Decision 3).
- A `bitmaprenderer` sink (Decision 1 — add only if WebGL measurements
  disappoint).
- Per-viewer quality adaptation, playout buffering (R5), or any
  transport/protocol change.
