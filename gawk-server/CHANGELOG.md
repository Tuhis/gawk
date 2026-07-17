# Changelog

## [0.16.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.15.0...gawk-server-v0.16.0) (2026-07-17)


### Features

* **gawk-broadcast:** native Linux broadcaster with hardware encode (R14 V0–V7) ([#44](https://github.com/Tuhis/gawk/issues/44)) ([69e764c](https://github.com/Tuhis/gawk/commit/69e764cfcdbc3d186c6d09b5da24a758f16ccdd5))
* **gawk-server:** expose externalTrafficPolicy and pod anti-affinity in the chart ([8817f28](https://github.com/Tuhis/gawk/commit/8817f2819626edd4acd36cfed5901bcab009d482))
* relay scale-out & high availability — R17 W1–W6 ([#47](https://github.com/Tuhis/gawk/issues/47)) ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))


### Bug Fixes

* **gawk-broadcast:** dispatch relay server messages by wire type and carry the R17 resume token — webtransport-go delivers server uni streams in nondeterministic order (docs/22 finding 9), and every /publish/{id} claim now needs the token, persisted as lastResumeToken. ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))

## [0.15.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.14.1...gawk-server-v0.15.0) (2026-07-14)


### Features

* viewer live-edge measurement + opt-in smoothed playout (R5 Q1-Q3) ([449104e](https://github.com/Tuhis/gawk/commit/449104e3a58d3b85f83353e0d0f429cc61624efc))

## [0.14.1](https://github.com/Tuhis/gawk/compare/gawk-server-v0.14.0...gawk-server-v0.14.1) (2026-07-14)


### Bug Fixes

* cover real keyframe latency in reorder wait + evict zombie subscribers (R10 field findings) ([92e3ef9](https://github.com/Tuhis/gawk/commit/92e3ef99311560befe981f2fba531d8ac68d0bc4))

## [0.14.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.13.0...gawk-server-v0.14.0) (2026-07-14)


### Features

* observability & metrics — Prometheus ops endpoint + client funnel stats (R9, M1-M7) ([43a1035](https://github.com/Tuhis/gawk/commit/43a1035318c9ac1ce8c7de976ebff205dea4d383))

## [0.13.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.12.0...gawk-server-v0.13.0) (2026-07-14)


### Features

* reliable keyframes over WebTransport uni streams (R8, S1-S5+S7) ([a522ec7](https://github.com/Tuhis/gawk/commit/a522ec71dd9db815a1fb49dcf55afbad37568ce1))

## [0.12.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.11.0...gawk-server-v0.12.0) (2026-07-14)


### Features

* log request origin when a connection is blocked ([f14eb9b](https://github.com/Tuhis/gawk/commit/f14eb9b8fc2ef0a84d1e56200a0818017acd97c2))

## [0.11.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.10.1...gawk-server-v0.11.0) (2026-07-13)


### Features

* **media:** increase default capture resolution to 4K and chunk cap to 3000 ([789fa6b](https://github.com/Tuhis/gawk/commit/789fa6b9f89e21c892dd01384a1f1ca35ac2cddb))

## [0.10.1](https://github.com/Tuhis/gawk/compare/gawk-server-v0.10.0...gawk-server-v0.10.1) (2026-07-13)


### Bug Fixes

* disable sysctl initContainer by default ([80a2fd6](https://github.com/Tuhis/gawk/commit/80a2fd690877d6c774dc85b7efc4b339add02d08))

## [0.10.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.9.1...gawk-server-v0.10.0) (2026-07-13)


### Features

* add privileged sysctl initContainer for UDP buffer tuning ([e181f2e](https://github.com/Tuhis/gawk/commit/e181f2e4bbe084132c739e4b927d4b79c3ff91d6))
* add publishSecretKeyRef to Helm chart for secure secrets ([7cfeb7d](https://github.com/Tuhis/gawk/commit/7cfeb7d4b829103d7199bbd56220c3062735a7ea))
* implement R2 hardening limits, access control, and bandwidth drops ([7cd4824](https://github.com/Tuhis/gawk/commit/7cd4824bedb6c0e45f9c5507e168adf39cacbaee))


### Bug Fixes

* wire R2 hardening limits into production and fix review findings ([8f67538](https://github.com/Tuhis/gawk/commit/8f6753867ca7525fc096add67b28f9296f788052))

## [0.9.1](https://github.com/Tuhis/gawk/compare/gawk-server-v0.9.0...gawk-server-v0.9.1) (2026-07-12)


### Bug Fixes

* harden R1 failure paths, close-code handling, and GC stats ([b7d8f58](https://github.com/Tuhis/gawk/commit/b7d8f583a92da4e8588252fae95c935dafa4d460))

## [0.9.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.8.0...gawk-server-v0.9.0) (2026-07-12)


### Features

* implement R1 multi-broadcaster support (E-G) ([b8eb374](https://github.com/Tuhis/gawk/commit/b8eb374eceaf1891b7e2bc726fd692d81ab1a25b))

## [0.8.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.7.0...gawk-server-v0.8.0) (2026-07-12)


### Features

* **gawk-echo:** add -origin flag for targets with an origin allowlist ([7ce77b9](https://github.com/Tuhis/gawk/commit/7ce77b9d985281557f4067653f3ba986b1f4482b))


### Bug Fixes

* guard test log buffer with a mutex to fix data race ([f10032f](https://github.com/Tuhis/gawk/commit/f10032fdbe03ebf57c0603f96de312b3c0c65630))

## [0.7.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.6.1...gawk-server-v0.7.0) (2026-07-12)


### Features

* add GAWK_QUIET_PROBE_LOGS to suppress loopback echo probe spam ([fd61f8c](https://github.com/Tuhis/gawk/commit/fd61f8c1c63c3f8895cea2cbdf51f74c984f88ae))

## [0.6.1](https://github.com/Tuhis/gawk/compare/gawk-server-v0.6.0...gawk-server-v0.6.1) (2026-07-12)


### Bug Fixes

* bypass Origin allowlist for loopback probe requests ([69ef281](https://github.com/Tuhis/gawk/commit/69ef281cf5ef18f6480be49e0f702934a8cbbf0c))

## [0.6.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.5.0...gawk-server-v0.6.0) (2026-07-12)


### Features

* restructure Helm chart dirs to deploy/charts/&lt;chart-name&gt; ([39ef8fe](https://github.com/Tuhis/gawk/commit/39ef8fe96d6df9c5fbf4e3b4d01c526565ea8a0c))

## [0.5.0](https://github.com/Tuhis/gawk/compare/gawk-server-v0.4.0...gawk-server-v0.5.0) (2026-07-12)


### Features

* add gawk-echo CLI probe for testing a live server without a browser ([7b68a54](https://github.com/Tuhis/gawk/commit/7b68a5405a431183fe450bbe739cea75f7e3ffb3))
* milestone C fan-out — hub hardening, restart-safe caches, /statusz (C1–C3) ([f0ff02c](https://github.com/Tuhis/gawk/commit/f0ff02cce9ba3437724dbfa4148df9f3bfb8a53d))
* milestone D — resilience, packaging, and CI/CD (D1–D4) ([3311ae2](https://github.com/Tuhis/gawk/commit/3311ae2b4164faaf3808a13dc01cdbafca7d7fa4))
* relay hub + publish/subscribe routes (B2, B3) ([72d311c](https://github.com/Tuhis/gawk/commit/72d311c1cbced3d4d3bdc2c52d512ae46df5aec7))
* scaffold gawk-server relay — milestone A + wire format (B1) ([b07c153](https://github.com/Tuhis/gawk/commit/b07c1534c514e899960a13690421a3af3a74e083))


### Bug Fixes

* cap dev cert span to 14 days incl. clock-skew backdate; A5 verified ([52e0f21](https://github.com/Tuhis/gawk/commit/52e0f210c1fd77e426ddeb75ab8dc29f47d7f152))
