# Changelog

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
