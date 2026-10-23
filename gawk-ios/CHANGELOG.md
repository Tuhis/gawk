# Changelog

## [1.4.0](https://github.com/Tuhis/gawk/compare/gawk-ios/v1.3.0...gawk-ios/v1.4.0) (2026-10-23)


### Features

* **r74:** PoP relays, a one-click LAN relay that federates with the home fleet ([#553](https://github.com/Tuhis/gawk/issues/553)) ([a60eac9](https://github.com/Tuhis/gawk/commit/a60eac9cfd0d189d451c1e13c48a923317209b8c))


### Bug Fixes

* **broadcast-desktop:** fix an mmdevapi.dll crash and a BBR estimate that never fell ([#554](https://github.com/Tuhis/gawk/issues/554)) ([cdd6c35](https://github.com/Tuhis/gawk/commit/cdd6c35e3a0ffb88e1b51527ce73fcf14d4f836d))

## [1.3.0](https://github.com/Tuhis/gawk/compare/gawk-ios/v1.2.0...gawk-ios/v1.3.0) (2026-10-22)


### Features

* **broadcast-desktop:** reliable uplink delivery and BBR behind settings (R73 RU3–RU5) ([#541](https://github.com/Tuhis/gawk/issues/541)) ([e3b7208](https://github.com/Tuhis/gawk/commit/e3b720842b971d4bdcab19fddce26e61a0ab7e73))


### Bug Fixes

* **telemetry:** see leg-A loss in frames and stop blaming leg B for it (R73 RU0) ([#539](https://github.com/Tuhis/gawk/issues/539)) ([d32a506](https://github.com/Tuhis/gawk/commit/d32a506a75b4e8b7732653618e46206a8b28629e))

## [1.2.0](https://github.com/Tuhis/gawk/compare/gawk-ios/v1.1.1...gawk-ios/v1.2.0) (2026-10-20)


### Features

* show open-source licences in the desktop and iOS apps (R72) ([#533](https://github.com/Tuhis/gawk/issues/533)) ([9d8837e](https://github.com/Tuhis/gawk/commit/9d8837e95356ade8643ffae9c3658d4d745eccc9))

## [1.1.1](https://github.com/Tuhis/gawk/compare/gawk-ios/v1.1.0...gawk-ios/v1.1.1) (2026-10-19)


### Bug Fixes

* **ios:** the content screening mark is static text to the accessibility audit ([#512](https://github.com/Tuhis/gawk/issues/512)) ([f857ed6](https://github.com/Tuhis/gawk/commit/f857ed6c93fe4acfc5685a69e99a857d0b1234db))

## [1.1.0](https://github.com/Tuhis/gawk/compare/gawk-ios/v1.0.0...gawk-ios/v1.1.0) (2026-10-14)


### Features

* **r40:** automatic content flagging — gawk-sampler, content flags and the screening notice (CF0–CF8) ([#500](https://github.com/Tuhis/gawk/issues/500)) ([99cf6d4](https://github.com/Tuhis/gawk/commit/99cf6d41c07803f7d5ce60716b1f7f99ba6be560))

## 1.0.0 (2026-10-07)


### Features

* **ios:** R65 IO1 — gawk-ios scaffolding ([#458](https://github.com/Tuhis/gawk/issues/458)) ([3497d96](https://github.com/Tuhis/gawk/commit/3497d969093a63efd38d103ee27f12ddeb306d3c))
* **ios:** R65 IO2 — the broadcast pipeline, capture and the test source ([#463](https://github.com/Tuhis/gawk/issues/463)) ([2cb5bc7](https://github.com/Tuhis/gawk/commit/2cb5bc771f693c82c189b228f747ff3623b89f83))
* **ios:** R65 IO3 — the Broadcast and Settings screens ([#465](https://github.com/Tuhis/gawk/issues/465)) ([0550b2e](https://github.com/Tuhis/gawk/commit/0550b2ec610ec215c40c37eace2694da95d90efb))
* **ios:** R65 IO4 — the Rust viewer core ([#459](https://github.com/Tuhis/gawk/issues/459)) ([ffcd455](https://github.com/Tuhis/gawk/commit/ffcd455d58615ead92e82a6ec9ffad2a983ec670))
* **ios:** R65 IO5 — the native player and the Watch screen ([#466](https://github.com/Tuhis/gawk/issues/466)) ([c1fc034](https://github.com/Tuhis/gawk/commit/c1fc0340d901c9b3fd9d17bdf425920b631170c1))
* **ios:** R65 IO6/IO7 — rooms, links and broadcaster telemetry in the core ([#464](https://github.com/Tuhis/gawk/issues/464)) ([0892a55](https://github.com/Tuhis/gawk/commit/0892a559ea7cb71cce0f0d41abefd1065c56780a))
* **ios:** R68 — the iOS app UX redesign (IX1–IX9) ([#475](https://github.com/Tuhis/gawk/issues/475)) ([a34495b](https://github.com/Tuhis/gawk/commit/a34495be592438b22929ee12a10c91d807940f81))
* **ios:** the viewer's UniFFI bridge and a fixture publisher (R65 IO5, docs/67 D3/D15/D16) ([#462](https://github.com/Tuhis/gawk/issues/462)) ([2769e98](https://github.com/Tuhis/gawk/commit/2769e9882f359db5afa625152c30c27214b83ed1))


### Bug Fixes

* **broadcast-desktop:** restart a stuck relay dial on a fresh endpoint ([#470](https://github.com/Tuhis/gawk/issues/470)) ([1139ba8](https://github.com/Tuhis/gawk/commit/1139ba87b10cd87a342a48996b8d3f50f9ca3c31))
* **ios:** bump gawk-devpub's lock entry with each release ([#476](https://github.com/Tuhis/gawk/issues/476)) ([540ba8c](https://github.com/Tuhis/gawk/commit/540ba8cf7f9e1cfdfa724406b15c4cd8212cfc49))
* **ios:** the room grid fits the screen instead of scrolling ([#483](https://github.com/Tuhis/gawk/issues/483)) ([a3c6dc5](https://github.com/Tuhis/gawk/commit/a3c6dc58b7045fa71344784db102a08801159c92))
* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))
