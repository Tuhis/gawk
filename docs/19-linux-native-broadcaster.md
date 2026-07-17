# R14 — Native Linux broadcaster (`gawk-broadcast`)

**Status**: design revised 2026-07-15 (post-review). **V0–V7 implemented
2026-07-15**; automated gates green (both Go modules), **manual verification
on the Linux gaming PC pending** — see [Verification plan](#verification-plan-manual),
which is the gate that matters here: nothing about hardware encode, the
portal, or the V4 timestamp-bias measurement is observable from the
development box (WSL2, no GPU encode block, no desktop portal). **V8 (direct
Vulkan Video encode) is not started** and remains hard-gated on V2's
on-hardware Stage-1 result. Three implementation deviations from this design
are recorded in [Implementation notes](#implementation-notes-deviations-from-this-design).

**Revision note (2026-07-15, recorded rather than quietly edited).** The
original same-day design was reviewed against the codebase and the outside
world before any chunk started, and three findings forced a re-plan:

1. **`pipewiregrab` is not in mainline FFmpeg — at all.** It is an unmerged
   2023–2024 patchset (Savoir-faire Linux; RFC as a libavdevice input, v1/v2
   as a lavfi source) that ships only as a downstream patch in their Jami
   project. It is absent from FFmpeg's filter docs and from master's
   `libavfilter/allfilters.c`; there is no `--enable-libpipewire` configure
   option in any release. The original doc called it "an ffmpeg ≥7.1 lavfi
   source filter" and built the whole capture path on it — that would have
   meant every broadcaster (us **and every friend**, per Decision 4's own
   premise) building a patched ffmpeg from source. The capture vehicle is now
   **GStreamer as a subprocess** with a **Go-owned portal handshake**
   (Decisions 3 and 5). Do not re-propose pipewiregrab without first checking
   that it actually merged.
2. **Hardware encode is a must-have; software encode is out** (user decision
   2026-07-15). `libx264` is removed from the cascade: a machine with no
   working hardware encoder is refused with a message pointing at the browser
   broadcaster, which already covers the software case fine. This also
   dissolves a contradiction the review found — "1080p60 because this engine
   is hardware-by-construction" vs. "libx264 is a permanent fallback". It is
   now actually hardware-by-construction.
3. **Viewer count and the "first viewer joined" notification were
   unimplementable as designed** — no wire message tells a publisher anything
   about subscribers, the browser broadcaster doesn't know its viewer count
   either, and `/statusz` is ops-only (cluster-internal listener, HMAC-keyed
   IDs). Both are dropped (Decision 18).

Two further decisions were made with the user the same day: the broadcaster
moves to its **own top-level Go module** with `wire` promoted to a public
package (Decision 1 — the original same-module plan silently coupled relay CI
and relay releases/deploys to broadcaster work), and the encoder cascade is
**Vulkan-first** (Decision 4, faithful to Decision 21's target-API staging).

## Goal

A standalone native broadcaster for Linux with **hardware encode**, bypassing
the browser entirely: a Gio **GUI app** for normal use, plus a CLI over the
same engine for headless/debug use. Easy to use, minimal dependencies — every
runtime dependency is a stock distro package, nothing is built from source,
and **the desktop's share picker appears every time you start** (the choice is
deliberately never persisted — see Decision 5, reversed 2026-07-16).

```
gawk-broadcast-gui                     # the app you actually use
gawk-broadcast -url … -fps 60          # same engine, headless
→ Broadcast code: K7M2QP   join: https://gawk.example/#/view/K7M2QP
```

It speaks the existing publisher protocol byte-for-byte. Zero server changes,
zero wire changes, zero viewer changes. The browser broadcaster stays exactly
as it is and remains the path for Windows/macOS, for Linux machines without a
usable hardware encoder, and for anyone who doesn't want to install anything.

## Why: the browser path cannot do hardware encode on Linux

This is not a tuning problem, it is a platform gap, and it is worth recording
precisely so nobody re-litigates it with a flag hunt:

- **WebCodecs `VideoEncoder` hardware encode ships on Windows, macOS and
  Android — not Linux.** Linux gets hardware *decode* only. Chromium's own
  VA-API doc is explicit about the whole surface: *"VA-API on Linux is not
  supported, but it can be enabled using the flags below, and it might work on
  certain configurations -- there's no guarantees."* The relevant flag is now
  `--enable-features=AcceleratedVideoEncoder` (the older `VaapiVideoEncoder`
  naming is gone).
- **On NVIDIA it is structurally impossible, not merely unsupported.**
  Chromium's Linux encode path is VA-API only (no NVENC encoder exists for
  Linux; the other backends are V4L2, Media Foundation, VideoToolbox), and
  `nvidia-vaapi-driver` is NVDEC-backed and **decode-only** — its author's
  position is that VA-API and NVENC are too different to bridge. So NVIDIA +
  Linux + any browser = software x264/OpenH264, permanently, at whatever cost
  in game fps that implies.
- **On Intel/AMD it is a "maybe"** worth exactly one five-minute test (the
  flag above, then check `chrome://gpu` → Video Acceleration Information) —
  but WebCodecs encode specifically is still documented as unsupported on
  Linux, so the expected outcome is that it doesn't help *us* even when
  VA-API encode lights up for WebRTC.
- **Firefox is worse, not better.** Its `VideoEncoder` support is narrower,
  and `docs/11` already catalogues its AVCC extradata and path-MTU bugs. That
  path means owning those bugs forever.

Going native also sheds three problems we already know about, for free: the
process-level backgrounding caveat recorded in R11, the `getDisplayMedia`
picker (replaced by the portal — still shown every start, but it is the
desktop's own dialog, not a browser tab chooser), and R4's field finding that
observed low fps was 4K-capture-source limited.

## Why this is much cheaper than "write a second broadcaster" sounds

Most of it already exists, in Go:

- **`gawk-server`'s `wire` package is the canonical frozen implementation
  with golden vectors.** The engine *reuses* it rather than becoming a third
  parallel implementation (after Go + `wire.ts`). V0 promotes it from
  `internal/wire` to a public `gawk-server/wire` package so the broadcaster
  module can import it; the golden vectors and the compile-time coupling are
  what keep a second broadcaster from silently rotting (Decision 11). This is
  the single biggest reason to write this in Go rather than Rust or C.
- **`webtransport-go` is already a dependency** and its `Dialer`/`Session`
  expose precisely the publisher's needs: `SendDatagram` (deltas, TimeSync),
  `OpenUniStream` (keyframes), `AcceptUniStream` (the announce), and
  `ReceiveDatagram` (TimeSync pongs). Better: unlike the browser's opaque
  `WebTransportError`, the dial returns the `*http.Response` — the engine can
  distinguish 401 (bad secret) / 404 (unknown ID) / 409 (publisher exists) /
  429 (limit) and say so (Decision 10).
- **The viewer already auto-detects Annex-B vs AVCC** (the `isAnnexB`
  start-code sniff where the pending config is applied in `viewer.ts` —
  `description` is only attached when the payload isn't Annex-B *and* the
  extradata starts with `0x01`; cite the behavior, not a line number — the
  original doc's `:352` had already drifted by review time). So the engine
  emits raw Annex-B with **empty extradata** and never constructs an
  `AVCDecoderConfigurationRecord` — verified: `ParseDecoderConfig` accepts
  empty extradata server-side, and the empty-extradata path routes the viewer
  into its Annex-B branch. That removes the nastiest interop risk in the
  whole idea before it exists.
- **A native client trusts the homelab CA directly**, so the
  `serverCertificateHashes` 14-day certificate dance doesn't apply.

The publisher protocol surface is five message types sent (VideoChunk,
DecoderConfig, StreamFrame, TimeSync, ClockMapping) plus two received
(BroadcastAnnounce on a server-initiated uni stream, TimeSync pongs as
datagrams).

## What this does and does not buy (be honest)

- **Does**: hardware encode on Linux at all (the browser cannot); removes
  capture→encode from the renderer process; replaces `getDisplayMedia` with
  the desktop's own portal picker (shown every start — the choice is never
  persisted, Decision 5 reversed 2026-07-16); a broadcaster that survives
  having no browser open.
- **Does not**: help Windows/macOS broadcasters (already fine), improve the
  viewer, serve machines without hardware encoders (that's the browser's
  job now, by decision), or change end-to-end latency by itself — the win is
  *available encode capacity and game fps on the broadcasting machine*, not a
  shorter glass-to-glass path. The g2g budget is unchanged.
- **Costs**: a second broadcaster implementation to keep protocol-current
  (Decisions 1 and 11 bound that cost), plus V0's one-time module/package
  churn.

## Shape: one engine, two shells, own module

The GUI requirement is not a layer on top of the CLI — it determines the
engine's shape. A `main` with flags and package globals would have to be
dismantled to grow a window, so the engine is a **package** from V1 and the
CLI is its first shell, not its home:

```
gawk-broadcast/                        top-level Go module (V0)
  internal/engine    Session{Start,Stop} + Callbacks + StartError{Phase,Status}
  internal/portal    XDG ScreenCast handshake (godbus); picker every start
  internal/gst       subprocess supervision + pipeline construction + cascade
  internal/mpegts    TS/PES demux → one AU per PES (V3)
  cmd/gawk-broadcast     CLI shell — headless, harness, debug
  cmd/gawk-broadcast-gui GUI shell — Gio window + notifications
```

`Session` deliberately mirrors the TypeScript `BroadcastSessionLike`
(`start`/`stop`) and `BroadcastCallbacks`
(`onBroadcastId`/`onStats`/`onError`/`onEnded`) from `transport/broadcaster.ts`.
Same idea, same names, other language: the two broadcasters stay legible to
each other, which is what makes a second implementation survivable. (The TS
surface also carries `setLadder`/`setEncoderSettings`; the engine deliberately
does not — Decision 9.)

The CLI is **not** scaffolding to be discarded. It proves the engine
end-to-end before any window exists (V4 gates the whole protocol and the
latency bias), and it stays as the headless path — the direct analogue of the
frozen `#/debug/broadcast` page next to the production UI.

## Locked decisions

1. **Own top-level module (`gawk-broadcast/`); `wire` promoted to public.**
   *Revised 2026-07-15; the original decision put everything inside the
   `gawk-server` module, and the review found two hidden costs it never
   priced in.* (a) CI runs `go vet ./...` + `go test -race ./...` over the
   gawk-server module — the moment a `cmd/` imports Gio, the **relay's** CI
   needs gcc + Wayland/X11/EGL/Vulkan headers or fails to build. (b)
   release-please maps everything under `gawk-server/` to the relay's release
   unit (chart == appVersion == image tag, with automated cluster-side
   deploy-on-release) — a GUI-only commit would bump the relay's version,
   rebuild its image and **redeploy the relay to the homelab**. So: V0
   promotes `internal/wire` → **`gawk-server/wire`** (public within the
   module; it may keep importing `internal/broadcastid` — the `internal` rule
   restricts importers by path prefix, and `gawk-server/wire` qualifies;
   nothing else needs promoting), and the broadcaster lives at
   **`gawk-broadcast/`** (module `github.com/Tuhis/gawk/gawk-broadcast`),
   depending on the gawk-server module via a local `replace`. Own
   release-please component (`gawk-broadcast-vX.Y.Z` tags in the one combined
   release PR), own CI job (which installs the Gio headers), and the relay's
   CI, image and deploys become permanently immune to broadcaster work. The
   cost is a one-time mechanical import rewrite plus doc sync — the "escape
   hatch" the original doc deferred, now taken deliberately because the
   benefit stopped being hypothetical.
2. **No container image, no Helm chart, no CI deploy.** These are binaries you
   run on your own gaming PC — `go build ./cmd/...`. Explicitly outside the
   "chart version == appVersion == image tag" model, which exists for
   cluster-side components. The gawk-broadcast release-please component exists
   for versioning/changelog only; attaching prebuilt binaries to its releases
   is a possible later convenience, not part of R14.
3. **The media stack is a GStreamer subprocess (`gst-launch-1.0`) for V1–V7;
   direct encode is V8.** *Revised 2026-07-15 — the original vehicle was an
   ffmpeg subprocess around `pipewiregrab`, which does not exist in any
   mainline or distro ffmpeg (see the revision note); mainline ffmpeg has no
   PipeWire input at all.* The honest reasons for a subprocess are unchanged
   and carry over intact: **crash isolation** — we are heading into flaky
   NVIDIA-on-Wayland driver territory, and a dying child process is a
   notification, whereas an in-process crash takes the GUI down with it — and
   **version tolerance** (any distro GStreamer ≥1.24 works; the elements are
   resolved at runtime). Every piece is a stock package: `gstreamer1.0-tools`
   (the `gst-launch-1.0` binary), `gstreamer1.0-pipewire` (`pipewiresrc`,
   shipped by the PipeWire project itself — it is how desktops already do
   screencast), `gstreamer1.0-plugins-base/-good/-bad` (the encoders; the
   `nvcodec` plugin `dlopen`s the NVIDIA driver's encode library at runtime,
   so distro packages suffice on NVIDIA too). ffmpeg keeps exactly one role:
   **offline generation of V8's reference bitstream** with mainline
   `h264_vulkan` (FFmpeg ≥7.1, an LTS release — verified) from a committed
   y4m fixture, which needs no capture and therefore no patches.
   **Rejected**: `go-gst` / in-process GStreamer (forfeits crash isolation,
   heavy cgo API); ffmpeg-as-runtime-vehicle (no PipeWire input in mainline);
   `go-astiav` (hard pin to n8.0, in-process). The earlier report of
   `pipewiresrc` capture stopping when an app goes fullscreen must be
   re-tested first-hand in V2 — but note that failure class lives in the
   compositor/portal stream, which *any* consumer (pipewiregrab included)
   would have hit identically; it was never a differentiator between tools.
4. **Hardware-only encode via a cascade probed per user, at every startup —
   Vulkan first.** Ordered preference **`vulkanh264enc`** → `nvh264enc` →
   `vah264enc` → **refusal**, each accepted only after a **real trial
   encode**, not by element availability (`gst-inspect-1.0` happily lists
   elements that fail at runtime on the actual device). Vulkan leads by user
   decision (2026-07-15), faithful to Decision 21: the target API gets
   exercised on every capable machine from day one. **`libx264`/software is
   gone** (user decision 2026-07-15): hardware encode is the entire reason
   this app exists, and the browser broadcaster already serves the software
   case — a machine where no candidate passes is refused with a sentence
   pointing there, not degraded. Trial mechanics, specified because the review
   found them underspecified: trials encode **`videotestsrc`** (never the
   portal — probing must not pop share dialogs); the last-good encoder is
   cached in the config file and re-verified first at startup (full cascade
   only on its failure or first run — still runtime-truth, without four
   serial probes before every session); and a `videotestsrc` trial cannot
   prove the *real* path's dmabuf import, so **the live pipeline start is the
   final probe** — on live failure the cascade advances and retries, which
   re-uses the portal session already granted for *this* Start (held in
   memory, not persisted — Decision 5), so retries cost seconds, not share
   dialogs. This is the native-side cousin
   of **R13's probe matrix**, one layer down, and it inherits R13's caveat —
   the probe's answer is advisory, runtime wins — which is the project's
   standing `getSettings()` rule in another costume: *trust the thing
   actually in hand, not the metadata that describes it*. The chosen encoder
   is reported at startup, in stats, and in the GUI.
   **This is a per-user runtime decision, not a design-time one.** R1 made
   gawk multi-broadcaster: friends broadcast from GPUs we do not know and
   cannot survey. The three hardware backends are **permanent
   infrastructure**, not scaffolding on the way to V8 — and the floor under
   them is the browser broadcaster, not a software rung here.
5. **Capture = Go-owned XDG ScreenCast portal handshake + `pipewiresrc`; the
   picker appears on every start (the choice is never persisted).**
   *Revised 2026-07-15* (vehicle) *and reversed 2026-07-16* (persistence).
   The original design had `pipewiregrab` do the portal dance and recorded
   "owning the portal" as a deferred future option (old Decision 19) that
   "does not compose with ffmpeg". Both halves are void: the vehicle changed,
   and `pipewiresrc` **requires** someone else to do the portal handshake
   (screen content is only reachable through a portal-granted PipeWire fd). So
   the engine does it: ~250 lines of pure Go over `godbus` — `CreateSession` →
   `SelectSources` (cursor mode **embedded**, so the pointer is visible like
   the browser path; **`persist_mode` and `restore_token` are deliberately
   never sent**) → `Start` → collect the stream node id → `OpenPipeWireRemote`
   → fd, passed to the child via `ExtraFiles` (`pipewiresrc fd=3 path=<node>` —
   the node id from `Start` is a PipeWire *global object id*, which `path`
   selects; `target-object` matches a node name/serial instead and fails at
   runtime with "target not found", found on first hardware bring-up
   2026-07-17). **Persisting the choice was dropped by user decision
   (2026-07-16): the broadcaster asks what to share on every start rather than
   silently reusing a prior grant.** The earlier design persisted the portal's
   restore token so you would "pick once, ever"; that is gone — no token is
   requested, kept, or replayed, so the desktop's own share picker (KDE, GNOME
   — we don't draw it, we don't theme it) reappears each run, and a cancelled
   picker is the typed `ErrCancelled` outcome. Within a single Start the
   already-granted session is still reused for cascade retries (in memory), so
   a live-probe failure does not re-pop the dialog. The GUI still needs **no
   source picker of its own** — the desktop's is the picker. There is no
   restore-token version gating any more; **do not hard-gate on Wayland**: the
   portal also works on X11 GNOME sessions — gate on the portal call
   succeeding and let the error message name the portal when it doesn't.
   (kmsgrab and gpu-screen-recorder remain V7 escape hatches; `x11grab` does
   a CPU readback per frame and is a non-starter at 4K.)
6. **Frame timestamps are stamped on arrival from the pipe, with a measured
   bias gate.** Stamping when an access unit is fully read biases R5's
   absolute capture→render latency *low* by (capture + encode + mux)
   latency. With the software encoder gone this is a genuinely small, roughly
   constant number — the review found the original claim was only true
   per-encoder (x264's default 40-frame lookahead alone is ~1.3 s at 30 fps),
   which is why Decision 13's invariants table now pins every candidate to
   ≤1 frame of internal latency, verified in its trial. The gate stands:
   **V4 measures the bias against a physical reference, and if it exceeds
   15 ms or proves non-constant, the fix is to anchor the MPEG-TS PES PTS**
   (already parsed for framing, Decision 7) **to the shared monotonic
   clock**. Note precisely: unanchored PTS only *stabilizes* the bias (it
   reveals inter-frame pacing, not the pipeline's constant floor); removing
   it requires capture-time stamps carried through on CLOCK_MONOTONIC —
   which Go's `time.Since` base shares on Linux — so the mpegts fallback is
   specified as *clock-anchored* PTS, not PTS alone. Frame timestamps and
   the TimeSync `t0` must read the **same monotonic clock** or
   `ClockMapping` publishes a mapping between two unrelated timelines and
   the viewer's absolute latency is nonsense.
7. **AU framing via MPEG-TS over the pipe.** *Revised 2026-07-15; the
   original design pushed raw Annex-B through the pipe and re-found frame
   boundaries with inserted AUDs, then listed "-f mpegts" as the fallback if
   Decision 6's gate failed — i.e., risked building two framing layers.* The
   pipe now carries **MPEG-TS** (`h264parse config-interval=-1 ! mpegtsmux !
   fdsink`): `payload_unit_start_indicator` gives AU boundaries structurally
   (one PES = one access unit), PES PTS comes along free (Decision 6's
   upgrade path), and `h264parse config-interval=-1` **guarantees SPS/PPS
   in-band before every IDR** — which is load-bearing, not cosmetic: on this
   path the DecoderConfig extradata is empty, so a late joiner primed with
   the relay's cached keyframe can only decode if the parameter sets travel
   inside the keyframe AU itself. The original design left that property to
   per-encoder header-repeat conventions; now the parser enforces it
   structurally, and V3 asserts it over a fixture. Keyframe classification is
   unchanged in spirit: an AU containing an IDR slice (NAL type 5) → reliable
   uni stream; otherwise → datagrams. The Go-side demux (`internal/mpegts`)
   is deliberately minimal: 188-byte packets, PAT/PMT once, single video PID,
   PUSI-delimited PES with the video-stream `PES_packet_length = 0`
   convention, AU accumulation bounded by `wire.MaxKeyframeBytes` — a few
   hundred lines against recorded fixtures, replacing the raw-splitter's
   adversarial start-code scanning.
8. **The codec string is parsed from the SPS, never assumed.** `avc1.` +
   hex(profile_idc, constraint_flags, level_idc) read out of the first SPS
   NAL, because the encoder may pick a different level than requested. ~20
   lines; the three bytes sit immediately after the NAL header, before any
   position an emulation-prevention byte can legally occupy, so a raw read is
   safe (leave that as a comment in the parser). On the Annex-B path the
   viewer uses this string as-is (its extradata-derived correction only runs
   for AVCC), so it is the *only* thing telling the decoder what it's
   getting — worth getting right.
9. **No ladder, no auto-fallback, no mid-session rung changes in v1.**
   Resolution/fps/bitrate are fixed for a session; scaling and fps limiting
   happen in the GStreamer graph (in hardware). R4's `FallbackController` is
   deliberately **not** ported: its trigger is `encodeQueueSize` growth, and
   R4's own hardware finding (2026-07-13) was that HW encoders drain frames
   without that signal ever firing — porting it would carry the complexity
   and reproduce the under-fire. Defaults: **1080p60**, **500 ms GOP** (item
   11's `keyframeIntervalMs`). The fps default tracks **R13**, not item 11:
   R13's 'auto' framerate resolves framerate-first — 60 where hardware is
   confirmed — and with the software path removed (Decision 4) this engine
   is hardware-encode by construction, so 1080p60 is now *coherent* rather
   than aspirational; 30 stays one flag away. **`SetLadder` is cut from the
   engine surface entirely** (*revised*: the original kept it "for symmetry"
   with the GUI disabling it while live — a dead-code footgun, since a rung
   change means restarting the child and the original design's restart
   re-ran the portal picker). Per Decision 5 (as reversed) a rung change that
   restarts the whole capture *does* re-pop the picker, so a restart-based
   `SetRung` is still cheaply addable later — as its own task, with frameId
   continuity and config re-emit specified, and noting it costs a share
   dialog. Not v1.
10. **No auto-reconnect; reclaim is one click, and errors carry the HTTP
    status.** This matches the browser broadcaster exactly —
    `BroadcasterScreen` doesn't auto-reconnect either (only the viewer does,
    via `ViewerSession`). The GUI remembers the last code and offers
    **Resume broadcast** (which, per Decision 5 as reversed, re-prompts the
    share picker like any other start), dialing `/publish/{id}`; on failure it applies the existing
    rule — **fall back to minting only when `phase == 'connect'`** — so the
    engine's `StartError` carries the same `Phase` distinction (`connect` vs
    `capture`): a capture-phase failure had a live publisher session and must
    not silently move to a different broadcast ID. Additionally — a genuine
    advantage over the browser, where `WebTransportError` hides the HTTP
    status — `webtransport-go`'s dial returns the `*http.Response`, so
    `StartError` also carries the status and the shells can say *"wrong
    secret" (401)*, *"broadcast not found" (404)*, *"someone is already
    publishing" (409)*, *"relay is full" (429)* instead of guessing. The
    announce read is bounded (the message is ≤258 bytes; cap the read and
    set a deadline) so a misbehaving relay cannot hang `Start`.
11. **The wire package is reused, never mirrored, and the engine is bound to
    the protocol by the same golden vectors.** No hand-copied constants, no
    re-derived header layouts. The module split (Decision 1) does not weaken
    this: the local `replace` directive means any wire change still breaks
    the engine's compilation or its golden-vector tests immediately — which
    is the point; this is the mechanism that keeps a second broadcaster from
    silently rotting.
12. **Uplink backpressure has a stated policy** (*new 2026-07-15; the review
    found none, and friends broadcast over home uplinks where saturation is a
    normal condition, not an edge case*). Datagrams: pin down quic-go's
    `SendDatagram` semantics in V3 (queue-full and `DatagramTooLargeError`
    are different failures); on a mid-frame send failure, **drop the rest of
    that frame's chunks** — a partial frame is dead weight the viewer's
    reassembler will discard anyway — and count it
    (`framesDroppedAtSend`). On `DatagramTooLargeError`, lower the chunk
    size to the reported `MaxDatagramPayloadSize` and re-chunk (the Go-side
    analogue of `docs/11`'s Firefox path-MTU fix — do not assume 1200 is
    reachable). Keyframe uni streams: **≤1 in flight**; if a new keyframe is
    ready while the previous stream is still writing, `CancelWrite` the old
    one and send the new — the relay's own supersede discipline, applied at
    the source. (The browser fire-and-forgets its keyframe streams with no
    bound — a known-weak spot we do not reproduce in fresh code; a stalled
    uplink with a 500 ms GOP would otherwise accumulate open streams toward
    stream-credit exhaustion, the publisher-side mirror of R10's zombie
    finding.) Frame IDs from 0, wrapping at uint32 like `nextFrameId` — the
    relay's R9 ingress-loss window depends on it.
13. **Encoder invariants are a table with per-candidate acceptance criteria**
    (*new 2026-07-15*), not incidental pipeline args — the protocol and the
    latency story depend on them, and three hardware backends × driver
    versions is too much surface to leave to defaults:

    | Invariant | Why it's load-bearing | Verified by |
    |---|---|---|
    | **No B-frames** | The whole viewer pipeline assumes decode order == presentation order (frameId ordering, reorder buffer, live-edge/pacing math) | V3 fixture: no B slices; V2 trial args pin `b-frames=0`/equivalent per element |
    | **Closed 500 ms all-IDR GOP** | Item 11's self-healing bound; relay caches "last keyframe" and viewers resync on IDRs only | V3 fixture: keyframe cadence ≈ 500 ms, every keyframe IDR |
    | **SPS/PPS in-band before every IDR** | Empty-extradata DecoderConfig ⇒ late-joiner priming decodes only self-sufficient keyframes | `h264parse config-interval=-1` + V3 fixture assertion |
    | **≤1 frame encoder-internal latency** | Decision 6's arrival-stamp bias claim is only true with zero-lookahead configs | V2 trial: frame-in→AU-out < 1 frame period per candidate |
    | **VFR pass-through, drop-only gating** | Portal capture is damage-driven; a CFR converter holds the last frame until the *next* arrives (unbounded on static content) then bursts stale duplicates that arrival-stamping would timestamp "now" | `videorate drop-only=true max-rate=<fps>` (never plain `videorate`/`fps` conversion); V2 review |
    | **Cursor embedded** | The browser path embeds the cursor; silently losing the pointer is a viewer-visible regression | Portal `cursor_mode=embedded` (Decision 5) + V4 manual check |

### GUI decisions

14. **Gio (`gioui.org`), locked.** Native Wayland (no XWayland, no webview),
    pure-Go application code, immediate mode. Build needs the usual Linux
    dev headers (`gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev
    libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev
    libxcursor-dev libvulkan-dev`) — document them in the README, they are the
    honest price (paid by the gawk-broadcast CI job, not the relay's —
    Decision 1). **Rejected**: *Fyne* (runs through XWayland by default;
    `-tags wayland` is mutually exclusive with X11 — harmless here since no
    media touches the toolkit, but Gio is the more interesting and more
    native choice for a project whose stated motive is learning); *Wails v3*
    (wants WebKitGTK 6.0, `-tags gtk3` for older distros, and WebKitGTK's
    DMA-BUF renderer has documented crashes on **Wayland + NVIDIA** — our
    exact target combo, worked around by disabling DMA-BUF; also puts a
    browser engine back inside the binary we are building *because* the
    browser can't do the job, and the R6 design-system reuse it promises is
    thinner than it looks: `BroadcasterScreen` is built around
    `BroadcastPipeline` + Zustand, so only tokens and a few primitives would
    carry over); *GTK4/gotk4* (large cgo API surface for a window with six
    controls).
15. **The window is the app; closing it ends the broadcast.** No tray, no
    background presence, no hidden state — if the window is gone, nothing is
    publishing. This is the simple, predictable behavior, and it is the one
    decision a tray would later overturn (see Deferred below), so it is worth
    naming rather than leaving implied.
16. **No preview.** You are looking at your own screen, and the portal picker
    already showed you what you're sharing. The browser broadcaster has a
    preview only because a tab isn't your screen. What people actually need is
    *"am I live and are frames moving"*, which a sent-fps readout and a
    heartbeat indicator answer at ~zero cost. Revisit only if V7's
    verification shows people genuinely can't tell.
17. **Notifications via `godbus` directly, with urgency levels — critical
    urgency for failures.** *Revised 2026-07-15.* The review surfaced a
    trap the original design walked straight into: **KDE's portal explicitly
    inhibits notifications while a ScreenCast session is active**
    (xdg-desktop-portal-kde MR !33), and Plasma can auto-enable
    do-not-disturb for mirrored screens and fullscreen apps — so the
    original plan's "only thing that reaches you once you're fullscreen in a
    game" was suppressed *by the act of broadcasting*, on KDE, by default.
    The freedesktop notification spec's **critical urgency** bypasses DND on
    Plasma by default and is shown over fullscreen on GNOME, so: **went
    live** (with the code) and **first frames flowing** are normal urgency
    (fine if they're swallowed mid-game); **subprocess died / publish error /
    broadcast ended unexpectedly** are **critical** — a child-process death
    you don't learn about for an hour is the failure mode notifications
    exist to prevent. `gioui.org/x/notify` is rejected
    (experimental *and* no urgency control); the D-Bus `Notify` call with an
    urgency hint is a page of code behind the same one-method interface, so
    the dependency delta is zero (`godbus` is already in for the portal).
    V6's acceptance criterion is written against the trap: the critical
    notification must be *seen* while a portal screencast is active **and** a
    fullscreen app is focused, on both KDE and GNOME.
18. **No viewer count, no "first viewer joined" notification.** *New
    2026-07-15, replacing features the original design promised.* Verified
    against the code: nothing on the wire tells a publisher about
    subscribers — the browser's `BroadcastStats` has no viewer count, the
    announce stream is one-shot, and `/statusz` is ops-only (cluster-internal
    TCP listener, HMAC-obfuscated broadcast keys a broadcaster cannot map to
    its own ID). Implementing either feature means a relay→publisher wire
    message — a protocol change R14's headline explicitly rules out. Dropped
    for browser parity (the browser broadcaster doesn't know its viewer count
    either). A `SubscriberCount` message is a fine *future* roadmap item —
    for both broadcasters at once, as its own wire+relay change — but it is
    not smuggled in here.
19. **Settings persist to `~/.config/gawk/broadcast.json`** (mode 0600):
    relay URL, **app URL** (the frontend origin for "join:" links and Copy
    link — it is *not derivable* from the relay URL, they are different
    hosts), publish secret, rung, encoder override, last broadcast code, and
    last-good encoder (Decision 4). **No portal restore token is stored**
    (Decision 5, as reversed 2026-07-16 — the choice of screen is deliberately
    never persisted). The file is still 0600 because the publish secret is a
    credential. The GUI writes it; CLI flags override it. A pre-shared secret
    in a local file is consistent with how it already travels (a query param,
    per R2 — the WebTransport JS API can't set headers).
20. **Stats parity where it's honest, `n/a` where it isn't.** The engine
    reports what it can see: chosen encoder, encoder fps, sent fps,
    datagrams, keyframe streams sent/failed/superseded, frames dropped at
    send (Decision 12), bytes, TimeSync RTT. It **cannot** see pre-encode
    capture fps — the GStreamer child owns that stage — so R9's funnel is
    reported with that stage marked unavailable rather than fabricated from
    the requested rate. Copy diagnostics JSON, matching R9's
    remote-troubleshooting story.
21. **Vulkan Video (`VK_KHR_video_encode_h264`) is the target encode API,
    reached in two stages.** It is the only encode API that is genuinely
    vendor-neutral: RADV (AMD), ANV (Intel) and NVIDIA's proprietary driver
    all implement it — NVIDIA was among the first — with no
    `nvidia-vaapi-driver`-shaped asterisk. Choosing it is the same instinct
    that picked WebTransport + WebCodecs over a mature WebRTC path:
    standards-track, newer, lower-level, more interesting.
    **Direct VAAPI is rejected outright, and the reason matters**: Chromium's
    Linux encode backend is VAAPI-only, and that is *exactly why* it cannot
    hardware-encode on NVIDIA. Building gawk on direct VAAPI would spend weeks
    reproducing the precise limitation R14 exists to escape. VAAPI is
    vendor-neutral for Intel and AMD and structurally excludes the third
    vendor — it is *less* portable than the cascade we already have, not
    more.
    - **Stage 1 (V2)**: `vulkanh264enc` leads the runtime cascade (user
      decision 2026-07-15 — the target API gets exercised on every capable
      machine from day one), and — decoupled from capture entirely —
      **mainline ffmpeg's `h264_vulkan` (≥7.1) generates the reference
      bitstream** from a committed y4m fixture on the development machine.
      The reference needs no portal, no patches, and no GStreamer: it is one
      offline command.
    - **Stage 2 (V8)**: direct Vulkan Video encode in Go, **gated on Stage 1
      establishing that this machine's driver encodes via Vulkan Video at
      all**, with the archived reference as the differential oracle.
    The staging is not timidity, it is what makes the work tractable: without
    a reference bitstream, "my encoder emits garbage" is near-undebuggable,
    and with one, ffmpeg's `vulkan_encode_h264.c` becomes an executable spec
    to port against. If Stage 1 fails, Stage 2 was never possible either —
    and that is a ten-minute finding instead of a week-long one.
    **Vulkan Video being the target API does not make it the only path.**
    Per Decision 4 this is a multi-broadcaster project on unsurveyed
    hardware: a friend's GPU may have no Vulkan Video encoder, an older
    driver, or a broken one — NVENC and VAAPI stay behind it, permanently.
    V8 shipping does not retire V2.

## Architecture

```
gaming PC (Linux)                                              relay
─────────────────                                              ─────
  cmd/gawk-broadcast-gui (Gio)      cmd/gawk-broadcast (CLI)
    window · notify(urgency)          flags · stderr stats
            └──────────┬───────────────────────┘
                       ▼
     internal/engine — Session{Start,Stop} + Callbacks + StartError{Phase,Status}
        │
        ├─ internal/portal (godbus): CreateSession → SelectSources(cursor=embedded,
        │      no persist) → Start → node id → picker every start →
        │      OpenPipeWireRemote → fd ──┐
        │                                ▼ (ExtraFiles → child fd 3)
        ├─ internal/gst: gst-launch-1.0 subprocess
        │      pipewiresrc fd=3 path=N                          (path, not target-object)
        │        ! videorate drop-only=true max-rate=60      (gate, never CFR)
        │        ! <hw convert/scale>                        (stays on GPU)
        │        ! vulkanh264enc | nvh264enc | vah264enc     ← trial-probed cascade
        │        ! h264parse config-interval=-1              (SPS/PPS every IDR)
        │        ! mpegtsmux ! fdsink fd=1
        │                │ stdout pipe (MPEG-TS)
        │                ▼
        ├─ internal/mpegts: PUSI → one PES = one AU (+ PTS, kept for Decision 6's
        │      upgrade path); arrival-stamp on the TimeSync clock
        │                │
        │                ├─ IDR in AU? ──yes─▶ wire.AppendStreamFrameHeader
        │                │        │             + embedded DecoderConfig + payload
        │                │        │             └─▶ OpenUniStream (≤1 in flight,
        │                │        │                 supersede) ────────────▶ CONNECT
        │                │        └──no──▶ chunk ≤ wire.MaxChunkPayload      /publish
        │                │                 wire.AppendVideoChunk               [?secret=]
        │                │                 └─▶ SendDatagram (drop frame
        │                │                     remainder on failure) ──────▶
        │                ├─ SPS parser ─▶ "avc1.42E02A" ─▶ wire.AppendDecoderConfig
        │                └─ TimeSync ping (2 s) ─▶ offset ─▶ ClockMapping (5 s)
        └─ AcceptUniStream ─▶ wire.ParseBroadcastAnnounce ─▶ code ─▶ shells
```

Reused verbatim from `gawk-server/wire` (public as of V0): `AppendVideoChunk`,
`AppendDecoderConfig`, `AppendStreamFrameHeader`, `AppendTimeSync`,
`ParseTimeSync`, `AppendClockMapping`, `ParseBroadcastAnnounce`, and the
constants (`MaxDatagramSize` 1200, `MaxChunkPayload` 1180, `MaxChunkCount`
3000, `StreamFrameHeaderSize` 24, `TimeSyncSize` 18, `ClockMappingSize` 10,
`MaxKeyframeBytes` 8 MiB).

## The capture path, end to end

The shape to hold in your head: **the picture stays on the GPU the whole way,
and our Go code never touches a pixel.** By the time anything reaches our
process it is already compressed H.264 — the app is a byte pump, and that is
why it can be simple.

1. **You click Start.** The app dials `/publish` *first*, before any capture.
   Same rule as the browser (Decision 10 / connect-before-picker): if the
   relay refuses, no share dialog should ever have appeared.
2. **The app asks your desktop for a screen — in Go, over D-Bus.** The XDG
   ScreenCast portal shows **your** desktop's own share dialog (KDE, GNOME).
   We don't draw it, we don't theme it. **This happens on every start — the
   choice is never persisted, so you pick what to share each run** (Decision 5,
   as reversed 2026-07-16).
3. **The portal returns a PipeWire stream** (a node id and an fd). Frames
   arrive as **dmabufs** — handles to GPU memory, not pixels. Nothing has
   been copied anywhere yet.
4. **The app spawns GStreamer** as a child process, handing it the PipeWire
   fd and taking a pipe on its stdout.
5. **GStreamer rate-limits (drop-only — never holding frames to fake CFR),
   scales and converts on the GPU.**
6. **GStreamer encodes on the GPU's dedicated encode block** (Vulkan Video /
   NVENC / VAAPI) — not the CPU, and not the shader cores either, so it isn't
   taking from the game's frame budget. This is the step the browser cannot
   do on Linux, and the entire reason R14 exists. There is **no software
   fallback**: a machine where no hardware candidate passes its trial is
   told to use the browser broadcaster instead (Decision 4).
7. **Compressed bytes come down the pipe as MPEG-TS.** *Now* the data is in
   CPU memory — but it's ~1% of the size, because it's already H.264.
8. **The app splits the TS into access units.** One PES packet = one frame
   (`payload_unit_start_indicator` marks the boundary — no start-code
   scanning heuristics), with the PES PTS kept on the side (Decision 6's
   upgrade path).
9. **The app stamps arrival time** from the same monotonic clock TimeSync
   uses (Decision 6 — and the reason that clock must be shared is that
   `ClockMapping` otherwise relates two unrelated timelines).
10. **The app reads the NAL types** in the AU. Contains an IDR (type 5)?
    Keyframe — and thanks to `config-interval=-1` it is self-sufficient
    (SPS/PPS inside), which is what the relay's cached-keyframe priming
    needs. On the first SPS it also parses `avc1.42E02A` (Decision 8) — the
    only thing telling the viewer's decoder what it's about to get.
11. **The road forks — this is R8's design, unchanged**: a **keyframe** goes
    out whole on one **reliable uni stream** (too big and too important to
    lose to one dropped packet; ≤1 in flight, newest supersedes), while a
    **delta** is sliced into ≤1180-byte **datagrams** (fast and lossy — lose
    one and you lose a frame, not a GOP; a mid-frame send failure drops the
    frame's remaining chunks, Decision 12).
12. **The relay fans out** to viewers exactly as it does for the browser
    broadcaster, because the bytes are identical.

Alongside all this, two small side channels: a TimeSync ping every 2 s (which
yields the relay-clock offset) and a ClockMapping publish every 5 s (which
turns that offset into the viewer's absolute capture→render latency).

**The one place the picture might touch the CPU** is step 6 on NVIDIA: the
portal hands out dmabuf frames and `nvh264enc` wants GL/CUDA memory, and
cross-device dmabuf import is historically unreliable on the proprietary
driver. Expected resolutions in order: dmabuf→GL/CUDA import if it works;
else a system-memory roundtrip (`videoconvert`-style — still *hardware
encode*, just with a copy tax; measure it at 1080p); else the machine is
AMD/Intel after all and `vah264enc`/`vulkanh264enc` take it. This is the risk
V2 exists to settle — same corner as the original design, new spelling.

## Dependencies (the honest bill of materials)

Runtime (all stock distro packages; **nothing is built from source** — this
is a V2 acceptance criterion, and the property whose absence sank the
original capture plan): `gstreamer1.0-tools`, `gstreamer1.0-pipewire`,
`gstreamer1.0-plugins-base`, `-good`, `-bad` (Vulkan/nvcodec/va encoder
elements; the nvcodec plugin `dlopen`s the NVIDIA driver's libraries at
runtime, so no special build), plus the desktop's own `xdg-desktop-portal`
backend, which every Wayland desktop already ships. Build-time: Go, plus
Gio's header list (Decision 14). `godbus` is pure Go.

## Chunks

| Chunk | Status |
|---|---|
| V0 — module split + wire promotion + CI/release wiring | ✅ implemented 2026-07-15 |
| V1 — engine: session surface + transport | ✅ implemented 2026-07-15 (announce read detached — see [Implementation notes](#implementation-notes-deviations-from-this-design)) |
| V2 — engine: portal + capture + encode cascade | ✅ implemented 2026-07-15; **the cascade itself is unverified until it runs on the gaming PC** |
| V3 — engine: bitstream + send policy | ✅ implemented 2026-07-15 |
| V4 — CLI shell + end-to-end + the timestamp-bias gate | ✅ shell implemented 2026-07-15; **the bias gate is a manual measurement, still pending** |
| V5 — GUI shell: window | ✅ implemented 2026-07-15 |
| V6 — GUI: stats panel, copy diagnostics, notifications | ✅ implemented 2026-07-15 |
| V7 — docs + escape hatches | ✅ implemented 2026-07-15 |
| V8 — direct Vulkan Video encode (Stage 2) | 📋 not started — hard-gated on V2's on-hardware result |

Automated gates are green for V0–V7, but read that narrowly: **an automated
gate on this engine cannot see a GPU, a portal, or a desktop.** The criteria
marked `manual` below, and the [Verification plan](#verification-plan-manual),
are the ones that decide whether R14 works.

### V0 — module split + wire promotion + CI/release wiring

Promote `internal/wire` → `gawk-server/wire` (public; it keeps importing
`internal/broadcastid`, which stays internal — the path-prefix rule permits
that); mechanical import rewrite across the relay tree; golden-vector tests
move untouched. Scaffold the `gawk-broadcast/` module (local `replace` on
`gawk-server`), add its release-please component and a CI job that installs
the Gio headers (Decision 14) and runs `go vet` + `go test -race` for the new
module only. The relay's CI job and Dockerfile are untouched.

| Acceptance criterion | Verified by |
|---|---|
| Relay builds/tests green after the wire promotion; golden vectors byte-identical | CI + `git diff --stat` showing mechanical-only changes in `gawk-server/` |
| `gawk-broadcast` module compiles against `gawk-server/wire` via `replace`; a deliberate wire-constant change breaks its build/tests (Decision 11 demonstrated) | one-off local experiment, noted in the PR |
| Broadcaster-only commits produce **no** gawk-server release entry, image rebuild, or homelab deploy | release-please dry-run / first release PR inspected |
| Relay CI job still needs no GUI headers | CI config review |
| CLAUDE.md / README / docs references to `internal/wire` synced | review |

### V1 — engine: session surface + transport

`internal/engine`: the `Session` type (`Start`/`Stop`) + `Callbacks` struct +
`StartError{Phase, Status}` (Decision 10), all injectable-clock for tests.
Dial `/publish` or `/publish/{id}` via `webtransport.Dialer`
(`TLSClientConfig` with system roots; `-insecure` for the dev cert); read the
first incoming uni stream (bounded: ≤258 bytes, with a deadline) →
`ParseBroadcastAnnounce` → `OnBroadcastID`.

TimeSync client mirroring `time-sync.ts` exactly: ping every
`TIME_SYNC_INTERVAL_MS` (2 s — the relay rate-caps at 5/s per session, a
constant, so this must not become a knob), keep the **lowest-RTT sample of
8**, `offsetUs = serverTimeUs - (t0Us + rttUs/2)`. Frame timestamps and the
TimeSync `t0` must read the **same monotonic clock** — `time.Since(start)` in
µs, the direct analogue of the relay's own `relayNowUs()` and of the
broadcaster's `performance.now()` — or `ClockMapping` publishes a mapping
between two unrelated timelines and the viewer's absolute latency is nonsense.
Publish `AppendClockMapping(offsetUs)` every `CLOCK_MAPPING_INTERVAL_MS` (5 s),
nothing before the first pong.

| Acceptance criterion | Verified by |
|---|---|
| `Session` surface mirrors `BroadcastSessionLike`/`BroadcastCallbacks` (minus ladder, Decision 9); no package globals, no `os.Exit`, no direct stdio — a shell could be anything | review + the fact that V4 and V5 both consume it unmodified |
| Dials `/publish`, reports the announced code; `-id` dials `/publish/{id}` and reclaims | integration test against an in-process `transport.Server` |
| `StartError.Phase` distinguishes `connect` from `capture`; `StartError.Status` carries the HTTP status when present; a capture-phase failure tears the session down (no zombie publisher) | integration test |
| Secret sent as `secret` query param; wrong/missing secret surfaces as a 401-bearing, human-readable error | integration test |
| Announce read is bounded and deadlined; a relay that never sends one fails `Start` cleanly | unit test with a fake session |
| TimeSync lowest-RTT-of-8 selection matches `time-sync.ts` on identical sample sequences | unit test with a fake clock + shared fixture |
| ClockMapping sent only after the first pong, then every 5 s | unit test with a fake clock |
| Frame-timestamp clock and TimeSync `t0` clock are the same source | unit test asserting a single injected `now()` feeds both |

### V2 — engine: portal + capture + encode cascade

`internal/portal`: the ScreenCast handshake over `godbus` (Decision 5) —
session, source selection (cursor embedded, **no persist**), start,
`OpenPipeWireRemote`. The picker appears every start; no restore token is
requested or stored.
`internal/gst`: pipeline construction per encoder; the **Vulkan-first
hardware-only cascade** (`vulkanh264enc` → `nvh264enc` → `vah264enc` →
refusal, Decision 4) with `videotestsrc` trial encodes, last-good caching,
live-start-as-final-probe; child supervision (stderr relayed at debug level,
non-zero exit → clean shutdown carrying the last stderr lines, never a silent
hang). Reference shape (VA path; exact elements/caps/property names are V2's
job):

```
gst-launch-1.0 -q \
  pipewiresrc fd=3 path=<node> \
  ! videorate drop-only=true max-rate=60 \
  ! vapostproc ! 'video/x-raw(memory:VAMemory),format=NV12,width=1920,height=1080' \
  ! vah264enc rate-control=cbr bitrate=8000 key-int-max=30 b-frames=0 \
  ! h264parse config-interval=-1 \
  ! mpegtsmux ! fdsink fd=1
```

**Known risk, to be settled here**: NVIDIA dmabuf import (see "the one place
the picture might touch the CPU"). **Stage-1 Vulkan gate (Decision 21)**:
whether this GPU encodes via Vulkan Video at all is established here, plus
the offline `h264_vulkan` reference for V8.

| Acceptance criterion | Verified by |
|---|---|
| Portal handshake yields a stream fd + node id; **`persist_mode`/`restore_token` are never sent, and the picker appears on every start** | manual on KDE + GNOME; unit tests over a fake D-Bus (`TestNeverRequestsPersistence`, `TestPickerAppearsOnEveryStart`) |
| A cancelled picker is the typed `ErrCancelled` outcome, distinguishable from any other failure | unit test over a fake D-Bus |
| Cascade picks the first candidate that *actually encodes*; a listed-but-broken element is rejected and the next tried; trials use `videotestsrc`, never the portal | unit test over an injected trial-encode runner |
| Cached last-good encoder is re-verified first; full cascade only on its failure or first run | unit test |
| Live-start failure advances the cascade and retries **without a new picker** (the in-memory portal session granted for this Start is reused) | manual + unit test over a fake child |
| **No hardware candidate ⇒ refusal with a sentence pointing at the browser broadcaster** — never a software fallback, never a stack trace | unit test (all candidates fail) + manual copy review |
| Every invariant in Decision 13's table holds for every accepted candidate (no B-frames, all-IDR 500 ms GOP, SPS/PPS per IDR, ≤1-frame latency, drop-only gating) | V2 trial checks + V3 fixture assertions |
| Chosen encoder + rung logged once and surfaced on `Callbacks`; child death surfaces as `OnError` with its last stderr, never hangs | unit test with a fake failing child |
| Encoder override forces one candidate and skips the cascade | unit test |
| At the chosen rung, no dropped frames and the encoder is confirmed hardware (`nvidia-smi` / `intel_gpu_top` / `radeontop` shows encode-engine activity) | manual, on the gaming PC |
| **Every runtime dependency resolves from stock distro packages** — a missing element produces a sentence naming the package to install, not a stack trace | manual on a clean container/VM |
| Stage-1 Vulkan result recorded (does this GPU encode via Vulkan Video?) and an `h264_vulkan` reference bitstream generated **offline with mainline ffmpeg** from the committed y4m fixture and archived | manual: `vulkaninfo | grep -i video_encode`, the trial encode, one ffmpeg command; fixture + reference committed or noted in the V8 task |

### V3 — engine: bitstream + send policy

`internal/mpegts`: minimal TS demux (188-byte packets, PAT/PMT, single video
PID, PUSI-delimited PES with `PES_packet_length = 0` handling, PTS extraction)
→ one AU per PES, accumulation bounded by `wire.MaxKeyframeBytes`. `sps.go`:
parse profile_idc/constraint_flags/level_idc → `avc1.XXXXXX` (EPB-safety note
in a comment, Decision 8). `send.go`: IDR-bearing AU → one `StreamFrame` uni
stream with embedded `DecoderConfig`, ≤1 in flight with supersede
(Decision 12); other AUs → chunk to `MaxChunkPayload` and `SendDatagram`,
with the Decision 12 failure policy (drop frame remainder; MTU re-chunk on
`DatagramTooLargeError`). Frame IDs from 0, wrapping at uint32.

| Acceptance criterion | Verified by |
|---|---|
| Demux emits exactly one AU per encoded frame across a recorded fixture, including TS packets split across reads and `PES_packet_length = 0` | unit test over a captured fixture from the real V2 pipeline, byte-by-byte feed at adversarial read sizes |
| An AU exceeding `MaxKeyframeBytes` errors, never allocates unboundedly | unit test |
| IDR detection matches the fixture's known keyframe positions; every keyframe AU contains SPS+PPS before the IDR slice (Decision 13) | unit test |
| Fixture contains no B slices; keyframe cadence ≈ 500 ms (Decision 13) | unit test over the fixture |
| SPS parse yields the codec string the encoder reports for the same fixture | unit test |
| Delta chunking round-trips against the existing reassembly logic | golden vectors from `wire_test.go` + a chunking unit test |
| quic-go send-failure semantics pinned in a test; mid-frame failure drops the frame remainder and counts it; `DatagramTooLargeError` lowers chunk size and re-chunks rather than dropping the frame | unit tests with a fake session |
| A keyframe stream stalled past the next keyframe is `CancelWrite`-ed and superseded (≤1 in flight) | unit test with a fake session |
| Frame IDs start at 0 and wrap at uint32 | unit test |

### V4 — CLI shell + end-to-end + the timestamp-bias gate (Decision 6)

`cmd/gawk-broadcast/`: flags (`-url`, `-app-url`, `-id`, `-secret`,
`-insecure`, `-resolution`, `-fps`, `-bitrate`, `-encoder`) + `GAWK_*` envs
over the V1–V3 engine; periodic stderr stats line. Then **measure the bias**:
compare the viewer overlay's `Latency (capture→render)` against a physical
reference (the `docs/13` bottleneck-playbook method — a clock on the captured
screen, photographed alongside the viewer) **at the shipped default,
1080p60** (the original doc gated at 1080p30 while defaulting to 60 — the
gate must test what ships).

| Acceptance criterion | Verified by |
|---|---|
| A Chrome viewer joins by code and renders; overlay reads `Renderer: WebGL`, `Transport: Worker` | manual |
| Overlay shows live `Live-edge drift`, a plausible `Latency (capture→render)`, populated `RTT (time-sync)` | manual |
| **Measured latency under-reports the physical reference by < 15 ms, and the gap is stable across a 60 s window, at 1080p60 on the chosen hardware encoder** | manual, photographed reference — **if this fails, Decision 6's clock-anchored-PTS path is required before R14 is done** |
| Keyframes arrive as streams, deltas as datagrams; `/statusz` + `/metrics` show the broadcast with ingress loss ≈ 0 on LAN | manual |
| Relay restart → reclaim by `-id` works; viewer recovers without a terminal 4000 | manual |
| The cursor is visible in the stream (Decision 13) | manual |
| Game fps at 1080p60 broadcast is materially better than the software-encode browser baseline on the same machine | manual — the whole point of R14 |

### V5 — GUI shell: window

`cmd/gawk-broadcast-gui/`: Gio window over the same engine. Settings (relay
URL, app URL, secret, resolution, fps, encoder override) loaded from and
saved to `~/.config/gawk/broadcast.json` (Decision 19); big Start/Stop; the
broadcast code with **Copy link** (built from the app URL); **Resume
broadcast** using the remembered code (Decision 10's reclaim→mint rule — which,
per Decision 5 as reversed, re-shows the share picker like any start); live
state and errors in plain language (the `StartError.Status` distinctions
surfaced as sentences). No source picker of its own — the desktop's portal is
the picker (Decision 5), no preview (Decision 16), no tray (Decision 15 —
closing the window stops the broadcast), no viewer count (Decision 18).

| Acceptance criterion | Verified by |
|---|---|
| Start → portal picker → code shown → viewer joins. Restart or Resume → picker **again** (never persisted, Decision 5 as reversed) → live | manual |
| Engine consumed unmodified from V1 (no engine changes needed to add a GUI) | review of the diff: `internal/engine` untouched |
| Settings round-trip the config file; file is 0600; flags still override in the CLI | unit test + manual |
| Resume dials `/publish/{id}`; a `connect`-phase failure offers minting, a `capture`-phase failure does not | unit test over a fake engine |
| Errors surface as sentences, not Go error strings — 401/404/409/429 each say something a friend can act on; a portal/GStreamer failure says what to install or check | manual + copy review |
| Closing the window stops the broadcast and leaves no publisher on the relay | manual + `/statusz` |

### V6 — GUI: stats panel, copy diagnostics, notifications

Stats panel over the engine's `OnStats` (encoder, fps, bitrate, datagrams,
keyframe streams sent/failed/superseded, frames dropped at send, TimeSync
RTT), with capture-fps marked unavailable rather than fabricated
(Decision 20), plus **Copy diagnostics** JSON matching R9's shape closely
enough to be pasted next to a viewer's. Notifications per Decision 17:
`godbus` `Notify` with urgency behind a one-method interface — critical for
subprocess death / publish error / unexpected end, normal for went-live /
frames-flowing.

| Acceptance criterion | Verified by |
|---|---|
| Panel reflects a live session; numbers agree with `/statusz` for the same broadcast | manual |
| Pre-encode capture fps reads `n/a`, never a fabricated number | review + manual |
| Copy diagnostics produces JSON pasteable beside a viewer's Copy diagnostics | manual |
| **A critical-urgency death notification is *seen* while the portal screencast is active and a fullscreen app is focused — on KDE (whose portal inhibits normal notifications during screencast) and on GNOME** | manual on both desktops — the trap Decision 17 exists for |
| Normal-urgency notifications may be swallowed mid-game; critical ones must not be | manual |
| Notification transport swappable without touching call sites | review |

### V7 — docs + escape hatches

README (quickstart, the runtime package list from the Dependencies section,
the Gio build dependencies from Decision 14, the Linux broadcaster section,
and the **Annex-B/AVCC gotcha** — a native publisher exercises the viewer's
Annex-B branch, which the browser never does), CLAUDE.md build-order entry +
docs list, ROADMAP R14 status. Record the escape hatches evaluated but not
built — kmsgrab, NvFBC, gpu-screen-recorder — so a future reader doesn't
rediscover them: see [Escape hatches](#escape-hatches-evaluated-not-built).

| Acceptance criterion | Verified by |
|---|---|
| README gotcha list names the Annex-B publisher path | review |
| Runtime + build dependencies documented (they are the honest price of Decisions 3, 5 and 14) | review |
| CLAUDE.md + ROADMAP reflect R14 status | review |
| Escape hatches recorded with their trade-offs; pipewiregrab's rejection recorded so nobody re-proposes it unchecked | review |
| All gates green (both modules' `go test ./...`, `go vet`, lint; frontend untouched) | CI |

### V8 — direct Vulkan Video encode (Stage 2, gated on V2)

**Entry gate**: V2 established that this machine's driver encodes H.264 via
Vulkan Video (the `vulkanh264enc` trial and/or the offline `h264_vulkan`
run) and archived a reference bitstream from the committed y4m fixture. If it
didn't, this chunk does not start — direct Vulkan would fail for the same
reason, and the cascade's `nvh264enc`/`vah264enc` are the answer instead.

**This chunk adds a candidate; it does not replace the stack.** Other
broadcasters run GPUs we can't survey (Decision 4), so direct Vulkan sits at
the top of the same runtime cascade (above `vulkanh264enc`) and every
delegated backend stays behind it, permanently. V8 shipping does not retire
V2.

Replace the encode half only: portal + `pipewiresrc` capture stay (the child
shrinks or, eventually, the dmabuf is imported directly), and `send.go` + the
session are untouched — the V1–V3 seams are what make this a contained swap
rather than a rewrite. Work: generate the `VK_KHR_video_encode_queue` /
`VK_KHR_video_encode_h264` bindings from `vk.xml`; create the video session +
session parameters (SPS/PPS built by us); dmabuf → `VkImage` import via
`VK_EXT_external_memory_dma_buf` + `VK_EXT_image_drm_format_modifier`; DPB and
reference-list management; rate control; drain coded buffers to Annex-B.

**Known risks, stated plainly because they set the schedule, not the typing**:

- **Go's Vulkan bindings are unmaintained and predate Vulkan Video** — the
  encode extensions are simply not in them, so binding generation from the
  registry is step zero, not a given.
- **No Go Vulkan Video encoder is known to exist.** Zero prior art: the
  references are all C (ffmpeg's `vulkan_encode_h264.c`, GStreamer's, NVIDIA's
  samples). This is the condition under which AI assistance is *least*
  effective, which is exactly why the reference bitstream from V2 is the
  entry gate — with an oracle this is a port, without one it is a search.
- **The driver floor is recent and moving**: Mesa 26.0 only just landed a
  large RADV video-encode correctness batch (DPB sizes, alignments, reference
  management). Expect to be testing against a target that is still settling,
  and expect failures to surface as `VK_ERROR_DEVICE_LOST` or silent garbage
  rather than helpful errors.
- **Capture is unaffected** — this chunk buys the encode API, nothing else.

| Acceptance criterion | Verified by |
|---|---|
| On the committed y4m fixture, direct-Vulkan output **decodes cleanly** (reference decoder / `VideoDecoder`), its **PSNR against the source is within a stated ε of the archived reference's PSNR**, and its structure is sane (SPS fields, all-IDR 500 ms GOP, NAL pattern) — *not* byte- or frame-identity, which differing encoder decisions make unachievable even for a perfect implementation | differential test against the fixture + archived reference |
| Chosen encoder reports `vulkan-direct` in stats; the delegated cascade remains available as fallback at runtime | manual + unit test |
| A Chrome viewer renders the direct-Vulkan stream with no corruption over a 10-minute heavy-motion session | manual |
| GPU encode-engine activity confirms hardware use (not a software fallback inside the driver) | manual |
| Game fps is no worse than the V4 delegated baseline at the same rung | manual — if it is, the whole exercise lost its point |

## Implementation notes (deviations from this design)

Three places where the implementation departs from what is written above,
plus field findings from real hardware. Recorded rather than quietly
absorbed, so the doc stays trustworthy as the account of what was built.

**1. The announce read is detached, not inline in `Start` (V1).** The design
says `Start` reads the first uni stream → `ParseBroadcastAnnounce` →
`OnBroadcastID`, and V1's criterion says "a relay that never sends one fails
`Start` cleanly". It doesn't: `Start` returns once the session is dialed, and
a goroutine reads the announce and fires `OnBroadcastID` when it arrives. The
reason is the rule docs/06 already established for the browser and the R1
review re-confirmed: the announce is *not* part of connect success — waiting
on it would make a slow-to-announce relay indistinguishable from a refused
connect, and collapse `StartError.Phase` (Decision 10), which the reclaim→
mint rule depends on. The bound and the deadline are kept
(`announceReadLimit` 258 B, `announceReadTimeout` 10 s) and the failure
surfaces via `OnError` instead. This bit back once: `Stop()` hung waiting on
the detached goroutine parked in `io.ReadAll`, fixed with a
`context.AfterFunc` that sets a past read deadline — covered by
`TestStopIsPromptWhileAnnounceReadIsPending`.

**2. The MPEG-TS fixture is ffmpeg-generated, not pipeline-captured (V2/V3).**
`internal/mpegts/testdata/sample.ts` comes from `videotestsrc`'s ffmpeg
equivalent (`testsrc2` → `libx264` → `mpegts`, 60 frames, GOP 15, no
B-frames, `repeat-headers=1`), not from a real capture+encode run. CI has no
GPU and no portal, so a live-pipeline fixture could not be regenerated or
trusted there. This is honest for what the fixture tests — TS/PES demuxing
and SPS parsing are container- and bitstream-level, not vendor-specific — and
the parts that *are* vendor-specific (dmabuf import, encode-block use,
in-band SPS/PPS from the real `h264parse`) are exactly what the manual
verification plan below covers. The fixture's provenance command is recorded
in `testdata/README.md`; its SPS was independently checked to be
`67 42 c0 0d` → `avc1.42C00D`.

**3. Integration tests run the real `gawk-server` binary, not an in-process
`transport.Server` (V1/V4).** V1's criteria name an in-process server, which
Decision 1 then made impossible: the module split puts `gawk-server`'s
`internal/transport` out of reach by Go's own rule. Building and running the
real relay binary (plus `gawk-devcert`) is not a workaround but the better
test: a hand-written fake would only assert the engine against *our belief
about the relay*, which is the belief most worth doubting in a second
implementation of a protocol. The tests attach a real subscriber and check
what actually comes out — self-sufficient keyframe, `avc1.42C00D` with empty
extradata, Annex-B start codes, IDR present, deltas flowing, ClockMapping
delivered — and cover reclaim/409 and secret/401 against real HTTP statuses.
They are skipped under `go test -short`.

**4. Live capture walks a two-rung capture ladder per encoder (field finding,
2026-07-16).** First real-hardware run (AMD, `vah264enc` trial-passed): the
live pipeline died during preroll inside `pipewiresrc` — `stream error:
unhandled format`, before a single frame reached the encoder. That error is
the PipeWire GStreamer plugin's own: the format the compositor chose for the
screencast stream could not be mapped back onto the downstream caps (known
field causes: DMA-BUF modifier / DRM-caps version skew between
`gst-plugin-pipewire` and the encoder's converter, and 10-bit formats from
HDR desktops). Two consequences in code: (a) `BuildPipeline` takes a
`CaptureMode` — `auto` (free negotiation, DMA-BUF zero-copy when healthy)
then `system-memory` (bare `video/x-raw` pinned at the source, forcing
modifier-less formats and CPU-visible buffers, plus a `videoconvert` after
the rate gate), and the live start tries both rungs per candidate before
advancing the cascade; (b) when *every* rung dies inside `pipewiresrc`, the
failure surfaces as `engine.ErrCaptureFormat` → `CaptureFormatMessage`, not
`ErrNoHardwareEncoder` — blaming the encoder would send the user chasing GPU
drivers over a compositor negotiation problem (the `ErrNoLaunchBinary`
lesson again). The sentinel checks in `app.Message`/the CLI's `userMessage`
were also moved ahead of `StartError` rendering: capture-phase failures
arrive `StartError`-wrapped, and the wrapper was shadowing every curated
message behind "Could not start capture: <Go error chain>". Diagnosis knob:
`GST_DEBUG=pipewire*:5` in the environment reaches the child (env is
inherited) and its stderr lands in the debug log. On-device confirmation
(2026-07-17, KDE-era compositor on a 240 Hz monitor): the compositor answers
the auto rung with `video/x-raw(memory:DMABuf), format=DMA_DRM,
drm-format=AR24, 2560x1440` — the caps map fine ("we got format") and then
`handle_format_change` still finishes with the error, so the failure is in
the finish/allocation step with `videorate` sitting between `pipewiresrc`
and `vapostproc`, not in the format mapping; the system-memory rung
negotiates `BGRA` over MemFd on the same machine and streams. Recovering
zero-copy (likely: rate-gate placement vs DMA_DRM caps) is a follow-up, not
a blocker.

**5. Pumps start with the child, not after the probe window (field finding,
2026-07-17).** The first successful on-device stream began with three
seconds of "dropping access unit: sender is behind": nothing read the
child's stdout until `liveProbe` returned, a pipe buffers ~64 kB ≈ 64 ms of
video, and `fdsink` then blocked — stalling the entire encode pipeline for
the probe window and bursting the stale backlog when the tap opened. The
demux pump now starts the moment the child does (`pumpHandle`); frames
entering the bounded channel before adoption are dropped silently (nobody is
listening until `Start` returns), a losing cascade attempt's pump neither
reports an error nor closes the frame channel, and the channel is flushed
between attempts so two SPS lineages never interleave. Two debugging
by-products: **`GAWK_DUMP_TS=<path>` tees the child's raw MPEG-TS to disk**
(play it with mpv/ffplay — the ground-truth instrument for "black at the
source vs broken on the viewer"), and the child's stderr moved to our own
`os.Pipe` with a reap-then-bounded-wait in `wait()` — `cmd.StderrPipe` is
closed by `Wait` out from under a still-running drain, which truncated the
dying words the capture-vs-encoder attribution reads (a race the `-race`
suite caught), while an unbounded wait deadlocks on a grandchild holding the
write end.

**6. A dropped delta drops the rest of its GOP (field finding, 2026-07-17).**
First real viewer session: video, but unusably corrupted, flashing
recognizable at each IDR — while a `GAWK_DUMP_TS` capture of the same
session played clean. The mechanism: the pump's channel-full drops happen
*before* `sender.send` assigns frameIds, so the wire carries contiguous ids
across the loss and the viewer's freeze-on-gap policy — which polices every
loss *after* frameId assignment (send failures, ingress loss, relay drops,
reassembly) — structurally cannot see the one loss point before it. The
viewer then decodes deltas whose references were never sent: reference-soup
artifacts until the next IDR flashes clean. `Source.offer` now enforces
freeze-over-corruption on that edge: once a delta is dropped, everything
until the next keyframe is dropped too (it would decode against a missing
reference), and a keyframe arriving into a full queue flushes the stale
backlog and takes its place rather than being the thing dropped — under
sustained backpressure the stream degrades to a clean keyframe cadence
instead of garbage. Frames already queued stay: their references were
consumed and sent, so they are connected. Related quality note, verified in
gst-plugins-bad source: `vah264enc` takes `bitrate` in kbps (8000 == 8 Mbps
is correct) but **assumes 30 fps for rate control when caps carry
`framerate=0/1`** — which this VFR pipeline always does; rate-control
calibration under VFR is an open follow-up alongside the zero-copy rung.
Second debug tap for exactly this hunt: **`GAWK_DUMP_H264=<path>`** tees the
demuxed Annex-B elementary stream (pre-policy, playable) — against
`GAWK_DUMP_TS` it isolates the demuxer, and against the viewer it isolates
drops/wire/decode.

**7. Demuxed AUs are cloned before they cross the frame channel (field
finding, 2026-07-17).** The dump comparison from note 6 paid off on its
first outing: both dumps clean, viewer black, and the viewer's Copy
diagnostics showing `configsApplied: 0` with `configLen == 0` on every
keyframe stream. Root cause: the pump handed `au.Data` into the frame
channel as-is, but the demuxer's documented contract (`mpegts.AU`) is that
the slice aliases its internal buffer and is only valid until the callback
returns — the next AU is appended into the *same backing array*. Every
frame the sender had not consumed yet was silently rewritten by the AUs
demuxed behind it. Consequences, all observed in the field: `ensureConfig`
parses the SPS from the *sender's* view of the bytes, found none (the bytes
were by then a later delta), and never derived a DecoderConfig — the viewer
sat on an unconfigured decoder, which swallows chunks silently
(`decoder.ts` guards `state !== 'configured'`), hence black with zero
errors; under motion the recycling scrambled queued frames into reference
soup (a second, independent source of the note-6 artifacts); a static
screen kept the channel drained, so each frame was consumed before its
buffer recycled — "static sharp, motion ruined". Both dumps were clean
because each taps the bytes at a moment they are still valid (TS pre-demux,
ES inside the callback). The fix is `bytes.Clone` at the callback boundary;
the regression test (`TestQueuedFramesSurviveTheDemuxersBufferReuse`)
streams the fixture without consuming, then byte-compares the queued frames
against a standalone demux — the aliasing also trips `-race` outright.

**8. Pipeline v2: nominal-rate caps, GPU-memory handoff, converter on the
portal boundary; default bitrate 16 Mbps (2026-07-17).** Field data (dumps
clean at 24 Mbps, garbage at the default 8) plus the vah264enc source
finding from note 6 led to three deliberate pipeline revisions. (a) **The
encoder caps now carry `framerate=<fps>/1`** even though the stream is VFR:
the portal's caps say `0/1` and vah264enc silently budgets rate control for
30 fps when it sees that — at 60 fps that halves the effective bitrate.
With `videorate drop-only=true` upstream this is *signalling, not CFR*: no
frame is ever synthesized, and capture running under the nominal rate makes
the encoder underrun its bitrate (correct direction — per-frame quality
stays at the 60 fps budget). The old pipeline-test guard treating any
`framerate=` caps as a CFR smell was revised accordingly (drop-only is the
invariant, the caps pin is not the violation). (b) **Auto capture puts the
candidate's converter directly on `pipewiresrc`** (rate gate after it): the
zero-copy rung died in finish/allocation with `videorate` between
`pipewiresrc` and `vapostproc` — the caps mapped, the DMA-BUF buffer-pool
negotiation did not. A GPU convert of a frame that is later dropped is the
price of the direct adjacency, paid by the GPU's video block. Auto mode
also pins the candidate's **GPU memory feature on the encoder caps**
(`memory:VAMemory` / `memory:CUDAMemory` / `memory:VulkanImage`): a bare
`video/x-raw` capsfilter means *system memory*, which silently forced a
download + re-upload round trip between converter and encoder.
System-memory capture keeps its proven shape (gate first — CPU converts
must never touch dropped frames — and bare caps) plus only the framerate
pin. **On-device confirmation (2026-07-17, GST_DEBUG=pipewire*:5):** with
the converter adjacent, the same compositor that previously died at
finish/allocation negotiates `DMA_DRM AR24 2560×1440` end to end —
`handle_format_change` finishes, `pipewirepool` wraps real DmaBuf fds
(14 745 600 bytes = 2560×1440×4), and the stream reaches `streaming`. The
zero-copy follow-up from note 4 is resolved; the system-memory rung stays
as the ladder's fallback. (c) **Default bitrate 8 → 16 Mbps**
(GUI/CLI-overridable). Also new:
`Stats.CapturePath` ("zero-copy"/"system-memory") via a `MediaSource`
method so a broadcaster can see which ladder rung won; measured **keyframe
cadence** (`Stats.KeyframeIntervalMs`, EMA over AU arrival stamps) on the
CLI stats line, GUI and diagnostics — `key-int-max` is a *frame* count
derived from the nominal fps, so damage-driven capture running under 60 fps
stretches the wall-clock GOP proportionally (observed ~650 ms at ~40 fps
against the 500 ms target; gst-launch cannot inject force-key-unit events,
so the honest answer is to measure and display, not to pretend). GUI:
proper dark `material.Palette` (the stock theme is light — text fields and
checkbox labels rendered near-black on the dark background), a bitrate
setting (Mbps, blank = default, clamped [1,100]), and an encode-path
indicator (Vulkan Video / NVENC / VA-API + capture path + rung) visible
without opening Details.

**9. VBR against a bitrate ceiling (2026-07-17).** Field feedback after the
note-8 build: quality good, default bandwidth "a bit too high". The fix is
rate control, not the codec: `avc1.64002A` is already **High profile**
(0x64 = 100) — the profiles above it (High 10, 4:2:2, 4:4:4) change bit
depth/chroma rather than compressing 8-bit 4:2:0 better, and B-frames, the
one big H.264 tool we don't use, are structurally forbidden (Decision 13).
So `vah264enc`/`nvh264enc` moved CBR → **VBR with the configured bitrate as
the ceiling** and a 75% target (`vbrTargetPercentage`): typical motion
averages ~12 Mbps of the 16 default, complex scenes still burst to the cap,
static screens spend ~nothing (damage-driven capture already sends no
frames). Property spelling differs per element and is easy to invert:
va's `bitrate` is the **max** with `target-percentage` the fraction; nv's
`bitrate` is the **target** with `max-bitrate` the cap. `vulkanh264enc`
stays CBR deliberately — its property surface is unverified on hardware
(same reason it pins no GOP arg) and a wrong enum would reject the lead
candidate at launch. The real next step in compression efficiency is a
codec change (the wire is codec-agnostic — the browser negotiates VP8/VP9
today — so `vaav1enc`-based AV1 is a plausible future roadmap item, needing
its own config-derivation path since the SPS parse is H.264-specific), not
an H.264 profile change.

**10. Resolution + framerate in the GUI; the 240 Hz observation
(2026-07-17).** The GUI settings card gained stream resolution
("WxH", blank = 1920x1080) and framerate-cap (blank = 60, clamped ≤240)
fields beside bitrate — same blank-means-default semantics, persisted to the
same config the CLI flags override; the WxH parser moved to
`engine.ParseResolution` so the two shells share it. Context that prompted
it: the broadcaster's display is 2560×1440@240 — the compositor advertises
`max-framerate=239/1` and delivers damage at up to that rate, so in auto
capture (converter before the rate gate, the price of zero-copy adjacency)
`vapostproc` may convert up to ~4× the frames the 60 fps gate keeps. That
cost lands on the GPU's video block; the lever is negotiating
`max-framerate` down in pipewiresrc's caps. **Implemented same day as a new
leading ladder rung** (`CaptureAutoCapped`, ladder now auto-capped → auto →
system-memory): a capsfilter directly on pipewiresrc pinning
`video/x-raw(ANY),max-framerate=<fps>/1` — features ANY so DMA-BUF stays
negotiable, and a passthrough capsfilter proxies the allocation query that
videorate once swallowed. pipewiresrc translates the field into the SPA
format's maxFramerate, which KWin/Mutter honor by throttling delivery at
the source; a compositor that refuses fails the rung's preroll and the
ladder falls to plain auto — the exact pipeline verified on device.
`CapturePath` distinguishes the rungs ("zero-copy (capped)" vs
"zero-copy"); on-device verify pending.

## Verification plan (manual)

On the Linux gaming PC: launch the GUI → Start → desktop portal picker
appears (**every start** — restart the app and confirm the picker appears
**again**, since the choice is never persisted, Decision 5 as reversed) →
code shown → join from a Chrome viewer on another machine.
Then, in order: hardware encode confirmed on the GPU's **own** encode counter
(not the pipeline's claim); game fps against the browser-broadcaster baseline
at the same rung; the V4 latency-bias gate at 1080p60; cursor visible;
keyframe streams vs delta datagrams in `/statusz`; Resume across a relay
restart (the picker appears again, as on any start); closing the window mid-broadcast leaves no
publisher behind; a heavy-motion scene for corruption/freeze behavior (the
500 ms GOP and the viewer's freeze-on-gap policy should behave exactly as
with the browser broadcaster — if they don't, the TS demux is the first
suspect); the **no-hardware refusal** message on a machine/VM without a
usable encoder; and the critical-notification drill (kill the GStreamer child
while a fullscreen app is focused) on **both KDE and GNOME** — KDE is the one
whose portal inhibits normal notifications during screencast. Repeat the
portal + notification path on both desktops; smoke-test an X11 GNOME session
(the portal works there; don't gate on Wayland).

## Deferred (wanted, not now)

Cut from scope 2026-07-15 to keep the first version small. All are additive —
none changes the engine, and the research is recorded so it doesn't need
redoing:

- **Tray icon.** Would overturn Decision 15 (window-closed currently means
  not-publishing) and give a background presence for the three hours you're
  in a game. `fyne.io/systray` is the pick when it comes back: D-Bus
  StatusNotifierItem, and the Fyne fork exists precisely to drop the GTK
  dependency, so it doesn't drag Fyne in. Known integration risk: Gio's
  `app.Main()` must own the main goroutine and blocks, and `systray.Run` also
  blocks — on Linux the SNI backend is pure D-Bus, so a goroutine should
  serve, but that needs proving. Note the review's caveat: a tray is *not*
  a mid-game signal either (you can't see it in a fullscreen game) — the
  critical-urgency notification (Decision 17) is that channel; the tray's
  value is background presence, a different feature.
- **Global start/stop hotkey.** Best-effort at best: the XDG GlobalShortcuts
  portal is implemented on KDE and Hyprland, **still blocked on GNOME**, and
  **absent on wlroots** compositors. So it could never have been load-bearing,
  which is a large part of why it's easy to drop.
- **Mid-session rung changes** (`SetRung`): cheap now that restarts don't
  re-prompt the picker (Decision 5), but still a restart with frameId
  continuity and config re-emit to specify — its own small task if wanted
  (Decision 9).
- **A relay→publisher `SubscriberCount` message** (viewer count + "first
  viewer joined" for both broadcasters): a real wire+relay change, benefiting
  the browser broadcaster equally — a future roadmap item, not an R14
  smuggle-in (Decision 18).

### Escape hatches (evaluated, not built)

If PipeWire capture proves untenable on real hardware, these are the routes
already surveyed — recorded with their prices so the evaluation isn't redone
from scratch. None is a free win; each trades away something the portal path
has:

- **kmsgrab** (mainline ffmpeg, X11 *and* Wayland, zero-copy dmabuf → VAAPI).
  The portal-less fallback. Price: needs `CAP_SYS_ADMIN` (a setcap on the
  binary — a real ask for a hobby app) and misses hardware-cursor planes, so
  the cursor disappears unless it's composited back in.
- **NvFBC** (NVIDIA, X11 only). The fastest path that exists. Price:
  X11-only, and on GeForce it needs the well-known driver patch — i.e. every
  friend patching their driver, which Decision 4's own premise rules out.
- **gpu-screen-recorder** as a capture+encode front end. It solves exactly
  this problem, and better than raw pipelines do. Price: **piping its output
  to stdout is not documented** (it targets files and RTMP), so adopting it
  starts with establishing whether a pipe is possible at all — a research
  task before it's an integration task.

**`pipewiregrab` is not on this list and is not an escape hatch** — it is
rejected outright (see the revision note at the top and the Rejected
section). It is not in mainline FFmpeg; mainline ffmpeg has no PipeWire input
at all. Verify it actually merged before re-proposing it.

## Rejected

- **`pipewiregrab` / ffmpeg as the capture vehicle** — not in mainline
  FFmpeg; an unmerged patchset carried downstream by Jami (verified
  2026-07-15 against FFmpeg's filter docs and master's `allfilters.c`).
  Mainline ffmpeg has **no** PipeWire input at all. Do not re-propose without
  first verifying it actually merged. ffmpeg keeps one job here: generating
  V8's reference bitstream offline with mainline `h264_vulkan` (≥7.1).
- **OBS as the media stack** (OBS → local MPEG-TS → Go publisher) — rejected
  by user decision 2026-07-15: the product must be an easy, simple app with
  minimal dependencies and no per-user tweaking; OBS is a heavyweight
  dependency and its own learning surface.
- **Software encode (`libx264`, or any software rung)** — user decision
  2026-07-15: hardware encode is the must-have; the browser broadcaster
  already covers software encode on Linux without issues. The app refuses
  and points there instead.
- **Browser flags on Linux** — the evidence in "Why"; a five-minute test on
  Intel/AMD is fine, but it is not a path.
- **CLI only** — a broadcaster you drive by remembering flag names is not one
  you use before a gaming session. The CLI stays as the engine's harness and
  headless path, not as the product.
- **Fyne / Wails v3 / GTK4** — Decision 14.
- **`gioui.org/x/notify`** — Decision 17: experimental *and* no urgency
  control, and urgency is load-bearing on KDE (screencast notification
  inhibition). Direct `godbus` costs nothing extra — it's already in for the
  portal.
- **Direct VAAPI** — Decision 21. Worth restating because it is the seductive
  wrong answer: VAAPI *looks* like the vendor-neutral Linux standard, and it
  is the exact API whose NVIDIA gap put us here. Chromium is VAAPI-only on
  Linux; that is why Chromium cannot encode on NVIDIA. Vulkan Video is the
  API that actually spans all three vendors.
- **Rust (`wtransport`) or C** — would make a third wire implementation and
  forfeit Decision 11's compile-time coupling. The learning goal is served
  better by the QUIC/Vulkan/Gio work than by a protocol re-port.
- **A local sidecar that hands encoded chunks to the browser page** (native
  capture+encode → localhost WebSocket → page does WebTransport) — all the
  native complexity, plus a browser, plus a hop, to keep a UI we don't need on
  this path.
- **`go-gst` / in-process GStreamer, `go-astiav`** — Decision 3: crash
  isolation is the point of the subprocess.
- **Raw Annex-B over the pipe + AUD splitting** — superseded by Decision 7's
  MPEG-TS framing (structural AU boundaries, in-band parameter sets, PTS for
  free); the original design risked building both framing layers.
- **WebRTC/MoQ** — unchanged from CLAUDE.md.
