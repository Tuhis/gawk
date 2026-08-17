# Design docs

One numbered doc per milestone. Each carries the decisions, the alternatives
that were rejected and *why*, and explicit acceptance criteria — so a doc is
readable years later as a record of what was decided rather than a snapshot
of what was true that week. Status lives in [`ROADMAP.md`](../ROADMAP.md),
never here.

Not a design doc, but in this directory:

| File | What |
|---|---|
| [`self-hosting.md`](self-hosting.md) | **Run your own gawk** — prerequisites, TLS, install, cluster mode, troubleshooting |
| [`gotchas.md`](gotchas.md) | Consolidated gotchas, 116 of them, each of which cost real debugging time |
| [`implementation-tasks.md`](implementation-tasks.md) | The v0.x server design + chunk breakdown (A1–D3) |

## Where to start

Reading all forty-one is not the point. Three give you the shape of the system:

- [`03`](03-single-client-e2e.md) — the end-to-end path: hub, publish,
  subscribe, and the wire format everything else is built on.
- [`12`](12-worker-and-reliable-keyframes.md) — why keyframes travel over
  reliable streams while deltas stay on datagrams, and how the viewer merges
  the two.
- [`22`](22-relay-scale-out.md) — how one broadcast's audience spans pods
  without a coordinator: per-broadcast Leases, origin/edge cascade.

## The foundations (v0.1–v0.5)

| Doc | Milestone |
|---|---|
| [`01`](01-loopback-test.md) | Local loopback: capture → encode → decode in one tab |
| [`02`](02-webtransport-hello.md) | WebTransport hello-world: TLS, dev certs, echo |
| [`03`](03-single-client-e2e.md) | Single-client end-to-end: hub, publish/subscribe |
| [`04`](04-fanout.md) | Fan-out: multi-subscriber, restart-safe caches, `/statusz` |
| [`05`](05-resilience-deploy.md) | Resilience, packaging, Helm charts, CI/CD |

## The relay

| Doc | Milestone |
|---|---|
| [`06`](06-multi-broadcaster.md) | R1 — Multi-broadcaster: the registry and join-by-code |
| [`07`](07-hardening.md) | R2 — Hardening: limits, publish secret, rate limiting |
| [`09`](09-automatic-fallback.md) | R4 — Automatic resolution fallback |
| [`13`](13-observability.md) | R9 — Observability: metrics, `/statusz`, the diagnosis playbook |
| [`22`](22-relay-scale-out.md) | R17 — Scale-out & HA: origin/edge cascade, resume tokens |
| [`23`](23-live-viewer-count.md) | R18 — Live viewer count, aggregated across pods |
| [`26`](26-relay-dvr-buffer.md) | R21 — Relay DVR ring buffer for resilient viewers |

## Delivery and resilience

| Doc | Milestone |
|---|---|
| [`12`](12-worker-and-reliable-keyframes.md) | R8 — Reliable keyframes + worker offload |
| [`24`](24-viewer-network-resilience.md) | R19 — Resilient viewer mode for lossy networks |
| [`34`](34-live-edge-forward-parity.md) | R29 — Forward parity for live-edge delivery |
| [`35`](35-connection-interleaving.md) | R30 — Connection interleaving (striped delivery) |

## The viewer

| Doc | Milestone |
|---|---|
| [`14`](14-viewer-render-performance.md) | R10 — Render performance: rAF coalescing, WebGL sink |
| [`15`](15-viewer-live-edge.md) | R5 — Live-edge measurement + opt-in smoothing |
| [`17`](17-viewer-playback-smoothing.md) | R12 — Paced presentation + adaptive playout offset |
| [`32`](32-live-edge-interpolation.md) | R27 — Frame interpolation in live-edge mode |
| [`37`](37-viewer-playback-presets.md) | R32 — Playback presets & settings UX |
| [`21`](21-ios-video-fullscreen.md) | R16 — iOS fullscreen via `<video>` (**rejected**, on-device) |
| [`27`](27-ios-mse-fullscreen.md) | R22 — iOS fullscreen via fMP4 + `ManagedMediaSource` |

## The broadcaster

| Doc | Milestone |
|---|---|
| [`08`](08-resolution-framerate-picker.md) | R3 — Resolution & framerate picker |
| [`16`](16-broadcaster-worker-offload.md) | R11 — Broadcaster worker offload |
| [`18`](18-advanced-broadcaster-settings.md) | R13 — Advanced settings, probe matrix, HW-aware ceilings |
| [`30`](30-broadcaster-capture-audio-guidance.md) | R24 — Capture & audio guidance |
| [`19`](19-linux-native-broadcaster.md) | R14 — Native **Linux** broadcaster |
| [`28`](28-native-broadcaster-audio.md) | R25 — Native broadcaster audio |
| [`39`](39-linux-app-sharing.md) | R35 — Single-app sharing (window + app audio) on Linux |
| [`38`](38-windows-native-broadcaster.md) | R34 — Native **Windows** broadcaster |

## Audio

| Doc | Milestone |
|---|---|
| [`20`](20-system-audio.md) | R15 — System audio: Opus over datagrams, video-master sync |

## Product surface

| Doc | Milestone |
|---|---|
| [`10`](10-production-ui.md) | R6 — Production UI |
| [`11`](11-cross-browser-compatibility.md) | Cross-browser: Firefox ↔ Chrome H.264 |
| [`29`](29-terms-and-conditions.md) | R23 — Terms & conditions |
| [`31`](31-quick-start-links.md) | R26 — Quick-start broadcast links |
| [`40`](40-relay-server-picker.md) | R37 — Streamlined relay server picker |

## Testing, telemetry, operations

| Doc | Milestone |
|---|---|
| [`25`](25-e2e-testing-in-ci.md) | R20 — Real-browser E2E in CI, both tiers |
| [`33`](33-telemetry-and-diagnostics.md) | R28 — Advanced diagnostics & telemetry |
| [`36`](36-telemetry-ui-history.md) | R31 — Telemetry UI v2: a diagnosis SPA |
| [`41`](41-local-dev-stack.md) | R38 — Local dev stack: `docker compose up`, three TLS lanes |

## Conventions

- A new milestone gets the next number; the file name is
  `NN-short-slug.md`.
- Every doc defines **explicit acceptance criteria** — a per-chunk criteria
  table or a goal → verified-by table. A doc that lists proposed changes
  without them is how the R2 review's critical finding got missed.
- Chunk prefixes: every single letter A–Z is claimed, so new milestones use
  two-letter prefixes (`DV1`, `MF1`, `TM1`, `CG1`, `UX1`).
- Decisions get revised in place with a dated note, not silently rewritten —
  see [`38`](38-windows-native-broadcaster.md) D12 for the shape of one.
