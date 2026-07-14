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
  message, big-endian) implemented twice — Go (`gawk-server/internal/wire`)
  and TypeScript (`gawk-app/src/transport/wire.ts`) — kept byte-compatible
  by shared golden test vectors.
- Codec is **negotiated**, not fixed: H.264 hardware realtime is the happy
  path, with VP9/VP8 fallbacks probed through a cascade of encoder configs.

## Repository layout

| Path | What |
|------|------|
| `ROADMAP.md` | High-level roadmap for post-v0.5 work (R1–R6), with ordering rationale and per-item scope sketches |
| `gawk-app/` | React SPA (Vite + TypeScript + Zustand). Production surfaces: `#/` (landing/join), `#/broadcast`, `#/view/<id>`; the stats-heavy diagnostics live frozen under `#/debug/*` (`broadcast`/`view`/`loopback`). `deploy/`: Dockerfile + Helm chart |
| `gawk-server/` | Go relay: WebTransport endpoint, pub/sub hub, dev-cert tooling. `deploy/`: Dockerfile + Helm chart. See its [README](gawk-server/README.md) |
| `docs/` | Per-milestone design notes and gotchas (`01`–`14`), plus [`implementation-tasks.md`](docs/implementation-tasks.md) — the server design + task breakdown and current progress |
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

What comes next (R5 viewer live-edge work, …) is laid
out in [`ROADMAP.md`](ROADMAP.md).

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
  Firefox never had it, so the overlays' Network section reads `—`
  everywhere and per-leg attribution leans on relay-side counters. Every
  consumer must feature-detect and treat all fields as nullable
  (`transport/net-stats.ts` — spec-aligned, lights up on re-ship).
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
