# R11 — Broadcaster worker offload

**Status**: implemented 2026-07-14 (chunks K1–K4); automated gates green,
manual browser verify pending.

## Goal

Move the broadcaster's frame path — capture consumption, pre-encode
scaling/gating, encoding, packetization, and transmission — off the page's
main thread into a dedicated Web Worker, mirroring the viewer's R8 S6
architecture. After this, main-thread work on the broadcast page (React
renders, the preview `<video>`, devtools, GC) can no longer add latency or
jitter to the frame pipeline, and the two halves of the system are
architecturally symmetric: pipeline in a worker, UI on main, control
messages between them, media never crossing back.

## What this does and does not buy (be honest)

- **Does**: insulates the frame path from main-thread contention (the
  plumbing between MSTP and `VideoEncoder.encode()` currently competes with
  React + preview compositing); removes the tab's UI responsiveness from the
  encode/send critical path; symmetry with the viewer.
- **Does not**: escape Chrome's *process-level* backgrounding. A dedicated
  worker lives in the same renderer process; OS priority demotion applies to
  every thread in it. (In practice a capturing tab is treated as active and
  isn't demoted — and NVENC encode/datagram I/O already run in the GPU/
  network service processes.) This change is about main-thread *contention*,
  not OS scheduling priority.

## Locked decisions

1. **Track transfer, not readable transfer.** The main thread transfers a
   `track.clone()` into the worker and the worker creates its own
   `MediaStreamTrackProcessor` there. Transferring `processor.readable`
   (MSTP created on main) was **rejected**: a transferred stream's chunks are
   still enqueued through the producing realm, so every frame would keep
   hopping through the main thread — exactly the exposure this split removes.
   Track transfer is also the spec-aligned shape (the current
   mediacapture-transform spec exposes MSTP in workers only; Chromium's
   Window exposure is legacy).
2. **`getDisplayMedia` stays on main; connect-before-picker ordering is
   preserved.** Acquisition needs the window scope + user gesture. The worker
   connects `/publish` first and only then asks main for capture
   (`awaitingCapture` event → main runs the share picker → posts the cloned
   track back, transferred). A connect failure therefore still never shows
   the share picker, and `BroadcastStartError.phase` semantics are identical:
   `'connect'` (reclaim→mint fallback allowed) vs `'capture'` (session was
   live; pipeline tears it down — no zombie publisher — and the error
   surfaces).
3. **Preview keeps the original track on main; the clone is the encode
   source.** Transfer detaches a track from its realm, so the original stays
   attached to the preview `<video>` and the *clone* is transferred. The
   browser's "Stop sharing" ends the whole source (original + clone): the
   worker-side pipeline stops itself via its own track's `ended` (the
   existing pipeline behavior), and main keeps a belt-and-braces `ended`
   listener that posts `stop`. Both paths are idempotent.
4. **Capability gate + fallback, viewer-style.** Worker path requires:
   main-side `Worker` + `HTMLCanvasElement.captureStream` (for the probe);
   worker-scope `VideoEncoder`, `WebTransport`, `MediaStreamTrackProcessor`,
   `OffscreenCanvas` (boot handshake, same shape as `viewer.worker.ts`); and
   `MediaStreamTrack` transferability, probed **synchronously** by
   postMessage-ing a dummy `canvas.captureStream()` track — a non-transferable
   type makes `postMessage` throw `DataCloneError` on the *sender*, so the
   probe needs no roundtrip. Any gate failing → the untouched main-thread
   `BroadcastPipeline` runs (Firefox always lands here: no MSTP anywhere and
   the rvfc fallback needs DOM). The probe runs before `start()`, so path
   choice never happens mid-session.
5. **Same interface, so `BroadcasterScreen`'s logic is untouched.** A
   `BroadcastSessionLike` interface (`start`/`stop`/`setLadder` +
   `BroadcastCallbacks`) is implemented by both `BroadcastPipeline` and the
   new `WorkerBroadcastSession`; an async `createBroadcastSession()` factory
   picks the path. The reclaim→mint fallback, error surfaces, and settings
   wiring in the screen don't change. The frozen debug `BroadcastPage`
   (`#/debug/broadcast`) keeps constructing `BroadcastPipeline` directly.
6. **One worker per session, terminated at session end.** The worker spawns
   on "Start a stream" (spawn cost is milliseconds against a connect RTT),
   so there is no long-lived worker state to invalidate across broadcasts on
   the main side; `BroadcastWorkerCore` still keeps the viewer core's
   generation guard against superseded-session callbacks.
7. **Zero server / wire / viewer changes.** Encoder, preprocessor,
   packetizer, `DatagramSender`, keyframe uni-streams, announce read,
   `FallbackController` — all reused byte-identically inside the worker; the
   pipeline gains only a media-source seam (Decision 8). The `onSourceStream`
   callback fires from the main side (it owns the preview stream); the
   worker-side pipeline simply has no stream to report.
8. **The seam is a `BroadcastMediaSourceFactory`.** `BroadcastPipeline`
   already treats capture as "a thing that yields frames + a native fps +
   an ended signal"; the seam names that
   (`capturePath`/`stream|null`/`nativeFps`/`onEnded`/`startFrames`/`stop`).
   Default factory wraps `startCapture` (main path, behavior identical);
   the worker factory posts `awaitingCapture` and resolves when the
   transferred track arrives. This is the `ViewerTransport`-seam move from
   R10 P3 applied to the other end of the pipeline.
9. **Frames never cross realms after acquisition.** Post-transfer, a
   `VideoFrame` is born (MSTP), scaled (OffscreenCanvas), encoded, and
   closed entirely inside the worker; worker→main traffic is small control/
   stats events only. This is the performance acceptance bar: the worker
   path must add **zero per-frame copies or postMessages** relative to the
   main-thread path.
10. **Placement is observable.** `BroadcastStats` gains
    `pipelineContext: 'worker' | 'main-thread'`, *detected* inside the
    pipeline via `window` absence (the viewer's R10 convention), shown in
    the broadcaster stats overlay and thus in Copy-diagnostics JSON. The
    worker capture path reports `capturePath: 'mstp-worker'`.

## Architecture

```
main thread                          broadcast worker
───────────                          ────────────────
BroadcasterScreen
  └─ createBroadcastSession()
       ├─ (fallback) BroadcastPipeline — unchanged main-thread path
       └─ WorkerBroadcastSession ──── broadcaster.worker.ts (shell: boot probe)
            │  'start' {config,url,opts,id,ladder}   └─ BroadcastWorkerCore
            │ ────────────────────────────────▶           └─ BroadcastPipeline
            │  'awaitingCapture'                                │ (worker media
            │ ◀────────────────────────────────                 │  source factory)
            │  getDisplayMedia() on main                        │
            │  preview ◀ original track                         │
            │  'capture' {track: clone (transferred), fps}      │
            │ ────────────────────────────────▶  MSTP(clone) → preprocess
            │  'stats'/'broadcastId'/'started'/…     → encode → packetize
            │ ◀────────────────────────────────      → WebTransport send
```

## Chunks

### K1 — media-source seam (pure refactor, main path identical)

`media/capture.ts`: extract `acquireDisplayStream()` (HW-probe fps cap +
`getDisplayMedia`) and the MSTP pump; add `BroadcastMediaSource` +
`trackMediaSource(track, nativeFps)` (lazy MSTP around a bare track,
`capturePath: 'mstp-worker'`). `transport/broadcaster.ts`: consume the seam
(default factory wraps `startCapture`), drop the `window.` prefix on the
stats timer, add `pipelineContext`, export `BroadcastSessionLike`.

| Acceptance criterion | Verified by |
|---|---|
| Existing pipeline tests pass unmodified (default factory ≡ old `startMedia`) | `broadcaster*.test.ts` green without edits |
| A source with `stream: null` runs media without firing `onSourceStream` | new test in `broadcaster.test.ts` |
| Teardown calls `source.stop()` on the injected source | new test in `broadcaster.test.ts` |
| `pipelineContext` reflects realm (`window` absence ⇒ `'worker'`) | new test in `broadcaster.test.ts` |

### K2 — worker core + shell

`transport/broadcast-worker-core.ts`: DOM-free `BroadcastWorkerCore`
(command/event protocol above; injectable pipeline factory for synchronous
unit tests, à la `ViewerWorkerCore`). `transport/broadcaster.worker.ts`:
boot capability post, `probeTrack` handling, command dispatch.

| Acceptance criterion | Verified by |
|---|---|
| `start` builds a pipeline, applies the ladder, posts `started` on resolve | `broadcast-worker-core.test.ts` |
| `startError` preserves `BroadcastStartError.phase`; foreign errors carry `phase: null` | 〃 |
| Worker media factory posts `awaitingCapture`; `capture` resolves it to a `trackMediaSource` (nativeFps threaded, `stream: null`); `captureFailed` rejects it | 〃 |
| All pipeline callbacks map to events; superseded-generation events are dropped | 〃 |
| `stop()` stops the pipeline and always ends with an `ended` post | 〃 |

### K3 — main-side session + path selection

`features/broadcaster/workerBroadcastSession.ts`: `WorkerBroadcastSession`
(same surface as `BroadcastPipeline`; owns acquisition, preview stream,
clone transfer, local teardown) + `createBroadcastSession()` (static gate →
spawn → boot wait (2 s timeout) → sync transfer probe → worker session;
else main-thread pipeline). `BroadcasterScreen` swaps `new
BroadcastPipeline(...)` for `await createBroadcastSession(...)` at both call
sites (reclaim + mint) — no other screen changes.

| Acceptance criterion | Verified by |
|---|---|
| `awaitingCapture` → acquire → clone transferred via `postMessage` transfer list → `onSourceStream(originalStream)` | `workerBroadcastSession.test.ts` (injected fake worker + acquire fn) |
| Acquire rejection posts `captureFailed`; `startError` events reject `start()` typed (phase preserved) and terminate the worker | 〃 |
| `ended` stops local (preview) tracks, fires `onEnded`, terminates the worker; `stop()` resolves on `ended` | 〃 |
| `setLadder` before `start` rides the start command; after, posts `setLadder` | 〃 |
| Environment without Worker/captureStream (jsdom) falls back to a real `BroadcastPipeline` | `createBroadcastSession` test |
| Reclaim→mint still only on `phase === 'connect'` | unchanged `BroadcasterScreen` logic + K2/K3 phase tests |

### K4 — observability + doc sync

Overlay `Pipeline` row (`Worker` / `Main thread`, mirroring the viewer's),
ROADMAP R11 entry, CLAUDE.md build-order + docs list, README gotcha (track
transfer detaches → clone for the worker, original for preview; sender-side
synchronous `DataCloneError` probe).

| Acceptance criterion | Verified by |
|---|---|
| Overlay shows the row; diagnostics JSON carries `pipelineContext` | `BroadcasterStatsOverlay.test.tsx` |
| All gates green (`npm test`, lint, build/tsc; server untouched) | CI |

## Verification plan (manual, pending)

On the gaming PC (Chrome): start a broadcast → overlay shows
`Pipeline: Worker`, `Capture fps`/`Encoder fps`/`Sent fps` match the
pre-split baseline at the same rung (no regression — Decision 9's zero-copy
claim is also checked by eyeballing funnel rates under load); preview live;
ladder changes apply mid-stream; "Stop sharing" from the browser bar ends
the session cleanly; reclaim after relay restart still works. On Firefox:
broadcast still works and overlay shows `Pipeline: Main thread` (fallback
engaged, zero regression). Kill test: DevTools CPU-throttle the main thread
(6×) while broadcasting — worker path should hold sent fps where the old
main-thread path sagged.
