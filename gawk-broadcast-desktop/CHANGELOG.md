# Changelog

## [2.6.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.5.0...gawk-broadcast-desktop/v2.6.0) (2026-10-07)


### Features

* **broadcast-desktop:** open the source picker on apps, remember its tab, pick on click ([#481](https://github.com/Tuhis/gawk/issues/481)) ([d6f81c1](https://github.com/Tuhis/gawk/commit/d6f81c13f5f3763277b78069cd9294070486f1aa))
* **ios:** R65 IO1 — gawk-ios scaffolding ([#458](https://github.com/Tuhis/gawk/issues/458)) ([3497d96](https://github.com/Tuhis/gawk/commit/3497d969093a63efd38d103ee27f12ddeb306d3c))
* **ios:** R65 IO2 — the broadcast pipeline, capture and the test source ([#463](https://github.com/Tuhis/gawk/issues/463)) ([2cb5bc7](https://github.com/Tuhis/gawk/commit/2cb5bc771f693c82c189b228f747ff3623b89f83))
* **ios:** R65 IO3 — the Broadcast and Settings screens ([#465](https://github.com/Tuhis/gawk/issues/465)) ([0550b2e](https://github.com/Tuhis/gawk/commit/0550b2ec610ec215c40c37eace2694da95d90efb))
* **ios:** R65 IO4 — the Rust viewer core ([#459](https://github.com/Tuhis/gawk/issues/459)) ([ffcd455](https://github.com/Tuhis/gawk/commit/ffcd455d58615ead92e82a6ec9ffad2a983ec670))
* **ios:** R65 IO5 — the native player and the Watch screen ([#466](https://github.com/Tuhis/gawk/issues/466)) ([c1fc034](https://github.com/Tuhis/gawk/commit/c1fc0340d901c9b3fd9d17bdf425920b631170c1))
* **ios:** R65 IO6/IO7 — rooms, links and broadcaster telemetry in the core ([#464](https://github.com/Tuhis/gawk/issues/464)) ([0892a55](https://github.com/Tuhis/gawk/commit/0892a559ea7cb71cce0f0d41abefd1065c56780a))
* **ios:** R68 — the iOS app UX redesign (IX1–IX9) ([#475](https://github.com/Tuhis/gawk/issues/475)) ([a34495b](https://github.com/Tuhis/gawk/commit/a34495be592438b22929ee12a10c91d807940f81))


### Bug Fixes

* **broadcast-desktop:** restart a stuck relay dial on a fresh endpoint ([#470](https://github.com/Tuhis/gawk/issues/470)) ([1139ba8](https://github.com/Tuhis/gawk/commit/1139ba87b10cd87a342a48996b8d3f50f9ca3c31))
* **broadcast-desktop:** stop the relaunch-wait test flaking on a loaded runner ([#461](https://github.com/Tuhis/gawk/issues/461)) ([eabc365](https://github.com/Tuhis/gawk/commit/eabc365916f4918e2ce6ebbcd4be6e0e3c08bf02))

## [2.5.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.4.1...gawk-broadcast-desktop/v2.5.0) (2026-10-04)


### Features

* **broadcast-desktop:** open gawk:// links in the desktop broadcaster (R66) ([#452](https://github.com/Tuhis/gawk/issues/452)) ([36f5783](https://github.com/Tuhis/gawk/commit/36f5783fd15a98e1fc42eac191968da45837da61))

## [2.4.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.4.0...gawk-broadcast-desktop/v2.4.1) (2026-10-02)


### Bug Fixes

* **broadcast-desktop:** make the in-place update download work and show the notice at the top ([#441](https://github.com/Tuhis/gawk/issues/441)) ([4bcd143](https://github.com/Tuhis/gawk/commit/4bcd143481b00f42b4cb66b11cb683fd0b6048be))

## [2.4.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.3.0...gawk-broadcast-desktop/v2.4.0) (2026-10-02)


### Features

* **broadcast-desktop:** pinned action bar, window fit and a remembered window size (R64) ([#433](https://github.com/Tuhis/gawk/issues/433)) ([7a4c6f3](https://github.com/Tuhis/gawk/commit/7a4c6f3ffe68699c0920ac5d0752fb0ba41cd19f))

## [2.3.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.2.0...gawk-broadcast-desktop/v2.3.0) (2026-10-01)


### Features

* **broadcast-desktop:** source preview on Ready and for Windows display shares (R63) ([#431](https://github.com/Tuhis/gawk/issues/431)) ([d3a5df6](https://github.com/Tuhis/gawk/commit/d3a5df6cc9373ec96569bb3e3ebbda0dc322a619))


### Bug Fixes

* **broadcast-desktop:** copy the code and link on one click, clear "Copied" ([#427](https://github.com/Tuhis/gawk/issues/427)) ([2a7492b](https://github.com/Tuhis/gawk/commit/2a7492b9e24f631b1da05f014306f022470d058e))

## [2.2.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.1.0...gawk-broadcast-desktop/v2.2.0) (2026-10-01)


### Features

* **broadcast-desktop:** desktop UX pass 2 (R62) ([#423](https://github.com/Tuhis/gawk/issues/423)) ([550d316](https://github.com/Tuhis/gawk/commit/550d316adeb61fd2d2df8282c4b260640c14e05f))
* **broadcast-desktop:** signed in-place update (R47) ([#421](https://github.com/Tuhis/gawk/issues/421)) ([e71ba49](https://github.com/Tuhis/gawk/commit/e71ba490008f10ef21942b60e6a653bb8c7b5ab9))

## [2.1.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v2.0.0...gawk-broadcast-desktop/v2.1.0) (2026-09-30)


### Features

* **broadcast-desktop:** check for updates every 15 minutes, add Check now; keep Cargo.lock in step with releases ([#415](https://github.com/Tuhis/gawk/issues/415)) ([bd567e5](https://github.com/Tuhis/gawk/commit/bd567e5dcd30d92d456e28112a7b90bf0c84d88e))
* **broadcast-desktop:** ship the Linux broadcaster as a .deb too (R61) ([#406](https://github.com/Tuhis/gawk/issues/406)) ([037a66e](https://github.com/Tuhis/gawk/commit/037a66e99ad88d38d7e4f6ebeaa98328849c7c1f))
* **broadcast-desktop:** tell the broadcaster when a newer release exists (R45) ([#414](https://github.com/Tuhis/gawk/issues/414)) ([d35e09a](https://github.com/Tuhis/gawk/commit/d35e09a4fa5494b163507c9b8e01c0089c592da1))


### Bug Fixes

* **broadcast-desktop:** give the macOS app bundle its Dock icon ([#405](https://github.com/Tuhis/gawk/issues/405)) ([953577e](https://github.com/Tuhis/gawk/commit/953577ef84b5c6fc2ed47ea91ecd9ddbeeca488a))

## [2.0.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.8.0...gawk-broadcast-desktop/v2.0.0) (2026-09-29)


### Features

* **broadcast-desktop:** Linux shell in the desktop workspace (R56 LX1–LX6) ([#398](https://github.com/Tuhis/gawk/issues/398)) ([c00b8c4](https://github.com/Tuhis/gawk/commit/c00b8c4dcc66e2a086c580649425de40880923df))

## [1.8.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.7.0...gawk-broadcast-desktop/v1.8.0) (2026-09-29)


### Features

* **broadcast-desktop:** notice Wi-Fi packet loss on macOS and point at the AWDL fix (R55 WU3) ([#396](https://github.com/Tuhis/gawk/issues/396)) ([23d37e7](https://github.com/Tuhis/gawk/commit/23d37e784882932d41a1d209320dad0fab7f74f7))

## [1.7.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.6.0...gawk-broadcast-desktop/v1.7.0) (2026-09-29)


### Features

* **r59:** usage and capacity metrics for the gawk.ioio.fi dashboard ([#387](https://github.com/Tuhis/gawk/issues/387)) ([cb9d188](https://github.com/Tuhis/gawk/commit/cb9d1884a19092b38213bf6edeec5cad2c3805d4))

## [1.6.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.5.2...gawk-broadcast-desktop/v1.6.0) (2026-09-28)


### Features

* **broadcast-desktop:** redesigned window — pages, gawk's look, rooms in the app (R58) ([#381](https://github.com/Tuhis/gawk/issues/381)) ([c837de4](https://github.com/Tuhis/gawk/commit/c837de4a5dc0ef7d387f48064dfa7857e92bde0b))


### Bug Fixes

* **broadcast-desktop:** no upscaling of small windows, no fps-gate drops on high-refresh displays ([#385](https://github.com/Tuhis/gawk/issues/385)) ([6129f39](https://github.com/Tuhis/gawk/commit/6129f3906595651aa2f0e7a078a789f8cdae9c9f))

## [1.5.2](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.5.1...gawk-broadcast-desktop/v1.5.2) (2026-09-27)


### Bug Fixes

* **r57:** close codes Chrome can read, and rooms that end cleanly ([#375](https://github.com/Tuhis/gawk/issues/375)) ([41fef4d](https://github.com/Tuhis/gawk/commit/41fef4d1ce6d7864c21d04e29dd27324bf24dfde))

## [1.5.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-desktop/v1.5.0...gawk-broadcast-desktop/v1.5.1) (2026-09-23)


### Bug Fixes

* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))

## [1.5.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows/v1.4.0...gawk-broadcast-windows/v1.5.0) (2026-09-20)


### Features

* **broadcasters:** R44 — app icons for the native broadcasters (IC1–IC5) ([#328](https://github.com/Tuhis/gawk/issues/328)) ([2de0c20](https://github.com/Tuhis/gawk/commit/2de0c20cf310cb1a42062eb1d259fd4561c0c2f3))

## [1.4.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows/v1.3.0...gawk-broadcast-windows/v1.4.0) (2026-09-14)


### Features

* **broadcasters:** nickname names the room tile and can change while live ([#316](https://github.com/Tuhis/gawk/issues/316)) ([067bc3d](https://github.com/Tuhis/gawk/commit/067bc3df1f01796f002ac55d6b67bb3f80e971f5))

## [1.3.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows/v1.2.0...gawk-broadcast-windows/v1.3.0) (2026-09-10)


### Features

* **r42:** rooms — multi-POV rooms over unchanged broadcasts (RM1–RM9) ([#302](https://github.com/Tuhis/gawk/issues/302)) ([e666f74](https://github.com/Tuhis/gawk/commit/e666f741d0b22624b9313b88d4dece022890200b))

## [1.2.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows/v1.1.1...gawk-broadcast-windows/v1.2.0) (2026-08-31)


### Features

* **r39:** admin portal for moderation ([#280](https://github.com/Tuhis/gawk/issues/280)) ([054d70b](https://github.com/Tuhis/gawk/commit/054d70b859ee98475654e6f6ea960b51e38b10af))

## [1.1.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows-v1.1.0...gawk-broadcast-windows/v1.1.1) (2026-08-19)


### Bug Fixes

* reap orphaned stripe legs via viewer-owned session groups and a liveness lease ([#259](https://github.com/Tuhis/gawk/issues/259)) ([dae116e](https://github.com/Tuhis/gawk/commit/dae116e87a41ce90992d56d11a1e9c77c0ef32c6))

## [1.1.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows-v1.0.0...gawk-broadcast-windows-v1.1.0) (2026-08-01)


### Features

* **broadcasters:** show the build version in both native broadcaster windows ([#214](https://github.com/Tuhis/gawk/issues/214)) ([aece3fd](https://github.com/Tuhis/gawk/commit/aece3fdce6b598b506428ffcd4564f68ee0adf18))
* **gawk-broadcast-windows:** 12 Mbps default, aspect-preserving encode, custom resolution, uplink warning ([2986a7f](https://github.com/Tuhis/gawk/commit/2986a7ffc842204b0648b4cb0e0082642d0bd78f))
* **gawk-broadcast-windows:** write debug.log so encoder refusals are diagnosable (F-8) ([10de84d](https://github.com/Tuhis/gawk/commit/10de84d76ad47bf878d17399adbd2adf7df618c4))


### Bug Fixes

* **gawk-broadcast-windows:** keyframe supersede livelock left joining viewers black (F-12) ([73b0adb](https://github.com/Tuhis/gawk/commit/73b0adb53e28abb583bd6c7fb33ee280b46b13a8))
* **gawk-broadcast-windows:** negotiate the NV12 input type from the MFT itself (F-9) ([3a3ad16](https://github.com/Tuhis/gawk/commit/3a3ad16fe92d23209ceb826aee7af2fdef49ee55))
* **gawk-broadcast-windows:** stack-blob PROPVARIANT heap corruption; contain panics; remove capture border (F-10/F-11) ([7c6a376](https://github.com/Tuhis/gawk/commit/7c6a376a87258c29ed7130f3711ce47eb83a3e01))

## 1.0.0 (2026-07-31)


### Features

* R34 native Windows broadcaster — WB0–WB8 ([#208](https://github.com/Tuhis/gawk/issues/208)) ([ec20a50](https://github.com/Tuhis/gawk/commit/ec20a5051ec78781fcfd285eea97e108deeef318))


### Bug Fixes

* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))
