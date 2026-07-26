# gawk

Self-hosted, low-latency live game streaming for the browser. A broadcaster
shares their screen; viewers join with a 6-character code and watch at
**sub-500 ms glass-to-glass latency** (≈50 ms measured on a hardware
encode/decode path) — no accounts, no plugins, no native app.

Built on **WebTransport datagrams + WebCodecs** with a custom Go relay that
**scales horizontally**: a self-federating fleet of relay pods carries
hundreds of concurrent broadcasts and ~1,000 viewers on a single hot
broadcast behind one UDP load balancer. Deploy the whole stack — relay +
web app — from Helm charts, on anything from a single node to a Kubernetes
cluster.

WebTransport + WebCodecs is a deliberate choice over the mature WebRTC/SFU
route (OvenMediaEngine et al.): a lower-level path that trades some ready-made
robustness for a leaner, self-owned pipeline and a real exploration of newer
browser transport and codec APIs.

## How it works

```
Broadcaster (Chrome)                relay (Go)                Viewers (Chrome, many)
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
| `ROADMAP.md` | High-level roadmap for post-v0.5 work (R1–R27), with ordering rationale and per-item scope sketches |
| `gawk-app/` | React SPA (Vite + TypeScript + Zustand). Production surfaces: `#/` (landing/join), `#/broadcast`, `#/view/<id>`; the stats-heavy diagnostics live frozen under `#/debug/*` (`broadcast`/`view`/`loopback`). `deploy/`: Dockerfile + Helm chart |
| `docs/` | Per-milestone design notes and gotchas (`01`–`32`), plus [`implementation-tasks.md`](docs/implementation-tasks.md) — the server design + task breakdown and current progress |
| `gawk-server/` | Go relay: WebTransport endpoint, pub/sub hub, dev-cert tooling. `wire/` is public so the native broadcaster can import it. `deploy/`: Dockerfile + Helm chart. See its [README](gawk-server/README.md) |
| `gawk-broadcast/` | Go native Linux broadcaster (R14): Gio GUI + CLI over a shared engine, hardware encode via portal + GStreamer. Own module, own release; no image or chart — a binary you run on your own PC. Also home of `gawk-pubsim` (R20): the engine replaying the committed H.264 fixture as a deterministic synthetic publisher for CI and drills. See its [README](gawk-broadcast/README.md) |
| `BUGS.md` | Known, confirmed, not-yet-fixed bugs (how found, impact, where a fix starts) |
| `e2e/` | R20 browser E2E harness: headless-Chrome viewer against a real relay fed by `gawk-pubsim`, plus the kind cluster config + per-pod assertions for the cluster tier. See [`e2e/README.md`](e2e/README.md) and [docs/25](docs/25-e2e-testing-in-ci.md) |
| `.github/workflows/` | CI (test/lint/build on PR + main; R20 browser E2E on PRs, cluster E2E on release PRs) and release automation (release-please → GHCR images + OCI Helm charts, versions from conventional commits) |

## Quickstart (local dev)

Prerequisites: Go ≥ 1.26 (the toolchain auto-downloads what the module
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

Browser E2E (R20): a real headless-Chrome viewer decoding real relayed
frames runs on every PR (`e2e` job), and a 2-replica cluster-mode variant on
kind runs on release PRs (`e2e-cluster`). Locally: see
[`e2e/README.md`](e2e/README.md).

### Containers & deployment

Both components build to images (`<component>/deploy/Dockerfile`) and ship
as Helm charts (`<component>/deploy/charts/<component>/`) published to GHCR by CI on
release — chart version, `appVersion` and image tag always match. Cluster
deployment is automated cluster-side: whenever a new version is released,
it is rolled out automatically (CI itself stays publish-only —
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
17. ✅ Viewer playback smoothing (R12): jitter measurement at the actual paint (presentation-cadence error, arrival p95−min, decode σ), an opt-in **"Paced playback"** mode (`PacedPresentationSink` holding ≤3 decoded frames to vsync-aligned targets, subsuming the R10 coalescing sink) with a jitter-tracked adaptive playout offset (clamp p95−min+34 ms to [50, 350]), and an experimental opportunistic frame-interpolation scaffold (WebGL2 blend, own default-off toggle). Viewer-only — zero server/wire changes — `docs/17` (T1–T4 implemented 2026-07-15; automated gates green, manual browser verify done 2026-07-19; the R20 `e2e` job also runs the adaptive+interpolated pipeline green on every PR; T5 motion-estimated interpolation + T6 findings not started)
18. 🚧 Advanced broadcaster settings (R13): `isConfigSupported` probe matrix, HW-aware auto ceiling + probe-driven 'auto' framerate default (60 when hardware probes it, else 30), acceleration tri-state, bitrate/codec overrides, probe-annotated pickers, and capture aligned to the sticky selection via live `applyConstraints` — **no settings change ever restarts the stream**. Supersedes R7; UI/pipeline only — zero server/wire changes — `docs/18` (L1–L5 implemented 2026-07-15; automated gates green, manual browser verify pending)
19. ✅ Native Linux broadcaster (R14): a Gio GUI app + CLI over a shared Go engine in the new top-level [`gawk-broadcast/`](gawk-broadcast/) module (importing the relay's now-public `gawk-server/wire` — reused, never mirrored), publishing with **hardware encode** from Linux, which the browser structurally cannot do there. Go-owned XDG ScreenCast portal handshake (the share picker appears on every start — the choice is never persisted) feeding a GStreamer subprocess; hardware-only cascade `vulkanh264enc` → `nvh264enc` → `vah264enc` → **refusal pointing at the browser** (no software rung), each accepted only by real trial encode; MPEG-TS over the pipe for structural AU boundaries; raw Annex-B with empty extradata (the only thing exercising the viewer's Annex-B branch). Zero server/wire/viewer changes — `docs/19` (V0–V7 implemented 2026-07-15; automated gates green including end-to-end tests against the real relay binary; **manual verification on the Linux gaming PC done 2026-07-19** — hardware encode, the latency-bias gate, portal + notifications on KDE/GNOME; not CI-reachable. V8 direct Vulkan Video encode is gated on V2's on-hardware Stage-1 result and not started)
20. 🔧 System audio (R15): Opus via WebCodecs over datagrams — one ~320 B Opus packet per datagram (48 kHz stereo, 128 kbps, 20 ms; no chunking/keyframes), wire types 0x07/0x08 + a hub audio-config cache, viewer decode feeding a main-thread `AudioWorklet` ring buffer, and good-enough A/V sync off the shared capture clock — **video-master** (docs/20 field finding 4): audio is held at start to meet the video presentation schedule, and residual clock drift is absorbed by a sub-audible playback-rate trim, because after playback begins no amount of buffering can change when a sample is heard. **Always on since 2026-07-23** — the experimental toggle is removed, the broadcaster requests system audio on every start, and a browser that can't start a source broadcasts video-only (the refusal is remembered for the page session, so the next start just works); viewer audio controls appear only when audio is received — `docs/20` (designed 2026-07-15, refreshed + **N1–N6 implemented 2026-07-19**; twelve hardware field findings, eleven fixed — finding 12 (an `avSkewMs` metric over-report; audio itself is fine) still open; audio owner-verified playing reliably on real hardware 2026-07-23, the formal verification pass still needs a re-run)
21. ⚠️ iOS native fullscreen (R16): the viewer's fullscreen button is a silent no-op on iPhone (no Element Fullscreen API there — every iOS browser is WebKit; the only native fullscreen is `webkitEnterFullscreen()` on a `<video>`, and the viewer paints a canvas). Attempted fix (**rejected after on-device testing — see status**): a `TeeRenderSink` decorator wraps each **presented** canvas frame (R12 pacing/interpolation preserved) into a worker-side `VideoTrackGenerator` feeding a hidden pre-armed `<video>`; tiered `useFullscreen` (element → video → CSS pseudo-fullscreen) so the button always does something; plus a new stats-overlay **Feature Gates** section (UpperCamelCase names, first gate `NativeVideoFullscreen`). Gated on the *absence* of `Element.requestFullscreen` — non-iPhone devices unchanged (overlay section aside). Viewer-only — zero server/wire changes — `docs/21` (U1–U3 implemented 2026-07-16; **U4 verdict 2026-07-19: native fullscreen still shows a black video on iPhone across three on-device passes → the native tier is rejected**; **superseded by R22 (docs/27), which deleted the tee and re-fed the same hidden `<video>` from an fMP4 muxer + `ManagedMediaSource` — the source type the native player actually presents**)
22. ✅ Relay scale-out & high availability (R17): the relay becomes N homogeneous pods behind the existing UDP LoadBalancer via a **self-federating origin/edge cascade** over the existing WebTransport protocol — the publisher's pod claims a per-broadcast k8s Lease as origin, other pods edge-pull on demand and re-fan-out (depth ≤ 2), so one hot broadcast's audience spans pods. Version rollouts stop breaking streams: drains send a new 4002 close code (clients reconnect with zero delay), a shared QUIC stateless-reset key makes abrupt deaths detectable in ~1 RTT, and the broadcaster auto-resumes with in-band resume tokens (wire 0x09) so relay restarts keep the broadcast ID and viewer URLs — worst rollout artifact ≤ 1 s freeze, vs today’s orphaned streams — `docs/22` (W1–W6 implemented 2026-07-16, automated gates green; kind two-pod smoke automated + green in the R20 `e2e-cluster` CI job 2026-07-18; remaining homelab drills + 200-viewer scale proof owner-accepted 2026-07-19 as CI non-goals)
23. ✅ Live "N watching" count (R18): a relay→publisher push channel plus relay→viewer fan-out reusing the R5 ClockMapping cache/prime template; new wire type 0x0B `TypeViewerCount`. In cluster mode only real viewers count — each edge reports its local count up its `/internal/subscribe` session and the origin aggregates and fans the global total down verbatim (edges excluded). The native GUI rings a "first viewer joined" notification on the 0→1 transition — `docs/23` (Y1–Y6 implemented 2026-07-18; automated gates green; the 2-pod cluster count is asserted in the `e2e-cluster` CI job; single-pod manual verify pending)
24. ✅ Resilient viewer mode for lossy networks (R19): an opt-in per-viewer mode (`?delivery=reliable`) where the relay re-frames the subscriber's deltas as length-prefixed records on per-GOP reliable uni "carrier" streams (QUIC recovers loss; the relay stays a byte forwarder), paired with a wider adaptive playout profile (clamp [150, 2000] ms) so a clean mobile link sits under 1 s behind live instead of freezing on loss. One stream-kind byte 0x0A `TypeReliableCarrier`; zero broadcaster changes — `docs/24` (X2–X5 implemented 2026-07-18, X1/X6 verification 2026-07-19; the carrier path is now covered under injected loss in CI)
25. ✅ E2E testing in CI (R20): a real headless-Chromium viewer decoding real relayed frames as a GitHub Actions gate. Tier 1 (every PR) publishes a committed H.264 fixture through the real native engine via a new `gawk-pubsim` CLI and asserts flow-shaped criteria from the viewer's Copy-diagnostics JSON; Tier 2 (release PRs only) stands up a 2-pod kind cluster and proves the origin/edge split. A headless browser-broadcaster pass drives the production broadcaster via tab capture — `docs/25` (Z1–Z3 + Z5 implemented 2026-07-18/19; both tiers green in real CI; Z4 burn-in → required-check flip pending)
26. 🔧 Relay DVR ring buffer for resilient mode (R21): each broadcast gets a short (2–3 s, `-dvr-window`) relay ring of whole GOPs with a per-subscriber cursor, so a "Deep buffer" viewer rides out a multi-second outage with no freeze and no lost frames — R19 shipped only the viewer-side delay buffer, which relocates the freeze rather than covering the stall. New wire type 0x0C `TypeDeliveryAck`; viewer delivery is now a three-way menu (live / resilient / Deep buffer). Server-side + viewer — `docs/26` (DV1–DV5 implemented 2026-07-23, automated gates green incl. an e2e deep-buffer pass; DV6 on-hardware verification + tuning pending)
27. 🔶 iOS native fullscreen via MSE (R22): the iPhone fullscreen button feeds the (kept) hidden `<video>` from a **worker-side fMP4 muxer over the encoded stream** through `ManagedMediaSource` — upstream of the decoder, which is exactly why it renders where R16's presented-frame MediaStream tee was black (spike-confirmed on a real iPhone 2026-07-23). The inline WebCodecs/canvas path is byte-identical everywhere; the R16 tee/generator code is deleted; H.264-only in v1 (VP broadcasts fall back to CSS pseudo-fullscreen). Since the first on-device pass (2026-07-26) the presentation also carries **audio**: the R15 Opus lane is muxed as a second SourceBuffer (probed with `isTypeSupported` — a refusal keeps the native player video-only), and entering native fullscreen hands the audio over exclusively, silencing the inline AudioWorklet sink. Viewer-only — zero server/wire changes — `docs/27` (MF1–MF4 implemented 2026-07-25; muxer + Opus-track playback CI-proven in Chrome `MediaSource` via `e2e/run.mjs --muxer-check` — a *component* proof: that step drives the muxer/presenter modules directly and never goes through the device gate, which under Chrome keeps every R22 code path off the production viewer entirely, so the gate/arming/worker wiring and all `ManagedMediaSource`-specific behavior are not covered there; **on-device passes 1–3 confirmed real video and audio in native fullscreen** and produced findings 1–6 (infinite duration, the muxed audio track, the AAC tier for iOS, the SourceBuffer ordering rule, the video sample durations and Safari's ES_Descriptor — all fixed; three hole/stall items open). **Pass 4, 2026-07-26**: native fullscreen fell back to CSS pseudo for a whole session with zero appends and zero errors — `ManagedMediaSource` parking was gating the *priming* appends, which cannot work because the system only asks for data once the element has some (finding 7, fixed); the segment sink also no longer disappears across a reconnect, and four new counters say where segments stopped. A further on-device pass is pending)

What comes next (the R11/R13 manual browser verifies; R14's V8 direct Vulkan
Video encode now the gaming-PC verify has passed; the R15 system audio build;
the R22 iPhone on-device verification pass (MF5); and the R20 E2E burn-in →
required-check flip) is laid out in
[`ROADMAP.md`](ROADMAP.md).

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
  (`transport/viewer-transport.ts`, `handleReadLoopEnd`). ([docs/06](docs/06-multi-broadcaster.md))

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

**Cross-browser (Firefox, `docs/11`)**

- **Firefox negotiates a 1024-byte WebTransport datagram MTU, not 1200** — so
  the packetizer reads `wt.datagrams.maxDatagramSize` at runtime rather than
  assume the ~1200-byte ceiling; a datagram over the negotiated limit is
  silently dropped. ([docs/11](docs/11-cross-browser-compatibility.md))
- **Firefox's `VideoEncoder` emits a malformed AVCC record** (bad reserved
  bits + a duplicated NALU-type byte); the viewer repairs it in
  `normalizeAvccExtradata` before configuring the decoder.
  ([docs/11](docs/11-cross-browser-compatibility.md))
- **System audio is Chromium-only, and even there it needs the picker's
  checkbox** — the broadcaster always *requests* system audio (R15 graduated it
  to always-on), but the OS only shares it when the user ticks "Share audio" /
  "Share tab audio" in the share picker; there is no system-audio source on
  macOS/Linux screen shares (a **tab** share carries audio everywhere via
  internal mirroring — the reliable fallback), and Firefox lacks WebCodecs
  `AudioEncoder`/MSTP so it streams video-only. R24 surfaces this **browser-aware
  by feature detection** (`audioLaneSupported()`, never UA sniffing) as
  dismissible pre-start tips + reactive notes, and never gates the start path.
  A note **fires only where audio is achievable** — Firefox is told video-only
  up front, never nagged at runtime. ([docs/30](docs/30-broadcaster-capture-audio-guidance.md))
- **A single-window share is the classic "frozen rectangle"** — it stops
  updating (or was never capturable) when the game goes exclusive-fullscreen or
  is alt-tabbed. Whole-screen is the recommended default for games; R24 nudges
  toward it whenever a `window` capture surface is detected
  (`displaySurface`, an advisory category only — never pipeline config).
  ([docs/30](docs/30-broadcaster-capture-audio-guidance.md))

**Media pipeline**

- **A canvas player gets no free display wake lock — take one explicitly.**
  Browsers keep the screen awake only while an `HTMLMediaElement` plays video.
  Both gawk surfaces paint canvases instead (the viewer's WebCodecs → WebGL
  sink; the broadcaster's screen capture), so the OS sees an idle page and runs
  its normal idle timer: on macOS the display dims a few minutes in and then
  sleeps, mid-stream, **fullscreen included** — fullscreen has never controlled
  display sleep, so there is no fullscreen flag to look for. Both screens hold a
  `navigator.wakeLock` screen lock while live (`lib/useWakeLock.ts`). The trap
  when touching it: the UA **auto-releases the lock whenever the document is
  hidden and never re-acquires it**, so it must be re-requested on
  `visibilitychange` or one tab switch loses it for the rest of the stream.
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
- **An adaptive buffer can only be as deep as its estimator can measure.**
  R19's resilient playout controller clamps to `[150, 2000] ms`, but the
  arrival-jitter signal driving it came from a **fixed-range histogram**
  (500 ms, values past it clamp into the top bin), so the offset could never
  exceed **~534 ms** — the advertised upper half was dead, and the design doc
  had explicitly reasoned that the tracker "needs no changes". It survived
  design, implementation and a manual verification pass because every test
  fed the *controller* a synthetic jitter number instead of measuring one.
  The histogram's range and window are now part of `PlayoutProfile`, with a
  test asserting `quantileRangeMs >= maxMs` per profile. When a control law
  and its estimator are tuned in different files, check the estimator's
  dynamic range covers the law's envelope.
  ([docs/24](docs/24-viewer-network-resilience.md))
- **A right-click menu is not a UI on a phone** — and a button that opens a
  popup needs two things jsdom will never tell you about. R19's "Resilient
  mode (mobile networks)" toggle lived only in the viewer's context menu,
  i.e. the feature built for phones was unreachable on phones; the fix is a
  visible "⋮" button in the control bar opening the *same* menu. Building it
  surfaced two browser-only traps. (1) **The opener is inside the
  outside-click handler's world**: the menu closes on any outside
  `pointerdown`, including the button's own, so the ensuing `click` re-opens
  what the pointerdown just closed and the button can never dismiss its own
  menu. A "was it open at pointerdown?" guard *passes in jsdom* — `fireEvent`
  runs inside `act`, which defers the flush — and fails in Chrome, which
  runs a microtask checkpoint between listeners. The menu now takes an
  `anchorRef` and excludes it from "outside": no race to time. (2)
  **Measuring an element mid-animation measures the animation**:
  `getBoundingClientRect()` on the menu returned its `scale(0.97)` opening
  frame, 3 % small, enough to drift it onto the button — and a fixed
  element's shrink-to-fit width depends on where you put it, so measuring at
  the requested position returns a squeezed size that then feeds the
  placement math. Measure the layout box (`offsetWidth`/`offsetHeight`) from
  a neutral corner, hidden until placed.
  ([docs/24](docs/24-viewer-network-resilience.md))
- **A silently-dead publisher session holds its slot for up to the QUIC idle
  timeout (~30 s), and rejecting the broadcaster's own reclaim in that window
  killed live broadcasts.** The reclaim got 409 from its own zombie session,
  clients fell back to minting a new ID (the browser can't tell 409 from
  404), and the old broadcast — with every viewer — was GC'd mid-stream.
  Since 2026-07-18 a resume-token-bearing claim that completes its upgrade
  **supersedes** the incumbent instead ("newest publisher wins" — the
  same-pod counterpart of R17's lease force-take; deposed session gets close
  code 4004, `CloseCodePublisherSuperseded`); a malformed request that fails
  the upgrade still deposes nothing.
  ([docs/06](docs/06-multi-broadcaster.md))
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
- **Every worker has its own `performance.timeOrigin` (its creation moment) —
  a `performance.now()` reading is meaningless in another worker.** Field bug
  (2026-07-19): the TimeSync offset is measured inside the *nested transport
  worker*, but capture→render latency applied it to the *viewer worker's*
  `now()`. Identical origins on first connect masked it; any mid-view
  reconnect (resilient-mode toggle, 4002 rollout drain, auto-reconnect)
  spawns a fresh transport worker minutes after the viewer worker, and the
  latency row inflated by exactly that age gap (~3 min after ~3 min of
  watching). `TimeSyncStats` now carries `timeOriginMs` and consumers rebase
  onto the sample's clock domain before applying the offset. When timing data
  crosses a worker boundary, ship its `timeOrigin` with it.
  ([docs/15](docs/15-viewer-live-edge.md))
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
- **An AVCC length prefix can be byte-identical to an Annex-B start code** —
  a 4-byte length prefix for any 256–511-byte NAL is `00 00 01 xx`, so a
  per-frame "does it start with a start code?" sniff will misparse real AVCC
  frames (the muxer fixture's frame 16 is exactly that). Decide the wire
  format at keyframes — avcC-bearing config ⇒ AVCC, leading start code with
  in-band SPS/PPS ⇒ Annex-B — and keep it sticky for the deltas.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **An fMP4 sample's declared duration must be the interval to its SUCCESSOR,
  not from its predecessor** — otherwise every cadence increase leaves a hole
  exactly that big in the buffered timeline, and a live stream's cadence is never
  constant (a reorder-gap resync discards a GOP and jumps ~33 ms → ~500 ms). It
  costs one frame of lookahead. Constant-cadence fixtures cannot catch this: the
  forward and backward intervals are equal there, which is why it shipped twice.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **An MSE SourceBuffer that never receives its init segment can block the whole
  MediaSource** — Chromium's demuxer waits for every added buffer's init segment
  before reporting metadata, so video appended alongside an unfed audio buffer
  never becomes playable and `play()` returns a promise that can never resolve
  (this hung a CI run for six minutes; `play()` resolves only when playback
  *begins*, so always race it against a timeout in a test harness). Other
  implementations treat a track-less buffer as absent and play video regardless —
  so a watchdog for this must act on the observable symptom (video appended, still
  no metadata and no buffered range), never on the proxy "the other track is
  quiet", or it destroys working presentations on the forgiving implementations.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **All of an MSE MediaSource's SourceBuffers must be added before the first init
  segment is appended** — Chromium's demuxer accepts new IDs only while
  initializing and throws `QuotaExceededError` ("reached the limit of SourceBuffer
  objects") afterwards. So create the buffer from its **mime** as soon as the
  track is known, and withhold only the init *segment* until you have bytes for
  it; a track's buffer existing and a track's init being appended are two
  different things worth tracking separately.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **`ManagedMediaSource.streaming` must never gate the *first* appends** — MMS
  sets it when the element needs more data, and an element that has never been
  given an init segment needs nothing (`buffered` empty, `readyState` 0). Park
  priming behind it and neither side moves: the source stays open, no append is
  ever attempted, no error is ever raised, and on iPhone the native fullscreen
  tier silently disappears for the whole session because `webkitEnterFullscreen`
  requires `HAVE_METADATA`. Prime through the park; pace only a fed element.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **A segment stream whose init segment is emitted once per session cannot
  tolerate a sink that comes and goes** — the R22 worker muxer survives
  reconnects and never re-emits its init, so unregistering the main-thread sink
  on a status change (a reconnect) can lose that one segment, after which every
  media segment is dropped for want of it. Silently, permanently. Tie such a
  sink's lifetime to its consumer, not to connection state.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **iOS refuses Opus in MP4 through ManagedMediaSource** — measured on iOS 18.7 /
  Safari 26.5.2: `isTypeSupported('audio/mp4; codecs="opus"')` is false, even
  though WebKit 17 added Opus-in-MP4 for file playback and Chrome's MSE accepts
  it. AAC-LC (`mp4a.40.2`), the codec Apple's own HLS mandates, is the fallback —
  which for a WebCodecs pipeline means transcoding the decoded PCM.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **A live MSE presentation must set `duration = Infinity`** — otherwise MSE
  keeps raising duration to the newest appended end timestamp, and two things
  break: the native player draws a finite scrub bar instead of the LIVE badge,
  and "the playhead reached the buffered end" becomes indistinguishable from
  "the media ended". **WebKit resolves that by pausing and firing `ended` on an
  MSE buffer underrun, where Chromium stalls and resumes** — so on iPhone every
  hiccup killed playback until the user tapped play, and no Chrome-based CI
  could see it. Set it in the `sourceopen` handler: the setter throws unless
  readyState is `open` with no SourceBuffer updating.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **`HTMLMediaElement.buffered` is the INTERSECTION of the SourceBuffers'
  ranges** — so with a muxed audio track, a hole in audio is a hole in playback
  and stalls the *video* the native player is showing. Hence one packet of
  lookahead in the audio muxer (a sample's declared duration must be the real
  interval to its successor, which also lets a lost datagram be absorbed by
  stretching the preceding sample over the gap), and hence the audio
  SourceBuffer is created on the first audio *sample*, never when its config
  arrives — an audio track with no samples empties the intersection.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **WebCodecs is `[SecureContext]`, and `about:blank` is not one** — an e2e page
  loaded via `about:blank` has `AudioEncoder`/`VideoEncoder` undefined (while
  non-gated APIs like `AudioData` and `MediaSource` are present, so it looks
  like a codec-support problem rather than an origin problem). Serve the page
  from a `localhost` origin — the `--muxer-check` harness fulfills one
  intercepted request in memory rather than starting a server.
  ([docs/27](docs/27-ios-mse-fullscreen.md))
- **iPhone has no Element Fullscreen API** — `Element.requestFullscreen`
  ships on iPadOS (16.4+) but not iPhone, in every iOS browser (all WebKit),
  so an optional-chained call is a silent no-op there. The only native
  fullscreen is `HTMLVideoElement.webkitEnterFullscreen()`: it needs a user
  gesture and a video with loaded media (hence the pre-armed hidden
  `<video>` — since R22 fed by a worker-side fMP4 muxer through
  `ManagedMediaSource`, after R16's MediaStream tee proved the native player
  renders locally generated tracks black), it does **not** fire
  `fullscreenchange` (track `webkitbeginfullscreen`/`webkitendfullscreen`
  instead), and **`display: none` on the video breaks it** — hide by
  size/position. ([docs/21](docs/21-ios-video-fullscreen.md))
- **One pipeline, one pacing schedule.** The reorder buffer's release gate and
  the paced sink's display target must derive from the *same* baseline. R15
  moved the display target onto the audio playhead and left the release gate
  on the arrival baseline; the difference (audio's jitter buffer + output
  latency) landed every frame in `PacedPresentationSink` long before its slot,
  and the sink drops the **oldest** past `MAX_HELD_FRAMES` — so it discarded
  each frame just as it came due. Total video freeze, the instant audio
  started. Video is now the master outright: both ends read the arrival
  baseline, av-sync exports no video-side lever at all, and audio aligns to
  video. ([docs/20](docs/20-system-audio.md) field findings 2 + 4)
- **Audio alignment is a start-time decision, not a buffering one.** The
  AudioWorklet consumes exactly `sampleRate` samples per second at 1×, so once
  playback has begun, holding chunks longer changes queue *depth* and nothing
  else — it cannot change when a given sample is heard. Lip sync is therefore
  set by *when playback starts* (hold the first chunk until the video
  presentation schedule says it is due), and everything after it is a **rate**
  problem: drift is absorbed by a sub-audible ±0.4% playback trim, never a
  step or a skip. ([docs/20](docs/20-system-audio.md) field finding 4)
- **A drop policy and a concealment policy can cancel each other out.** R15's
  audio buffer dropped an incoming chunk on overflow *without* advancing its
  expected-timestamp cursor, so the run of drops came back as a hole — which
  the gap branch then filled with exactly as much synthesized silence,
  re-adding the depth the drop existed to shed. Overflow-dropping could
  therefore never lower the depth, only convert audio into silence: on Safari
  the buffer latched at the ceiling and served **74 % overflow drops against
  zero packet loss** (received == decoded), ~25 % real audio and ~75 % silence.
  Two symptoms that cannot both be true — overflow drops climbing *and*
  underruns climbing — are the signature. ([docs/20](docs/20-system-audio.md)
  field finding 8)
- **A depth estimate credited down at 4 Hz is stale by 250 ms** — more than the
  200 ms of overflow slack it was being compared against, so a healthy
  real-time producer feeding a real-time sink cleared the ceiling near the end
  of every report window and dropped audio ~46×/s. Extrapolate a known-rate
  drain between reports. ([docs/20](docs/20-system-audio.md) field finding 8)
- **Don't shadow a queue you don't own — make its owner report it.** R15's
  audio buffer maintained its own count of what the AudioWorklet held, from
  deliveries and drain deltas. A shadow cannot audit itself, and it drifted for
  three different reasons across two findings (undelivered chunks while the node
  booted; an assumed context sample rate; a suspended context that stopped
  reporting), each time latching the buffer above its overflow ceiling with no
  way back. The worklet now reports its own depth in **content ms** (each
  chunk's `frameCount / sampleRate`, so the context's rate cannot enter the
  accounting), and a cumulative counter on each side reconciles the chunks
  in flight when the report was generated.
  ([docs/20](docs/20-system-audio.md) field findings 7 + 8)
- **An AudioContext does not have to run at the rate you asked for** — macOS
  hands back the device rate (44.1 kHz is routine) for 48 kHz content, and a
  browser may reject the `sampleRate` option outright. Playing one source sample
  per output frame there is 8.8 % slow and a semitone low, *and* under-drains
  the queue by 8 %/s. Always read `ctx.sampleRate` back; the worklet's read rate
  is `content ÷ context`, with the drift trim multiplying it (the fractional
  resampler is already there for the trim — the bug is hardcoding its base
  to 1). ([docs/20](docs/20-system-audio.md) field finding 8)
- **A jitter-buffer target that is only a ceiling is not a jitter buffer** —
  R15's audio buffer enforced its 40–150 ms target on overflow but forwarded
  every chunk to the worklet on arrival, so the sink played at ~0 ms depth and
  any jitter ran it dry (zero is a reflecting barrier: underruns discard time
  that can never be won back). The target must be a **floor**: prime to it
  before playing, rebuild it after a genuine dry spell.
  ([docs/20](docs/20-system-audio.md) field finding 3)
- **`getDisplayMedia` audio is all-or-nothing, not best-effort** — if the
  browser cannot start a system-audio source it rejects the **whole**
  request with `NotReadableError: Could not start audio source`, taking video
  down with it; it does *not* degrade to a video-only grant. Two independent
  failure classes, and the exception is identical for both: **platform** (no
  loopback path at all — Linux and macOS screen/window shares) and **device
  state** (Windows *can* capture system audio, and still fails when the
  default output endpoint won't open for loopback — exclusive-mode holders,
  disconnected/asleep endpoints, some virtual devices). **Tab audio uses
  Chrome's internal mirroring path, not OS loopback, so it works where system
  audio doesn't** — it's the discriminator when triaging, and the fallback
  when capturing. `capture.ts` retries once without audio, but that retry
  needs its own transient activation, which the seconds spent in the picker
  have usually already spent — so a failed first start is the common landing
  spot. Since audio became unconditional (2026-07-23) the refusal is
  **remembered for the rest of the page session**, so simply starting again
  broadcasts video-only; a reload retries audio, because the device-state
  class of failure is transient.
  ([docs/20](docs/20-system-audio.md) field finding 1 + Graduation)
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
- **The viewer count is relay-pushed and eventually-consistent (~1–2 s)** —
  since R18 (2026-07-18) both broadcasters and every viewer show a live
  "N watching" figure over a new relay-originated `ViewerCount` datagram
  (0x0B). It is only ever trusted from a relay peer: a broadcaster-sent
  count is dropped (spoof guard), and in cluster mode edges report their
  local count up while the origin fans the global total down — edge
  sessions themselves are never counted.
  ([docs/23](docs/23-live-viewer-count.md))
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

**Relay fleet (R17)**

- **Every `/publish/{id}` claim needs a resume token** (wire 0x09, the
  `resume` query param) — including reclaims that used to work bare. An old
  client's tokenless reclaim gets 403 and falls back to minting; that's the
  designed hijack fix, not a bug. The native broadcaster persists the token
  as `lastResumeToken` beside `lastBroadcastId`.
  ([docs/22](docs/22-relay-scale-out.md))
- **Server uni-stream accept order is NOT open order.** The relay sends the
  announce (0x03) and the resume token (0x09) on two uni streams;
  webtransport-go delivers them to `AcceptUniStream` in nondeterministic
  order — the native engine saw the token first in ~half of dials, and its
  old "read the first stream, strict-parse as announce" turned that into
  "no broadcast code". Dispatch server messages by wire type, never by
  arrival order. ([docs/22](docs/22-relay-scale-out.md) finding 9)
- **The hijack fix needs an explicit `resumeTokenKey` to hold between
  broadcasters** — a token key derived from the broadcaster-distributed
  publish secret is computable by every broadcaster, so it only stops
  non-secret-holders. Set the independent server-side key (it wins over the
  secret derivation; `resume_token_key_mode=explicit-key` in the startup
  log confirms). ([docs/22](docs/22-relay-scale-out.md))
- **Fleet-shared Secrets are load-bearing**: without a shared resume-token
  key (or publish secret), re-homing 403s on every pod except the minting
  one; without a shared `statsKey`, one broadcast has N metric identities;
  without the shared `StatelessResetKey`, abrupt pod deaths cost the ~30 s
  idle timeout instead of ~1 RTT. Check `resume_token_key_mode` in the
  startup log. ([docs/22](docs/22-relay-scale-out.md))
- **Drain is close-first-while-Ready, never "unready then linger"** —
  kube-proxy flushes UDP conntrack on endpoint removal, so an
  established flow can be re-DNAT'd mid-stream at an unspecified time; the
  4002 close must go out while the conntrack entries still point at the
  draining pod. Don't re-derive this. ([docs/22](docs/22-relay-scale-out.md))
- **`replicas > 1` requires `config.clusterMode: true`** — the chart
  refuses to render otherwise (two pods without federation split the
  publisher from the subscribers). ([docs/22](docs/22-relay-scale-out.md))
- **The ClockMapping is rewritten per hop, never forwarded verbatim** across
  an edge pull — each pod's monotonic clock has an arbitrary epoch, so a
  forwarded mapping would corrupt viewers' capture→render latency by that
  epoch difference. ([docs/22](docs/22-relay-scale-out.md))

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
- **Edge pods verify the internal origin dial against the system root
  pool** — the R17 edge dialer deliberately never sets
  `InsecureSkipVerify`, so on a cluster without cert-manager (the R20 kind
  E2E, any self-signed install) the origin→edge pull fails TLS unless the
  pods trust the cert: set `SSL_CERT_FILE=/tls/tls.crt` via the chart's
  `extraEnv` (Go then uses the mounted self-signed cert as its root pool;
  its SANs must cover `config.internalServerName`).
  ([docs/25](docs/25-e2e-testing-in-ci.md))
- **`gawk-loadgen`'s `gaps` counter has a structural baseline** — keyframes
  ride reliable streams (R8), so the datagram frameID sequence skips one ID
  per GOP: a healthy stream reads (keyframes/s × viewers) gaps/s. Only
  growth beyond that baseline is loss/reorder.
  ([docs/25](docs/25-e2e-testing-in-ci.md))

## License

Proprietary — copyright (c) 2026 Juho Kuusisto, all rights reserved; see
[LICENSE](LICENSE). Not open source (may be relicensed in the future).
Outside contributions are not accepted without an explicit license grant —
relicensing must remain a sole-copyright-holder decision.
