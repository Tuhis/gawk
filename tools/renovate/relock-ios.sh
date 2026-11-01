#!/bin/sh
# Re-lock the iOS Rust workspace after a bump in a desktop crate it shares.
#
# gawk-ios/rust uses the desktop workspace's wire, engine, capture, encode and
# audio crates by path (docs/67 OD9), so a bump in one of their Cargo.toml
# files changes the iOS graph too. Renovate only refreshes the lock file of
# the workspace it edited (gawk-broadcast-desktop/Cargo.lock), which left
# gawk-ios/rust/Cargo.lock stale and the ios workflow's xcode jobs failing at
# `cargo fetch --locked` (PRs #642, #648, #649). Renovate runs this before
# gen-notices.py; it is equally the command to run by hand after editing a
# shared crate's dependencies.
#
# `cargo update --workspace` is the minimal re-lock: it resolves only what the
# manifests no longer agree with and leaves every other locked version where
# it is, so a clean tree is a no-op and a bump moves only the bumped crate and
# what it newly drags in. Lock file maintenance remains what moves the rest.
set -eu
cd "$(dirname "$0")/../.."

cargo update --workspace --manifest-path gawk-ios/rust/Cargo.toml
