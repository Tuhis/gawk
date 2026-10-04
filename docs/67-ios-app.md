# R65 — iOS app: native broadcaster and viewer (docs/67)

**Status**: proposed 2026-10-03. Owner decisions OD1–OD16 (§2) were taken
the same day in an interview; **OD17 (2026-10-04) moved screen capture from a
ReplayKit extension to ScreenCaptureKit in the app** (§12). **IO1 and IO4
implemented 2026-10-04**; IO0, IO2, IO3 and IO5–IO8 (§9) are not started.
**Work runs Simulator-first (OD13, D26)**: phase S builds and tests
everything the Simulator can run; phase D starts on devices with **IO0, a
throwaway spike whose pre-registered verdict (§9.1) gates the device
acceptance of the broadcast pipeline**: whether iOS keeps the capturing app
running behind a game. Decisions marked *provisional* are
confirmed or revised in §12 once IO0 is done. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**

- Viewing in iOS Safari already works and **is not changed**. WebKit runs
  the whole WebTransport + WebCodecs worker pipeline, and R16/R22
  ([docs/21](21-ios-video-fullscreen.md), [docs/27](27-ios-mse-fullscreen.md))
  work around the missing Element Fullscreen on iPhone.
- The broadcast side is the macOS broadcaster's media path
  ([docs/54](54-macos-native-broadcaster.md)) on iOS 27's ScreenCaptureKit,
  in the app's own process (OD17). It reuses the `wire`, `engine`, `encode` and `audio` crates of
  [`gawk-broadcast-desktop`](../gawk-broadcast-desktop) **by path**, and
  inherits their invariants (docs/38 D9–D11, docs/54 D5–D10) rather than
  restating new ones.
- The viewer side is the first viewer outside `gawk-app`. It follows the
  wire contract the SPA reads (docs/03, docs/04, docs/12, docs/20,
  docs/34). From the SPA's playout it ports only the adaptive offset
  estimator (docs/17 Decision 6); AVFoundation presents (OD7, D15).

---

## 1. Why, and what "done" means

- **There is no way to broadcast from an iPhone or iPad today.** iOS Safari
  has no `getDisplayMedia`, and every iOS browser is WebKit. Capturing the
  whole screen is native-only: from iOS 27 through **ScreenCaptureKit** in
  the app itself (OD17), before it through a ReplayKit Broadcast Upload
  Extension, which the iOS 27 SDK deprecates.
- **Watching in Safari is second-class.** iPhone fullscreen depends on a
  fullscreen-only MSE surface whose on-device pass is still open (R22 MF5).
  There is no Picture-in-Picture, so you can't watch while playing a game
  yourself. Audio stops when Safari goes to the background.
- An `AVSampleBufferDisplayLayer` gives native fullscreen, rotation, PiP
  and background audio as platform features rather than workarounds.

### Milestone acceptance criteria

Pre-registered. "Device" means the owner's iPhone 17 Pro Max or iPad Pro
on iOS 27 (OD14). CI cannot see device screen capture, a hardware encoder's
behaviour under thermal load or PiP. As on every native milestone
(docs/19, docs/38, docs/54), the device criteria decide whether R65 works.

| # | Goal | Verified by |
|---|---|---|
| G1 | Starting a broadcast from the Broadcast screen (the system content-sharing picker, display chosen) streams the whole device screen with its audio to the default fleet, with no settings touched; a stock web viewer at `gawk.ioio.fi` plays it | device |
| G2 | A 30-minute broadcast while playing a 3D game: the app is never suspended or jetsammed behind the game (no capture gap over 2 s), peak footprint recorded, thermal state never `.critical` | device, logged frame gaps, footprint and thermal state |
| G3 | Glass-to-glass latency of an iOS broadcast to a desktop Chrome viewer is ≤ 250 ms, by R14 V4's photographed-reference method | device |
| G4 | A/V sync of an iOS broadcast: viewer-reported median `\|avSkewMs\| ≤ 60 ms`, p95 ≤ 120 ms over 60 s (R25's criteria, unchanged) | device, viewer diagnostics |
| G5 | Rotating the device mid-broadcast: viewers keep playing, at the new aspect, within one GOP (≤ 500 ms of frozen video) | device |
| G6 | The broadcast survives a relay pod restart, and an app relaunch within the grace period, on the same code (R17 resume) | integration in CI (relay kill) + device |
| G7 | The native player plays H.264, VP9 and VP8 broadcasts (browser on Chrome, Firefox and the desktop apps as sources) | device + Rust tests on recorded streams |
| G8 | Native-player glass-to-glass latency is within 100 ms of Safari's on the same H.264 broadcast, measured side by side **with both on Balanced** (both run the same adaptive estimator) | device |
| G9 | PiP and background audio: playback continues in PiP over another app, and audio continues with the screen locked, for 10 minutes each | device |
| G10 | A 5 s network outage while watching recovers to the live edge with no permanent added delay (the drop-to-live rule, D15) | device (airplane mode toggle) + Rust test |
| G11 | Rooms: an iOS broadcast attaches to a room and shows in the room's web view; the iOS viewer plays a room of three in grid and focus | device |
| G12 | Nothing changes for anyone else: no wire change, the relay change is the R59 `app` vocabulary value and the origin allowlist value only, and the desktop apps' tests and behaviour are unchanged by D4's gating | CI + review |

## 2. Owner decisions (2026-10-03)

| # | Decision |
|---|---|
| OD1 | **Broadcast the device screen through ReplayKit** (a Broadcast Upload Extension). Camera streaming is out of scope. *Superseded 2026-10-04 by OD17*: the device screen is still what's broadcast, through ScreenCaptureKit. |
| OD2 | **A native player** (VideoToolbox / libvpx → `AVSampleBufferDisplayLayer`), not a WKWebView around the SPA, and not a broadcast-only app. |
| OD3 | **Signed for the owner's own devices first.** TestFlight and then the public App Store will follow, in a later milestone (§5). |
| OD4 | **SwiftUI over a Rust core**, bridged with UniFFI. |
| OD5 | **A new top-level module, `gawk-ios`**, with its own release-please component and its own version. |
| OD6 | **iPhone and iPad, iOS 27 and later.** *Refined 2026-10-04*: the floor moved from iOS 26 to iOS 27, matching the Xcode 27 / iOS 27 SDK the development machine runs. |
| OD7 | **Playout uses AVFoundation timing**: timestamped sample buffers presented under an `AVSampleBufferRenderSynchronizer` at a small target delay. R12's presentation machinery (sub-frame pacing, interpolation) is not ported. *Refined in review 2026-10-03*: the target delay is R12's **adaptive** offset, not a constant, because a fixed playout offset is a rejected design (CLAUDE.md; docs/12 Decision 7, docs/17 Decision 10). See D15. |
| OD8 | **v1 includes** rooms (join and attach), Picture-in-Picture, opt-in telemetry and R37's server picker with per-server secrets. **Mic audio is not in v1.** |
| OD9 | **Shared Rust is used by path, and the build runs from the repo root**, as `gawk-admin` does with `gawk-server`. A semantic change to a shared desktop crate needs a `gawk-ios`-scoped commit in the same PR. |
| OD10 | **iOS CI runs from the first chunk** on GitHub-hosted macOS (since IO1, the `xcode-27` image; D24), with Xcode 27 selected explicitly: Rust cross-builds and tests, plus an unsigned `xcodebuild` with simulator tests. |
| OD11 | **The broadcast carries app audio** (ReplayKit `audioApp` → Opus through the shared audio crate; since OD17, ScreenCaptureKit's audio output, V-3). Uplink transport is whatever the shared engine does, so R55's carriers arrive when R55 lands them (D12), with no iOS work. |
| OD12 | **VP8/VP9 broadcasts play natively through a bundled libvpx**, so every broadcast plays in the app. **IO0 runs first on devices, as a measuring spike.** |
| OD13 | **Simulator first.** Everything is built and tested in the iOS Simulator before any device work (D26). |
| OD14 | **Devices**: an iPhone 17 Pro Max and an iPad Pro. **Signing**: the owner's paid Apple Developer Program membership, enrolled when phase D starts; phase S needs no team. |
| OD15 | **XcodeGen** generates the Xcode project from a checked-in `project.yml` (D2). |
| OD16 | **Bundle ID `fi.ioio.gawk`, display name "gawk"** (D28). |
| OD17 | *2026-10-04, with the iOS 27 floor (OD6).* **Capture through ScreenCaptureKit in the app process, not a ReplayKit Broadcast Upload Extension.** The iOS 27 SDK deprecates `RPBroadcastSampleHandler` ("No longer supported") and `RPSampleBufferType` ("Use `SCStreamOutputType` instead"), and brings `SCStream` and `SCContentSharingPicker` to iOS. Taken knowing the risk: one project reports iOS 27 suspending a backgrounded capturing app even with the `screen-capture` background mode, while the deprecated extension kept working. IO0 decides it (§9.1); the extension design is recorded in §3 as the fallback. |

## 3. Alternatives considered and rejected

Recorded so they aren't re-derived. Each was put to the owner on
2026-10-03 or follows from a recorded decision.

| Alternative | Why not |
|---|---|
| **WKWebView around the SPA** for viewing | It inherits every Safari limit this milestone exists to remove (no PiP from a canvas, the R16/R22 fullscreen detour). It's also unverified whether WKWebView exposes WebTransport at all, and an app that is a wrapped website is App Review's "minimum functionality" rejection (guideline 4.2). |
| **Broadcast-only app** (view in Safari) | Leaves PiP and background audio unsolved, which is half the reason to have an app (OD2). |
| **Pure Swift** | Apple ships no WebTransport client, so we'd hand-roll HTTP/3 + WebTransport framing over Network.framework's QUIC. That makes a **fifth wire mirror** and a second implementation of resume, send policy, parity and telemetry the desktop engine already has. |
| **Slint for the UI** | Slint's iOS support is young, and PiP, the content-sharing picker and the share sheet are UIKit/SwiftUI-only. Sharing `main.slint` with the desktop window (docs/54 D11) buys little on a phone. |
| **iOS as a fourth shell in the desktop workspace** | Its release unit and version would be the desktop's, and the "desktop" name would be wrong. OD5 chose a separate module. |
| **Extract a neutral shared-core workspace first** | The cleanest ownership, but a refactor of the desktop workspace and its CI ahead of any iOS value. Path dependencies (OD9) get the same reuse now. The extraction can follow if a third consumer appears. |
| **Camera broadcasting** (`AVCaptureSession`) | A different product (IRL streaming). Not a game stream (OD1). |
| **In-app ReplayKit** (`RPScreenRecorder`) | Captures only our own app. Useless for streaming a game. |
| **HEVC encode** | Firefox viewers can't decode it and the SPA's codec negotiation has no HEVC rung. H.264 is what every viewer plays. |
| **AVPlayer + LL-HLS** for viewing | Seconds of latency by design. It would need a packager on the relay. |
| **Porting R12's presentation machinery** (sub-frame slot matching, interpolation) | OD7. AVFoundation's synchronizer presents against the display's vsync and owns A/V sync, which is what that machinery does for a canvas. Only the offset estimator, which decides *how far* behind live to play, is ported (D15). |
| **A fixed playout delay** (the 150 ms of this doc's first draft) | Rejected twice already: docs/12 Decision 7 (the 200 ms sketch) and docs/17 Decision 10, which retired fixed 150 ms because adaptive dominates it at every point of the trade curve. A synchronizer is no new evidence: the latency cost is the same whoever presents. |
| **Safari fallback or an "unsupported" message for VP8/VP9** | OD12. Every broadcast should play in the app. |
| **A ReplayKit Broadcast Upload Extension** (this doc's design until 2026-10-04) | OD17. Deprecated in the iOS 27 SDK, and it put the whole media path in a separate process under a ~50 MB jetsam limit, behind an App Group, a shared Keychain group and a Darwin-notification status channel. It is **§9.1's fallback**: it still works on iOS 27, so if IO0 shows iOS suspending the capturing app, it comes back as the pre-OD17 D6/D7/D17/D18 (this file's history at commit `896e56d`). |
| **Universal links in v1** | Need Associated Domains, which needs the paid team and an AASA file served by `gawk-app`'s nginx. Both belong with distribution (§5); v1 uses a `gawk://` scheme (D20). |

## 4. Decisions

### D1 — Layout: `gawk-ios/`, a Rust workspace and an XcodeGen project

```
gawk-ios/
  rust/
    Cargo.toml            # workspace; path deps into ../../gawk-broadcast-desktop/crates
    crates/
      core/               # the UniFFI surface (D3): one cdylib/staticlib, both targets
      broadcast/          # ScreenCaptureKit → engine glue: sample intake, rotation, rung (D8–D11)
      viewer/             # subscribe, reassembly, parity repair, decode, timing (D13–D16)
  app/
    project.yml           # XcodeGen (D2)
    Gawk/                 # the SwiftUI app: Watch, Broadcast, Rooms, Settings, capture
    GawkTests/            # Swift unit tests, run in the Simulator
  scripts/build-core.sh   # cargo build + uniffi-bindgen, Xcode's first build phase (D2)
  README.md               # build, run on a device, the IO0 instrument
```

`rust/` path-depends on `gawk-broadcast-desktop/crates/{wire,engine,capture,encode,audio}`.
`capture` is in the set for its portable and Apple pieces only: `gate.rs`
(`FpsGate`), `fit.rs` (`fit_within`), `host.rs` (the `mach_absolute_time`
host clock) and `sck_policy.rs`'s `ENCODER_MAX_IN_FLIGHT`. `sck_policy` is
already an ungated, portable module (it imports only `crate::gate` and
std), so using it compiles no ScreenCaptureKit code.
**There is no fifth wire mirror** (CLAUDE.md: reuse, never mirror): the
desktop `crates/wire` and its golden vectors are the iOS app's too. Its
tests run in iOS CI as well, so a `gawk-server/wire/**` change triggers the
iOS job (D24).

### D2 — The Xcode project is generated, not committed

XcodeGen reads the checked-in `project.yml`. `*.xcodeproj` is
git-ignored. A Run Script build phase calls `cargo build` for the active
SDK and architecture and runs `uniffi-bindgen` against the library it just
built, so Xcode's Run builds Rust as well and the bindings always describe
the library being linked. Chosen by the owner (OD15) over a committed
`.xcodeproj` (with Xcode 16's synchronized folders) and Tuist.

**Rationale**: `project.pbxproj` merge conflicts are the standard failure of
a hand-maintained Xcode project. A generated one keeps CI and every
checkout identical.

### D3 — One Rust static library, linked into the app

`crates/core` builds a static library (`libgawk_core.a`, device
`aarch64-apple-ios` or simulator `aarch64-apple-ios-sim`, whichever Xcode is
building), linked straight into the app; its generated Swift bindings are
compiled into the app target. UniFFI's surface is callback-shaped: Swift
hands the core sample buffers' raw planes, timestamps and orientation, and
receives status, decoded frames and audio packets through callback
interfaces. Neither side exposes tokio or objc2 types across the boundary.

**Rationale**: since OD17 there is one process, so there's nothing to share a
framework with. *Revised 2026-10-04*: the first draft's XCFramework,
embedded once and linked by the app and the extension, existed for the
extension.

### D4 — Shared desktop crates gain iOS gating, not iOS code paths

- **`cfg(any(target_os = "macos", target_os = "ios"))` on exactly two
  modules**, `encode/vt.rs` and `capture/host.rs` (the host clock;
  `engine/src/clock.rs` has only the `Clock` trait and an `Instant` clock),
  plus the matching objc2 dependency lines. objc2's framework crates already
  build for iOS. **`vt_policy` and `sck_policy` need no change**: both are
  ungated, portable modules that run as host tests everywhere, and the Linux
  encoder uses `vt_policy` directly (`encode/src/gst.rs`), so gating either
  would break the Linux build.
- **A `self-update` cargo feature, default on, that iOS turns off.** It gates
  `engine::update`, `engine::install` and `minisign-verify`. App Store apps
  must not update themselves. **`ureq` stays**: `engine::telemetry` posts
  reports through a `ureq::Agent` and is ungated, and D23 needs it.
  `flate2` and `tar` are already Linux-only target dependencies, so they
  need nothing.
- **Nothing else.** No iOS module in the desktop crates. ScreenCaptureKit and
  UIKit code lives in `gawk-ios`.

Each of these is a desktop-crate change, so OD9's coupling rule applies: the
PR carries a `gawk-ios`-scoped commit, and the desktop job's `cargo test`
must be unchanged (G12).

### D5 — Identity, origin and the R59 labels

- **Distribution**: `gawk-ios`, injected with `engine::defaults::set_this`
  (docs/58 D2; `set_this` lives in `pub mod defaults` and isn't re-exported), with telemetry `kind: "gawk-ios"`.
- **Origin**: every dial sends `Origin: gawk://ios`. The desktop engine
  already sets the header ([transport.rs], docs/38 D19), and the production
  relay's `-allowed-origins` gains the value before first use. Whether the
  allowlist is checked on `/subscribe` as well as `/publish` is a V-item
  (V-10); the value is added either way.
- **Metrics**: dials send `app=ios&os=ios`. `os=ios` is already in R59's
  vocabulary (docs/61 D1). `app` gains `ios` in a `gawk-server` commit; a
  relay before it labels the app `other`, which is the designed degradation.

[transport.rs]: ../gawk-broadcast-desktop/crates/engine/src/transport.rs

### D6 — The app owns the broadcast media path, in process

```
SCStream (display, from SCContentSharingPicker; D18)
  output .screen  ─▶ orientation + FpsGate ─▶ VTPixelTransferSession (convert/rotate/scale, D8, D9)
                                                 │ IOSurface CVPixelBuffer (420v), host PTS
                                                 ▼
                                      VTCompressionSession (encode/vt.rs, D10)
                                                 │ AVCC → Annex-B, SPS/PPS before IDR
                                                 ▼
  output .audio   ─▶ ASBD-driven shim ─▶ resample 48k ─▶ Framer ─▶ libopus (D11)
                                                 │
                                                 ▼
                               engine session (send policy, resume, timesync,
                               parity, telemetry; R55 carriers
                               once WU1/WU2 land) ─▶ wtransport ─▶ relay
  output .microphone ─▶ never added (OD8)
```

While a game is in front, the app runs in the background under the
`screen-capture` background mode (D28); without it iOS stops the stream with
`SCStreamErrorMissingBackgroundMode`. **Whether iOS 27 keeps it running is
the milestone's main risk** (OD17, §8), and IO0 measures it first. The
broadcast pipeline is the whole broadcaster above the engine's seams
(`VideoSource`, `AudioSource`, `Clock`, `RelaySession`), exactly as the macOS
shell is above them (docs/54 §5), and it runs whether or not any screen is
showing. The viewer (D13–D16) shares the process but not the pipeline.

### D7 — Capture lifecycle maps onto the engine's states

| ScreenCaptureKit | Engine |
|---|---|
| The picker's observer reports a chosen display (`contentSharingPicker(_:didUpdateWith:for:)`) | Start the `SCStream`. With a stored resume token for the selected server, try `/publish/{id}` with it (R17); otherwise mint. Show the code. |
| Frames arrive with `SCFrameStatus` `.suspended` (the system paused capture, e.g. a call; V-5) | Stop feeding encoders; keep the session and keepalive. Viewers see the broadcaster away, as with desktop **Pause** (docs/64). |
| `.complete` frames again after `.suspended` | Force an IDR (docs/54 D7's on-demand IDR) and continue. |
| The user stops: the app's **Stop**, or the system's indicator (`stream(_:didStopWithError:)` with a user-stopped code) | Close the session cleanly. Keep the resume token for the grace period so a restart within 5 minutes gets the same code. |
| `didStopWithError` with any other code (`SCStreamErrorMissingBackgroundMode`, `SCStreamErrorSystemStoppedStream`, …) | Close the session and say why in the app; the next start reclaims the code. |
| Close code **4000**, **4004** or **4006**, or a `SessionClosing` (0x17, R57) naming one | Stop the `SCStream` and show the engine's user-facing sentence for that code. All three are terminal for a publisher (`engine::resume::terminal_for_publisher`; 4006 is an operator kill, R39), so no auto-resume. The set comes from the engine, never restated. |
| 4001–4003, transport loss | The engine's resume loop, unchanged. |
| Jetsam / crash | Nothing runs. The relay holds the slot through the GC grace; the next start reclaims it with the stored token (G6). |

### D8 — Rung: 1080p60, 500 ms GOP, 8 Mbps peak *(provisional)*

- **Size**: the panel is ≈ 1320 × 2868 on a Pro Max iPhone, more on an iPad
  Pro. Fit the upright frame into a 1920 × 1920 long-edge box with the shared
  fit rule (`capture::fit::fit_within`; aspect kept, never upscale, even
  dimensions). A portrait phone streams 884 × 1920; landscape 1920 × 884.
  `SCStreamConfiguration`'s `width`/`height` are requested at the fitted
  size, but iOS 27 has no `scalesToFit`, `preservesAspectRatio` or
  `pixelFormat`, so `VTPixelTransferSession` still converts to `420v`, and
  scales whatever size arrives (V-6 records what iOS delivers).
- **Frame rate**: frames arrive when the screen changes, up to the display
  rate; ProMotion panels can deliver 120. iOS 27 has no
  `minimumFrameInterval`, so `capture`'s `FpsGate` caps at 60.
  PTS pass through (VFR, docs/54 D7).
- **Bitrate**: 8 Mbps peak / 75 % mean, below the desktop's 12 because
  cellular uplinks are the common case. A **Cellular** preset (720p30,
  3 Mbps) is offered, and selected automatically on an expensive
  `NWPath` (D19).
- **GOP**: 500 ms, forced by the app (docs/54 D7: VT low-latency mode is
  infinite-GOP after the IDR).

**Provisional on IO0** (memory) and IO8 (thermals).

### D9 — Rotation is a resolution change

ReplayKit delivered the buffer in the panel's native orientation with the
device orientation in an attachment; whether ScreenCaptureKit on iOS does
the same or rotates for us is V-7. Either way the wire has no rotation field
and viewers trust the frame in hand (CLAUDE.md), so the pipeline makes
frames upright before encode: in `VTPixelTransferSession` (with D8's
convert and scale, one pass, one pool) when they arrive in panel
orientation, or not at all when they arrive upright. On an
orientation change it recreates the compression session for the new size
**inside the same publish session**: a new `DecoderConfig` and an IDR, the
code unchanged and the relay's caches simply replaced by the new keyframe.
That's what the web broadcaster already does when its frame size moves
(the ladder's dimension check, docs/08 and docs/09; `broadcaster.ts`
recreates the encoder whenever the preprocessed frames stop matching it),
and what viewers already handle (they reconfigure when the config carried
with a keyframe changes). It is **not** the desktop's quality change while
live, which restarts the publish session and invalidates the caches
(docs/64 OD1, D8). A rotation must not cost a session. The change is **debounced 500 ms**, so an
iPad turned back and forth doesn't flood keyframes. Face-up and face-down
keep the last orientation.

### D10 — Encode: the docs/54 D7 invariants, unchanged

`encode/vt.rs` is the macOS encoder, cfg-widened (D4). Every row of
docs/54 D7's invariant table holds on iOS, and the trial-gate shape is the
same: synthetic `420v` buffers through a session built like the live one,
before going live. **No software encode rung** (docs/38 §7): every device on
the iOS 27 floor has a hardware H.264 encoder, so the refusal path is
unreachable in practice but kept, with its message pointing at a desktop
broadcaster. **Backpressure** is docs/54 D10's gate, `ENCODER_MAX_IN_FLIGHT`
(3, in `capture`; `encode/mft.rs`'s `MAX_IN_FLIGHT` is the Windows one): at the
limit the incoming capture buffer is dropped and counted (favor dropped
frames), and its `CVPixelBuffer` is released at once so ScreenCaptureKit's
pool never starves.

### D11 — App audio: R25's Opus contract, format read from the buffer

- **Source**: the stream's `.audio` output, with `capturesAudio` on,
  `sampleRate` 48 000 and `channelCount` 2 requested, and
  `excludesCurrentProcessAudio` on, so the app's own player never loops into
  its broadcast. What it carries on iOS (the whole system's audio, or only
  the foreground app's) is V-3.
- **Format** is read from every buffer's `AudioStreamBasicDescription`,
  never assumed: the requested rate is a request, and ReplayKit's app audio
  was reported as 44.1 kHz and big-endian 16-bit on some versions. A shim
  converts to interleaved `f32`, resamples to 48 kHz when it must (a
  fixed-ratio polyphase resampler in `crates/broadcast`, allocation-free on
  the hot path), and feeds the shared `Framer`.
- **The encoder side** is the shared `audio` crate, untouched: libopus
  48 kHz stereo, 128 kbps constant, 20 ms frames, DTX/FEC off,
  `RESTRICTED_LOWDELAY`.
- **One clock** (docs/54 D5): ScreenCaptureKit stamps video and audio
  buffers with host-clock PTS on macOS, so the macOS host `Clock` (`capture/src/host.rs`,
  `mach_absolute_time`, cfg-widened by D4)
  serves both and A/V skew is zero by construction. V-4 verifies iOS does
  the same.
- **Audio never fails a broadcast** (R25 Decision 6): any audio failure
  drops audio, says so in the app's live status and leaves video running.

### D12 — Transport: the engine's, one runtime thread *(provisional)*

The engine runs on a **current-thread** tokio runtime on one dedicated
thread, not the desktop's multi-thread runtime. *Revised 2026-10-04*: this
was for the extension's ~50 MB limit, which OD17 removed; it stays because a
backgrounded app is a jetsam candidate too and one thread is all the send
path needs, and IO0's footprint measurement (V-1) can relax it. Uplink
transport is whatever the engine negotiates (OD11): today datagrams, plus
R29 parity when `RelayCapabilities` asks for it. **R55's per-GOP reliable
carriers are not built yet**: `CapUplinkCarriers` exists only in docs/57,
and the relay ingest (WU1) and engine carrier (WU2) are not started. When
they land the broadcast pipeline inherits them with no iOS change; until then **no
R65 acceptance criterion assumes them**, and G2/G3 are measured on
datagrams. **QUIC connection
migration is disabled.** The relay sits behind a UDP load balancer whose
kube-proxy conntrack keys on the 5-tuple, so a migrated path can land on
another pod (docs/22). On a path change (D19) the engine reconnects with its
resume token instead.

**Provisional on IO0** (V-1).

### D13 — The viewer core: the SPA's wire contract, in Rust

`crates/viewer` is new code. The engine is broadcaster-only, and no Rust
viewer exists. It implements, against the shared `wire` crate's parsers:

| Wire | Handling in v1 |
|---|---|
| `VideoChunk` (0x01) | Reassembly per frameID, chunk table, serial-arithmetic ordering. |
| `StreamFrame` (0x04) on a uni stream | Keyframes. An IDR resets the delta dependency chain. |
| `DecoderConfig` (0x02) | Codec string and description. A change resets the decoder (D9's rotations arrive this way). |
| `AudioFrame` (0x07), `AudioConfig` (0x08) | Opus packets, decoded by libopus (D16). |
| `TimeSync` (0x05), `ClockMapping` (0x06) | Capture → render latency for the stats sheet (G3, G8). |
| `ParityChunk` (0x0E), `RelayCapabilities` (0x0F) | R29 repair of delta chunk loss (D14). |
| `ViewerCount` (0x0B), `DeliveryAck` (0x0C), `TelemetryHello` (0x0D) | Display, the delivery truth row, the telemetry token. |
| Room types (0x13–0x16), `SessionClosing` (0x17) | R42 rooms (D21), R57 in-band closes. |
| Close codes | **Terminal for a viewer: 4000 (broadcast ended) and 4006 (operator kill)**; no reconnect, and the player says why. 4001–4003 reconnect. The set lives in shared code: IO4 adds `terminal_for_viewer` beside the close-code constants in `crates/wire` (with `CLOSE_CODE_TERMINATED_BY_OPERATOR`, if the mirror lacks it), tested against `wire.go`'s doc comments, so the viewer never restates it. |
| `ReliableCarrier` (0x0A), `StripeState` (0x10) | **Not in v1.** The viewer never requests `delivery=reliable` or stripes, so the relay never sends them (both are opt-in by dial parameter). |

**Delta loss rule**: a delta frame that can't be completed or repaired by
its deadline is dropped, and every delta after it is skipped **until the
next keyframe** (≤ 500 ms with the forced GOP). Decoding a broken reference
chain shows corruption; skipping shows a short freeze. Favor dropped frames
over corrupted ones.

### D14 — Parity repair moves into the shared `wire` crate

The canonical Go package recovers chunks (`RecoverChunks`,
`gawk-server/wire/parity.go`, tested in `parity_test.go` and
`internal/transport/parity_loss_test.go`), and the SPA's TS does too. The
Rust mirror computes R29's P/Q symbols but **deliberately doesn't mirror
recovery** (`crates/wire/src/parity.rs`: "reconstruction is the viewer's
job"), because until now no Rust code was a viewer. This milestone makes one,
so IO4 reverses that note: it adds `recover_chunks` to `crates/wire`, with
the recovery vectors restated **from the canonical Go tests** (the house
rule for mirrors: vectors restated, never imported) and the module comment
updated to say why. It's a desktop-crate change under OD9; the desktop apps
gain a tested function they don't call.

### D15 — Playout: an `AVSampleBufferRenderSynchronizer` at R12's adaptive offset

- **Video**: H.264 is enqueued **compressed** to an
  `AVSampleBufferDisplayLayer` (via an `AVSampleBufferVideoRenderer`),
  which decodes in hardware itself. VP8 and VP9 are decoded by libvpx in
  `crates/viewer` to `CVPixelBuffer`s from an IOSurface pool and enqueued
  decoded. If IO0's probe finds VideoToolbox decodes VP9 on iOS 27, VP9 takes
  the compressed path and libvpx covers VP8 only.
- **Audio**: decoded PCM is enqueued to an `AVSampleBufferAudioRenderer`
  under the same `AVSampleBufferRenderSynchronizer`, which owns A/V sync.
  With no audio track the synchronizer runs on the host clock alone.
- **Timebase**: frames are stamped in the broadcaster's capture clock.
  The synchronizer's time is anchored so a frame plays `offset` after its
  arrival. `offset` is docs/17 Decision 6's estimator, ported to
  `crates/viewer` with the SPA's constants (`transport/playout.ts`):
  `clamp(arrivalP95 − arrivalMin + 34, 50, 350)` ms, recomputed every
  500 ms (the SPA's stats tick), **seeded at 150 ms for the first 5 s** while the jitter window
  fills, slewed up fast (50 ms/s) and down slowly (5 ms/s, after 15 s below).
  The slew moves each sample's presentation time gradually
  (`ts + baseline + offset`), never as a step: the "rate a fraction off
  1.0" this paragraph first described, carried in the timestamps (§12). On a clean link it
  settles near 50 ms. A constant is not an option: it is a rejected design
  (docs/12 Decision 7, docs/17 Decision 10).
- **Presets**: the SPA's two non-reconnecting playout presets (docs/37):
  **Balanced** (the default, the estimator above) and **Lowest latency**
  (`off`: video samples carry `DisplayImmediately` and audio is scheduled
  at the minimum offset, 50 ms). The two `resilient`-delivery presets wait
  for R19 delivery in the viewer (§5).
- **Drop to live**: if the newest received PTS runs more than **2 × `offset`**
  ahead of what's presented (after a stall, a background trip or an
  outage), flush both renderers, wait for the next keyframe and re-anchor.
  This is a native rule: the web has no PTS-distance check and jumps on
  decoder backpressure instead, which the native player also does (a deep
  renderer queue requests the same resync). G10 tests it. It never slows
  playback to catch up; it jumps.

### D16 — Audio decode: libopus in Rust

The `opus` crate (already in the tree for encode) decodes to 48 kHz
stereo `f32`, which Swift wraps as LPCM `CMSampleBuffer`s with the
packet's PTS. Apple's AudioToolbox can also decode Opus, but keeping decode
in Rust means CI tests it on the host, where an Apple-only decoder couldn't
be tested.

### D17 — Settings and secrets: one process, no sharing

| What | Where |
|---|---|
| Selected server, room to attach, quality preset, telemetry opt-in | the app's standard `UserDefaults` |
| Per-server secrets (R37, docs/40), R17 resume tokens | the app's Keychain, `kSecAttrAccessibleAfterFirstUnlock` (a broadcast resumes behind a locked screen) |
| Live status: code, viewers, state, last error | in memory, observed by the UI |

*Revised 2026-10-04 (OD17)*: the App Group, shared Keychain group and
Darwin-notification status channel existed to talk to the extension, and
went with it. None of this needs a paid team.

### D18 — Starting and stopping a broadcast

The Broadcast screen's **Start** presents the system's
`SCContentSharingPicker` for the display
(`presentPickerUsingContentStyle(.display)`), with `showsMicrophoneControl`
and `showsCameraControl` off (OD8, OD1). Choosing the screen starts the
stream (D7). The code appears as soon as the relay assigns it, with **Copy
link**, **Copy code** and the share sheet. Stopping is the app's **Stop**
or the system's capture indicator; unlike ReplayKit, the app can stop its
own capture.

Display capture takes **everything on screen**, notifications included.
Before the first broadcast the Broadcast screen says so, with a pointer to
Focus modes. Protected (DRM) content is black in the capture, as the system
dictates.

### D19 — Network paths

An `NWPathMonitor` in the app tells the core when the path changes
(Wi-Fi ↔ cellular) and whether it's expensive. A path change triggers an
immediate resume reconnect rather than waiting for idle timeouts (D12). An
expensive path selects the Cellular preset at broadcast start. It doesn't
switch mid-broadcast, which would be an unannounced quality change.

### D20 — Joining: code, `gawk://` link, room

The Watch screen takes a typed code (the SPA's format and validation),
or a `gawk://watch/<CODE>` or `gawk://room/<name>` link. The query
parameters `relay=` and `nick=` mean what they mean on the web (docs/40,
the 2026-10 room nickname). A `relay=` link to a non-default server shows
docs/40's persistent strip, as the desktop apps do (docs/64 D3).

*Revised 2026-10-03*: the grammar, including a third path,
`gawk://broadcast?room=&nick=&relay=`, is now defined once for every
native app in [docs/68](68-desktop-gawk-links.md) D1 and parsed by the
shared `engine::link` (docs/68 D2), which IO6 uses rather than parsing
links itself. The broadcaster UI (IO3) handles `broadcast` links by
prefilling, as the desktop does (docs/68 D4).

### D21 — Rooms

- **Broadcaster**: the room to attach is chosen in the app before Start
  (saved and recent rooms, as docs/60 has). The pipeline attaches with the
  shared `engine::room` code (R42), unchanged.
- **Viewer**: a room view in **grid** and **focus**, the SPA's two layouts.
  Each participant is one subscribe session and one renderer. At most
  **four tiles play at once**: in a larger room the rest show their last
  frame, with a tap to swap. Focus plays one tile at full rate and pauses
  the others' decode (the sessions stay up). *Provisional on IO8 thermals.*

### D22 — PiP and background audio

`AVPictureInPictureController` with the
`ContentSource(sampleBufferDisplayLayer:playbackDelegate:)` source. The
audio session is `.playback`, with background mode `audio` (beside the
broadcaster's `screen-capture`, D28), so PiP and a locked screen keep audio. The playback delegate reports a live stream
(`isPlaybackPaused` false, an infinite time range), so the PiP window shows
no scrubber. Entering the background without PiP keeps audio only and
stops video decode.

### D23 — Telemetry, server picker, terms

- **Telemetry** (R28, docs/33): off until the user opts in, as on desktop.
  The engine's telemetry for the broadcast; the viewer core reports the
  SPA's viewer report shape, keyed by the `TelemetryHello` token.
  `kind: "gawk-ios"`; `gawk-telemetry` accepts the kind (IO7).
- **Server picker** (R37, docs/40): the probed server list via `/echo` and
  `RelayIdentity` (0x11), per-server secrets in the Keychain (D17), and the
  compiled-in default (CLAUDE.md: the official deployment is the default
  target).
- **Terms**: the desktop apps' TC5 posture, a link in Settings.

### D24 — CI: `ios.yml` on GitHub's `xcode-27` image

- **Triggers**: `gawk-ios/**`, `gawk-broadcast-desktop/crates/{wire,engine,capture,encode,audio}/**`,
  `gawk-server/wire/**`. The desktop workflow gains `gawk-ios/rust/**`, so a
  shared-crate change runs both sides.
- **Rust**: `cargo test` on the macOS host (wire vectors, viewer reassembly,
  parity recovery, the resampler, the offset estimator against the SPA's
  `playout.test.ts` cases restated, the drop-to-live rule); `cargo build` for
  `aarch64-apple-ios` and `aarch64-apple-ios-sim`; and two integration
  tests against a real `gawk-server` binary, on the desktop engine's
  `tests/support/relay.rs` harness:
  - **Publisher (IO2)**: `crates/broadcast` driven on the host with
    synthetic capture-shaped buffers (portrait, then a rotation), on the
    current-thread runtime with `self-update` off, through a rolling relay
    restart. It must resume on the same code with frameID continuity, the
    shape of the engine's existing `resume_survives_a_relay_restart`, but
    through the iOS glue (G6's CI half).
  - **Viewer (IO4)**: the viewer core subscribed to a broadcast from
    `gawk-pubsim`, through a relay restart and induced datagram loss
    (parity repair, the delta-loss rule, re-subscribe).
- **Xcode 27, selected explicitly**: the jobs run on the `xcode-27` runner
  label (GitHub's images are one per major Xcode, and `macos-latest` had
  Xcode 26 only in October 2026) and `xcode-select` the Xcode whose
  `xcodebuild -version` is the pinned one, never a path: the image ships
  Xcode 27.0 at a release-candidate path.
- **Xcode**: `xcodegen`, then `xcodebuild build-for-testing` unsigned
  (`CODE_SIGNING_ALLOWED=NO`) and `test` on an iOS 27 simulator: Swift unit
  tests and a Watch-screen smoke test against the same local relay.
  Whether ScreenCaptureKit captures in the Simulator is V-12; CI drives the
  broadcast pipeline with D27's source either way.
- Signing, archives and TestFlight upload belong to §5's milestone.

### D25 — Versioning and conventions

- A `gawk-ios` release-please component (tag `gawk-ios/vX.Y.Z`), starting at
  0.1.0. It publishes no artifact in R65 (OD3). Its version is the app's
  `CFBundleShortVersionString`.
- Commit scope `ios` in `CONTRIBUTING.md`'s table, with OD9's coupling rule
  written next to `gawk-admin`'s.
- When IO1 lands, CLAUDE.md's repository layout gains a `gawk-ios` entry
  for the facts a reader can't derive: the path dependency, the coupling
  rule, and that the app owns the capture path in process (OD17).

### D26 — Simulator first: what phase S can prove, and what waits for a device

| Runs in the iOS Simulator | Device only |
|---|---|
| The SwiftUI app, every screen and flow | **Background execution**: whether iOS keeps the capturing app running behind a game (the `screen-capture` mode, OD17). Simulator apps are Mac processes and aren't held to device suspension or jetsam |
| The Rust core (it's an `aarch64-apple-ios-sim` build, the same code) | **Memory**: the footprint behind a game and its jetsam headroom (V-1) |
| The viewer end to end: subscribe, reassembly, parity, libvpx, Opus, `AVSampleBufferDisplayLayer`, the synchronizer, the adaptive offset | **Thermals** and battery |
| PiP and background audio, functionally | **Hardware encode behaviour**: VideoToolbox in the Simulator runs on the Mac's encoder, so its latency and rate control say nothing about the phone's (V-11) |
| The broadcast pipeline below capture, fed by D27's synthetic source, and real ScreenCaptureKit capture if the Simulator provides it (V-12) | The device's real capture buffers: size, orientation, audio format, clocks (V-3, V-4, V-6, V-7) |
| Rooms, links, server picker, telemetry, the Keychain plumbing | Glass-to-glass latency on the real path |
| Network changes (Network Link Conditioner on the Mac) | Cellular, and Wi-Fi ↔ cellular handover |

So phase S proves the code is correct and phase D proves it fits. The
pre-registered memory, thermal and latency criteria (G2, G3, G8, §9.1) are
judged on devices only. A Simulator number is never quoted as evidence for
them.

### D27 — A synthetic broadcast source for phase S

The app gains a **debug-only** "Test broadcast" that runs `crates/broadcast`
fed by a generated source instead of ScreenCaptureKit: a moving test
pattern with a frame counter and a clock, as `CVPixelBuffer`s shaped like
the capture's (portrait, then rotations on a timer), plus a tone as
audio-output-shaped buffers. Same `VideoSource`/`AudioSource` seams, same
rotation, rung, encode and engine code as real capture. It's compiled out of
release builds and never reachable by a user.

### D28 — Identifiers

Every identifier hangs off the bundle ID (OD16), which is permanent once the
app ships, so they're fixed here and nowhere else:

| What | Value |
|---|---|
| App bundle ID | `fi.ioio.gawk` |
| URL scheme (D20) | `gawk://` |
| Display name (`CFBundleDisplayName`) | gawk |
| Background modes (`UIBackgroundModes`) | `screen-capture` (D6), `audio` (D22) |

*Revised 2026-10-04 (OD17)*: the extension's bundle ID
(`fi.ioio.gawk.BroadcastUpload`), the App Group and the Keychain access group
are gone with the extension. Nothing had shipped, so nothing permanent was
spent.

The App Store listing name is separate and must be unique across the store.
It's chosen in the distribution milestone (§5), and may differ from the
display name.

## 5. Non-goals

- **TestFlight and App Store distribution.** A follow-up milestone covers
  signing in CI, App Store Connect upload, the privacy manifest and labels,
  user-generated-content report and block (guideline 1.2, with R39/R40),
  universal links with an AASA file served by `gawk-app`, and how the
  compiled-in default relay is presented in review.
- Mic audio and commentary (OD8); camera broadcasting (OD1).
- R19 reliable delivery, R21 DVR, R30 striping in the viewer (D13). They're
  opt-in by dial parameter and can follow.
- Porting R12's presentation machinery or interpolation (OD7). The offset
  estimator is ported (D15).
- Any change to the Safari viewer, R16/R22 included.
- Android; a native viewer on desktop.

## 6. What deliberately does not exist

Inherited from docs/38 §7 and docs/54 §7: no software encode; no
viewer→server keyframe back-channel; no auto-resume through 4000/4004/4006
(publisher) or reconnect through 4000/4006 (viewer); no
second clock; no standalone `DecoderConfig` datagrams; no Opus
DTX/FEC/bitrate knob. Added here: no app extension (OD17); no
QUIC connection migration (D12); no self-update (D4); no raw broadcast IDs
in logs or telemetry (CLAUDE.md, R9 D3).

## 7. UX flows

**First broadcast (G1)**: open the app → Broadcast → the notification
note (D18) → **Start** → the system picker → the screen → the code and
**Copy link** in the app → switch to the game. Zero settings touched.

**Watch by link**: tap a `gawk://watch/<CODE>` link → the app opens
straight into the player → rotate for fullscreen → swipe home and PiP
continues (G9).

**Rooms**: Broadcast → room field (saved/recent) → Start; viewers who open
the room on the web see the stream join. In the app, Rooms → a room code →
grid; tap a tile for focus.

**Resume**: the relay pod restarts → the broadcast resumes on the same
code with an IDR (G6); after a jetsam, the user relaunches and starts again
within the grace and gets the same code.

**Errors**: 4000/4004/4006 end the broadcast with the system's alert carrying
our reason (D7). A full fleet or a refused secret says which, in the app.

## 8. Risks

| Risk | Mitigation |
|---|---|
| iOS 27 suspends the capturing app behind a game despite the `screen-capture` mode (reported by one project, OD17) | IO0 measures it first; §9.1's Fail branch restores the ReplayKit extension (§3) |
| Jetsam of the backgrounded app with the engine, quinn, VideoToolbox, the conversion pool and Opus | IO0 records the footprint (V-1); D12's current-thread runtime; D8's cap |
| Thermal throttling with a game, encode and upload together | IO0/IO8 log `ProcessInfo.thermalState`; step down a rung on `.serious`, announced in the live status |
| The capture's audio format or content differs from what's requested | D11 reads the ASBD per buffer; V-3 records what devices deliver |
| libvpx software decode drains the battery on long watches | Only VP8/VP9 sources (Firefox broadcasters, the minority) take it; IO8 records the cost |
| The viewer core drifts from the SPA's wire behaviour | It's tested on recorded streams and against the real relay in CI (D24), and shares `crates/wire`'s parsers |
| quic-go / webtransport-go bumps have broken WebKit before (gotchas) | Not exposed: the iOS app speaks the desktop's Rust stack, not WebKit's |
| App Review later | Out of scope here, written into §5 so the follow-up starts from it |

## 9. Chunks and acceptance criteria

Chunks run in two phases (OD13). **Phase S, Simulator**: IO1, IO4, IO5,
IO6, IO3, IO7, and IO2's pipeline on D27's source, each accepted on its
*Simulator* criteria below. **Phase D, devices**: IO0 first, then IO2's and
IO5's device criteria, then IO8. IO0's verdict can revise D8 and D12, which
then lands as a change to the already-built pipeline, not a redesign of
phase S.

| Chunk | Scope | Accepted when |
|---|---|---|
| **IO0** (phase D, first) | **Spike (throwaway branch, never merged).** A minimal app linking the engine and `vt.rs` with D4's gating; capture the display through `SCContentSharingPicker` at 1080p60 with audio and broadcast to the fleet with a game in front. Log every capture gap over 2 s, `phys_footprint` every second, thermal state and dropped frames; photograph glass-to-glass. Probe `VTIsHardwareDecodeSupported(kCMVideoCodecType_VP9)` and record the capture's buffer size and format, orientation behaviour, audio ASBD and clocks (V-3–V-7). | §9.1's verdict and the V-items are recorded in §12 |
| **IO1** | Scaffolding: D1–D4, D5's identity and origin, D24's CI, D25's component and conventions | CI is green on a PR that touches only a desktop crate; the desktop job's `cargo test` is unchanged with `self-update` on; the iOS host tests pass with it off; the simulator builds both targets |
| **IO2** | The broadcast pipeline: D6–D12, D19, D27 | **Simulator**: D27's test broadcast plays in a desktop Chrome viewer, rotations included, and survives a relay restart. **Device**: G1 (without the app UI: settings seeded by hand), G2, G3, G4, G5 on a device; G6 on a device; D24's **publisher** integration test (relay restart through the iOS glue) green in CI |
| **IO3** | The broadcaster UI: D17, D18, the server picker and secrets (D23), room attach (D21) | **Simulator** (through D27's source), then on a device: the code shows within 2 s of the first captured frame; a non-default server with a secret works; an attach shows in the room's web view (G11's first half) |
| **IO4** | The viewer core: D13, D14, D16; Rust tests on recorded H.264, VP9 and VP8 streams; D24's **viewer** integration test | Tests green in CI, the viewer integration test included; a `DecoderConfig` change mid-stream resets the decoder with no crash; parity recovery passes the restated vectors; the delta-loss rule is tested |
| **IO5** | The native player: D15, D22 | **Simulator**: G7 (all three codecs play), G9 (PiP and background audio work), G10 (outage via Network Link Conditioner). **Device**: G7–G10, G8's latency included |
| **IO6** | Joining and rooms in the viewer: D20, D21 | In the Simulator: a `gawk://` link opens the right broadcast; `relay=` shows the strip; G11's second half |
| **IO7** | Telemetry and metrics: D23's telemetry, D5's `app=ios` in `gawk-server`, the `gawk-ios` kind in `gawk-telemetry` | A test session appears in the telemetry dashboard; relay metrics label it `app="ios"` |
| **IO8** | The owner's device pass: iPhone 17 Pro Max and iPad Pro, broadcaster and viewer, H.264/VP9/VP8 sources, battery and thermal notes, D21's tile cap | G1–G12 recorded in §12 |

### 9.1 IO0 pre-registered verdict

*Revised 2026-10-04 (OD17)*: the first verdict judged the extension's
~50 MB budget. With capture in the app, the question that decides the design
is whether iOS keeps the app running behind a game.

On the iPhone 17 Pro Max, broadcasting a 3D game at D8's rung for 30 minutes:

- **Pass**: no capture gap over 2 s while the game is in front (the app was
  never suspended), no jetsam, glass-to-glass ≤ 250 ms to a desktop Chrome
  viewer, thermal state never `.critical`. D6, D8 and D12 are confirmed as
  written, and the peak footprint is recorded (V-1).
- **Conditional**: no suspension and no jetsam, but glass-to-glass over
  250 ms or `.critical` once. Drop the cap to 1280 on the long edge and the
  encoder pool to `ENCODER_MAX_IN_FLIGHT = 2`, measure again, and amend D8
  before IO2's device criteria.
- **Fail**: every other first-run outcome, which includes any capture gap
  over 2 s with the game in front, any jetsam, or `.critical` more than
  once. A suspension or a jetsam means stop: OD17 is reversed and the
  ReplayKit extension design (§3) is restored before IO2's device
  criteria, with its own memory verdict (the pre-OD17 §9.1). A thermal miss
  alone means re-measure at 1280 as for Conditional, and any miss on that
  re-measure is a Fail.

The three buckets are exhaustive: anything that isn't Pass or Conditional
is Fail. The Conditional re-measure is judged by Pass's criteria at the
1280 cap; passing it amends D8, and anything else is a Fail.

## 10. V-items (verified on device, recorded in §12)

| # | Question | Decides |
|---|---|---|
| V-1 | Peak footprint of the backgrounded app at 1080p60 and 720p30, multi-thread vs current-thread runtime | D8, D12, §9.1 |
| V-2 | Does VideoToolbox decode VP9 on iOS 27 (and AV1, for the record)? | D15 |
| V-3 | The capture's audio: whole system or foreground app only, and its ASBD (rate, endianness, interleaving) against the 48 kHz stereo requested | D11 |
| V-4 | Are ScreenCaptureKit's video and audio PTS on the host clock on iOS? | D11, G4 |
| V-5 | Does a locked screen or a call stop the stream, or deliver `.suspended` frames? | D7 |
| V-6 | Buffer size, pixel format and frame rate the capture delivers on a 120 Hz panel in a 60 fps game, against the `width`/`height` requested | D8 |
| V-7 | Are frames delivered in panel orientation with an orientation attachment, or already upright? In a landscape-locked game, which orientation? | D9 |
| V-8 | Footprint and thermal cost of four playing tiles on iPhone vs iPad | D21 |
| V-9 | Does PiP from `AVSampleBufferDisplayLayer` survive a renderer flush (drop to live)? | D15, D22 |
| V-10 | Does the relay check `-allowed-origins` on `/subscribe` too? | D5 |
| V-11 | Does `vt.rs`'s low-latency, hardware-required session open in the Simulator? If not, D27 uses a Simulator-only encoder spec without `RequireHardwareAcceleratedVideoEncoder` (debug builds only) | D27 |
| V-12 | Does `SCContentSharingPicker` display capture deliver frames in the iOS 27 Simulator? (answerable in phase S) | D24, D26 |

## 11. Open questions

None. All four were answered on 2026-10-03: Q1 (signing team) and Q4
(devices) became OD14, Q2 (project generation) OD15, and Q3 (bundle ID and
display name) OD16.

## 12. Deviations and field findings

IO0's verdict, the V-items and every deviation from §4 are recorded here,
dated.

- **2026-10-04 — the iOS 27 SDK deprecates the ReplayKit broadcast
  extension (OD17).** IO1's first Xcode 27 build warned that
  `RPBroadcastSampleHandler` is "No longer supported" and that
  `RPSampleBufferType` should become `SCStreamOutputType`. The iOS 27 SDK
  makes `SCStream`, `SCContentSharingPicker` (display style) and
  `SCStreamConfiguration`'s `width`, `height`, `capturesAudio`,
  `sampleRate`, `channelCount` and `excludesCurrentProcessAudio` available on
  iOS, with `SCStreamErrorMissingBackgroundMode` for an app that captures in
  the background without the mode; `pixelFormat`, `minimumFrameInterval`,
  `queueDepth` and `scalesToFit` stay macOS-only. The owner chose
  ScreenCaptureKit in the app (OD17); D3, D6–D11, D17, D18, D26–D28, §8,
  §9.1 and §10 were revised the same day. Public reports at the time:
  LiveKit's Swift SDK moved to ScreenCaptureKit on iOS 27
  (livekit/client-sdk-swift#1135), and another project removed its iOS 27
  ScreenCaptureKit mirror because the app was suspended in the background
  even with the `screen-capture` mode and `NSScreenCaptureUsageDescription`
  declared, while the deprecated extension kept working
  (MyNamesEMurray/LensLink#161). IO0 settles which holds for a game
  broadcast.
- **2026-10-04 — IO1: D5's labels needed one more engine change than D4
  allowed.** D4 said the shared crates gain iOS gating and nothing else, but
  `engine::relay::publish_url` hardcoded `app=desktop`, and `CLIENT_OS` fell
  through to `linux` on any target that wasn't Windows or macOS, so an iOS
  dial would have said `app=desktop&os=linux`. `Distribution` gained an `app`
  field (`desktop` for the three desktop distributions, `ios` for
  `gawk_core::identity::IOS`), `publish_url` reads it, and `CLIENT_OS` gained
  an `ios` arm. Both are pinned by tests in each workspace.
- **2026-10-04 — IO1: the shared crates depend on the engine without its
  default features.** `capture`, `encode` and `audio` took `gawk-engine`
  with defaults, which would have turned `self-update` back on through
  feature unification however the iOS workspace asked for it. They now say
  `default-features = false`; the desktop shells and `ui` keep the default,
  so the desktop build is unchanged. `ios.yml` fails if the resolved iOS
  graph contains `self-update` or `minisign-verify`.
- **2026-10-04 — IO1: build plumbing D2 didn't spell out.**
  `scripts/build-core.sh` is the Run Script phase: it unsets Xcode's
  `SDKROOT` (cargo's host build scripts can't link against the iOS SDK),
  runs cargo from inside `rust/` (rustup picks `rust-toolchain.toml` by
  directory, and UniFFI's library mode runs `cargo metadata` there), and
  generates the bindings from the static library it just built. The iOS
  workspace repeats the desktop workspace's `[patch.crates-io]` for the
  vendored wtransport, because a patch applies only in the workspace that
  declares it. The desktop component's release also bumps the shared
  crates' versions in `gawk-ios/rust/Cargo.lock` (a root-relative
  release-please extra file), so a desktop release never leaves the iOS lock
  stale. The core's Swift function is `initializeCore()`, not
  `initialize()`, which collides with `NSObject.initialize`.
- **2026-10-04 — IO4: where the native viewer does and doesn't follow the
  SPA.** `crates/viewer` ports `reassembler.ts`, `reorder-buffer.ts`
  (with its grace controller), `playout.ts`, `live-edge.ts`,
  `time-sync.ts` and `reconnect.ts` with their tests restated case for
  case. Deliberate differences: the delta-loss rule is D13's (never step
  over a hole), where the web's default allows one skipped delta per GOP;
  the recovered-frame ledger and arrival accounting are not ported (their
  only consumer is R30's stripe detector); the estimator runs on the web's
  500 ms tick, not every second as D15 first said; D15's drop-to-live check
  is new rather than a port (see D15). The session reads close codes the
  web can't: a 404 before any session connected ends the viewer, while a
  404 after one (a restarted relay that doesn't know the broadcast until
  its publisher reclaims it) is part of the reconnect ladder; an in-band
  SessionClosing (R57) names the code a bare close lacks.
- **2026-10-04 — IO4: `engine::transport::dial_subscribe`.** A second
  desktop-crate change beyond D4: the engine never subscribed, so its
  wtransport `connect` (Origin header, keepalive, the vendored refusal
  status) had no subscribe entry point. It's a five-line sibling of
  `dial_room` returning the same `RelaySession` seam, which the viewer's
  session and its fake-transport tests are written against. The engine's
  relay test harness now finds the repo root by walking up, so the viewer's
  integration test can include it.
- **2026-10-04 — IO4: libvpx through a pinned pre-release (OD12).** The
  stable `shiguredo_libvpx` (2026.1.0) runs libvpx's configure without a
  target, so it can only build for the host. `2026.2.0-canary.2`
  (2026-10-03) is the first release that builds libvpx v1.17.0 from source
  for `aarch64-apple-ios` and `aarch64-apple-ios-sim` (a small patch for the
  arm64 simulator). The owner accepted it pinned exactly (`=`) with the
  `source-build` feature: the build clones the libvpx tag from GitHub and
  compiles it, and no prebuilt binary is ever downloaded. It needs git,
  network access at build time and rustup's `llvm-tools` (symbol
  prefixing), which `rust/rust-toolchain.toml` now lists. Move to the stable
  2026.2.0 when it ships. libvpx is BSD-3; its notice belongs to the
  distribution milestone (§5), since R65 publishes nothing (OD3).
- **2026-10-04 — IO2: what the iOS 27 SDK and the Simulator answered.**
  - **V-12: no.** The iOS 27 *Simulator* SDK has no ScreenCaptureKit at all
    (only the device SDK does), so capture compiles for devices only and
    phase S broadcasts from D27's test source.
  - **V-11: no.** `vt.rs`'s low-latency, hardware-required session fails in
    the Simulator (`VTCompressionSessionCreate -12908`). As D27 planned,
    iOS Simulator builds (and only those, `target_abi = "sim"`) take a
    software session; it also needs `MaxFrameDelayCount = 0`, or it holds
    its first frames and D10's in-flight gate (3) starves it for good. It
    is slow (about a dozen AUs in 10 s of a 1320 × 2868 source), which D26
    already says means nothing.
  - **V-7, from the SDK**: iOS 27 attaches `SCStreamFrameInfoVideoOrientation`
    (a `CGImagePropertyOrientation`) to every frame, so frames come in panel
    orientation with an attachment, as ReplayKit's did; the device pass
    still decides which orientation a landscape-locked game reports.
  - **V-4, by construction**: `SCStream` exposes its `synchronizationClock`,
    so capture converts each PTS onto the host clock with
    `CMSyncConvertTime` instead of assuming they are host time.
  - **D9's "one pass" is two when rotating.** `VTPixelTransferSession`
    scales and converts but cannot rotate, and `VTPixelRotationSession`
    rotates but does not scale, so a rotated frame is scaled first (the
    smaller frame is what gets turned) and rotated second; an upright one
    is still one pass.
  - Phase S evidence: the `BroadcastLoopTests` XCTest broadcasts D27's
    source through the real `Broadcaster` to a local relay and plays it
    back through the core's own `Viewer` in the Simulator, a rotation
    included, with the tone; the relay restart is the host-side publisher
    integration test (D24), where the code and frame-ID space carry on.
- **2026-10-04 — IO6/IO7: rooms, links and telemetry, as built.**
  - **`engine::room::watch_room`**, a fourth desktop-crate addition: the
    shared room control session run for a viewer. It joins, reports the
    roster and never attaches, because no publish identity ever arrives.
    The iOS room view is built on it rather than a second room client.
  - `gawk://` links go through the shared `engine::link` parser (docs/68),
    never re-parsed in the app; a dropped parameter is reported by name
    only.
  - **V-10: yes**, from the relay's code (#460): one `CheckOrigin` guards
    every client-facing upgrade, `/subscribe`, `/room/*` and `/echo`
    included, so the production relay's `allowedOrigins` must list
    `gawk://ios` before the app first dials it, or watching, broadcasting
    and even the server picker's probe fail.
  - **Broadcaster telemetry** drives the engine's `Reporter` exactly as the
    desktop shell does (off until opted in; the advertised ingest wins; the
    opt-out wins over both). **Viewer telemetry is not built yet**: the
    reporter is broadcaster-only (its role and sample type are fixed), so
    the viewer's reports need it generalised first. IO7's acceptance ("a
    test session appears in the dashboard") is met by the broadcaster's
    reports; the viewer's remain open.
- **2026-10-04 — IO5: the native player, as built in the Simulator.**
  - **G7 (Simulator): all three codecs play.** The Watch smoke test went
    Live and enqueued video against `gawk-devpub` publishing the H.264, VP8
    and VP9 fixtures, with screenshots a second apart showing moving frames
    and fullscreen landscape.
  - **G9 (Simulator): background audio yes, PiP unverifiable.** Audio kept
    arriving at ~48 blocks/s for 25 s in the background while video
    enqueueing stopped. `AVPictureInPictureController.isPictureInPictureSupported()`
    is **false** in the iOS 27 Simulator, so the PiP path (D22's content
    source and live playback delegate) is built but first checked on a
    device.
  - **G10 (Simulator): recovered.** A 5 s relay freeze (SIGSTOP/SIGCONT)
    dropped to live twice; the offset then settled back to its 50 ms floor
    and held there, so the outage added no permanent delay.
  - **iOS 27's renderer API**: `addRenderer`, `enqueue`, `flush`, `status`
    and `isReadyForMoreMediaData` are deprecated in Swift, so the player uses
    the receiver API (`sampleBufferReceiver(adding:)`, `enqueueImmediately`,
    enqueue results and rendering events). Without "not ready", the
    backpressure resync (D15) fires on sustained lateness at enqueue (>0.25 s
    for 0.5 s), a backlog over 48 frames, or a decode failure, at most once a
    second.
  - **What's on screen is reported every 16 ms.** At 10 Hz the report lagged
    up to ~100 ms, which at a settled ~100 ms offset crossed D15's 2 × offset
    and dropped a healthy stream to live over and over; a test pins it.
  - **Background stops enqueueing, not decoding (D22).** VP8/VP9 still decode
    in Rust while backgrounded without PiP; stopping decode needs a core
    switch. The broadcast-ID alphabet is restated in Swift (`BroadcastCode`)
    until the core exports its check.
- **First device run (2026-10-05): every certificate failed with
  `UnknownIssuer`.** The engine's `with_native_certs` loads roots through
  `rustls-native-certs`, which has no iOS backend: it reads the Unix
  certificate directories, which an iPhone (and the Simulator) doesn't have,
  so it trusts nothing. Phase S never saw it because every Simulator relay
  ran with `insecure`. On iOS the engine now verifies through
  Security.framework (`rustls-platform-verifier`, `transport.rs`
  `ios_tls`); the desktops keep `with_native_certs`. `TlsTrustTests` dials
  the default fleet and fails on any certificate error (opt-in: it needs the
  internet).
