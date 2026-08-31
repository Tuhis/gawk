# Changelog

## [1.2.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows/v1.1.1...gawk-broadcast-windows/v1.2.0) (2026-08-31)


### Features

* **r39:** admin portal for moderation ([#280](https://github.com/Tuhis/gawk/issues/280)) ([054d70b](https://github.com/Tuhis/gawk/commit/054d70b859ee98475654e6f6ea960b51e38b10af))

## [1.1.1](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows-v1.1.0...gawk-broadcast-windows/v1.1.1) (2026-08-19)


### Bug Fixes

* reap orphaned stripe legs via viewer-owned session groups and a liveness lease ([#259](https://github.com/Tuhis/gawk/issues/259)) ([dae116e](https://github.com/Tuhis/gawk/commit/dae116e87a41ce90992d56d11a1e9c77c0ef32c6))

## [1.1.0](https://github.com/Tuhis/gawk/compare/gawk-broadcast-windows-v1.0.0...gawk-broadcast-windows-v1.1.0) (2026-08-01)


### Features

* **broadcasters:** show the build version in both native broadcaster windows ([#214](https://github.com/Tuhis/gawk/issues/214)) ([aece3fd](https://github.com/Tuhis/gawk/commit/aece3fdce6b598b506428ffcd4564f68ee0adf18))
* **gawk-broadcast-windows:** 12 Mbps default, aspect-preserving encode, custom resolution, uplink warning ([2986a7f](https://github.com/Tuhis/gawk/commit/2986a7ffc842204b0648b4cb0e0082642d0bd78f))
* **gawk-broadcast-windows:** write debug.log so encoder refusals are diagnosable (F-8) ([10de84d](https://github.com/Tuhis/gawk/commit/10de84d76ad47bf878d17399adbd2adf7df618c4))


### Bug Fixes

* **gawk-broadcast-windows:** keyframe supersede livelock left joining viewers black (F-12) ([73b0adb](https://github.com/Tuhis/gawk/commit/73b0adb53e28abb583bd6c7fb33ee280b46b13a8))
* **gawk-broadcast-windows:** negotiate the NV12 input type from the MFT itself (F-9) ([3a3ad16](https://github.com/Tuhis/gawk/commit/3a3ad16fe92d23209ceb826aee7af2fdef49ee55))
* **gawk-broadcast-windows:** stack-blob PROPVARIANT heap corruption; contain panics; remove capture border (F-10/F-11) ([7c6a376](https://github.com/Tuhis/gawk/commit/7c6a376a87258c29ed7130f3711ce47eb83a3e01))

## 1.0.0 (2026-07-31)


### Features

* R34 native Windows broadcaster — WB0–WB8 ([#208](https://github.com/Tuhis/gawk/issues/208)) ([ec20a50](https://github.com/Tuhis/gawk/commit/ec20a5051ec78781fcfd285eea97e108deeef318))


### Bug Fixes

* trigger release ([f568e11](https://github.com/Tuhis/gawk/commit/f568e11b05048056d3633165fb9131c8bd54d28d))
