# gawk-broadcast-windows

The native **Windows** broadcaster: share one application — its window
plus **that app's own audio**, via WASAPI process loopback — or the whole
desktop, hardware-encoded through Media Foundation, straight to the gawk
relay over WebTransport. The design doc is
[docs/38](../docs/38-windows-native-broadcaster.md); read it before
changing anything here — every structural choice below is a numbered
decision there.

**Status:** implemented and CI-gated; the on-hardware acceptance pass on a
real gaming PC is what remains (docs/38 §10). All portable logic is
unit-tested cross-platform; the Windows halves are type-checked and linted
for `x86_64-pc-windows-msvc` in CI.

This is a Rust Cargo workspace, deliberately its own top-level component
with its own CI job and release line. No container, no Helm chart, no
deploy: a single binary you run on your own gaming PC.

## Getting a build

Grab `gawk-broadcast-windows-x86_64.exe` from the
**[Releases page](https://github.com/Tuhis/gawk/releases)**, or with the
GitHub CLI:

```
gh release download --pattern 'gawk-broadcast-windows-x86_64.exe'          # newest
gh release download gawk-broadcast-windows-v1.1.0 --pattern '*.exe'        # specific
```

(Releases newer than v1.1.0 are tagged `gawk-broadcast-windows/vX.Y.Z` —
the separator changed repo-wide in August 2026; older tags use the dash.)

`INSTALL.md`, `BUILD-INFO.txt` and `SHA256SUMS` are attached alongside it.
The EXE is unsigned by design (distribution is to known operators, docs/38
D17), so the checksum is the integrity check — and **SmartScreen will warn
about an unknown publisher on first run**: "More info" → "Run anyway".

For an *unreleased* build, every green CI run uploads an artifact:

```
gh run download --name gawk-broadcast-windows-x86_64-<commit sha>
```

**Which build am I running?** The window header shows `v1.0.0+g1a2b3c4` —
the release plus the commit it was built from. That matches the artifact
name and `BUILD-INFO.txt`, so a screenshot is enough to identify a build.
The same string is the first line of `debug.log` and the `appVersion` key
in **Copy diagnostics**. There is no `--version` flag: a windowed EXE has
no console to print to.

## Troubleshooting: `debug.log`

The app is a windowed EXE — nothing useful ever appears on stderr.
Instead every launch writes **`%APPDATA%\gawk\debug.log`** (next to
`broadcast.json`; the previous launch is kept as `debug.log.old`). It
records the adapter the D3D11 device landed on, the hardware encoder MFTs
enumerated, each candidate's trial verdict with the failing Media
Foundation step, and session lifecycle — enough to diagnose a "No hardware
H.264 encoder was found" refusal from the file alone. Error cards point at
it; attach it to any bug report.

## Defaults

Blank settings mean "the default", resolved at use, never at save
(docs/38 D13):

| Setting | Default |
|---|---|
| Relay | `https://api.gawk.ioio.fi:4433` |
| App URL (join links) | `https://gawk.ioio.fi` |
| Telemetry ingest | `https://gawk.ioio.fi/api/telemetry/v1/ingest` (`off` = send nothing) |
| Origin | `gawk-broadcast://windows` — the relay's `-allowed-origins` must include it |
| Rung | 1080p60, 500 ms GOP, 12 Mbps peak VBR — the resolution is a bounding box: the stream keeps the source's aspect ratio inside it |

## Building

Any host builds and tests the portable crates (wire, engine); the
Windows-only crates compile empty off-Windows so `cargo test` works
anywhere. The pinned toolchain in `rust-toolchain.toml` auto-installs via
rustup.

```
cargo test                                # golden vectors, chunking, defaults
cargo clippy --all-targets -- -D warnings
cargo build --release                     # target/release/gawk-broadcast(.exe)
```

Quirks that will bite you:

- **libopus needs `CMAKE_POLICY_VERSION_MINIMUM=3.5`** in the environment:
  the bundled source's CMakeLists predates CMake 4's floor. CI sets it;
  set it locally or `cargo test` dies in `audiopus_sys`'s build script.
- **The WARP conversion tests self-skip on hosts whose WARP lacks D3D11
  video** (hosted runners do). Run `cargo test -p gawk-capture` on a real
  Windows machine to actually execute them.
- Cross-type-checking from a non-Windows host works two ways. The quick
  local one is the `x86_64-pc-windows-gnu` target + mingw-w64. The one CI
  uses is **`cargo xwin` against `x86_64-pc-windows-msvc`**, which links
  the real shipped ABI; it needs `llvm` (for `llvm-lib`, or `ring` fails),
  a `/usr/bin/clang-cl` shim carrying `-mssse3 -msse4.1` (or libopus
  fails), and the MSVC SDK. All three are baked into the CI runner image —
  see docs/38 D18 before reproducing it by hand.

## Layout

| Crate | Role |
|---|---|
| `crates/wire` | The wire-format mirror — see below |
| `crates/engine` | Session lifecycle, send policy, resume supervisor. No GUI, no COM/WinRT; media enters through traits, the network through a `RelaySession` seam |
| `crates/capture` | Windows.Graphics.Capture frame source + window/monitor picker enumeration |
| `crates/encode` | Media Foundation hardware H.264 MFT cascade, trial-gated |
| `crates/audio` | WASAPI process/system loopback + Opus |
| `crates/app` | The Slint GUI shell — the only binary, `gawk-broadcast.exe` |

### The wire crate is a mirror, not an implementation

`gawk-server/wire/wire.go` is the source of truth; `gawk-app`'s `wire.ts`
and `gawk-broadcast/internal/wirecheck` are the other mirrors. The golden
vectors in `crates/wire/tests/golden.rs` are deliberately **restated as
literal hex**, never imported or generated — a shared fixture could be
edited once and stay green everywhere, defeating the purpose. Every wire
change lands in `wire.go` first and must be hand-mirrored here,
byte-identically. The GF(256) parity symbols are additionally pinned
against bytes generated by running the Go `ComputeParity` — the two
implementations must compute identical P/Q.

## Licensing: the one dependency that is a choice

`gawk-broadcast.exe` is Apache-2.0 like the rest of the repository, and
every crate it links is permissive — with one deliberate exception. The
GUI uses **Slint**, which is tri-licensed, and gawk takes the
**Royalty-free Desktop License v2.0** (docs/38 D12). That is what lets
this component stay Apache-2.0 instead of becoming the repository's one
GPL island. The license's one condition is attribution: the "Made with
Slint" badge in the [root README](../README.md#license) and on the
releases page.

Two other things a license scan will not tell you: **libopus** is
statically linked through `audiopus_sys` (ISC bindings over Xiph's
BSD-3-Clause C library), and the **MSVC C runtime** it links is
Microsoft's, governed by the Visual Studio license terms. Full inventory:
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).

`deny.toml` in this directory is what keeps the above true: CI runs
`cargo-deny check licenses` against it, and a copyleft crate arriving
through a dependency bump fails the build.
