# R65 — iOS app: native broadcaster and viewer (docs/67)

**Status**: proposed 2026-10-03. Owner decisions OD1–OD12 (§2) were taken
the same day in an interview. Chunks **IO0–IO8** (§6) are not started.
**IO0 is a throwaway spike whose pre-registered verdict (§6.1) gates every
later chunk.** Decisions marked *provisional* in §3 are confirmed or
revised once IO0 is done. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: iPhone viewing in Safari already works.
WebKit runs the whole WebTransport + WebCodecs worker pipeline, and R16/R22
([docs/21](21-ios-video-fullscreen.md), [docs/27](27-ios-mse-fullscreen.md))
work around the missing Element Fullscreen. R65 does **not** replace that
path, and the SPA is unchanged. The broadcaster reuses the Rust engine,
wire, encode and audio crates from
[`gawk-broadcast-desktop`](../gawk-broadcast-desktop) (R34, R52, R56) **by
path**. iOS becomes a new top-level module, not a fourth desktop shell.

---

## 1. Why

- **There is no way to broadcast from an iPhone or iPad today.** iOS Safari
  has no `getDisplayMedia`, and every iOS browser is WebKit. Screen capture
  outside your own app exists only through a ReplayKit **Broadcast Upload
  Extension**, so only a native app can broadcast.
- **Watching in Safari is second-class.** iPhone fullscreen goes through a
  fullscreen-only MSE surface whose on-device pass is still open (R22 MF5).
  There is no Picture-in-Picture, so watching while playing a game yourself
  isn't possible. Audio stops when Safari goes to the background.
- A native player on `AVSampleBufferDisplayLayer` gets real fullscreen,
  rotation, PiP and background audio from the platform.

## 2. Owner decisions (2026-10-03)

| # | Decision |
|---|---|
| OD1 | **Broadcast the device screen through ReplayKit** (a Broadcast Upload Extension). Camera streaming is out of scope. |
| OD2 | **A native player** (VideoToolbox / libvpx → `AVSampleBufferDisplayLayer`), not a WKWebView around the SPA, and not a broadcast-only app. |
| OD3 | **Signed for the owner's own devices first.** TestFlight and then the public App Store will follow, but in a later milestone (§7). |
| OD4 | **SwiftUI over a Rust core**, bridged with UniFFI. |
| OD5 | **A new top-level module, `gawk-ios`**, with its own release-please component and its own version. |
| OD6 | **iPhone and iPad, iOS 26 and later.** |
| OD7 | **Playout uses AVFoundation timing.** Buffers are timestamped, presented against a `controlTimebase` / `AVSampleBufferRenderSynchronizer`, and held at a small target delay. R12's adaptive controller is not ported. |
| OD8 | **v1 includes** rooms (join and attach), Picture-in-Picture, opt-in telemetry and R37's server picker with per-server secrets. Mic audio is **not** in v1. |
| OD9 | **Shared Rust is used by path, and the build runs from the repo root**, as `gawk-admin` does with `gawk-server`. A semantic change to a shared desktop crate needs a `gawk-ios`-scoped commit in the same PR. |
| OD10 | **iOS CI runs from the first chunk** on `macos-latest`: Rust cross-builds and tests, plus an unsigned `xcodebuild` with simulator tests. |
| OD11 | **The broadcast carries app audio** (ReplayKit `audioApp` → Opus through the shared audio crate). Uplink transport is whatever the shared engine does, R55's carriers included. |
| OD12 | **VP8/VP9 broadcasts play natively through a bundled libvpx**, so every broadcast plays in the app. **IO0 runs first, as a measuring spike.** |

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Layout.** `gawk-ios/` holds `rust/`, a Cargo workspace with `crates/core` (the UniFFI surface), `crates/viewer` and `crates/broadcast`; `app/` with the SwiftUI app, the `BroadcastUpload` extension and the shared Swift package; and `deploy/`, which is empty. `rust/` path-depends on `../../gawk-broadcast-desktop/crates/{wire,engine,encode,audio}`. | OD5, OD9. Path dependencies mean one copy of the wire mirror, not a fifth. |
| D2 | **No new wire mirror.** `crates/wire` is reused as is. Its golden-vector tests run in iOS CI as well, so a wire change on `gawk-server/wire/**` triggers the iOS job (§5). | CLAUDE.md: reuse, never mirror. |
| D3 | **Shared crates get `cfg(any(target_os = "macos", target_os = "ios"))` where the code is Apple-generic.** VideoToolbox encode (`encode/vt.rs`) and the objc2 bindings qualify. The self-update path (`engine::install`, `update`, `ureq` + `minisign`) is behind a default-on `self-update` feature that iOS turns off, because App Store apps must not update themselves. | A desktop-crate change, so OD9's coupling rule applies to these commits. |
| D4 | **Distribution identity**: `gawk-ios`, injected with `engine::set_this` (`docs/58` D2), with its own telemetry `kind`. Dials send `app=ios&os=ios`, and the relay's R59 `app` vocabulary gains `ios` in a `gawk-server` commit ([docs/61](61-usage-and-capacity-metrics.md) D1: a new value costs a relay release). | Keeps R59 dashboards honest. Until the relay gains `ios` it degrades to `other`. |
| D5 | **The broadcast extension owns the whole media path**: ReplayKit `CMSampleBuffer` → rotate/scale (`VTPixelTransferSession`) → VideoToolbox H.264 realtime with a 500 ms GOP forced by the app (as on macOS, docs/54) → engine session. The containing app never touches media. It is suspended while a game is in front, so it can't. | ReplayKit's model. *Provisional on IO0* (memory). |
| D6 | **App ↔ extension through an App Group**: settings (server, room, quality) in a shared `UserDefaults` suite; per-server secrets and R17 resume tokens in a shared Keychain access group; live status (code, viewers, errors) written by the extension and observed by the app through Darwin notifications. The broadcast starts from `RPSystemBroadcastPickerView` with `preferredExtension` set. | The extension has no UI. The code and the share link have to show in the app. |
| D7 | **Rotation is a resolution change.** On a change in `RPVideoSampleOrientationKey`, the extension rotates frames upright and re-configures the encoder, sending a new decoder config and a keyframe, the same path as a desktop quality change while live (docs/64). It is debounced (500 ms) so an iPad that flips back and forth doesn't flood keyframes. | Our wire has no rotation metadata, and viewers trust the frame in hand (CLAUDE.md). |
| D8 | **Encode size**: the long edge is capped at 1920 and the short edge rounded to 16. ReplayKit delivers native panel pixels (e.g. 1179 × 2556), which are too big for the extension's memory and the uplink. Frame rate follows ReplayKit, which sends frames only when the screen changes. *Cap provisional on IO0.* | Memory cap; the R13 quality presets map onto it. |
| D9 | **The viewer core is new Rust in `gawk-ios/rust/crates/viewer`**: subscribe dial, chunk reassembly, the keyframe uni-stream, decoder-config parsing, Opus decode (libopus through the `opus` crate already in the tree), VP8/VP9 decode (libvpx, OD12), and timing. It hands Swift `CVPixelBuffer`-ready frames, or H.264 access units for VideoToolbox, through UniFFI callbacks. | No Rust viewer exists yet; the engine is broadcaster-only. Kept in `gawk-ios` (not the desktop workspace) because only iOS needs it today. A future native desktop viewer would lift it then. |
| D10 | **Playout (OD7)**: H.264 goes to VideoToolbox (`VTDecompressionSession`), VP8/VP9 to libvpx, and both are enqueued to an `AVSampleBufferDisplayLayer`. Audio goes to an `AVSampleBufferAudioRenderer`, both under one `AVSampleBufferRenderSynchronizer`. Target delay is 150 ms behind the newest received PTS, re-anchored when the lag goes beyond 2× the target (drop to live, as R5 does). | Lets the platform handle A/V sync and pacing. The re-anchor rule keeps the latency from building up when AVFoundation would rather buffer. |
| D11 | **PiP and background audio**: `AVPictureInPictureController` with the `sampleBufferDisplayLayer` content source, audio session category `.playback`, and background mode `audio`. | OD8. |
| D12 | **Join by code, by link, and in rooms.** The app takes a typed code, a `gawk://` custom-scheme link, and an R42 room view (grid / focus) of participants' streams. Universal links for `gawk.ioio.fi` (AASA served by `gawk-app`'s nginx) wait for the distribution milestone (§7). | Free personal signing can't use Associated Domains, and a cross-component change belongs with the App Store work. |
| D13 | **The Xcode project is generated with XcodeGen** from a checked-in `project.yml`. The `.xcodeproj` is not committed. | No `project.pbxproj` merge conflicts; CI and every contributor build from the same source. *Open question Q2.* |

## 4. Risks

| Risk | Mitigation |
|---|---|
| The extension's ~50 MB memory limit (jetsam kills it without warning) with tokio, quinn, VideoToolbox, Opus and the rotation buffer | IO0 measures it; fall back to a current-thread runtime, a smaller resolution cap and fewer in-flight buffers |
| Thermal throttling with a game, encode and upload all running | IO0 records `ProcessInfo.thermalState` over 30 minutes; quality steps down on `.serious` |
| libvpx software decode drains the battery on long watches | Measured in IO8. It's only used for VP8/VP9 broadcasts, which come from Firefox broadcasters, the minority |
| quic-go/webtransport-go bumps have broken WebKit before (`docs/gotchas.md`) | iOS doesn't use WebKit's stack. The Rust client is the desktop one, already exercised against the fleet |
| App Review later (UGC guideline 1.2: report/block) | Out of scope here, written down for §7 |

## 5. CI

A new `ios.yml` runs on `macos-latest` and triggers on `gawk-ios/**`,
`gawk-broadcast-desktop/crates/{wire,engine,encode,audio}/**` and
`gawk-server/wire/**`. Steps: `cargo test` for the host target; `cargo
build` for `aarch64-apple-ios` and `aarch64-apple-ios-sim`; `xcodegen`;
`xcodebuild build-for-testing` unsigned, then `test` on an iOS 26 simulator
(the Swift unit tests and a viewer smoke test against a local relay plus
`gawk-pubsim`). ReplayKit doesn't run in the simulator, so the extension is
built in CI and only exercised on a device (IO8). The desktop workflow's
path filters also gain `gawk-ios/rust/**`, so a shared-crate change sees
both sides. Signing, archives and TestFlight upload are §7.

## 6. Chunks and acceptance criteria

| Chunk | Scope | Accepted when |
|---|---|---|
| **IO0** | **Spike (throwaway branch, never merged)**: a minimal app and extension that link the shared engine and VT encode, broadcast the screen at 1080p60 with app audio to the fleet, and log peak `phys_footprint`, thermal state and glass-to-glass latency. Also probes whether `VTIsHardwareDecodeSupported` reports VP9 on iOS 26. | The §6.1 verdict is recorded in this doc |
| **IO1** | Scaffolding: `gawk-ios/` layout (D1), UniFFI, XcodeGen, `ios.yml` (§5), the release-please component, D3's cfg/feature gating in the desktop crates, D4's identity | CI is green on a PR that touches only a desktop crate; `cargo test` in `gawk-broadcast-desktop` is unchanged; host-target tests pass with `self-update` off |
| **IO2** | The broadcast extension: D5, D7, D8, app audio (OD11), R17 resume across extension restarts, R57 `SessionClosing` handling | On a device, a 30-minute broadcast during a game has peak memory under the IO0 budget with no jetsam; rotating produces one new config and keyframe each time and the viewers keep playing; killing the extension and starting again within the grace period gives the same code |
| **IO3** | The broadcaster UI: picker button, code and link with a share sheet, live status, server picker with per-server secrets (R37, docs/40), room attach (R42), D6's App Group plumbing | The code shows within 2 s of Start; a non-default server with a secret works; attaching to a room shows the stream in the room's web view |
| **IO4** | Viewer core (D9): subscribe, reassembly, keyframe stream, config, Opus and libvpx decode, timing; Rust tests against recorded vectors and `gawk-pubsim` | Rust tests pass in CI for H.264, VP9 and VP8 inputs; a broadcast's config change (D7) yields a decoder reset without a crash |
| **IO5** | The native player (D10, D11): fullscreen, rotation, PiP, background audio, the drop-to-live re-anchor | On a device: glass-to-glass within 100 ms of Safari's on the same H.264 broadcast; PiP keeps going over another app for 10 minutes; a 5 s network outage recovers to live without permanent added delay |
| **IO6** | Join and rooms: typed code, `gawk://` links (D12), a room grid/focus view | A `gawk://` link opens the right broadcast; a room of three plays in grid and focus |
| **IO7** | Telemetry and metrics: opt-in R31 sessions with the `gawk-ios` kind; R59's `app=ios` in `gawk-server`; `gawk-telemetry` accepts the kind | A test session shows up in the telemetry dashboard; the relay's metrics label it `app="ios"` |
| **IO8** | The owner's on-device pass: iPhone and iPad; broadcaster and viewer; H.264 and VP9 sources; battery and thermal notes | Recorded in this doc's §8 |

### 6.1 IO0 pre-registered verdict

Measured on the owner's iPhone, broadcasting a 3D game at 1080p60 for 30 minutes:

- **Pass**: peak extension footprint ≤ 40 MB (10 MB under the limit), no
  jetsam, glass-to-glass ≤ 250 ms to a desktop Chrome viewer, thermal state
  never `.critical`. D5 and D8 are confirmed as written.
- **Conditional**: peak 40–48 MB. Lower D8's cap to 1280 on the long edge,
  move to a current-thread tokio runtime, and measure again before IO2.
- **Fail**: jetsam at 720p with a current-thread runtime. Stop and redesign
  the transport inside the extension (a minimal QUIC client without tokio)
  before IO1.

## 7. Non-goals (this milestone)

- **TestFlight and App Store distribution.** A follow-up milestone covers
  signing in CI, App Store Connect upload, privacy labels and manifest, UGC
  report/block (guideline 1.2, tied to R39/R40), universal links with an
  AASA file from `gawk-app`, and how the compiled-in default relay is
  presented in review.
- Mic audio and commentary (OD8); camera broadcasting (OD1).
- Porting R12's adaptive playout (OD7).
- Changing the Safari viewer, R16/R22 included.
- Android.

## 8. Open questions

| # | Question |
|---|---|
| Q1 | Is the Apple Developer Program membership (paid, as R52 MB7's notarization needs) the one that signs the iOS builds? App Groups and the extension's Keychain sharing need it; free personal teams also expire builds after 7 days. |
| Q2 | XcodeGen (D13) or a committed `.xcodeproj` / Tuist? |
| Q3 | Bundle ID prefix and the app's display name (e.g. `fi.ioio.gawk`, "gawk"). |
| Q4 | Which devices are available for IO0/IO8 (iPhone model, iPad model)? D8's cap and the thermal criteria depend on them. |
