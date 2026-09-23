# R56 — Linux in the desktop workspace, and retiring the Go broadcaster

**Status**: designed 2026-09-24; owner decisions OD1–OD15 taken the same
day. Chunks **LX0–LX9**; none started. **The Go app (`gawk-broadcast/`) is
frozen to fixes only from 2026-09-24** (OD6, D15). The ROADMAP entry
([R56](../ROADMAP.md#r56--linux-in-the-desktop-workspace-and-retiring-the-go-broadcaster))
carries the summary; this doc restates the decisions so it reads on its own.

**Relationship to the other broadcasters**: this is the fourth native
broadcaster design and the second Linux one. R14
([docs/19](19-linux-native-broadcaster.md)) and R35
([docs/39](39-linux-app-sharing.md)) built the Go app; R34
([docs/38](38-windows-native-broadcaster.md)) and R52
([docs/54](54-macos-native-broadcaster.md)) built the Rust desktop
workspace. R56 moves Linux into that workspace as a third shell and retires
the Go app. What is new is only the Linux platform layer. It is **built
from scratch, not ported**: docs/19 and docs/39 contribute their lessons,
their verification registers and their refusal and failure semantics, not
their process architecture. Wherever this doc says "inherited", the
mechanism is the named doc's, verbatim, and is not re-argued here.

---

## 1. Why, and what "done" means

Three native broadcasters exist. Two of them share one engine, one wire
mirror and one `main.slint` by construction (docs/54 D1, D11). The Linux
one shares nothing, and the drift is measurable:

- **Every cross-cutting feature has been built twice**: native room attach
  (R42), the relay server picker and its profiles (R37, whose native probe
  display is still deferred on both sides), telemetry (R28), and the
  planned update notice and signed update (R45, R47).
- **The engines already behave differently.** Only the Rust engine forces
  an IDR on resume (docs/38 D5) and carries the F-12 keyframe-write fix
  (docs/38 §11). Only the Rust shells confirm close-while-live and ship a
  default app URL. Only the Rust engine will get R55's uplink carriers
  (docs/57 OD5). The default peak bitrate differs (Rust 12 Mbps, Go 16).
- **The Go app's architecture exists to work around Go, not Linux.** The
  GStreamer subprocess, MPEG-TS pipe framing, the PTS anchor (docs/19 D3,
  D6, D7) and the separate cgo `gawk-pw-helper` (docs/39 D4) all come from
  avoiding cgo in a Go binary. In Rust, `gstreamer-rs` and `pipewire-rs` are
  first-class bindings, and docs/38 D3 already deleted the same layer on
  Windows.

So the Linux shell joins the workspace, and the Go app is frozen, then
removed once the new app has passed the hardware pass.

### Milestone acceptance criteria

Pre-registered. "Manual" means on real hardware in a KDE Plasma Wayland
session (OD8). CI structurally cannot see a hardware encoder, a compositor
or the portal picker, so the manual criteria decide R56, exactly as
docs/19 and docs/38 warn.

| # | Goal | Verified by |
|---|---|---|
| G1 | **Feature parity with the Go app**: every "kept" or "changed" row of the §4 D16 parity ledger works in the new app, and every "dropped" row is dropped on purpose with its reason recorded | review of D16 against the build + manual |
| G2 | Mode 1: sharing one window streams that window, and only the app chosen in the whose-audio step is heard. A second app playing audio at the same time is inaudible to viewers (docs/39 AG1) | manual |
| G3 | Mode 2: sharing a screen streams the desktop with whole-system audio, and survives an output-device switch mid-broadcast, or fails audio-only | manual |
| G4 | Hardware encode only: with no hardware candidate passing its trial, the app refuses with the curated message and points at the browser broadcaster. It never software-encodes | unit (scripted trial) + manual |
| G5 | Relay and viewer untouched: a stock viewer at `gawk.ioio.fi` plays the stream with zero diffs outside the desktop workspace, docs, site, CI, and the relay origin allowlist values | integration against the real `gawk-server` binary + review |
| G6 | Wire parity: inherited. The shared `wire` crate's golden vectors, byte-identical with `gawk-server/wire`, `wire.ts` and `gawk-broadcast/internal/wirecheck` | unit (existing) |
| G7 | Glass-to-glass latency under 500 ms, and **no worse than the Go app's baseline on the same rig**, measured with R14 V4's photographed-reference method | manual, photographed reference |
| G8 | A/V sync: viewer-reported median `\|avSkewMs\| ≤ 60 ms`, p95 `≤ 120 ms` over 60 s (R25), in both modes | manual, viewer diagnostics |
| G9 | Zero-config first run: extract the tarball → run → "Choose what to share…" → pick → Start reaches the production fleet, and the join link works, with no settings touched | manual |
| G10 | The broadcast survives a relay pod restart or rolling drain with automatic resume, and the first frame after resume is a forced IDR (new on Linux) | integration (kill/restart relay) + manual during a fleet rollout |
| G11 | Crash posture: a GStreamer, driver or PipeWire failure ends the broadcast with a critical notification, or degrades audio per docs/39 D6. It never kills the app silently. **Any** process death (SIGKILL, OOM) leaves no PipeWire object behind | unit (injected failures) + CI kill matrix (headless PipeWire) + manual (V-1, V-9) |
| G12 | Windows and macOS are unaffected: their identities, binaries and CI jobs behave identically. The Linux work adds cfg-gated modules and never changes a shared default silently | CI + review |
| G13 | Idle cost: the idle window uses ~0 % CPU (the Gio app shipped a 20–30 % idle bug, docs/19 finding 12) | manual + profiler |
| G14 | Retirement: after LX9 no Go Linux release exists any more. `gawk-pubsim`, the dev stack, `e2e` and `e2e-cluster` stay green. `releases/gawk-broadcast/latest.json` is frozen at the last Go release, and the site offers only the new tarball | CI + review |

## 2. Owner decisions (taken 2026-09-24)

| # | Decision | Choice |
|---|---|---|
| OD1 | Code home | **A third shell, `crates/app-linux`, in `gawk-broadcast-desktop/`.** Same engine, wire, `main.slint`, audio lane, H.264 and cascade code as Windows and macOS. One workspace version, one release-please component, one changelog. A Linux-only fix bumps all three binaries, accepted as in docs/54 OD1. |
| OD2 | Rewrite, not a port | The Linux platform layer is built from scratch against the workspace's seams. The Go code is a reference for behaviour and field lessons, never a source to translate. |
| OD3 | Encode | **`gstreamer-rs`, in-process**, with R14's hardware-only, trial-probed cascade `vulkanh264enc` → `nvh264enc` → `vah264enc` → refusal. Reverses docs/19 D3's subprocess. The LX0 spike measures the crash posture before anything else is built on it (D4, §9). |
| OD4 | App-audio control plane | **In-process `pipewire-rs`**, on its own thread and its own core connection, with no `object.linger`. Reverses docs/39 D4 and §7. Cleanup-by-construction still holds, because process death closes the connection (D8). |
| OD5 | CLI | **GUI only in v1** (docs/38 OD12). The Go CLI retires with the app. The engine stays free of any shell, so a CLI crate for all three OSes can be a later milestone. |
| OD6 | Deprecation | **Freeze now, remove at parity.** The Go app is fixes-only from 2026-09-24. R45 and R47 are built in Rust only. One last Go release carries a deprecation notice (LX8), and the Go app is removed after LX7 passes (LX9). |
| OD7 | Packaging | **A tarball, as today**: binary + desktop entry + hicolor icons + `install-desktop.sh` (R44). GStreamer and PipeWire come from the distro. No AppImage, Flatpak or `.deb`. |
| OD8 | Verification floor | **NVIDIA (the gaming PC) and one AMD or Intel machine**, both gating, both in a **KDE Plasma Wayland** session. GNOME, X11 and wlroots sessions are best-effort: recorded when tried, never gating. |
| OD9 | Architecture | **x86_64 only.** |
| OD10 | Identity | **A new per-platform identity**, matching Windows and macOS: distribution `gawk-broadcast-linux`, origin `gawk-broadcast://linux`, asset `gawk-broadcast-linux-x86_64.tar.gz`, manifest `releases/gawk-broadcast-linux/latest.json`, telemetry `browser`/diagnostics `kind` `gawk-broadcast-linux`. |
| OD11 | `gawk-pubsim` | **Keep a shrunk Go module as test tooling** (`engine`, `fixture`, `mpegts`, `opus`, `pubsim`, `wirecheck`). Its release-please component and Linux release stop. Porting pubsim to Rust is a separate, later milestone. |
| OD12 | Linux-only knobs kept | **All four**: the encoder pin (`encoder`), the audio-device pin (`audioDevice`), the H.264 dump tap (`GAWK_DUMP_H264`), and the mid-session capture rebuild. |
| OD13 | Thumbnail | **Yes**: the 1 Hz "what viewers see" thumbnail from the live frames, as on Windows and macOS. Reverses docs/19 D16. It is dropped on any capture path where it would break zero-copy (D4, V-3). |
| OD14 | Distro floor | **Ubuntu 24.04-class**: built in an `ubuntu:24.04` container (glibc 2.39, GStreamer 1.24, PipeWire 1.0). Covers Ubuntu 24.04+, Debian 13, Fedora 40+ and Arch. The Go card's "glibc 2.34" was never the real floor, because GStreamer ≥ 1.24 already excluded Ubuntu 22.04 and Debian 12. |
| OD15 | Version | **The desktop workspace goes to 2.0.0** on the release that first ships Linux (a one-time `release-as` override, D13), so Linux users never see 1.15 → 1.6. |

## 3. Non-goals

- **A CLI** (OD5), **AppImage / Flatpak / `.deb`** (OD7), **aarch64** (OD9).
- **Software encode**: refusal is the behaviour (docs/19 D4, G4).
- **Direct Vulkan Video** (R14 V8): still open, still the long-term target
  API (docs/19 D21). If ever built, it is a fourth candidate in this app's
  cascade, not part of R56.
- **Persisting the portal choice**: no `persist_mode`, no restore token. The
  owner's 2026-07-16 decision (docs/19 D5, as reversed) stands: every
  broadcast asks what to share.
- **Porting `gawk-pubsim`** (OD11).
- **Gating on GNOME, X11 or wlroots** (OD8). Nothing hard-gates on them
  either: docs/19 D5's "don't hard-gate on Wayland" stands.
- **Any change to relay, wire format or viewer** beyond one origin-allowlist
  line per deployment (D2).
- **Microphone, tray icon, global hotkeys** (docs/19 D15, docs/38 OD7/OD8).
- **Keeping the Go and Rust Linux apps at feature parity during the
  overlap.** The Go app is frozen (OD6), and new features land in Rust only.

## 4. Decisions

### D1 — Workspace shape: `crates/app-linux`, Linux modules behind `cfg(target_os = "linux")`

- **New crate** `crates/app-linux` (package `gawk-broadcast-app-linux`,
  bin **`gawk-broadcast-linux`**). The bin name is unique in `target/`,
  for the reason docs/54 §11 gave for `gawk-broadcast-macos`. It implements
  `ui::shell::Platform` and builds its media through
  `ui::shell::MediaBuilder`, like the other two shells.
- **Linux platform modules live in the shared crates**, gated like the
  Windows and macOS ones:
  - `capture`: `portal.rs` (ashpd), `pwclock.rs` (the clock join, D4)
  - `encode`: `gst.rs` (the pipeline plan, the trial runner, the live
    encoder), plus a pure `gst_policy.rs` compiled everywhere, in the
    `vt_policy.rs` pattern, so its rules are unit-tested on every host
  - `audio`: `gstsrc.rs` (the system cascade, D7), `pwctl.rs` (the control
    plane, D8), and a pure `pwgraph.rs` compiled everywhere
- **Reused unchanged**: `wire`, `engine` (session, send policy, resume,
  timesync, telemetry, rooms, config), `ui` (`main.slint`, the shell, the
  diagnostics), `encode::{h264, cascade}`, `capture::{fit, gate}`, and
  `audio::{framer, opusenc, level, toc, lane}`.
- **What changes in shared code** is named and small: the D2 identity
  switch, D6's `Sender::restart_codec`, D10's config path and keys, D9's
  `native-menu` split and whose-audio card, and a Linux-gated
  `PcmFormat` constructor for gst's F32 interleaved caps (D7).
- `app-windows` keeps its non-Windows dev-shell stub for host tests. It
  stops being the thing a Linux developer runs.

### D2 — Identity: `defaults::LINUX`, injected by the Linux shell; `://native` stays allowlisted

- `engine::defaults` gains
  `LINUX = {"gawk-broadcast-linux", "gawk-broadcast://linux", "gawk-broadcast-linux-x86_64.tar.gz", "Linux"}`.
- **The shell injects its identity; the target OS does not choose it.**
  `THIS` is deliberately *not* `cfg(windows)` today, because the Windows
  shell is linted, tested and integration-tested on Linux hosts
  (docs/38 D18) and must keep its identity there. Switching `THIS` to
  `LINUX` on every Linux build would silently move all of that onto the
  Linux identity, and nothing in CI would test Windows identity behaviour
  any more. So:
  - `THIS` (a `const`) becomes `defaults::this() -> &'static Distribution`,
    backed by a `OnceLock`.
  - `app-linux` calls `defaults::set_this(&LINUX)` once, first thing in
    `main`, before `ui::shell::run`.
  - Unset, `this()` returns exactly today's rule: `MACOS` on macOS,
    `WINDOWS` everywhere else. `app-windows`, `app-macos`, the engine's
    relay-integration suite and every host test therefore keep today's
    identity byte for byte.
  - `set_this` is idempotent for the same value and panics on a second,
    different value, so two shells can never race each other.
  - `ORIGIN` becomes `defaults::origin()`. Every current consumer of `THIS`
    reads `this()`: telemetry `browser`/`os`, diagnostics `kind`,
    `Config::resolve_origin`, and the refusal wording.
  - A Cargo feature was rejected: `cargo test --workspace` unifies features
    across crates, so `app-linux` enabling one would flip the identity of
    `app-windows`'s host tests too.
- `this_build_is_the_distribution_of_its_target` keeps its assertion for
  the unset case. New tests cover `set_this(&LINUX)` in a separate test
  binary, so the global never leaks into other tests, and a second
  different value panicking. The refusal wording gains a Linux branch that
  names "this PC", not "Windows".
- **Relay allowlists**:
  - Production values (the ioio repo) and `docs/self-hosting.md` gain
    `gawk-broadcast://linux`.
  - **`gawk-broadcast://native` stays allowlisted everywhere,
    indefinitely.** `gawk-pubsim` sends it (the dev stack's
    `GAWK_ALLOWED_ORIGINS`, docs/41 §4.5), and un-upgraded Go binaries keep
    working.
  - The self-hosting table re-labels `://native` as "`gawk-pubsim` and
    legacy Linux app" at LX9.
- **Telemetry**: the shared reporter sends `browser: gawk-broadcast-linux`,
  `os: Linux`. LX6 checks `gawk-telemetry` for any hard-coded
  `gawk-broadcast` client class, e.g. a filter or label map, and adds the
  new value beside it.

### D3 — Capture: the portal via `ashpd`, picked before Start, never persisted

The docs/19 D5 handshake, now over `ashpd`:
`CreateSession` → `SelectSources` → `Start` → `OpenPipeWireRemote`.

- `SelectSources` sends:
  - `types = monitor | window`
  - `multiple = false`
  - `cursor_mode = embedded`
  - **no `persist_mode`, no restore token**
- `Start` yields `streams[0]`: the node id, `source_type` and the optional
  `size`, with the any-integer-width parsing Go needed.
- A virtual or unknown `source_type` takes the monitor path.
- Outcomes:
  - Cancel → the typed "cancelled" outcome, with no error card.
  - Unreachable portal → a sentence naming the portal.
  - No Wayland gate.

**Ordering follows macOS, not the Go app.** The Share card's "Choose what
to share…" opens the portal *before* Start, like docs/54 D11. The grant
and fd are held in memory until the broadcast ends or the user re-picks,
and are then released, so **every broadcast asks** (docs/19 D5 as reversed). Within
one broadcast the grant is reused for cascade retries (D5) and capture
rebuilds (D6), and never costs a second picker. The Go app dialled
`/publish` first, then opened the picker. That order existed because the
picker lived inside Start. Picking first is what the shared card
expresses, and it lets the whose-audio step (D9) happen before anything
connects.

**Geometry** (docs/39 D2, inherited): the configured resolution is a
bounding box. The fit input is the portal's reported `size`, and
`capture::fit::fit_within` gives even-floored fitted dimensions.

### D4 — One in-process video pipeline per broadcast; one clock; errors attributed by element

**Topology.** It is built from typed element factories, not a
`parse_launch` string. A pure plan function (`gst_policy`) produces the
element list and properties, so the unit tests assert the plan the way
Go's `BuildPipeline` string tests did:

```
pipewiresrc fd=<portal fd> path=<node id> do-timestamp=true
  ! <capture rung, which includes the candidate convert>    (the Go pipeline.go ladder; expansions in the table below)
  ! video/x-raw(<memory>),width=W,height=H,framerate=F/1     (fitted W×H; F only budgets rate control — VFR passes through)
  ! tee name=t
  t. ! <encoder> ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au
     ! appsink name=video sync=false max-buffers=2 drop=false
  t. ! queue leaky=downstream max-size-buffers=1 ! videorate drop-only=true max-rate=1
     ! <download> ! videoconvertscale ! video/x-raw,format=RGBA,width=320 ! appsink name=thumb sync=false drop=true max-buffers=1
```

The capture rungs expand exactly as the Go ladder did, and every rung only
ever drops frames:

| Rung | Elements |
|---|---|
| `auto-capped` | `video/x-raw(ANY),max-framerate=F/1 ! <convert> ! videorate drop-only=true max-rate=F` (the compositor paces delivery; the gate catches the rest) |
| `auto` | `<convert> ! videorate drop-only=true max-rate=F` (convert straight on the source, keeping DMA-BUF allocation adjacent) |
| `system-memory` | `video/x-raw ! videorate drop-only=true max-rate=F ! videoconvert ! <convert>` |

`<convert>` is the candidate's: `vulkanupload ! vulkancolorconvert`,
`cudaupload ! cudaconvertscale`, or `vapostproc` (D5).

- **The thumbnail branch** (OD13) exists only on capture paths where
  V-3 shows it costs no zero-copy. Elsewhere `show-thumbnail` is false,
  and the Share card says so in its caption rather than showing a stale
  image.
- **One clock, structurally.**
  - The pipeline runs on `GstSystemClock` with `clock-type=monotonic`.
    A buffer's capture time is `base_time + PTS` on `CLOCK_MONOTONIC`.
  - The engine's `MonotonicClock` is `Instant`, which is also
    `CLOCK_MONOTONIC` on Linux.
  - `capture::pwclock::mapper(clock)` is the same affine device→session map
    Windows (`qpc.rs`) and macOS (`host.rs`) expose, in ns instead of
    100 ns units.
  - So video and audio (D7) stamp on one timeline with no anchor. **The Go
    `ptsAnchor` and its bias gate are deleted, not ported** (docs/19 D6 and
    D7 existed because of the pipe). V-7 still measures glass-to-glass
    against the photographed reference.
- **Backpressure**, inherited in shape: the producer-queue policy is the
  docs/38 D5 send-policy table (the Linux original is docs/19 Decision 12),
  and the in-flight pin is docs/54 D10.
  - The appsink callback classifies the AU, runs `ensure_idr_headers`, and
    offers it to the shared `FrameGate`. No other queue sits between the
    encoder and the sender.
  - Rate limiting lives in the capture rung and is drop-only on every rung
    (see the rung expansions under the plan). `framerate=F/1` in the caps
    only budgets rate control, and nothing ever synthesizes frames.
- **Errors.**
  - A dedicated bus thread attributes each `ERROR` message by its source
    element:
    - `pipewiresrc` or a converter → capture (D6's rebuild)
    - the encoder → encode (D5's cascade advance, while still in the live
      probe window; a session error after it)
  - Every appsink callback runs under a `catch_unwind` fence (docs/38 D3).
    A panic becomes a session error through the normal path.
- **Crash posture: the one real risk of OD3.**
  - An in-process driver fault can take the process down. A dying child
    was merely a notification (docs/19 D3).
  - LX0 measures this on both GPUs before anything else is built.
  - **The pre-registered fallback** keeps every decision in this doc
    except process topology: the same binary re-execs itself as a media
    child (`gawk-broadcast-linux --media-child`). The child runs exactly
    this pipeline and ships AUs and PCM over a pipe with length-prefixed
    framing (`u32 len ‖ u64 ts_us ‖ u8 flags ‖ payload`), not MPEG-TS.
  - Choosing the fallback is an owner decision recorded in §11. It is not
    silently taken.

### D5 — Encode: the R14 cascade, trial-gated by the shared validator, with forced IDR

- **Candidates**: `vulkanh264enc` → `nvh264enc` → `vah264enc` → refusal,
  with Go `cascade.go`'s per-candidate properties carried as a table:

| Candidate | Convert / memory | Rate control | GOP / B-frames |
|---|---|---|---|
| `vulkanh264enc` | `vulkanupload ! vulkancolorconvert` / `memory:VulkanImage` | `rate-control=cbr bitrate=<peak kbps>` (no VBR in the element) | cadence forced by D5's keyframe rule; no B-frames by profile |
| `nvh264enc` | `cudaupload ! cudaconvertscale` / `memory:CUDAMemory` | `rc-mode=vbr zerolatency=true bitrate=<75 %> max-bitrate=<peak>` | `gop-size=fps/2 bframes=0` |
| `vah264enc` | `vapostproc` / `memory:VAMemory` | `rate-control=vbr bitrate=<peak> target-percentage=75` | `key-int-max=fps/2 b-frames=0` |

- **Trials use `cascade::TrialRunner`.** `videotestsrc` → convert →
  encoder → appsink. They never touch the portal.
- **Trials are judged by the shared `cascade::validate_trial`**, which is
  stricter than Go's "the child survived 3 s". It checks:
  - ≤ 1 frame retained at drain
  - no B-slices
  - output timestamps ⊂ input timestamps
  - first AU is an IDR
  - a forced IDR appears within 3 frames
  - SPS/PPS present
  - the codec string is taken from the SPS
- **The forced-IDR row is the one that can change the cascade's outcome**
  on real hardware. If a candidate fails *only* that row (plausible for
  `vulkanh264enc`), LX3 records it in V-2. The owner then decides between
  accepting the candidate without resume-IDR on that GPU and skipping it.
  The validator is never quietly weakened.
- **The live start is the final probe** (docs/19 D4): the pipeline must
  survive 3 s. On failure the cascade advances, reusing D3's grant.
- **Last-good cache.** The last good encoder is cached in `lastGoodEncoder`
  and re-verified first. The key and element-name values are the Go app's,
  so a user's cached winner carries over (D10).
- **The encoder pin** (`encoder`, OD12) skips the cascade and still trials
  the pinned candidate.
- **Forced IDR, new on Linux.** An upstream `GstForceKeyUnit` event on the
  encoder's sink pad serves the trial's forced-IDR check, the resume IDR
  (docs/38 D5) and the capture-rebuild restart.
- **Keyframe cadence.** GOP = `fps/2` frames at the encoder input.
  Wall-clock GOP is measured and displayed (EMA α = 0.3), like the other
  two apps.
- **Rung**: the shared defaults, **1920×1080, 60 fps, 12 Mbps peak,
  500 ms GOP**, with the shared advanced overrides.
  - Linux's old unset default of 16 Mbps drops to 12 (the docs/38 F-12
    reason: keyframe headroom on the uplink).
  - A `bitrateBps` the user saved is kept.
- **Codec string** from the SPS (shared `h264.rs`).
  `h264parse config-interval=-1` plus `ensure_idr_headers` put SPS/PPS
  before every IDR.
- **Refusal**: the shared `refusal_message`, with the resolved app URL.
- **The H.264 dump tap** (OD12): `GAWK_DUMP_H264=<path>` appends every AU
  as sent, in Annex-B. It is shell-level and Linux-only, and read once at
  Start. The TS tap has no meaning without the pipe and is dropped.

### D6 — Mid-session capture rebuild, and `Sender::restart_codec`

**The rebuild itself** (OD12, the Go behaviour):
- A capture-attributed error mid-broadcast rebuilds the video pipeline on
  the same portal fd and node, walking the capture ladder.
- It is capped at **60 rebuilds per rolling 30 s**, then ends the broadcast.
- Each rebuild counts in `captureRestarts` (stats, diagnostics, telemetry).
- The session, frame-id space and relay connection carry on. The viewer
  sees a freeze, then a keyframe.

**`Sender::restart_codec`.** The Rust `Sender::set_codec` is set-once, which
is right while one encoder owns the session and wrong across a rebuild,
because the new pipeline may have landed on another rung or encoder. LX2
adds `Sender::restart_codec(codec)`: it drops the cached DecoderConfig and
re-derives it from the rebuilt pipeline's first SPS. This is Go's
`AccessUnit.EncoderRestarted` semantics as an explicit call. Windows and
macOS never call it.

### D7 — Audio: the system cascade in gst, the R25 contract in the shared lane

**The audio pipeline is separate from the video pipeline.** A second
pipeline, on the same `GstSystemClock`:

```
<source> ! audioconvert ! audioresample
  ! audio/x-raw,format=F32LE,rate=48000,channels=2,layout=interleaved
  ! appsink name=audio sync=false
```

- Separate pipelines mean an audio failure can never touch video (R25
  Decision 6, subordinate audio).
- Nothing needs muxing any more, so the Go app's shared muxer has no
  successor.

**Sources**: the Go cascade, inherited.

| Source | Element | Condition |
|---|---|---|
| `pipewire-monitor` | `pipewiresrc stream-properties=props,stream.capture.sink=true` | default |
| `pulse-default-monitor` | `pulsesrc device=@DEFAULT_MONITOR@` | fallback |
| `pulse-device` | `pulsesrc device=<audioDevice>` | the audio-device pin (OD12): skips the cascade *and* the whose-audio step, with a visible note (docs/39 D3) |
| `app-sink-monitor` | `pipewiresrc target-object=<sink serial> stream-properties=props,stream.capture.sink=true` | window mode + an app chosen (D8) |

- **Trials** take 25 buffers (~500 ms) within one 8 s budget. The winner is
  cached in `lastGoodAudioSource`.
- **Encoding moves out of gst.** The appsink feeds
  `gawk_audio::lane::Lane`: the shared `Framer` → libopus → first-packet
  TOC gate. The R25 contract (48 kHz, stereo, 128 kbps CBR, 20 ms,
  LowDelay, no DTX/FEC, config resent at 1 Hz) is then implemented **once
  for all three OSes**, and gst's `opusenc` is retired. A Linux-gated
  `PcmFormat` constructor describes the fixed F32 caps.
- **Timestamps**: `base_time + PTS` through the same `pwclock` mapper as
  video, falling back to `packet_timestamp_us`'s arrival-minus-buffered
  rule if a buffer lacks a PTS.

### D8 — App audio: the docs/39 tee, in-process on its own PipeWire connection

The *mechanism* is docs/39 D3 **as amended by its §8 findings F2 and F3**
(what actually shipped):
- a virtual sink `gawk-app-capture-<pid>`, media class
  `Audio/Sink/Internal`
- its channel layout comes from the target app's own output ports, falling
  back to the widest real sink, then stereo (docs/39 F3)
- the target app's `Stream/Output/Audio` ports are **linked** into it as a
  tee, never re-routed
- the capture source is the sink's monitor, addressed by `object.serial`
- the sink is created once per broadcast and never recreated; a layout
  change re-links into it, channel-matched else positional (docs/39 F2).
  Recreating it would change the `object.serial` under a running
  `pipewiresrc` and turn a headphone switch into a dead audio branch.

The *process shape* changes (OD4):

- **`pwctl` runs on a dedicated thread** with its own `pipewire::MainLoop`
  and **its own core connection**, separate from gst's `pipewiresrc`
  connections.
  - The sink and links are proxies on that connection, never
    linger-flagged.
  - Any process exit closes the connection, and the daemon destroys them.
    That is the same cleanup-by-construction argument docs/39 D4 made for
    the helper, with the process boundary replaced by the connection
    boundary.
- **The engine talks to `pwctl` over channels**, carrying the old NDJSON
  protocol's operations and events as Rust enums:
  - operations: `watch`, `capture{binary}`, `capture-system`, `release`
  - events: `apps`, `sink`, `links`, `fatal`
  - Every request has the helper's 5 s round-trip timeout, so a wedged loop
    degrades audio and can never block the UI or video thread.
- **Graph logic is a pure module** (`audio::pwgraph`). It keeps Go
  `pwgraph.go`'s rules: `application.process.binary` matching, the
  emitting-apps list, and link bookkeeping. **Every docs/39 §8–§9 finding
  (F1–F11) is restated as a Rust test**, above all F8 (the dropped opening
  registry burst), F2 (sink never recreated) and F5 (capture answered with
  a link count).
- **Failure semantics**: the docs/39 D6 table verbatim, with F2's
  amendment to its layout-change row (re-link into the existing sink, never
  recreate it). "Helper missing" becomes "control plane failed to connect". Every row still ends in system
  audio or silence, never a failed broadcast.
- **Why this reversal is sound** (docs/39 §7 rejected in-process
  libpipewire):
  - The rejection guarded a Go binary whose only native code was one cgo
    loop.
  - Under OD3 the far larger GStreamer and driver surface is in-process
    anyway, so a second process would isolate the smaller component while
    the larger one shares the address space.
  - What mattered in D4 was that **no exit path leaves state behind**, and
    that survives.
  - The CI kill matrix (LX4) asserts it empirically, as AG5 did.

### D9 — GUI: the shared cards; a portal Share card, the whose-audio card, no menu bar

`app-linux` sets these `main.slint` properties:

- `system-picker: true`: the portal Share card, like macOS.
  - "Choose what to share…" opens the portal (D3).
  - After a pick the card shows the mode ("One window" / "Whole screen"),
    the fitted size, and "Change…".
  - It cannot show whose window was picked, because the portal never says
    (docs/39 §1). The card states the mode, never a guessed app.
- **`native-menu: false`** (new). Today `MenuBar` is gated on
  `system-picker`. LX5 splits it into its own property, macOS sets both,
  and Linux gets no in-window menu bar.
- `mono-font: "DejaVu Sans Mono"`, falling back to `monospace`.

**The whose-audio card** (docs/39 D5, inherited) is the one Linux card:
- **When it appears**: after a *window* pick, inside the Share card, before
  Start. The Go app blocked inside Start to show it.
- **Rows**:
  - the live list of emitting apps (`Name · binary (N streams)`), with the
    last-used binary preselected when present
  - the fixed rows "Whole system" and "No audio"
- **The caveat line**: "Apps appear here when they play sound — start the
  game's audio first if the list looks empty."
- **Mid-session** it collapses to a status line, and its one action is the
  shared `switch-to-system-audio` callback.
- **The silence hint is link-count driven** (docs/39 D6), surfaced through
  `Media::audio_silence_hint`, rather than the Windows level-meter
  heuristic.

**Everything else is the shared shell**:
- header (encode line `Vulkan Video — vulkanh264enc · zero-copy · 1920×1080@60`), audio line and level meter, "N watching", version badge
- Broadcast card, Room card, Details stats, Settings with server profiles
- copy diagnostics
- **the close-while-live confirmation (new on Linux)**
- the terms notice

**Notifications**:
- They go over `org.freedesktop.Notifications` via `zbus` (already in the
  lockfile through Slint), with the urgency hint and `replaces_id`.
- Critical: start failure, broadcast error, ended unexpectedly, first
  viewer joined. Normal: started, ended, room ended.
- The icon is `fi.ioio.gawk.broadcast` when installed, else `video-display`
  (docs/53).
- With no session bus they are discarded silently.

**Wayland `app_id`** is `fi.ioio.gawk.broadcast`, so the R44 desktop entry
and icon match the window. This is verified in V-11. The desktop entry's
`StartupWMClass` stays the same.

### D10 — Config: the Go app's file, honoured as-is

- **Path**: `$XDG_CONFIG_HOME/gawk/broadcast.json`, falling back to
  `~/.config/gawk/broadcast.json`.
  - This is the file the Go app writes. The Rust `default_path` on Linux
    today ignores `XDG_CONFIG_HOME`. LX1 fixes that, with a test.
  - Mode 0600 (the existing `write_owner_only`), plaintext credentials
    (the Linux rule, docs/19 D19).
- **Keys**: the shared Rust keys, plus four Linux keys the Rust `Config`
  gains. They are read and written everywhere and used only on Linux:
  `encoder`, `audioDevice`, `audioApp`, `lastGoodAudioSource`.
  - The flat `relayUrl`/`publishSecret` override slots are migrated by the
    existing Rust `migrate`, whose F9 rule is the Go one.
  - `captureMode` is unused on Linux, because the portal decides.
  - The legacy `roomLabel` is dropped on save, as in Go.
- **Carry-over is a test.** A config written by the Go app (fixture: every
  key populated) loads in the Rust app with the same effective relay,
  secret-per-server, room, nickname, resume ID and token, rung, encoder
  cache and audio preselection.
  - V-12 proves it end to end: the Rust app resumes a broadcast code the
    Go app minted.
- **Visible default changes**:
  - the app URL now defaults to `https://gawk.ioio.fi`, so join links work
    out of the box (docs/38 D13); the Go default was empty
  - the unset peak bitrate is 12 Mbps (D5)

### D11 — Packaging: the tarball, renamed

`gawk-broadcast-linux-x86_64.tar.gz` contains:
- `gawk-broadcast-linux`
- `install-desktop.sh`
- `share/applications/fi.ioio.gawk.broadcast.desktop`, with
  `Exec=gawk-broadcast-linux`. **Same app ID**, so installing the new app
  replaces the Go entry in place.
- `share/icons/hicolor/{scalable,256x256}/apps/…`
- `INSTALL.md`, `BUILD-INFO.txt`, `THIRD-PARTY-NOTICES-linux.md`, `LICENSE`

`SHA256SUMS` is a sibling asset covering all three platforms.

- **Where the pieces come from**: the desktop entry, `assemble-share.sh` and
  `install-desktop.sh` move to `gawk-broadcast-desktop/tools/linux/`. LX6
  copies them, and LX9 deletes the Go copies.
- **libopus is linked statically** (`LIBOPUS_STATIC=1`, bundled cmake
  build), as on Windows and macOS, so it is not a runtime dependency.
- **Runtime dependencies**, all from the distro and named in INSTALL.md
  with apt/dnf/pacman lines:
  - GStreamer ≥ 1.24: core, plugins-base, -good, -bad, and the `pipewire`
    plugin
  - libpipewire-0.3
  - xdg-desktop-portal + a backend
  - fontconfig, libxkbcommon, and the Wayland/X11 client libraries that
    winit loads
- **CI asserts the `ldd` set** of the shipped binary against an allowlist,
  as the Go job did for the helper, so a new runtime dependency is a
  reviewed change.

### D12 — Build and CI: an `ubuntu:24.04` container for everything that compiles Linux code

- **In the container.** The Linux product build, plus the host clippy,
  tests and coverage for `cfg(target_os = "linux")` modules, run in an
  `ubuntu:24.04` container job on the self-hosted runners. It has
  `libgstreamer1.0-dev`, `libgstreamer-plugins-base1.0-dev`,
  `libpipewire-0.3-dev`, `libfontconfig-dev`, `libxkbcommon-dev`, `clang`,
  `cmake` and `pkg-config`. For the integration tests it also has
  `pipewire`, `wireplumber`, `dbus`, `gstreamer1.0-pipewire` and
  `gstreamer1.0-plugins-{base,good}`.
- **Why a container and not the runner image**: the runner image stays
  unchanged, and the build's glibc floor is pinned by the container
  (OD14), not by whenever the runner is upgraded.
- **The Windows cross-build stays on the runner** (`cargo xwin clippy`,
  `cargo xwin build`, docs/38 D18). The `macos` job is untouched.
- **The headless-PipeWire harness is ported** from Go `pwtest`: a
  per-test `XDG_RUNTIME_DIR`, a D-Bus session, pipewire, wireplumber and a
  null sink. It runs the `pwctl` integration and kill-matrix tests.
  - It keeps Go's skip-gate lesson (docs/39 F11): a step fails the job if
    any of those tests was skipped.
  - It also runs a gst-level test: `videotestsrc` through the D4 plan, with
    an encoder stub (`x264enc`, **test-only**, never a production
    candidate) into the appsink → `FrameGate` → the real relay. That
    proves the plumbing CI can see.
- **Checks**:
  - The glibc floor: CI extracts the highest `GLIBC_` symbol version the
    binary needs, asserts ≤ 2.39, and records it in `BUILD-INFO.txt`.
  - `-version` / the badge carry `+g<sha>` (docs/38 WB9).
- **Licences**:
  - `deny.toml` `targets` gains `x86_64-unknown-linux-gnu`.
  - `tools/licenses/gen-notices.py` gains the Linux target and
    `THIRD-PARTY-NOTICES-linux.md`, with its freshness check.
  - Dynamically linked LGPL GStreamer and PipeWire are named in the notices.
- **Coverage**: the Linux modules count toward the existing
  `gawk-broadcast-desktop` floor.
- The path filter gains `tools/linux/**`.

### D13 — Release: a third distribution on the desktop release, at 2.0.0

- **Attach**: `attach-release` stages the tarball beside the EXE and the
  zip, and `SHA256SUMS` covers all three.
- **Manifest**: a third `publish-release-manifest` call with distribution
  `gawk-broadcast-linux` and primary `gawk-broadcast-linux-x86_64.tar.gz`.
  `tools/releases/manifest.py` `TAG_STEMS` gains
  `"gawk-broadcast-linux": ("gawk-broadcast-desktop",)`.
- **Site**: the Linux download card switches to `data-dl="gawk-broadcast-linux"`.
  Its text states the real floor: "Ubuntu 24.04 or newer (or equivalent),
  a hardware H.264 encoder; tested on KDE Plasma (Wayland)". The fallback
  link filters `gawk-broadcast-desktop/`.
- **Version** (OD15): the LX6 PR sets `"release-as": "2.0.0"` on the
  `gawk-broadcast-desktop` package in `release-please-config.json`. The
  config key is used rather than a `Release-As:` commit footer because it
  shows up in review, and because a footer depends on what the squash
  commit's body ends up containing. release-please does **not** clear the
  key, and while it is set every release of the component is forced to
  2.0.0. So the PR that follows the 2.0.0 release removes it, and LX6's
  criteria require that. `CONTRIBUTING.md` gains a short section on
  one-time version overrides in this PR.
- **The old manifest** `releases/gawk-broadcast/latest.json` freezes at the
  last Go release (LX8). Nothing reads it except the site card, which has
  moved. The Go app never shipped an update check.
- **R45 / R47**:
  - docs/47 D5's validator table swaps its Linux row to distribution
    `gawk-broadcast-linux` and asset `gawk-broadcast-linux-x86_64.tar.gz`.
  - docs/48's Linux install path swaps the binary inside the extracted
    directory.
  - Both get dated notes in LX6. Neither milestone builds anything for the
    Go app (OD6).

### D14 — Telemetry, terms, diagnostics

- **Telemetry**: the shared R28 reporter, `browser: gawk-broadcast-linux`,
  `os: Linux`, with the shared pairing rule and `off` semantics.
  - The Go-only `lastGoodAudioSource` and `audioApp` reach the report
    through the existing `audioSource` field and a new optional `audioApp`
    stats field (LX4).
  - It is added to `gawk-telemetry`'s schema field list only if the schema
    rejects unknown fields. LX4 checks.
- **Terms**: the TC5 notice (shared).
- **Diagnostics JSON**: the shared shape with `kind: gawk-broadcast-linux`.
  - It includes `captureRestarts`, `capturePath`, `shareMode` and
    `audioApp`, so no Linux diagnosis loses a field the Go JSON had.
  - One honest `n/a` from the Go app goes away: capture fps is now
    measured.

### D15 — The Go app: frozen now, one deprecation release, removed at parity

- **Freeze (from 2026-09-24, OD6).** `gawk-broadcast/` accepts `fix`
  commits only: breakage, security, a relay-compatibility fix. No `feat`.
  - R45 and R47 skip the Go app.
  - R55's "the Go broadcaster follows" branch (docs/57 OD5) is void.
  - R35's AS7 on-hardware pass is **not** run on the Go app. Its register
    is folded into this doc's §10, item by item: docs/39 V-1 → V-14,
    V-2–V-4 → V-4, V-5 → V-5, V-6 and V-7 → V-6, V-8 → V-8, V-9 → V-7.
  - R14 V8 re-homes to this app (§3).
  - `CLAUDE.md`'s module-roles entry says the module is frozen.
- **LX8, the deprecation release.** Once a desktop release carrying the
  Linux tarball exists (LX6), one last Go change is allowed. It is
  `feat(broadcast)`, so release-please cuts a final minor. It adds:
  - a persistent header line in the Go GUI: "This app has been replaced —
    download gawk broadcast for Linux", linking the site's download
    section;
  - a start-up log line in the Go CLI saying the same.
  That release also writes the frozen `releases/gawk-broadcast/latest.json`.
- **LX9, removal (after LX7 passes).**
  - **Delete**: every package that `gawk-pubsim`, `wirecheck` and their
    tests do not import. Concretely: `cmd/gawk-broadcast`,
    `cmd/gawk-broadcast-gui`, `cmd/gawk-pw-helper`, and
    `internal/{app, appaudio, config, desktop, fit, gst, notify, portal, pwgraph, pwproto, pwtest, telemetry, version}`
    and `desktop/`.
  - **Keep**: `internal/{engine, fixture, mpegts, opus, pubsim, wirecheck}`
    and `cmd/gawk-pubsim`. The deletion list is re-derived at LX9 from
    `go list -deps ./cmd/gawk-pubsim ./internal/wirecheck/...` rather than
    trusted from this doc.
  - **CI**: `broadcast` becomes a pubsim/engine test job with none of the
    Gio, PipeWire or GStreamer apt packages. `attach-broadcast-release` and
    the Linux artifact are deleted.
  - **release-please**: the `gawk-broadcast` component is removed from the
    config and the manifest. Test tooling is not a released product.
  - **Coverage**: the `coverage-floors.json` key is re-floored to what
    remains.
  - **Docs**: the Go module's README and INSTALL become a short "test
    tooling" note. `CLAUDE.md`'s repository layout, `docs/self-hosting.md`,
    `docs/gotchas.md` (Go-app-only gotchas marked historical) and
    `BUGS.md` (the mpegts lint entry stays with the kept package, and
    Go-app-only entries are closed as retired) are updated.

### D16 — The parity ledger (G1's checklist)

Every user-facing behaviour of the Go app, with its fate. "Kept" means the
same behaviour. "Changed" names the difference. "Dropped" names the reason.

| Go app behaviour | Fate in `gawk-broadcast-linux` |
|---|---|
| Portal picker on every broadcast, window or monitor, cursor embedded, no restore token | **Kept**; picked before Start instead of inside it (D3) |
| Fit geometry: box + portal size, even-floored | **Kept** (shared `fit_within`) |
| Hardware cascade `vulkanh264enc` → `nvh264enc` → `vah264enc` → refusal, trial-probed, last-good cached, live start as final probe | **Kept**, with the stricter shared validator (D5) |
| Capture ladder `auto-capped` / `auto` / `system-memory`; capture-path label | **Kept** (D4) |
| Mid-session capture rebuild, 60 per 30 s, reusing the grant | **Kept** (D6) |
| Encoder pin (`-encoder`, `encoder`, `GAWK_ENCODER`) | **Kept** as the `encoder` config key; the flag and env go with the CLI |
| Audio: `pipewire-monitor` → `pulse-default-monitor`, `audioDevice` pin, 25-buffer trial, cached | **Kept** (D7) |
| Opus via gst `opusenc` | **Changed**: the shared libopus lane, same R25 contract (D7) |
| App audio: whose-audio step, emitting-apps list, last-used preselect, whole system / no audio rows | **Kept**; shown before Start (D9) |
| Link-count silence hint + switch to whole-system audio | **Kept** (D8, D9) |
| `gawk-pw-helper` subprocess | **Changed**: in-process `pwctl` on its own connection (D8) |
| Arrival stamping + `ptsAnchor` + bias gate | **Dropped**: no pipe; one clock end to end (D4) |
| MPEG-TS pipe, `GAWK_DUMP_TS` | **Dropped**: no pipe |
| `GAWK_DUMP_H264` | **Kept** (D5) |
| No resume IDR (gst-launch cannot force one) | **Changed**: forced IDR on resume (D5, G10) |
| Rung 1080p60, 16 Mbps default, resolution/fps dropdowns incl. 1440p and 120 fps | **Changed**: 12 Mbps default; same dropdowns (shared) plus the shared Custom size |
| Default relay, per-server secret profiles (R37), telemetry pairing rule, `off` | **Kept** (shared engine) |
| App URL: none by default | **Changed**: `https://gawk.ioio.fi` by default |
| Room card: attach / detach / new room / nickname / open room view | **Kept** (shared Room card) |
| Resume `<CODE>` button; resume token only for the matching ID; mint fallback only after connect-phase failures | **Kept** (shared) |
| Notifications with urgency, replace-previous, first-viewer critical, none per resume attempt | **Kept** (D9) |
| Copy diagnostics (`kind: gawk-broadcast`) | **Changed**: `kind: gawk-broadcast-linux`, shared shape plus the Linux fields (D14) |
| Stats rows incl. capture rebuilds, keyframe interval, RTT, audio packets | **Kept**; capture fps becomes a real number |
| Version badge `X.Y.Z+g<sha>[.dirty]` | **Changed**: the shared badge (no `.dirty`, docs/38 WB9) |
| Closing the window ends the broadcast, no confirmation | **Changed**: the shared confirmation while live |
| Terms notice | **Kept** (shared) |
| No preview | **Changed**: the 1 Hz thumbnail where zero-copy allows (OD13) |
| Desktop entry + icons + `install-desktop.sh`, app ID `fi.ioio.gawk.broadcast` | **Kept**, `Exec=gawk-broadcast-linux` (D11) |
| Config at `$XDG_CONFIG_HOME/gawk/broadcast.json`, 0600 | **Kept**, same file (D10) |
| Origin `gawk-broadcast://native`, kind `gawk-broadcast` | **Changed**: `gawk-broadcast://linux`, `gawk-broadcast-linux` (D2) |
| The CLI (`gawk-broadcast`) and its flags: `-insecure`, `-stats`, `-v`, `-room-new`, `-audio-app`, … | **Dropped** in v1 (OD5); headless users stay on the last Go release until a CLI milestone |
| `gawk-pubsim` | **Kept in Go** (OD11), not part of this app |

## 5. Architecture

```
gawk-broadcast-desktop/
  crates/
    wire/  engine/  ui/                  # shared, unchanged except D2/D6/D9/D10's named edits
    capture/   fit.rs gate.rs …          # shared
               portal.rs pwclock.rs      # linux: ashpd handshake, the CLOCK_MONOTONIC mapper
    encode/    h264.rs cascade.rs …      # shared
               gst_policy.rs             # everywhere: the pipeline plan + candidate table (pure)
               gst.rs                    # linux: trial runner, live pipeline, force-key-unit, bus
    audio/     framer … lane.rs          # shared
               pwgraph.rs                # everywhere: graph rules (pure)
               pwctl.rs gstsrc.rs        # linux: control plane thread, system cascade
    app-windows/  app-macos/
    app-linux/                           # the Linux shell: Platform impl, notifications, pipeline assembly
  tools/linux/                           # desktop entry, assemble-share.sh, install-desktop.sh
```

Dataflow (one process, two gst pipelines, one PipeWire control
connection, no pipes):

```
 Share card ──ashpd──▶ portal ──fd + node──┐
                                           ▼
 video pipeline:  pipewiresrc ─▶ capture rung ─▶ convert ─▶ tee ─┬─▶ encoder ─▶ h264parse ─▶ appsink ─▶ classify IDR ─▶ FrameGate ─▶ engine ─▶ wtransport ─▶ relay
                                                                 └─▶ 1 Hz download/scale ─▶ thumbnail appsink ─▶ Share card
 audio pipeline:  pipewiresrc(monitor | app sink) / pulsesrc ─▶ F32 48k 2ch appsink ─▶ Lane (Framer ─▶ libopus ─▶ TOC gate) ─▶ engine
 pwctl thread:    own PipeWire core ─▶ null sink + port links (tee) ─▶ events (apps, sink serial, links) ─▶ whose-audio card / silence hint
 clock:           GstSystemClock(monotonic) base_time + PTS ─▶ pwclock mapper ─▶ session µs  ==  engine MonotonicClock
```

The engine's seams (`RelaySession`, `Clock`, `Sender`, `ui::shell::Media`
and `Platform`) are the contract. Nothing Linux-specific is visible above
them.

## 6. UX flows

**First run (G9)**: extract → run `./gawk-broadcast-linux` (or
`install-desktop.sh` first) → the window opens on the Share card → "Choose
what to share…" → the desktop's own picker → pick a screen or window → if a
window, the whose-audio card fills in → Start broadcast → encode line +
code + join link → Copy link. No fields touched.

**Refusal (G4)**: no hardware candidate passes → the error card with the
shared refusal message, linking the browser broadcaster at the resolved app
URL. Nothing else happens.

**Re-pick**: "Change…" while idle drops the held grant and opens the
picker again. While live, re-picking is not offered: it is a new
broadcast, as on the Go app.

**Capture rebuild**: a window resize that kills `pipewiresrc` → a brief
freeze → the pipeline rebuilds on the held grant → a keyframe → the Details
row "Capture rebuilds: 1". No picker.

**Resume**: inherited. The amber heartbeat and "Reconnecting…", a forced
IDR on success, 4004 terminal, and `Resume <CODE>` after an app restart.

**Mode-1 audio doubt**: links at zero for 10 s → the docs/39 D6 hint → one
click to whole-system audio.

**Stopping**: `Stop broadcast`, or closing the window (with the live
confirmation). Both pipelines go to NULL synchronously and the `pwctl`
connection closes before exit (the `finish()` incident class, docs/19
"Auto-resume").

## 7. What deliberately does not exist

- **Inherited from docs/38 §7 verbatim**: no software encode rung; no
  preview player (the 1 Hz thumbnail is the bounded exception); no
  viewer→server keyframe back-channel; no mint fallback outside
  connect-phase failures; no auto-resume through 4000/4004; no second
  clock; no standalone DecoderConfig datagrams; no DTX/FEC/Opus bitrate
  knob.
- **Inherited from docs/39 §7**: no re-routing of anyone's audio (tee
  only), no guessing the window→app correlation, no `pw-cli`/`pw-link`
  control plane.
- **Added here**: no restore token; no `object.linger`; no MPEG-TS; no
  `gst-launch-1.0` subprocess unless the D4 fallback is chosen by the
  owner; no Go code in the shipped Linux product.

## 8. Chunks and acceptance criteria

Prefix **LX** (Linux; the first free two-letter prefix that reads right).

**Ordering**:
- LX0 is a go/no-go on OD3's one risk, on real hardware, before anything
  else is built.
- LX1 gives CI a Linux product job and the new identity.
- LX2–LX4 are the hardware-facing layers.
- LX5 assembles the product and LX6 ships it.
- LX7 is the pass that decides R56.
- LX8 and LX9 retire the Go app. LX8 may land any time after LX6; LX9 only
  after LX7.

### LX0 — Spike: in-process GStreamer + portal on both GPUs (go/no-go)

| Acceptance criterion | Verified by |
|---|---|
| A minimal `app-linux` reaches the production relay: ashpd portal → in-process `pipewiresrc` → each hardware candidate that exists on the machine → appsink → engine. A viewer plays it for 10 min on the NVIDIA machine **and** on the AMD/Intel machine, both KDE Wayland | manual |
| Crash posture measured (V-1): induced failures (exceed the NVENC session limit, unplug the captured monitor, suspend/resume, kill the compositor's portal backend) either surface as bus errors or kill the process. Each outcome recorded in §11 | manual, recorded |
| Owner go/no-go recorded in §11: in-process (D4 as written) or the `--media-child` fallback | review |
| Slint + winit on Wayland on both GPUs: window, fractional scaling, `app_id`, idle CPU (V-10, V-11) recorded | manual |
| A lessons ledger is added to §11: every docs/19 "Implementation notes" item and docs/39 §8–§9 finding is marked *applies* (with the chunk that honours it) or *obsolete* (with why) | review |

### LX1 — Identity, config, CI container, empty shell

| Acceptance criterion | Verified by |
|---|---|
| `defaults::LINUX` injected by `app-linux` via `set_this` (D2); unset `this()` is today's rule, so `app-windows` host tests, clippy and the relay-integration suite on Linux runners still resolve to `WINDOWS` (docs/38 D18); a conflicting second `set_this` panics; refusal wording covers all three (G12) | unit (separate test binary for the injected case) + CI |
| Config: `XDG_CONFIG_HOME` honoured on Linux; `encoder`, `audioDevice`, `audioApp`, `lastGoodAudioSource` round-trip; a Go-written fixture config loads with identical effective values (D10) | unit |
| The `ubuntu:24.04` container job: fmt, clippy `-D warnings`, `cargo test --workspace`, coverage, and `cargo build --release -p gawk-broadcast-app-linux`; artifact `gawk-broadcast-linux-x86_64-<sha>` with `BUILD-INFO.txt` (version, commit, glibc floor, `ldd`), `if-no-files-found: error` | CI |
| glibc floor ≤ 2.39 asserted; `ldd` allowlist asserted; `deny.toml` and `gen-notices.py` cover the Linux target, freshness check green | CI |
| `gawk-broadcast-linux` launches on the gaming PC, shows the D9 cards in their empty state, and quits; no media code yet | manual |

### LX2 — Capture and the video pipeline

| Acceptance criterion | Verified by |
|---|---|
| Portal: pick → card shows mode and fitted size; cancel is silent; no portal is a sentence; no `persist_mode` or restore token is ever sent (asserted on the ashpd request) | unit (request builder) + manual |
| The D4 plan: element list and properties per candidate × capture rung, pure and unit-tested; built into a real pipeline in the container with a test-only encoder | unit + CI integration |
| One clock: AU timestamps from `base_time + PTS` via `pwclock`; strictly monotonic; same timeline as `MonotonicClock` (fake clock + real gst clock test) | unit + CI integration |
| Capture ladder walked on `pipewiresrc` failure; `ErrCaptureFormat`-equivalent sentence when all rungs fail | unit (scripted bus errors) |
| Mid-session rebuild on the held grant, capped at 60 / 30 s, counted in `captureRestarts`; `Sender::restart_codec` re-derives the DecoderConfig from the new SPS (D6) | unit + CI integration (forced source error) |
| Thumbnail branch present only on paths V-3 cleared; 1 Hz; never blocks the encoder branch (leaky queue) | CI integration + manual |
| Window resize / fullscreen / minimize on KWin (V-4) recorded; an odd-sized window encodes fitted, not stretched (V-14) | manual |

### LX3 — Encode

| Acceptance criterion | Verified by |
|---|---|
| `GstTrialRunner` implements `cascade::TrialRunner`; `videotestsrc` trials; `validate_trial` enforced per candidate; refusal when all fail (G4) | unit (scripted runner) + manual |
| Candidate property table (D5) applied exactly; GOP `fps/2`; no B-frames; rate control accepted | unit (plan) + manual |
| Forced IDR via `GstForceKeyUnit` within 3 frames, per candidate on real hardware (V-2); any candidate failing only that row escalated to the owner, not waived | manual, recorded |
| Live start = final probe (3 s); cascade advances on live failure without a second picker | unit (scripted) + manual |
| `lastGoodEncoder` cached and re-verified first; `encoder` pin honoured | unit |
| Codec string from the SPS; SPS/PPS before every IDR | existing unit |
| Resume sends a forced IDR first (G10) | CI integration (kill/restart relay) |
| `GAWK_DUMP_H264` writes the AUs as sent | unit |

### LX4 — Audio and the PipeWire control plane

| Acceptance criterion | Verified by |
|---|---|
| System cascade `pipewire-monitor` → `pulse-default-monitor`; `audioDevice` pin; 25-buffer trials; `lastGoodAudioSource` cached | unit (plan) + CI integration (headless PipeWire) |
| gst F32 appsink → shared `Lane`: 20 ms frames, one Opus packet per datagram, TOC gate, R25 contract unchanged | existing unit + CI integration |
| `pwctl`: one sink per broadcast with the layout from the target app's own ports (widest real sink, then stereo, as fallbacks; F3); tee links; re-link on churn; a layout change re-links into the **existing** sink and never recreates it (F2); every docs/39 F1–F11 restated as a test | CI integration (headless PipeWire) |
| Telemetry and diagnostics carry the new optional `audioApp` stats field; `gawk-telemetry` ingests a report containing it (the field is added to its schema list if the schema rejects unknown fields, D14) | unit + integration against `gawk-telemetry` |
| Kill matrix: SIGKILL, panic, and clean exit of the app process each leave no `gawk-app-capture-*` node or link in the daemon (G11) | CI integration |
| Wedged-loop guard: a `pwctl` request past 5 s degrades audio per the docs/39 D6 table; video untouched | unit (fake control plane) |
| Subordination: every audio failure leaves video running; wire byte-identical to video-only when audio is off | unit + CI integration |
| Mode 1 isolation (G2, V-5) and churn (V-6); A/V sync (G8, V-6) | manual |

### LX5 — The Linux shell

| Acceptance criterion | Verified by |
|---|---|
| `Platform` implemented; `native-menu` split from `system-picker` with macOS unchanged; the whose-audio card in `main.slint` behind its property; `show-thumbnail` honest per path | unit (shell) + manual |
| Pick → whose-audio → Start → code → copy link (G9); re-pick while idle; close-while-live confirmation | manual |
| Notifications over D-Bus with urgency and replace-previous; icon when installed; no session bus = silent | unit (message building) + manual (V-11) |
| Diagnostics JSON: `kind: gawk-broadcast-linux`, the Linux fields (D14); telemetry `browser`/`os` | unit |
| Idle window ~0 % CPU (G13, V-10) | manual + profiler |

### LX6 — Packaging, release path, docs

| Acceptance criterion | Verified by |
|---|---|
| Tarball layout per D11; desktop entry `Exec=gawk-broadcast-linux`, same app ID; `install-desktop.sh` installs, validates (`desktop-file-validate`) and uninstalls in CI | CI |
| Attach: tarball on the desktop release beside the EXE and zip; `SHA256SUMS` covers all three; the third manifest written; `TAG_STEMS` row with a test; the site card reads the new manifest | CI + the first release after merge |
| The first release carrying the tarball is `gawk-broadcast-desktop/v2.0.0` (OD15); the `release-as` key is removed in the PR right after it, and the next release is 2.0.x/2.1.0 | the release + review |
| `gawk-telemetry` shows `gawk-broadcast-linux` sessions like any other class (D2) | manual |
| Docs: desktop README Linux section + `INSTALL.md` (floor, apt/dnf/pacman lines, "when it doesn't work"); `docs/self-hosting.md` origin rows; docs/47 and docs/48 dated Linux-row notes; ROADMAP status; CLAUDE.md layout; gotchas synced | review |
| Production relay values gain `gawk-broadcast://linux` (ioio repo); `://native` kept | review + manual |

### LX7 — The on-hardware acceptance pass

| Acceptance criterion | Verified by |
|---|---|
| G1–G3, G7 (photographed reference, same rig as the Go baseline), G8, G9, G10 (fleet rollout), G11's manual half, G13, on **both** the NVIDIA and the AMD/Intel machine, KDE Plasma Wayland | manual |
| Every §10 register item has a recorded answer in §11 | review |
| The D16 ledger is re-walked against the release build | review |

### LX8 — The Go deprecation release

| Acceptance criterion | Verified by |
|---|---|
| The Go GUI shows the persistent "replaced" line linking the site's download section; the Go CLI logs it at start | unit (Go) + manual |
| Released as the final `gawk-broadcast` minor; `releases/gawk-broadcast/latest.json` written one last time | the release |

### LX9 — Removing the Go app

| Acceptance criterion | Verified by |
|---|---|
| The deletion list re-derived from `go list -deps` (D15); only pubsim, wirecheck and what they import remain | review |
| `gawk-pubsim`, the dev stack (`sim` and `rooms` profiles), `e2e`, `e2e-cluster` green (G14) | CI |
| The `broadcast` CI job reduced (no Gio/PipeWire/GStreamer packages); `attach-broadcast-release` deleted; `gawk-broadcast` removed from release-please config + manifest; coverage key re-floored | CI + review |
| CLAUDE.md, `docs/self-hosting.md`, `docs/gotchas.md`, `BUGS.md`, the module README updated; `tools/linux/` is the only copy of the desktop entry and scripts | review |

## 9. Risks

- **In-process driver faults (OD3).** NVIDIA-on-Wayland was the reason for
  R14's subprocess.
  - LX0 measures it on both GPUs before anything is built on it.
  - The `--media-child` fallback (D4) keeps every other decision intact,
    so choosing it costs one chunk, not a redesign.
- **The forced-IDR row vs `vulkanh264enc`** (D5). The shared validator may
  reject the leading candidate. This is escalated, not waived, and the
  cascade still has two other candidates on NVIDIA and AMD/Intel.
- **Thumbnail vs zero-copy** (OD13). DMABuf, CUDA and Vulkan memory may
  not download cheaply at 1 Hz. The thumbnail is dropped per path by
  design (V-3).
- **Rebuilding field-tested behaviour.** The Go app's capture and audio
  paths carry hardware lessons. The LX0 lessons ledger and the D16 parity
  ledger are there so nothing is re-learned by accident.
- **gstreamer-rs / pipewire-rs / ashpd versions.** The bindings must build
  against Ubuntu 24.04's GStreamer 1.24 and PipeWire 1.0, so feature flags
  are capped at `v1_24`. They must also agree on the tokio and zbus
  versions Slint already pulls in. LX1 locks the graph.
- **A shared release unit, now three platforms.** Accepted (OD1). The
  per-distribution manifests keep a retraction per platform possible
  (docs/47 D11).
- **The overlap window.** Until LX9, a Linux user can run either app. The
  freeze means the Go app only falls behind. It can never gain a feature
  the Rust app lacks.
- **Headless CLI users** lose the CLI at LX9 (OD5). The last Go release
  keeps working against the relay: its origin stays allowlisted (D2), and
  the wire only gains types, which it ignores.

## 10. On-hardware verification register

Pre-registered. Each item is a claim this doc makes on paper that only
real hardware can prove. Both machines, KDE Plasma Wayland, unless noted.

| # | Claim to verify | Chunk |
|---|---|---|
| V-1 | In-process crash posture: which induced failures (NVENC session limit, captured monitor unplugged, suspend/resume, portal backend killed) surface as bus errors vs. process death | LX0 |
| V-2 | Cascade outcome per GPU: which candidate wins, and each `validate_trial` row per candidate, above all forced IDR within 3 frames | LX0, LX3 |
| V-3 | Capture path per GPU (`zero-copy (capped)` / `zero-copy` / `system-memory`), and the thumbnail branch's cost on each: kept or dropped | LX2 |
| V-4 | Window capture on KWin: resize (rebuild without a picker), exclusive fullscreen (the docs/19 report, first-hand), occluded, minimized (docs/39 V-2–V-4) | LX2 |
| V-5 | Mode-1 isolation: the chosen app audible, a second app inaudible (docs/39 V-5) | LX4 |
| V-6 | Churn and sync: menu→game transitions re-link cleanly; A/V sync through the sink hop within R25's criteria (docs/39 V-6, V-7) | LX4 |
| V-7 | Glass-to-glass vs. the Go baseline on the same rig, photographed reference (docs/39 V-9, R14 V4) | LX7 |
| V-8 | The link-count silence hint fires on a silent target, and its switch works mid-session (docs/39 V-8) | LX4 |
| V-9 | PipeWire cleanup on real hardware: SIGKILL the app mid-broadcast → no `gawk-app-capture-*` node in `pw-cli ls` | LX4 |
| V-10 | Idle CPU of the Slint window on both GPUs | LX0, LX5 |
| V-11 | Wayland `app_id` matches the desktop entry (icon in the task switcher); notification urgency honoured by Plasma | LX0, LX5 |
| V-12 | Carry-over: the Rust app, on the Go app's real config, resumes a broadcast code the Go app minted, with the same server secret | LX7 |
| V-13 | Relay pod restart and a fleet rollout: auto-resume with a forced IDR first | LX7 |
| V-14 | Fit: a deliberately odd-sized window is encoded fitted, not stretched; the Share card and stats show the fitted dimensions (docs/39 V-1, AG3) | LX2 |

## 11. Deviations and field findings

None yet. LX0 adds the lessons ledger and the go/no-go here.

## 12. Revisions this doc makes to earlier docs

Each gets a dated note in place, in the same PR as this doc:

- **docs/19 D3** (GStreamer subprocess; in-process rejected) → superseded
  for the new app by OD3. The Go app keeps it until LX9.
- **docs/19 D16** (no preview) → superseded by OD13.
- **docs/39 D4 and §7** (helper subprocess; in-process libpipewire
  rejected) → superseded by OD4 / D8.
- **docs/38 §3 and docs/54 §3** ("sharing code with the Go Linux
  broadcaster" as a non-goal) → moot. Linux joins the workspace instead.
- **docs/57 OD5** ("the Go Linux broadcaster follows") → void. The Rust
  Linux app inherits WU2 with the engine.
- **docs/47 D5 / docs/48**: the Linux rows move to the new distribution
  at LX6 (D13). The notes land then.

## 13. References

- [docs/19](19-linux-native-broadcaster.md): R14, the Go Linux app (cascade, portal, auto-resume, verification)
- [docs/39](39-linux-app-sharing.md): R35, window + app audio on Linux (the tee, the whose-audio step, failure table, findings)
- [docs/38](38-windows-native-broadcaster.md): R34, the Rust workspace's origins (engine port and the D5 send policy, D3 in-process, §7 prohibitions, WB9); [docs/54](54-macos-native-broadcaster.md) D10 for the in-flight pin
- [docs/54](54-macos-native-broadcaster.md): R52, the shared workspace and the system-picker card
- [docs/28](28-native-broadcaster-audio.md): R25, the native audio contract
- [docs/47](47-desktop-update-check.md), [docs/48](48-signed-in-place-update.md): R45 / R47
- [docs/53](53-app-icons.md): R44, icons and the desktop entry
- [docs/57](57-wifi-uplink.md): R55, the uplink carriers this app inherits
