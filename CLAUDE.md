# Project: Self-Hosted Low-Latency Game Stream

## Purpose & Context
Hobby self-hosted game-streaming web app for a small private group (~15 friends)
watching a single broadcaster (my gaming PC). Hosted on a homelab with a 1gbps
symmetric connection. Target: sub-500ms glass-to-glass latency.

Primary motivation is **learning** — exploring newer, lower-level browser tech.
Success = a working low-latency stream *and* a rewarding technical exploration.
This is why WebTransport + WebCodecs was chosen over a mature WebRTC/SFU path
(e.g. OvenMediaEngine) — that route was lower-risk but less interesting.

## Architecture (locked in)
- **Frontend**: React SPA (Vite + TypeScript + Zustand + CSS Modules)
- **Capture**: `getDisplayMedia` — whole-screen preferred (exclusive-fullscreen
  game compatibility). Frame delivery uses `MediaStreamTrackProcessor` on
  Chromium; falls back to a hidden `<video>` + `requestVideoFrameCallback` on
  Firefox (which lacks MSTP).
- **Encode/decode**: WebCodecs `VideoEncoder` / `VideoDecoder`. Codec is
  **negotiated**, not fixed: preference list `avc1.42E02A` → `avc1.640028` →
  `avc1.42E01F` → `vp09.00.40.08` → `vp09.00.31.08` → `vp8`, each probed with a
  cascade of hardware/latency config variants. H.264 hardware `realtime` mode
  is the happy path (Chromium broadcaster on the gaming PC); the fallbacks
  exist because Firefox's `VideoEncoder` support is much narrower.
- **Transport**: WebTransport datagrams
- **Relay server**: Custom Go server using `github.com/quic-go/webtransport-go`.
  Pub/sub hub — one publisher fans out encoded video datagrams to up to 15
  subscriber sessions.

## Relay server behavior (implemented — chunks B2/B3 + C1–C3 + D1 + R1 + R2)
- Caches the last keyframe + decoder config to prime newly-joined viewers
  (`gawk-server/internal/hub`); a **new publisher session invalidates both
  caches** (frameIDs reset, config may differ) while caches persist when the
  broadcaster is merely away (GC grace period defaults to 5 minutes)
- Drops chunks for slow subscribers rather than blocking others (per-subscriber
  bounded queue, non-blocking enqueue); cached config re-emitted before every
  keyframe's chunk 0
- Routes: `CONNECT /publish` (mint ID), `CONNECT /publish/{id}` (reclaim ID),
  `CONNECT /subscribe/{id}` (subscribe to ID), `/echo` diagnostic, `GET /healthz`,
  `GET /statusz` (JSON RegistryStats snapshot)
- **QUIC keepalive keeps idle viewers connected while the broadcaster is
  away** (`-keepalive-period`, default 10s; `-max-idle-timeout`, default
  30s). Raising the idle timeout alone is a no-op — the effective timeout is
  the min of both endpoints and browsers advertise ~30s; the keepalive is
  the mechanism.
- Viewer side: **auto-reconnect with backoff** (`ViewerSession` wrapping the
  single-shot `ViewerPipeline`; 1s→15s capped, 10 attempts). Never-connected
  failures are fatal by design — `WebTransportError` hides the HTTP status,
  so 429/bad-cert/wrong-URL are indistinguishable in JS. Close code **4000
  (broadcast ended) is terminal** — no reconnect; it is sent on GC expiry
  and when a subscriber loses the post-upgrade race to GC. Close codes only
  travel on `wt.closed`, and the read loop can settle first — `viewer.ts`
  races them (see README gotchas).
- Broadcaster side: `BroadcastPipeline.start()` failures are typed
  (`BroadcastStartError.phase`); post-connect failures tear the session
  down internally (no zombie publisher). BroadcastPage falls back from
  reclaim to mint **only** on `phase === 'connect'`.
- Framing protocol is **implemented** (`gawk-server/wire`): VideoChunk
  datagrams carry frameID + chunkIndex/chunkCount + keyframe flag + timestamp
  (20-byte header, big-endian); a separate DecoderConfig message carries
  codec string + AVCC extradata. Golden test vectors for the future TS mirror
  live in `wire_test.go` and `docs/02-webtransport-hello.md`. chunkCount is
  capped at 3000 (`wire.MaxChunkCount` == `MAX_CHUNK_COUNT` in `wire.ts`).
- Hardening is **implemented** (R2, see `docs/07-hardening.md`): limits on
  concurrent broadcasts (default 5), total subscribers (50), per-IP connection
  rate (3/s burst 10, loopback bypassed), and global egress bandwidth
  (unlimited by default; over-limit datagrams are dropped at the subscriber
  drain, never queued); publishing requires a pre-shared secret when
  `-publish-secret` is set (query param — the WebTransport JS API can't set
  headers); `/statusz` keys broadcasts by a per-process HMAC of the ID (raw
  IDs are joinable and only ~31^6 strong — never expose them or hash them
  unkeyed). All knobs are flags + `GAWK_*` envs + Helm values, and **must be
  plumbed through `registryOptions` in `cmd/gawk-server/main.go`** — the R2
  post-implementation review found them wired only into the test helper.

## Directory structure
- `README.md` — project overview, quickstart, and the consolidated gotcha
  list (keep it in sync when a new gotcha lands in `docs/`)
- `BUGS.md` — known, confirmed, not-yet-fixed bugs (currently none; the
  Chrome 152 `getStats()` entry was root-caused 2026-07-14 as an upstream
  API removal, not a gawk bug — no browser ships `WebTransport.getStats()`
  today; see docs/13 D7). Check it before debugging anything
  overlay/stats-related; remove entries when fixed.
- `CODE-REVIEW.md` — **coding + review guidelines; bug fixes are test-first
  (failing test before the fix, always). Follow it for every change and
  review.**
- `ROADMAP.md` — **high-level roadmap for post-v0.5 work (R1 multi-broadcaster
  → R6 production UI), with per-item status and design links.**
- `gawk-app` is the folder for the frontend application
- `gawk-server` is the folder for the backend (the Relay server) — Go module,
  see `gawk-server/README.md` for build/run. Its `wire` package is **public**
  (not `internal/`) so `gawk-broadcast` can import it — see R14 Decision 1.
- `gawk-broadcast` is the native Linux broadcaster (R14) — a **separate** Go
  module (GUI + CLI over a shared `internal/engine`), importing
  `gawk-server/wire` unchanged rather than mirroring it; see
  `gawk-broadcast/README.md`. Not a container/chart/deploy component: it's a
  binary you run on your own PC.
- `docs/` — per-build-step design notes and gotchas. **Every design doc must
  define explicit acceptance criteria for its milestones and chunks of work**
  (a per-chunk criteria table à la `docs/implementation-tasks.md`, or a
  goal → verified-by table à la `docs/07`) — the R2 review traced its
  critical finding partly to a doc that listed proposed changes without
  acceptance criteria, so nothing forced the "does the flag actually reach
  production?" question. See
  `docs/01-loopback-test.md` for v0.1 (local loopback),
  `docs/02-webtransport-hello.md` for v0.2 (server + TLS + echo),
  `docs/03-single-client-e2e.md` for v0.3 (hub + publish/subscribe relay),
  `docs/04-fanout.md` for v0.4 (fan-out hardening, restart-safe caches,
  `/statusz`), `docs/05-resilience-deploy.md` for v0.5 (keepalive, viewer
  auto-reconnect, Docker, Helm, CI/release — **includes the deploy runbook**),
  `docs/06-multi-broadcaster.md` for R1 (multi-broadcaster design + E1–G2 chunks;
  implemented and verified), `docs/07-hardening.md` for R2 (limits, access
  control, bandwidth cap; implemented, incl. post-implementation review),
  `docs/08-resolution-framerate-picker.md` for R3 (broadcaster ladder:
  pre-encode scaling + fps gating, H1–H4 chunks),
  `docs/09-automatic-fallback.md` for R4 (automatic resolution fallback:
  encode-backpressure detection + auto step-down/up, I1–I4 chunks;
  implemented + manually verified 2026-07-14),
  `docs/10-production-ui.md` for R6 (production UI: landing/broadcaster/viewer
  surfaces, monochrome design system, J1–J6 chunks; implemented, manual
  browser verify passed 2026-07-14),
  `docs/13-observability.md` for R9 (observability & metrics: TCP ops
  endpoint with Prometheus `/metrics` + ServiceMonitor, relay ingress-loss
  window, client funnel stats + both-surface overlays, bottleneck playbook;
  M1–M7 implemented + manually verified 2026-07-14, M8 Grafana deferred),
  `docs/14-viewer-render-performance.md` for R10 (viewer render performance:
  Firefox drop/decode-gap diagnosis, rAF-coalesced latest-frame-wins
  rendering + WebGL render sink behind the R8 `RenderSink` seam, nested
  transport-worker split behind a `ViewerTransport` seam, decoder queue
  5→10, P1–P4 chunks; P1–P3 + queue bump implemented 2026-07-14, re-verified
  on Chrome + Firefox 2026-07-14),
  `docs/15-viewer-live-edge.md` for R5 (viewer live-edge, re-scoped;
  **Q1–Q3 implemented 2026-07-14, Q4 measurement pass + manual verify
  passed 2026-07-15 — done**: live-edge drift metric (windowed-min baseline, zero protocol
  change), absolute capture→render latency via new `TimeSync` (0x05, 18 B) /
  `ClockMapping` (0x06, 10 B) wire messages with the relay's monotonic clock
  as common reference — relay answers pings inline from both routes'
  read loops (rate-capped constant, never the video queue), hub caches +
  join-primes the mapping like the cached keyframe; plus a self-owned
  per-leg RTT independent of `getStats()`, and an opt-in smoothed playout
  (reorder-release pacing, `PLAYOUT_OFFSET_MS` 150, default off, right-click
  toggle, worker `playout` command). The viewer→server keyframe-request
  back-channel is **rejected for good** there, Decision 6),
  `docs/16-broadcaster-worker-offload.md` for R11 (broadcaster worker
  offload: broadcast pipeline in a Web Worker fed by a transferred
  `MediaStreamTrack` clone, K1–K4 chunks; implemented 2026-07-14, manual
  verify pending),
  `docs/17-viewer-playback-smoothing.md` for R12 (viewer playback smoothing:
  jitter measurement, paced presentation + adaptive offset behind a new
  separate "Paced playback (adaptive)" toggle, experimental frame
  interpolation with pre-registered kill criteria, T1–T6 chunks; **T1–T4
  implemented 2026-07-15, manual verify pending; T5/T6 not started;
  adaptive + interpolation are the production viewer's defaults since
  2026-07-15** — the right-click menu disables them),
  `docs/18-advanced-broadcaster-settings.md` for R13 (advanced broadcaster
  settings, **supersedes R7**: `isConfigSupported` probe matrix, HW-aware
  auto ceiling + 'auto' framerate default resolving framerate-first (60 fps
  when hardware probes it, else 30 — consciously revising item 11's fan-out
  default; software path keeps 30), acceleration
  tri-state (auto/hardware-only/software-only), capture aligned to the
  sticky selection via live `applyConstraints` so **no settings change ever
  restarts the stream** while R4 auto-stepping stays encode-only,
  bitrate/codec overrides + probe-annotated pickers, L1–L5 chunks;
  implemented 2026-07-15, manual browser verify pending),
  `docs/19-linux-native-broadcaster.md` for R14 (native Linux broadcaster:
  Gio GUI app + CLI over a shared Go engine in a new top-level
  `gawk-broadcast/` module, hardware-only encode via a Go-owned ScreenCast
  portal handshake + GStreamer subprocess; Vulkan Video as the target
  encode API; V0–V7 chunks + V8 direct Vulkan Video encode; **V0–V7
  implemented 2026-07-15, automated gates green, manual verify on the
  gaming PC pending; V8 gated on V2's on-hardware Vulkan result**),
  `docs/20-system-audio.md` for R15 (system audio, **experimental,
  default-off**: Opus/WebCodecs over datagrams — one Opus packet per
  datagram, new wire types 0x07/0x08 + hub audio-config cache, viewer-worker
  decode with a main-thread AudioWorklet sink, good-enough A/V sync
  (shared capture clock, adaptive audio jitter buffer, audio-master pacing
  in R12 paced modes); N1–N6 chunks; **designed 2026-07-15, not
  started**),
  `docs/21-ios-video-fullscreen.md` for R16 (iOS native fullscreen:
  iPhone has **no Element Fullscreen API** — today's viewer fullscreen
  button is a silent no-op there; a `TeeRenderSink` decorator wraps each
  *presented* canvas frame (R12 smoothing preserved, interpolated frames
  included) into a worker-side `VideoTrackGenerator` whose track feeds a
  hidden pre-armed `<video>` for `webkitEnterFullscreen()`, with CSS
  pseudo-fullscreen as the fallback tier; gated on the *absence* of
  `Element.requestFullscreen` so **non-iPhone devices are byte-identical**
  (sole overlay-only exception: the new **Feature Gates** stats section —
  UpperCamelCase gate names, first gate `NativeVideoFullscreen`);
  U1–U4 chunks; **U1–U3 implemented 2026-07-16; U4: two on-device passes
  black → tee reworked to decoded-frame clones 2026-07-16, third pass
  pending**).
- Each component has `deploy/` (Dockerfile + Helm charts); `.github/workflows/`
  holds CI + release automation.
- `docs/implementation-tasks.md` — **the server design + chunked task
  breakdown (A1–D3) and R1 support (E–G reference) with per-chunk status.**

## Build order
1. Local loopback testing — **done** (v0.1 in `gawk-app/`; see `docs/01-loopback-test.md`)
2. WebTransport hello-world (TLS/certificate setup) — **done** (chunks
   A1–A4 + B1; A5 browser echo verified 2026-07-10 with Chrome inside WSL2;
   see `docs/02-webtransport-hello.md`).
3. Single-client end-to-end — **done** (v0.3: B2 hub, B3 publish/subscribe
   routes, B4 frontend transport module + broadcast/view pages, hash-routed
   `#/broadcast` / `#/view` / `#/loopback`; manual browser verify passed
   2026-07-10 — see `docs/03-single-client-e2e.md`).
4. Fan-out (multi-subscriber) — **done** (v0.4: C1 hub hardening, C2
   restart-safe caches, C3 `/statusz`; manual multi-viewer browser verify
   passed 2026-07-12 — see `docs/04-fanout.md`).
5. Resilience + deployment — **done** (v0.5: D1 keepalive + viewer
   auto-reconnect, D2 Docker images, D3 Helm charts, D4 release-please CI;
   first release cycle, homelab install + automated deploy-on-release, and
   manual end-to-end browser verify all completed 2026-07-12 — see
   `docs/05-resilience-deploy.md`). Forced keyframes were already
   broadcaster-side (`keyframeIntervalFrames: 120` in `gawk-app/src/media/`).
6. Multi-broadcaster support — **done** (R1: E1-E4 server registry + routes,
   F1-F4 frontend ID announcements + reclaim UI + terminal states; manual
   E2E verify passed 2026-07-12 — see `docs/06-multi-broadcaster.md`).
7. Hardening — **done** (R2: limits, publish secret, connection rate
   limiting, defensive parsing, bandwidth cap, obfuscated `/statusz`;
   implemented 2026-07-13 with post-implementation review fixes — see
   `docs/07-hardening.md`).
8. Broadcaster resolution & framerate picker — **done** (R3, implemented +
   manually verified 2026-07-13: native/1080p/720p/480p × native/60/30/5 ladder, pre-encode
   OffscreenCanvas scaling + timestamp fps gate, ladder-scaled bitrate,
   **keyframe cadence now time-based** (`keyframeIntervalMs`, replacing
   `keyframeIntervalFrames: 120` — a frame-count GOP is 24 s at 5 fps; default
   was 2000, later cut to **500** for loss recovery — see item 11),
   live mid-stream changes via encoder recreate; zero server changes — see
   `docs/08-resolution-framerate-picker.md`).
9. Automatic resolution fallback — **done; implemented + released 2026-07-13,
   manually verified 2026-07-14** (R4: encode-queue rejection-ratio detection with
   hysteresis + cooldown, in the pure `media/fallback.ts` `FallbackController`;
   a new **"auto" resolution selection (default)** steps both down and up
   (up-probes with exponential backoff against oscillation) plus
   encoder-error step-down, while **explicit rungs are never auto-stepped** —
   frame drops over overriding the broadcaster; zero server/wire/viewer
   changes — see `docs/09-automatic-fallback.md`). **Real-hardware finding
   (2026-07-13)**: the auto step-down does not fire on the gaming PC's
   hardware encode path — HW encoders drain frames without `encodeQueueSize`
   growing past the `> 2` trigger, so the rejection signal under-fires;
   observed low fps was source-limited (4K capture), a correct no-op.
   Software-path verification passed 2026-07-14 (named thresholds in
   `fallback.ts` kept as-is); a hardware-strain signal is a possible
   deferred follow-up.
10. Production UI — **done; implemented 2026-07-13, manual browser verify
   passed 2026-07-14** (R6, `docs/10-production-ui.md`). Three surfaces (landing
   at `#/`, broadcaster at `#/broadcast`, viewer at `#/view/<id>`),
   monochrome-restrained design system (tokens in `styles/global.css` +
   `src/ui/` primitives), segmented code entry, preview-hero broadcaster,
   cinematic viewer with fullscreen + a stats overlay (hotkey
   `Ctrl+Alt+Shift+D` + right-click menu); current pages re-homed **frozen**
   under `#/debug/*` (they do not share components with the production UI).
   UI-only — zero server/wire/viewer-protocol changes; all transport/media/state
   modules carry over untouched. Chunks J1–J6. **R5 (viewer live-edge) was
   skipped for now**; R6 doesn't depend on it (an R5 live-edge metric slots
   into the stats overlay later).
11. Heavy-motion resilience — **done** (2026-07-14; UI/pipeline only, zero
   server/wire changes). Three changes to cut heavy-motion corruption on
   Chrome and "Awaiting keyframe" stalls on Firefox: (a) **500ms GOP**
   (`keyframeIntervalMs` 2000→500) so a lost/discarded frame self-heals in
   ≤0.5 s; (b) **viewer freeze-on-gap** (`viewer.ts`): a delta whose frameId
   skips ahead holds the last good frame until the next keyframe instead of
   feeding the decoder corrupt references (visible corruption → brief freeze;
   gap discards surface on the existing "Awaiting keyframe" stat); (c)
   **30 fps default fan-out cap** (`framerateRung` default native→30 in
   `broadcastSettingsStore`; a broadcaster can still pick 60/native), halving
   the datagram rate and viewer decode load. Manual browser verify passed
   2026-07-14 —
   see `docs/08-resolution-framerate-picker.md` (GOP + fps default) and
   `docs/03-single-client-e2e.md` (freeze-on-gap decode policy).
12. Worker offloading & reliable keyframes — **done (S1–S7: reliable
   keyframes + worker offload); browser-verified 2026-07-14** (R8,
   `docs/12-worker-and-reliable-keyframes.md`).
   Keyframes now travel over **per-subscriber reliable WebTransport uni
   streams** instead of datagrams — a lost UDP packet can no longer ruin a
   (large, heavy-motion) keyframe. The relay uses **store-and-forward
   fan-out** (`gawk-server/internal/hub`): it reads each keyframe stream fully
   into a bounded buffer (`MaxKeyframeBytes`) *before* opening one uni stream
   per subscriber, structurally decoupling the publisher from any slow
   subscriber; a subscriber stalling past `KeyframeWriteTimeout` is
   `CancelWrite`-ed and superseded by the newest keyframe (**≤1 in-flight per
   subscriber**) — "drops over stalls" at stream granularity. **Only keyframes
   are reliable; deltas stay on datagrams** (reliable deltas would reintroduce
   head-of-line blocking). New wire message `TypeStreamFrame` (0x04, golden
   vectors keep Go/TS byte-identical). The viewer merges stream keyframes with
   datagram deltas in a pure, bounded **reorder buffer**
   (`transport/reorder-buffer.ts`, **no fixed playout offset** — live-edge
   philosophy preserved, constant-offset playout left to R5) that centralizes
   the freeze-on-gap policy formerly in `viewer.ts`. New relay knobs
   (`-max-keyframe-bytes`, `-keyframe-write-timeout` + `GAWK_*` + Helm) are
   plumbed through `registryOptions` per the R2 finding. The preliminary
   sketch's fixed 200 ms playout offset was **rejected**. **S6 (worker offload)
   is implemented**: the whole viewer pipeline runs in a Web Worker
   (`transport/viewer.worker.ts` around a DOM-free `ViewerWorkerCore` that
   *reuses* `ViewerSession` for reconnect) and renders decoded frames to a
   transferred `OffscreenCanvas` via a `RenderSink` seam — **frames never cross
   back to the main thread**. A boot handshake probes worker WebCodecs/
   WebTransport *before* the one-shot `transferControlToOffscreen()`, so an
   unsupported worker falls back to the main-thread `ViewerSession`
   (`features/viewer/useViewerConnection.ts`); the worker + transfer live for
   the whole view and teardown is StrictMode-deferred (see README gotcha). Zero
   WebRTC/MoQ; wire+server+broadcaster+viewer changed in lock-step. S6 is
   UI/pipeline-only (zero server/wire changes on top of S1–S5).
13. Observability & metrics — **done (M1–M7); manually verified 2026-07-14;
   M8 (Grafana dashboard) deferred** (R9, `docs/13-observability.md`,
   2026-07-14). The relay grew a **plain-TCP ops endpoint** (`-metrics-addr`,
   default `:2112`, literal `off` disables — an empty env var reads as unset)
   serving Prometheus `/metrics`, `/healthz`, and a curl-able `/statusz`
   mirror — the HTTP/3 server itself is unscrapeable. Metrics come from a
   snapshot `prometheus.Collector` over `Registry.Stats()` (one source of
   truth): per-broadcast `gawk_broadcast_*{broadcast=<obfuscated>}` +
   lifetime `gawk_relay_*` totals (two prefixes because client_golang rejects
   one family with two label sets). New signals: an RTP-style **ingress-loss
   window** (`hub/ingress.go` — broadcaster→relay frame/chunk loss, robust to
   datagram reordering), keyframe-drop reasons split
   (superseded/slow/bandwidth/open_failed), `sendErrors` exposed, egress
   bytes, `gawk_connections_total{route,outcome}`, per-subscriber
   `subscriberDetails` in `/statusz`. Chart: metrics ClusterIP Service +
   gated ServiceMonitor; the metrics port never touches the public
   LoadBalancer. Clients: `WebTransport.getStats()` sampling (feature-
   detected, all-nullable — and since 2026-07-14 known to exist in **no**
   shipping browser: Chromium removed it, rewrite "in development"; the
   `connection.*` rows are dark until it re-ships, see docs/13 D7; since
   2026-07-15 the overlay bitrates are self-counted instead — viewer
   `videoBytesReceived` mirrors broadcaster `bytesSent`, rows renamed
   "Video bitrate (recv)"/"(sent)") + funnel
   rates (capture →
   post-gate → encoded → **sent** fps; received → decoded → rendered fps),
   stall ages, and a shared sectioned stats overlay on **both** production
   surfaces (same hotkey) with **Copy diagnostics** JSON (rolling ~10 s
   sample window) as the remote-troubleshooting story. The
   symptom→signature **bottleneck playbook** lives in docs/13. Zero
   wire-format changes.
14. Viewer render performance — **done; P1–P3 + decoder-queue bump + field-finding
   fixes implemented 2026-07-14, re-verified on Chrome + Firefox 2026-07-14
   (P4 remainder deferred)** (R10,
   `docs/14-viewer-render-performance.md`; P1–P4 UI/pipeline only — the
   field findings later added one server-side fix, zombie eviction, below).
   Diagnosis: the R8 worker ran transport + decode + render on one
   thread, and Firefox's 2D-canvas `drawImage(VideoFrame)` does a synchronous
   CPU conversion per frame — starving the datagram reader (silent datagram
   drops → "Dropped (incomplete)") and the decoder (decoded fps < received
   fps) at once. **P1** `CoalescingRenderSink` (latest-frame-wins, ≤1 draw per
   worker-rAF tick, superseded frames closed unseen — so "Rendered fps" now
   reads ≈min(decoded, display Hz) and lower than decoded fps under load is
   *healthy*); **P2** `WebGLRenderSink` (textured quad,
   `texImage2D(VideoFrame)`; **chosen over `bitmaprenderer`**, which adds
   per-frame `createImageBitmap` churn and may hit the same Gecko software
   conversion) with the 2D sink as fallback via `createRenderSink()`, plus
   placement stats in `ViewerStats` + overlay — `renderer`
   ('webgl'/'2d'/null), `pipelineContext` ('worker'/'main-thread', *detected*
   via `window` absence), `transport` ('worker'/'in-process'/null); healthy
   fast path reads WebGL/Worker/Worker. **P3**
   transport/decode worker split: `ViewerPipeline` consumes a
   `ViewerTransport` seam (`viewer-transport.ts`; `onClosed` is the single
   session-end signal — the wt.closed race lives inside the transport);
   the viewer worker spawns a **nested** `transport.worker.ts` (one per
   pipeline attempt, graceful close then 250 ms reap) that runs the
   WebTransport read loops via the same `LocalViewerTransport` and posts
   transferable buffers, so decode/render pressure can't starve the
   incoming-datagram queue; in-process transport remains for the main-thread
   path and no-nested-worker fallback. **P4 partial**: decoder queue bound
   raised 5→10 (`getMaxDecoderQueueSize`); the rest deferred pending Firefox
   measurements. **Field findings (2026-07-14, docs/14)**: keyframes are
   store-and-forwarded (~236 KB) and can land >500 ms behind their datagram
   deltas — `KEYFRAME_WAIT_MS` raised 200→1000 ms (200 ms made every GOP
   keyframe-only ~2 fps on a congested peer; signature: one gap resync per
   keyframe, zero reassembler drops); the relay now **evicts** a subscriber
   after 10 consecutive keyframe stream-open failures (zombie session with
   exhausted stream credit) with non-terminal close code **4001**
   (`CloseCodeSubscriberUnresponsive` == `CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE`);
   Chrome 152 "broke" `getStats()` sampling → root-caused as Chromium
   removing the API entirely (docs/13 D7). **Restart/rollover fix**
   (finding 5): R8 severed the reassembler's restart recovery (keyframes
   bypass it on streams, so its late-delta watermark never reset — a
   broadcaster restart froze viewers into a 2 fps keyframe slideshow,
   "Dropped (late)" at full rate). Stream keyframes now sync the watermark
   (`noteStreamKeyframe`), all frameId comparisons are serial/uint32-wrap-
   aware (`wire.frameIdAhead`/`nextFrameId`; broadcaster wraps its counter at
   source), and a serially-backwards keyframe = immediate reorder resync.
15. Viewer live-edge — **done; Q1–Q3 implemented 2026-07-14, Q4 measurement
   pass + manual browser verify passed 2026-07-15 (all knobs kept)**
   (R5 re-scoped, `docs/15-viewer-live-edge.md`).
   (a) **Live-edge drift** (`transport/live-edge.ts`): per-frame
   `viewerNow − capture timestamp` minus its 60 s windowed min — pure lag
   with the clock offset cancelled (capture.ts already stamps frames on the
   broadcaster's `performance.now()`), reset on the reorder buffer's
   restart signal; zero protocol change. (b) **Absolute capture→render
   latency**: `TimeSync` (0x05, 18 B) ping/pong on both routes against the
   relay's **monotonic** clock (inline reply from the session read loops,
   per-session 5/s token bucket — constants, no new knobs) + a broadcaster
   `ClockMapping` (0x06, 10 B) that the hub caches, fans out, join-primes,
   and invalidates like the cached keyframe; the client keeps the lowest-RTT
   sample of 8 (error ≈ rtt/2 per leg) — also yields a self-owned
   `RTT (time-sync)` on both overlays, immune to `getStats()` being
   unshipped in every current browser (docs/13 D7). Negative g2g clamps to 0. Both messages strict-parsed, golden
   vectors Go↔TS. (c) **Opt-in smoothed playout** (`transport/playout.ts`):
   reorder-release pacing to `timestamp + arrival-baseline + 150 ms`,
   default off, right-click "Smooth playback" toggle (persisted), crosses to
   the viewer worker as a `playout` command setting module state read live
   per advance; drop/resync policies unchanged (delay, not patience). The
   keyframe-request back-channel was **rejected for good** (docs/15
   Decision 6). New overlay rows: Live-edge drift, Latency (capture→render),
   Playout (Delivery section).
16. Broadcaster worker offload — **implemented 2026-07-14 (K1–K4); automated
   gates green, manual browser verify pending** (R11,
   `docs/16-broadcaster-worker-offload.md`; UI/pipeline only, zero
   server/wire changes). The broadcast pipeline (MSTP pump → preprocess →
   encode → packetize → send) runs in a Web Worker
   (`transport/broadcaster.worker.ts` around a DOM-free `BroadcastWorkerCore`
   that reuses `BroadcastPipeline` via a new media-source seam). The main
   thread keeps `getDisplayMedia` (window scope + gesture) and the preview
   (original track); a **`track.clone()` is transferred** into the worker,
   which creates MSTP there — transferring `processor.readable` was rejected
   (frames would still hop through the main-thread realm). Connect-before-
   picker ordering and `BroadcastStartError.phase` semantics (reclaim→mint
   only on `'connect'`) are preserved via an `awaitingCapture` handshake.
   Capability gate: worker boot handshake + a synchronous dummy-track
   transfer probe (`canvas.captureStream()`; `DataCloneError` throws
   sender-side) — any failure falls back to the untouched main-thread
   pipeline (Firefox always does; `#/debug/broadcast` stays main-thread).
   `WorkerBroadcastSession`/`createBroadcastSession`
   (`features/broadcaster/`) present the same `BroadcastSessionLike` surface,
   so `BroadcasterScreen`'s reclaim/mint logic is unchanged.
   `BroadcastStats.pipelineContext` + an overlay "Pipeline" row expose the
   placement. **Note**: a worker does *not* escape Chrome's process-level
   backgrounding (same renderer process) — this removes main-thread
   contention from the frame path, nothing more.
17. Viewer playback smoothing — **T1–T4 implemented 2026-07-15; automated
   gates green, manual browser verify pending; T5 (motion-estimated
   interpolation) + T6 (findings pass) not started** (R12,
   `docs/17-viewer-playback-smoothing.md`; viewer-client only, zero
   server/wire changes). (a) **Jitter measurement** (T1):
   presentation-cadence error recorded at the actual paint (draw-interval
   minus timestamp-interval — ≡0 for perfect pacing at any fps), arrival
   jitter as windowed p95−min via a new `WindowedQuantileTracker`
   (live-edge.ts sibling), decode jitter σ; new `ViewerStats` fields +
   Delivery overlay rows. (b) **Paced presentation** (T2):
   `PacedPresentationSink` subsumes the R10 `CoalescingRenderSink` (no
   target ⇒ identical coalescing; with targets it holds ≤3 decoded frames
   and presents each in its vsync slot); playout is now a three-mode
   setting — `'off' | 'fixed' (the R5 150 ms toggle, unchanged) |
   'adaptive'` — with a **separate, mutually-exclusive "Paced playback
   (adaptive)" right-click toggle** (user decision: never repurpose an
   existing toggle), persisted as `gawk:playout-mode` (legacy boolean key
   migrates to `'fixed'`); in adaptive mode the reorder buffer releases at
   `target − DECODE_LEAD_MS` (35 ms) so the decoder frame pool stays
   bounded. (c) **Adaptive offset** (T3): `PlayoutController` —
   clamp(p95−min + 34 ms headroom, [50, 350]), seed 150 until ~5 s of
   window, slew up 50 ms/s / down 5 ms/s after a 15 s dwell; live value on
   the overlay. (d) **Experimental interpolation scaffold** (T4, droppable):
   `InterpolatingWebGLRenderSink` (ping-pong textures, upload/present(α)
   blend shader, WebGL2-only) + opportunistic α=0.5 mid-slots (30→60, no
   added latency — synthesized only when the next frame is already in
   hand, never across >100 ms gaps); own "Frame interpolation
   (experimental)" toggle, offered only where the pipeline reports it
   available; **pre-registered kill criteria** — a documented rejection of
   T5 is a valid completion. **Defaults flipped 2026-07-15 (user
   decision)**: the production viewer defaults to **adaptive paced playback
   with interpolation on** (right-click menu disables either; a legacy
   explicit smooth-playback opt-out migrates to live-edge, and the
   pipeline-level/module default stays `'off'` so `#/debug/*` keeps
   live-edge) — docs/17 Decision 8, as superseded.
18. Advanced broadcaster settings — **implemented 2026-07-15 (L1–L5);
   automated gates green, manual browser verify pending** (R13,
   `docs/18-advanced-broadcaster-settings.md`; supersedes R7; UI/pipeline
   only, zero server/wire changes). `VideoEncoder.isConfigSupported` **probe
   matrix** (`media/probe.ts`; prefer-hardware supported=true is Chromium's
   HW commitment — advisory only, runtime `configure()` wins); **HW-aware
   auto ceiling** (auto resolution starts at the highest rung probing
   hardware; 1080p software floor-ceiling) + **'auto' framerate default**
   resolving framerate-first (60 when any rung probes HW at 60, else 30 —
   consciously revising item 11's fixed-30 fan-out default; software path
   keeps 30; never 'native'); **acceleration tri-state**
   (auto/hardware-only-refuses-software/software-only) + bitrate override
   ([0.5, 50] Mbps) + codec pin, all persisted, all applied via encoder
   recreate; **capture aligned to the sticky target** (explicit rung or auto
   ceiling — never auto steps) via live `track.applyConstraints` on the
   media-source seam (broad `getDisplayMedia` grant kept; matrix refined
   from real frame dims **upward only** to avoid the constrain→shrink→
   re-ceiling feedback loop; worker path constrains the transferred clone
   worker-side) — **no settings change ever restarts the stream**; the old
   >1080p@>30 force-caps are removed (explicit choices honored, annotated
   instead); probe-annotated pickers (badge/disable, never remove) +
   Advanced settings panel + overlay Auto ceiling/Auto fps rows.
19. Native Linux broadcaster — **V0–V7 implemented 2026-07-15; automated
   gates green (both Go modules), manual verify on the gaming PC pending;
   V8 not started — it is hard-gated on V2's on-hardware Vulkan result**
   (R14, `docs/19-linux-native-broadcaster.md`). A **Gio GUI app**
   (`cmd/gawk-broadcast-gui`) + a CLI (`cmd/gawk-broadcast`) over a shared
   engine (`internal/engine`) in a **new top-level `gawk-broadcast/` Go
   module**, publishing with **hardware encode** from Linux, because the
   browser structurally cannot: WebCodecs `VideoEncoder` HW encode ships on
   Windows/macOS/Android only (Linux gets HW *decode* only), Chromium's
   VA-API doc disclaims Linux support, and on NVIDIA it is impossible in
   principle — Chromium's Linux encode path is VA-API only and
   `nvidia-vaapi-driver` is decode-only by design. **Don't go flag-hunting
   for browser HW encode on Linux; it isn't there.** Hardware encode is a
   **hard requirement**: cascade `vulkanh264enc` → `nvh264enc` →
   `vah264enc` → **refusal pointing at the browser** (no software rung —
   software encode is the browser's job; user decision 2026-07-15), each
   candidate accepted only by **real trial encode** (`videotestsrc`, never
   the portal; last-good cached; the live start is the final probe — R13's
   probe-matrix instinct one layer down, incl. its advisory-only caveat).
   Design notes worth knowing before touching either end: **V0 promoted
   `internal/wire` to a public `gawk-server/wire`** so the new module can
   import it (the original same-module plan coupled relay CI to Gio's cgo
   headers and made broadcaster commits auto-redeploy the relay; instead:
   own release-please component `gawk-broadcast-vX.Y.Z` + own CI job with
   the Gio deps), and reusing `wire` unchanged (never mirroring it) is what
   keeps a second broadcaster from rotting; the engine's
   `Session`/`Callbacks` mirror the TS
   `BroadcastSessionLike`/`BroadcastCallbacks` (incl. `StartError.Phase` —
   Resume applies the same reclaim→mint-only-on-`connect` rule — plus the
   HTTP status, which webtransport-go exposes and the browser's opaque
   `WebTransportError` can't); the viewer **already auto-detects Annex-B vs
   AVCC** (the `isAnnexB` sniff in `viewer.ts`), so the engine emits raw
   Annex-B with empty extradata and builds no avcC record. **Capture:
   Go-owned XDG ScreenCast portal handshake** (`godbus`, **the share picker
   appears on every start — the choice is deliberately never persisted, so
   `persist_mode`/`restore_token` are never sent** (reversed 2026-07-16 — the
   original "pick once, ever" restore-token design was dropped by user
   decision: always ask what to share on restart); cursor embedded; works on
   X11 GNOME too, gate on the portal not on Wayland) **feeding a GStreamer
   subprocess**
   (`pipewiresrc fd=…` → encoder → `h264parse config-interval=-1` →
   `mpegtsmux` → stdout pipe; one PES = one AU, in-band SPS/PPS at every
   IDR — load-bearing because the DecoderConfig extradata is empty).
   **`pipewiregrab` was rejected 2026-07-15: it is NOT in mainline FFmpeg**
   (an unmerged patchset carried downstream by Jami; mainline ffmpeg has no
   PipeWire input at all) — don't re-propose it without verifying it
   actually merged; ffmpeg's one remaining role is generating V8's
   reference bitstream **offline** with mainline `h264_vulkan` (≥7.1) from
   a committed y4m fixture. **Vulkan Video (`VK_KHR_video_encode_h264`) is
   the target encode API** (V8 = direct Vulkan in Go), the only one
   spanning RADV + ANV + NVIDIA with no asterisk. **Direct VAAPI is
   rejected and don't re-propose it**: Chromium's Linux backend is VA-API
   only and that is *precisely why* it can't encode on NVIDIA — building on
   VAAPI reproduces the limitation R14 exists to escape. V8 is **gated on
   V2's Stage-1 Vulkan result**, its differential oracle criteria are
   decode-clean + PSNR-within-ε-of-reference + structural sanity (byte or
   frame identity is unachievable even for a perfect implementation), and
   it adds a top-of-cascade candidate rather than retiring the rest.
   Subprocess rationale is crash isolation + version tolerance, *not* "no
   cgo" (Gio already requires cgo). Fixed rung **1080p60** (coherent now
   the engine is hardware-only by construction; 500 ms GOP); `SetLadder`
   cut from the v1 surface (restarts are picker-free thanks to the restore
   token, so a rung change is cheaply addable later); R4's
   `FallbackController` deliberately **not** ported (its `encodeQueueSize`
   trigger is the one R4 found never fires on HW encode). **Encoder
   invariants are acceptance criteria**: no B-frames (decode order ==
   presentation order is a protocol assumption), ≤1-frame encoder-internal
   latency per candidate, drop-only fps gating (never CFR-converting
   damage-driven capture); **uplink policy**: ≤1 in-flight keyframe stream
   with supersede, drop-frame-remainder on datagram send failure, MTU
   re-chunk on `DatagramTooLargeError`. GUI: Gio (native Wayland; Wails
   rejected — WebKitGTK DMA-BUF crashes on Wayland+NVIDIA); the window
   **is** the app (closing it ends the broadcast), no preview, no source
   picker; **notifications via `godbus` with critical urgency for
   failures** — KDE's portal **inhibits normal notifications while screen
   casting**, so only critical-urgency ones reach a fullscreen broadcaster;
   **no viewer count / "first viewer joined"** (nothing on the wire tells a
   publisher about subscribers — browser parity; a `SubscriberCount`
   message is a possible future wire+relay change, not an R14 smuggle-in).
   **Tray and global hotkeys deferred 2026-07-15** — research kept in the
   doc's Deferred section; don't re-derive it. Not a
   container/chart/CI-deploy component — binaries you run on your own PC.
20. System audio — **designed 2026-07-15 (N1–N6), not started** (R15,
   `docs/20-system-audio.md`). **Experimental, default-off**: an "Enable
   audio (experimental)" toggle in the broadcaster's advanced settings
   (off ⇒ `audio: false`, byte-identical to today), and the viewer shows
   audio controls (mute/volume, overlay Audio section) **only when audio is
   actually received**. Direction (settled over reliable-stream Opus, raw
   PCM, and MediaRecorder+MSE alternatives): **Opus via WebCodecs over
   datagrams** — 48 kHz stereo, 128 kbps, 20 ms frames ≈ 320 B, so **one
   Opus packet per datagram** (no chunking/reassembly/keyframes/reliable
   streams). New wire types `TypeAudioFrame` 0x07 (16 B header: own uint32
   seq space + timestampUs on the **same broadcaster `performance.now()`
   clock as video** — the load-bearing sync decision) and `TypeAudioConfig`
   0x08; hub gains two dispatch cases + a `cachedAudioConfig` join-prime
   slot (ClockMapping lifecycle); config re-sent at 1 Hz (no keyframe to
   anchor re-emits to); audio **never** touches the video ingress-loss
   window or `framesRelayed`. Broadcaster: parallel audio lane in
   `BroadcastPipeline` (processing off — game audio, not voice);
   no-audio-track = graceful video-only (Firefox broadcasters, unchecked
   picker box); worker path transfers the audio `track.clone()` beside the
   video clone. **The toggle applies on the next broadcast start** — the
   one R13 live-apply exception, forced by `getDisplayMedia` (an audio
   track can't be added without re-prompting). Viewer: `AudioDecoder` in
   the viewer worker, decoded `AudioData` **transferred to a main-thread
   `AudioWorklet` ring buffer** (`AudioContext` can't live in a worker —
   the first deliberate decoded-media worker→main crossing); gaps →
   silence, late → drop, live-edge discipline; no Opus FEC/PLC in v1
   (WebCodecs exposes no hook). A/V sync ("good-enough"): `avSkewMs`
   measured always (target median ≤ 60 ms, p95 ≤ 120 ms); live-edge default
   keeps video undelayed with a small adaptive audio jitter buffer
   (40–150 ms, R12 controller pattern); in R12 paced modes **audio becomes
   the master clock** for video display targets (slew-limited,
   arrival-baseline fallback); frame interpolation unaffected by
   construction. Non-goals: mic mixing, R14-native audio (wire types are
   ready — follow-up there), FEC, DTX, audio-only mode.
21. iOS native fullscreen — **U1–U3 implemented 2026-07-16 (automated gates
   green); U4 in progress — two on-device passes found native fullscreen
   black; the tee was reworked to decoded-frame clones 2026-07-16 (below),
   third pass pending**
   (R16, `docs/21-ios-video-fullscreen.md`; viewer-client only, zero
   server/wire changes). The viewer fullscreen button is a **silent no-op
   on iPhone**: no Element Fullscreen API exists there (iPad has it since
   16.4; every iOS browser is WebKit), and the only native fullscreen is
   `HTMLVideoElement.webkitEnterFullscreen()` — which needs a `<video>`,
   while the viewer paints a canvas. Design: keep the whole pipeline +
   R12 smoothing untouched and add a **presentation tee** — a
   `TeeRenderSink` decorator around the context sink (inside
   `PacedPresentationSink`, passing through the interpolation
   `upload`/`present` surface) that, once armed, wraps each **presented**
   frame as `new VideoFrame(canvas, {timestamp})` into a worker-side
   `VideoTrackGenerator` (worker-only API — Safari 18; iOS's WebTransport
   floor is Safari 26.4, so it's guaranteed present, and iOS was confirmed
   on-device 2026-07-15 to run the worker pipeline); the track transfers
   to the main thread once and feeds a hidden **pre-armed**
   `<video playsinline muted>` (armed at `watching` — a lazy arm risks
   leaving the user gesture; `display:none` breaks the API, hide by
   size/position). `useFullscreen` becomes three tiers: element fullscreen
   (unchanged where it exists) → `webkitEnterFullscreen` → CSS
   pseudo-fullscreen. **Gate = absence of `Element.requestFullscreen`**
   (iPhone signature) checked once main-thread: non-gated devices are
   **byte-identical** (no tee construction, no worker messages, no video
   element — hard requirement). Pre-registered fallback: if U1's probe
   (`VideoTrackGenerator` + trial `VideoFrame`-from-OffscreenCanvas — the
   one unverified load-bearing fact) fails on real hardware, U2/U3 drop
   and pseudo-fullscreen ships. Native fullscreen shows the system player
   UI — overlay/menu unreachable there by construction. Also ships the
   overlay's new **Feature Gates section** (UpperCamelCase gate names as a
   TS string-literal union; `ViewerStats.featureGates`), first/only gate
   `NativeVideoFullscreen`; the section renders only where gates are
   reported (broadcaster overlay untouched) and appears on every viewer —
   the one deliberate, overlay-only R16 change on non-gated devices.
   **U4 (2026-07-16): two on-device passes showed native fullscreen
   entering but black.** Pass-1 defenses (tee-local zero-based monotonic
   PTS — never broadcaster-clock source timestamps; `preserveDrawingBuffer`;
   gesture-context `play()` before `webkitEnterFullscreen` + imperative
   `video.muted = true`; gated-only overlay **Native Fullscreen** section)
   didn't cure it; pass 2 showed frames flowing end-to-end (tee climbing,
   element playing + presenting) → **`VideoFrame`-from-WebGL-canvas
   readback content is black on iOS WebKit even with preserveDrawingBuffer;
   don't build on canvas readback there.** The tee now writes **clones of
   the decoded frames it presents** (`new VideoFrame(frame, {timestamp:
   localTs})`; `draw()` clones pre-consume, `upload()` holds the clone
   until its `present(1)`, superseding uploads close it unseen; probe
   trials clone-with-timestamp; canvas readback + preserveDrawingBuffer
   removed) — interpolated mid-blends no longer cross, fullscreen shows
   real frames at paced cadence. New overlay **Content sample** row
   (periodic 4×4 peak-RGB of the element) separates black frame content
   from a black native player. Third pass pending; pre-registered verdicts
   in docs/21 "U4 findings" (high sample + still black ⇒ the native player
   can't present locally generated MediaStreams ⇒ remove tier 2, ship
   pseudo). BUGS.md entry tracks it.

## Deployment & CI (locked in — decided 2026-07-12)
- **Helm charts, one per component** (`gawk-server/deploy/charts/gawk-server/`,
  `gawk-app/deploy/charts/gawk-app/`), separately versioned; **chart version ==
  appVersion == image tag** always. The frontend is deployed too (nginx
  behind Ingress class `nginx-int`); the relay is a UDP LoadBalancer
  (nginx ingress can't proxy WebTransport).
- **Versioning**: SemVer 2 from conventional commits via release-please
  (monorepo manifest mode, **one combined release PR** — separate PRs
  conflicted on the shared manifest, don't switch back; tags stay
  per-component: `gawk-server-vX.Y.Z` / `gawk-app-vX.Y.Z`). First releases
  (v0.5.0 both) published 2026-07-12.
- **Registry**: GHCR — images `ghcr.io/tuhis/<component>`, charts
  `oci://ghcr.io/tuhis/charts/<component>` (lowercase; private → classic PAT
  pull secret).
- **CI is publish-only; deploys are automated cluster-side** (since
  2026-07-12): whenever a new version is released, it is deployed to the
  homelab automatically. CI never touches the cluster — no cluster
  credentials in GitHub. Manual `helm upgrade --install` remains the
  initial-install / break-glass path (runbook in `docs/05`). Don't
  re-propose raw manifests, semantic-release, or CI-driven deploys
  without discussion.

## On the horizon (not started)
- Media over QUIC (MoQ) — explicitly deferred; still an unstable IETF draft
  (draft-17 surveyed) with no native browser support yet. Don't build toward it now.

## Key constraints / principles to respect
- ~1200-byte safe datagram payload limit drives the chunking design — don't
  assume larger payloads are safe
- Favor dropped frames over stalled playback (this is why datagrams, not streams,
  were chosen)
- Small fixed audience + homelab bandwidth headroom means we can trade some
  robustness for simplicity vs. a general-purpose streaming platform
- **Trust the actual `VideoFrame` in hand, not `MediaStreamTrack.getSettings()`
  or any other metadata source** — Chrome's `getSettings()` has been observed
  to disagree with the frames MSTP actually delivers. Configure encoders and
  compute layout from the frames themselves. See `docs/01-loopback-test.md`.

## Explicitly set aside (don't suggest reintroducing without discussion)
- WebRTC + self-hosted SFU (OvenMediaEngine, LiveKit, mediasoup) — more mature,
  was the "safe" choice, deliberately not taken
- MoQ — future direction, not current
