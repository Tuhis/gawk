# R65 — iOS app: native broadcaster and viewer (docs/67)

**Status**: proposed 2026-10-03. Owner decisions OD1–OD12 (§2) were taken
the same day in an interview. Chunks **IO0–IO8** (§9) are not started.
**IO0 is a throwaway spike whose pre-registered verdict (§9.1) gates every
later chunk.** Decisions marked *provisional* are confirmed or revised in
§12 once IO0 is done. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**

- Viewing in iOS Safari already works and **is not changed**. WebKit runs
  the whole WebTransport + WebCodecs worker pipeline, and R16/R22
  ([docs/21](21-ios-video-fullscreen.md), [docs/27](27-ios-mse-fullscreen.md))
  work around the missing Element Fullscreen on iPhone.
- The broadcast side is the macOS broadcaster's media path
  ([docs/54](54-macos-native-broadcaster.md)) moved into a ReplayKit
  extension. It reuses the `wire`, `engine`, `encode` and `audio` crates of
  [`gawk-broadcast-desktop`](../gawk-broadcast-desktop) **by path**, and
  inherits their invariants (docs/38 D9–D11, docs/54 D5–D10) rather than
  restating new ones.
- The viewer side is the first viewer outside `gawk-app`. It follows the
  wire contract the SPA reads (docs/03, docs/04, docs/12, docs/20,
  docs/34) and none of the SPA's playout code (OD7).

---

## 1. Why, and what "done" means

- **There is no way to broadcast from an iPhone or iPad today.** iOS Safari
  has no `getDisplayMedia`, and every iOS browser is WebKit. Capturing the
  screen outside your own app exists only as a ReplayKit **Broadcast
  Upload Extension**, so only a native app can broadcast from iOS.
- **Watching in Safari is second-class.** iPhone fullscreen depends on a
  fullscreen-only MSE surface whose on-device pass is still open (R22 MF5).
  There is no Picture-in-Picture, so you can't watch while playing a game
  yourself. Audio stops when Safari goes to the background.
- An `AVSampleBufferDisplayLayer` gives native fullscreen, rotation, PiP
  and background audio as platform features rather than workarounds.

### Milestone acceptance criteria

Pre-registered. "Device" means a real iPhone or iPad on iOS 26, on the
owner's hardware (Q4). CI cannot see ReplayKit, a hardware encoder's
behaviour under thermal load or PiP. As on every native milestone
(docs/19, docs/38, docs/54), the device criteria decide whether R65 works.

| # | Goal | Verified by |
|---|---|---|
| G1 | Starting a broadcast from the app's picker button streams the whole device screen with app audio to the default fleet, with no settings touched; a stock web viewer at `gawk.ioio.fi` plays it | device |
| G2 | A 30-minute broadcast while playing a 3D game: no jetsam of the extension, peak footprint within the IO0 budget, thermal state never `.critical` | device, logged footprint and thermal state |
| G3 | Glass-to-glass latency of an iOS broadcast to a desktop Chrome viewer is ≤ 250 ms, by R14 V4's photographed-reference method | device |
| G4 | A/V sync of an iOS broadcast: viewer-reported median `\|avSkewMs\| ≤ 60 ms`, p95 ≤ 120 ms over 60 s (R25's criteria, unchanged) | device, viewer diagnostics |
| G5 | Rotating the device mid-broadcast: viewers keep playing, at the new aspect, within one GOP (≤ 500 ms of frozen video) | device |
| G6 | The broadcast survives a relay pod restart and an extension restart within the grace period on the same code (R17 resume) | integration in CI (relay kill) + device |
| G7 | The native player plays H.264, VP9 and VP8 broadcasts (browser on Chrome, Firefox and the desktop apps as sources) | device + Rust tests on recorded streams |
| G8 | Native-player glass-to-glass latency is within 100 ms of Safari's on the same H.264 broadcast, measured side by side | device |
| G9 | PiP and background audio: playback continues in PiP over another app, and audio continues with the screen locked, for 10 minutes each | device |
| G10 | A 5 s network outage while watching recovers to the live edge with no permanent added delay (the drop-to-live rule, D15) | device (airplane mode toggle) + Rust test |
| G11 | Rooms: an iOS broadcast attaches to a room and shows in the room's web view; the iOS viewer plays a room of three in grid and focus | device |
| G12 | Nothing changes for anyone else: no wire change, the relay change is the R59 `app` vocabulary value and the origin allowlist value only, and the desktop apps' tests and behaviour are unchanged by D4's gating | CI + review |

## 2. Owner decisions (2026-10-03)

| # | Decision |
|---|---|
| OD1 | **Broadcast the device screen through ReplayKit** (a Broadcast Upload Extension). Camera streaming is out of scope. |
| OD2 | **A native player** (VideoToolbox / libvpx → `AVSampleBufferDisplayLayer`), not a WKWebView around the SPA, and not a broadcast-only app. |
| OD3 | **Signed for the owner's own devices first.** TestFlight and then the public App Store will follow, in a later milestone (§5). |
| OD4 | **SwiftUI over a Rust core**, bridged with UniFFI. |
| OD5 | **A new top-level module, `gawk-ios`**, with its own release-please component and its own version. |
| OD6 | **iPhone and iPad, iOS 26 and later.** |
| OD7 | **Playout uses AVFoundation timing**: timestamped sample buffers presented under an `AVSampleBufferRenderSynchronizer` at a small target delay. R12's adaptive controller is not ported. |
| OD8 | **v1 includes** rooms (join and attach), Picture-in-Picture, opt-in telemetry and R37's server picker with per-server secrets. **Mic audio is not in v1.** |
| OD9 | **Shared Rust is used by path, and the build runs from the repo root**, as `gawk-admin` does with `gawk-server`. A semantic change to a shared desktop crate needs a `gawk-ios`-scoped commit in the same PR. |
| OD10 | **iOS CI runs from the first chunk** on `macos-latest`: Rust cross-builds and tests, plus an unsigned `xcodebuild` with simulator tests. |
| OD11 | **The broadcast carries app audio** (ReplayKit `audioApp` → Opus through the shared audio crate). Uplink transport is whatever the shared engine does, R55's carriers included. |
| OD12 | **VP8/VP9 broadcasts play natively through a bundled libvpx**, so every broadcast plays in the app. **IO0 runs first, as a measuring spike.** |

## 3. Alternatives considered and rejected

Recorded so they aren't re-derived. Each was put to the owner on
2026-10-03 or follows from a recorded decision.

| Alternative | Why not |
|---|---|
| **WKWebView around the SPA** for viewing | It inherits every Safari limit this milestone exists to remove (no PiP from a canvas, the R16/R22 fullscreen detour). It's also unverified whether WKWebView exposes WebTransport at all, and an app that is a wrapped website is App Review's "minimum functionality" rejection (guideline 4.2). |
| **Broadcast-only app** (view in Safari) | Leaves PiP and background audio unsolved, which is half the reason to have an app (OD2). |
| **Pure Swift** | Apple ships no WebTransport client, so we'd hand-roll HTTP/3 + WebTransport framing over Network.framework's QUIC. That makes a **fifth wire mirror** and a second implementation of resume, send policy, parity and telemetry the desktop engine already has. |
| **Slint for the UI** | Slint's iOS support is young. The extension has no UI anyway, and PiP, the picker button and the share sheet are UIKit/SwiftUI-only. Sharing `main.slint` with the desktop window (docs/54 D11) buys little on a phone. |
| **iOS as a fourth shell in the desktop workspace** | Its release unit and version would be the desktop's, and the "desktop" name would be wrong. OD5 chose a separate module. |
| **Extract a neutral shared-core workspace first** | The cleanest ownership, but a refactor of the desktop workspace and its CI ahead of any iOS value. Path dependencies (OD9) get the same reuse now. The extraction can follow if a third consumer appears. |
| **Camera broadcasting** (`AVCaptureSession`) | A different product (IRL streaming). Not a game stream (OD1). |
| **In-app ReplayKit** (`RPScreenRecorder`) | Captures only our own app. Useless for streaming a game. |
| **HEVC encode** | Firefox viewers can't decode it and the SPA's codec negotiation has no HEVC rung. H.264 is what every viewer plays. |
| **AVPlayer + LL-HLS** for viewing | Seconds of latency by design. It would need a packager on the relay. |
| **Porting R12's adaptive playout** | OD7. AVFoundation's synchronizer does pacing and A/V sync, and a fixed target with a drop-to-live rule (D15) covers the live edge. R12 is the reference if field data says otherwise. |
| **Safari fallback or an "unsupported" message for VP8/VP9** | OD12. Every broadcast should play in the app. |
| **Universal links in v1** | Need Associated Domains, which needs the paid team and an AASA file served by `gawk-app`'s nginx. Both belong with distribution (§5); v1 uses a `gawk://` scheme (D20). |

## 4. Decisions

### D1 — Layout: `gawk-ios/`, a Rust workspace and an XcodeGen project

```
gawk-ios/
  rust/
    Cargo.toml            # workspace; path deps into ../../gawk-broadcast-desktop/crates
    crates/
      core/               # the UniFFI surface (D3): one cdylib/staticlib, both targets
      broadcast/          # ReplayKit → engine glue: sample intake, rotation, rung (D8–D11)
      viewer/             # subscribe, reassembly, parity repair, decode, timing (D13–D16)
  app/
    project.yml           # XcodeGen (D2)
    Gawk/                 # the SwiftUI app: Watch, Broadcast, Rooms, Settings
    BroadcastUpload/      # the RPBroadcastSampleHandler extension; no UI
    Shared/               # Swift package: App Group store, Keychain, UniFFI bindings
  README.md               # build, run on a device, the IO0 instrument
```

`rust/` path-depends on `gawk-broadcast-desktop/crates/{wire,engine,encode,audio}`.
**There is no fifth wire mirror** (CLAUDE.md: reuse, never mirror): the
desktop `crates/wire` and its golden vectors are the iOS app's too. Its
tests run in iOS CI as well, so a `gawk-server/wire/**` change triggers the
iOS job (D24).

### D2 — The Xcode project is generated, not committed

XcodeGen reads the checked-in `project.yml`. `*.xcodeproj` is
git-ignored. A Run Script build phase calls `cargo build` for the active
SDK and architecture and runs `uniffi-bindgen` into `Shared/`, so Xcode's
Run builds Rust as well. *Open question Q2.*

**Rationale**: `project.pbxproj` merge conflicts are the standard failure of
a hand-maintained Xcode project. A generated one keeps CI and every
checkout identical.

### D3 — One Rust framework, linked by the app and the extension

`crates/core` builds one XCFramework (`GawkCore`, device `aarch64-apple-ios`
plus simulator `aarch64-apple-ios-sim`). It's embedded in the app bundle
once and linked by both targets. UniFFI's surface is callback-shaped:
Swift hands the core sample buffers' raw planes, timestamps and orientation,
and receives status, decoded frames and audio packets through callback
interfaces. Neither side exposes tokio or objc2 types across the boundary.

**Rationale**: one framework halves the bundle size against a static lib in
each target. Its clean pages don't count against the extension's footprint;
dirty pages do, and IO0 measures them.

### D4 — Shared desktop crates gain iOS gating, not iOS code paths

- **`cfg(any(target_os = "macos", target_os = "ios"))`** where the code is
  Apple-generic: `encode/vt.rs` and `vt_policy.rs`, the host-clock `Clock`,
  and the objc2 dependency lines. objc2's framework crates already build for
  iOS.
- **A `self-update` cargo feature, default on, that iOS turns off.** It gates
  `engine::update`, `engine::install`, `ureq`, `minisign-verify`, `flate2`
  and `tar`. App Store apps must not update themselves, and dropping them
  shrinks the extension.
- **Nothing else.** No iOS module in the desktop crates. ReplayKit, App Group
  and UIKit code lives in `gawk-ios`.

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

### D6 — The extension owns the whole broadcast media path

```
RPBroadcastSampleHandler (BroadcastUpload)
  processSampleBuffer(.video)  ─▶ orientation + FpsGate ─▶ VTPixelTransferSession (rotate/scale, D9)
                                                             │ IOSurface CVPixelBuffer, host PTS
                                                             ▼
                                                  VTCompressionSession (encode/vt.rs, D10)
                                                             │ AVCC → Annex-B, SPS/PPS before IDR
                                                             ▼
  processSampleBuffer(.audioApp) ─▶ ASBD-driven shim ─▶ resample 48k ─▶ Framer ─▶ libopus (D11)
                                                             │
                                                             ▼
                                           engine session (send policy, resume, timesync,
                                           parity, R55 carriers, telemetry) ─▶ wtransport ─▶ relay
  processSampleBuffer(.audioMic) ─▶ ignored (OD8)
```

The containing app never touches media. While a game is in front it's
suspended, so it couldn't. The extension is the whole broadcaster above the
engine's seams (`VideoSource`, `AudioSource`, `Clock`, `RelaySession`),
exactly as the macOS shell is above them (docs/54 §5).

### D7 — Extension lifecycle maps onto the engine's states

| ReplayKit | Engine |
|---|---|
| `broadcastStarted(withSetupInfo:)` | Read settings from the App Group (D17). With a stored resume token for the selected server, try `/publish/{id}` with it (R17); otherwise mint. Write the code to the App Group. |
| `broadcastPaused()` (the system paused capture, e.g. a call) | Stop feeding encoders; keep the session and keepalive. Viewers see the broadcaster away, as with desktop **Pause** (docs/64). |
| `broadcastResumed()` | Force an IDR (docs/54 D7's on-demand IDR) and continue. |
| `broadcastFinished()` | Close the session cleanly; clear the live status. Keep the resume token for the grace period so a restart within 5 minutes gets the same code. |
| Close code **4000** / **4004** or a `SessionClosing` (0x17, R57) naming them | `finishBroadcastWithError(_:)` with a user-facing reason. These are terminal (CLAUDE.md); no reconnect. |
| 4001–4003, transport loss | The engine's resume loop, unchanged. |
| Jetsam / crash | Nothing runs. The relay holds the slot through the GC grace; the next start reclaims it with the stored token (G6). |

### D8 — Rung: 1080p60, 500 ms GOP, 8 Mbps peak *(provisional)*

- **Size**: ReplayKit delivers native panel pixels (1179 × 2556 on an
  iPhone 15 Pro, more on an iPad Pro). Fit the upright frame into a
  1920 × 1920 long-edge box with the shared `fit` rule (aspect kept, never
  upscale, even dimensions). A portrait phone streams 886 × 1920; landscape
  1920 × 886.
- **Frame rate**: ReplayKit sends frames when the screen changes, up to the
  display rate; ProMotion panels can deliver 120. An `FpsGate` caps at 60.
  PTS pass through (VFR, docs/54 D7).
- **Bitrate**: 8 Mbps peak / 75 % mean, below the desktop's 12 because
  cellular uplinks are the common case. A **Cellular** preset (720p30,
  3 Mbps) is offered, and selected automatically on an expensive
  `NWPath` (D19).
- **GOP**: 500 ms, forced by the app (docs/54 D7: VT low-latency mode is
  infinite-GOP after the IDR).

**Provisional on IO0** (memory) and IO8 (thermals).

### D9 — Rotation is a resolution change

ReplayKit always delivers the buffer in the panel's native orientation, with
the device orientation in the `RPVideoSampleOrientationKey` attachment.
The wire has no rotation field and viewers trust the frame in hand
(CLAUDE.md), so the extension rotates frames upright in
`VTPixelTransferSession` (with D8's scale, one pass, one pool). On an
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
the iOS 26 floor has a hardware H.264 encoder, so the refusal path is
unreachable in practice but kept, with its message pointing at a desktop
broadcaster. **Backpressure** is docs/54 D10's `MAX_IN_FLIGHT` gate: at the
limit the incoming ReplayKit buffer is dropped and counted (favor dropped
frames), and its `CVPixelBuffer` is released at once so ReplayKit's pool
never starves.

### D11 — App audio: R25's Opus contract, format read from the buffer

- **Format** is read from every buffer's `AudioStreamBasicDescription`,
  never assumed. ReplayKit's app audio has been reported as 44.1 kHz and as
  big-endian 16-bit on some versions (V-3). A shim converts to interleaved
  `f32`, resamples to 48 kHz (a fixed-ratio polyphase resampler in
  `crates/broadcast`, allocation-free on the hot path), and feeds the shared
  `Framer`.
- **The encoder side** is the shared `audio` crate, untouched: libopus
  48 kHz stereo, 128 kbps constant, 20 ms frames, DTX/FEC off,
  `RESTRICTED_LOWDELAY`.
- **One clock** (docs/54 D5): ReplayKit stamps video and audio buffers with
  host-clock PTS, so the engine's host `Clock` (`mach_absolute_time`, D4)
  serves both and A/V skew is zero by construction. V-4 verifies the
  stamps are host time and not a media clock.
- **Audio never fails a broadcast** (R25 Decision 6): any audio failure
  drops audio, says so in the app's live status and leaves video running.

### D12 — Transport inside the extension: the engine's, one runtime thread *(provisional)*

The engine runs on a **current-thread** tokio runtime on one dedicated
thread, not the desktop's multi-thread runtime: worker threads' stacks and
per-thread allocator arenas cost memory the extension doesn't have. Uplink
transport is whatever the engine negotiates (OD11): datagrams, plus R55's
per-GOP reliable carriers when the relay advertises `CapUplinkCarriers`, and
R29 parity when `RelayCapabilities` asks for it. **QUIC connection
migration is disabled.** The relay sits behind a UDP load balancer whose
kube-proxy conntrack keys on the 5-tuple, so a migrated path can land on
another pod (docs/22). On a path change (D19) the engine reconnects with its
resume token instead.

**Provisional on IO0**: if a current-thread runtime still doesn't fit,
§9.1's fail branch applies.

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
| `ReliableCarrier` (0x0A), `StripeState` (0x10) | **Not in v1.** The viewer never requests `delivery=reliable` or stripes, so the relay never sends them (both are opt-in by dial parameter). |

**Delta loss rule**: a delta frame that can't be completed or repaired by
its deadline is dropped, and every delta after it is skipped **until the
next keyframe** (≤ 500 ms with the forced GOP). Decoding a broken reference
chain shows corruption; skipping shows a short freeze. Favor dropped frames
over corrupted ones.

### D14 — Parity repair moves into the shared `wire` crate

The Rust `wire` crate computes R29's RAID-6 P/Q symbols (`parity.rs`) but
can't **recover** a chunk; recovery exists only in `gawk-app`'s TS. IO4
adds `recover_parity` to `crates/wire`, with recovery vectors restated from
the TS tests (the house rule for mirrors: vectors restated, never
imported). It's a desktop-crate change under OD9, and the desktop apps
gain nothing from it but a tested function they don't call.

### D15 — Playout: an `AVSampleBufferRenderSynchronizer` at a fixed target

- **Video**: H.264 is enqueued **compressed** to an
  `AVSampleBufferDisplayLayer` (via an `AVSampleBufferVideoRenderer`),
  which decodes in hardware itself. VP8 and VP9 are decoded by libvpx in
  `crates/viewer` to `CVPixelBuffer`s from an IOSurface pool and enqueued
  decoded. If IO0's probe finds VideoToolbox decodes VP9 on iOS 26, VP9 takes
  the compressed path and libvpx covers VP8 only.
- **Audio**: decoded PCM is enqueued to an `AVSampleBufferAudioRenderer`
  under the same `AVSampleBufferRenderSynchronizer`, which owns A/V sync.
  With no audio track the synchronizer runs on the host clock alone.
- **Timebase**: frames are stamped in the broadcaster's capture clock.
  The synchronizer's time is anchored so the newest received PTS plays
  **150 ms** after arrival (the target delay). It covers the jitter of
  one reassembly deadline plus a decode, and is a constant, not a
  controller (OD7).
- **Drop to live**: if the newest received PTS runs more than **2 × target**
  ahead of what's presented (after a stall, a background trip or an
  outage), flush both renderers, wait for the next keyframe and re-anchor.
  This is R5's live-edge rule in its simplest form, and what G10 tests. It
  never slows playback to catch up; it jumps.

### D16 — Audio decode: libopus in Rust

The `opus` crate (already in the tree for encode) decodes to 48 kHz
stereo `f32`, which Swift wraps as LPCM `CMSampleBuffer`s with the
packet's PTS. Apple's AudioToolbox can also decode Opus, but keeping decode
in Rust means CI tests it on the host, where an Apple-only decoder couldn't
be tested.

### D17 — App ↔ extension: an App Group and a shared Keychain group

| What | Where | Written by |
|---|---|---|
| Selected server, room to attach, quality preset, telemetry opt-in | App Group `UserDefaults` suite | app |
| Per-server secrets (R37, docs/40), R17 resume tokens | Keychain access group, `kSecAttrAccessibleAfterFirstUnlock` | app (secrets), extension (tokens) |
| Live status: code, viewers, state, last error | App Group `UserDefaults`, plus a Darwin notification on change | extension |

The app observes the Darwin notification and re-reads. Nothing passes
media, and the extension never waits on the app. **App Groups and Keychain
sharing need a paid team** (Q1).

### D18 — Starting and stopping a broadcast

The Broadcast screen hosts an `RPSystemBroadcastPickerView` with
`preferredExtension` set to ours and `showsMicrophoneButton = false` (OD8).
Tapping it opens the system sheet. **Start Broadcast** runs a three-second
countdown and the extension starts. The code appears in the app as soon as
the extension writes it, with **Copy link**, **Copy code** and the share
sheet. Stopping is the system's red status pill, Control Center, or the
same picker button. The app can't stop the extension directly: ReplayKit has
no API for it.

ReplayKit captures **everything on screen**, notifications included.
Before the first broadcast the Broadcast screen says so, with a pointer to
Focus modes. Protected (DRM) content is black in the capture, as the system
dictates.

### D19 — Network paths

An `NWPathMonitor` in each process tells the core when the path changes
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

### D21 — Rooms

- **Broadcaster**: the room to attach is chosen in the app before Start
  (saved and recent rooms, as docs/60 has). The extension attaches with the
  shared `engine::room` code (R42), unchanged.
- **Viewer**: a room view in **grid** and **focus**, the SPA's two layouts.
  Each participant is one subscribe session and one renderer. At most
  **four tiles play at once**: in a larger room the rest show their last
  frame, with a tap to swap. Focus plays one tile at full rate and pauses
  the others' decode (the sessions stay up). *Provisional on IO8 thermals.*

### D22 — PiP and background audio

`AVPictureInPictureController` with the
`ContentSource(sampleBufferDisplayLayer:playbackDelegate:)` source. The
audio session is `.playback`, with background mode `audio`, so PiP and a
locked screen keep audio. The playback delegate reports a live stream
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

### D24 — CI: `ios.yml` on `macos-latest`

- **Triggers**: `gawk-ios/**`, `gawk-broadcast-desktop/crates/{wire,engine,encode,audio}/**`,
  `gawk-server/wire/**`. The desktop workflow gains `gawk-ios/rust/**`, so a
  shared-crate change runs both sides.
- **Rust**: `cargo test` on the macOS host (wire vectors, viewer reassembly,
  parity recovery, the resampler, the drop-to-live rule); `cargo build` for
  `aarch64-apple-ios` and `aarch64-apple-ios-sim`; an integration test
  that runs the viewer core against a real `gawk-server` binary fed by
  `gawk-pubsim`, including a relay kill (G6).
- **Xcode**: `xcodegen`, then `xcodebuild build-for-testing` unsigned
  (`CODE_SIGNING_ALLOWED=NO`) and `test` on an iOS 26 simulator: Swift unit
  tests and a Watch-screen smoke test against the same local relay.
  ReplayKit doesn't run in the simulator, so the extension is built and
  linked in CI and only exercised on a device.
- Signing, archives and TestFlight upload belong to §5's milestone.

### D25 — Versioning and conventions

- A `gawk-ios` release-please component (tag `gawk-ios/vX.Y.Z`), starting at
  0.1.0. It publishes no artifact in R65 (OD3). Its version is the app's
  `CFBundleShortVersionString`.
- Commit scope `ios` in `CONTRIBUTING.md`'s table, with OD9's coupling rule
  written next to `gawk-admin`'s.
- When IO1 lands, CLAUDE.md's repository layout gains a `gawk-ios` entry
  for the facts a reader can't derive: the path dependency, the coupling
  rule, and that the extension owns the media path.

## 5. Non-goals

- **TestFlight and App Store distribution.** A follow-up milestone covers
  signing in CI, App Store Connect upload, the privacy manifest and labels,
  user-generated-content report and block (guideline 1.2, with R39/R40),
  universal links with an AASA file served by `gawk-app`, and how the
  compiled-in default relay is presented in review.
- Mic audio and commentary (OD8); camera broadcasting (OD1).
- R19 reliable delivery, R21 DVR, R30 striping in the viewer (D13). They're
  opt-in by dial parameter and can follow.
- Porting R12's adaptive playout (OD7).
- Any change to the Safari viewer, R16/R22 included.
- Android; a native viewer on desktop.

## 6. What deliberately does not exist

Inherited from docs/38 §7 and docs/54 §7: no software encode; no
viewer→server keyframe back-channel; no auto-resume through 4000/4004; no
second clock; no standalone `DecoderConfig` datagrams; no Opus
DTX/FEC/bitrate knob. Added here: no media in the containing app (D6); no
QUIC connection migration (D12); no self-update (D4); no raw broadcast IDs
in logs or telemetry (CLAUDE.md, R9 D3).

## 7. UX flows

**First broadcast (G1)**: open the app → Broadcast → the notification
note (D18) → the picker button → Start Broadcast → countdown → the code
and **Copy link** in the app → switch to the game. Zero settings touched.

**Watch by link**: tap a `gawk://watch/<CODE>` link → the app opens
straight into the player → rotate for fullscreen → swipe home and PiP
continues (G9).

**Rooms**: Broadcast → room field (saved/recent) → Start; viewers who open
the room on the web see the stream join. In the app, Rooms → a room code →
grid; tap a tile for focus.

**Resume**: the extension is killed or the relay pod restarts → the
broadcast resumes on the same code with an IDR (G6); after a jetsam, the
user restarts from the picker within the grace and gets the same code.

**Errors**: 4000/4004 end the broadcast with the system's alert carrying
our reason (D7). A full fleet or a refused secret says which, in the app.

## 8. Risks

| Risk | Mitigation |
|---|---|
| The extension's ~50 MB memory limit (jetsam kills it without warning) with the engine, quinn, VideoToolbox, the rotation pool and Opus | IO0 measures first; D12's current-thread runtime; D8's cap; §9.1's branches |
| Thermal throttling with a game, encode and upload together | IO0/IO8 log `ProcessInfo.thermalState`; step down a rung on `.serious`, announced in the live status |
| ReplayKit's audio format differs from what's reported | D11 reads the ASBD per buffer; V-3 records what devices deliver |
| libvpx software decode drains the battery on long watches | Only VP8/VP9 sources (Firefox broadcasters, the minority) take it; IO8 records the cost |
| The viewer core drifts from the SPA's wire behaviour | It's tested on recorded streams and against the real relay in CI (D24), and shares `crates/wire`'s parsers |
| quic-go / webtransport-go bumps have broken WebKit before (gotchas) | Not exposed: the iOS app speaks the desktop's Rust stack, not WebKit's |
| App Review later | Out of scope here, written into §5 so the follow-up starts from it |

## 9. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **IO0** | **Spike (throwaway branch, never merged).** A minimal app and extension linking the engine and `vt.rs` with D4's gating hacked in; broadcast at 1080p60 with app audio to the fleet. Log peak `phys_footprint` every second, thermal state and dropped frames; photograph glass-to-glass. Probe `VTIsHardwareDecodeSupported(kCMVideoCodecType_VP9)` and record the `audioApp` ASBD (V-3) and buffer clocks (V-4). | §9.1's verdict and the V-items are recorded in §12 |
| **IO1** | Scaffolding: D1–D4, D5's identity and origin, D24's CI, D25's component and conventions | CI is green on a PR that touches only a desktop crate; the desktop job's `cargo test` is unchanged with `self-update` on; the iOS host tests pass with it off; the simulator builds both targets |
| **IO2** | The broadcast extension: D6–D12, D19 | G1 (without the app UI: settings seeded by hand), G2, G3, G4, G5 on a device; G6's relay-kill integration test green in CI |
| **IO3** | The broadcaster UI: D17, D18, the server picker and secrets (D23), room attach (D21) | The code shows within 2 s of the extension's first frame; a non-default server with a secret works; an attach shows in the room's web view (G11's first half) |
| **IO4** | The viewer core: D13, D14, D16; Rust tests on recorded H.264, VP9 and VP8 streams; the CI integration test against the relay | Tests green in CI; a `DecoderConfig` change mid-stream resets the decoder with no crash; parity recovery passes the restated vectors; the delta-loss rule is tested |
| **IO5** | The native player: D15, D22 | G7, G8, G9, G10 on a device |
| **IO6** | Joining and rooms in the viewer: D20, D21 | A `gawk://` link opens the right broadcast; `relay=` shows the strip; G11's second half |
| **IO7** | Telemetry and metrics: D23's telemetry, D5's `app=ios` in `gawk-server`, the `gawk-ios` kind in `gawk-telemetry` | A test session appears in the telemetry dashboard; relay metrics label it `app="ios"` |
| **IO8** | The owner's device pass: iPhone and iPad, broadcaster and viewer, H.264/VP9/VP8 sources, battery and thermal notes, D21's tile cap | G1–G12 recorded in §12 |

### 9.1 IO0 pre-registered verdict

On the owner's iPhone, broadcasting a 3D game at D8's rung for 30 minutes:

- **Pass**: peak extension footprint ≤ 40 MB (10 MB under the limit), no
  jetsam, glass-to-glass ≤ 250 ms to a desktop Chrome viewer, thermal state
  never `.critical`. D8 and D12 are confirmed as written.
- **Conditional**: peak 40–48 MB, or `.critical` once. Drop the cap to 1280
  on the long edge and the encoder pool to `MAX_IN_FLIGHT = 2`, measure
  again, and amend D8 before IO2.
- **Fail**: every other first-run outcome, which includes a peak above
  48 MB, any jetsam, glass-to-glass over 250 ms, or `.critical` more than
  once. A latency or thermal miss alone means: re-measure at 1280 as for
  Conditional, and record which one missed. A memory fail, or **any** miss
  on the 1280 re-measure, means stop: redesign the extension's transport (a
  minimal QUIC client without tokio, or the engine split so only the send
  path runs in the extension) before IO1.

The three buckets are exhaustive: anything that isn't Pass or Conditional
is Fail. The Conditional re-measure is judged by Pass's criteria at the
1280 cap; passing it amends D8, and anything else is a Fail.

## 10. V-items (verified on device, recorded in §12)

| # | Question | Decides |
|---|---|---|
| V-1 | Peak extension footprint at 1080p60 and 720p30, multi-thread vs current-thread runtime | D8, D12, §9.1 |
| V-2 | Does VideoToolbox decode VP9 on iOS 26 (and AV1, for the record)? | D15 |
| V-3 | `audioApp` ASBD: sample rate, endianness, interleaving, across devices | D11 |
| V-4 | Are ReplayKit video and audio PTS on the host clock? | D11, G4 |
| V-5 | Does a locked screen stop or pause the broadcast? Is `broadcastPaused` delivered? | D7 |
| V-6 | Frame rate ReplayKit delivers on a 120 Hz panel in a 60 fps game | D8 |
| V-7 | Does `RPVideoSampleOrientationKey` track the device or the interface orientation in a landscape-locked game? | D9 |
| V-8 | Footprint and thermal cost of four playing tiles on iPhone vs iPad | D21 |
| V-9 | Does PiP from `AVSampleBufferDisplayLayer` survive a renderer flush (drop to live)? | D15, D22 |
| V-10 | Does the relay check `-allowed-origins` on `/subscribe` too? | D5 |

## 11. Open questions

| # | Question |
|---|---|
| Q1 | Is the paid Apple Developer Program membership (the one R52 MB7's notarization needs) the team that signs iOS builds? App Groups and Keychain sharing need it, and free personal teams expire builds after 7 days. |
| Q2 | XcodeGen (D2), or a committed `.xcodeproj`, or Tuist? |
| Q3 | Bundle ID prefix and the app's display name (e.g. `fi.ioio.gawk`, "gawk"). |
| Q4 | Which devices are available for IO0 and IO8 (iPhone model, iPad model)? D8's rung and D21's tile cap depend on them. |

## 12. Deviations and field findings

None yet. IO0's verdict, the V-items and every deviation from §4 are
recorded here, dated.
