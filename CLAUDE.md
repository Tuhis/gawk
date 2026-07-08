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

## Relay server behavior
- Caches the last keyframe to prime newly-joined viewers
- Drops chunks for slow subscribers rather than blocking others
- Currently uses minimal framing (single keyframe-flag byte) — **needs extension**
  to include frame IDs + chunk indexes, since encoded frames can exceed the safe
  datagram payload size (~1200 bytes)

## Directory structure
- `gawk-app` is the folder for the frontend application
- `gawk-server` is the folder for the backend (the Relay server)
- `docs/` — per-build-step design notes and gotchas. See
  `docs/01-loopback-test.md` for v0.1 (local loopback).

## Build order
1. Local loopback testing — **done** (v0.1 in `gawk-app/`; see `docs/01-loopback-test.md`)
2. WebTransport hello-world (nail TLS/certificate setup)
3. Single-client end-to-end
4. Fan-out (multi-subscriber)
5. Resilience features (forced keyframes, etc.)

## On the horizon (not started)
- Extend framing protocol: frame IDs + chunk indexes for multi-datagram frames
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
