#!/bin/bash
# Builds the Rust core for one iOS platform and regenerates its Swift
# bindings (docs/67 D2, D3). Xcode runs it as the app's first build phase,
# so Run in Xcode builds Rust too; CI and a fresh checkout can run it by hand:
#
#   gawk-ios/scripts/build-core.sh iphonesimulator   # or iphoneos
#
# Outputs, all under gawk-ios/app/Build/ (git-ignored):
#   rust/<platform>/libgawk_core.a   linked into the app
#   ffi/                             the C header + module map Swift imports
# and app/Generated/gawk_core.swift, compiled into the app.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ios="$(dirname "$here")"
rust="$ios/rust"
out="$ios/app/Build"

# Xcode runs build phases with a minimal PATH; rustup may live in either of
# these (brew's rustup is keg-only).
export PATH="$HOME/.cargo/bin:/opt/homebrew/opt/rustup/bin:/opt/homebrew/bin:$PATH"
# The vendored libopus's CMakeLists predates CMake 4's floor (gawk-audio).
export CMAKE_POLICY_VERSION_MINIMUM=3.5

platform="${1:-${PLATFORM_NAME:-iphonesimulator}}"
case "$platform" in
  iphonesimulator) target=aarch64-apple-ios-sim ;;
  iphoneos)        target=aarch64-apple-ios ;;
  *) echo "error: unsupported platform '$platform'" >&2; exit 1 ;;
esac

# Debug builds of the app link a dev-profile core; everything else release.
if [ "${CONFIGURATION:-Debug}" = "Debug" ]; then
  profile=dev; dir=debug
else
  profile=release; dir=release
fi

# Xcode exports its own SDKROOT (the iOS SDK) into the phase's environment.
# Cargo compiles build scripts and proc macros for the Mac, and those links
# fail against an iOS SDK, so the iOS SDK must reach only the target build,
# which cc-rs and rustc find by target triple on their own.
unset SDKROOT
export IPHONEOS_DEPLOYMENT_TARGET=27.0

# From inside the workspace: rustup picks rust-toolchain.toml by directory,
# and UniFFI's library mode runs `cargo metadata` in the current one.
cd "$rust"
target_dir="${CARGO_TARGET_DIR:-$rust/target}"
cargo build -p gawk-core \
  --target "$target" --profile "$profile"

lib="$target_dir/$target/$dir/libgawk_core.a"
mkdir -p "$out/rust/$platform" "$out/ffi" "$ios/app/Generated"

# Bindings come from the library's own metadata (UniFFI library mode), so
# they can never describe a different build than the one being linked.
gen="$(mktemp -d)"
trap 'rm -rf "$gen"' EXIT
cargo run --quiet -p uniffi-bindgen -- \
  generate --library "$lib" --language swift --out-dir "$gen"

# Only touch the outputs when they changed, so an unchanged core does not
# make Xcode recompile every Swift file that imports it.
sync() { cmp -s "$1" "$2" || cp -f "$1" "$2"; }
sync "$lib" "$out/rust/$platform/libgawk_core.a"
sync "$gen/gawk_core.swift" "$ios/app/Generated/gawk_core.swift"
sync "$gen/gawk_coreFFI.h" "$out/ffi/gawk_coreFFI.h"
sync "$gen/gawk_coreFFI.modulemap" "$out/ffi/module.modulemap"
