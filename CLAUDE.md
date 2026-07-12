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

## Relay server behavior (implemented — chunks B2/B3 + C1–C3)
- Caches the last keyframe + decoder config to prime newly-joined viewers
  (`gawk-server/internal/hub`); a **new publisher session invalidates both
  caches** (frameIDs reset, config may differ) while caches persist when the
  broadcaster is merely away
- Drops chunks for slow subscribers rather than blocking others (per-subscriber
  bounded queue, non-blocking enqueue); cached config re-emitted before every
  keyframe's chunk 0
- Routes: `CONNECT /publish` (single publisher, 409 when taken),
  `CONNECT /subscribe` (429 when full), `/echo` diagnostic, `GET /healthz`,
  `GET /statusz` (JSON `hub.Stats` snapshot)
- Framing protocol is **implemented** (`gawk-server/internal/wire`): VideoChunk
  datagrams carry frameID + chunkIndex/chunkCount + keyframe flag + timestamp
  (20-byte header, big-endian); a separate DecoderConfig message carries
  codec string + AVCC extradata. Golden test vectors for the future TS mirror
  live in `wire_test.go` and `docs/02-webtransport-hello.md`.

## Directory structure
- `README.md` — project overview, quickstart, and the consolidated gotcha
  list (keep it in sync when a new gotcha lands in `docs/`)
- `gawk-app` is the folder for the frontend application
- `gawk-server` is the folder for the backend (the Relay server) — Go module,
  see `gawk-server/README.md` for build/run
- `docs/` — per-build-step design notes and gotchas. See
  `docs/01-loopback-test.md` for v0.1 (local loopback),
  `docs/02-webtransport-hello.md` for v0.2 (server + TLS + echo),
  `docs/03-single-client-e2e.md` for v0.3 (hub + publish/subscribe relay),
  `docs/04-fanout.md` for v0.4 (fan-out hardening, restart-safe caches,
  `/statusz`).
- `docs/implementation-tasks.md` — **the server design + chunked task
  breakdown (A1–D3) with per-chunk acceptance criteria and progress status.
  Start here when continuing server work.**

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
5. Resilience features (forced keyframes, etc.) — chunks D1–D3 incl. deployment

## On the horizon (not started)
- Periodic forced keyframes for resilience
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
