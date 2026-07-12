# Changelog

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
