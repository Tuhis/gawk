# Changelog

## [1.9.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.8.1...gawk-broadcast-v1.9.0) (2026-07-29)


### Features

* **r30:** large-frame e2e fixture + striped browser passes; fix recovery-raced phantom frames ([#194](https://github.com/Tuhis/gawk/issues/194)) ([d135f74](https://github.com/Tuhis/gawk/commit/d135f7438e49cfe2881c727cc566ba73eb9ebecd))
* **r30:** striped delivery — ST2–ST6 (wire, relay legs, viewer transport, controller, observability) ([#191](https://github.com/Tuhis/gawk/issues/191)) ([bf8db99](https://github.com/Tuhis/gawk/commit/bf8db99d901e8b4e47c69c8283b9f1bbfface44e))

## [1.8.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.8.0...gawk-broadcast-v1.8.1) (2026-07-28)


### Bug Fixes

* **r29:** give the viewer a datagram receive queue deep enough for a frame ([#180](https://github.com/Tuhis/gawk/issues/180)) ([d735df7](https://github.com/Tuhis/gawk/commit/d735df701a75bfcab2a9884d495aedc3723ff62b))

## [1.8.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.7.1...gawk-broadcast-v1.8.0) (2026-07-28)


### Features

* **r29:** forward parity for live-edge delivery ([#175](https://github.com/Tuhis/gawk/issues/175)) ([9bca5d9](https://github.com/Tuhis/gawk/commit/9bca5d9a7a080e4fa9c51733fbc16231b4f84555))

## [1.7.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.7.0...gawk-broadcast-v1.7.1) (2026-07-27)


### Bug Fixes

* give reliable viewers' audio its own carrier, and make the native broadcaster visible to telemetry ([#173](https://github.com/Tuhis/gawk/issues/173)) ([0758d66](https://github.com/Tuhis/gawk/commit/0758d668948a12e287428f4037dc32e9ba822595))

## [1.7.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.6.0...gawk-broadcast-v1.7.0) (2026-07-27)


### Features

* **r25:** give the native Linux broadcaster system audio (docs/28 NA2–NA7) ([#169](https://github.com/Tuhis/gawk/issues/169)) ([b4e0d19](https://github.com/Tuhis/gawk/commit/b4e0d198ef91f9f34833b1664d8a7658850c17d6))
* **r28:** default the native broadcaster to the ioio fleet, telemetry included ([#168](https://github.com/Tuhis/gawk/issues/168)) ([aa17ad9](https://github.com/Tuhis/gawk/commit/aa17ad9a7b6aa4be1764cdbd3890a1d5ebc24255))


### Bug Fixes

* **r14:** stop the GUI free-running at 20-30% CPU while idle ([#167](https://github.com/Tuhis/gawk/issues/167)) ([575218e](https://github.com/Tuhis/gawk/commit/575218e8c20c5dde18a770248a2b7efef6754abf))

## [1.6.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.5.0...gawk-broadcast-v1.6.0) (2026-07-26)


### Features

* **r28:** advanced diagnostics & telemetry (docs/33 TM1–TM8) ([#151](https://github.com/Tuhis/gawk/issues/151)) ([034ad97](https://github.com/Tuhis/gawk/commit/034ad97496918c21a6f9242b3f9b291461436088))

## [1.5.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.4.1...gawk-broadcast-v1.5.0) (2026-07-24)


### Features

* **r23:** terms & conditions surface + broadcaster acknowledgment ([#129](https://github.com/Tuhis/gawk/issues/129)) ([1b855f9](https://github.com/Tuhis/gawk/commit/1b855f97d8caea538cf6fb15099eac7b1d1c8adc))

## [1.4.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.4.0...gawk-broadcast-v1.4.1) (2026-07-23)


### Bug Fixes

* stop sending DVR subscribers a second, live keyframe timeline ([#112](https://github.com/Tuhis/gawk/issues/112)) ([7de0d18](https://github.com/Tuhis/gawk/commit/7de0d18f1bcb9fc55e75c3da1dbf1ff32a399cb0))

## [1.4.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.3.1...gawk-broadcast-v1.4.0) (2026-07-23)


### Features

* relay DVR ring so resilient viewers ride out dropouts (R21) ([#111](https://github.com/Tuhis/gawk/issues/111)) ([b92de77](https://github.com/Tuhis/gawk/commit/b92de77f8a2e9fbeaf22a05a112d89e89cdf88b5))

## [1.3.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.3.0...gawk-broadcast-v1.3.1) (2026-07-21)


### Bug Fixes

* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))

## [1.3.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.2.0...gawk-broadcast-v1.3.0) (2026-07-19)


### Features

* **r15:** system audio — Opus over datagrams (experimental) ([#64](https://github.com/Tuhis/gawk/issues/64)) ([8cefcb7](https://github.com/Tuhis/gawk/commit/8cefcb73b63f442c41a5b05421a99426feda4ee5))

## [1.2.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.1.0...gawk-broadcast-v1.2.0) (2026-07-18)


### Features

* **gawk-broadcast:** viewer count in engine + GUI, first-viewer notification (R18 Y5) ([05ea0f7](https://github.com/Tuhis/gawk/commit/05ea0f7d41d24dd33982085a0c614016244fac0f))
* gawk-pubsim synthetic fixture publisher + browser E2E harness (R20 Z1) ([54e62e5](https://github.com/Tuhis/gawk/commit/54e62e5875c22ba5ec28057adc207218f857e99b))
* **wire:** TypeViewerCount 0x0B datagram in all three mirrors (R18 Y1) ([938ce28](https://github.com/Tuhis/gawk/commit/938ce2882c708530873c6ea7a79017f4387bf2f8))

## [1.1.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.0.1...gawk-broadcast-v1.1.0) (2026-07-18)


### Features

* resilient viewer mode — reliable carrier delivery + extended buffering (R19 X2–X5) ([#55](https://github.com/Tuhis/gawk/issues/55)) ([e02bf19](https://github.com/Tuhis/gawk/commit/e02bf194d44fe7fd77cd262ac87b979cb95b9a1c))

## [1.0.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-v1.0.0...gawk-broadcast-v1.0.1) (2026-07-18)


### Bug Fixes

* **gawk-server:** token-bearing reclaim supersedes a zombie publisher instead of 409ing ([#53](https://github.com/Tuhis/gawk/issues/53)) ([a20fa67](https://github.com/Tuhis/gawk/commit/a20fa6712e2976c0fb32e27650ac82fae768a8c0))

## 1.0.0 (2026-07-17)


### Features

* **gawk-broadcast:** native Linux broadcaster with hardware encode (R14 V0–V7) ([#44](https://github.com/Tuhis/gawk/issues/44)) ([69e764c](https://github.com/Tuhis/gawk/commit/69e764cfcdbc3d186c6d09b5da24a758f16ccdd5))
* relay scale-out & high availability — R17 W1–W6 ([#47](https://github.com/Tuhis/gawk/issues/47)) ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))


### Bug Fixes

* **gawk-broadcast:** dispatch relay server messages by wire type and carry the R17 resume token — webtransport-go delivers server uni streams in nondeterministic order (docs/22 finding 9), and every /publish/{id} claim now needs the token, persisted as lastResumeToken. ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))
