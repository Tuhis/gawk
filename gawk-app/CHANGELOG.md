# Changelog

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
