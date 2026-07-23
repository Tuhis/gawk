# Changelog

## [0.28.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.28.1...gawk-app-v0.28.2) (2026-07-23)


### Bug Fixes

* **viewer:** keep Deep-buffer audio on the video schedule through the startup hold ([#119](https://github.com/Tuhis/gawk/issues/119)) ([aa4a749](https://github.com/Tuhis/gawk/commit/aa4a749e2cd475ae496f46394c31727e5e4d3064))

## [0.28.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.28.0...gawk-app-v0.28.1) (2026-07-23)


### Bug Fixes

* **viewer:** hold Deep-buffer audio to the DVR depth so A/V stays in sync ([#115](https://github.com/Tuhis/gawk/issues/115)) ([d70e92d](https://github.com/Tuhis/gawk/commit/d70e92d22bd6ca0843e45448f1c712cdda178c74))
* **viewer:** measure A/V skew at presentation and snap on re-anchor (docs/20 finding 9) ([#116](https://github.com/Tuhis/gawk/issues/116)) ([0924b8d](https://github.com/Tuhis/gawk/commit/0924b8d09251174b76991794cac8a6c02e026dc4))

## [0.28.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.27.0...gawk-app-v0.28.0) (2026-07-23)


### Features

* **viewer:** retire the fixed playout toggle, pacing is one binary (docs/17 Decision 10) ([#113](https://github.com/Tuhis/gawk/issues/113)) ([5632a3b](https://github.com/Tuhis/gawk/commit/5632a3bcdbdf0423875f7e56a28e5b9e06876a7c))


### Bug Fixes

* stop sending DVR subscribers a second, live keyframe timeline ([#112](https://github.com/Tuhis/gawk/issues/112)) ([7de0d18](https://github.com/Tuhis/gawk/commit/7de0d18f1bcb9fc55e75c3da1dbf1ff32a399cb0))

## [0.27.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.5...gawk-app-v0.27.0) (2026-07-23)


### Features

* relay DVR ring so resilient viewers ride out dropouts (R21) ([#111](https://github.com/Tuhis/gawk/issues/111)) ([b92de77](https://github.com/Tuhis/gawk/commit/b92de77f8a2e9fbeaf22a05a112d89e89cdf88b5))


### Bug Fixes

* **viewer:** recover from a dead session instead of freezing forever ([ba58c1e](https://github.com/Tuhis/gawk/commit/ba58c1eb51d9477f02707610d2480803e466f569))
* **viewer:** stop audio latching into synthesized silence (docs/20 finding 8) ([#108](https://github.com/Tuhis/gawk/issues/108)) ([c684f90](https://github.com/Tuhis/gawk/commit/c684f90b799b96ccc1af12c8cb2d4ee575ef8b35))

## [0.26.5](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.4...gawk-app-v0.26.5) (2026-07-22)


### Bug Fixes

* **viewer:** let the resilient playout buffer reach its 2 s envelope ([#96](https://github.com/Tuhis/gawk/issues/96)) ([3041fca](https://github.com/Tuhis/gawk/commit/3041fca9642ce13f40ca79ca10316b29a91a9bfb))
* **viewer:** make the resilient-mode menu reachable without a right-click ([#98](https://github.com/Tuhis/gawk/issues/98)) ([e2614a8](https://github.com/Tuhis/gawk/commit/e2614a81091ff3555b2367c99c1f719181afd187))
* **viewer:** offer the interpolation toggle under resilient mode ([#106](https://github.com/Tuhis/gawk/issues/106)) ([6b613cd](https://github.com/Tuhis/gawk/commit/6b613cd572d571cc31d0486900678b6cc1dbfc85))
* **viewer:** prune settled server-stream tasks to stop a slow leak ([#101](https://github.com/Tuhis/gawk/issues/101)) ([e336334](https://github.com/Tuhis/gawk/commit/e336334821964b7280bed31cb8f4e890241797b3))

## [0.26.4](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.3...gawk-app-v0.26.4) (2026-07-21)


### Bug Fixes

* recover viewers whose keyframe delivery stalls (Safari freeze) ([#90](https://github.com/Tuhis/gawk/issues/90)) ([8db98a6](https://github.com/Tuhis/gawk/commit/8db98a6cb1196506e4b87e50ee8526fcd19fd6d2))

## [0.26.3](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.2...gawk-app-v0.26.3) (2026-07-21)


### Bug Fixes

* **audio:** count only delivered chunks + recover from worklet stalls ([#86](https://github.com/Tuhis/gawk/issues/86)) ([27811f9](https://github.com/Tuhis/gawk/commit/27811f970ceee192189c4bd1a28b001d3ad5d382))

## [0.26.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.1...gawk-app-v0.26.2) (2026-07-21)


### Bug Fixes

* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))

## [0.26.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.26.0...gawk-app-v0.26.1) (2026-07-20)


### Bug Fixes

* **gawk-app:** degrade to video-only when system audio can't start ([1dca620](https://github.com/Tuhis/gawk/commit/1dca620b1f7d96b657293868e169aabcd4b31f43))
* **gawk-app:** make video the A/V sync master and align audio to it ([98e66b2](https://github.com/Tuhis/gawk/commit/98e66b297c60feba9e3bf0a86e5bbd09a5a75722))

## [0.26.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.25.2...gawk-app-v0.26.0) (2026-07-19)


### Features

* **r15:** system audio — Opus over datagrams (experimental) ([#64](https://github.com/Tuhis/gawk/issues/64)) ([8cefcb7](https://github.com/Tuhis/gawk/commit/8cefcb73b63f442c41a5b05421a99426feda4ee5))

## [0.25.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.25.1...gawk-app-v0.25.2) (2026-07-19)


### Bug Fixes

* **gawk-app:** reset broadcaster stage when start() fails after capture ([169689d](https://github.com/Tuhis/gawk/commit/169689d1305ce083144c4a21b1cfe839bb2aa16f))

## [0.25.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.25.0...gawk-app-v0.25.1) (2026-07-18)


### Bug Fixes

* **gawk-app:** rebase TimeSync samples across worker clock domains ([c0b0b8f](https://github.com/Tuhis/gawk/commit/c0b0b8f71cf6d704d911597e3b114724c8714bbf))

## [0.25.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.24.0...gawk-app-v0.25.0) (2026-07-18)


### Features

* **gawk-app:** live "N watching" badge + overlay rows on both surfaces (R18 Y4) ([362da15](https://github.com/Tuhis/gawk/commit/362da15932fe2dfa9f98b09235ce3ae4a72d1dcd))
* **wire:** TypeViewerCount 0x0B datagram in all three mirrors (R18 Y1) ([938ce28](https://github.com/Tuhis/gawk/commit/938ce2882c708530873c6ea7a79017f4387bf2f8))

## [0.24.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.23.1...gawk-app-v0.24.0) (2026-07-18)


### Features

* resilient viewer mode — reliable carrier delivery + extended buffering (R19 X2–X5) ([#55](https://github.com/Tuhis/gawk/issues/55)) ([e02bf19](https://github.com/Tuhis/gawk/commit/e02bf194d44fe7fd77cd262ac87b979cb95b9a1c))

## [0.23.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.23.0...gawk-app-v0.23.1) (2026-07-18)


### Bug Fixes

* **gawk-server:** token-bearing reclaim supersedes a zombie publisher instead of 409ing ([#53](https://github.com/Tuhis/gawk/issues/53)) ([a20fa67](https://github.com/Tuhis/gawk/commit/a20fa6712e2976c0fb32e27650ac82fae768a8c0))

## [0.23.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.22.2...gawk-app-v0.23.0) (2026-07-17)


### Features

* **gawk-broadcast:** native Linux broadcaster with hardware encode (R14 V0–V7) ([#44](https://github.com/Tuhis/gawk/issues/44)) ([69e764c](https://github.com/Tuhis/gawk/commit/69e764cfcdbc3d186c6d09b5da24a758f16ccdd5))
* relay scale-out & high availability — R17 W1–W6 ([#47](https://github.com/Tuhis/gawk/issues/47)) ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))


### Bug Fixes

* **gawk-broadcast:** dispatch relay server messages by wire type and carry the R17 resume token — webtransport-go delivers server uni streams in nondeterministic order (docs/22 finding 9), and every /publish/{id} claim now needs the token, persisted as lastResumeToken. ([73045cd](https://github.com/Tuhis/gawk/commit/73045cd3790a8793c5bf45db159de969a041bca9))

## [0.22.2](https://github.com/Tuhis/gawk/compare/gawk-app-v0.22.1...gawk-app-v0.22.2) (2026-07-16)


### Bug Fixes

* iOS fullscreen tee writes decoded-frame clones — canvas readback is black on iOS WebKit (R16 U4 pass 2) ([f6301b6](https://github.com/Tuhis/gawk/commit/f6301b652587bfbeb6980d581eca83c4ce4d44fa))

## [0.22.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.22.0...gawk-app-v0.22.1) (2026-07-16)


### Bug Fixes

* iOS native fullscreen black screen — tee-local PTS, preserveDrawingBuffer, gesture play(), element diagnostics (R16 U4) ([8e2ee56](https://github.com/Tuhis/gawk/commit/8e2ee561037b478003ba04f56831fe8215cc6ce3))

## [0.22.0](https://github.com/Tuhis/gawk/compare/gawk-app-v0.21.1...gawk-app-v0.22.0) (2026-07-16)


### Features

* Feature Gates rows show bare ✓/✗ with the detail as a hover tooltip ([b4313a1](https://github.com/Tuhis/gawk/commit/b4313a18fbecdb1488da9f07dae3ce02279f34ba))
* iOS native fullscreen — canvas tee to &lt;video&gt;, tiered useFullscreen, Feature Gates (R16 U1–U3) ([ada0d27](https://github.com/Tuhis/gawk/commit/ada0d278f83fb8a2e45655a036b1ccb013134c8b))
* viewer error cards show friendly copy keyed on a structured error kind ([a500629](https://github.com/Tuhis/gawk/commit/a500629c2d11456b156ff3906216d49643af9b4b))

## [0.21.1](https://github.com/Tuhis/gawk/compare/gawk-app-v0.21.0...gawk-app-v0.21.1) (2026-07-15)


### Bug Fixes

* viewer "Bitrate (recv)" always empty — count received video bytes client-side ([2f45766](https://github.com/Tuhis/gawk/commit/2f45766d38d9895ff6229a649c7caf5621cba95b))

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
