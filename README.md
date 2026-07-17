# gawk

Self-hosted low-latency game streaming for a small private group (~15
friends) watching one broadcaster's gaming PC. Homelab-hosted, 1 Gbps
symmetric uplink, targeting sub-500 ms glass-to-glass latency.

The primary motivation is **learning**: this deliberately explores newer,
lower-level browser tech — WebTransport datagrams + WebCodecs — instead of
the mature WebRTC/SFU route (OvenMediaEngine et al.), which would have been
the safe choice.

## How it works

```
Broadcaster (Chrome)                relay (Go)                Viewers (Chrome, ≤15)
getDisplayMedia                                              
  → VideoEncoder (WebCodecs)      WebTransport/QUIC            reassemble frames
  → chunk into ≤1200 B datagrams ──→ /publish                    → VideoDecoder
     (custom wire format)             fan-out hub ──→ /subscribe ──→ canvas
```

- **Transport is QUIC datagrams**, not streams: a lost chunk means a dropped
  frame, never stalled playback.
- The relay is a **byte forwarder**. It parses headers only to observe: it
  caches the latest decoder config and last complete keyframe, and primes
  every newly-joined viewer with them — late joiners get a picture
  immediately instead of waiting for the next keyframe.
- One frozen **wire format** (20-byte VideoChunk header + DecoderConfig
  message, big-endian) implemented twice — Go (`gawk-server/wire`)
  and TypeScript (`gawk-app/src/transport/wire.ts`) — kept byte-compatible
  by shared golden test vectors.
- Codec is **negotiated**, not fixed: H.264 hardware realtime is the happy
  path, with VP9/VP8 fallbacks probed through a cascade of encoder configs.

## Repository layout

| Path | What |
|------|------|
| `ROADMAP.md` | High-level roadmap for post-v0.5 work (R1–R16), with ordering rationale and per-item scope sketches |
| `gawk-app/` | React SPA (Vite + TypeScript + Zustand). Production surfaces: `#/` (landing/join), `#/broadcast`, `#/view/<id>`; the stats-heavy diagnostics live frozen under `#/debug/*` (`broadcast`/`view`/`loopback`). `deploy/`: Dockerfile + Helm chart |
| `docs/` | Per-milestone design notes and gotchas (`01`–`21`), plus [`implementation-tasks.md`](docs/implementation-tasks.md) — the server design + task breakdown and current progress |
| `gawk-server/` | Go relay: WebTransport endpoint, pub/sub hub, dev-cert tooling. `wire/` is public so the native broadcaster can import it. `deploy/`: Dockerfile + Helm chart. See its [README](gawk-server/README.md) |
| `gawk-broadcast/` | Go native Linux broadcaster (R14): Gio GUI + CLI over a shared engine, hardware encode via portal + GStreamer. Own module, own release; no image or chart — a binary you run on your own PC. See its [README](gawk-broadcast/README.md) |
| `BUGS.md` | Known, confirmed, not-yet-fixed bugs (how found, impact, where a fix starts) |
| `.github/workflows/` | CI (test/lint/build on PR + main) and release automation (release-please → GHCR images + OCI Helm charts, versions from conventional commits) |

## Quickstart (local dev)

Prerequisites: Go ≥ 1.23 (the toolchain auto-downloads what the module
needs), Node ≥ 22, Chromium-based browser.

```sh
# 1. Relay with an ephemeral dev certificate
cd gawk-server
go run ./cmd/gawk-server -dev-cert
#    → copy cert_hash_hex from the startup log

# 2. Frontend
cd gawk-app
npm install
npm run dev            # http://localhost:5173
```

Open `http://localhost:5173/#/broadcast`, open the **⚙ settings** panel,
paste the cert hash into the "Dev cert hash" field, **Start a stream**, pick a
screen — you get a 6-char code. The cert hash persists to `localStorage`, so
in another tab just open the join link (or type the code on the landing page
`#/`) and it connects. No Chrome flags needed — the app passes the hash via
`serverCertificateHashes`. (The old stats-heavy pages remain at
`#/debug/broadcast` and `#/debug/view` for diagnostics.)

To probe a running server without a browser:

```sh
cd gawk-server
go run ./cmd/gawk-echo -cert-hash <cert_hash_hex>   # datagram RTT test
```

### Tests

```sh
cd gawk-server && go vet ./... && CGO_ENABLED=1 go test -race ./...
cd gawk-app    && npm test && npm run lint && npm run build
```

### Containers & deployment

Both components build to images (`<component>/deploy/Dockerfile`) and ship
as Helm charts (`<component>/deploy/charts/<component>/`) published to GHCR by CI on
release — chart version, `appVersion` and image tag always match. Homelab
deployment is automated cluster-side: whenever a new version is released,
it is deployed to the homelab automatically (CI itself stays publish-only —
no cluster credentials in GitHub). Initial install runbook, GHCR pull-secret
setup and the release flow:
[`docs/05-resilience-deploy.md`](docs/05-resilience-deploy.md).

Frontend deploy-time flags are set as gawk-app chart values under `config.*`
(rendered into a `/config.js` the SPA reads at load — no rebuild, no server
endpoint). Notably, set `config.requirePublishSecret=true` when the relay runs
with `-publish-secret`, so the broadcaster is prompted for the secret on
"Start a stream". See [`docs/10`](docs/10-production-ui.md) Decision 11.

```sh
cd gawk-server && docker build -f deploy/Dockerfile -t gawk-server:dev .
cd gawk-app    && docker build -f deploy/Dockerfile -t gawk-app:dev .
```

## Status

Milestones (detail in [`docs/implementation-tasks.md`](docs/implementation-tasks.md)):

1. ✅ Local loopback (capture → encode → decode in one tab) — `docs/01`
2. ✅ WebTransport hello-world: TLS, dev certs, echo — `docs/02`
3. ✅ Single-client end-to-end: hub, publish/subscribe, frontend transport — `docs/03`
4. ✅ Fan-out hardening: multi-subscriber, restart-safe caches, `/statusz` — `docs/04`
5. ✅ Resilience + deployment: keepalive, viewer auto-reconnect, Docker, Helm charts, release-please CI — `docs/05`
6. ✅ Multi-broadcaster support: server registry, path-based routes, client uni-stream ID announcements, reclaim UI, and ended states — `docs/06` (completed 2026-07-12)
7. ✅ Hardening (R2): broadcast/subscriber/bandwidth limits, publish secret, connection rate limiting, defensive parsing, obfuscated `/statusz` IDs — `docs/07` (completed 2026-07-13)
8. ✅ Broadcaster resolution & framerate picker (R3): pre-encode scaling + fps gating ladder, ladder-scaled bitrate, time-based keyframe cadence, live mid-stream changes — `docs/08` (completed 2026-07-13)
9. ✅ Automatic resolution fallback (R4): encode-queue rejection-ratio detection with hysteresis + cooldown, a default "auto" selection that steps down and back up (backoff against oscillation) while explicit rungs are never auto-stepped, encoder-error step-down — `docs/09` (implemented + released 2026-07-13; real-hardware check found the queue signal under-fires on hardware encoders; software-path verify passed 2026-07-14)
10. ✅ Production UI (R6): landing/broadcaster/viewer surfaces, a monochrome design system (`styles/global.css` tokens + `src/ui/` primitives), segmented join-code entry, and a cinematic fullscreen viewer with a stats overlay (`Ctrl+Alt+Shift+D` or right-click); the old pages are re-homed frozen under `#/debug/*`. UI-only — zero server/wire/pipeline changes — `docs/10` (implemented 2026-07-13; manual browser verify passed 2026-07-14)
11. ✅ Heavy-motion resilience: 500 ms GOP (`keyframeIntervalMs` 2000→500), viewer freeze-on-gap (hold the last good frame on a frameId discontinuity instead of decoding corrupt deltas), and a 30 fps default fan-out cap (broadcaster can still pick 60/native) — cuts heavy-motion corruption on Chrome and "Awaiting keyframe" stalls on Firefox. UI/pipeline only — zero server/wire changes — `docs/08` + `docs/03` (implemented 2026-07-14; manual browser verify passed 2026-07-14)
12. ✅ Worker offloading & reliable keyframes (R8): keyframes now travel over per-subscriber reliable WebTransport uni streams with server **store-and-forward** fan-out (write deadline, supersede-stale, late-joiner priming), and the viewer merges them with datagram deltas in a pure bounded reorder buffer with centralized freeze-on-gap; deltas stay on datagrams. The whole viewer pipeline (decode + `OffscreenCanvas` render + reconnect) now runs in a **Web Worker** — frames never cross to the main thread — with a capability handshake and a main-thread fallback. Wire/server/broadcaster/viewer change together — `docs/12` (S1–S7 implemented 2026-07-14; automated gates green, browser-verified 2026-07-14)
13. ✅ Observability & metrics (R9): the relay grew a plain-TCP ops endpoint (`-metrics-addr`, default `:2112`) serving Prometheus `/metrics`, `/healthz` and a curl-able `/statusz` mirror (the HTTP/3 server itself is unscrapeable), with per-broadcast + lifetime metric families, an ingress-loss window that attributes broadcaster→relay loss, split drop reasons, connection-outcome counters, per-subscriber `/statusz` detail, and a Helm metrics Service + gated ServiceMonitor (never on the public LoadBalancer). Both production surfaces now have a sectioned stats overlay (same hotkey) with pipeline funnel rates, `WebTransport.getStats()` connection health, and a **Copy diagnostics** JSON export; a symptom→signature bottleneck playbook lives in the doc — `docs/13` (M1–M7 implemented + manually verified 2026-07-14; M8 Grafana dashboard deferred)
14. ✅ Viewer render performance (R10): rAF-coalesced latest-frame-wins rendering + a WebGL render sink behind the R8 `RenderSink` seam, a nested transport-worker split so decode/render pressure can't starve the datagram queue, decoder queue 5→10; field findings added a 1 s keyframe reorder wait and relay eviction of zombie subscribers (close code 4001) — `docs/14` (P1–P3 + fixes implemented 2026-07-14; re-verified on Chrome + Firefox 2026-07-14; P4 remainder deferred)
15. ✅ Viewer live-edge (R5, re-scoped): a **live-edge drift** metric (windowed-min baseline over capture timestamps, zero protocol change), **absolute capture→render latency** via new `TimeSync` (0x05) / `ClockMapping` (0x06) wire messages with the relay's monotonic clock as the common reference (plus a self-owned per-leg RTT that doesn't need `getStats()`), and an **opt-in smoothed playout** mode (reorder-release pacing, +150 ms, default off, right-click toggle). The keyframe-request back-channel was rejected for good — `docs/15` (Q1–Q3 implemented 2026-07-14; Q4 measurement pass + manual browser verify passed 2026-07-15, all knobs kept — measured glass-to-glass ~50 ms with hardware encode+decode, meeting the sub-500 ms target; up to ~2500 ms on software codec paths)
16. 🚧 Broadcaster worker offload (R11): the broadcast pipeline (MSTP capture pump, scaling/gating, encode, packetize, WebTransport send) runs in a Web Worker fed by a transferred `MediaStreamTrack` clone; `getDisplayMedia` + preview stay on main, connect-before-picker ordering and `BroadcastStartError.phase` semantics preserved, capability probe + main-thread fallback (Firefox), overlay "Pipeline" row shows placement. UI/pipeline only — zero server/wire changes — `docs/16` (implemented 2026-07-14; automated gates green, manual browser verify pending)
17. 🚧 Viewer playback smoothing (R12): jitter measurement at the actual paint (presentation-cadence error, arrival p95−min, decode σ), a separate opt-in **"Paced playback (adaptive)"** mode (`PacedPresentationSink` holding ≤3 decoded frames to vsync-aligned targets, subsuming the R10 coalescing sink) with a jitter-tracked adaptive playout offset (clamp p95−min+34 ms to [50, 350]), and an experimental opportunistic frame-interpolation scaffold (WebGL2 blend, own default-off toggle). Viewer-only — zero server/wire changes — `docs/17` (T1–T4 implemented 2026-07-15; automated gates green, manual browser verify pending; T5 motion-estimated interpolation + T6 findings not started)
18. 🚧 Advanced broadcaster settings (R13): `isConfigSupported` probe matrix, HW-aware auto ceiling + probe-driven 'auto' framerate default (60 when hardware probes it, else 30), acceleration tri-state, bitrate/codec overrides, probe-annotated pickers, and capture aligned to the sticky selection via live `applyConstraints` — **no settings change ever restarts the stream**. Supersedes R7; UI/pipeline only — zero server/wire changes — `docs/18` (L1–L5 implemented 2026-07-15; automated gates green, manual browser verify pending)
19. 🚧 Native Linux broadcaster (R14): a Gio GUI app + CLI over a shared Go engine in the new top-level [`gawk-broadcast/`](gawk-broadcast/) module (importing the relay's now-public `gawk-server/wire` — reused, never mirrored), publishing with **hardware encode** from Linux, which the browser structurally cannot do there. Go-owned XDG ScreenCast portal handshake (the share picker appears on every start — the choice is never persisted) feeding a GStreamer subprocess; hardware-only cascade `vulkanh264enc` → `nvh264enc` → `vah264enc` → **refusal pointing at the browser** (no software rung), each accepted only by real trial encode; MPEG-TS over the pipe for structural AU boundaries; raw Annex-B with empty extradata (the only thing exercising the viewer's Annex-B branch). Zero server/wire/viewer changes — `docs/19` (V0–V7 implemented 2026-07-15; automated gates green including end-to-end tests against the real relay binary; **manual verification on the Linux gaming PC pending** — hardware encode, the latency-bias gate, portal + notifications on KDE/GNOME. V8 direct Vulkan Video encode is gated on V2's on-hardware Stage-1 result and not started)
20. 📋 System audio (R15, experimental): Opus via WebCodecs over datagrams — one ~320 B Opus packet per datagram (48 kHz stereo, 128 kbps, 20 ms; no chunking/keyframes), new wire types 0x07/0x08 + a hub audio-config cache, viewer-worker decode feeding a main-thread `AudioWorklet` ring buffer, and good-enough A/V sync off the shared capture clock (adaptive audio jitter buffer; audio-master video pacing in the R12 paced modes). Default-off "Enable audio (experimental)" broadcaster toggle; viewer audio controls appear only when audio is received — `docs/20` (designed 2026-07-15, not started)
21. 📋 iOS native fullscreen (R16): the viewer's fullscreen button is a silent no-op on iPhone (no Element Fullscreen API there — every iOS browser is WebKit; the only native fullscreen is `webkitEnterFullscreen()` on a `<video>`, and the viewer paints a canvas). Fix: a `TeeRenderSink` decorator wraps each **presented** canvas frame (R12 pacing/interpolation preserved) into a worker-side `VideoTrackGenerator` feeding a hidden pre-armed `<video>`; tiered `useFullscreen` (element → video → CSS pseudo-fullscreen) so the button always does something; plus a new stats-overlay **Feature Gates** section (UpperCamelCase names, first gate `NativeVideoFullscreen`). Gated on the *absence* of `Element.requestFullscreen` — non-iPhone devices unchanged (overlay section aside). Viewer-only — zero server/wire changes — `docs/21` (designed 2026-07-16, not started)

What comes next (the R11/R12/R13 manual browser verifies and R14's manual
verify on the gaming PC, then R14's V8 direct Vulkan Video encode if that
verify says the hardware is there, plus the R15 system audio and R16 iOS
native fullscreen builds) is laid out in [`ROADMAP.md`](ROADMAP.md).

## Important gotchas

Hard-won; each cost real debugging time. Details live in the linked docs.

**Environment**

- **WSL2: Chrome must run *inside* WSL2** alongside the server. Windows
  Chrome cannot complete a QUIC handshake to a server in WSL2 NAT mode
  (localhost forwarding is TCP-only; via the NAT IP Chrome idle-times-out
  even though CLI clients work). ([docs/02](docs/02-webtransport-hello.md))
- **`go test -race` needs `CGO_ENABLED=1`** — this environment defaults to 0.
- **`npx tsc --noEmit` in `gawk-app` passes vacuously** — the root
  `tsconfig.json` is solution-style (references only), so it checks nothing.
  `npm run build` (`tsc -b`) is the real typecheck; vitest strips types and
  won't catch type errors either. ([CODE-REVIEW.md](CODE-REVIEW.md))
- **npm may silently skip native bindings** (rolldown/vite 8, oxlint) due to
  [npm/cli#4828](https://github.com/npm/cli/issues/4828); build/lint/test
  then die with "Cannot find native binding" and the documented
  delete-and-reinstall remedy does not help. Workaround:
  `npm install @rolldown/binding-linux-x64-gnu @oxlint/binding-linux-x64-gnu --no-save`.
  ([docs/03](docs/03-single-client-e2e.md))
- **UDP buffers**: quic-go wants ~7 MiB receive buffers; Linux defaults to
  208 KiB. `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`
  for real streaming loads.
- **The production viewer (`#/view/<id>`) has no cert-hash field** — it is
  deliberately chrome-free (fullscreen + leave only). For `-dev-cert` local
  testing set the hash once in the broadcaster's ⚙ panel; it persists to
  `localStorage` (shared same-origin, so the viewer picks it up). Or use
  `#/debug/view`, which keeps the full settings form. ([docs/10](docs/10-production-ui.md))
- **The debug viewer uses `#/debug/view/<id>`, not `#/view/<id>`** — R6 gave
  `#/view/<id>` to the production viewer, and `ViewPage` syncs its code into the
  hash, so it must stay in its own namespace or clicking **Watch** bounces you
  into the production UI. Any "frozen" page that writes `location.hash` is not
  actually route-frozen. ([docs/10](docs/10-production-ui.md))
- **A codec the viewer's browser can't decode is a *terminal* error, not a
  reconnect** — an unsupported codec (e.g. Chrome can't decode a Firefox
  broadcaster's `avc1.640034` High-5.2) fails every attempt identically, so the
  viewer shows a "can't play this stream" card and stops instead of looping the
  auto-reconnect. Decoder/codec failures are tagged `fatal` in
  `ViewerPipeline`; `ViewerSession` reconnects only on transport drops. (H.264
  `description` handling also matters: an Annex-B extradata blob must *not* be
  passed as the AVCC `description` — `viewer.ts` sniffs `avcC`'s `0x01` prefix.)

**Certificates (Chromium `serverCertificateHashes` rules)**

- Dev cert must be **ECDSA P-256** and its **total validity span ≤ 14 days,
  clock-skew backdate included** — a 14 d + 1 h cert is rejected with an
  opaque `CERTIFICATE_VERIFY_FAILED (certificate unknown)`. The Go probe
  won't catch this; only Chrome enforces it. ([docs/02](docs/02-webtransport-hello.md))
- The dev cert is **regenerated on every server start** — re-paste the fresh
  `cert_hash_hex` after each restart; a stale hash fails identically. This
  also means the viewer's **auto-reconnect can never heal a relay restart in
  `-dev-cert` mode** — test that with a file-based dev cert
  (`gawk-devcert -out certs`). ([docs/05](docs/05-resilience-deploy.md))

**webtransport-go (v0.11.x) — all fail at runtime, not compile time**

- ALPN must be added manually: wrap your TLS config in
  `http3.ConfigureTLSConfig(...)`, the library won't.
- WebTransport SETTINGS must be enabled manually:
  `webtransport.ConfigureHTTP3Server(s.H3)`, or clients reject with
  "server didn't enable WebTransport".
- Go *clients* need `EnableStreamResetPartialDelivery: true` in their
  `quic.Config`. ([docs/02](docs/02-webtransport-hello.md))
- **Session close code hiding on datagram reads**: In both Go and JS, when a session is closed with a custom error code, reading from the datagram queue/channel returns only a generic `EOF` or channel-closed status. To retrieve the actual close code (e.g. `4000`), the client must listen to `wt.closed` (JS) or block on `AcceptStream` / `AcceptUniStream` (Go). ([docs/06](docs/06-multi-broadcaster.md))
- **…and `wt.closed` can lose the settle-order race**: on a server close, the
  datagram read loop and the `wt.closed` promise settle in unspecified,
  browser-dependent order. If close semantics (like 4000 = "broadcast ended,
  don't reconnect") ride on `wt.closed` alone, a read-loop-first settle
  silently degrades them to an anonymous drop. On read-loop termination,
  race `wt.closed` with a short timeout before deciding
  (`viewer.ts handleReadLoopEnd`). ([docs/06](docs/06-multi-broadcaster.md))

**Observability (R9)**

- **`GAWK_METRICS_ADDR` cannot be disabled with an empty value** — the env
  helper treats empty as unset and falls back to the default `:2112`. The
  literal `off` disables; the Helm chart passes it when
  `metrics.enabled=false`. ([docs/13](docs/13-observability.md))
- **client_golang rejects one metric family with two label sets** — you
  can't emit `gawk_x_total{broadcast=…}` and an unlabeled `gawk_x_total`
  from the same registry (gather fails with "inconsistent label
  dimensions"). Hence the `gawk_broadcast_*` / `gawk_relay_*` split.
  ([docs/13](docs/13-observability.md))
- **`WebTransport.getStats()` currently exists in NO shipping browser** —
  Chromium removed its pre-spec implementation (absent in 150/151/152;
  spec-conformant rewrite "in development",
  [chromestatus](https://chromestatus.com/feature/5194440034746368)) and
  Firefox never had it, so the overlays' `connection.*` rows read `—`
  everywhere and per-leg attribution leans on relay-side counters. Every
  consumer must feature-detect and treat all fields as nullable
  (`transport/net-stats.ts` — spec-aligned, lights up on re-ship). The
  self-owned rows are immune: `RTT (time-sync)` and, since 2026-07-15,
  "Video bitrate (recv)"/"(sent)" — clients count their own video bytes
  (`videoBytesReceived`/`bytesSent`) instead of asking the transport.
  ([docs/13](docs/13-observability.md))

**Media pipeline**

- **Trust the actual `VideoFrame`, never `MediaStreamTrack.getSettings()`** —
  Chrome has been observed reporting one resolution while delivering frames
  at another. Configure encoders from the first real frame.
  ([docs/01](docs/01-loopback-test.md))
- H.264 needs **even dimensions**; `getDisplayMedia` can hand out odd sizes
  on some DPI combos — round down to even before configuring the encoder.
- Constrain only `width` in `getDisplayMedia`; constraining both dimensions
  makes Chrome pillarbox ultrawide sources.
- WebCodecs: the **first chunk after `VideoDecoder.configure()` must be a
  keyframe**, and decoder configure/decode calls must be strictly ordered
  (promise-chain them).
- **A lost frame corrupts everything until the next keyframe — so the viewer
  freezes on a frameId gap.** Inter-frame coding means a delta whose reference
  was dropped decodes to visible garbage. Since R8 this freeze-on-gap policy
  lives in **one place** — the pure `transport/reorder-buffer.ts`, which merges
  reliable stream keyframes with datagram deltas by frameId and, on a
  non-contiguous *delta*, holds the last good frame (waits for the next
  keyframe) instead of decoding corruption. Consequences: the **500 ms GOP**
  (`keyframeIntervalMs`, cut from 2000) is what keeps that freeze short, and gap
  discards surface on the **"Awaiting keyframe"** / gap-resync stats — so those
  counters rising on Chrome during heavy motion is the fix working, not a
  decoder falling behind.
  ([docs/03](docs/03-single-client-e2e.md), [docs/12](docs/12-worker-and-reliable-keyframes.md))
- **Keyframes arrive hundreds of ms behind their own deltas — the reorder
  wait must cover *transfer* latency, not just reordering jitter (R10).**
  A ~236 KB keyframe is store-and-forwarded as one stream while its trailing
  deltas arrive instantly as datagrams; to a congested peer it was measured
  landing > 500 ms late. With `KEYFRAME_WAIT_MS` at 200 ms the held deltas
  expired first, so every GOP became keyframe-only ~2 fps playback — the
  telltale signature is **one gap resync per keyframe with zero reassembler
  drops**. The wait is now 1000 ms (bounded by `MAX_BUFFERED_FRAMES`).
  Relatedly, a subscriber whose keyframe stream *opens* fail persistently
  (exhausted stream credit — a zombie session that stopped reading streams)
  is now **evicted** after 10 consecutive failures with non-terminal close
  code 4001, so it reconnects if live and stops costing fan-out if dead.
  ([docs/14](docs/14-viewer-render-performance.md))
- **frameIds are uint32 serial numbers — every comparison must be wrap- and
  restart-aware, and moving keyframes off datagrams (R8) silently broke the
  restart path (R10).** The reassembler drops "late" deltas against a
  watermark that only keyframes could reset — but since R8 keyframes bypass
  it on streams, so a broadcaster restart (frameIds reset to 0, viewers stay
  connected) dropped *every* new-session delta as late for ~36 min: a 2 fps
  keyframe slideshow. Signature: "Dropped (late)" climbing at full frame
  rate, `keyframeWaitDrops` flat. Now: stream keyframes sync the watermark
  (`noteStreamKeyframe`), frameId comparisons use serial arithmetic
  (`wire.frameIdAhead`, handles uint32 rollover), and a serially-backwards
  keyframe triggers an immediate reorder resync (a grace wait there costs an
  extra GOP of freeze). ([docs/14](docs/14-viewer-render-performance.md))
- **Keyframes go over a reliable uni stream; deltas stay on datagrams (R8).**
  A keyframe split into ~1200-byte datagrams is ruined by a single lost packet,
  and heavy-motion keyframes are large — so the broadcaster sends each keyframe
  (config prepended) over `createUnidirectionalStream()` and the relay
  **stores-and-forwards** it: it reads the whole keyframe into a bounded buffer
  (capped by `MaxKeyframeBytes`) *before* fanning out one uni stream per
  subscriber, which is what structurally decouples the publisher from any slow
  subscriber. A subscriber that stalls past `KeyframeWriteTimeout` is
  `CancelWrite`-ed and superseded by the newest keyframe (**≤1 in-flight per
  subscriber**) — "drops over stalls" at stream granularity. Only keyframes are
  reliable; making deltas reliable would reintroduce head-of-line blocking.
  ([docs/12](docs/12-worker-and-reliable-keyframes.md))
- **`transferControlToOffscreen()` is one-shot and irreversible — so the viewer
  worker must outlive React's StrictMode remount (R8).** The production viewer
  runs its pipeline in a Web Worker and hands it the `<canvas>` via
  `transferControlToOffscreen()`; a second call on the same element throws, and
  a transferred canvas can't be reclaimed for a main-thread 2D context. So the
  worker + transfer are created **once for the life of the view** (reconnects
  happen *inside* the worker, reusing `ViewerSession`), and teardown is
  **deferred a macrotask** so StrictMode's synchronous unmount→remount reuses
  the same worker instead of trying to transfer twice. Worker support is probed
  with a **boot handshake before the transfer**, so an unsupported worker
  (e.g. no worker WebCodecs) falls back to the main-thread pipeline with the
  canvas still attached. ([docs/12](docs/12-worker-and-reliable-keyframes.md))
- **Firefox's 2D-canvas `drawImage(VideoFrame)` is a synchronous CPU
  conversion — and on a shared viewer worker thread it starves everything
  else (R10).** When one thread runs the datagram reader, decoder feeding
  *and* rendering, a slow per-frame draw overflows the browser's small
  incoming-datagram queue (silent drops → "Dropped (incomplete)") and backs
  up decode (decoded fps < received fps). Hence two structural fixes: the
  worker renders through `createRenderSink()` — a **WebGL** textured-quad
  sink (2D fallback) wrapped in a **coalescing** sink that draws at most
  once per rAF tick, latest-frame-wins — and the WebTransport read loops run
  in a **nested transport worker** (`transport.worker.ts`, one per pipeline
  attempt behind the `ViewerTransport` seam) posting transferable buffers,
  so decode/render pressure can't reach the datagram queue at all.
  Consequence: "Rendered fps" reads ≈min(decoded fps, display Hz) — rendered
  *below* decoded under load is coalescing working, not frame loss. Every
  fallback here degrades silently, so the overlay shows the actual placement:
  "Renderer" / "Pipeline" / "Transport" read **WebGL / Worker / Worker** on
  the fast path (— / Main thread / In-process on the no-worker fallback).
  ([docs/14](docs/14-viewer-render-performance.md))
- **Transferring a `MediaStreamTrack` detaches it — the broadcast worker gets
  a *clone*, the original stays for the preview (R11).** The broadcaster
  pipeline runs in a Web Worker fed by a transferred `track.clone()` (MSTP is
  created *in the worker* — transferring `processor.readable` instead would
  keep piping every frame through the main-thread realm, defeating the
  split). `getDisplayMedia` itself must stay on main (window scope + user
  gesture), and transferability is probed **before** committing to the worker
  path by postMessage-ing a dummy `canvas.captureStream()` track —
  non-transferable types throw `DataCloneError` *synchronously on the
  sender*, no roundtrip needed. Firefox lands on the untouched main-thread
  pipeline via the same boot-handshake fallback as the viewer; the overlay's
  broadcaster "Pipeline" row shows which path is live.
  ([docs/16](docs/16-broadcaster-worker-offload.md))
- **`isConfigSupported({hardwareAcceleration: 'prefer-hardware'})` answering
  `supported: true` is Chromium's hardware commitment** — the spec has no
  "require hardware", but Chromium returns false when it can't do HW, which
  is what makes the R13 probe matrix (and its "hardware only" mode)
  meaningful. The probe stays *advisory*: the live `configure()` result wins,
  and the overlay's "Encode mode" row shows the runtime truth — picker
  badges are predictions, not guarantees.
  ([docs/18](docs/18-advanced-broadcaster-settings.md))
- **`isConfigSupported` is not free on Chrome — never fire probes
  unbounded.** Every *pending* call holds a real encoder instance (software
  probes at 4K allocate full encoder contexts), so requesting the
  broadcaster surface's ~170 matrix combos in parallel OOM-crashed the tab.
  `EncoderSupportProber` gates all probes through a fixed slot pool
  (`MAX_CONCURRENT_PROBES`, 4) and the per-codec matrices only probe once
  the settings panel opens — route any new probe callers through the
  prober, never `VideoEncoder.isConfigSupported` directly in a loop.
  ([docs/18](docs/18-advanced-broadcaster-settings.md))
- **Track-clone constraints are per-track and must be applied worker-side
  (R13).** Capture alignment (`track.applyConstraints` on the sticky
  resolution/fps target) lands on whichever track feeds MSTP: the
  transferred clone inside the broadcast worker, or the original on the
  main-thread path. Constraining the main-thread original does nothing for
  a worker session — clones hold independent constraints (that's also why
  the preview stays native). And the probe matrix refines from real frame
  dims **upward only**: re-probing at our own constrained dims would feed
  the ceiling its own output (constrain → smaller frames → lower ceiling →
  constrain…). ([docs/18](docs/18-advanced-broadcaster-settings.md))
- **iPhone has no Element Fullscreen API** — `Element.requestFullscreen`
  ships on iPadOS (16.4+) but not iPhone, in every iOS browser (all WebKit),
  so an optional-chained call is a silent no-op there. The only native
  fullscreen is `HTMLVideoElement.webkitEnterFullscreen()`: it needs a user
  gesture and a video with loaded media (hence R16's pre-armed hidden
  `<video>` fed by the worker-side canvas tee), it does **not** fire
  `fullscreenchange` (track `webkitbeginfullscreen`/`webkitendfullscreen`
  instead), and **`display: none` on the video breaks it** — hide by
  size/position. ([docs/21](docs/21-ios-video-fullscreen.md))
- **~1200-byte safe datagram payload** drives the chunking design — don't
  assume larger datagrams survive the path.
- **Raising the QUIC idle timeout does not keep idle viewers alive** — the
  effective timeout is the min of both endpoints' advertised values and
  browsers advertise ~30s. The server-side keepalive (`-keepalive-period`)
  is the mechanism. ([docs/05](docs/05-resilience-deploy.md))
- **Hardware encoders don't surface backpressure via `encodeQueueSize`** —
  they drain frames without the queue growing past R4's `> 2` threshold, so
  the encode-queue rejection signal (auto-fallback's sole trigger) under-fires
  on the hardware path the gaming PC uses. Low fps there is usually
  source-limited, not encoder strain (encoder queue stays 0–2, `Dropped
  (source)` flat). Reproduce real backpressure with DevTools CPU throttling,
  which forces the software encode path.
  ([docs/09](docs/09-automatic-fallback.md))

**Native Linux broadcaster (R14)**

- **The browser cannot hardware-encode on Linux — don't go flag-hunting.**
  WebCodecs `VideoEncoder` HW encode ships on Windows/macOS/Android only
  (Linux gets HW *decode*), Chromium's own VA-API doc disclaims Linux, and
  on NVIDIA it's impossible in principle: Chromium's Linux encode path is
  VA-API only and `nvidia-vaapi-driver` is decode-only by design. That gap
  is the entire reason `gawk-broadcast/` exists.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`gawk-broadcast` is the *only* Annex-B publisher — and the only thing
  exercising the viewer's Annex-B branch.** It emits raw Annex-B with
  **empty DecoderConfig extradata** and builds no avcC record; the viewer's
  `isAnnexB` start-code sniff (`viewer.ts`) routes it into the branch that
  ignores extradata. The browser broadcaster always sends AVCC, so a
  regression there breaks native broadcasts while browser ones stay green.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`h264parse config-interval=-1` is load-bearing, not cosmetic.** Empty
  extradata means a late joiner primed with the relay's cached keyframe can
  only decode if SPS/PPS travel *inside* the keyframe AU. Drop the flag and
  late joiners see nothing while everyone already watching is fine.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **Don't gate the native broadcaster on Wayland** — the XDG ScreenCast
  portal works on X11 GNOME sessions too. Gate on the portal call
  succeeding, never on the session type.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`pipewiregrab` is NOT in mainline FFmpeg** — it's an unmerged patchset
  carried downstream by Jami; mainline ffmpeg has no PipeWire input at all.
  This is why capture is a GStreamer subprocess. Don't re-propose it
  without verifying it actually merged.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **Notifications must be critical urgency or the broadcaster never sees
  them** — KDE's portal inhibits normal notifications *while screen
  casting*, so the act of broadcasting suppresses exactly the notifications
  a fullscreen broadcaster needs. Failures use critical; going live doesn't.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **No viewer count, in any broadcaster** — nothing on the wire tells a
  publisher about subscribers, so the native app can't show one either.
  That's parity with the browser, not an omission.
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`pipewiresrc` can die during preroll with `stream error: unhandled
  format` — that's capture, not the encoder.** The compositor's chosen
  screencast format sometimes can't be mapped onto the downstream caps
  (DMA-BUF modifier/DRM-caps skew, 10-bit HDR desktops). The live start
  walks a three-rung capture ladder per encoder (rate-capped zero-copy —
  `max-framerate` asked of the compositor — then free negotiation, then
  system-memory pinned `video/x-raw`) before advancing the cascade, and an
  all-pipewiresrc failure reports as `ErrCaptureFormat`, never "no hardware
  encoder". Diagnose with `GST_DEBUG=pipewire*:5` (the child inherits env).
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`gawk-broadcast` is its own Go module and its CI job needs cgo + Gio
  headers** (`libwayland-dev`, `libvulkan-dev`, …) and `CGO_ENABLED=1` —
  with cgo off, the GUI fails as "build constraints exclude all Go files in
  `gioui.org/internal/vk`", which says nothing about cgo. The engine and CLI
  need no headers: `go test ./internal/...` works bare. The relay's CI job
  must stay header-free (Decision 1).
  ([docs/19](docs/19-linux-native-broadcaster.md))
- **`mpegts.AU.Data` aliases the demuxer's buffer — clone before it outlives
  the callback.** A frame handed to the channel un-cloned is rewritten by
  the AUs demuxed behind it: in the field this meant clean debug dumps (both
  taps read the bytes while still valid) but a black viewer — the SPS parse
  ran on recycled bytes, so no DecoderConfig was ever derived, and an
  unconfigured `VideoDecoder` swallows chunks with zero errors. Motion
  scrambled queued frames to reference soup; a static screen (drained
  channel) stayed sharp. `-race` catches it outright.
  ([docs/19](docs/19-linux-native-broadcaster.md))

**CI / deployment**

- **Tags created with `GITHUB_TOKEN` don't trigger workflows** — publish
  jobs must chain off release-please outputs in the same workflow, never
  `on: push: tags`. ([docs/05](docs/05-resilience-deploy.md))
- **release-please separate PRs self-conflict in a monorepo** (all release
  PRs edit the shared manifest, and the bot doesn't refresh the stranded
  one) — use one combined release PR (`"separate-pull-requests": false`);
  tags/releases/publishes stay per-component.
  ([docs/05](docs/05-resilience-deploy.md))
- **GHCR**: refs are lowercase-only (`tuhis`), and cluster pulls need a
  **classic** PAT with `read:packages` (fine-grained PATs don't cover GHCR).
- **The relay is HTTP/3-only (no TCP listener)** — kubelet probes must exec
  the bundled `gawk-echo` binary; `httpGet`/`tcpSocket` can never succeed.
- **That exec probe crash-loops the pod once `allowedOrigins` is set** —
  `gawk-echo` sends no `Origin` header, so `CheckOrigin` rejected its own
  probe as soon as production configured a real origin (dev's empty
  allowlist never hit it). `CheckOrigin` now exempts loopback `RemoteAddr`.
  ([docs/05](docs/05-resilience-deploy.md))
- **release-please `extra-files` paths are package-relative** — a repo-
  relative path silently leaves Chart.yaml unbumped.
