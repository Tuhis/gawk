# gawk-ios

The gawk iOS app for iPhone and iPad (iOS 27 and later): broadcast the
device screen, and watch any broadcast in a native player with fullscreen,
Picture-in-Picture and background audio. The design is
[docs/67](../docs/67-ios-app.md).

It is SwiftUI over a Rust core. The core reuses the desktop broadcasters'
`wire`, `engine`, `capture`, `encode` and `audio` crates by path, so the iOS
app speaks the same wire code and resume logic as the desktop apps.

```
rust/                    Rust workspace: core (the UniFFI surface), broadcast, viewer, devpub
app/project.yml          XcodeGen spec; the Xcode project is generated from it
app/Gawk/                the SwiftUI app; Design/ holds the tokens and primitives (docs/70)
app/GawkLiveActivity/    the Live Activity's widget extension (docs/70 D15)
app/GawkTests/           Swift unit tests, run in the Simulator
app/GawkUITests/         UI tests, against a local relay
scripts/build-core.sh    builds the core and its Swift bindings for one platform
scripts/check-theme.sh   Theme.swift's colours against gawk-app's and the desktop's tokens
scripts/room-fixtures.sh the rooms the UI tests watch and join
```

## Requirements

- macOS on Apple Silicon with **Xcode 27** and the iOS 27 Simulator runtime
  (`xcodebuild -downloadPlatform iOS`).
- [XcodeGen](https://github.com/yonaskolb/XcodeGen): `brew install xcodegen`.
- rustup. `rust/rust-toolchain.toml` pins the compiler and both iOS targets,
  and rustup installs them on first use. With Homebrew's keg-only rustup, put
  `$(brew --prefix rustup)/bin` on `PATH`.
- CMake, for the vendored libopus (`brew install cmake`). Builds set
  `CMAKE_POLICY_VERSION_MINIMUM=3.5` for its old CMakeLists;
  `build-core.sh` does that for you.
- git and network access to github.com on the first build: the viewer's
  libvpx is cloned from upstream and compiled from source (docs/67 OD12),
  using rustup's `llvm-tools`, which `rust-toolchain.toml` installs. Don't
  set `IPHONEOS_DEPLOYMENT_TARGET` for a host build (`cargo test`): clang
  reads it and builds the host libvpx for iOS, and the cached library then
  fails to link until `cargo clean -p shiguredo_libvpx`.

## Build and run

```sh
cd gawk-ios/app
xcodegen                     # writes Gawk.xcodeproj
open Gawk.xcodeproj          # Run builds the Rust core too
```

Or from the command line, unsigned, in the Simulator:

```sh
xcodebuild -project Gawk.xcodeproj -scheme Gawk \
  -destination 'platform=iOS Simulator,name=iPhone 17' \
  CODE_SIGNING_ALLOWED=NO test
```

The app's first build phase runs `scripts/build-core.sh`. It builds
`gawk-core` for the platform Xcode is building (`aarch64-apple-ios-sim` or
`aarch64-apple-ios`; dev profile for Debug, release otherwise) and
regenerates the Swift bindings from that library with UniFFI. The library,
the C module and the bindings land in `app/Build/` and `app/Generated/`;
both are git-ignored.

Running on a device needs a signing team, which phase D of the milestone
adds (docs/67 OD14). The Simulator needs none.

## Tests

```sh
cd gawk-ios/rust && cargo test --workspace     # the core, on the Mac
```

The shared desktop crates are path dependencies, not members, so their own
tests run from their workspace. CI also runs them the way this app links
them, with the engine's `self-update` feature off:

```sh
cd gawk-broadcast-desktop && cargo test -p gawk-wire -p gawk-engine --no-default-features
```

The tests that need a relay skip without one. To run them all, start a dev
relay with rooms, a broadcast and the room fixtures, then pass their
variables with xcodebuild's `TEST_RUNNER_` prefix, as `ios.yml` does:

```sh
gawk-server -addr 127.0.0.1:4499 -cert-file cert.pem -key-file key.pem \
  -publish-secret smoke -rooms -rooms-file gawk-ios/scripts/static-rooms.json \
  -max-broadcasts 120 -max-room-broadcasts 10 -broadcast-grace 60m
gawk-ios/rust/target/debug/gawk-devpub --url https://127.0.0.1:4499 --insecure --secret smoke
gawk-ios/scripts/room-fixtures.sh https://127.0.0.1:4499 smoke /tmp/rooms
```

The variables are `GAWK_SMOKE_RELAY_URL`, `GAWK_SMOKE_SECRET` and
`GAWK_SMOKE_ROOM_CODE` for the unit tests, and the `GAWK_UI_*` ones that
`GawkUITests/UITestSupport.swift` lists for the UI tests.

CI is [`ios.yml`](../.github/workflows/ios.yml).
