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

## Where the detail lives — read these, don't mirror them here
This file holds only what a session **cannot** derive from the repo: locked-in
decisions, rationale, gotchas and prohibitions. Everything else has one
authoritative home. Read it there.

| You want | Read | Never |
|---|---|---|
| Status of every milestone (R1–R32+) | `ROADMAP.md` status table | copy status here |
| Design + decisions for a milestone | its `docs/NN-*.md` | summarise it here |
| Consolidated gotchas | `docs/gotchas.md` | duplicate them here |
| How to self-host a deployment | `docs/self-hosting.md` | re-explain it here |
| How to run the stack locally | `README.md` §Quickstart, `docs/41` | restate the lanes here |
| What each design doc covers | `docs/README.md` (the index) | maintain a second list |
| Open confirmed bugs | `BUGS.md` | enumerate them here |
| Coding + review rules | `CODE-REVIEW.md` | restate them here |
| Commit type/scope table, PR etiquette | `CONTRIBUTING.md` | restate them here |
| Wire types, close codes | `gawk-server/wire/wire.go` | maintain a second list |
| v0.x server chunk breakdown (A1–D3) | `docs/implementation-tasks.md` | — |
| Build/run per module | that module's `README.md` | — |

**This table is a rule, not a courtesy.** CLAUDE.md previously carried ~1,800
lines mirroring `ROADMAP.md`, `README.md` and the design docs. Every copy
drifted: it silently missed two shipped milestones (R29 forward parity, R30
connection interleaving) and three wire types, and contradicted itself on a
finding count. If you find yourself writing milestone narrative here, put it in
the doc and link it instead.

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
- **Transport**: WebTransport datagrams (keyframes ride reliable uni streams)
- **Relay server**: Custom Go server using `github.com/quic-go/webtransport-go`.
  Pub/sub hub — one publisher fans out encoded video datagrams to subscriber
  sessions.

## Relay server behaviour
Recap only — the mechanisms live in `docs/03`, `docs/04`, `docs/06`, `docs/07`
and, for the multi-pod cascade, `docs/22`.

- Caches the last keyframe + decoder config to prime newly-joined viewers
  (`gawk-server/internal/hub`); a **new publisher session invalidates both
  caches** (frameIDs reset, config may differ), while caches persist when the
  broadcaster is merely away (GC grace period defaults to 5 minutes).
- **Drops chunks for slow subscribers rather than blocking others**
  (per-subscriber bounded queue, non-blocking enqueue).
- Routes: `CONNECT /publish` (mint ID), `CONNECT /publish/{id}` (reclaim),
  `CONNECT /subscribe/{id}`, `/internal/subscribe/{id}` (edge pull, cluster
  mode), `/echo`, `GET /healthz`, `GET /statusz`.

### Relay invariants (rules, not narrative)
- **Every new knob must be plumbed through `registryOptions` in
  `cmd/gawk-server/main.go`** — flags + `GAWK_*` envs + Helm values. The R2
  post-implementation review found a whole set of limits wired only into the
  test helper, so they never reached production.
- **Never expose raw broadcast IDs, and never hash them unkeyed.** They are
  joinable and only ~31^6 strong; `/statusz` keys them by a per-process HMAC.
- **Raising `-max-idle-timeout` alone is a no-op.** The effective timeout is
  the min of both endpoints and browsers advertise ~30s — `-keepalive-period`
  is the mechanism that keeps idle viewers connected while a broadcaster is away.
- **A token-bearing reclaim supersedes an active publisher session** ("newest
  publisher wins", close code 4004). A silently-dead publisher otherwise holds
  its slot until the QUIC idle timeout, and the old 409 made the broadcaster's
  own reclaim fall back to minting a new ID, orphaning every viewer. The R17
  resume-token gate must pass first, so tokenless requests can't kill a healthy
  broadcast.
- **Close codes 4000 and 4004 are terminal** — no client reconnect (4004 is a
  publisher's own zombie session losing a resume-token race, "terminal for
  resume" per its `wire.go` doc comment). 4001/4002/4003 are not; see the doc
  comments in `wire.go` before adding a code.
- Session-end reasons are cause-recovered via `http3.Server.ConnContext`
  (`internal/transport/endreason.go`). Without it every abrupt death logged an
  identical, useless `reason: "context canceled"`. Don't "simplify" it back.

## Repository layout
Module roles and the facts `ls` can't tell you. Layout itself: read the tree.

- `gawk-app` — frontend SPA.
- `gawk-server` — the Go relay. Its `wire` package is **public** (not
  `internal/`) specifically so `gawk-broadcast` and `gawk-telemetry` can import
  it (R14 Decision 1). Reuse it; **never mirror it**.
- `gawk-broadcast` — native Linux broadcaster: a **separate** Go module (GUI +
  CLI over a shared `internal/engine`). Not a container/chart/deploy
  component — a binary you run on your own PC. Since R35 it ships a **third
  binary**, `gawk-pw-helper`: cgo against `libpipewire-0.3`, spawned per
  broadcast to capture one application's audio. It is a control plane — no
  media passes through it — and its crash-safety comes from owning nothing:
  every object it creates is a proxy on its own connection with no
  `object.linger`, so the daemon reaps them however it dies. Don't give it
  media, and don't make it linger (`docs/39`).
- `gawk-broadcast-windows` — native Windows broadcaster (R34): a **Rust Cargo
  workspace**, not a Go module. Its `crates/wire` is the **fourth wire
  mirror** (vectors restated, never imported); its CI job **runs on the
  self-hosted Linux runners, cross-compiled to msvc with cargo-xwin** — see
  `docs/38` D18 before touching it, especially the clang-cl/libopus wrapper.
- `gawk-telemetry` — optional per-session diagnostics service; the **third**
  top-level Go module, **default off everywhere**. Two listeners, and the split
  **is** the security posture: ingest is public (same-origin path on the
  frontend Ingress), while the dashboard/read API/MCP sit on a listener that is
  never routed publicly. Keep it that way.
- `docs/` — per-milestone design notes. Each component has `deploy/`
  (Dockerfile + Helm chart); `.github/workflows/` holds CI + release automation.

## Key constraints / principles to respect
- ~1200-byte safe datagram payload limit drives the chunking design — don't
  assume larger payloads are safe.
- **Favor dropped frames over stalled playback** (this is why datagrams, not
  streams, were chosen).
- Self-hosted target with bandwidth headroom (a known operator, not the open
  internet) means we can trade some robustness for simplicity vs. a
  general-purpose public streaming platform — even though the relay now scales
  out horizontally, we don't owe a hostile-network SLA.
- **Trust the actual `VideoFrame` in hand, not `MediaStreamTrack.getSettings()`
  or any other metadata source** — Chrome's `getSettings()` has been observed
  to disagree with the frames MSTP actually delivers. Configure encoders and
  compute layout from the frames themselves. See `docs/01-loopback-test.md`.
- **Bug fixes are test-first**: write the failing test, watch it fail, then fix.
  See `CODE-REVIEW.md` — every rule there exists because its absence cost a real
  bug.

## Rejected — don't re-propose without new evidence
Each of these was investigated and settled; the reasoning is in the linked doc.
Re-deriving them costs a cycle and has happened before.

- **Browser hardware encode on Linux** — it does not exist. WebCodecs HW encode
  ships on Windows/macOS/Android only; Chromium's Linux encode path is VA-API
  only, and on NVIDIA `nvidia-vaapi-driver` is decode-only *by design*. Don't go
  flag-hunting. (`docs/19`)
- **Direct VAAPI for the native broadcaster** — rejected: it reproduces exactly
  the limitation R14 exists to escape. Vulkan Video is the target API.
  (`docs/19`)
- **`pipewiregrab`** — not in mainline FFmpeg (an unmerged patchset carried
  downstream by Jami). Don't re-propose without verifying it actually merged.
  (`docs/19`)
- **Deepening the viewer buffer to survive outages** — it *relocates* the freeze
  to after the outage and adds permanent latency. The relay-side DVR ring is the
  answer. (`docs/26`)
- **A viewer→server keyframe-request back-channel** — rejected for good.
  (`docs/15` Decision 6)
- **A fixed playout offset** (the 200 ms sketch) — rejected in favour of the
  adaptive controller. (`docs/12`)
- **"Unready-then-linger" pod draining** — wrong: kube-proxy flushes UDP
  conntrack on endpoint removal, so drains send close code 4002 *while the pod
  is still Ready*. (`docs/22`)
- **Canvas readback on iOS WebKit** — `VideoFrame`-from-WebGL-canvas content is
  black there even with `preserveDrawingBuffer`. Don't build on it. (`docs/21`)
- **Native `webkitEnterFullscreen` fed by a MediaStream** — black across three
  on-device passes; the shipping iPhone path is an fMP4 muxer +
  `ManagedMediaSource`, with CSS pseudo-fullscreen as fallback. (`docs/21` U4,
  `docs/27`)
- **Tray icon and global hotkeys for the native GUI** — deferred; the research
  is already written down. Don't re-derive it. (`docs/19`)

## Conventions
- **Every design doc must define explicit acceptance criteria** for its
  milestones and chunks (a per-chunk criteria table à la
  `docs/implementation-tasks.md`, or a goal → verified-by table à la `docs/07`).
  The R2 review traced its critical finding partly to a doc that listed proposed
  changes without acceptance criteria, so nothing forced the "does the flag
  actually reach production?" question.
- **Chunk prefixes: every single letter A–Z is claimed.** New milestones use
  two-letter prefixes (e.g. `DV1`, `MF1`, `TM1`, `CG1`, `UX1`).
- New wire types and close codes are allocated in `gawk-server/wire/wire.go`
  and must be mirrored in the TS (`wire.ts`), `gawk-broadcast`
  (`internal/wirecheck`) and `gawk-broadcast-windows` (`crates/wire`) checks,
  with golden vectors kept byte-identical across all mirrors. The Windows
  CI job triggers on `gawk-server/wire/**` too, so the Rust mirror's gates run
  in the same PR as the wire change (this was not true before 2026-07-31).
- **Commit messages *and* PR titles must be Conventional Commits.** PRs land by
  squash merge, so the PR title becomes the commit subject on `main` — it is the
  string release-please parses. A non-conforming title releases nothing and
  leaves the change out of the changelog; commit 74c3107 ("R37: streamlined
  relay server picker") is exactly that failure. This applies to the title of
  every PR, including docs- and chore-only ones. Type/scope table and the
  bump rules: `CONTRIBUTING.md`.
- Keep `docs/gotchas.md` in sync when a new gotcha lands in `docs/`,
  and remove `BUGS.md` entries when they are fixed.

## Deployment & CI (locked in — decided 2026-07-12)
- **Helm charts, one per component**, separately versioned; **chart version ==
  appVersion == image tag** always. The frontend is deployed too (nginx behind
  Ingress class `nginx-int`); the relay is a UDP LoadBalancer (nginx ingress
  can't proxy WebTransport).
- **Versioning**: SemVer 2 from conventional commits via release-please
  (monorepo manifest mode, **one combined release PR** — separate PRs conflicted
  on the shared manifest, don't switch back; tags stay per-component:
  `gawk-server-vX.Y.Z` / `gawk-app-vX.Y.Z`).
- **Registry**: GHCR — images `ghcr.io/tuhis/<component>`, charts
  `oci://ghcr.io/tuhis/charts/<component>` (lowercase; private → classic PAT
  pull secret).
- **CI is publish-only; deploys are automated cluster-side.** Whenever a new
  version is released it is deployed to the homelab automatically. CI never
  touches the cluster — no cluster credentials in GitHub. Manual
  `helm upgrade --install` remains the initial-install / break-glass path
  (runbook in `docs/05`). Don't re-propose raw manifests, semantic-release, or
  CI-driven deploys without discussion.

## On the horizon (not started)
- Media over QUIC (MoQ) — explicitly deferred; still an unstable IETF draft
  (draft-17 surveyed) with no native browser support yet. Don't build toward it
  now.

## Explicitly set aside (don't suggest reintroducing without discussion)
- WebRTC + self-hosted SFU (OvenMediaEngine, LiveKit, mediasoup) — more mature,
  was the "safe" choice, deliberately not taken
- MoQ — future direction, not current
