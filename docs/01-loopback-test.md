# v0.1 — Local Loopback Test

This is build-order item #1 from `CLAUDE.md`. It proves out the full
**capture → encode → decode → render** pipeline in a single browser tab,
without any network transport. If the pipeline is sound here, WebTransport
(item #2) only needs to slot itself between encoder and decoder.

The loopback also gave us a UI scaffold, a state store, and a real
end-to-end latency measurement to compare against later.

## Success criteria

Both met on Chrome (broadcaster side), degraded-but-working on Firefox:

- User clicks **Start Capture**, picks a screen via `getDisplayMedia`
- Source (raw MediaStream) and Decoded (WebCodecs → Canvas 2D) previews
  update side by side, both matching the true source aspect ratio
- Stats panel shows non-zero encoder / decoder fps, encoder queue mostly at
  0, hardware acceleration when available, and a plausible end-to-end
  latency (< 100 ms on Chrome with H.264 hardware)
- No errors on Stop; no leaked `VideoFrame`s in devtools

## Stack decisions

| Choice                | Picked                       | Reason                                                                                              |
| --------------------- | ---------------------------- | --------------------------------------------------------------------------------------------------- |
| Bundler               | Vite                         | Fast HMR, standard React SPA setup today.                                                           |
| Language              | TypeScript                   | WebCodecs types (`VideoFrame`, `EncodedVideoChunk`, `VideoEncoderConfig`) are unwieldy without it.  |
| State                 | Zustand                      | Small app, few global slices (pipeline status, config, stats, error). Redux Toolkit would be heavy. |
| Decoded-frame drawing | Canvas 2D `drawImage`        | Simple, portable, easy to debug. WebGL / `MediaStreamTrackGenerator` are lower-CPU but Chrome-only. |
| Styling               | Vanilla CSS + CSS Modules    | Small UI, no framework overhead.                                                                    |

## Module structure

```
gawk-app/src/
  main.tsx                        # React entry
  App.tsx                         # top-level (currently just LoopbackPage)
  styles/global.css               # CSS variables, resets, button styles
  features/
    loopback/
      LoopbackPage.tsx            # composes the loopback experience
      LoopbackPage.module.css
      components/
        CaptureControls.tsx       # start/stop + status pill
        SourcePreview.tsx         # <video srcObject={stream} />
        DecodedPreview.tsx        # <div><canvas /></div>  (see aspect-ratio note)
        StatsPanel.tsx            # codec / accel / fps / queue / latency
  media/                          # framework-agnostic pipeline
    capture.ts                    # startCapture (MSTP | video+rVFC)
    encoder.ts                    # Encoder wrapper: codec + variant probing
    decoder.ts                    # Decoder wrapper: configured from encoder metadata
    loopback.ts                   # LoopbackPipeline: wires it all together
    types.ts                      # CaptureConfig, PipelineStats, codec preference list
  state/
    pipelineStore.ts              # Zustand: status, config, stats, encoderInfo, capturePath, error
  lib/
    logger.ts                     # tiny timestamped console logger
```

`media/` is deliberately framework-agnostic — no React, no Zustand, just
callbacks. When we add WebTransport (item #2), the encoder's output goes
to the wire and the decoder's input comes from the wire; the LoopbackPipeline
is replaced by two counterparts (BroadcasterPipeline and ViewerPipeline)
without touching the individual encoder/decoder/capture modules.

## Media pipeline

```
getDisplayMedia
  → MediaStreamTrack (video)
      ├─→ <video srcObject> (Source preview)
      └─→ Frame source (MSTP preferred, video+rVFC fallback)
            → VideoFrame
              → VideoEncoder.encode()          [H.264 realtime, hardware if available]
                → EncodedVideoChunk + decoderConfig (on first chunk)
                  → decoder chain (serialized promise)
                    → VideoDecoder.decode()
                      → decoded VideoFrame
                        → canvas.drawImage()
                        → frame.close()
```

Key operational rules baked into `LoopbackPipeline`:

- Encoder is **not** configured at startup. It's configured lazily on the
  first `VideoFrame` we actually receive, using that frame's real dimensions.
  See "Bug: getSettings() lied" below.
- Encoder rejects incoming frames when its queue depth exceeds 2. Those
  become `droppedFrames++`. Favor dropped frames over stalled playback.
- Every `VideoFrame` and `EncodedVideoChunk` is `.close()`d after use — they
  hold GPU-backed buffers.
- All decoder operations chain off a single serialized promise so
  `configure` completes before the first `decode`, and chunks are decoded
  in arrival order (P-frames never reach the decoder before the keyframe
  they depend on).

## Codec negotiation

Two nested loops in `Encoder.configure()`:

1. **Codec preference list**, in order:
   - `avc1.42E02A` (H.264 baseline level 4.2)
   - `avc1.640028` (H.264 high level 4.0)
   - `avc1.42E01F` (H.264 baseline level 3.1)
   - `vp09.00.40.08` (VP9 profile 0 level 4.0)
   - `vp09.00.31.08` (VP9 profile 0 level 3.1)
   - `vp8`
2. **Config variants**, from strongest to weakest hints:
   - `prefer-hardware` + `realtime`
   - `prefer-hardware`
   - `realtime`
   - default (no hints)

For each `(codec, variant)`, we call `VideoEncoder.isConfigSupported()` and
accept the first that returns `supported: true`.

The `prefer-hardware` + `realtime` variant is the happy path on Chrome —
NVENC/QuickSync/AMF hardware, low-latency mode, sub-millisecond encode
per frame. Firefox's `VideoEncoder` support is much narrower; it typically
lands somewhere in the fallback rungs, on VP8 or VP9 with software encode
and quality-mode latency, so Firefox as a *broadcaster* is degraded.
Firefox as a *viewer* is fine — its `VideoDecoder` handles all the codecs
we might send.

The "acceleration" stat is derived from the winning variant:
- `prefer-hardware` accepted + resolved config also says `prefer-hardware`
  → **hardware**
- resolved config says `prefer-software` → **software**
- otherwise → **unknown**

## Capture path

`capture.ts` exposes one API, `startCapture()`, and picks an implementation
at runtime:

- **`MediaStreamTrackProcessor`** (Chromium): hands us `VideoFrame`s
  directly off the `MediaStreamTrack`. Preferred.
- **`<video>` element + `requestVideoFrameCallback`** (Firefox and older
  Chromium): hidden `<video srcObject={stream} />`, `rVFC` gives us a
  timestamp per presented frame, and we build a `VideoFrame` from the
  video element via `new VideoFrame(videoElement, { timestamp })`. Works
  anywhere WebCodecs is available.

The `<video>` fallback pays two costs the MSTP path avoids:

- **Compositor cost**: the browser has to actually render the stream into
  the video element before we can grab pixels from it. Under game load,
  that competes with the game for CPU.
- **Tab-visibility throttling**: when a fullscreen game hides the Chrome
  tab, `requestVideoFrameCallback` is throttled aggressively. MSTP is not
  bound to a DOM element and keeps flowing.

To keep the pipeline uniform, the MSTP path re-timestamps each frame with
`performance.now() * 1000` (it constructs a new `VideoFrame` wrapping the
same underlying buffer, then closes the original — no copy). This way,
downstream latency math works identically regardless of which capture
path was chosen.

## Latency measurement

We stamp each captured frame's `timestamp` field in **microseconds since
navigation start**, i.e. `Math.round(performance.now() * 1000)`. That
timestamp propagates all the way through: encoder writes it into
`EncodedVideoChunk.timestamp`, decoder writes it into the decoded
`VideoFrame.timestamp`.

In `LoopbackPipeline.handleDecoded`:

```ts
const nowUs = performance.now() * 1000;
this.stats.lastEndToEndLatencyMs = (nowUs - decoded.captureTimestampUs) / 1000;
```

Both sides of that subtraction share the `performance.now()` timebase,
so the number is meaningful. Encode and decode latencies are measured
independently by recording start times in a `Map<timestampUs, startMs>`
before each `encode()` / `decode()`, and reading them back in the output
callback.

## Aspect-ratio handling

Every `VideoFrame` carries `displayWidth` / `displayHeight`. On each frame
in `LoopbackPage.onDecodedFrame`, we:

1. Set `canvas.width` / `canvas.height` to match the frame if they differ.
2. Set `canvas.parentElement.style.aspectRatio` to
   `${frame.displayWidth} / ${frame.displayHeight}` on every frame
   (cheap; idempotent). This is done on the **wrapper `<div>`**, not on the
   canvas itself. See DecodedPreview: `<div class="canvasFrame"><canvas
   class="canvasFill"/></div>`.

Why a wrapper div: Chrome's `<canvas>` implementation doesn't reliably
honor CSS `aspect-ratio: auto` or inline `style.aspectRatio` when the
canvas's buffer dimensions change at runtime. A plain `<div>` has no
intrinsic-aspect complications and always respects `aspect-ratio`. The
canvas fills its wrapper 100% × 100% with `object-fit: contain` as a
letterbox safety net.

The Source `<video>` sets its own inline `aspect-ratio` on
`loadedmetadata` and `resize`, using `videoWidth` / `videoHeight`. Video
elements do honor intrinsic aspect, so they don't need the wrapper trick.

## Bugs and gotchas we hit

Not just for future us — several of these are real cross-browser landmines
that anyone building on WebCodecs will trip.

### `getSettings()` lied on Chrome

`MediaStreamTrack.getSettings()` returned dimensions and aspect ratio that
disagreed with the frames MSTP actually delivered — reporting 1920×1429
(1.34:1) while `<video>.videoWidth`/`videoHeight` and the MSTP `VideoFrame`
both reported 1920×804 (2.39:1). We were configuring the encoder from
`getSettings()`, and Chrome's `VideoEncoder` silently rescaled incoming
frames from the true 1920×804 to the configured 1920×1428 — vertical
distortion baked into every encoded chunk.

**Fix**: configure the encoder from the first actual frame's
`displayWidth`/`displayHeight` instead of `getSettings()`. The lazy
initialization in `LoopbackPipeline.start()` handles this: the frame
callback runs before the encoder is created, and the first frame that
arrives determines the encoder's dimensions.

**Broader takeaway**: the `VideoFrame` you actually receive is the
authoritative source of truth. `getSettings()`, `getCapabilities()`,
`videoWidth`/`videoHeight`, and the frame itself can all disagree.

### getDisplayMedia constraints pillarbox ultrawide sources

With `width: { ideal: 1920 }` **and** `height: { ideal: 1080 }`, Chrome
scaled our ultrawide 21:9 source into a 1920×1080 frame with black bars
left/right. Firefox was looser. Even `max` on both dimensions had the same
effect: Chrome scales to fit both caps by squishing aspect.

**Fix**: constrain only *one* dimension (width) in `getDisplayMedia`
options. Chrome (and Firefox) then scales proportionally to preserve
source aspect.

### Odd frame dimensions break H.264

Chrome sometimes hands us odd heights (we saw 1429 in one config). H.264
encoders require even (and ideally macroblock-aligned) dimensions;
`isConfigSupported` returns `false` for `avc1.*` at odd height, and we
fall through to VP9. VP9 accepts more shapes.

**Fix**: `roundDownToEven()` on the frame's `displayWidth`/`displayHeight`
before passing to the encoder config. A one-pixel crop is imperceptible
and keeps H.264 hardware in play.

### Decoder race between `configure` and `decode`

The encoder's first chunk contains a `decoderConfig` we use to configure
the decoder. That `configure()` is async. Meanwhile, subsequent chunks
are arriving synchronously and hitting `decoder.decode()`, which is a
no-op if the decoder isn't `configured` yet — silently dropping P-frames.
Then when a later P-frame *does* reach a configured decoder, it
references a missed frame → `EncodingError: Decoding error.` This was
invisible with H.264 (which happened to line up) and immediately fatal
with VP9.

**Fix**: chain every decoder operation off a single serialized promise
(`decoderChain: Promise<void>` in `LoopbackPipeline`). Configure and all
decodes queue behind each other; order is preserved; nothing races.

### Firefox lacks `MediaStreamTrackProcessor`

Not available in Firefox at all as of writing. Detection at runtime, plus
the `<video>` + `requestVideoFrameCallback` fallback path described above.

### Firefox's `VideoEncoder` is much narrower than Chrome's

- H.264 encode: unavailable on Firefox+Linux, unreliable on Firefox+Windows
- Hardware acceleration hints: often rejected outright
- `latencyMode: 'realtime'`: not universally implemented

Together, these push Firefox broadcasters to software VP8/VP9 quality-mode
encoding, which is high-CPU and high-latency (100–300 ms of buffering for
better rate control). The codec preference list and config variant cascade
degrade gracefully into this rung — Firefox works, just badly.

For our actual production topology (one broadcaster on the gaming PC, up
to 15 viewers on any browser), this is fine: broadcaster is Chrome,
Firefox viewers only run the decoder side.

### `mediaTime` vs `presentationTime` for VideoFrame timestamps

Initially we stamped VideoFrames with `meta.mediaTime * 1_000_000`. That's
the media element's *media timeline*, not `performance.now()`. Subtracting
one from the other at decode time gave garbage (~58 000 ms). Fixed by
using `meta.presentationTime`, which shares the `performance.now()`
timebase.

## How to verify

```bash
cd gawk-app
npm install
npm run dev
```

Open `http://localhost:5173` in Chrome or Edge (Chromium 120+). Firefox
Developer Edition works too, degraded.

1. Click **Start Capture**. Pick "Entire screen" and (if multi-monitor)
   select the specific monitor you want.
2. Source and Decoded panels should show identical content at the same
   aspect ratio, decoded lagging by ≤100 ms visually.
3. Stats panel should show:
   - **Capture path**: `mstp` on Chrome, `video-rvfc` on Firefox
   - **Codec**: `avc1.42E02A` on Chrome, one of the fallbacks on Firefox
   - **Acceleration**: `hardware` on Chrome (target), likely `software` on Firefox
   - **Encoder fps** ≈ monitor refresh
   - **Encoder queue** mostly at 0
   - **Encode latency** < 2 ms on Chrome hardware; 5–15 ms on software
   - **End-to-end latency** < 50 ms typical on Chrome
4. Click **Stop**. Both previews clear. No console errors, no leaked
   `VideoFrame` warnings.

## Out of scope for v0.1 (deferred)

- WebTransport / relay server (build-order items #2–#4)
- Multi-datagram chunk framing
- Periodic forced keyframes for resilience
- Audio capture
- Auth / access control
- Adaptive bitrate / resolution
- Encoder-in-worker (currently on the main thread; measurable but not
  blocking at this scale)
- Broadcaster/viewer route split (will happen naturally in item #3)
- `MediaStreamTrackGenerator` for the decoded-preview path (Chrome-only
  optimization; canvas 2D is fine for now)

## What's next

Build-order item #2: WebTransport hello-world. TLS/cert plumbing for the
Go relay, then a trivial publisher-sends-datagrams, subscriber-prints-them
exchange. The encoder's `EncodedVideoChunk` output slot becomes the input
to a `WebTransport.datagrams.writable` writer; on the viewer side, a
reader pipes into the decoder chain. The rest of this codebase should not
need to change.
