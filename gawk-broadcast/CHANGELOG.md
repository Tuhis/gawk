# Changelog

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
