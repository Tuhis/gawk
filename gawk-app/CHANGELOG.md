# Changelog

## [0.21.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.20.1...gawk-app-v0.21.0) (2026-07-15)


### Features

* default the viewer to adaptive paced playback + interpolation ([3d0f5e3](https://github.com/Tuhis/gawk/commit/3d0f5e365beaf2ecc247dd580b28fa061134c784))

## [0.20.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.20.0...gawk-app-v0.20.1) (2026-07-15)


### Bug Fixes

* bound concurrent isConfigSupported probes — Chrome tab OOM crash ([50483f6](https://github.com/Tuhis/gawk/commit/50483f6ef75708063f2952be1eafce94d2f4c34f))

## [0.20.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.19.0...gawk-app-v0.20.0) (2026-07-15)


### Features

* annotate codec pin options with per-codec HW support ([ed4445c](https://github.com/Tuhis/gawk/commit/ed4445cba82d132338ff1c1bfbbc0e31cd7b4462))


### Bug Fixes

* stack the advanced encoder settings vertically ([919e9da](https://github.com/Tuhis/gawk/commit/919e9da2d4d20867e04b6b6783128aaf5e4512b2))

## [0.19.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.18.0...gawk-app-v0.19.0) (2026-07-15)


### Features

* adaptive playout offset controller (R12 T3) ([30aae8a](https://github.com/Tuhis/gawk/commit/30aae8aefe49515a64991228362449082cc0935d))
* experimental frame-interpolation scaffold (R12 T4) ([92f9409](https://github.com/Tuhis/gawk/commit/92f940917756def4de04d0eef308ddbcfe1e9771))
* paced presentation sink + adaptive playout mode plumbing (R12 T2) ([d1fd7c7](https://github.com/Tuhis/gawk/commit/d1fd7c737cab19231847b496ecaa16e9fbc7d7c6))
* R13 advanced broadcaster settings (supersedes R7) ([#39](https://github.com/Tuhis/gawk/issues/39)) ([e5619dc](https://github.com/Tuhis/gawk/commit/e5619dcaecadfcc843f94ea00d89d43ee73f65ab))
* show stream resolution + framerate in the viewer stats overlay ([24882ef](https://github.com/Tuhis/gawk/commit/24882ef87420f41668cadb1ebc27c99ecd176882))
* viewer jitter measurement foundation (R12 T1) ([4d11c8f](https://github.com/Tuhis/gawk/commit/4d11c8f8f0863deac4222f5590ff581dba8c7217))


### Bug Fixes

* truthful path-MTU log + explicit software-encoding hint ([a17ab35](https://github.com/Tuhis/gawk/commit/a17ab3548fcec3a9a98e6362c86af33b0457987d))

## [0.18.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.17.2...gawk-app-v0.18.0) (2026-07-14)


### Features

* offload broadcast pipeline to a Web Worker (R11) ([02aed13](https://github.com/Tuhis/gawk/commit/02aed13fb9f934161653a6bf10fcd27973dd91b8))
* viewer live-edge measurement + opt-in smoothed playout (R5 Q1-Q3) ([449104e](https://github.com/Tuhis/gawk/commit/449104e3a58d3b85f83353e0d0f429cc61624efc))

## [0.17.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.17.1...gawk-app-v0.17.2) (2026-07-14)


### Bug Fixes

* restore restart recovery severed by R8 + wrap-aware frameId arithmetic ([7cd8104](https://github.com/Tuhis/gawk/commit/7cd810473427c1b8f0eae40385f42d1c73892de1))

## [0.17.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.17.0...gawk-app-v0.17.1) (2026-07-14)


### Bug Fixes

* cover real keyframe latency in reorder wait + evict zombie subscribers (R10 field findings) ([92e3ef9](https://github.com/Tuhis/gawk/commit/92e3ef99311560befe981f2fba531d8ac68d0bc4))

## [0.17.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.16.0...gawk-app-v0.17.0) (2026-07-14)


### Features

* transport worker split, decoder queue 5→10, placement stats (R10 P3+P4-partial) ([aad212a](https://github.com/Tuhis/gawk/commit/aad212aa555ec47168b32de4dc5f594c650927b0))

## [0.16.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.15.0...gawk-app-v0.16.0) (2026-07-14)


### Features

* viewer render performance — WebGL + rAF-coalesced render sinks (R10, P1-P2) ([01296e0](https://github.com/Tuhis/gawk/commit/01296e09529abd3ac5b8834ec795ce1d70cc3dea))

## [0.15.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.14.0...gawk-app-v0.15.0) (2026-07-14)


### Features

* observability & metrics — Prometheus ops endpoint + client funnel stats (R9, M1-M7) ([43a1035](https://github.com/Tuhis/gawk/commit/43a1035318c9ac1ce8c7de976ebff205dea4d383))
* offload the viewer pipeline to a Web Worker (R8 S6) ([336d9e0](https://github.com/Tuhis/gawk/commit/336d9e0ab497ccfee451fcc8a73fe8a4b727fe94))


### Bug Fixes

* **viewer:** track pending decodes to enforce queue size limit properly ([3e1069c](https://github.com/Tuhis/gawk/commit/3e1069c80b1a2cee2033e993bf6fcf45d73b67fb))

## [0.14.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.13.1...gawk-app-v0.14.0) (2026-07-14)


### Features

* reliable keyframes over WebTransport uni streams (R8, S1-S5+S7) ([a522ec7](https://github.com/Tuhis/gawk/commit/a522ec71dd9db815a1fb49dcf55afbad37568ce1))

## [0.13.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.13.0...gawk-app-v0.13.1) (2026-07-14)


### Bug Fixes

* reduce viewer corruption and drops during heavy motion ([2c6d50f](https://github.com/Tuhis/gawk/commit/2c6d50f9877afb7fed224fb376f09b7e4f0de1fa))
* **viewer:** bound decode queue size to prevent unbounded latency ([70e2065](https://github.com/Tuhis/gawk/commit/70e2065cabc6a03246b4cb5af9817071c6d4b833))

## [0.13.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.12.0...gawk-app-v0.13.0) (2026-07-13)


### Features

* **media:** implement dynamic GPU HW encoding check and cap framerate above FullHD; default dev mode to prompt publish secret ([384d520](https://github.com/Tuhis/gawk/commit/384d520f5e4f86ad26a40239dfea231ae9dccd2d))

## [0.12.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.11.0...gawk-app-v0.12.0) (2026-07-13)


### Features

* **media:** add 4K-compatible H.264 level profiles to default preferences ([14eacbe](https://github.com/Tuhis/gawk/commit/14eacbe028afa8b29d3a5f76282c53825ef564d8))
* **media:** increase default capture resolution to 4K and chunk cap to 3000 ([789fa6b](https://github.com/Tuhis/gawk/commit/789fa6b9f89e21c892dd01384a1f1ca35ac2cddb))

## [0.11.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.10.0...gawk-app-v0.11.0) (2026-07-13)


### Features

* **gawk-app:** automatic resolution fallback (R4) ([#25](https://github.com/Tuhis/gawk/issues/25)) ([2853ca6](https://github.com/Tuhis/gawk/commit/2853ca6c4e67f5cabe3df439146abacb5260273b))

## [0.10.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.9.0...gawk-app-v0.10.0) (2026-07-13)


### Features

* add broadcaster resolution & framerate picker (R3) ([f8f256b](https://github.com/Tuhis/gawk/commit/f8f256bfb6be4e92ed99a36325456ddf10dc8754))

## [0.9.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.8.2...gawk-app-v0.9.0) (2026-07-13)


### Features

* implement R2 hardening limits, access control, and bandwidth drops ([7cd4824](https://github.com/Tuhis/gawk/commit/7cd4824bedb6c0e45f9c5507e168adf39cacbaee))


### Bug Fixes

* wire R2 hardening limits into production and fix review findings ([8f67538](https://github.com/Tuhis/gawk/commit/8f6753867ca7525fc096add67b28f9296f788052))

## [0.8.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.8.1...gawk-app-v0.8.2) (2026-07-12)


### Bug Fixes

* unbreak gawk-app build — type the vi.fn mocks in broadcaster.test.ts ([a19810b](https://github.com/Tuhis/gawk/commit/a19810b4f1975f6305eb3d121cf2930fc133d4cd))

## [0.8.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.8.0...gawk-app-v0.8.1) (2026-07-12)


### Bug Fixes

* harden R1 failure paths, close-code handling, and GC stats ([b7d8f58](https://github.com/Tuhis/gawk/commit/b7d8f583a92da4e8588252fae95c935dafa4d460))

## [0.8.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.7.1...gawk-app-v0.8.0) (2026-07-12)


### Features

* implement R1 multi-broadcaster support (E-G) ([b8eb374](https://github.com/Tuhis/gawk/commit/b8eb374eceaf1891b7e2bc726fd692d81ab1a25b))

## [0.7.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.7.0...gawk-app-v0.7.1) (2026-07-12)


### Bug Fixes

* correct default production server url ([b6c5b21](https://github.com/Tuhis/gawk/commit/b6c5b2179d104315995a0557eb2a5a73d60efa4c))

## [0.7.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.6.0...gawk-app-v0.7.0) (2026-07-12)


### Features

* auto-detect production domain for gawk-app default backend URL ([d17eb32](https://github.com/Tuhis/gawk/commit/d17eb326f6852e47e0293d55be324473e886b8d3))
* frontend transport module + broadcast/view pages (B4) ([6c6e99f](https://github.com/Tuhis/gawk/commit/6c6e99ff0bf17fa18867c8315d7d91e18a890e4a))
* initial commit ([70ce6ea](https://github.com/Tuhis/gawk/commit/70ce6ea4a50f05c8f446db7c2d8092a94e823f0c))
* milestone D — resilience, packaging, and CI/CD (D1–D4) ([3311ae2](https://github.com/Tuhis/gawk/commit/3311ae2b4164faaf3808a13dc01cdbafca7d7fa4))
* restructure Helm chart dirs to deploy/charts/&lt;chart-name&gt; ([39ef8fe](https://github.com/Tuhis/gawk/commit/39ef8fe96d6df9c5fbf4e3b4d01c526565ea8a0c))


### Bug Fixes

* viewer decoder HW→software fallback; milestone B verified ([b065570](https://github.com/Tuhis/gawk/commit/b065570796364c534c18518fa39e05c8232db405))

## [0.6.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.5.0...gawk-app-v0.6.0) (2026-07-12)


### Features

* restructure Helm chart dirs to deploy/charts/&lt;chart-name&gt; ([39ef8fe](https://github.com/Tuhis/gawk/commit/39ef8fe96d6df9c5fbf4e3b4d01c526565ea8a0c))

## [0.5.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.4.0...gawk-app-v0.5.0) (2026-07-12)


### Features

* frontend transport module + broadcast/view pages (B4) ([6c6e99f](https://github.com/Tuhis/gawk/commit/6c6e99ff0bf17fa18867c8315d7d91e18a890e4a))
* initial commit ([70ce6ea](https://github.com/Tuhis/gawk/commit/70ce6ea4a50f05c8f446db7c2d8092a94e823f0c))
* milestone D — resilience, packaging, and CI/CD (D1–D4) ([3311ae2](https://github.com/Tuhis/gawk/commit/3311ae2b4164faaf3808a13dc01cdbafca7d7fa4))


### Bug Fixes

* viewer decoder HW→software fallback; milestone B verified ([b065570](https://github.com/Tuhis/gawk/commit/b065570796364c534c18518fa39e05c8232db405))
