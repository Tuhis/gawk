# Changelog

## [1.11.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.10.0...gawk-admin/v1.11.0) (2026-10-26)


### Features

* **r79:** room voice through a Mumble bridge (MV0–MV7) ([#595](https://github.com/Tuhis/gawk/issues/595)) ([33a4a4f](https://github.com/Tuhis/gawk/commit/33a4a4f904f79937ebddb6e0a06f3df4d12f7ba8))
* **relay:** stop ends the broadcast for viewers at once (R78) ([#568](https://github.com/Tuhis/gawk/issues/568)) ([2143db6](https://github.com/Tuhis/gawk/commit/2143db67e492e9e492301bf5002781dbed4311ea))


### Bug Fixes

* **site:** move the party relay off the app downloads onto its own page ([#581](https://github.com/Tuhis/gawk/issues/581)) ([62cafd9](https://github.com/Tuhis/gawk/commit/62cafd9365b8056960d4ba3a7b08ed080a3eb248))

## [1.10.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.9.0...gawk-admin/v1.10.0) (2026-10-23)


### Features

* **r74:** PoP relays, a one-click LAN relay that federates with the home fleet ([#553](https://github.com/Tuhis/gawk/issues/553)) ([a60eac9](https://github.com/Tuhis/gawk/commit/a60eac9cfd0d189d451c1e13c48a923317209b8c))

## [1.9.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.8.0...gawk-admin/v1.9.0) (2026-10-15)


### Features

* **admin:** name the fired labels in content-flag summaries ([#511](https://github.com/Tuhis/gawk/issues/511)) ([d2f1e66](https://github.com/Tuhis/gawk/commit/d2f1e6614f4eb3fc213c16d1030e2c7b29e40935))
* **r70:** broadcast moderation timeline (MT1–MT6) ([#507](https://github.com/Tuhis/gawk/issues/507)) ([25a0768](https://github.com/Tuhis/gawk/commit/25a0768bde09aeed9eea0d788bd218a6724da36f))

## [1.8.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.7.0...gawk-admin/v1.8.0) (2026-10-14)


### Features

* **r40:** automatic content flagging — gawk-sampler, content flags and the screening notice (CF0–CF8) ([#500](https://github.com/Tuhis/gawk/issues/500)) ([99cf6d4](https://github.com/Tuhis/gawk/commit/99cf6d41c07803f7d5ce60716b1f7f99ba6be560))

## [1.7.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.6.0...gawk-admin/v1.7.0) (2026-10-04)


### Features

* **telemetry:** OIDC for the telemetry read surface (R53) ([#453](https://github.com/Tuhis/gawk/issues/453)) ([ac68ad1](https://github.com/Tuhis/gawk/commit/ac68ad12ab4e680c5df325368f3fab564c1affd1))

## [1.6.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.5.0...gawk-admin/v1.6.0) (2026-09-29)


### Features

* **admin:** rooms-manager role for static room create and delete (R60) ([#395](https://github.com/Tuhis/gawk/issues/395)) ([8c6fb7b](https://github.com/Tuhis/gawk/commit/8c6fb7be10d336725a01875a3f14588a88d658d2))

## [1.5.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.4.0...gawk-admin/v1.5.0) (2026-09-29)


### Features

* **admin:** rooms read API, rooms-reader role and opt-in room activity webhooks (R49) ([#388](https://github.com/Tuhis/gawk/issues/388)) ([0c134c1](https://github.com/Tuhis/gawk/commit/0c134c1f7e0c77df890a9ff218e21591e9ce0aa9))
* **r59:** usage and capacity metrics for the gawk.ioio.fi dashboard ([#387](https://github.com/Tuhis/gawk/issues/387)) ([cb9d188](https://github.com/Tuhis/gawk/commit/cb9d1884a19092b38213bf6edeec5cad2c3805d4))

## [1.4.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.3.0...gawk-admin/v1.4.0) (2026-09-23)


### Features

* **events:** name broadcasts and rooms in cleartext in every event ([#348](https://github.com/Tuhis/gawk/issues/348)) ([cd82ee0](https://github.com/Tuhis/gawk/commit/cd82ee0586286681b8c8f65a8740de0ee33d66e0))

## [1.3.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.2.0...gawk-admin/v1.3.0) (2026-09-21)


### Features

* **admin:** R48 OA5 — a Console on the API page that sends calls as the signed-in operator ([#337](https://github.com/Tuhis/gawk/issues/337)) ([52cc418](https://github.com/Tuhis/gawk/commit/52cc418052d339729757e343b020b416fd56e973))


### Bug Fixes

* **eventbus:** never abandon a configured bus, and render maxBytes as an integer ([#334](https://github.com/Tuhis/gawk/issues/334)) ([16059b9](https://github.com/Tuhis/gawk/commit/16059b915956e6752f1691c27750c4a4ba01c8cc))
* **server:** restore Safari/WebKit viewers — quic-go v0.62.0 + WT flow-control SETTINGS ([#342](https://github.com/Tuhis/gawk/issues/342)) ([ce63ec6](https://github.com/Tuhis/gawk/commit/ce63ec6442242611ded40301340e4475619b5377))

## [1.2.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.1.0...gawk-admin/v1.2.0) (2026-09-20)


### Features

* **admin:** R48 — OpenAPI contract for the gawk-admin API ([#323](https://github.com/Tuhis/gawk/issues/323)) ([e7f632a](https://github.com/Tuhis/gawk/commit/e7f632a2de5d21130f303d2683f540fef48f3d41))
* **admin:** R51 — event contract: CloudEvents, JSON Schema, AsyncAPI, Standard Webhooks ([#327](https://github.com/Tuhis/gawk/issues/327)) ([b4ed483](https://github.com/Tuhis/gawk/commit/b4ed483449759a7339c644fe5c41e8dd5ecc575b))
* **server:** R50 — relay event bus over NATS JetStream ([#329](https://github.com/Tuhis/gawk/issues/329)) ([921770d](https://github.com/Tuhis/gawk/commit/921770d7a362b647a8385b9810951a1239623541))

## [1.1.0](https://github.com/Tuhis/gawk/compare/gawk-admin/v1.0.0...gawk-admin/v1.1.0) (2026-09-10)


### Features

* **r42:** rooms — multi-POV rooms over unchanged broadcasts (RM1–RM9) ([#302](https://github.com/Tuhis/gawk/issues/302)) ([e666f74](https://github.com/Tuhis/gawk/commit/e666f741d0b22624b9313b88d4dece022890200b))

## 1.0.0 (2026-08-31)


### Features

* **admin:** restyle the portal in the gawk design language ([#295](https://github.com/Tuhis/gawk/issues/295)) ([ca6f510](https://github.com/Tuhis/gawk/commit/ca6f510a05204f0a3a647f212dfcc5258e0de34d))
* **r39:** admin portal for moderation ([#280](https://github.com/Tuhis/gawk/issues/280)) ([054d70b](https://github.com/Tuhis/gawk/commit/054d70b859ee98475654e6f6ea960b51e38b10af))


### Bug Fixes

* **admin:** portal deep links rendered broadcasts, whatever the hash said ([#290](https://github.com/Tuhis/gawk/issues/290)) ([80e9350](https://github.com/Tuhis/gawk/commit/80e935020b4c4e8d17d006f486b116b545af803f))
* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))
