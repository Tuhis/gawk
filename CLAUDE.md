# Project: Self-Hosted Low-Latency Game Stream

## Purpose & Context
Self-hosted, low-latency live game-streaming platform: a broadcaster shares
their screen and viewers watch in the browser at sub-500ms glass-to-glass
latency (≈50ms measured on a hardware encode/decode path). Join-by-code UX
(no accounts, plugins, or native app), multiple simultaneous broadcasters,
and a custom Go relay that **scales horizontally** — a self-federating fleet
of relay pods carries hundreds of concurrent broadcasts and ~1000 viewers on
a single hot broadcast behind one UDP load balancer. Ships as Helm charts +
GHCR images, deployable from a single node to a Kubernetes cluster; the
reference deployment runs on a homelab with a 1gbps symmetric uplink.

WebTransport + WebCodecs is a deliberate choice over a mature WebRTC/SFU path
(e.g. OvenMediaEngine): a lower-level, self-owned pipeline chosen partly as a
genuine exploration of newer browser transport/codec APIs. That exploration
stays an explicit success criterion — a working low-latency stream *and* a
rewarding technical deep-dive — but it is not a licence to cut product corners.

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
- **Session-end reasons are cause-recovered** (`internal/transport/endreason.go`,
  2026-07-27): quic-go cancels the request *stream*'s context before the
  connection's, and http3 links the two with a plain `context.AfterFunc`, so
  every abrupt death used to log the useless `reason: "context canceled"` —
  idle timeout, stateless reset and peer CONNECTION_CLOSE all identical, which
  is why the docs/19 incident could not be diagnosed. The server now keeps the
  QUIC connection's context via `http3.Server.ConnContext` and logs
  `context.Cause` from it (with a bounded wait, because the stream always wins
  that race); a reason that already says something is never overwritten.
- **A token-bearing reclaim supersedes an active publisher session**
  ("newest publisher wins", since 2026-07-18 — docs/06 revision): a
  silently-dead publisher session holds its slot until the ~30s QUIC idle
  timeout, and the old 409 there made the broadcaster's own reclaim fall
  back to minting a new ID, orphaning every viewer (the old broadcast was
  GC'd mid-stream — seen in production logs). `Registry.TakeOverPublish`
  deposes the incumbent (close code **4004 `CloseCodePublisherSuperseded`**,
  non-flapping: neither broadcaster client auto-reconnects) — the same-pod
  counterpart of R17 W3's lease force-take — but only **after** the R17
  resume-token gate passes AND the claiming session completes its upgrade,
  so tokenless or malformed requests can't kill a healthy broadcast; 403
  (no token) and 404 stay pre-upgrade rejections.
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
- `BUGS.md` — known, confirmed, not-yet-fixed bugs. Nine open entries (plus a
  lint-hygiene note) as of 2026-07-27: two Safari viewer stalls (keyframe
  delivery stops; a dead-session freeze that now recovers but whose WebKit
  root cause is still unknown), a misleading "Streamer offline" join-reject
  card, a broadcaster stuck on LIVE after a silent worker death (the viewer
  `avSkewMs` metric's over-reporting on long/stressed sessions, finding 12
  below, was root-caused and fixed 2026-07-26 — no longer a BUGS.md entry),
  and three **iPhone-native-fullscreen-only** entries from the
  first R22 on-device pass (docs/27 findings 3–5, diagnosed by code reading:
  the ~100 ms MSE cushion that structurally cannot grow, no stall watchdog on
  the presentation element, and MMS parking manufacturing buffered holes while
  armed — though the holes actually *measured* on device turned out to be the
  video sample-duration bug instead, now fixed; that entry carries the
  correction), plus two more from on-device pass 2: resilient/Deep-buffer viewers
  losing video **entirely** to the known Safari stream wedge (all video rides
  streams in those modes; audio rides datagrams in *resilient* mode so it freezes
  with sound, but R21 DV5 puts a **Deep-buffer** viewer's audio on its own
  carrier stream too — there the wedge is a total blackout, corrected 2026-07-26
  from a second capture, which also shipped the viewer `checkMediaStall`
  watchdog and cut `DefaultDVRProgressTimeout` 30 s → 6 s so the freeze recovers
  in ~6 s instead of 31), and
  the MSE presentation buffering 20+ s of 7 MP media on a phone; plus a recorded
  set of `gawk-broadcast/internal/mpegts`
  lint advisories that are not runtime defects. (The native broadcaster GUI's
  20–30 % idle CPU burn was filed and fixed 2026-07-27 — gio-x's
  `component.TextField` invalidated every frame a field held text; see docs/19
  finding 12.) (The iPhone
  native-fullscreen black video was resolved 2026-07-25 by R22's MSE path —
  docs/27.) (The Chrome 152 `getStats()` entry was root-caused 2026-07-14
  as an upstream API removal, not a gawk bug — no browser ships
  `WebTransport.getStats()` today; see docs/13 D7.) Check it before debugging anything
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
- `gawk-telemetry` is the optional per-session diagnostics service (R28) — the
  **fourth** top-level Go module, importing `gawk-server/wire` through the same
  local `replace` gawk-broadcast uses (for the 0x0D session tokens). Ships an
  image + chart like the relay and app, but is **default off** everywhere:
  without a fleet telemetry key the relay sends no hello and every client
  collects nothing. Two listeners, and the split IS the security posture —
  ingest is public (a same-origin path on the frontend Ingress), while the
  dashboard/read API/MCP live on a listener that is never routed publicly.
  See `gawk-telemetry/README.md`.
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
  implemented 2026-07-15, manual verify done 2026-07-19; T5/T6 not started;
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
  gaming PC done 2026-07-19 (hardware encode/portal/GUI — not CI-reachable);
  V8 gated on V2's on-hardware Vulkan result, not started**),
  `docs/20-system-audio.md` for R15 (system audio, **always on since
  2026-07-23** — shipped experimental/default-off, graduated by removing
  the toggle outright: Opus/WebCodecs over datagrams — one Opus packet per
  datagram, new wire types 0x07/0x08 + hub audio-config cache, viewer-worker
  decode with a main-thread AudioWorklet sink, good-enough A/V sync
  (shared capture clock, adaptive audio jitter buffer, **video-master**
  pacing since 2026-07-20 — docs/20 field finding 4); N1–N6 chunks; designed 2026-07-15, refreshed
  2026-07-19 post-R16–R20; **N1–N6 implemented 2026-07-19, automated gates
  green in all three modules; graduated from experimental 2026-07-23 after
  the owner confirmed reliable playback on real hardware — thirteen field
  findings, all thirteen fixed (finding 13 moved the skew measurement to the
  listener 2026-07-26 and root-caused finding 12 with it);
  the formal docs/20 verification pass still needs a full re-run** — deviations
  in the doc's "Implementation status"),
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
  U1–U4 chunks; **U1–U3 implemented 2026-07-16; U4 verdict 2026-07-19:
  native `webkitEnterFullscreen` still shows a black video on iPhone across
  three on-device passes (the decoded-frame clone tee did not cure it) → per
  the pre-registered U4 criteria the native tier is not viable on iOS WebKit;
  pseudo-fullscreen (CSS) is the shipping path (probe/gate already fall back
  to it) — see docs/21 "U4 findings" + BUGS.md**),
  `docs/22-relay-scale-out.md` for R17 (relay scale-out & high availability:
  N homogeneous relay pods, **self-federating origin/edge cascade over the
  existing WebTransport protocol** — per-broadcast **k8s Lease** origin
  registry with force-take fencing, `/internal/subscribe/{id}` edge pull
  that dials the lease's pod IP only (cascade depth ≤ 2), per-hop
  ClockMapping rewrite (no cluster clock); rollout drains send new close
  code **4002 while the pod is still Ready** (kube-proxy flushes UDP
  conntrack on endpoint removal — never "unready-then-linger"), a **shared
  QUIC `StatelessResetKey`** gives ~1 RTT death detection, clients reconnect
  at 0 ms on 4002, broadcaster auto-resume + **resume tokens** (wire 0x09,
  required for all `/publish/{id}` claims — closes the graced-ID hijack
  **when the explicit fleet `resumeTokenKey` is set**; it wins over the
  publish-secret derivation, which every secret-holder can compute);
  allocations 0x09+/4002/4003 only (0x07/0x08 are R15's); W1–W6
  chunks; **W1–W6 implemented 2026-07-16, automated gates green; kind
  two-pod smoke automated + green in the `e2e-cluster` CI job (2026-07-18,
  docs/25 Z3); remaining homelab drills (rollout/crash/rebind blips,
  conntrack empiricism) + 200-viewer scale proof closed owner-accepted
  2026-07-19 (CI non-goals — kind lacks the physics) — see docs/22 findings,
  incl. the
  deviations (replicas defaults to 1 with a chart guard requiring
  clusterMode for >1; single-pod upgrades are RollingUpdate ≤1 s blips
  now) and the PR #47 post-review fixes (lingered-out edge hubs deleted,
  holder-gated lease Delete, resume-token key precedence)**),
  `docs/23-live-viewer-count.md` for R18 (live "N watching" count for both
  broadcaster and viewers — the `SubscriberCount` deferred from R14: the
  count already exists server-side (`externalSubsLocked`), what's new is a
  **relay→publisher push channel** (hub-held sender + a 1 s registry count
  pump) and relay→viewer fan-out reusing the R5 `ClockMapping`
  cache/prime/invalidate template; new wire type **0x0B `TypeViewerCount`**
  (6 B, uint32); **cluster mode is the crux — only real viewers count, never
  edges**: each edge reports its local viewer count up the existing
  `/internal/subscribe` session (TimeSync-ping precedent), the origin
  aggregates `G = ownExternalSubs + Σ edge reports` (edge sessions are
  `internal`, excluded) and fans `G` down verbatim (no per-hop rewrite —
  counts are pod-independent); storm-proof by the fixed-cadence pump; "first
  viewer joined" derived client-side from the 0→1 transition, no separate
  message (the native GUI rings it at critical urgency, once per broadcast);
  Y1–Y6 chunks; **implemented 2026-07-18 (designed same day), automated
  gates green in all three modules; the 2-pod kind cluster viewer-count check
  is automated in the `e2e-cluster` CI job (2026-07-19, `cluster-assert.sh`:
  origin `viewersGlobal` == Σ per-pod real viewers, edges excluded);
  single-pod browser/native + re-home + storm manual verify still pending —
  deviations recorded in the doc's "Implementation status":
  publisherSend lives on `hub.Publisher` (BindSend), a spoofed well-formed
  count is dropped without counting bad, `#/debug/*` shows no count (frozen
  pages), and the count is trusted only from relay peers**),
  `docs/24-viewer-network-resilience.md` for R19 (resilient viewer mode for
  lossy networks — LTE/5G mobile viewers: opt-in per-subscriber **reliable
  delivery** (`?delivery=reliable`; relay writes delta datagrams as
  length-prefixed verbatim records on per-GOP reliable uni "carrier"
  streams — relay stays a byte forwarder, viewer feeds records into its
  existing datagram pipeline; keyframe streams untouched; rotation at
  keyframe fan-out is the drop point) + an **extended adaptive playout
  profile** (clamp [150, 2000] ms, seed 500) with wider reorder capacity and
  RTT-scale gap patience; one new stream-kind discriminator byte (**0x0A
  `TypeReliableCarrier`** — 0x09 is R17's, 0x07/0x08 are R15's), zero new
  datagram messages, zero broadcaster changes; manual right-click toggle
  first (default off, mode change = deliberate reconnect), auto-detect
  deferred as a suggest-banner sketch; supersedes docs/12 Decision 1 **for
  this opt-in mode only**; docs/23 is R18 (designed 2026-07-18); X1–X6 chunks;
  **X2–X5 implemented 2026-07-18, automated gates green; X1 netem/browser
  baseline + X6 verification done 2026-07-19 (lossy-network behaviour — not
  CI-reachable) — ordering deviation recorded in the
  doc's "Implementation status"**. **Post-review fix 2026-07-22 (docs/24
  finding 8, review finding `PLAYOUT-1`)**: Decision 7's "the existing
  `WindowedQuantileTracker` needs no changes" was wrong — its 500 ms
  histogram range capped measured arrival jitter, so the `[150, 2000]`
  clamp could never exceed **~534 ms** and the mode under-buffered exactly
  the deep stalls it exists for. The histogram's range + window are now
  `PlayoutProfile` fields (resilient 2500 ms / 8 s; PLAYOUT-3 fixed by the
  same change), a large rise steps instead of slewing (`stepUpAboveMs`,
  resilient-only — down is still never stepped), and the min tracker
  deliberately keeps its 60 s window because it is also `releasableAt`'s
  anchor. Mode-off is byte-identical; **X6's headline criterion needs a
  re-run**. **Post-review fix 2026-07-22 (docs/24 finding 9, review finding
  `PRODUCT-2`)**: Decision 9 put the toggle in the right-click menu only, so
  the mode built for phones was unreachable on phones (iOS Safari fires no
  `contextmenu`). The viewer control bar now carries a visible "⋮" **More
  options** button opening the *same* `ContextMenu`; `ContextMenu` gained
  `anchor: 'bottom-right'` (grow up-left, or the bottom-bar viewport clamp
  puts the menu back over the button) and `anchorRef` (exclude the opener
  from outside-click, or its click re-opens what its pointerdown closed — a
  flush-order race jsdom's `act` hides and Chrome loses), plus placement
  measured from `offsetWidth`/`offsetHeight` at a neutral corner (the
  `scale(0.97)` open animation and position-dependent shrink-to-fit both
  corrupt a naive measure); touch-sized menu rows under
  `@media (pointer: coarse)` only. Decision 11's suggest-banner stays
  deferred — its thresholds need X6 re-run against the post-finding-8
  buffer. **Post-review fix 2026-07-22 (docs/24 finding 10, review finding
  `PRODUCT-1`)**: the carrier path had no automated coverage under loss —
  every carrier test ran against in-memory fakes or a zero-loss loopback, so
  a regression degrading carriers back to lossy delivery would ship green.
  Now `gawk-server/internal/transport/resilient_loss_test.go` puts a
  userspace UDP forwarder in front of the relay dropping **15 % of the
  relay→viewer packets** (R19's actual lossy leg; the publisher stays wired
  direct so ingress is clean) with a plain subscriber behind the same
  forwarder as a same-conditions **control**, and asserts across a GOP
  rotation that every relayed delta arrives verbatim with no holes and zero
  `CarrierRecordsDropped` while the control loses 15–17 of 96. Cross-carrier
  order is asserted **by content, never by accept order** (webtransport-go
  does not accept in open order — docs/22 finding 9). `tc netem` is not
  usable in CI (unprivileged containers, no NET_ADMIN) and is not needed for
  a downlink-only model. The R20 tier-1 harness additionally repeats its
  viewer pass with `gawk:resilient-mode` seeded, covering the browser's
  carrier reader + the `?delivery=reliable` negotiation end to end (no loss
  injected there — that is the Go test's job). **Post-review fix 2026-07-22
  (docs/24 finding 11, review finding `PRODUCT-3`)**: the mode's dominant
  failure was invisible to operators — a reliable subscriber's bounded queue
  overflowing in `enqueueLocked` incremented only the generic `dropped`
  counter, so it read as `datagrams_dropped_total{reason="queue_full"}`,
  byte-identical to a routine slow *datagram* viewer, even though on a
  reliable subscriber the hole lands in a stream the viewer trusts as
  in-order and costs it a freeze-to-keyframe. It now has its own
  **`reason="carrier_queue_full"`** slice (+ `carrierQueueOverflow` in
  `/statusz`, per broadcast and per subscriber); the drop still counts in
  `dropped`, so the bucket is a slice of one total and `queue_full` stays
  derived by subtraction (R9) — now minus the carrier slice too, floored at
  zero. `carrierRecordsDropped` is deliberately **not** incremented: these
  deltas die at the queue before becoming records, and the two counters
  answer different questions ("did the carrier fail?" vs "did the drain keep
  up?"). Relay-only, test-first, zero wire/viewer/broadcaster changes.
  **Post-review fix 2026-07-22 (docs/24 finding 12, review finding
  `BACKPRESSURE-2`)**: Decision 5's "reuse `KeyframeWriteTimeout`" as the
  per-record carrier deadline let the mode manufacture a **1 s stall of its
  own** — two GOPs — whenever a peer's stream window closed, because the two
  writes are not comparable work: a keyframe is a large message on its own
  stream written by a goroutine that blocks nobody, while a carrier record is
  written by `drainReliable`, the one goroutine owning that subscriber's
  entire delta path (audio sideband included), so its deadline *is* the freeze
  every delta behind it inherits. The carrier now has its own
  **`CarrierWriteTimeout` = 500 ms** (one GOP — also when the next keyframe
  rotates the carrier and gives a clean resync point), a constant beside the
  eviction thresholds rather than a knob; the deadline is computed **once per
  dequeued record** and shared by the lazy open's prologue write and the
  record write (so it can't stall for 2×); and
  `Options.carrierWriteTimeout()` returns
  `min(CarrierWriteTimeout, KeyframeWriteTimeout)` so an operator's *tighter*
  stall tolerance still wins — patience may shrink, never grow past a GOP.
  The review's alternative half (a `carRotations` re-check before the write)
  was deliberately **not** taken: it can't bound an already-blocked write, and
  dropping the in-hand record on rotation would discard deltas the healthy
  path delivers. Relay-only, test-first, zero wire/viewer/broadcaster
  changes. **Post-review fix 2026-07-22 (docs/24 finding 13, review findings
  `BW-CHARGE` + the `BACKPRESSURE-4` it is paired with)**: `drainReliable`
  charged `consumeBandwidth` at the *top* of the loop — before the rotation,
  `carDead`/`closed` and carrier-open decisions — so every record it then
  dropped had already debited the cap. That cap is one process-wide bucket
  shared by **every broadcast on the pod**, so a dead GOP's tail (all of it,
  after one failed open) was paid for by *unrelated* broadcasts' viewers, in
  bytes that never reached a wire; the mirror image was the 2-byte carrier
  prologue, counted into `egressCarrierBytes` but never charged. Now one
  charge per record, taken **below** the drop decisions, for exactly the
  bytes that record puts on the wire — the record plus the prologue when it
  is the record whose lazy open starts the GOP's carrier (one charge site,
  and an over-cap drop is decided before a stream exists, where `openCarrier`
  would already have written the prologue). Charge and drop accounting share
  one `n`. Deliberately **not** "charge only on a successful write": a cap
  that debits after the bytes are gone can't refuse anything, so the
  datagram path's check-then-send ordering stands and a record whose write
  *fails* stays charged (one record per dead GOP, not the tail). Inert on a
  default fleet — `-max-bandwidth` unset ⇒ no limiter. Relay-only,
  test-first, zero wire/viewer/broadcaster changes. **Post-review fix
  2026-07-22 (docs/24 finding 14, review finding `BACKPRESSURE-3`)**:
  `enqueueLocked`'s bounded queue is shared by both
  drains, and its drop-**newest** overflow policy (shed the newcomer) is right
  for the datagram drain — a live-edge, loss-tolerant viewer whose queue
  already holds fresher frames — but backwards when reused for a reliable
  subscriber. The carrier delivers records in order, so a queue overflow forces
  a keyframe resync either way; drop-newest reliably delivers the *stale*
  backlog and discards the near-live frames, stranding the viewer as far behind
  as the queue is deep (256 records ≈ 4–8 s @ 30–60 fps) at exactly the moment
  the mode should keep it near live. Now, for `s.reliable` only, the overflow
  path evicts the queue's **oldest** record (a non-blocking receive) before
  enqueuing the newcomer, so the queue trends toward live; the datagram path is
  byte-identical. The evict-then-enqueue is race-safe on an invariant already
  in the code — `enqueueLocked` is the sole sender and always runs under
  `registry.mu` (via `fanOutLocked`) while the drain only ever *removes*, so a
  freed head can't be re-filled by anyone else — and the second send stays a
  non-blocking `select` regardless (fan-out must never block under the lock).
  Exactly one datagram is shed per overflow, so finding-11's accounting is
  unchanged (one `dropped` + one `carrierQueueOverflow`, `queue_full` still
  derivable by subtraction). Relay-only, test-first, zero
  wire/viewer/broadcaster changes. **Post-review fix
  2026-07-22 (docs/24 finding 15, review finding `WIRE-1`)**: the carrier
  framing tests round-tripped the 23-byte golden chunk and asserted
  *rejection* of `MaxDatagramSize + 1`, but never carried a record whose
  datagram is exactly **1200 B** — which is not an edge case but the
  **common** one (a full delta chunk is exactly `MaxDatagramSize`), sitting
  on the uint16 length prefix's inclusive boundary; `DecoderConfig` and
  `AudioFrame` already pinned theirs. Test-only guard added to all three
  wire mirrors (the boundary datagram is built as a real full delta chunk,
  the relay test frames a second record behind it, the viewer test splits
  the read on the boundary, and the `04b0` prefix is pinned as hex).
  Verified load-bearing: with `>` flipped to `>=` in `AppendCarrierRecord`
  the whole pre-existing wire suite stays green in every mirror).
  **Post-review fix 2026-07-22 (docs/24 finding 16, review finding
  `LIFECYCLE-2`)**: Decision 7's stored-vs-effective playout split is honored
  by the pipeline (`viewer.ts` reports `stats.interpolation` off
  `getPlayoutMode()`, which resilient mode forces to adaptive) but not by
  `ViewerScreen`'s menu, which gated the "Frame interpolation
  (experimental)" entry on the **stored** mode — so a resilient viewer whose
  stored playout is `'off'`/`'fixed'` had the experimental, most
  GPU-expensive viewer feature running with no control to turn it off, on
  exactly the phones R19 exists for. The entry is now gated on
  `resilientMode || playoutMode === 'adaptive'` — the effective mode computed
  from local state, deliberately *not* from `stats.playoutMode`, which lags a
  toggle by up to a stats tick. Viewer-UI only, test-first, zero
  server/wire/broadcaster changes; no "governed by Resilient mode"
  annotation, because unlike the two pacing entries this toggle still does
  what it says under resilient mode),
  `docs/32-live-edge-interpolation.md` for R27 (frame interpolation in
  live-edge mode: **designed 2026-07-25, revised in owner review through
  2026-07-26, not started**; viewer-client only — zero
  server/wire/broadcaster changes, zero new worker messages. Extends the
  R12 T4 experimental interpolation to `playoutMode 'off'`, where it is
  structurally unreachable today (mid-frames need both endpoints; live-edge
  presents on decode, so N+1 doesn't exist when N paints — ≥1 frame of
  latency is the price of admission). Mechanism (**timestamp-scheduled**,
  owner decision — the first-draft arrival-triggered hold-one is recorded
  rejected in Decision 1: its blends inherited arrival jitter and its hold
  was invisible to the A/V schedule): in live-edge with interpolation
  active the pipeline supplies `displayTargetMs = ts + arrivalBaseline + H`
  — the same schedule shape and anchor adaptive mode uses (the baseline is
  maintained in every mode) — and the **unmodified** T4 paced-sink
  machinery does the rest (slot presentation, early-upload, `midSlotMs`),
  so blends land timestamp-true. **`H` ≈ one median source gap** (~33 ms @
  30 fps) from a pure estimator, hard-capped at `MAX_LIVE_EDGE_HOLD_MS =
  67` and **derived from the frame interval, never from jitter** (frames
  whose jitter exceeds `H` present ASAP; late intervals just aren't
  interpolated — bounded latency with opportunistic smoothness, vs.
  adaptive's jitter-envelope). **Variable game fps (GPU-load swings) is the
  designed-for case**: `H` is a prediction committed one frame ahead, so it
  **slews, never steps** (R12's rate-nudge lesson — video cadence only),
  engage/disengage carries dual thresholds + a dwell (no video-timing
  flapping around the ~40 fps boundary), the median ignores hitches, both
  prediction-error directions degrade gracefully, and blend *placement*
  stays exact under any fps variation (mids come from both frames' actual
  timestamps; `H` only gates availability) — a **dips smoother** engaging
  exactly in the ~20–40 fps judder regime. **Zero when there's nothing to
  buy**: `H = 0` when no mid rAF tick fits (60 on 60 Hz), past the low-fps
  bound, or toggle off — byte-identical then; also capped by a low
  percentile of recent gaps so bursts can't exceed `MAX_HELD_FRAMES` (the
  docs/20 finding-2 shape). **Release stays immediate — the reorder buffer
  is untouched**; the sink is the only holder (≤2 frames). One toggle
  governs both modes — **accepted (owner decision 2026-07-25)**:
  `stats.interpolation` goes capability-based, menu gate reduces to
  `!= null`; paced-off now implies the ≈1-frame hold unless interpolation
  is also unticked. **A/V sync = a fixed ≈16.7 ms audio delay** (owner
  decision 2026-07-26, superseding the schedule-coupled variant recorded
  in Decision 5): applied on the audio-alignment side whenever the
  live-edge interpolation gate is open; `H` **never touches the audio
  schedule**. 16.7 ms is below the trim's 20 ms deadband, so every
  transition (toggle, engage/disengage) is absorbed silently — the
  slew-vs-trim sizing, engage-step absorption and finding-11 coupling all
  deleted. Bounded residuals: ≈ `H` − 16.7 audio lead engaged (~16 ms @
  30 fps, imperceptible; exact on 120/144 Hz where 60 fps engages),
  ≈ 16.7 ms lag disengaged; `avSkewMs` verifies, the constant is the knob.
  Cost visible (new "Interpolation hold" overlay row; `presentation`
  reports `paced-*` with `playoutMode 'off'`; `capToRenderMs` samples at
  decode and excludes the hold — recorded caveat). **Kill criteria
  pre-registered** (interpolated-interval rate < ~50 % on a clean link,
  reads worse than plain live-edge side-by-side, or added latency > 1.5×
  median gap ⇒ remove/confine — documented rejection is valid completion).
  Chunks **LI1–LI4**; LI3's hardware leg + LI4 sequenced after R15's
  pending re-verification (its formal docs/20 pass, not `avSkewMs`
  finding 12, which is fixed)),
  `docs/33-telemetry-and-diagnostics.md` for R28 (advanced diagnostics &
  telemetry: **designed + TM1–TM8 implemented 2026-07-26, TM10 (dip episodes) +
  TM11 (configured target) 2026-07-27, automated gates
  green in all four modules; TM9 (Grafana) dropped by owner scope decision so
  R9 M8 stays open; manual verification pending — deviations in the doc's
  §4.9 "Implementation status"**; pulls the trigger R9's
  Non-goals set — "client→server metrics push… revisit if remote
  troubleshooting of friends' sessions becomes routine". It became routine:
  R15's twelve field findings, R19/R21/R22's cycles all ran on hand-shuttled
  Copy-diagnostics blobs, and open findings elsewhere need *history* that a
  single diagnostics snapshot can't give (the pattern `avSkewMs` finding 12
  needed before it was root-caused 2026-07-26 is why nobody had the data
  earlier). **Adds no measurement layer** — `ViewerStats` (~80 fields),
  `BroadcastStats`, `engine.Stats`, `RegistryStats`/`subscriberDetails` and
  `lib/diagnostics.ts` already exist; R28 is a pipe, a store and a query
  surface. Owner-locked: an **optional `gawk-telemetry` service, files-first**
  (hive-partitioned gzipped NDJSON on a PVC, chart-gated default off);
  **14 days raw + permanent per-session rollups**; **four read surfaces** (MCP
  with `diagnose()`, HTTP JSON, a built-in dashboard, and the Grafana
  dashboard deferred since R9 M8); **always-on collection, zero PII**. The
  load-bearing gap it closes: `/statusz` has `subscriberDetails[].key` and
  **nothing ever tells the client its own key**, so relay-side and
  client-side views of one viewer cannot be joined — TM1 mints a per-session
  token delivered in-band as new wire type **0x0D `TypeTelemetryHello`**
  (35 B: flags + reportInterval + 24-byte token + the **obfuscated**
  broadcast key, so the client never learns an ID it shouldn't send), a
  stateless HMAC on R17's resume-token pattern that lets the service verify
  ingest with no lookup. Decisions worth not re-deriving: collection is
  **out-of-band HTTPS on a same-origin path** of the existing frontend
  Ingress — telemetry must outlive the transport it reports on, and
  same-origin is what makes the `sendBeacon` unload flush work without a
  preflight it can't perform; the **relay is scraped, never pushes** (no
  outbound HTTP client or queue in the process carrying every broadcast's hot
  path — sub-scrape-interval sessions get `relayCoverage: "none"` rather than
  a silent gap, and the relay chart gains an optional *headless* companion
  Service because a ClusterIP would load-balance the per-pod scrape);
  **DuckDB is a query option, not a runtime dependency** (first-class
  endpoints are plain Go over rollups, keeping the module cgo-free);
  **relay numbers anchor, client numbers are testimony** (docs/20 finding 7's
  lesson — a wedged client's own accounting is the least reliable evidence in
  the system and is exactly what it sends — so evidence carries provenance
  and client-only rules cap their confidence); **collection lives on the
  main thread**, zero worker/pipeline/wire changes on the client beyond 0x0D;
  and validation is **strict envelope, tolerant payload, typed rollup** (D15)
  — the envelope is protocol and rejects, but the `stats` objects are data and
  must never reject, because **version skew is permanent, not transient**: a
  closed field list would make shipping a gawk-app with a new field reject
  every batch from updated clients until the service redeploys, and an old
  open tab forever, losing telemetry exactly during a deploy. Known fields are
  typed (wrong type dropped + counted, never fatal — a string `"30"` must not
  enter an fps series), unknown fields survive verbatim, `app.version` **is**
  the schema version, structural bounds hold regardless of types, and the
  coerced/dropped/unknown tally rides the rollup as `schemaAnomalies` so a
  verdict computed over junk is visibly a verdict to distrust.
  The item's point is `diagnose()`: **docs/13's bottleneck playbook as code**,
  returning ranked verdicts + evidence, never raw samples — an MCP surface
  that dumps 80-field series is *worse* than today's copy-paste, so a 32 KB
  default-response ceiling is an acceptance criterion, not a habit. Chunks
  **TM1–TM9**; two of them carry the value and neither is droppable — TM6
  (`diagnose()`) for the machine, and **TM8, a first-class live operator
  dashboard**: one page listing every active broadcast with its broadcaster
  **and** each viewer, anything obviously wrong highlighted before the
  operator clicks anything, severity — not recency — sorting the live group
  with **recently ended broadcasts as a separate recessed group below**
  (their stored verdicts, past-tense labels; the grouping IS the precedence —
  a live `warn` outranks an ended `bad`),
  the **same rule engine** as `diagnose()` over a live window (two
  disagreeing truths about one stream would be worse than no dashboard),
  four states ok/warn/bad/**unknown** with hysteresis, and a hard rule that
  a missing report reads stale/unknown and **never** green. Only the ingest
  path is public; the dashboard + read API sit on a separate non-public
  listener (R9 D1's posture). TM9 (Grafana) is the droppable one — trends
  can wait, a stuttering broadcast cannot),
  `docs/31-quick-start-links.md` for R26 (quick-start broadcast links:
  **designed 2026-07-25, not started**; frontend-only — zero server / wire /
  broadcaster-protocol change, and the cheapest item on the roadmap. A hash
  query grammar on the existing route (`#/broadcast?start=1&res=720&fps=30`)
  plus a **primed one-click surface**, so a bookmark or an embedded link goes
  from cold browser to share picker in **one click** instead of four. The
  load-bearing fact, stated so it is never re-litigated as a bug:
  **`getDisplayMedia` requires transient user activation, so zero clicks is
  impossible** — a fresh document has none and navigation doesn't carry it
  across documents (`media/capture.ts` already records this in the R15
  finding-1 comment explaining why the video-only retry can't re-prompt). What
  the link removes is every *other* click and every decision. Containment is
  the design: the armed surface calls the existing `beginStart` and nothing
  else, so reclaim→mint, `StartError.phase` and the LIVE stage are
  byte-identical, and a bare `#/broadcast` renders exactly as today — R26
  changes what the pre-start card looks like and what the settings store
  holds, nothing at or after the click. The grammar (`res`/`fps`/`hw`/
  `bitrate` in Mbps/`codec`) can express **nothing the settings panel
  cannot**, validated against *imported* vocabularies so no second list can
  drift; invalid params are ignored with a quiet note, never fatal. Owner
  decisions: the **publish secret is never carryable in a link** (a link gets
  bookmarked, synced, pasted and screenshotted — a credential must not inherit
  that lifecycle; secret-required deploys prompt once per device, then armed
  links are one-click); link settings are a **session-only store override**
  that never touches `localStorage` (a 480p quick-share bookmark must not
  rewrite the 1080p gaming default); a **dedicated minimal surface**, not the
  existing card; and **no pre-connect on load** — an abandoned bookmark tab
  would otherwise hold one of the relay's 5 default broadcast slots for the
  5-minute GC grace. R23's terms gate is never bypassed, only repositioned:
  an armed link presents an unmet gate *on load*, and its "Agree & continue"
  click **is** the capture gesture. Named risk: the ~5 s activation window
  already contains **three** async stages before `getDisplayMedia` — the R11
  worker boot + capability gate (`BOOT_TIMEOUT_MS` 2 s, a hard worst case),
  the WebTransport dial, and R13's `refreshMatrix()` probes — so click→picker
  becomes a measured criterion (< 2 s). Lever ranking is recorded rather than
  guessed: **worker boot is the safest to pre-warm** (no relay slot, no
  encoder contexts), the dial cannot move (= pre-connect, rejected), and
  **probing at load is the least attractive** — it is exactly what
  OOM-crashed a tab on 2026-07-15 (`MAX_CONCURRENT_PROBES` = 4 exists because
  every pending `isConfigSupported` holds a real encoder instance), so if
  pre-warmed at all it must be armed-path only.
  Non-goals: zero-click autostart; a **stable bookmarkable broadcast ID**
  (R17 requires a relay-minted resume token for all `/publish/{id}` claims, so
  a client can't choose its own ID, and carrying that token is a publish
  credential — persistent channels stay R1's deferred item); viewer-side link
  params (a join link is a *shared* artifact whose params travel to other
  people's devices — deliberate follow-up). Chunks **QL1–QL6**; QL1 also fixes
  a latent bug — `#/view/<id>?utm_source=…` currently bounces to the landing
  page),
  `docs/29-terms-and-conditions.md` for R23 (terms & conditions / usage terms:
  **designed + TC1–TC5 implemented 2026-07-24, automated gates green (gawk-app
  tsc/765 vitest/oxlint/build + helm lint/render + gawk-broadcast vet/gofmt/app
  tests/GUI build), manual browser verify pending**; frontend-only — zero
  server/wire/broadcaster-protocol change. A `#/terms` hash route + a bundled
  default terms text (R6 chrome), a **one-time *blocking* broadcaster
  acknowledgment before the first transport connect** (versioned
  `gawk:terms-accepted` localStorage key, re-prompts on a `termsVersion` bump;
  **viewers are never gated** — read-only from landing footer / broadcaster
  Settings / the viewer "⋮" menu), and operator customization via the
  **existing runtime `config.js`/ConfigMap** (new `termsVersion` /
  `operatorName` / `operatorContact` / optional `termsUrl` fields) plus a
  full-body override that is a **sanitized HTML fragment** — the one
  security-review line. Owner decisions: **Finnish governing-law one-liner**
  (no forum clause); **no hard age gate / nothing recorded** — text states 18+
  with under-18 parental/guardian consent; **monitoring reserves broad rights
  *and* states current practice** (broad monitor/analyze/record/block/remove
  discretion + a factual "as currently operated, no persistent media
  recording — not a warranty", worded to stay true if R21's DVR ring lands);
  **English only**. The default text is a **protective template written to the
  operator's priorities, NOT legal advice** — the doc and the chart README both
  carry that caveat. The native broadcaster (R14) gets a one-line reference +
  link, not a gate (TC5, droppable)),
  `docs/30-broadcaster-capture-audio-guidance.md` for R24 (broadcaster capture
  & audio guidance: **designed + CG1–CG5 implemented 2026-07-24, automated
  gates green (gawk-app tsc/vitest/oxlint/build), manual browser verify
  pending**; **frontend-only — zero server/wire/broadcaster-protocol/media-
  pipeline change**, ships *words* + a little dismissible reactive UI state.
  Tells a first-time streamer whole-screen-vs-window and how to get audio,
  **browser-aware by feature detection** (`audioLaneSupported()`, never UA
  sniffing) — the load-bearing fact being **audio is Chromium-only in
  practice** (Firefox has neither `AudioEncoder` nor MSTP; system audio has no
  loopback on macOS/Linux screen shares — a **tab** share carries audio
  everywhere). Reconciles the ROADMAP sketch's staleness: R15 graduated the
  audio toggle away (docs/20), so gawk always requests audio and the only user
  step is the picker's "Share audio" checkbox. New pure
  `features/broadcaster/captureGuidance.ts` (the copy + decisions, one home;
  imports `BroadcastStats['audioState']`, re-exports `audioLaneSupported`);
  a **collapsed-by-default pre-start "Sharing tips" `<details>`** (never on the
  start path — no modal, no extra click; the terms gate stays the only
  pre-connect gate); **dismissible reactive live notes** (audio-missing when
  `audioState` is `'no-track'`/`'unavailable'` AND audio is achievable;
  window-share when `displaySurface === 'window'` — an advisory category only,
  never pipeline config), dismissal persisted per `gawk:hint-*` key (a
  conscious "don't nag experienced users" trade — the notes are an onboarding
  aid, not a durable guard rail); a compact settings echo; and an **honest
  stats-overlay audio State** — `BroadcasterStatsOverlay` gained an
  `audioSupported` prop so Firefox reads "Not supported here" instead of the
  misleading "No audio shared". **Gated on the capability, never the
  `audioState` string** — Firefox lands on `'no-track'` (the `!track` branch
  wins before the `'unsupported'` check), which the string can't tell from a
  Chromium unticked box. Adversarial design review (self + independent
  subagent) ran **before** implementation; its must-fix findings (the Firefox
  `'unsupported'` error, the persisted-dismissal one-shot framing, test-map
  holes) are folded into docs/30. Chunks **CG1–CG5** (two-letter prefix; single
  letters A–Z claimed)),
  `docs/28-native-broadcaster-audio.md` for R25 (native broadcaster audio:
  **designed 2026-07-23; NA1 spike done 2026-07-27 on Debian 13/KDE —
  Decisions 2/3/4/5 confirmed on hardware, both design-level risks closed,
  NA2–NA5 unblocked; NA2–NA8 not started**; flips docs/20's "audio in the R14
  native broadcaster" non-goal, which had already written down the shape.
  R15 built the feature and only the *browser* got a producer — wire
  0x07/0x08 live in the **shared Go** `gawk-server/wire`, the relay's
  dispatch/cache/join-prime and the whole viewer path are engine-agnostic and
  shipped, and R15's golden vectors were mirrored into `gawk-broadcast`'s
  `wirecheck` ahead of need (docs/20 deviation 6). So this is a **producer
  change and nothing else**: zero relay, wire, viewer and `gawk-app` changes.
  Design: capture is **outside the portal** (ScreenCast carries no audio —
  don't go looking for a flag) from the **default sink's monitor** via
  PipeWire, which the portal already proves present; a **probed source
  cascade** — `pipewiresrc` with `stream.capture.sink=true` first *because
  WirePlumber follows the default sink*, so a headphone↔speaker switch
  re-routes instead of erroring, then `pulsesrc @DEFAULT_MONITOR@`, then an
  explicit device — and because an audio trial pops no picker it runs
  **before** the portal handshake (the `EnsureBinary` ordering rule); encode
  stays in **GStreamer** (Opus 48 kHz stereo, 20 ms, `dtx=false`,
  `inband-fec=false`, forced `channels=2` — a 5.1 monitor would give
  multistream Opus, undecodable by WebCodecs without the `OpusHead`
  description R15 deliberately leaves empty), because cgo bindings would put
  libopus headers on `gawk-pubsim`'s build path and break the R20 harness's
  reason for existing; **audio muxes into the existing MPEG-TS** (verified:
  `mpegtsmux` takes `audio/x-opus` and is `GstAggregator`-based, so it does
  not hold audio hostage to damage-driven video) so that **one `ptsAnchor`
  serves both media** — the load-bearing sync decision, since one PTS
  timeline through one affine map makes relative A/V skew zero by
  construction, where a second pipe (the pre-registered fallback:
  `rtpopuspay timestamp-offset=0 ! rtpstreampay`) reintroduces a constant
  unmeasurable lip-sync bias; audio is **subordinate** (no source ⇒
  video-only and say so; a live failure naming an audio element ⇒ drop audio,
  retry the same rung) and an **outer** cascade dimension, never per-rung
  (that would be 18 attempts and a minute of startup); an *optional*
  `AudioSource` interface keeps video-only a first-class shape, which is what
  R20 tier-1's standing no-audio assertion needs; and `gawk-pubsim` gains a
  **committed Opus fixture behind a default-off flag** so tier-1 exercises the
  real engine send path while the no-audio pass keeps running. Chunks
  **NA1–NA8**, NA1 an on-hardware spike that confirms or overturns the muxing
  decision before NA3/NA4 are written. **NA1 answered all three of the doc's
  unverified facts (2026-07-27, two runs via a self-contained tester script,
  `gawk-broadcast/scripts/na1-audio-spike.sh` + `NA1-SPIKE.md`, written so a
  non-contributor with a Linux box can run one command and send back one
  tarball)**: (1) the Opus-in-TS control header begins **`7F E0`, not
  `FF E0`** — the mapping spec's 11-bit prefix holds `0x3FF`, which is *ten*
  ones — and GStreamer writes **one access unit per PES** where ffmpeg batches
  five (NA4 keys on one, tolerates more); (2) `pipewiresrc`'s
  `stream.capture.sink` forwarding works, in both the `=true` and
  `=(string)true` spellings; (3) pipewire-pulse resolves `@DEFAULT_MONITOR@`.
  All six probed spellings captured real system audio, and candidate 1
  **followed a mid-recording default-output switch** — Decision 2's headline
  claim and goal criterion 2, met, which narrows Decision 6's mid-session hole.
  The `GstAggregator` risk was measured rather than assumed and Decision 4
  holds: with the video pad starved for **4.6 s**, audio still arrived every
  20.0 ms with zero gaps over 100 ms. The lever is the **declared caps
  framerate**, not actual arrival — a 1-frame-per-5-s *proxy* did burst audio
  into 5 s clumps, because the encoder's frame interval becomes the live
  pipeline's declared latency, so `BuildPipeline` pinning `framerate=<fps>/1`
  (docs/19's rate-control fix) is what keeps audio smooth on a motionless
  screen. NA4's fixture is committed at
  `gawk-broadcast/internal/mpegts/testdata/opus-h264-na1.ts` (263 KB, real
  muxer output, both streams, PMT descriptors, and the first audio PES whose
  `start_trim_flag=1` makes its header 6 bytes where every later one is 4).
  Two doc corrections fell out, both recorded in docs/28's findings rather
  than silently applied — the second is a **retraction**: `fakesink
  num-buffers=25` in Decision 2's trial pipeline is *valid* (`GstFakeSink` has
  its own `num-buffers`), so a pre-spike "fix" to `identity eos-after=25` was
  wrong and is reverted),
  `docs/27-ios-mse-fullscreen.md` for R22 (iOS native fullscreen via MSE:
  **designed 2026-07-23, spike-confirmed on iPhone the same day; MF1–MF4 +
  MF5's observability/docs half implemented 2026-07-25 — automated gates green
  incl. a new `e2e/run.mjs --muxer-check` Chrome `MediaSource` playback step in
  the `e2e` CI job; the R16 tee is deleted and its BUGS.md black-video entry
  closed; MF5's on-device verification pass is pending** (deviations in the
  doc's "Implementation status & deviations" — notably the muxer's
  keyframe-decided sticky wire format: an AVCC 4-byte length prefix for a
  256–511-byte NAL is byte-identical to an Annex-B start code, so a per-frame
  sniff misparses real AVCC frames); supersedes R16's rejected native tier. R16's `VideoTrackGenerator`
  MediaStream tee showed **black** across three on-device passes (docs/21 U4),
  and R16 had rejected MSE up front as survey option C — but a real-iPhone
  spike proved a **`ManagedMediaSource`-backed `<video>` presents real frames**
  under `webkitEnterFullscreen` where a MediaStream is black (the native player
  accepts standard buffered media, not a locally generated track). Design:
  **parallel tee, not primary** (user-confirmed) — the WebCodecs/canvas inline
  path stays byte-identical (sub-500 ms live-edge + R12 pacing/interpolation +
  R5 metrics preserved on iPhone), MSE feeds a *second*, fullscreen-only
  `<video>` so its buffered latency is confined to the native player; a
  worker-side **fMP4 muxer forks the reorder buffer's release stream** (the
  encoded frames feeding the decoder) + `DecoderConfig` — **upstream** of the
  decoder, vs. R16's *downstream* presented-frame tee, which is exactly why MSE
  renders and the tee was black — handling both wire formats (AVCC browser +
  Annex-B native, SPS/PPS + dims from the bitstream), no B-frames ⇒ trivial
  fMP4 timing; **reuses the R16 gate/tiers/`presentationVideo`
  seam/`NativeVideoFullscreen` gate and deletes
  `TeeRenderSink`/generator/MediaStream** (the cleanup R16's U4 verdict left in
  BUGS.md); tier-2's *source* changes MediaStream→MMS and `useFullscreen`
  barely changes; **interpolation retained inline + CSS-pseudo, not in the
  native player** (post-decode synthesized frames aren't encoded — "good to
  have, not must have"); **H.264-only v1** (a VP-codec broadcast probes false
  on iPhone → pseudo); **CI-provable in Chrome `MediaSource`** (R20 tier-1),
  only the iPhone-native presentation stays manual; zero server/wire/broadcaster
  changes. Chunks **MF1–MF5** — two-letter prefix, A–Z claimed),
  `docs/26-relay-dvr-buffer.md` for R21 (relay DVR ring buffer for resilient
  mode: **DV1–DV5 implemented 2026-07-23 (wire type 0x0C `TypeDeliveryAck`,
  5 B; `-dvr-window`/`-dvr-max-bytes`/`-dvr-max-catchup`/`-dvr-audio` flags;
  `internal/hub/dvr*.go`; viewer "Deep buffer" delivery mode) — DV6
  on-hardware verification pending**. R19 shipped the viewer half of
  the latency trade (a deep adaptive playout buffer) but not the relay half,
  so the mode survives loss/jitter on a *connected* link and still freezes on
  an actual outage — the viewer's buffer is made of **delay, not pre-fetched
  data**, so during a stall it needs exactly the frames captured during that
  stall, and the relay destroys those at the 500 ms `CarrierWriteTimeout` and
  again in docs/24 finding 17's dead-GOP purge. **Deepening the viewer buffer
  alone relocates the freeze** to after the outage and adds permanent latency
  — don't re-propose it. Design: **one ring per broadcast holding whole GOPs**
  (a cursor must start at a keyframe) with **a cursor per subscriber** — one
  copy of the bytes, fan-out becomes O(1); a replayed GOP is byte-identical on
  the wire to a live one, so **zero wire changes**; falling off the tail is the
  mode's only frame loss, converting "stalls > 500 ms lose frames" into
  "stalls > ring window lose frames"; **the buffer must strictly exceed the
  stall** — covering `S` with buffer `B` needs `B/(B−S)`× burst bandwidth, so
  2 s of buffer cannot cover a 2 s stall at any rate; per-subscriber catch-up
  ceiling (2×) so one recovering viewer can't take the pod's egress budget;
  audio joins the ring behind its own flag (a gated, measured partial reversal
  of docs/20 field finding 5); health becomes "is the cursor advancing?" or the
  existing eviction thresholds kill healthy laggards; mode off ⇒
  byte-identical, ring allocated lazily. Chunks **DV1–DV6** — every
  single-letter chunk prefix A–Z is claimed, so this and later milestones use
  two letters),
  `docs/25-e2e-testing-in-ci.md` for R20 (E2E testing in CI: a real
  Chromium viewer decoding real relayed frames as a GitHub Actions gate —
  **Tier 1** single-pod browser E2E on every PR (relay `-dev-cert` →
  `serverCertificateHashes`, a new `gawk-pubsim` fixture-publisher CLI
  reusing the native engine, playwright-core + preinstalled system Chrome
  driving the production viewer, flow-shaped assertions on the R9
  Copy-diagnostics JSON via the `clipboard.writeText` stub precedent);
  **Tier 2** cluster-mode E2E **on release-please PRs only** (user
  decision; fresh pinned kind cluster, real chart with 2 replicas +
  clusterMode + NodePort over kind UDP `extraPortMappings` —
  `kubectl port-forward` is TCP-only — origin/edge split proven from
  per-pod `/statusz`/metrics; automates docs/22's pending two-pod smoke);
  private-repo CI minutes (2,000 free/month, 2-core no-GPU runners) as a
  design constraint — concurrency cancel-in-progress, timeout caps, no
  push-to-main runs, advisory burn-in before checks become required;
  browser-broadcaster tier was a droppable Z5 stretch with kill criteria;
  Z1–Z5 chunks (`Y` is R18's — claimed by the concurrent docs/23 design);
  **Z1 done + Z2/Z3 implemented 2026-07-18, Z5 implemented 2026-07-19** —
  spike verdicts: hash-pinned
  WebTransport works in headless Chrome as-is, and the browser broadcaster
  works headless **via tab capture** (`run.mjs --browser-broadcast`, a
  second tier-1 step: `--auto-select-tab-capture-source-by-title` against
  a harness-owned animated tab — headless *screen* capture grants but
  delivers black frames; worker offload falls back to main-thread
  headlessly, recorded not asserted); `gawk-pubsim` lives in
  `gawk-broadcast/cmd/` (fixture embedded via the new `internal/fixture`
  package), the harness in top-level `e2e/`; test-the-test passed locally
  for Z2 and Z5,
  and the tier-2 flow was rehearsed on a local kind cluster (= docs/22's
  two-pod smoke, incl. the `SSL_CERT_FILE=/tls/tls.crt` edge-TLS-trust
  finding); **both tiers now green in real CI** — tier-1 `e2e` on every PR,
  `e2e-cluster` on the 2026-07-18 release PRs (Z3 acceptance met; origin/edge
  split + browser viewer asserted); Z4 burn-in → required flip pending (plus
  a re-run on the new self-hosted `ioio-k8s` runners, migrated 2026-07-19
  after those GitHub-hosted green runs, and the Z5 step's first CI run)).
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
   gates green, manual browser verify done 2026-07-19; T5 (motion-estimated
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
   setting — `'off' | 'fixed' (the R5 150 ms mode) | 'adaptive'` —
   persisted as `gawk:playout-mode`; **since 2026-07-23 (docs/17 Decision
   10, user decision) viewer pacing is ONE binary right-click toggle,
   "Paced playback"**: adaptive dominates fixed at every point on the
   trade curve (clamp floor 50 ms is *below* fixed's constant on a clean
   link, ceiling above it on a dirty one, warmup seeds at the same 150 ms,
   and only adaptive sets a `displayTargetMs` — so fixed paid the
   buffering latency while presenting unpaced). `'fixed'` survives as a
   mode, offered by `ViewerScreen` only under `isDevEnvironment()`
   ("Smooth playback (fixed 150 ms)"), because a measurement-free offset
   is the control that separates a pacing bug from a bug in the jitter
   estimator driving it (PLAYOUT-1, docs/24 finding 8); stored `'fixed'`
   and the legacy boolean's `'1'` both migrate to `'adaptive'`, legacy
   `'0'` still to `'off'`. In adaptive mode the reorder buffer releases at
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
   gates green (both Go modules), manual verify on the gaming PC done
   2026-07-19 (hardware encode/portal/GUI — not CI-reachable); V8 not started
   — it is hard-gated on V2's on-hardware Vulkan result**
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
   picker; the GUI hand-draws **every** widget including its text fields —
   `gioui.org/x` is not a dependency any more, because
   `component.TextField` executed an `op.InvalidateCmd` on every frame a
   field held text and burned 20–30 % CPU **while idle** from launch
   (docs/19 finding 12, 2026-07-27; the pre-filled settings are what armed
   it, and Decision 9's disabled panel is why it vanished during a
   broadcast). `cmd/gawk-broadcast-gui/main_test.go` holds the line: an idle
   window must schedule **no** already-due wakeups, asserted over the whole
   window rather than one widget; **notifications via `godbus` with critical urgency for
   failures** — KDE's portal **inhibits normal notifications while screen
   casting**, so only critical-urgency ones reach a fullscreen broadcaster;
   **no viewer count / "first viewer joined"** was an R14 non-goal (nothing
   on the wire told a publisher about subscribers then; a `SubscriberCount`
   message was named as a future wire+relay change, not an R14 smuggle-in) —
   **delivered 2026-07-18 by R18 (docs/23)**: the engine now surfaces the
   relay's `ViewerCount` push (`Stats.ViewerCount` + `OnViewerCount`) and
   the GUI shows "N watching" and rings the first-viewer notification at
   critical urgency, once per broadcast, on the 0→≥1 transition.
   **Tray and global hotkeys deferred 2026-07-15** — research kept in the
   doc's Deferred section; don't re-derive it. Not a
   container/chart/CI-deploy component — binaries you run on your own PC.
   **Auto-resume added 2026-07-27 (docs/19 Decision 21), after a production
   incident**: R14 shipped without the transport auto-resume the browser has
   had since R17 W2, and on 2026-07-27 broadcast `DE6G6P` died 78 minutes in
   and was GC'd with three viewers attached because nothing reclaimed the ID
   inside the grace. Three defects compounded: `finish()` fired `OnEnded`
   **without `teardown()`**, so the GStreamer child, the portal ScreenCast
   session and the pump all kept running forever (the machine still showed
   the screen being shared); `App.ended()` nulls `a.sess`, so `Stop()`/`Quit()`
   could not stop what was orphaned; and the relay-loss path set no `lastErr`,
   so the "Broadcast ended" notification went out at **normal** urgency — which
   KDE's portal inhibits *while casting*, i.e. exactly then (Decision 17's
   trap). Now: `finish()` **is** teardown + `OnEnded` (one event, one
   teardown); a recoverable loss reclaims the code transport-only (capture,
   encoder, frameId space, derived config and learned chunk budget all
   survive) with 250 ms→5 s backoff over a 5-minute window; new
   `OnResuming`/`OnResumed` callbacks drive an amber "Reconnecting…" heartbeat
   instead of a green "Live" or a spurious "Ready"; HTTP 404/401/403/409 stop
   the retry; **close codes 4000 and 4004 are never resumed** — "newest
   publisher wins" only converges because the deposed publisher stays down, so
   an engine reclaiming after a 4004 would flap forever (the code is read back
   through `OpenUniStream()`, the one place webtransport-go keeps the cause it
   discards from the session context); a reclaim is **never** a mint; and the
   TimeSync estimate + ClockMapping publisher are **reset on every resume**
   because the relay's reference is its own *process* monotonic clock, so a
   reclaim landing on another pod measures a different origin. New
   `Stats.Resumes`/`Stats.Resuming`. The incident's *trigger* is still
   unproven — see BUGS.md's fleet-LB entry.
20. System audio — **designed 2026-07-15 (N1–N6); refreshed 2026-07-19
   against R16–R20; N1–N6 implemented 2026-07-19, automated gates green;
   thirteen hardware field findings — findings 1–8 detailed below, plus 9
   (`avSkewMs` measured buffering depth + estimator lag, not lip-sync —
   fixed by measuring at presentation and snapping the mapping on a
   re-anchor), 10 (a stale flush left Deep-buffer audio ~2.8 s behind), 11
   (toggling paced playback left audio behind), 12
   (`avSkewMs` over-reports on long/stressed sessions — a metric
   artifact; audio itself is fine), and 13 (audio settled ~`outputLatency`
   behind the picture while `avSkewMs` read zero — the skew was measured at
   the worklet's write position instead of the speaker, and the broadcaster
   anchored audio timestamps at encoder output; measuring at the listener
   2026-07-26 fixed both 13 and 12, see docs/20 field findings 12 + 13);
   all thirteen fixed 2026-07-19→07-26, and the
   owner reports audio playing reliably on real hardware as of 2026-07-23 —
   which is what graduated it from experimental (below), though the formal
   docs/20 verification pass reached only step 3 of 9 (2026-07-24) and still
   needs a full re-run** (R15,
   `docs/20-system-audio.md`). Shipped **experimental, default-off** behind
   an "Enable audio (experimental)" toggle; **graduated 2026-07-23 (user
   decision, docs/20 "Graduation") by removing that toggle** — the
   production broadcaster now requests system audio on every start
   (`BROADCASTER_CAPTURE_CONFIG` in `BroadcasterScreen`), the store field
   and its `gawk.audioEnabled` key are gone, and the frozen `#/debug/*`
   surfaces keep plain `DEFAULT_CAPTURE_CONFIG` (audio absent). Because the
   toggle was also field finding 1's only escape hatch, `capture.ts` now
   **remembers an audio-source refusal for the page session** — the first
   start can still die on `NotReadableError`, the next asks video-only and
   broadcasts; not persisted, so a reload retries audio. The viewer still
   shows audio controls (mute/volume, overlay Audio section) **only when
   audio is actually received**. Direction (settled over reliable-stream Opus, raw
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
   video clone. **Audio is decided at broadcast start, never mid-stream** —
   the one R13 live-apply exception, forced by `getDisplayMedia` (an audio
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
   **As implemented (2026-07-19)**: `gawk-app/src/media/audio-lane.ts`
   (broadcaster: anchor → `AudioEncoder` → datagrams + 1 Hz config),
   `transport/audio-decode.ts` (viewer decode → planar PCM),
   `transport/audio-buffer.ts` (the live-edge ring-buffer policies, pure),
   `transport/av-sync.ts` (skew + the drift-trim controller, module state
   like `playout.ts`), `features/viewer/audioSink.ts` (AudioContext +
   AudioWorklet + GainNode). Deviations worth knowing before touching it:
   decoded audio crosses the worker boundary as **planar Float32 buffers,
   not a transferred `AudioData`** (the worklet needs planar channels, so
   the copy happens worker-side); the worklet processor is a **source
   string → Blob URL** (`addModule` needs a URL, and `?url` on a `.ts` file
   would serve raw TypeScript); the buffer's timeline-change threshold is
   **asymmetric** (backwards >1 s = restart, forwards >5 s = reconnect — a
   symmetric bound late-dropped forever after a short-session restart); and
   the audio buffer's profile re-seed is keyed on **profile identity, not
   value range** (the default and resilient envelopes overlap at 150 ms).
   **Field finding 1 (2026-07-19, docs/20)**: the first
   real-hardware attempt never reached the audio code — `getDisplayMedia`
   audio is **all-or-nothing in Chromium**: when the browser can't start a
   system-audio source it rejects the *whole* request with
   `NotReadableError: Could not start audio source` and the broadcast dies in
   phase `'capture'`. Two failure classes, one identical exception —
   **platform** (no loopback at all: Linux, macOS screen/window shares) and
   **device state** (Windows supports it and still failed on the gaming PC —
   exclusive-mode/asleep/virtual default output endpoint, or a window
   selected; root cause there **not yet identified**, triage list in
   docs/20). **Tab audio bypasses OS loopback** (internal mirroring), so it
   both discriminates during triage and is the way to verify R15 today.
   `acquireDisplayStream` now retries once without audio on `NotReadableError`
   only (never on `NotAllowedError` — a cancelled picker must not re-prompt),
   reports `audioState: 'unavailable'` (distinct from `'no-track'`, which
   means the user unchecked the box), and when the retry has no transient
   activation left throws the original audio cause plus the way out.
   **Field findings 2 + 3 (2026-07-20, docs/20)** — first time audio actually
   played (a standard-audio Windows machine; finding 1's box had a virtual
   audio-device stack): video froze almost completely and audio broke up
   constantly, two independent seam bugs. **(2)** N5 put the *display target*
   on the audio playhead but left the reorder buffer's *release gate* on the
   arrival baseline — the gap between the two schedules (audio's jitter buffer
   + output latency) delivered every frame to `PacedPresentationSink` long
   before its slot, and the sink drops the **oldest** past
   `MAX_HELD_FRAMES: 3`, so it discarded each frame as it came due →
   total freeze. Fixed by `av-sync.audioBaselineMs()` (audio analogue of
   `arrivalBaselineMs()`), read by **both** the release gate and the display
   target — one schedule per pipeline; live-edge mode unaffected (offset 0).
   Accepted: in paced modes, audio-on now costs the audio path's depth in
   video latency. **(3)** `AudioJitterBuffer` enforced its 40–150 ms target
   only as an overflow *ceiling* and forwarded every chunk on arrival, so the
   worklet played at ~0 ms depth and any jitter ran it dry; the target is now
   a **floor** (prime to it, re-arm on a still-dry underrun report,
   concealment silence through the same gate or it overtakes the audio it
   fills behind). Both fixed test-first. **Field finding 4 (2026-07-20,
   owner decision)** inverts Decision 10: **video is the master clock and is
   never rescheduled for audio** — audio is the medium with slack (one Opus
   packet per datagram, no reassembly/keyframe wait, so it arrives materially
   earlier than video, which also pays the playout offset). av-sync no longer
   exports any video-side lever (a test pins that absence). The
   load-bearing constraint: the worklet consumes exactly sampleRate samples/s
   at 1×, so **after playback starts, buffering can no longer change when a
   sample is heard** — alignment is a start-time decision, drift is a rate
   problem. So: (a) `AudioJitterBuffer` holds the first chunk until the video
   presentation schedule (`ViewerStats.videoScheduleBaseEpochMs`, rebased per
   context) says it is due, less `AudioContext.outputLatency` — that hold
   becomes the sink's depth for the session, which also retires finding 3's
   underruns; (b) `AudioRateController` absorbs clock/soundcard drift
   (~100 ms/hour) with a sub-audible rate trim (±0.4%, slewed 0.0008/s,
   20 ms deadband, give-up at 2 s) applied by a fractional-read resampler in
   the worklet. Fallbacks: no schedule ⇒ depth floor; schedule never fires ⇒
   released after `MAX_ALIGNMENT_HOLD_MS`; underrun re-prime ⇒ depth floor,
   not a past schedule. `avMaster` now reads 'video' | 'free';
   `alignmentHoldMs` is the lip-sync diagnostic. **Field finding 5
   (2026-07-20, docs/20)** reverses the carrier-routing half of Decision 12:
   audio broke up in resilient mode because R19 delivered it as records on the
   per-GOP reliable carrier (head-of-line blocking behind video deltas +
   GOP-clumped tail drops — worse than concealed single-packet datagram loss).
   `drainReliable` now routes `TypeAudioFrame`/`TypeAudioConfig` to the
   unreliable datagram path (`sendSidebandDatagram`); only video deltas ride
   the carrier. Relay-only, test-first, zero wire/broadcaster/viewer changes;
   the profile-carrying half of Decision 12 stands (audio still adopts the
   resilient depth envelope, aligning to the deep video playhead per finding
   4). **Field finding 6 (2026-07-21, docs/20)**: live-edge (`playoutMode`
   off) audio was near-silent because finding 4's video-master alignment
   gives the worklet ~0 buffer depth when video presents on arrival, so
   normal arrival jitter (72–158 ms) starved it (severity tracked jitter;
   `audioPacketsDecoded` stayed ~50/s, proving it was not loss — which also
   validated finding 5's carrier fix as safe). The adaptive jitter target is
   now a **depth floor in every mode** (`audio-buffer.ts` releases only when
   the video schedule is due AND `bufferedMs ≥ targetMs`); paced modes
   unchanged, live-edge gets a ~90–150 ms cushion at the cost of audio
   sitting a jitter-depth behind video. `avSkewMs` (~3–5 s) was a
   frozen-playhead artifact of the underruns. **Field finding 7 (2026-07-21,
   docs/20)**: with 1–6 fixed, audio started then degraded to short cracks
   within ~10 s and, on a later session, cut out entirely.
   `AudioJitterBuffer.queuedMs` (a shadow of the worklet's real queue depth)
   counted chunks `AudioSink.forward()` silently dropped while the AudioWorklet
   node was still booting (async `addModule`) or its port threw — permanently
   inflating `bufferedMs` above the overflow ceiling, so `push()` spuriously
   overflow-dropped ~39/50 incoming packets (crackle) and, once the worklet
   stopped draining (Safari suspends the AudioContext), froze the estimate above
   the ceiling and dropped every packet forever (total silence, no recovery).
   Fixed test-first, viewer only: (a) **honest accounting** — `emit` signals
   delivery, `queuedMs`/`establishedDepthMs` count only delivered audio; (b) a
   **sink-ready release gate** so the alignment cushion is never released into a
   null node; (c) **stall recovery** — no worklet playhead report for >1 s while
   audio arrives ⇒ resume a suspended context + flush buffer/worklet to
   re-anchor at the live edge; plus a `resets` (recoveries) overlay row and a
   hardened `flush()`. **Field finding 8 (2026-07-23, docs/20)**: on Safari
   audio broke up continuously while video was fine — `overflowDrops` at
   **74 % of arrivals** with `audioPacketsReceived == audioPacketsDecoded`
   (zero wire loss) and `bufferedMs` pinned at the ceiling. Two policies
   cancelled each other: `push()` counted an overflow drop *before* advancing
   `nextExpectedUs`, so the run of drops returned as a hole, and the gap
   branch concealed it with exactly as much silence — through `emitChunk`, so
   the silence re-added the depth the drop was meant to shed. Overflow-
   dropping could not lower the depth at all, only convert audio into silence
   (~25 % real audio, ~75 % synthesized), and once over the ceiling it never
   came back; `noteUnderrun`'s `queuedMs === 0` guard discarded the one signal
   that proved the estimate wrong. Two things pushed it over: the ceiling was
   compared against an estimate credited down only at the worklet's ~4 Hz
   report (stale by 250 ms vs. 200 ms of slack — 46 spurious drops/s on a
   healthy real-time feed), and any single ~200 ms hiccup latched it. Fixed
   viewer-only, test-first, each mechanism mutation-verified: an overflow drop
   advances the cursor (a skip toward live — deliberately not charged to the
   lead budget, since overflow means audio is running *late*); shedding is
   hysteretic down to `max(target, establishedDepth)` (parked at the ceiling,
   input rate == drain rate keeps it there, dropping at the margin forever);
   concealment is gated on a **100 ms accumulated-lead budget** (owner
   decision — below it the skip is inaudible and av-sync's rate trim absorbs
   it, above it one concealment repays the whole debt); the depth estimate is
   extrapolated at 1× between reports (capped at 500 ms so a suspended context
   can't decay a real backlog); and an underrun clamps the estimate to what
   the closing window delivered. New `gapsSkipped` counter + split overlay
   rows — the concealment/overflow *ratio* is what identified this. Sample
   rates deliberately untouched (`ctx.sampleRate` is still never read back);
   and the same change closes the root both findings share: the buffer no
   longer *shadows* the worklet's queue at all. The worklet reports its own
   depth in **content ms** (`frameCount / sampleRate` per chunk) in the report
   it already sent at 4 Hz, `notePlayed(delta)` becomes `noteDepth(absolute)`,
   and cumulative counters on both sides (`receivedMs` / `deliveredTotalMs`,
   neither reset on flush) reconcile chunks in flight when a report was
   generated — which retired the underrun clamp. **Sample rate, same change**:
   `build()` never read back `ctx.sampleRate`, and macOS/Safari routinely hands
   back a 44.1 kHz context for 48 kHz Opus — 8.8 % slow, a semitone low, and an
   8 %/s under-drain that walks any inferred depth to the ceiling by itself. The
   worklet's base read rate is now `chunk.sampleRate / contextRate` × trim (the
   fractional resampler the trim already needed — its base was simply hardcoded
   to 1), the context rate is read back with a fallback when the option is
   *refused* (which used to take the whole stream video-only), and no
   main-thread code converts frames to ms any more. New overlay rows: **Sink
   rate** (annotated `(resampling)`) and **Gaps filled / skipped**.
   **Owner verdict 2026-07-23: with all findings fixed, audio plays
   reliably on real hardware** — the basis for the graduation above; the
   formal docs/20 verification-plan pass is still not re-run.
21. iOS native fullscreen — **U1–U3 implemented 2026-07-16 (automated gates
   green); U4 verdict 2026-07-19: the native path still does not work on
   iPhone — `webkitEnterFullscreen` enters but shows a black video across
   three on-device passes (the decoded-frame clone tee did not cure it) →
   per the pre-registered U4 criteria the native tier is not viable on iOS
   WebKit, and pseudo-fullscreen (CSS) is the shipping path (the probe/gate
   already fall back to it)**
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
   from a black native player. **Third pass 2026-07-19: still black ⇒
   native tier rejected, pseudo-fullscreen ships** — the pre-registered
   verdict in docs/21 "U4 findings" (high sample + still black ⇒ the native
   player can't present locally generated MediaStreams ⇒ remove tier 2, ship
   pseudo). BUGS.md entry tracks it.
22. Relay scale-out & high availability — **W1–W6 implemented 2026-07-16
   (automated gates green; kind two-pod smoke automated + green in the
   `e2e-cluster` CI job 2026-07-18; remaining homelab drills
   (rollout/crash/rebind, conntrack) + 200-viewer scale proof closed
   owner-accepted 2026-07-19 as CI non-goals — docs/22 "Implementation
   status & findings")**
   (R17, `docs/22-relay-scale-out.md`). Product prep: N homogeneous relay
   pods behind the existing UDP LoadBalancer;
   **self-federating origin/edge cascade over the existing wire protocol**
   (chosen over a NATS backplane — runner-up, revisit-if triggers recorded —
   and over per-broadcast sharding, which caps a hot broadcast at one pod;
   targets: hundreds of broadcasts, 500–1k viewers on a hot one, single
   region, no new stateful infra). Two layers. **Rollout resilience first**
   (W1–W2, valuable at replicas:1): SIGTERM drain sends new non-terminal
   close code **4002 while the pod is still Ready** — kube-proxy flushes
   UDP conntrack on endpoint removal, so "unready-then-linger" draining is
   wrong, don't re-derive it; a **shared QUIC `StatelessResetKey`** (absent
   before W1) is what makes abrupt deaths detectable in ~1 RTT instead of
   the ~30 s idle timeout; clients reconnect at 0 ms on 4002 / ≤250 ms on
   abrupt errors (the old 1 s backoff floor ate the whole blip budget);
   **broadcaster auto-resume** (capture + encoder kept, transport-only
   reconnect, forced keyframe on re-attach via a new `KeyframeCadence`
   hook — config already rides every keyframe stream); **resume tokens**
   (HMAC over the ID, keyed by the explicit fleet resume-token key when set
   — it **wins** over the HKDF-from-publish-secret fallback, which every
   secret-holder can compute (PR #47 review) — in-band as new wire
   type **0x09** — browsers can't read response headers) required for
   **all** `/publish/{id}` claims incl. unknown-ID claims which now create
   the hub — this is what lets relay restarts keep broadcast IDs, and with
   the explicit key it closes the old graced-ID hijack. **Resume vs restart is frameID
   continuity — no server-side epoch** (`generation` never leaves the
   process; R10's gap/backwards-keyframe machinery covers both). Then
   **federation** (W3–W5, dormant behind `-cluster-mode`, default off ⇒
   byte-identical single-pod behavior): per-broadcast **k8s Lease** (holder
   + pod-IP addr + originGeneration, CAS force-take — the
   broadcaster-in-hand resume token makes re-homing event-driven, TTL only
   covers crashes; leaderless janitor; lease deletion = cluster-wide 4000);
   `/internal/subscribe/{id}` edge pull (PSK + generation params, HTTP
   status rejections — the dialer is Go; **edges dial the lease's pod IP,
   never the Service VIP** — with generation fencing that bounds cascade
   depth ≤ 2); edge prime caches **invalidated on upstream loss** (stale
   prime + new-origin deltas structurally impossible); origin
   **self-demotes to edge** on lease loss (edges closed with **4003**);
   per-hop ClockMapping rewrite via a Go TimeSync-estimator port (**no
   cluster clock**). Fleet plumbing (W5–W6): shared statsKey Secret (one
   obfuscated metrics identity per broadcast across pods), origin/edge role
   labels + separate edge-leg ingress-loss family, per-IP limiter
   **trusted-CIDR bypass** (etp=Cluster SNAT means rollout reconnect herds
   hit the 3/s bucket; **etp=Local was rejected only under MetalLB L2** —
   the homelab turned out to run **BGP mode**, where Local is the
   recommended setting: real client IPs + ECMP spread, exposed via chart
   `service.externalTrafficPolicy` + `podAntiAffinity` since 2026-07-18,
   docs/22 finding 10), chart flip to replicas ≥ 2 +
   RollingUpdate maxSurge 1/maxUnavailable 0 + PDB + drain-aware `/readyz`.
   Allocations: **0x09+/4002/4003 only** (0x07/0x08 reserved by R15);
   version skew during rolling updates makes internal-protocol changes
   skew-tolerance-mandatory. Non-goals: zero-blip (QUIC session handoff not
   implementable on quic-go, unneeded at ≤1 s), crash RTO ≤ ~15 s
   best-effort, geo edges, HPA, MoQ. **Rebase onto R14 (2026-07-18)
   adapted the native broadcaster in-PR**: server uni-stream accept order
   is NOT open order (webtransport-go — the token beat the announce in
   ~half of dials, docs/22 finding 9), so the engine dispatches server
   messages by wire type and persists the resume token as
   `lastResumeToken` for reclaim.
23. Resilient viewer mode for lossy networks — **X2–X5 implemented
   2026-07-18, automated gates green (both Go modules + app); X1 netem
   baseline/browser spike + X6 verification done 2026-07-19 (real-cellular
   behaviour stays owner-verified, but since 2026-07-22 the carrier path
   **is** covered under injected loss in CI — docs/24 finding 10) — a
   recorded ordering
   deviation, see docs/24 "Implementation status"** (R19,
   `docs/24-viewer-network-resilience.md`;
   `docs/23` is R18 live viewer count, designed 2026-07-18). Opt-in per-viewer
   mode for LTE/5G mobile viewers: smooth video at 0.5–2 s behind live
   instead of freeze-until-keyframe stutter under packet loss. Mechanism
   (user decision over buffer-only, relay-cache+NACK, and FEC): the relay
   delivers the opted-in subscriber's deltas as length-prefixed verbatim
   datagram records on **per-GOP reliable uni carrier streams** (QUIC
   retransmission recovers loss; relay stays a byte forwarder — no frame
   reassembly; existing keyframe-stream, supersede and 4001-eviction
   machinery untouched — carrier opens feed the same eviction streak;
   carrier rotation at keyframe fan-out = the drop
   point, drops-over-stalls at GOP granularity — a stalled record write is
   deadline-cancelled and the GOP tail dropped; egress cap charged per
   record), negotiated via `?delivery=reliable` (publish-secret
   query-param precedent), paired with a resilient viewer profile:
   `PlayoutController` clamp **[150, 2000] ms** seed 500, reorder capacity
   64→256, RTT-scale gap patience (250 ms) — same adaptive formula, wider
   profile (`transport/resilient.ts` + profile-carrying `PlayoutController`),
   so a clean mobile link sits well under 1 s. Zero broadcaster changes
   (the lossy leg is relay→viewer); the stream-kind discriminator is
   **0x0A `TypeReliableCarrier`** + record framing (`uint16 len ‖ datagram`)
   golden-vectored in all three wire mirrors; the viewer's uni-stream
   reader (`readServerStreams`) dispatches by the two-byte prologue and
   feeds records into the existing datagram path; zero new relay knobs
   (KeyframeWriteTimeout/QueueDepth/caps reused). Graceful degradation
   against an old relay (buffer-only; overlay reports "reliable requested /
   datagrams served"). **R17
   interop**: reliable conversion happens at the subscriber's serving pod;
   origin→edge stays datagram-based, the param never propagates upstream.
   Manual toggle first ("Resilient mode (mobile networks)", persisted
   `gawk:resilient-mode`, default off; mode change = deliberate reconnect;
   while on, the paced/smooth entries are annotated as governed by it and
   the stored playout mode survives untouched). **Since R21 (docs/26) this is
   a three-way delivery menu — live / resilient / "Deep buffer" — persisted
   as `gawk:viewer-delivery`; the old `gawk:resilient-mode` key is now read
   only to migrate existing viewers.** Auto-detect deferred
   as a suggest-banner design sketch. Supersedes docs/12 Decision 1 for
   this opt-in mode only; default mode keeps datagrams. Observability:
   overlay Delivery-mode + carrier rows, `/statusz`
   `subscriberDetails.reliable` + carrier counters, Prometheus
   `gawk_broadcast_reliable_subscribers` + `carrier_*_total` +
   `egress_bytes_total{kind="carrier"}`, docs/13 playbook row.
24. E2E testing in CI — **Z1 done + Z2/Z3 implemented 2026-07-18, Z5
   implemented 2026-07-19; both
   tiers now green in real CI (tier-1 `e2e` on every PR; `e2e-cluster` on
   the 2026-07-18 release PRs — Z3 acceptance met); Z4 burn-in → required
   flip pending, plus a re-run on the new self-hosted `ioio-k8s` runners
   (migrated 2026-07-19 after those GitHub-hosted green runs) and the Z5
   step's first CI run; `Y` is R18's, claimed by the concurrent docs/23
   design)**
   (R20, `docs/25-e2e-testing-in-ci.md`). GitHub Actions proof that
   streaming works before a release ships: **Tier 1** on every PR runs
   the real relay (`-dev-cert`), publishes the committed H.264 fixture
   through the real native engine via a new **`gawk-pubsim`** CLI
   (`relay_integration_test.go`'s fixture source as a standalone tool —
   no Gio imports, so no cgo/apt headers; also makes the manual
   `gawk-loadgen` drills self-contained), and drives the **production
   viewer in headless system Chromium** (playwright-core, the
   `gawk-app:verify` recipes), asserting **flow-shaped** criteria from
   the R9 Copy-diagnostics JSON captured via a `clipboard.writeText`
   stub — never fps ceilings or latency numbers (2-core no-GPU runners);
   since 2026-07-22 the tier-1 step repeats that viewer pass once with
   **R19 resilient mode** seeded (docs/25 finding 16), covering the
   browser's carrier reader + the `?delivery=reliable` negotiation.
   **Tier 2** runs **only on release-please PRs** (user decision;
   `workflow_dispatch` escape hatch): fresh pinned kind cluster, real
   chart with `replicaCount=2` + `config.clusterMode=true` +
   `service.type=NodePort` over kind UDP `extraPortMappings`
   (`kubectl port-forward` is TCP-only and cannot carry WebTransport),
   pubsim + ~12 loadgen sessions spreading across both pods, origin/edge
   split asserted from per-pod `/statusz`/metrics — the automated
   successor to docs/22's pending two-pod smoke (homelab drills and the
   200-viewer scale proof stay manual). Private-repo minutes are a
   design constraint (2,000 free/month): concurrency
   cancel-in-progress, `timeout-minutes` caps, no push-to-main runs
   (release PRs re-check every merged change), measured budget with a
   trigger-narrowing-never-assertion-thinning fallback; both tiers land
   advisory and flip to required after a flake-free burn-in.
   Chromium-only v1 (Firefox deferred with a revisit-if); the
   browser-broadcaster tier (getDisplayMedia automation) was a
   pre-registered droppable stretch (Z5) — **implemented 2026-07-19, no
   kill needed**: a second tier-1 step (`run.mjs --browser-broadcast`)
   drives the production broadcaster surface headlessly with the capture
   picker auto-granted by `--auto-select-tab-capture-source-by-title`
   against a harness-owned animated tab — **tab capture, never screen
   capture, which grants but delivers solid black frames headlessly**;
   the encode funnel (capture/encode/sent + keyframe streams, software
   `avc1.*`) is asserted from the broadcaster's own Copy-diagnostics
   JSON, and the R11 worker offload is recorded not asserted (headless
   Chrome exposes no worker MSTP and refuses track transfer → main-thread
   fallback engages by design; docs/25 findings 12–15). Z1's spike
   confirmed the one load-bearing unknown:
   **hash-pinned WebTransport works in headless Chrome as-is** (no SPKI
   flag, no Xvfb — docs/25 findings).
25. Relay DVR ring buffer for resilient mode — **DV1–DV5 implemented
   2026-07-23; DV6 on-hardware verification + tuning pending** (R21,
   `docs/26-relay-dvr-buffer.md`; server-side + a viewer "Deep buffer"
   delivery mode). Gives each broadcast a short (2–3 s, `-dvr-window`) relay
   ring of whole GOPs with a per-subscriber cursor, so a resilient/deep
   viewer rides out a multi-second outage with no freeze and no lost frames
   (R19 shipped only the viewer-side delay buffer, which relocates the freeze
   rather than covering the stall). New wire type **0x0C `TypeDeliveryAck`**
   (5 B) acks the negotiated delivery mode; knobs `-dvr-window` (3s),
   `-dvr-max-bytes` (24 MiB), `-dvr-max-catchup` (4×), `-dvr-audio` (on) are
   plumbed through `registryOptions`; a DVR subscriber is excluded from the
   LIVE keyframe fan-out (docs/26 field finding 1). Viewer delivery is now a
   three-way menu (live / resilient / Deep buffer) persisted as
   `gawk:viewer-delivery`. Automated gates green incl. an e2e deep-buffer
   viewer pass; on-hardware tuning is the remaining DV6 work.
26. iOS native fullscreen via MSE — **MF1–MF4 + MF5's observability/docs half
   implemented 2026-07-25; two on-device passes 2026-07-26 CONFIRMED the design
   (real video AND audio in native fullscreen). Pass 1: 1 (no
   `duration = Infinity`: no LIVE badge, and WebKit pauses + fires `ended` on an
   MSE underrun where Chromium stalls and resumes, so every hiccup killed
   playback until a manual tap) and 2 (audio muxed as a second SourceBuffer with
   an exclusive inline-sink/native-player handoff) — both FIXED. Pass 2: **iOS
   18.7 refuses `audio/mp4; codecs="opus"` through ManagedMediaSource**
   (measured), so the probe gained a second tier — **AAC-LC transcoded from the
   decoded PCM in the worker** (`transport/audio-transcode.ts` + an `mp4a`/`esds`
   init segment carrying the encoder's own AudioSpecificConfig), CI-proven in
   Chrome by a forced-AAC `--muxer-check` pass. Finding 3 REVISED: the buffered
   holes measured on device were **not** MMS parking (zero presenter-side drops —
   `segmentsAppended == muxInitSegments + muxMediaSegments`) but the **video
   sample-duration** scheme, which declared each sample's duration as the interval
   from its PREDECESSOR, so every cadence increase (a reorder-gap resync jumps
   ~33 ms → ~500 ms) left a hole that big; fixed with the same one-frame lookahead
   audio already had, and constant-cadence fixtures are exactly why tests never
   caught it. Finding 5 (found by CI, not the device): **every SourceBuffer must
   be created before the first init segment is appended** — Chromium throws
   `QuotaExceededError` otherwise — so the buffer is created up front from the
   mime while the init segment is still withheld until a sample can follow it
   (`initAppended` is now tracked separately from the buffer existing); plus
   per-track SourceBuffer error handling (an audio error no longer blanks video)
   and a sticky, rebuild-video-only audio drop (a dead audio range would freeze
   video, since `buffered` is the tracks' INTERSECTION). Pass 4 (2026-07-26) found the tier gone entirely — CSS pseudo for a whole
   session, `segmentsAppended: 0` with `appendErrors: 0` on a healthy open
   source: **`pump()` was parking the PRIMING appends on MMS `streaming`**, which
   deadlocks by construction (the system asks for data when the element needs
   more; an element with no init segment needs nothing), so priming now goes
   through regardless and parking resumes only once the element reports playable
   media (finding 7). Two silent paths closed alongside it: the segment sink no
   longer disappears while `status` leaves 'watching' (the muxer emits its init
   ONCE per session, so a reconnect could lose it and every segment after it),
   and `presentationSurface` gained `segmentsReceived`/`segmentsQueued`/
   `segmentsDroppedNoInit`/`mmsStreaming` — the four fields that separate "the
   hop is broken" from "the appender is parked", which no capture could tell
   apart before. Still open: the ~100 ms
   live-edge cushion that structurally cannot grow because the muxer is fed after
   the paced release; no stall watchdog on the element; steady-state MMS parking
   as an unmeasured hole source; 20+ s of 7 MP media buffered on the phone. A
   further on-device pass is pending** (R22,
   `docs/27-ios-mse-fullscreen.md`; viewer-client only, zero server/wire/
   broadcaster changes). The iPhone fullscreen button's hidden `<video>` (R16
   scaffolding: gate, tiers, hiding rules, `NativeVideoFullscreen` gate — all
   kept) is now fed by a **worker-side fMP4 muxer over the reorder buffer's
   release stream** through **`ManagedMediaSource`** — encoded bytes, upstream
   of the decoder, which is why it renders where R16's presented-frame
   MediaStream tee was black (spike-confirmed on iPhone 2026-07-23; the tee is
   deleted and its BUGS.md entry closed). Inline WebCodecs/canvas byte-identical
   everywhere; non-gated devices byte-identical, period. Key pieces:
   `transport/fmp4-muxer.ts` (pure; golden-vectored against a committed
   18-frame Annex-B fixture, `transport/h264-fixture.ts`; keyframe-decided
   sticky wire format — see the README gotcha on the AVCC length-prefix /
   start-code collision; muxer-owned monotonic output timeline re-anchoring
   across restarts), the `onReleasedFrame` callback fork
   (viewer → session → worker-core `frameTap`, generation-guarded),
   `muxSegment` worker events (transferred buffers),
   `features/viewer/msePresentation.ts` (probe: MMS/MSE presence + H.264-only +
   `isTypeSupported`; `MsePresenter`: serialized appends, MMS `streaming`
   pacing, cached-init re-prime on remount/overflow, `changeType` on codec
   change, keyframe-resync drops, prune, playing-only live catch-up), and
   `useFullscreen` tier 2's in-gesture seek-to-live → `play()` →
   `webkitEnterFullscreen()` with pause-on-exit (loaded-but-paused arming —
   no dual decode while inline). The muxer's output is CI-proven to play in
   Chrome `MediaSource` (`e2e/run.mjs --muxer-check`, first step of the `e2e`
   job) — but that step drives the muxer/presenter **modules** directly and
   **never goes through the device gate**, so what stays manual is more than the
   iPhone-native presentation: the gate/arming/worker wiring (jsdom-covered
   instead), everything `ManagedMediaSource`-specific (Chrome has no MMS at all —
   `streaming` parking, MMS eviction, the `srcObject` wiring), and WebKit's
   pause-on-underrun, which is definitionally what Chrome does *not* do and is
   why docs/27 finding 1 shipped green. See docs/27 Decision 10's coverage
   boundary before trusting a green muxer check.

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
- Self-hosted target with bandwidth headroom (a known operator, not the open
  internet) means we can trade some robustness for simplicity vs. a
  general-purpose public streaming platform — even though the relay now scales
  out horizontally, we don't owe a hostile-network SLA
- **Trust the actual `VideoFrame` in hand, not `MediaStreamTrack.getSettings()`
  or any other metadata source** — Chrome's `getSettings()` has been observed
  to disagree with the frames MSTP actually delivers. Configure encoders and
  compute layout from the frames themselves. See `docs/01-loopback-test.md`.
27. Advanced diagnostics & telemetry — **TM1–TM8 implemented 2026-07-26,
   TM10 (dip episodes) + TM11 (configured target) 2026-07-27;
   automated gates green in all four modules; TM9 (Grafana) dropped by owner
   scope decision (R9 M8 stays open); manual verification pending** (R28,
   `docs/33-telemetry-and-diagnostics.md`; deviations in its §4.9). A new
   **optional, default-off `gawk-telemetry` module** — the fourth — that adds
   **no measurement**: `ViewerStats`/`BroadcastStats`/`engine.Stats`/
   `RegistryStats` already exist, and this is a pipe, a store and a query
   surface. The load-bearing gap it closes: `/statusz` names a subscriber by a
   random key and **nothing ever told the client its own key**, so the relay's
   view of a viewer and the viewer's view of itself could not be joined. New
   wire type **0x0D `TypeTelemetryHello`** (35 B) closes it — a stateless HMAC
   session token on R17's resume-token pattern, delivered on a **reliable uni
   stream** (the ResumeToken precedent; DeliveryAck's datagram needed a
   re-announce loop because one-shot join-time datagrams get lost), carrying
   the token, the **obfuscated** broadcast key and whether the fleet collects
   at all. `subscriberDetails` gains `sessionId`; `/statusz` gains
   `publisherSessionId`. **Everything is gated on the relay's `-telemetry-key`:
   absent, no hello is sent and the relay + telemetry charts render
   byte-identically to pre-R28** (asserted in CI by diff). Collection is
   out-of-band HTTPS on a **same-origin** path of the frontend Ingress —
   telemetry must outlive the transport it reports on, and same-origin is what
   makes the `sendBeacon` unload flush work without a preflight it cannot
   perform. The **relay is scraped, never pushes** (no outbound HTTP client or
   queue in the process carrying every broadcast's hot path); the relay chart
   gains an optional **headless** companion Service because a ClusterIP would
   load-balance the per-pod scrape, and sub-scrape-interval sessions get
   `relayCoverage: "none"` rather than a silent gap. Storage is files-first:
   hive-partitioned gzipped NDJSON on a PVC, **14 days raw + permanent
   per-session rollups**, retention a directory delete rather than a query;
   DuckDB is a **query option, not a runtime dependency**, which keeps the
   module cgo-free. Validation is **strict envelope, tolerant payload, typed
   rollup** (D15) — version skew is permanent, not transient, so a closed
   field list would lose telemetry exactly during a deploy: known fields are
   typed (wrong type dropped + counted, never fatal), unknown fields survive
   verbatim, `app.version` IS the schema version, and the anomaly tally rides
   the rollup as `schemaAnomalies`. **`diagnose()` is docs/13's bottleneck
   playbook as code** — 15 rules (the 14 rows plus docs/20 finding 8's
   concealment-vs-overflow latch), returning ranked verdicts + evidence and
   **never raw series**; each evidence item is tagged `relay | client |
   derived`, and a verdict resting only on client testimony **caps its own
   confidence** (docs/20 finding 7: a wedged client's own accounting is the
   least reliable evidence in the system and is exactly what it sends). A
   healthy session gets a **positive** verdict with the checks that passed, so
   "no issues" is distinguishable from "the analysis never ran". Every default
   response is bounded at **32 KB**, asserted against a synthetic 4-hour
   session — `diagnose()` there costs 765 bytes. The **live dashboard** (TM8,
   non-droppable) is one page listing every live broadcast with its
   broadcaster and every viewer, severity-sorted with recently-ended
   broadcasts in a **separate recessed group below** (the grouping IS the
   precedence — a live `warn` outranks an ended `bad`), the **same rule
   engine** as `diagnose()`, hysteresis on escalation, and a hard rule that a
   missing report reads stale/unknown and **never** green. MCP rides
   streamable HTTP on the read listener. Zero PII: no IP storage (the rate
   limiter uses one and never persists it), no raw UA (reduced on-device to
   `"Chrome 152"`/`"Windows"`), no cross-session identity, never media; R23's
   terms gained a practice paragraph naming what is and is not collected, and
   `termsVersion` bumped to 2026-07-26.
   **The native broadcaster reports by default (2026-07-27, docs/33 §4.15)** —
   reversing deviation 15's "unset means off", which had left R28's native
   producer dark on exactly the hardware whose findings filled docs/19 and
   docs/20. `config.DefaultRelayURL` (`https://api.gawk.ioio.fi:4433`) and
   `config.DefaultTelemetryURL` (`https://gawk.ioio.fi/api/telemetry/v1/ingest`)
   name the reference fleet, so a first run needs no configuration; the CLI
   flag, GUI field and env var still override, and **`off` is the opt-out
   because blank is taken** (blank = "follow the default", the R9
   `-metrics-addr off` spelling). Two rules worth not re-deriving: blank
   telemetry resolves **only when the relay is also the default one** — a
   session's token is an HMAC minted by the relay it connected to, so a
   self-hosted relay's batch could not be verified elsewhere anyway, and
   silently POSTing a private deployment's data to a third party is the wrong
   default even when it is discarded — and **resolution happens at use, never
   at save**, so a moved fleet address is not pinned by whatever each user's
   config captured (a test asserts `Save` writes neither constant).
   `Reporter.SetURL` makes the endpoint re-readable per `Start` (a settings
   field that needed an app restart would not be a setting), with queued
   batches keeping the endpoint they were *produced* for. Zero wire, relay,
   service, viewer and `gawk-app` changes.
   **The viewer names its own session (2026-07-27, docs/33 §4.13)**: the mirror
   image of §4.12's find-a-broadcast-by-its-code. D8 makes every viewer on the
   dashboard anonymous by construction, so an operator on a call with a
   stuttering friend could not tell which of four rows was them. The viewer
   stats overlay now opens with a **Telemetry → Session id** row: the
   **sessionId, never the token** — 24 bytes of one field, but the id *names* a
   session while the tag *authorizes writing* to one (§4.2), which is the whole
   reason one is displayable and the other is not; **8 characters on screen**
   (byte-for-byte the dashboard's own `sessionId.slice(0, 8)`, and about what a
   person can read down a phone line) with the full 24 in the tooltip and in
   Copy diagnostics, where `diagnose()` needs all of them. Read back from the
   `TelemetryCollector`, never derived from the hello, so the overlay can only
   ever show an id the dashboard has a row for — a telemetry-off fleet still
   sends a well-formed token in a hello that starts no session, and that reads
   `—`. Kept after the session ends (a viewer reads it out *because* the stream
   just broke) and replaced on reconnect (new token, new row). Viewer-only:
   the broadcaster overlay belongs to the operator, who already knows which
   broadcast is theirs.
   **TM10 — dip episodes (2026-07-27, docs/33 D16 + §4.10)**: an audit asked
   whether the shipped item could answer *"why is this stream stuttering, why
   does my viewer framerate drop to 2 every now and then?"* It could not — a
   stream collapsing to 2 fps every twenty seconds diagnosed as **`healthy`**,
   and a paired test now pins that it no longer does. The data was all present;
   every *consumer* collapsed it. Six mechanisms, each defensible alone and
   worth not re-deriving: funnel rules read session **medians** (deliberately —
   an earlier median-vs-p05 mix falsely fired `decoder-choking` on a clean e2e
   stream, so the fix for that false positive installed this false negative);
   the rollup computed `p05`/`min` for every fps series that **no rule read**
   (`playoutOffsetMs`'s P95 was the only tail statistic consumed anywhere);
   **2 fps evades every freeze detector by construction** — it *is* the 500 ms
   GOP cadence, so `timeSinceLastFrameMs` never crosses the 1000 ms stall
   threshold and `stalls` reads 0 on a visibly stuttering stream;
   `keyframe-gap-churn`'s ratio diluted from ~1.0 inside a dip to ~0.02 across
   a session; the live projection folded **only the last sample of each batch**
   (4 of 5 discarded at a 2 s interval / 10 s flush) and `EscalateSamples`
   then suppressed what survived, because an instantaneous fact cannot hold for
   two consecutive evaluations; and the client decimated 3 of every 4 stats
   ticks with no aggregation. The fix adds **episodes beside the percentiles,
   never inside them**: a median answers "did this stage keep up for most of
   the session" and keeps answering exactly that (a test asserts dips cannot
   make a funnel ratio fire), while a contiguous run below `DipRatio` × the
   window's own baseline answers "did it fall apart, how often, and what did
   the counters do while it did". Self-relative baseline (an absolute floor
   would accuse a deliberate 5 fps stream forever), `MinBaselineFps` guard,
   durations from real `tMs` deltas, and **counter deltas captured inside each
   episode** — which is what turns "your fps dipped" into
   `keyframe-only-delivery`, playbook row 9's physics measured in the window
   where its ratio is legible. **One detector, two windows** (the session for
   the rollup, a bounded `DipWindowSamples` ring for the live dashboard), so a
   stored verdict and a live row can never disagree about what a dip is; the
   ring is also what makes an episode *durable* enough to survive hysteresis.
   Two supporting changes: the client carries the interval's minimum as a
   nested **`intervalMin`** object (never by emitting the worst tick *as* the
   sample — that would bias every median and re-break the ratios), and
   `get_session` downsampling became **envelope-preserving** (worst per bucket)
   because decimation drops exactly the transients the timeline is opened for.
   A dip is **not** folded into `stalls`: 2 fps is media flowing badly, a stall
   is media absent, and collapsing the two would make `stalls` mean two things.
   **TM11 — the configured target (2026-07-27, docs/33 D17 + §4.11)**: the
   exact complement of TM10, and neither subsumes the other. The dip detector
   is **self-relative**, so it structurally cannot see a stream whose baseline
   was wrong all along — a broadcaster that asked for 60 fps and delivered a
   flawless 30 for the whole session has a flat baseline, **zero episodes** and
   a perfect funnel (capture 30 → encode 30 → sent 30), and every rule
   correctly passes it. Nothing *degraded*; it simply never was what it was
   asked to be. That was invisible because **the target was never recorded**:
   `BroadcastStats` carried no dims, no target fps and no video bitrate (only
   `audioBitrateBps`), and the three fields it *did* carry — `autoRung`,
   `autoCeiling`, `autoFps` — are `null` in explicit mode **by design**, so the
   more deliberately a broadcaster configured the stream the less was recorded;
   meanwhile the native engine reported `Width`/`Height`/`Fps`/`BitrateBps` and
   the rollup listed them in neither `broadcasterConfig` nor
   `broadcasterSeries`, so they were pruned at 14 days having never reached a
   row. `Config["resolution"]` also read `frameWidth`/`frameHeight`, which only
   a **viewer** reports, so broadcaster rows had no resolution and
   `fleet_summary(groupBy:"resolution")` grouped them all under `unknown`.
   Now: target dims/fps/bitrate/codec/acceleration are recorded **from
   `EncoderConfigured`** — what the encoder *committed to*, never the settings
   that asked (a rung can be refused, clamped or renegotiated, and re-running
   on every encoder recreate means an R4 auto step moves the target with it) —
   the rollup maps both producers' spellings onto one set of config keys, and
   `resolution` resolves per role. One rule, **`delivered-below-target`**
   (`targetShortfallRatio` 0.75). Its shape is set by an honesty problem worth
   not re-deriving: gawk's capture is **damage-driven**, so a motionless screen
   legitimately produces far fewer frames than the target (docs/09's
   real-hardware finding, and why R4's ladder deliberately does not step for
   it) — a rule reading any shortfall as a fault would accuse every quiet
   stream on the fleet. So `captureFps` is the discriminator and rides in the
   evidence either way: capture below target too ⇒ "source-limited", capture at
   target ⇒ "the shortfall is in the pipeline". Severity is **`warn`, never
   `bad`** — a discrepancy against intent is not a broken stream, and the rules
   for an actually-broken one already exist.

## Explicitly set aside (don't suggest reintroducing without discussion)
- WebRTC + self-hosted SFU (OvenMediaEngine, LiveKit, mediasoup) — more mature,
  was the "safe" choice, deliberately not taken
- MoQ — future direction, not current
