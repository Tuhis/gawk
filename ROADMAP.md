# Roadmap

High-level roadmap for the work that follows v0.5. The v0.x milestones
(loopback → hello-world → single-client → fan-out → resilience + deploy,
chunks A1–D4) are complete — see [`docs/implementation-tasks.md`](docs/implementation-tasks.md)
and the [Status section of the README](README.md#status). This document is
the equivalent forward-looking view: what we build next, in what order, and
why.

**How this document is used**: each item below is deliberately high-level.
When an item is picked up, it gets its own numbered design doc continuing the
existing `docs/NN-*.md` series (milestones, chunked tasks, acceptance
criteria — same shape as `docs/implementation-tasks.md` used for the server).
This file then just tracks per-item status and links to that doc. Ordering is
a proposal based on dependencies and risk, not a contract — but reordering
should be a conscious decision, since later items assume earlier ones (R2's
limits are per-broadcast, R4 builds on R3's ladder, R6 presents whatever
feature set exists).

| # | Item | Status |
|---|------|--------|
| R1 | [Multi-broadcaster support](#r1--multi-broadcaster-support) | ✅ done ([docs/06](docs/06-multi-broadcaster.md)) |
| R2 | [Hardening](#r2--hardening) | ✅ done ([docs/07](docs/07-hardening.md)) |
| R3 | [Broadcaster resolution & framerate picker](#r3--broadcaster-resolution--framerate-picker) | 🚧 implemented, manual verify pending ([docs/08](docs/08-resolution-framerate-picker.md)) |
| R4 | [Automatic resolution fallback](#r4--automatic-resolution-fallback) | not started |
| R5 | [Viewer live-edge enhancements](#r5--viewer-live-edge-enhancements) | not started |
| R6 | [Production UI](#r6--production-ui) | not started |

---

## R1 — Multi-broadcaster support

**Goal**: multiple simultaneous, independent 1-to-many sessions. Five friends
can each stream to their own audience at the same time. Starting a broadcast
mints a short, shareable **broadcast ID**; the broadcaster's UI shows the ID
and a direct join link (`#/view/<id>`), and viewers join by pasting the code
or clicking the link.

**Why first**: this reshapes the hub, the routes, and the URL scheme that
everything else builds on. Hardening limits (R2) are per-broadcast,
`/statusz` becomes per-broadcast, and the production UI (R6) is designed
around the join-by-code flow. Doing R1 later means reworking all of them.

**Scope sketch**:

- The hub (`gawk-server/internal/hub`) becomes a registry of independent
  broadcast sessions keyed by broadcast ID. Everything that is currently
  singleton state — publisher slot, subscriber set, cached decoder config,
  cached last keyframe — becomes per-broadcast. The existing semantics carry
  over unchanged *within* a broadcast: new-publisher-session invalidates that
  broadcast's caches, caches persist while its broadcaster is merely away,
  slow subscribers get drops not blocking.
- **Broadcast IDs are ephemeral and per-session**: a new code is minted each
  time a broadcast starts and dies when the broadcast ends. Short enough to
  read out loud (~6 characters), from an unambiguous alphabet (no `0/O`,
  `1/l/I`). With ~5 concurrent broadcasts the space is nowhere near
  exhaustion; unguessability is what matters (see R2 access-control note).
- Routes grow an ID dimension: publish targets a specific broadcast,
  subscribe requires one. `409 publisher exists` becomes per-broadcast.
- Frontend: broadcast page surfaces the ID + copyable join link; view page
  accepts an ID from the URL hash or a text field. `ViewerSession`
  reconnect logic re-joins the same broadcast ID.

**Key design questions** (for the design doc):

- **How the ID reaches the broadcaster at session setup.** The WebTransport
  JS API exposes no response headers or body on the CONNECT, so the server
  can't mint the ID in the CONNECT response. Two candidate shapes:
  client-mints the ID and publishes to `/publish/<id>` (server rejects
  collisions — fine, they're ~impossible), or a tiny HTTPS
  `POST /broadcasts` mint endpoint called before CONNECT. Client-minted is
  simpler; the mint endpoint gives the server control (and a natural place
  for the R2 broadcast secret). Decide in the design doc.
- **Broadcast lifecycle when the publisher drops.** Today "broadcaster away,
  caches persist" is the desired behavior for a relay restartable
  broadcaster. Per-broadcast, an abandoned session must eventually be
  garbage-collected (timeout after publisher disconnect with no return) or
  the registry leaks. Viewers of a GC'd broadcast need a terminal state, not
  infinite reconnect.
- What `/statusz` looks like with N broadcasts (see R2).

**Non-goals**: persistent per-broadcaster channels ("juho's channel" with a
stable bookmarkable ID surviving restarts) — deliberately deferred; it needs
an identity/persistence story the ephemeral design avoids. Noted as a
possible follow-up, not part of R1.

**Status**: Implemented and fully verified (2026-07-12) — see [`docs/06-multi-broadcaster.md`](docs/06-multi-broadcaster.md) progress table.

## R2 — Hardening

**Goal**: the relay enforces explicit limits and a minimal trust model
instead of relying on obscurity and good behavior, so a bug, a rogue client,
or an accidentally-shared URL can't take the homelab down.

**Why here**: limits are naturally per-broadcast and per-server, so this
lands right after R1 defines what a broadcast is. It's also a prerequisite
for being comfortable leaving the service running unattended.

**Scope sketch**:

- **Configurable limits**: max concurrent broadcasts (default ~5), max
  viewers per broadcast (default 15, extending today's global 429 semantics),
  connection-attempt rate limiting. All via flags + Helm values, matching the
  existing pattern in `gawk-server/internal/config`.
- **Access control — shared secret to broadcast**: publishing requires a
  pre-shared key configured server-side (Helm value / secret) and entered
  once in the broadcaster UI. Viewing stays gated only by knowing the
  unguessable broadcast ID — acceptable for a ~15-friend audience, and
  losing a viewer secret ceremony keeps join friction near zero.
- **Resource bounds**: cap memory held per broadcast for the cached keyframe
  + decoder config (a hostile or buggy publisher shouldn't be able to grow
  them unboundedly), and bound total registry size via the broadcast limit.
- **Defensive parsing**: strict validation limits on datagram headers
  (chunk counts, lengths, config sizes) — malformed input drops the datagram
  or the session, never panics or allocates unboundedly.
- **Observability**: `/statusz` reports per-broadcast stats (viewer counts,
  drop rates, cache state) plus server-wide totals, so limit tuning is
  data-driven.
- **Bandwidth limits**: configurable cap on bandwidth consumed by the relay
  server instance, ensuring server admin can stay within agreed bandwidth
  consumption. What to do when limit is reached is left to the design
  document.

**Key design questions**: where the broadcast secret is presented (CONNECT
header vs. the R1 mint endpoint — these two designs should be decided
together); whether rate limiting lives in the relay or is better done at the
homelab edge; what, if anything, needs to change in the Helm charts' defaults
for a "safe by default" install.

**Non-goals**: real user accounts, tokens, or a login flow — overkill for
the audience. DDoS-grade protection — this is a private homelab service, not
a public platform.

**Status**: done.

## R3 — Broadcaster resolution & framerate picker

**Goal**: the broadcaster chooses what they send. Resolution: **native /
1080p / 720p / 480p**. Framerate: **native / 60 / 30 / 5 fps**. Native
remains the default; the lower rungs trade fidelity for encode headroom and
bandwidth (5 fps is the "screen-sharing a menu, saving my uplink" mode).

**Why here**: independent of R1/R2 (it's broadcaster-side media pipeline
work) but a hard prerequisite for R4, which automates stepping down the same
ladder. Building the picker first means R4 only adds the *decision*, not the
machinery.

**Scope sketch**:

- **Scaling happens broadcaster-side, before encode.** WebCodecs
  `VideoEncoder` does not scale — a scaling stage goes into
  `gawk-app/src/media/` between capture and encoder, likely
  `VideoFrame → OffscreenCanvas (or createImageBitmap resize) → new
  VideoFrame`. Requesting a lower capture resolution from `getDisplayMedia`
  instead is *not* the plan: we keep capture at native and scale ourselves,
  so the ladder can change mid-stream (R4) without renegotiating capture.
- **Framerate limiting = timestamp-based frame dropping before encode** —
  skip frames until the inter-frame interval matches the target. No capture
  renegotiation here either.
- Selected resolution means "cap the longer dimension at the rung's 16:9
  width equivalent (1080p → 1920), preserve aspect ratio" — ultrawide
  3440×1440 at 1080p becomes 1920×804, not a 16:9 squeeze. Capping the
  longer dimension (not the shorter) keeps total pixel count bounded even
  for pathological aspect ratios like 25000×1080.
- Encoder reconfiguration on ladder change (new config → new keyframe →
  viewers' decoders reconfigure via the existing DecoderConfig re-emit path —
  this flow already exists for late joiners and should be verified for
  mid-stream config changes).
- UI: picker on the broadcast page, disabled/locked once R6 decides where
  settings live.

**Key design questions**: cheapest scaling path that stays off the main
thread (OffscreenCanvas in the existing worker vs. main-thread canvas);
whether 480p at 60 fps is worth allowing or the matrix should be constrained;
how a mid-stream resolution change interacts with the relay's cached
keyframe (stale-resolution keyframe priming a new viewer for one GOP).

**Constraints to respect**: the project rule from `docs/01` is doubly
important here — derive all dimensions from the actual `VideoFrame` in hand,
never `getSettings()`; and keep H.264's even-dimension requirement when
computing scaled sizes.

**Status**: implemented 2026-07-13 (chunks H1–H3 + automated gates; manual
browser verification pending) — see
[`docs/08-resolution-framerate-picker.md`](docs/08-resolution-framerate-picker.md).

## R4 — Automatic resolution fallback

**Goal**: if the broadcaster's machine can't sustain encoding at the chosen
resolution, step down the R3 ladder automatically instead of stuttering or
dying. The broadcaster picked "native" on a laptop that can't do it — the
stream should degrade to 720p on its own, with a visible indication, rather
than requiring the broadcaster to diagnose encoder backpressure themselves.

**Why here**: pure delta on top of R3 — the ladder, the scaling stage, and
mid-stream reconfiguration all exist; R4 adds detection and the decision
loop.

**Scope sketch**:

- **Detection**: sustained `VideoEncoder.encodeQueueSize` growth, encode
  errors, or output falling persistently behind the target framerate. Needs
  hysteresis — a one-off spike (scene change, background task) must not
  trigger a downgrade.
- **Action**: step down one rung (resolution first; possibly framerate as a
  later rung), reconfigure via the R3 machinery, notify the UI ("streaming
  at 720p — encoder can't keep up at 1080p").
- Auto-stepping never overrides an *explicit* lower choice — it only moves
  below the broadcaster's selection, and the selection remains what the UI
  shows as intent.

**Key design questions**: whether to ever step back **up** automatically
(risk: oscillation; a conservative option is "step down automatically,
step up manually" for v1 — decide in the design doc); exact
detection thresholds and window sizes (needs empirical tuning on the real
gaming PC); whether bitrate reduction should be a rung on the ladder before
resolution drops.

**Non-goals**: per-viewer adaptation (simulcast/SVC-style "each viewer gets
what their downlink supports") — that's a different architecture (the relay
is a byte forwarder by design) and explicitly out of scope.

**Status**: not started.

## R5 — Viewer live-edge enhancements

**Goal**: viewers never fall behind live. **Playing the latest frames is
strictly more important than playing every frame** — this has been the
project's transport philosophy from day one (datagrams, drops over stalls);
R5 extends it through the viewer's decode/render pipeline, which today can
still accumulate latency under load.

**Why here**: benefits from R3/R4 first reducing pathological input (a
struggling encoder produces exactly the bursty, gappy stream that stresses
the viewer), and the design doc should measure before building — some of
this may already be adequate.

**Scope sketch**:

- **Latest-frame-first rendering**: if multiple decoded frames are pending,
  render the newest and drop the rest — never queue decoded frames for
  smooth-but-late playback.
- **Backlog discard + skip-ahead**: if reassembled-but-undecoded frames pile
  up (slow decode, tab throttling, network burst after a stall), discard the
  backlog. Because inter-frames can't decode without their predecessors,
  skipping means jumping to the **next keyframe** — drop everything until
  one arrives, then resume.
- **Live-edge measurement**: expose glass-to-glass / capture-to-render delta
  in stats (the wire format already carries capture timestamps) so "are we
  at live?" is observable, not vibes. Feeds the R6 debug overlay.
- Recovery-time bound: after any stall (reconnect, tab unfocus), time back
  to live is bounded by the keyframe interval (currently 120 frames
  broadcaster-side) — evaluate whether that interval is right once skip-ahead
  exists.

**Key design questions**: whether a **viewer→server keyframe-request signal**
is worth adding so a behind viewer doesn't wait up to a full GOP for the next
keyframe. Today the data flow is strictly one-way (broadcaster → relay →
viewers) and the relay deliberately doesn't talk back to the broadcaster;
a request channel is a real protocol addition (new wire message, relay
aggregation/debouncing so 15 viewers can't spam the encoder). May be
overkill given the existing cached-keyframe priming — the design doc should
start with measurements.

**Status**: not started.

## R6 — Production UI

**Goal**: the UI stops looking like a diagnostics rig. For the real
audience: fullscreen-first viewing, join by code or link, minimal chrome
that gets out of the way — think Netflix but sleeker, with Apple-calibre
polish and restraint. A friend clicking a join link should see the stream
and essentially nothing else.

**Why last**: the UI presents whatever feature set exists — join-by-code
(R1), broadcaster settings (R3), degradation notices (R4), live-edge status
(R5). Building it earlier means rebuilding it.

**Scope sketch**:

- **Viewer experience**: land on `#/view/<id>` → stream fills the viewport,
  controls (fullscreen, leave, maybe volume when audio ever lands) fade out
  after inactivity. Sensible idle/connecting/broadcaster-away states instead
  of raw status text.
- **Broadcaster experience**: start broadcast → pick screen → get shown the
  broadcast code + copy-link button prominently; settings (R3 pickers,
  broadcast secret) tucked behind an unobtrusive panel.
- **The current UI survives as the debug surface**: today's stats-heavy
  broadcast/view pages and the loopback page move behind a hidden `#/debug`
  route (not linked from the production UI) and stay maintained — they are
  the troubleshooting story. Additionally a lightweight stats overlay
  (keyboard shortcut, à la Netflix's `Ctrl+Shift+Alt+D`) in the production
  viewer covers the "is it the stream or my machine" question without
  leaving the sleek UI.
- Visual design pass: typography, spacing, dark-first palette, motion — this
  is a design effort, not just a re-layout, and the design doc should include
  actual mockups before implementation.

**Key design questions**: how much of the existing page code is reused vs.
rebuilt (the transport/media modules are UI-agnostic and carry over
untouched; the React pages are probably rebuilt); whether `#/debug` shares
components with the production UI or stays frozen as-is.

**Status**: not started.

---

## Explicitly out of scope (unchanged from CLAUDE.md)

Restated here so the roadmap doesn't quietly reopen settled decisions:

- **WebRTC + self-hosted SFU** (OvenMediaEngine, LiveKit, mediasoup) — the
  mature path, deliberately not taken; this project exists to explore
  WebTransport + WebCodecs.
- **Media over QUIC (MoQ)** — still an unstable IETF draft with no native
  browser support; a future direction, not a current target. Don't build
  toward it.
- **Persistent broadcaster channels** — deferred beyond R1 (see R1
  non-goals).
