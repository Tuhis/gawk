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
- Framing protocol is **implemented** (`gawk-server/internal/wire`): VideoChunk
  datagrams carry frameID + chunkIndex/chunkCount + keyframe flag + timestamp
  (20-byte header, big-endian); a separate DecoderConfig message carries
  codec string + AVCC extradata. Golden test vectors for the future TS mirror
  live in `wire_test.go` and `docs/02-webtransport-hello.md`. chunkCount is
  capped at 1000 (`wire.MaxChunkCount` == `MAX_CHUNK_COUNT` in `wire.ts`).
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
- `CODE-REVIEW.md` — **coding + review guidelines; bug fixes are test-first
  (failing test before the fix, always). Follow it for every change and
  review.**
- `ROADMAP.md` — **high-level roadmap for post-v0.5 work (R1 multi-broadcaster
  → R6 production UI), with per-item status and design links.**
- `gawk-app` is the folder for the frontend application
- `gawk-server` is the folder for the backend (the Relay server) — Go module,
  see `gawk-server/README.md` for build/run
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
  implemented, manual verify pending),
  `docs/10-production-ui.md` for R6 (production UI: landing/broadcaster/viewer
  surfaces, monochrome design system, J1–J6 chunks; implemented, automated
  gates green, manual browser verify pending),
  `docs/13-observability.md` for R9 (observability & metrics: TCP ops
  endpoint with Prometheus `/metrics` + ServiceMonitor, relay ingress-loss
  window, client funnel stats + both-surface overlays, bottleneck playbook;
  M1–M7 implemented 2026-07-14, manual verify + M8 Grafana pending),
  `docs/14-viewer-render-performance.md` for R10 (viewer render performance:
  Firefox drop/decode-gap diagnosis, rAF-coalesced latest-frame-wins
  rendering + WebGL render sink behind the R8 `RenderSink` seam, nested
  transport-worker split behind a `ViewerTransport` seam, decoder queue
  5→10, P1–P4 chunks; P1–P3 + queue bump implemented 2026-07-14, Firefox
  verify pending).
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
9. Automatic resolution fallback — **implemented + released; software-path
   verify + tuning pending** (R4: encode-queue rejection-ratio detection with
   hysteresis + cooldown, in the pure `media/fallback.ts` `FallbackController`;
   a new **"auto" resolution selection (default)** steps both down and up
   (up-probes with exponential backoff against oscillation) plus
   encoder-error step-down, while **explicit rungs are never auto-stepped** —
   frame drops over overriding the broadcaster; zero server/wire/viewer
   changes — see `docs/09-automatic-fallback.md`). **Real-hardware finding
   (2026-07-13)**: the auto step-down does not fire on the gaming PC's
   hardware encode path — HW encoders drain frames without `encodeQueueSize`
   growing past the `> 2` trigger, so the rejection signal under-fires;
   observed low fps was source-limited (4K capture), a correct no-op. Named
   thresholds in `fallback.ts` are unstarted (no HW backpressure to tune
   against); a hardware-strain signal is a possible deferred follow-up.
10. Production UI — **implemented; automated gates green, manual browser
   verify pending** (R6, `docs/10-production-ui.md`). Three surfaces (landing
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
   the datagram rate and viewer decode load. Manual browser verify pending —
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
13. Observability & metrics — **implemented (M1–M7); manual verify + M8
   (Grafana dashboard) pending** (R9, `docs/13-observability.md`,
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
   detected, all-nullable — Firefox lacks it) + funnel rates (capture →
   post-gate → encoded → **sent** fps; received → decoded → rendered fps),
   stall ages, and a shared sectioned stats overlay on **both** production
   surfaces (same hotkey) with **Copy diagnostics** JSON (rolling ~10 s
   sample window) as the remote-troubleshooting story. The
   symptom→signature **bottleneck playbook** lives in docs/13. Zero
   wire-format changes.
14. Viewer render performance — **P1–P3 + decoder-queue bump implemented
   2026-07-14; Firefox before/after verify pending** (R10,
   `docs/14-viewer-render-performance.md`; UI/pipeline only, zero server/wire
   changes). Diagnosis: the R8 worker ran transport + decode + render on one
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
   measurements.

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
