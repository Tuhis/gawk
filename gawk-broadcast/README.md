# gawk-broadcast — gawk-pubsim test tooling

[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2FTuhis%2Fgawk%2Fbadges%2Fgawk-broadcast.json)](../docs/43-coverage-reporting.md)

This module used to be the native Linux broadcaster, written in Go (R14,
[`docs/19`](../docs/19-linux-native-broadcaster.md)). That app was replaced
by `gawk-broadcast-linux` in
[`gawk-broadcast-desktop`](../gawk-broadcast-desktop/README.md) and removed
on 2026-10-08 (R56 LX9, [`docs/58`](../docs/58-linux-desktop-broadcaster.md)
D15). Its last release is `gawk-broadcast/v1.15.3`.

What is left is test tooling, and none of it is released:

- **`cmd/gawk-pubsim`**: a simulated publisher. It loops a committed H.264
  and Opus fixture through the real publisher engine (announce, resume
  token, reliable keyframe streams, delta datagrams, TimeSync, rooms), so
  the dev stack, the `e2e` and `e2e-cluster` CI tiers and the iOS viewer
  tests need no GPU ([`docs/25`](../docs/25-e2e-testing-in-ci.md) Decision 3).
  Its package comment lists the flags and the machine-readable output.
- **`internal/engine`, `fixture`, `mpegts`, `opus`, `pubsim`**: what pubsim
  runs on.
- **`internal/wirecheck`**: one of the wire mirrors. Its golden vectors
  restate `gawk-server/wire`'s byte for byte (`CONTRIBUTING.md`).

pubsim dials with the Origin `gawk-broadcast://native` (`engine.DefaultOrigin`),
so a relay that restricts origins must allow it for pubsim to publish.

## Build and test

Pure Go, no cgo and no system headers:

```sh
CGO_ENABLED=0 go build ./cmd/gawk-pubsim
go test ./...
go test -short ./...    # skips the tests that build and run the real relay
```

The engine's integration tests build and run the real `gawk-server` and
publish the fixture through it.
