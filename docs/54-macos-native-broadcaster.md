# R52 — Native macOS broadcaster

**Status**: designed 2026-09-18. Chunks **MB0–MB8**; **MB0 implemented
2026-09-22**, **MB1 implemented and owner-verified 2026-09-23**; **MB2
(capture) implemented 2026-09-23** — its unit criteria are green, its
manual ones (the picker, V-1's no-prompt check, frames/cursor, occluded,
minimized, V-2) await the owner's pass on a Mac; **MB3 (encode — the first
real broadcast) implemented 2026-09-23**, its hardware trial and
encode-to-relay criteria verified on an M1, its first on-screen broadcast
the owner's; **MB4 (audio) implemented 2026-09-23**, its unit criteria
green, G1/G2/V-4 the owner's; **MB5 (the macOS shell) implemented
2026-09-23**, its on-screen criteria and V-8 the owner's; **MB6** —
telemetry is in (the shared reporter, `kind: gawk-broadcast-macos`); the
update notice and in-place install are **blocked on R45/R47, which are not
started**; **MB7 (packaging, signing, release path, docs) implemented
2026-09-23**, its signed run waiting on the Apple secrets. MB8 is the
owner's on-hardware pass. §11 records what each chunk turned up. The
ROADMAP entry ([R52](../ROADMAP.md#r52--native-macos-broadcaster)) carries
the research summary and owner decisions this doc builds on; the decisions
are restated in §2 so the doc reads on its own.

**Relationship to the other broadcasters**: this is the third native
broadcaster. It shares its *shape* with the Windows app (R34,
[docs/38](38-windows-native-broadcaster.md)) and, unlike the Linux app
(R14, [docs/19](19-linux-native-broadcaster.md)), shares its **code**: the
Windows Rust workspace becomes a desktop workspace with two binaries.
Wherever this doc says "inherited", the mechanism is docs/38's, verbatim,
and is not re-argued here.

---

## 1. Why (recap) and what "done" means

The driver is R34's, not R14's. WebCodecs hardware-encodes on macOS, so the
browser broadcaster is fine for *encode*; what it cannot do is capture
fidelity:

- `getDisplayMedia` on macOS gives whole-screen or one window with **no
  per-application audio** — Chrome's system-audio share is whole-system at
  best, and only on a screen share.
- The browser's picker means alt-tabbing out of a fullscreen game.
- ScreenCaptureKit scopes video **and audio** to one application, and
  VideoToolbox has a dedicated low-latency hardware encoder mode. No
  browser surfaces either.

### Milestone acceptance criteria

Pre-registered up front. "Manual" means on a real Apple Silicon Mac
running macOS 14+; CI structurally cannot see a hardware encoder, a
compositor or an interactive desktop, so the manual criteria are what
decide whether R52 works — exactly as docs/19 and docs/38 warn.

| # | Goal | Verified by |
|---|---|---|
| G1 | Mode 1: sharing one app streams that window's video and **only** that app's audio (a second app playing audio on the same Mac is inaudible to viewers) | manual |
| G2 | Mode 2: sharing a display streams the desktop and whole-system audio, surviving an output-device switch mid-broadcast (or failing audio-only) | manual |
| G3 | Hardware encode only: with no hardware H.264 encoder the app refuses with the curated message and points at the browser broadcaster; it never software-encodes | unit (faked session creation) + manual on a VM |
| G4 | The relay and viewer are untouched: a stock viewer at `gawk.ioio.fi` plays the stream with zero diffs outside the desktop workspace, docs, site and the relay origin-allowlist value | integration vs. the real `gawk-server` binary + review |
| G5 | Wire parity: inherited — the shared `wire` crate's golden vectors are the Windows app's, byte-identical with `gawk-server/wire`, `wire.ts` and `gawk-broadcast/internal/wirecheck` | unit test (existing) |
| G6 | Glass-to-glass latency sub-500 ms on the hardware path, measured with R14 V4's photographed-reference method | manual, photographed reference |
| G7 | A/V sync: viewer-reported median `\|avSkewMs\| ≤ 60 ms`, p95 `≤ 120 ms` over 60 s (R25's criteria) | manual, viewer diagnostics |
| G8 | Zero-config first run: open the app → "Choose what to share" → pick → Start reaches the production fleet with no settings touched and **no Screen Recording prompt** (the system-picker path) | manual |
| G9 | The broadcast survives a relay pod restart or rolling drain with an automatic resume (≤ a few seconds of frozen video, no viewer reconnect) | integration (kill/restart relay) + manual during a fleet rollout |
| G10 | Signed and notarized: a fresh macOS 14 user account downloads the release zip, opens the app, and Gatekeeper opens it without a "damaged"/"unidentified developer" dialog; the Screen Recording grant (if one is ever needed) **survives the next release** | manual, two consecutive releases |
| G11 | The Windows binary is unaffected by MB0: the EXE built from the renamed workspace behaves identically on the gaming PC and the Windows CI job still triggers only on its paths | CI + manual smoke |

## 2. Owner decisions (taken 2026-09-18)

| # | Decision | Choice |
|---|---|---|
| OD1 | Code shape | **One shared Cargo workspace, two binaries.** `gawk-broadcast-windows/` → **`gawk-broadcast-desktop/`**. One workspace version, one release-please component, one changelog; a Windows-only fix bumps the macOS binary too, accepted. |
| OD2 | Rename sequencing | **MB0 is a pure rename, its own PR, merged before any macOS code exists.** |
| OD3 | Signing and distribution | **Developer ID + notarization** from the first release artifact. Everything Apple-account-related is a repository secret (§4 D13); nothing identifying lives in the tree. |
| OD4 | CI host | **GitHub-hosted `macos-latest` (Apple Silicon), path-filtered** — the one exception to docs/38 D18. The Windows job keeps cross-compiling on the self-hosted Linux runners. |
| OD5 | Floor and architecture | **macOS 14 (Sonoma)+, `aarch64-apple-darwin` only.** No universal binary; Intel Macs use the browser broadcaster. |
| OD6 | Picker | **The system `SCContentSharingPicker`.** No in-app picker (plan B only, §10 V-1). |
| OD7 | Audio API | **ScreenCaptureKit audio on the same `SCStream`.** Core Audio process taps are the pre-registered fallback. |
| OD8 | Encode | **VideoToolbox low-latency H.264 only**, trial-gated. No software rung. |
| OD9 | GUI | **Slint**, the Windows shell's information architecture; `UNUserNotificationCenter` notifications; no tray icon, hotkeys or microphone. |
| OD10 | Telemetry, terms, update | In scope day one, mirroring the Windows shell (R28 reporter, TC5 terms, R45 notice, R47 install adapted to a bundle). |

## 3. Non-goals

- **Software encode fallback** — refusal (D8's message).
- **HEVC / AV1** — the viewer negotiates H.264 → VP9 → VP8; VideoToolbox's
  low-latency mode emits High-profile H.264, which is `avc1.64xxxx` in the
  viewer's own preference list. AV1 encode does not exist on any Apple
  Silicon through M5 (decode only).
- **HDR capture** — `captureDynamicRange` is macOS 15+; a follow-up rung.
- **Intel Macs, universal binary** — OD5.
- **Microphone, tray icon, global hotkeys, CLI shell** — docs/38 OD7/OD8/OD12.
- **An in-app picker** — OD6; recorded plan B only.
- **Core Audio process taps** — OD7; fallback only.
- **Mac App Store** and the App Sandbox it requires (ScreenCaptureKit works
  sandboxed, but the sandbox forbids the R47 bundle swap and adds entitlement
  review for nothing this distribution needs).
- **A `.dmg`** — a notarized, stapled `.app` in a `.zip` is the whole
  product (D14).
- **A ladder, auto-fallback or mid-session rung change** — R14 Decision 9.
- **Any change to relay, wire format or viewer** beyond one origin-allowlist
  line (D16). Sharing code with the Go Linux broadcaster.

## 4. Decisions

### D1 — One desktop workspace: `gawk-broadcast-desktop/`, two binaries, one release unit

OD1. The Windows workspace already has the seam this needs: `wire` and
`engine` are platform-neutral, `audio`'s Opus/framer/level/TOC half is
platform-neutral, and `capture`/`encode`/`audio`'s Windows halves compile
empty off Windows (`cfg(windows)`). What changes:

| Crate | After MB0 | After MB1 |
|---|---|---|
| `wire` | unchanged | unchanged — the fourth mirror serves both binaries; G5 is inherited |
| `engine` | unchanged | unchanged, except `defaults` gains the macOS origin, kind and asset-name constants beside the Windows ones |
| `audio` | unchanged | portable half unchanged; `wasapi.rs` stays `cfg(windows)`; a new `sck.rs` is `cfg(target_os = "macos")` |
| `capture` | unchanged | `wgc.rs`/`d3d.rs`/`qpc.rs` stay `cfg(windows)`; `gate.rs`/`fit.rs` (portable) are shared; **`picker.rs` is portable today and stays so** — it is the Windows in-app picker's alt-tab eligibility filter, kept host-testable on purpose (docs/38 D6), and macOS never calls it. The macOS modules are `sck.rs` + **`sck_picker.rs`** (the `SCContentSharingPicker` observer) under `cfg(target_os = "macos")` — a distinct name, because a second `picker` module in the same crate would collide with the unconditional one |
| `encode` | unchanged | `mft.rs` stays `cfg(windows)`; new `vt.rs` (macOS); `h264.rs` (SPS parse, Annex-B) and `cascade.rs` are shared |
| `app` | unchanged | **split**: `app-windows` (the existing crate, renamed) and `app-macos`. Shared `.slint` files move to `crates/ui/` and both shells import them; platform cards differ (§6). |

Not split into per-platform crates (`capture-macos` etc.) because the
portable halves are exactly what should stay in one crate under one test
suite; `cfg(target_os)` on modules is the existing convention and keeps
`cargo test --workspace` meaningful on every host.

One workspace version (`Cargo.toml` `[workspace.package].version`, the
release-please `simple` type as today). Tags: `gawk-broadcast-desktop/vX.Y.Z`.
The **distribution names** stay per platform (D14): the release page holds
`gawk-broadcast-windows-x86_64.exe` and `gawk-broadcast-macos-arm64.zip`.

### D2 — MB0 is a rename with zero behaviour change, landed alone

OD2. Directory, workspace name, workflow file (`broadcast-windows.yml` →
`broadcast-desktop.yml`), release-please component, `coverage-floors.json`
key, `deny.toml` path in `ci.yml`, `renovate.json5`, `THIRD-PARTY-NOTICES`,
root README (module table, badge filter), CLAUDE.md repository layout,
docs/38 (a dated note at the top, not a rewrite), `docs/gotchas.md` paths. What does **not** change in MB0: the Windows EXE's file name,
the release asset name, the R46 manifest path
`releases/gawk-broadcast-windows/latest.json` (D14 keeps manifests keyed by
distribution, so R45's compiled-in URL and the site card are untouched), the
telemetry `kind`, the origin. Acceptance is G11: the artifact from the
renamed tree is the same program.

The old tag series `gawk-broadcast-windows/vX.Y.Z` ends; release-please's
manifest gets the new component at the same version, so the first desktop
release is the next bump, not a reset. The release-please manifest change
and the renamed changelog land in the same PR.

### D3 — Bindings: the `objc2-*` framework crates, with unsafe confined to three modules

The choice is between the `objc2` family (`objc2-screen-capture-kit`,
`objc2-video-toolbox`, `objc2-core-media`, `objc2-core-video`,
`objc2-core-foundation`, `objc2-user-notifications`, `block2` for
completion handlers — auto-generated from Apple's headers, complete,
maintained by the objc2 project, MIT) and `cidre` (one hand-written crate
covering the same frameworks, ergonomic, single-author, "personal research
project" by its own README). `objc2-*` is chosen:

- The CLAUDE.md instinct against depending on what one person can abandon.
  `objc2` has a maintainer group and 100M+ downloads; `cidre` has one
  author and no stability promise.
- The API surface we need is small (one stream, one compression session,
  one picker, one notification center) and the raw bindings' `unsafe` is
  confined to `capture::sck`, `capture::sck_picker`, `encode::vt` and
  `audio::sck`. Everything above those modules is safe Rust, like `wgc.rs`
  and `mft.rs` today with the `windows` crate.
- Panics cannot cross the Objective-C callback boundary (it is `extern "C"`
  in effect: unwinding through it aborts). Every delegate/output callback
  runs under `catch_unwind`, the D3 panic-hygiene rule of docs/38 made
  structural at the FFI edge.

Pre-registered fallback: `cidre`, if an `objc2-*` gap blocks a chunk (the
crates are generated, so a missing selector is a bug report, not a
redesign). The safe `screencapturekit` crate is rejected: it bridges through
Swift with `swift-bridge`, which puts a Swift toolchain in the build for
one crate.

### D4 — Capture: one `SCStream` from the system picker's filter, `420v`, VFR pass-through

- **Picker** (OD6, `capture::sck_picker`): `SCContentSharingPicker.shared` with a configuration of
  `allowedPickerModes = [singleWindow, singleApplication, singleDisplay]`,
  `allowsChangingSelectedContent = true` (re-pick without stopping), our own
  bundle in `excludedBundleIDs`. The observer receives an `SCContentFilter`;
  its `style` (window / application / display) decides the audio mode (D6)
  and the GUI's mode label. The picker is how the user picks content, so
  the app never enumerates `SCShareableContent` and never needs the Screen
  Recording grant on the happy path (G8; verification V-1).
- **Stream**: `SCStream(filter, configuration, delegate)` with one
  `SCStreamOutput` for `.screen` and one for `.audio` on their own serial
  dispatch queues. Configuration: `pixelFormat = 420v`
  (`kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange`) — the format
  VideoToolbox consumes natively, so **there is no conversion stage**;
  `BGRA` is the pre-registered fallback if V-9 shows the encoder rejecting
  SCK's buffers. `colorSpaceName = ITU_R_709_2`, `width`/`height` from D9's
  fit (SCK scales in the compositor; the app never resamples),
  `minimumFrameInterval = 1/fps`, `showsCursor = true` (R14 invariant),
  `capturesAudio` per D6, `queueDepth` per D10.
- **VFR discipline**: SCK is damage-driven. A frame whose
  `SCStreamFrameInfo.status` is `.idle` carries no new content and is
  dropped before the encoder; `.complete` frames pass through the shared
  drop-only `FpsGate`. Never synthesize CFR (R14 Decision 13).
- **Timestamps**: the sample buffer's presentation timestamp, which SCK
  stamps on the host clock (`mach_absolute_time`, `CMClockGetHostTimeClock`).
  D7 holds with the clock swapped.
- **Minimized / hidden**: a minimized window pauses delivery (Apple's
  documented behaviour, same as WGC). Landing as docs/38 D6: viewers keep
  the last frame; the GUI shows "window is minimized — restore it to
  resume" keyed off the frame status going stale for > 1 s. A window on
  another Space is undocumented (V-2).
- **Occluded**: captured in full — the selling point, as on Windows.
- **Ownership rule**: SCK's surface pool stalls if the app holds more than
  `queueDepth − 1` buffers; the encoder path releases every
  `CMSampleBuffer` the moment `VTCompressionSessionEncodeFrame` returns
  (VideoToolbox retains what it needs), so the pool only ever sees one
  buffer in our hands at a time (D10).

### D5 — One clock: the host clock end to end

Inherited from docs/38 D7 with `mach_absolute_time` in place of QPC. Video
frames carry host-clock PTS from SCK; audio sample buffers carry host-clock
PTS from the same SCK stream; TimeSync's `t0` reads the host clock through
the engine's one `Clock` implementation. Both media are stamped from the
same monotonic clock at the source, so the affine mapping is the identity
and A/V skew is zero by construction. The existing engine unit test (a
scripted capture pair preserves source spacing) covers the macOS `Clock`
with no new test shape.

### D6 — Audio: ScreenCaptureKit audio on the same stream; R25's Opus contract verbatim; audio never fails a broadcast

OD7. `capturesAudio = true`, `sampleRate = 48000`, `channelCount = 2`,
`excludeCurrentProcessAudio = true`. SCK filters audio **at the application
level**: a window or application filter yields the owning app's audio and
nothing else (macOS 13+, reliable app scoping since 14.4, which is inside
the 14.0 floor but is recorded as V-4 rather than assumed); a display
filter yields every app's audio except ours. That is mode 1 and mode 2 with
one API and no PID plumbing — the reason OD6's picker costs nothing here.

- **Format**: read from each buffer's `AudioStreamBasicDescription`, never
  assumed (Float32 48 kHz, planar or interleaved, is what the field
  reports). A `Float32 → i16`/interleave shim feeds the shared `Framer`;
  the encoder side (libopus 48 kHz/stereo forced, 128 kbps constant, 20 ms
  frames, DTX/FEC off, `RESTRICTED_LOWDELAY`, TOC-vs-config check) is the
  shared `audio` crate, untouched.
- **Subordination** (R25 Decision 6, docs/38 D8): audio is probed before
  going live — but SCK cannot run an audio-only stream, so the probe is the
  first 500 ms of the real stream's audio output rather than a separate
  trial. Any live audio failure drops audio, notifies, leaves video
  running; audio-off is byte-identical to a video-only broadcaster.
- **Silence hint**: the docs/38 D8 "no audio from *App* yet" hint carries
  over, with its one-click switch reworded: "switch to whole-system audio"
  re-runs the picker in display mode, because system audio in SCK comes
  from a display filter. This is the one mid-session change permitted
  (new capture, same Opus stream, same seq space).
- **Fallback (pre-registered, not built)**: Core Audio process taps
  (`AudioHardwareCreateProcessTap`, macOS 14.2+, `CATapDescription` over a
  flat list of process object IDs, delivered through an aggregate device,
  `NSAudioCaptureUsageDescription` + its own TCC prompt). Taken only if V-4
  finds a game whose audio escapes the SCK app filter, and then as a second
  audio source behind the same `AudioSource` seam.

### D7 — Encode: VideoToolbox low-latency H.264, trial-gated, the docs/38 D9 invariants restated

OD8. `VTCompressionSessionCreate` with an encoder specification of
`EnableLowLatencyRateControl = true` and
`RequireHardwareAcceleratedVideoEncoder = true` — the low-latency mode
**is** hardware-only by Apple's definition, one-in-one-out, no B-frames or
lookahead, High profile. Session properties:

| Invariant | Mechanism | Verified in trial by |
|---|---|---|
| No B-frames (decode order == presentation order) | low-latency mode + `AllowFrameReordering = false` | output PTS strictly monotonic and equal to input |
| Closed 500 ms GOP | **the app forces it**: `kVTEncodeFrameOptionKey_ForceKeyFrame` every `fps/2` frames counted at the encoder input (wall-clock GOP stretches under damage-driven capture and is measured + displayed, EMA α = 0.3, like Linux and Windows). `MaxKeyFrameInterval`/`MaxKeyFrameIntervalDuration` are also set, but low-latency mode is documented as "infinite GOP after the IDR", so the forced-IDR path is the one relied on (V-3) | IDR spacing in the trial bitstream |
| SPS/PPS in-band before **every** IDR | VideoToolbox emits AVCC with parameter sets only in the `CMFormatDescription`; the app prepends them (Annex-B) to every IDR from the description attached to that sample — load-bearing because extradata is empty (docs/38 D10, inherited) | NAL scan of trial bitstream |
| ≤ 1 frame encoder-internal latency | low-latency mode (one-in-one-out by contract) + `RealTime = true` | input-count minus output-count at drain |
| Rate control: peak-constrained, peak = user-facing bitrate, mean = 75 % | `AverageBitRate` = 75 % of the rung's bitrate + `DataRateLimits` = [peak bytes, 1 s]. Apple DTS's own guidance for low-latency live is ABR; `VariableBitRate` is documented incompatible with low-latency mode and `ConstantBitRate` only partly supported — both are **not** attempted (V-6 measures the overshoot on the first frames the forum thread reports) | properties accepted; bitrate over the trial within 10 % of target |
| VFR pass-through | real host-clock PTS + duration on every `EncodeFrame`; `ExpectedFrameRate` = nominal fps for budgeting only | output PTS equal input PTS |
| Cursor embedded | capture-side (D4) | — |
| On-demand IDR (resume re-prime, docs/38 D5) | `ForceKeyFrame` on the next frame | forced mid-trial, IDR observed |
| Profile | `ProfileLevel = H264_High_AutoLevel`; the `avc1.PPCCLL` string is parsed from the SPS by the shared `h264.rs`, never assumed | parser output matches the SPS |

**Trial**: ~30 synthetic `420v` `CVPixelBuffer`s (an IOSurface-backed pool
the trial allocates itself, never the capture stream) through a session
built exactly as the live one; every row above is checked on the output
before the session type is accepted. **Enumeration is not acceptance**
(R14's probing lesson): `VTCopyVideoEncoderList` is consulted only for the
refusal message's wording. The last-good result is cached in the config and
re-verified first at startup; there is no vendor cascade because there is
one hardware encoder on Apple Silicon — the "cascade" is
`420v` → `BGRA` input (V-9), then refusal.

**Refusal** (G3), wording adapted from docs/38 D9 because the reason is the
same as Windows':

> No hardware H.264 encoder was found, so gawk-broadcast can't start —
> it deliberately has no software encoder. The browser broadcaster at
> &lt;app URL&gt; hardware-encodes fine on a Mac; what you lose without
> this app is per-application audio and background-window capture.

**Zero-copy**: SCK's IOSurface-backed `CVPixelBuffer` goes straight into
`VTCompressionSessionEncodeFrame`; nothing touches system memory until the
encoded AU comes back as a `CMBlockBuffer`, which is copied once into the
`AccessUnit` the engine owns (the Windows path copies once at the same
point).

### D8 — Annex-B with empty extradata; codec string from the SPS

Inherited verbatim from docs/38 D10. The AVCC → Annex-B rewrite (4-byte
length prefixes → start codes) is a new shared helper in `encode::h264`,
unit-tested on Windows vectors too, since Media Foundation vendors differ
and this is now the single place that guarantees the invariant on both
platforms.

### D9 — The rung: 1080p60 / 500 ms GOP / 12 Mbps peak, with the advanced overrides

Inherited from docs/38 D11, including the fit rule: the picked content's
size is fitted into the rung's bounding box preserving aspect (the shared
`fit.rs`), and SCK is asked for the fitted size so the compositor scales.
Retina: a 5K display fitted to 1080p is a 2.67× downscale in the
compositor — the same path Apple's own screen sharing uses; V-5 measures
whether it costs frame pacing at 60 fps.

### D10 — Backpressure: `queueDepth` and the in-flight pin, restated for SCK

docs/38 pins `mft::MAX_IN_FLIGHT < d3d::RING_SLOTS` so backpressure trips
before the converter ring wraps onto an in-flight texture. Here there is no
converter ring, and the pool is SCK's: `queueDepth` (3–8, default 3) is the
number of surfaces SCK will have outstanding. The restated pin:

- the encoder's `MAX_IN_FLIGHT` (frames submitted, not yet returned) is the
  backpressure gate: at the limit, drop the incoming frame (favor dropped
  frames over stalled playback) and count it;
- `queueDepth = MAX_IN_FLIGHT + 2`, so SCK never stalls on our account even
  when every in-flight slot is full and one buffer is in the output
  callback — a `const` assertion, as on Windows;
- because VideoToolbox retains the `CVPixelBuffer` for the encode's
  duration, "in flight" means "buffers VideoToolbox holds", and the pool
  math is on that count.

V-5 measures the actual encode latency at 1080p60 and whether
`MAX_IN_FLIGHT = 3` (the Windows value) keeps the gate from tripping at
steady state.

### D11 — GUI: Slint, the Windows shell's cards, a picker button instead of a picker card

OD9. The `.slint` sources move to a shared `crates/ui/` and both shells
compile them with `slint-build`; platform differences are properties and
one card, not forks. Slint's royalty-free desktop licence covers macOS on
the same terms (docs/38 D12, revised 2026-08-01) and the "Made with Slint"
badge already ships in the READMEs; `deny.toml` is unchanged.

Cards, with the delta from docs/38 D12 named:

1. **Header** — identical (state, heartbeat, encode line reading
   `VideoToolbox — Apple H.264 · zero-copy · 1920×1080@60`, audio line +
   level meter, "N watching", upload-bandwidth warning, version badge with
   the R45 line).
2. **Share card** — replaces the Windows picker card. A **"Choose what to
   share…"** button opens the system picker; after a pick it shows what
   was chosen (window title + app name, app name, or display name), the
   mode label ("this app's audio" / "whole-system audio"), a 1 Hz thumbnail
   rendered from the live stream's own frames (downscaled off the capture
   queue via `vImage` — no second stream, no second permission), and a
   **"Change…"** button (re-pick). No thumbnails-before-picking: the system
   picker has them.
3. **Broadcast card** — identical (Start/Stop, code, join link, copy).
4. **Settings card** — identical, with `macOS` in the relay/app/telemetry
   captions and the R45 checkbox.
5. **Diagnostics** — identical, `kind: "gawk-broadcast-macos"`.

Look and feel: system light/dark followed, the gawk accent, SF-adjacent
spacing; **the window has a real `.app` menu bar** (Quit, About with the
version, Settings…) because macOS users expect ⌘Q and ⌘, to work — Slint
exposes the native menu on macOS. Close-while-live confirmation as on
Windows. Notifications via `UNUserNotificationCenter` with the docs/38 D12
urgency mapping; requires a bundle identifier (D14), which is one more
reason the product is a bundle, never a bare binary. Focus mode behaviour
is V-8.

### D12 — Config: `~/Library/Application Support/gawk/broadcast.json`, mode 0600, no Keychain

Same file name and keys as Linux and Windows (docs/38 D14), plus
`captureMode` as on Windows. Atomic write, corrupt-tolerant, blank means
the default resolved at use (docs/38 D13, defaults table with
`origin = gawk-broadcast://macos`).

Credentials (`publishSecret`, `lastResumeToken`): macOS is Unix, so the
**Linux rule applies unchanged** — the file is mode 0600 and the values
are plaintext in it. The Keychain was considered and rejected: a Keychain
item's ACL is bound to the signing identity, so every ad-hoc dev build (and
any signing change) prompts "wants to use your confidential information" —
exactly the identity-churn problem D13 exists to avoid, moved somewhere
worse. A copied config file leaks the same thing it does on Linux, where
the same trade was accepted.

### D13 — Signing, notarization and secrets: Developer ID, App Store Connect API key, nothing identifying in the tree

OD3. This is load-bearing, not polish: the Screen Recording grant is keyed
on the app's **designated requirement**, which for an ad-hoc signature is
the binary's own hash — so every rebuild is a new app to TCC — and Sequoia
removed the Control-click override for unsigned apps. A Developer ID
signature makes the requirement `<Team ID> + <bundle ID>`, stable across
releases (G10).

Mechanics, in the macOS release job (D15):

| Step | Tool | Input |
|---|---|---|
| Import the signing identity into a throwaway keychain | `security import` | `APPLE_SIGNING_CERTIFICATE_P12` (base64 `.p12`), `APPLE_SIGNING_CERTIFICATE_PASSWORD` |
| Sign the bundle | `codesign --sign "<identity>" --options runtime --timestamp --entitlements entitlements.plist` | the identity name is **discovered** from the imported keychain (`security find-identity -v -p codesigning`), never written down |
| Notarize | `xcrun notarytool submit --wait` | `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID`, `APPLE_NOTARY_KEY_P8` — an **App Store Connect API key**, chosen over Apple-ID + app-specific password precisely so **no Apple ID or e-mail address exists anywhere in the repo, the workflow or the logs** |
| Staple | `xcrun stapler staple` | — |
| Zip for release | `ditto -c -k --keepParent` | — |

Rules:

- **Every one of those five values is a repository secret.** The workflow
  references them by name only; none is echoed; `security find-identity`
  output is not printed. The Team ID appears in the signed binary's
  signature (that is unavoidable and public by design) but is never a
  constant in the tree.
- **PRs never sign.** `pull_request` runs (including from forks, which see
  no secrets) build an **ad-hoc-signed** bundle (`codesign -s -`) uploaded as
  a CI artifact for the owner's own testing, with `BUILD-INFO.txt` saying so
  and the README's "test build" note explaining the consequence: an
  ad-hoc build must be opened via System Settings → Privacy & Security →
  Open Anyway, and any TCC grant it acquires dies with the build. Signed,
  notarized bundles are produced only on `push` to `main` and by the attach
  job. This mirrors the existing "never on pull_request" posture of the
  attach jobs (#218).
- **A run that should sign but cannot fails loudly** (missing secret,
  notarization rejected) rather than attaching an unsigned bundle — docs/48
  SU1's rule for minisign, applied here.
- **Hardened runtime, minimal entitlements**: `entitlements.plist` is an
  empty dictionary unless a trial shows a need (ScreenCaptureKit and
  VideoToolbox need none; `com.apple.security.cs.allow-jit` and friends are
  not requested). `Info.plist` carries `NSScreenCaptureUsageDescription`
  (shown only if the non-picker path is ever hit) and *not*
  `NSAudioCaptureUsageDescription` (added with the tap fallback, if ever).
- **Bundle identifier** `fi.ioio.gawk.broadcast` — derived from the project
  domain that already ships as the default relay/app URL, so it reveals
  nothing new. A fork edits the constant like `SITE_URL`.
- **R47 keys**: the minisign key pair docs/48 D1 introduces signs
  `SHA256SUMS` for this release set too, unchanged.

### D14 — Packaging: a notarized `.app` in a `.zip`; per-distribution manifests; the site card

- The only deliverable is `gawk-broadcast-macos.app`, assembled by
  `tools/macos/bundle.sh` from the release binary, a templated
  `Info.plist` (`CFBundleIdentifier`, `CFBundleShortVersionString` from
  `version.txt`, `LSMinimumSystemVersion = 14.0`, `NSHighResolutionCapable`,
  `LSApplicationCategoryType = public.app-category.video`,
  `CFBundleIconFile` when R44 supplies the `.icns` — until then no icon,
  like the other two apps today), `entitlements.plist`, and the same
  README/THIRD-PARTY-NOTICES/LICENSE set the Windows artifact
  carries. No installer, no `.dmg` (a stapled bundle in a zip opens clean;
  a `.dmg` would need its own notarization and buys a drag-to-Applications
  animation).
- **Release asset**: `gawk-broadcast-macos-arm64.zip`, fixed name, beside
  the Windows EXE on the same `gawk-broadcast-desktop/vX.Y.Z` release, with
  its own `SHA256SUMS` line (one `SHA256SUMS` per release set, signed once
  by R47's minisign step).
- **Manifests stay per distribution, not per component.** R46's writer
  takes a name and a primary asset; the desktop attach job runs it **twice**
  — `releases/gawk-broadcast-windows/latest.json` (primary: the EXE, path
  unchanged so R45's compiled-in URL and the existing site card are
  untouched) and `releases/gawk-broadcast-macos/latest.json` (primary: the
  zip). The manifest's `component` field carries the distribution name; the
  R45 validator (docs/47 D5) already compares it against "the app's own",
  which for the macOS binary is `gawk-broadcast-macos`. docs/46 gets a dated
  note that "component" in the manifest means distribution.
- **Site**: a third download card, `data-dl="gawk-broadcast-macos"`, reading
  its manifest exactly like the other two; the no-script fallback links the
  releases page filtered by the desktop tag. README badge filter becomes
  `gawk-broadcast-desktop*`, labelled `broadcast-desktop`.
- **Coverage**: one measurement, in the Linux job, under the renamed
  `gawk-broadcast-desktop` key at the existing floor; the macOS job runs
  the tests but does not report coverage (one floor, one source of truth).

### D15 — CI: a `macos-latest` job beside the Linux one, path-filtered, unsigned on PRs

OD4. `broadcast-desktop.yml` keeps the existing Linux jobs (host clippy,
`cargo xwin clippy`, tests incl. the real-relay integration suite, coverage,
the Windows artifact, the attach job) and adds:

- **`macos`** on `macos-latest`: `cargo fmt --check`, `cargo clippy
  --all-targets -- -D warnings` (host = Darwin, so this is what type-checks
  every `cfg(target_os = "macos")` block — the analogue of the xwin clippy
  pass), `cargo test --workspace` (portable halves plus the macOS crates'
  host-testable logic: Annex-B rewrite, plist templating, picker-filter →
  mode mapping, fps gating, config), `cargo build --release --target
  aarch64-apple-darwin`, bundle, ad-hoc sign, upload
  `gawk-broadcast-macos-<sha>` with `BUILD-INFO.txt`
  (`built-by: GitHub-hosted macos-latest`, toolchain, SDK version from
  `xcrun --show-sdk-version`, `signed: ad-hoc (PR build)` or
  `Developer ID + notarized`).
- **Trigger paths**: the workspace, `gawk-server/wire/**`, `tools/macos/**`,
  the workflow itself. A docs-only PR starts nothing (G11).
- **Relay integration on macOS**: the `ignored` real-relay suite runs once
  in the `macos` job too (Go is on the hosted image; `gawk-server` builds
  there), because "wtransport works on macOS" is a claim the transport
  gates should prove, not the crate's README (MB1).
- **Attach**: the existing `attach-release` job gains the macOS steps of
  D13 and the second manifest of D14; it needs the macOS bundle, so the
  signed build runs on `macos-latest` inside that job (secrets available:
  `push` to `main` only). Cost: macOS minutes are free on a public
  repository; the job is expected under ten minutes.
- **Why not cross-compile from Linux** (the docs/38 D18 instinct): it needs
  `MacOSX.sdk` extracted onto Linux, whose licence terms restrict use to
  Apple hardware, and signing/notarizing from Linux (`rcodesign`) is
  possible but unproven with a hardened-runtime bundle. Free hosted macOS
  runners remove the reason to try.

### D16 — Origin: `gawk-broadcast://macos`

Inherited from docs/38 D19 with the value changed. The production relay's
`-allowed-origins` gains this entry before first use — the one permitted
production-side change (G4 carves it out).

### D17 — Telemetry, terms, update check and install

- **Telemetry** (docs/38 D15): `kind: "gawk-broadcast-macos"`; everything
  else shared code.
- **Terms**: TC5 posture, link in the settings card.
- **R45 notice**: the shared `engine::update` module with the macOS
  distribution name and asset name (`gawk-broadcast-macos-arm64.zip`) in
  the docs/47 D5 validator's per-platform table; the manifest URL for
  `gawk-broadcast-macos`.
- **R47 install, adapted to a bundle** (docs/48 D5 is a one-file rename
  swap): download the zip to the bundle's parent directory, verify
  minisign + sha256, `ditto -x -k` into `<name>.app.new`, verify with
  `codesign --verify --deep --strict` **and** `spctl --assess --type
  execute` (the stapled ticket makes the Gatekeeper check offline), rename
  the running bundle to `.app.old`, rename `.new` into place, `open -n` the
  new bundle, exit; the new build deletes `.old` at its next launch.
  Because the app wrote the files itself they carry no quarantine
  attribute, and because the designated requirement is unchanged the TCC
  grant (if any) survives — both are acceptance criteria (V-7), not
  assumptions. If the bundle's parent is not writable (an admin-installed
  `/Applications` on a standard account), the flow stops at "download
  ready, here is the file", per docs/48 D5.

## 5. Architecture

```
gawk-broadcast-desktop/
  Cargo.toml              # workspace; release-please bumps the version
  crates/
    wire/                 # unchanged: the fourth mirror, both binaries
    engine/               # unchanged: session, send policy, resume, timesync,
                          #   telemetry, update; platform constants in defaults
    capture/              # gate.rs, fit.rs, picker.rs (Windows alt-tab filter, portable) shared
                          #   windows: wgc.rs, d3d.rs, qpc.rs
                          #   macos:   sck.rs (SCStream), sck_picker.rs (SCContentSharingPicker)
    encode/               # h264.rs (SPS parse, AVCC→Annex-B), cascade.rs shared
                          #   windows: mft.rs      macos: vt.rs
    audio/                # framer, opusenc, level, toc shared
                          #   windows: wasapi.rs   macos: sck.rs (SCStream audio output)
    ui/                   # the .slint sources both shells compile
    app-windows/          # the Windows shell (today's app crate, renamed)
    app-macos/            # the macOS shell: menu bar, notifications, bundle-aware paths
  tools/macos/            # bundle.sh, Info.plist.in, entitlements.plist
```

Dataflow (one process, two SCK output queues, one encoder, no pipes):

```
SCContentSharingPicker ──SCContentFilter──▶ SCStream
                                              │ .screen (420v, host PTS)     │ .audio (Float32 48k, host PTS)
                                              ▼                              ▼
                                       idle-drop + FpsGate             shim → Framer → libopus
                                              │                              │
                                              ▼                              ▼
                                   VTCompressionSession            audio lane (seq, 1 Hz config)
                                     (forced IDR cadence)                    │
                                              │ AVCC + fmt desc              │
                                              ▼                              │
                                 AVCC→Annex-B, SPS/PPS before IDR            │
                                              │                              │
                                              ▼                              ▼
                              engine: classify IDR, chunk, StreamFrame ─▶ wtransport session ─▶ relay
                                              │ 1 Hz
                                              ▼
                                        GUI thumbnail (vImage downscale)
```

The engine's seams (`RelaySession`, `VideoSource`/`AudioSource`, `Clock`)
are the contract; nothing macOS-specific is visible above them.

## 6. UX flows

**First run (G8)**: unzip → double-click (or drag to Applications first) →
Gatekeeper opens a notarized app silently → window opens on the Share card
→ "Choose what to share…" → the system picker → pick a window / app /
display → the card fills in → `Start broadcast` → encode line + code + join
link → `Copy link`. Zero fields touched, zero permission dialogs.

**Refusal (G3)**: no hardware encoder ⇒ error card with D7's message; the
browser-broadcaster link uses the resolved app URL.

**Re-pick**: "Change…" opens the picker again; if the filter style changes
(window → display), the audio mode follows and the encoder is *not*
restarted — a new capture, same session.

**Resume**: inherited (amber heartbeat, "Reconnecting…", forced IDR on
success; 4004 terminal; `Resume <CODE>` after an app restart).

**Mode-1 audio doubt**: level meter flat ~10 s ⇒ D6's hint, one click into
the picker in display mode.

**Update**: the R45 line under the version badge; clicking "Install" runs
D17's swap, never while live.

**Stopping**: `Stop broadcast`, ⌘Q, or close the window (with the
are-you-live confirmation). The stream is stopped synchronously before exit
so SCK never keeps a capture running for a dead process (the `finish()`
incident class).

## 7. What deliberately does not exist (inherited prohibitions)

docs/38 §7 verbatim: no software encode rung; no preview player (the 1 Hz
thumbnail is the bounded exception); no viewer→server keyframe
back-channel; no mint fallback outside connect-phase failures; no
auto-resume through 4000/4004; no second clock; no standalone DecoderConfig
datagrams; no DTX/FEC/Opus bitrate knob. Added here: no in-app picker on
the happy path; no Keychain; no `.dmg`; no sandbox.

## 8. Chunks and acceptance criteria

Prefix **MB** (Mac Broadcaster; first free two-letter prefix that reads
right — `MF` is R22's).

Ordering: MB0 alone first (D2). MB1 proves the transport on Darwin and
gives CI a macOS job before any framework code. MB2–MB4 are
hardware-facing and land behind trials. MB5 assembles the product, MB6 the
non-media plumbing, MB7 the release path, MB8 is the pass that decides the
milestone.

### MB0 — The rename

| Acceptance criterion | Verified by |
|---|---|
| `gawk-broadcast-windows/` → `gawk-broadcast-desktop/`; every reference in D2's list updated in one PR; `git log --follow` works on the moved files | review |
| The Windows job (renamed workflow) is green, triggers only on its paths, uploads the artifact under the new name; a docs-only PR does not start it (G11) | CI, observed |
| release-please: component `gawk-broadcast-desktop` at the current version; combined-release-PR behaviour intact; the next release tags `gawk-broadcast-desktop/vX.Y.Z` and still attaches `gawk-broadcast-windows-x86_64.exe` and writes `releases/gawk-broadcast-windows/latest.json` | review + the first release after merge |
| The EXE from the renamed tree runs on the gaming PC and broadcasts to production; `kind`, origin and the R45 URL unchanged (G11) | manual smoke |
| docs/38 carries a dated rename note at the top; CLAUDE.md repository layout names the new directory; `coverage-floors.json` key renamed at the same floor | review |

### MB1 — Workspace split, macOS CI job, transport on Darwin

| Acceptance criterion | Verified by |
|---|---|
| `app` → `app-windows` + `app-macos`; `.slint` sources in `crates/ui/`; `cargo test --workspace` green on Linux (host), via `cargo xwin clippy` (msvc), and on `macos-latest` (D15) | CI |
| The `macos` job: fmt, clippy `-D warnings` on Darwin, tests, release build, ad-hoc-signed bundle artifact `gawk-broadcast-macos-<sha>` with `BUILD-INFO.txt`, `if-no-files-found: error` | CI |
| The real-relay integration suite passes on `macos-latest`: Origin header honoured by an allowlisting relay, rejection status and close code visible, kill-and-restart resume (docs/38 D2 gates, on Darwin) | CI |
| `gawk-broadcast://macos`, `kind`, distribution + asset names as constants in `engine::defaults`; the docs/47 D5 validator table gains the macOS row with a test | unit |
| `app-macos` launches on a Mac, shows the window with the D11 cards in their empty state, and quits via ⌘Q; no framework code yet | manual |

### MB2 — Capture

| Acceptance criterion | Verified by |
|---|---|
| The system picker opens from the Share card, returns a filter, and the card shows what was picked with the right mode label; re-pick works while idle and while live | manual |
| **No Screen Recording prompt** on the picker path on a fresh user account (V-1) | manual |
| `SCStream` delivers `420v` frames at the fitted size with host-clock PTS; `.idle` frames dropped; drop-only fps gating; cursor visible | unit (status/gate logic on fake frames) + manual |
| Buffer ownership: at most one `CMSampleBuffer` held outside VideoToolbox at any time; the `queueDepth = MAX_IN_FLIGHT + 2` pin is a `const` assertion | unit + review |
| Occluded window keeps delivering; minimized shows the hint; another-Space behaviour recorded (V-2) | manual |
| Panic in an output callback surfaces as a session error and ends the broadcast cleanly (D3) | unit (injected panic on a fake output) |

### MB3 — Encode

| Acceptance criterion | Verified by |
|---|---|
| Session creation with the D7 specification; refusal path with the curated message when creation is faked to fail (G3) | unit |
| Trial gate enforces every D7 row on real hardware, recorded bitstream analysis; forced-IDR cadence gives 500 ms GOPs under low-latency mode (V-3) | manual, recorded |
| AVCC→Annex-B rewrite + SPS/PPS-before-every-IDR from the format description; unit-tested on Windows vendor fixtures too (D8) | unit |
| `avc1.PPCCLL` from the SPS matches the Go `sps.go` vectors (shared code, no new test) | existing unit |
| ABR + `DataRateLimits` accepted; bitrate over the trial within 10 % of target; first-frames overshoot measured (V-6) | manual |
| `420v` input accepted directly, or the `BGRA` fallback recorded (V-9) | manual |
| Last-good result cached and re-verified first; forced IDR on resume | unit (scripted trial) + integration |

### MB4 — Audio

| Acceptance criterion | Verified by |
|---|---|
| Mode 1: target app audible, concurrent other app inaudible (G1) for ≥ 2 real games incl. one with a launcher/helper process (V-4) | manual |
| Mode 2: system audio; output-device switch survives or fails audio-only (G2) | manual |
| ASBD read per buffer; Float32/planar/interleaved all handled into the shared `Framer`; 48 kHz stereo on the wire | unit (synthetic ASBDs) + manual |
| Opus packets: shared checks (TOC, one packet per datagram, oversize dropped + counted) | existing unit |
| Subordination: probe-fail and live-fail leave video running; wire byte-identical to video-only | unit (fake source) + manual |
| Single-clock invariant holds with SCK audio PTS (D5) | existing unit against the macOS `Clock` + manual `avSkewMs` |

### MB5 — GUI shell

| Acceptance criterion | Verified by |
|---|---|
| All D11 cards; Idle/Starting/Live + amber; picker → start → code → copy-link flow (G8) | manual |
| Menu bar: Quit, About (version), Settings; ⌘Q ends the broadcast and stops the stream before exit | manual |
| Notifications via `UNUserNotificationCenter` with the urgency mapping; Focus behaviour measured (V-8) | manual |
| Config at the D12 path, mode 0600, blank-means-default captions, panel disabled while live | unit (config) + manual |
| 1 Hz thumbnail from the live frames; idle window ~0 % CPU | manual + profiler |
| Diagnostics JSON: `kind`, nullable-pointer semantics, browser field names | unit |

### MB6 — Telemetry, update notice, update install

| Acceptance criterion | Verified by |
|---|---|
| TelemetryHello handled; batches with the browser field names; a real session lands in the production telemetry dashboard as `gawk-broadcast-macos` | unit + manual |
| R45 notice against `releases/gawk-broadcast-macos/latest.json`; opt-out checkbox | unit + manual |
| R47 bundle swap (D17): verify → swap → relaunch; no quarantine on the new bundle; TCC grant survives (V-7); read-only parent falls back to "download ready" | manual, two builds |

### MB7 — Packaging, signing, release path, docs

| Acceptance criterion | Verified by |
|---|---|
| `tools/macos/bundle.sh` produces a bundle whose `Info.plist` carries the D14 keys; `codesign --verify --deep --strict` and `spctl --assess` pass on the release artifact | CI (signed run) |
| Secrets referenced by name only; no identity, Team ID, key ID or address in the tree or in any log line; a run missing a secret fails the attach loudly | review + CI |
| PR builds are ad-hoc signed and say so in `BUILD-INFO.txt`; `push` to `main` and the attach job sign + notarize + staple | CI, observed on a PR and on `main` |
| Attach: zip beside the EXE on the desktop release; `SHA256SUMS` covers both; **two** manifests written; the site's third card renders from the macOS one; README badge filter updated | the first release after merge |
| README section for macOS (fresh-account first run, the ad-hoc test-build caveat, the "when it doesn't work" list); docs/46 dated note (component = distribution); ROADMAP status; CLAUDE.md layout; gotchas synced | review |

### MB8 — The on-hardware acceptance pass

| Acceptance criterion | Verified by |
|---|---|
| G1, G2, G6 (photographed reference), G7 (`avSkewMs`), G9 (fleet rollout), G10 (two consecutive signed releases on a fresh account) all pass against production | manual |
| Every §10 register item has a recorded answer in §11 | review |
| Relay `-allowed-origins` gains `gawk-broadcast://macos` in the production values | review + manual |

## 9. Risks

- **The picker exemption is undocumented** (V-1). If the system-picker path
  still triggers Sequoia's periodic re-approval prompt, the product still
  works — the prompt is a monthly click, not a failure — and the finding is
  recorded; the in-app picker (plan B) is not built for it.
- **Low-latency mode vs. a 500 ms GOP** (V-3). The forced-IDR path is
  under our control frame by frame, so the risk is an encoder that ignores
  `ForceKeyFrame` in low-latency mode; then the fallback is a non-low-latency
  session with `AllowFrameReordering = false` and `RealTime = true`, whose
  latency V-5 would have to measure against G6.
- **SCK app-level audio with helper processes** (V-4). Same shape as
  Windows V-3a; the D6 hint + display-mode switch is the designed landing;
  process taps are the named next rung.
- **Signing-secret handling.** A leaked `.p12` is a revocation and a new
  certificate — the TCC designated requirement survives that (it keys on
  Team ID, not the certificate), which is why D13 keys nothing on the
  certificate itself.
- **`objc2-*` gaps.** Generated bindings can lag a new SDK; the fallback is
  named (D3).
- **A shared release unit.** A Windows regression now ships a macOS bump
  and vice versa; accepted in OD1, and the per-distribution manifests mean
  a retraction (docs/47 D11) can still be per platform.

## 10. On-hardware verification register

**V-1** the system picker needs no Screen Recording grant, and whether the
Sequoia re-approval prompt appears over 30+ days (MB2, MB8); **V-2** a
captured window on another Space / hidden app (MB2); **V-3** forced-IDR
cadence under `EnableLowLatencyRateControl` (MB3); **V-4** SCK app-scoped
audio vs. games with launcher/helper processes, ≥ 2 real games (MB4);
**V-5** encode latency at 1080p60 and the `MAX_IN_FLIGHT`/`queueDepth` pin
at steady state, incl. a 5K → 1080p compositor downscale (MB2/MB3); **V-6**
ABR + `DataRateLimits` acceptance and first-frame overshoot (MB3); **V-7**
TCC grant and quarantine state across an R47 bundle swap and across two
signed releases (MB6, MB8); **V-8** notifications vs. Focus modes during a
fullscreen game (MB5); **V-9** `420v` SCK buffers accepted directly by the
compression session (MB3).

Findings land in §11 as they arrive; decisions are revised in place with a
dated note (docs/README conventions).

## 11. Deviations and field findings

**MB0 (2026-09-22)** — what the rename turned up beyond D2's list:

- **The manifest writer could not express "distribution ≠ component".**
  `publish-release-manifest` used its one `component` input for the version
  lookup, the `releases/<…>/latest.json` path *and* the manifest's
  `component` field, and `tools/releases/manifest.py` rejected any tag whose
  stem was not that field. Keeping `releases/gawk-broadcast-windows/latest.json`
  (D2, D14) therefore needed part of D14's mechanism in MB0 rather than MB7:
  the action gained a `distribution` input (defaulting to `component`, so the
  Linux caller is unchanged), and the tool a `TAG_STEMS` table naming the
  tag stems each distribution may carry — `gawk-broadcast-windows` accepts
  `gawk-broadcast-desktop` and its own pre-rename stem (backfills),
  `gawk-broadcast-macos` only `gawk-broadcast-desktop`. MB7 only adds the
  second call.
- **The coverage badge writer would have counted the Windows workspace
  twice.** `coverage.py badges` carries every record forward, so the last
  `gawk-broadcast-windows` record would have stayed in `data.json` and the
  aggregate for ever beside the new `gawk-broadcast-desktop` one. It now drops
  any component absent from `coverage-floors.json` (and its badge file) —
  safe because `check` already refuses a record for an unlisted component.
- **Artifact names are distribution names.** MB0's "uploads the artifact
  under the new name" is read as the renamed *workflow*: the CI artifact stays
  `gawk-broadcast-windows-x86_64-<sha>`, matching D15's
  `gawk-broadcast-macos-<sha>`, the release asset and the README's
  `gh run download` line.
- **Backfills of pre-rename releases** must be dispatched from a pre-rename
  ref: the attach job now asks the manifest for `gawk-broadcast-desktop`,
  which a pre-rename commit's manifest does not have. It fails loudly ("not a
  package"), never attaches the wrong thing.
- **The site's no-script fallback** for the Windows card filtered releases
  by `gawk-broadcast-windows`, which would stop matching the newest release;
  it filters by `gawk-broadcast-desktop/` now. The card's manifest URL is
  unchanged.
- **First desktop release.** release-please finds no release for the new
  component and attributes commits by path; nothing before the rename
  touches `gawk-broadcast-desktop/`, so the first desktop changelog starts at
  the MB0 commit and the version continues from the manifest's 1.5.0.
- **Darwin, early**: the whole workspace (Slint included) builds, and `cargo
  test --workspace`, host clippy and the real-relay integration suite (5/5)
  pass on an Apple Silicon Mac, macOS 26.5, from the renamed tree. MB1 still
  has to prove it on `macos-latest`.

**MB1 (2026-09-23)** — deviations from D1/D11 as written, and findings:

- **`crates/ui` is a crate (`gawk-ui`), not a directory both shells
  compile.** D11 has each shell run `slint-build` over the shared `.slint`;
  that gives each shell its own generated `MainWindow` type, so no Rust
  that seeds or reads the window could ever be shared — MB5 would copy the
  Windows shell's ~1,000 lines of settings/room/caption plumbing. `gawk-ui`
  compiles `main.slint` once and exports the types, and holds what both
  shells need above the engine: `version` (moved from the Windows shell,
  with the build-revision stamp its `build.rs` emitted — `rustc-env` only
  reaches the crate that emits it) and, as the first shared window logic,
  `refresh_captions`. The Windows shell's build script now only links the
  icon. D11's rule stands unchanged: one `.slint`, platforms differ in
  properties and one card (`system-picker` switches the Windows picker card
  for the Share card; `mono-font` replaces the hardcoded Consolas).
- **Binary name `gawk-broadcast-macos`**, not `gawk-broadcast`: two
  workspace binaries with one name collide in `target/` on any host that
  builds both. It matches the bundle name anyway.
- **`app-macos` is a stub off macOS, with every dependency macOS-gated.**
  The Linux and msvc jobs build the whole workspace; gating keeps the
  second Slint shell out of their compile (and the EXE's feature
  unification untouched). The Windows `build` job now names its package
  (`-p gawk-broadcast-app-windows`) so it never links the stub EXE.
- **Per-distribution identity in `engine::defaults`**: `WINDOWS`, `MACOS`
  (`name`, `origin`, `asset`, `os`) and `THIS`, chosen by `target_os` — not
  `cfg(windows)`, because the Windows shell is tested on Linux hosts and must
  keep its identity there. `ORIGIN` and the telemetry reporter's
  `browser`/`os` derive from `THIS`. docs/47 D5 carries a dated note: its
  validator table's macOS row is these constants (AU3 is not started, so
  there is no validator yet to extend — the constants and their pin test are
  what MB1 can deliver of that criterion).
- **`deny.toml` judges the macOS graph too** (`targets` gains
  `aarch64-apple-darwin`): from MB1 a macOS artifact exists, and an unjudged
  graph is how a GPL crate would arrive. `cargo-deny check licenses` passes
  with it.
- **`tools/macos/` is at the repository root** (with `tools/icon` etc.), as
  D15's trigger list names it; §5's tree draws it inside the workspace. The
  bundle is assembled and signed by `tools/macos/bundle.sh` — ad-hoc unless
  `SIGN_IDENTITY` is set, hardened runtime and the empty entitlements either
  way — and verified with `codesign --verify --deep --strict`. No icon until
  `tools/icon` generates an `.icns` (D14).
- **The CI artifact carries no THIRD-PARTY-NOTICES yet.** The committed file
  is the msvc graph's; the macOS graph (Slint's AppKit backend, `objc2`,
  …) needs its own generated list, which is MB7's release-artifact work. The
  MB1 artifact is a 7-day CI build for on-hardware passes, not a release.
- **Local, on an Apple Silicon Mac (macOS 26.5)**: fmt, Darwin clippy
  `-D warnings`, `cargo test --workspace` (the new `defaults` pins run their
  macOS branch), the real-relay suite, the release build and the bundle all
  pass; the bundle opens under the hardened runtime showing the D11 cards
  empty, and exits cleanly on a `quit` Apple Event — the same `terminate:`
  path the application menu's Quit (⌘Q) takes. Pressing ⌘Q itself is left
  to the owner's manual pass: this session's terminal has no Accessibility
  grant to send keystrokes.
- **Owner pass (2026-09-23)**: the CI bundle opens on the owner's Mac and
  ⌘Q closes it — MB1's manual criterion passes.

**MB2 (2026-09-23)** — how the capture landed, deviations, and findings:

- **The policy is its own portable module**, `capture::sck_policy`: frame
  admission (only `.complete` frames, then the shared drop-only `FpsGate`),
  the staleness watch behind the minimized hint, `SCShareableContentStyle` →
  audio scope / mode label / `captureMode`, the `CallbackGuard` panic fence,
  CMTime → 100 ns, the `queueDepth` pin and the thumbnail. Every rule is a
  host test on all three CI hosts; `sck` and `sck_picker` only translate
  framework values. `host` is the macOS twin of `qpc` (D5): the engine's
  `QpcMapper` is unit-agnostic affine arithmetic, so it maps host-clock
  100 ns ticks unchanged.
- **Stale means "no live content", not "no new frames".** D4 keys the hint
  off the frame status going stale; `.idle` is SCK's "nothing changed", so
  counting it as stale would flag every unchanging window as minimized.
  Only a run of `.blank`/`.suspended` (or no frames) for > 1 s shows the
  hint. Whether a minimized window arrives as `.blank` or as silence is V-2's
  to record; both trip it.
- **The `queueDepth` pin lives in `sck_policy`**, as
  `QUEUE_DEPTH = ENCODER_MAX_IN_FLIGHT + 2` with `const` assertions for the
  pin and SCK's documented 3–8 range. MB3's VideoToolbox session takes its
  in-flight limit from that constant, so the pin cannot drift from the gate
  it protects.
- **Ownership is enforced by the type, not by a count.** A `Frame<'a>` is
  lent to the frame callback for the call only (the output queue is
  serial), so the app can never hold a second SCK surface; MB3's encoder
  keeps what it needs by VideoToolbox retaining the pixel buffer. D10's
  "at most one outside VideoToolbox" is therefore review-verified by the
  signature rather than unit-counted.
- **The thumbnail is pure Rust, not vImage** (D11): a nearest-neighbour,
  BT.709 video-range NV12 → RGBA sample of the frame already in hand, once
  a second, into a 320×180 box. At that size and rate it costs less than a
  vImage round trip, and it is a host test. It shows in the window's
  existing thumbnail card; moving it into the Share card is MB5's layout
  work.
- **Start is a capture test until MB3.** There is no encoder yet, so Start
  runs the stream alone and the header says "Capture test — nothing is
  sent"; the encode line and Details show the delivered size, pixel format,
  measured fps and both drop counters. That is what MB2's manual criteria
  are checked against.
- **The picker is active only while it is on screen or a capture runs.**
  Activating it at launch (the first cut) put the system's screen-sharing
  indicator in the menu bar of an app that was sharing nothing. It is now
  activated on present, kept active while live (so the menu-bar control can
  re-pick too, D4), and deactivated after every picker result when idle and
  on stop.
- **Share-card names need macOS 15.2.** `SCContentFilter`'s
  `includedWindows`/`includedApplications`/`includedDisplays` arrived in
  15.2; on 14.0–15.1 the card says "A window" / "An app" / "A display". The
  accessors are sent only when the filter answers to them.

**One shell core (2026-09-23, ahead of MB3).** MB3 is where the macOS app
first broadcasts, and a broadcast needs everything the Windows shell's
`main.rs` does around the pipeline — settings and server profiles, the
identity latch, rooms, the engine-event handling, resume re-prime, stats,
diagnostics — about 1,500 lines, none of it Windows-specific except at the
handful of `#[cfg(windows)]` sites that reached into the pipeline. Rather
than a second copy, that code moved verbatim into `gawk-ui` as
`gawk_ui::shell`, with exactly two seams: `Platform` (resolve a Start from
the picker, notifications, credentials, the platform's own callbacks and
ticks) and `Media` (the running pipeline — the calls the old `cfg(windows)`
sites made: re-prime, thumbnail, capture fps, audio state/level/hint, the
audio switch, minimized, failure, shutdown). `messages`, `diagnostics` and
`debuglog` moved with it; the diagnostics `kind` and the refusal sentence
("hardware-encodes fine on Windows" / "on a Mac", D7) follow
`defaults::THIS`. `app-windows` is now its platform only: the WGC picker
card, the Media Foundation pipeline behind `Media`, toasts, DPAPI. The one
behaviour change is on non-Windows dev hosts, where the Windows binary now
refuses Start before dialing instead of after. The quit path gained a
final bounded stop after the event loop returns, for ⌘Q, which ends the
loop without the close dialog.

**MB3 (2026-09-23)** — how the encode landed, and what the hardware said:

- **V-3 and V-9 answered on an Apple M1, macOS 26.5.2.** The D7 trial —
  `crates/encode/src/vt.rs`'s `the_trial_passes_on_this_mac`, run with
  `--ignored` — creates the low-latency hardware session, and the forced-IDR
  cadence is honoured under `EnableLowLatencyRateControl` (V-3: IDRs at
  frame 0, at the 30-frame boundary and at the forced frame). `420v`
  IOSurface-backed input is accepted directly (V-9, with the trial's own
  buffers; SCK's are the same format and backing). No B-frames, VFR
  pass-through and SPS/PPS before every IDR all hold. Codec string:
  `avc1.64002A` (High, level 4.2) at 1080p60 12 Mbps. The BGRA fallback is
  therefore not built; it stays pre-registered.
- **Required vs advisory properties.** `RealTime`, `AllowFrameReordering`,
  `ProfileLevel` and `AverageBitRate` must be accepted or the trial rejects
  the session. `DataRateLimits`, `ExpectedFrameRate` and the two
  `MaxKeyFrameInterval*` keys are set but a refusal is logged, not fatal:
  the cadence never depended on the GOP keys (low-latency mode documents
  them as ignored), and the peak cap is V-6's to measure — refusing to
  broadcast over it would trade a bitrate question for a dead app. All were
  accepted on the M1.
- **The encoder is fed session-clock timestamps.** The host PTS is mapped
  once at capture (D5), and VideoToolbox returns the PTS it was given, so
  output needs no second mapping — the `AccessUnit` timestamp is the input
  time in µs.
- **SPS/PPS come from the format description on every IDR.** The output
  callback copies the AVCC bytes once (D7's zero-copy note), rewrites them
  with the shared `h264::avcc_to_annex_b` and prepends the description's
  parameter sets through the shared `cascade::ensure_idr_headers` — the same
  helper the Windows path uses for vendors that omit in-band headers. A
  malformed AVCC unit is refused whole, never truncated.
- **End to end against the real relay** (`crates/encode/tests/vt_to_relay.rs`,
  ignored by default, macOS only): VideoToolbox's real output through the
  engine to a real `gawk-server`, a rolling restart, the reclaim of the same
  code, then `force_idr` — the next AU is an IDR with its parameter sets
  and reaches the restarted relay. That is MB3's "forced IDR on resume"
  criterion; the shell calls `Media::force_idr` on `Resumed`. The real-relay
  harness moved to `crates/engine/tests/support/relay.rs` so the two suites
  share it.
- **D12's config file landed early**, because MB3's broadcasts go through
  the shared shell, which loads and saves it: `~/Library/Application
  Support/gawk/broadcast.json`, and on every Unix the file is now written
  mode 0600 (it was the umask's 0644 — on Linux dev hosts too). Plaintext
  credentials, no Keychain, as D12 decided.
- **Not yet:** notifications go to the debug log until MB5 brings
  `UNUserNotificationCenter`; audio is off until MB4 (a video-only
  broadcast, byte-identical to audio-off on the wire); `VTCopyVideoEncoderList`
  is not consulted — D7 wanted it only for the refusal's wording, which does
  not need it. The trial and the relay test need hardware, so the
  `macos-latest` job runs neither; they are for a Mac.

**MB4 (2026-09-23)** — audio, and where D1's module plan bent:

- **No `audio::sck` module.** D1 drew one, but the audio arrives on the
  `SCStream` the capture crate owns (D6: one stream), so the framework half
  is `capture::sck`'s — a second output on its own serial queue, each buffer
  lent as an `AudioBlock` (its ASBD fields and planes) — and the audio
  crate's half is portable: `audio::pcm` (the ASBD → interleaved stereo
  shim: Float32 or Int16, planar or interleaved, mono duplicated; anything
  not 48 kHz refused, not resampled) and `audio::lane` (framer → libopus →
  TOC check, R25's contract untouched). Both are host tests.
- **D6's probe is the lane's first packet.** SCK cannot run an audio-only
  stream, and D6 made the probe the first moments of the real stream's
  audio. `Lane` advertises the AudioConfig only with the first packet that
  passes the TOC check, so audio that fails before producing one leaves the
  wire byte-identical to a video-only broadcaster; any failure is sticky and
  reported once. Unit-tested for both the probe and the live case.
- **Audio has its own panic fence.** The first cut shared video's
  `CallbackGuard`, which would have ended the broadcast on an audio panic —
  against D6. A second guard stops audio only (`Capture::audio_failed`, the
  audio line reads "error"). A review of the PR found the second half: the
  audio queue holds the lane's mutex while feeding it, so a caught panic
  still poisoned it and the GUI's next level read would `unwrap()` the
  poison on the UI thread. The GUI reads now treat a poisoned lane as
  silence; a regression test poisons it and reads.
- **One clock (D5).** Audio maps its host PTS through the same `QpcMapper`
  instance as video, so A/V skew is zero by construction; a buffer without
  a time is stamped on arrival minus its own duration.
- **"Use whole-system audio"** (D6's hint) re-runs the system picker in
  display mode for the live stream (`presentPickerForStream:usingContentStyle:`);
  the new display filter swaps the audio scope on the same stream — same
  Opus stream, same seq space. The shell's audio line keeps saying "App
  audio" after that switch (its capture mode is fixed at Start); MB5's shell
  work is where that label follows the filter.
- **Owner-pending:** G1 and V-4 (mode-1 isolation against ≥ 2 real games,
  one with a launcher/helper process), G2 (whole-system audio across an
  output-device switch), and G7's `avSkewMs` on a live viewer.

**MB5 (2026-09-23)** — most of the shell arrived with the shared core
(settings, rooms, the lifecycle, stats, diagnostics, the D12 config file);
what MB5 itself added, and how:

- **The menu bar.** On macOS Slint installs a default application menu —
  About (the bundle's name and `CFBundleShortVersionString`), Services,
  Hide, Quit ⌘Q — so D11's Quit and About come from it, and ⌘Q was already
  what MB1's owner pass exercised. What it lacks, **Settings… ⌘,**, is a
  `MenuBar` in the shared window that exists only in picker mode (Windows
  has no menu bar); Slint maps its `Control` modifier to ⌘ on Apple
  platforms. The item opens the Settings card.
- **Notifications** through `UNUserNotificationCenter`, docs/38 D12's two
  urgencies mapped to an active banner, with the default sound for the
  critical one; a delegate shows banners while the app is frontmost, as the
  Windows toasts do. The center throws an Objective-C exception in a process
  without a bundle identifier, so notifications are enabled only when
  running as the `.app` (D14); every one is also a debug-log line. No
  time-sensitive delivery (an entitlement D13 does not request); how they
  fare under Focus is V-8's. `app-macos::notify` is the one module outside
  D3's three with `unsafe` in it, for the delegate class.
- **The Share card holds the thumbnail** (D11) — the separate "what viewers
  see" card stays the Windows layout.
- **The audio label follows a live re-pick.** `Media::capture_mode` (a
  defaulted trait method; Windows keeps the mode Start resolved) lets the
  macOS pipeline report the mode its current filter implies, so after
  "Use whole-system audio" the line reads "System audio" and the per-app
  silence hint stops.
- **Owner-pending:** the on-screen criteria (all D11 cards, the
  Idle/Starting/Live/amber flow, picker → start → code → copy link, ⌘Q ends
  a live broadcast cleanly), V-8 (notifications under Focus during a
  fullscreen game), and idle CPU ~0 %.

**MB6 (2026-09-23)** — half of it cannot exist yet:

- **Telemetry is done by construction.** The shared shell's reporter
  answers `TelemetryHello`, batches with the browser field names, and
  reports `browser: gawk-broadcast-macos`, `os: macOS` from
  `defaults::THIS` (MB1); the diagnostics dump's `kind` follows the same
  constant (unit-tested). A real session landing in the production
  dashboard is the owner's to confirm.
- **The update notice and the in-place install are blocked.** D17 builds on
  R45's `engine::update` and R47's install flow; neither milestone has
  started (docs/47, docs/48 — both "not started", and the Windows app has
  neither either). What R52 contributes is ready for them: the macOS
  distribution name, asset name and manifest path are constants in
  `engine::defaults` (MB1), docs/47 D5 names the macOS row, and MB7 writes
  `releases/gawk-broadcast-macos/latest.json`. The D17 bundle-swap design
  stands; it lands with R47.

**MB7 (2026-09-23)** — the release path, and where it bent:

- **Signing happens in the `macos` job, on `push` and dispatch only, and
  only when the secrets exist.** D15 had the attach job build and sign;
  the `macos` job already builds the bundle on every push, so it signs
  there instead: import the `.p12` into a throwaway keychain, discover the
  Developer ID identity (masked, never printed), `bundle.sh` with
  `SIGN_IDENTITY`, `notarytool submit --wait` with the App Store Connect key,
  `stapler staple`, then `codesign --verify --deep --strict` and
  `spctl --assess` — MB7's first criterion, run on every signed build.
  PRs never sign.
- **An unsigned bundle is never released — and Windows does not wait for
  Apple credentials.** With the signing secret configured, the attach job
  refuses a bundle whose `BUILD-INFO.txt` is not "Developer ID + notarized"
  (fails loudly, D13). Without it — the state today — the attach job
  releases the Windows EXE alone, warns, and writes only the Windows
  manifest: blocking every Windows release on an Apple account would have
  been the literal reading of D13 and the wrong one. Configuring the five
  secrets is what turns the macOS distribution on. The same holds for the
  `macos` job itself: `attach-release` depends on it softly (`!cancelled()`
  plus hard checks on lint/test/build), so a macOS flake, hosted-image drift
  or a backfill of a commit older than the bundle releases Windows alone
  with a warning instead of skipping the release — the review of the MB7 PR
  caught the first cut making the whole Windows release wait on it.
- **The release set.** `gawk-broadcast-macos-arm64.zip` (the stapled
  bundle, `ditto`-zipped), `BUILD-INFO-macos.txt`,
  `THIRD-PARTY-NOTICES-macos.md` beside the EXE and its files, one
  `SHA256SUMS` over all of them, and a second manifest
  (`releases/gawk-broadcast-macos/latest.json`). R47's minisign step, when
  it exists, signs the one `SHA256SUMS`.
- **macOS has its own THIRD-PARTY-NOTICES** — `THIRD-PARTY-NOTICES-macos.md`,
  generated for `aarch64-apple-darwin` by the same `gen-notices.py`, checked
  by the same freshness job (its glob now covers it). Doing that exposed a
  host dependence: the generator walked proc-macro crates, whose own
  dependencies resolve for the build host, so the Windows file came out
  different on a Mac than on the Linux CI runner. It now passes
  `no-proc-macro` — which is what its "build-dependencies are excluded"
  meant — and the Windows notices shrink from 377 to 267 packages, the
  difference being compile-time-only crates that were never in the EXE.
- **The site's macOS card stays hidden until its manifest exists**
  (`data-dl-until-released`): before the first signed release it would
  point at releases with no Mac asset. `docs/self-hosting.md` lists
  `gawk-broadcast://macos`; docs/46 carries the dated
  "component means distribution" note.
- **The owner's to do:** create the Developer ID Application certificate and
  an App Store Connect API key (Developer role) in the Apple Developer
  account, and add the five repository secrets D13 names
  (`APPLE_SIGNING_CERTIFICATE_P12` base64, `APPLE_SIGNING_CERTIFICATE_PASSWORD`,
  `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID`, `APPLE_NOTARY_KEY_P8`).
  The next push to `main` touching the workspace then produces a signed,
  notarized artifact, and the next desktop release carries it.

## 12. References

Apple documentation and sessions the decisions cite (retrieved 2026-09-18):

- ScreenCaptureKit: [Meet ScreenCaptureKit (WWDC22)](https://developer.apple.com/videos/play/wwdc2022/10156/),
  [Take ScreenCaptureKit to the next level (WWDC22)](https://developer.apple.com/videos/play/wwdc2022/10155/),
  [What's new in ScreenCaptureKit (WWDC23, the picker)](https://developer.apple.com/videos/play/wwdc2023/10136/),
  [`SCStreamConfiguration.queueDepth`](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/queuedepth),
  [`pixelFormat`](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/pixelformat),
  [`SCFrameStatus`](https://developer.apple.com/documentation/screencapturekit/scframestatus),
  [`SCContentSharingPicker`](https://developer.apple.com/documentation/screencapturekit/sccontentsharingpicker),
  [`captureDynamicRange` (macOS 15)](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/capturedynamicrange),
  [`captureMicrophone` (macOS 15)](https://developer.apple.com/documentation/screencapturekit/scstreamconfiguration/capturemicrophone).
- Core Audio taps (fallback): [Capturing system audio with Core Audio taps](https://developer.apple.com/documentation/CoreAudio/capturing-system-audio-with-core-audio-taps),
  [`CATapDescription`](https://developer.apple.com/documentation/coreaudio/catapdescription).
- VideoToolbox: [`kVTVideoEncoderSpecification_EnableLowLatencyRateControl`](https://developer.apple.com/documentation/videotoolbox/kvtvideoencoderspecification_enablelowlatencyratecontrol),
  [Explore low-latency video encoding with VideoToolbox (WWDC21)](https://developer.apple.com/videos/play/wwdc2021/10158/),
  [`kVTCompressionPropertyKey_RealTime`](https://developer.apple.com/documentation/videotoolbox/kvtcompressionpropertykey_realtime),
  [Apple Developer Forums: rate-control modes under low-latency encoding](https://developer.apple.com/forums/thread/799459).
- AV1: [Apple newsroom, M5 Pro / M5 Max (2026-03-03)](https://www.apple.com/newsroom/2026/03/apple-debuts-m5-pro-and-m5-max-to-supercharge-the-most-demanding-pro-workflows/) — "AV1 decode", no encode.
- Signing and Gatekeeper: [`com.apple.developer.persistent-content-capture`](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.persistent-content-capture)
  (the restricted entitlement this design does not request),
  [Sequoia removes the Gatekeeper contextual-menu override](https://mjtsai.com/blog/2024/07/05/sequoia-removes-gatekeeper-contextual-menu-override/),
  [Sequoia screen-recording prompts and the persistent-content-capture entitlement](https://mjtsai.com/blog/2024/08/08/sequoia-screen-recording-prompts-and-the-persistent-content-capture-entitlement/),
  [Homebrew: casks do not bypass Gatekeeper](https://docs.brew.sh/Homebrew-Security-and-Supply-Chain).
- Rust: [objc2 cross-compiling notes](https://docs.rs/objc2/latest/objc2/topics/cross_compiling/index.html),
  [quinn (tested on macOS)](https://github.com/quinn-rs/quinn),
  [cidre (the named fallback)](https://github.com/yury/cidre).
- CI: [GitHub-hosted runners (macOS labels; free on public repositories)](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
