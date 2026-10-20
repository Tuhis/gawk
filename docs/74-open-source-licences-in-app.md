# R72 — Open-source licences inside the native apps

**Status**: proposed and implemented 2026-10-20 in one PR (OL1–OL5), at the
owner's request ("show the list of used OSS with licenses in the native apps
including the iOS app"). OL6, the owner's look at the page on each platform,
is open. Status lives in [`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**: builds on the notices generator and
licence gate of the Apache-2.0 relicense (`tools/licenses/gen-notices.py`,
the `licenses` and `licenses-fresh` CI jobs, `deny.toml`), the per-platform
notices of [docs/54](54-macos-native-broadcaster.md) D14 and
[docs/58](58-linux-desktop-broadcaster.md) D12, and closes the item
[docs/67](67-ios-app.md) left to the distribution milestone ("libvpx is
BSD-3; its notice belongs to the distribution milestone"). It changes the
Settings page of the shared desktop window ([docs/54](54-macos-native-broadcaster.md)
D11: one `main.slint`) and of the iOS app ([docs/70](70-ios-ux-redesign.md)
D22). No wire, relay or engine-behaviour change.

---

## 1. Why

The permissive licences gawk's native apps are built on (MIT, BSD, ISC,
Apache-2.0, Zlib, Unicode-3.0) all require the copyright notice and licence
text to accompany copies of the software. BSD-3-Clause's wording is "in the
documentation and/or other materials provided with the distribution".
Apache-2.0 §4(d) adds any `NOTICE` file. None of them names an About
screen, but each attaches to *what the user receives*, and before R72 that
was only partly covered:

1. **Desktop: the notices were beside the binary, not in it.** The release
   uploads `THIRD-PARTY-NOTICES*.md` as separate assets next to
   `gawk-broadcast-windows-x86_64.exe` and `gawk-broadcast-macos-arm64.zip`.
   A user who downloads only the EXE or the zip gets none of them, and the
   self-updater ([docs/48](48-signed-in-place-update.md)) replaces the binary
   and never fetches them. The Linux `.deb` and tarball did carry the file.
2. **Desktop: libopus's own licence was missing.** Each blurb said "Xiph's
   BSD-3-Clause license covers the C library itself", but the text wasn't in
   the file: the generator lists Cargo packages, and libopus is a C library
   compiled in by `audiopus_sys`. Only the bindings' ISC licence was there.
3. **iOS: nothing.** R65 was signed for the owner's own devices (docs/67
   OD3), so nothing had been distributed. But on TestFlight or the App Store
   the `.ipa` is all a user receives, so the notices have to be inside the
   app. The iOS Rust graph had no notices file, and no `cargo-deny`
   gate either: the `licenses` job only judged `gawk-broadcast-desktop`.
   The iOS graph is not a subset of the desktop's. It adds libvpx (compiled
   from upstream source by `shiguredo_libvpx`) and UniFFI, which is
   **MPL-2.0** and was outside the allowlist.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **The generator covers iOS and the bundled C libraries.** `gen-notices.py` gains an `ios` component (`gawk-ios/THIRD-PARTY-NOTICES.md`, `cargo tree -e normal --target aarch64-apple-ios -p gawk-core`) and a `NATIVE_LIBRARIES` table that adds a `-sys` crate's C library whenever the crate is in a graph: libopus from `audiopus_sys`'s own `opus/COPYING`, and libvpx from `tools/licenses/native/libvpx/` (LICENSE and PATENTS, v1.17.0, committed because `shiguredo_libvpx` clones libvpx at build time and ships no text). The libvpx entry is pinned to the crate's `[package.metadata.external-dependencies.libvpx] version`, and the run fails if a bump moves it. Packages that declare a licence and ship no text (the uniffi crates) get it from `tools/licenses/texts/`. | One generator, one freshness check, and Renovate's postUpgradeTasks already regenerates on every bump. Pinning the vendored text to the version the crate builds is what keeps a libvpx bump from shipping an old notice. `-p gawk-core` because the iOS workspace also holds the bindgen and a dev publisher, which ship nothing. |
| D2 | **Each app embeds its own file, and one parser reads it.** The desktop shells pass theirs through a new required `Platform::notices()` (`include_str!` in `app-windows`, `app-macos`, `app-linux`). The iOS core embeds `gawk-ios/THIRD-PARTY-NOTICES.md` and exposes `third_party_notices()` over UniFFI. Both read it with `gawk_engine::notices::parse`, which reads the generator's own layout: Packages table, Source code table, licence texts. | Embedding is what makes the notices survive a single-file download and a self-update. The shell picks the file, as it picks its identity (CLAUDE.md, docs/58 D2), never the target OS. A required trait method means a fourth shell can't forget it. One parser in the shared `engine` serves both apps and is tested on Linux against every committed file, so a generator layout change fails a Rust test instead of shipping an empty page. iOS already path-depends on `engine` (docs/67 OD9). |
| D3 | **The page: Settings → Open-source licences.** Desktop: an **APP** row with **View**, opening page 5 of the window (a virtualised `ListView`). iOS: a row in **About** pushing a list with a detail screen per licence text. Both show the same content in the same order: a line that gawk is Apache-2.0 and built on what follows, the generator's blurb, every package with its version, licence and copyright, the MPL sources (iOS), then each distinct licence text with what it applies to. Parsed on first open, not at launch. | Where both platforms' users look for it. The file is a few hundred KB and few users open the page, so it is not parsed at start-up. |
| D4 | **UniFFI's MPL-2.0 is accepted as a scoped `cargo-deny` exception**, in a new `gawk-ios/rust/deny.toml` that the `licenses` job runs. The exception is per crate (`uniffi`, `uniffi_core`, `uniffi_bindgen`, …), so another MPL crate still fails. The notices get a **Source code** section linking each MPL package's crates.io source (§3.2(a)). | MPL-2.0 is file-level copyleft. It binds the MPL files, which gawk uses unmodified, not the app (§3.3 allows a Larger Work under other terms). In executable form its one extra duty is saying where the source is, and the Source code section does that. UniFFI was already linked (docs/67 D3); this records the decision the missing gate had skipped. |
| D5 | **Release assets unchanged.** The notices still ship beside the EXE, the zip and in the `.deb`/tarball. | Harmless, and it keeps the attributions readable without running the app. |

### Not done

- **Slint's attribution** stays as it was (docs/38 D12: the §2(b) badge on
  the README and release page). The page lists the Slint crates and their
  licence texts like any others, which is more than the royalty-free licence
  asks for.
- **An `AboutSlint` widget** is not added; D12 chose the badge.
- **The iOS Settings bundle** (`Settings.bundle` acknowledgements in the
  system Settings app) was not used: a page in the app is where gawk's own
  Settings already are, and it needs no second generated format.

## 3. Acceptance criteria

| Goal | Verified by |
|---|---|
| The generator writes `gawk-ios/THIRD-PARTY-NOTICES.md`, and the three desktop files list `libopus` with Xiph's text | `python3 tools/licenses/gen-notices.py windows macos linux ios`; the committed files; `licenses-fresh` |
| libvpx's notice can't go stale silently | `native_library()` exits non-zero when `shiguredo_libvpx`'s pinned libvpx version differs from `NATIVE_LIBRARIES` |
| The iOS graph is gated on the same allowlist; only the named uniffi crates may be MPL-2.0 | `cargo-deny check licenses` in `gawk-ios/rust` (the `licenses` job) |
| MPL packages say where their source is | `## Source code` in the iOS file; `the_committed_ios_notices_parse` |
| Every committed file parses whole: a row per package (= the summary's count), a licence on each, every text non-empty, the C libraries present | `gawk_engine::notices` tests (`the_committed_*_notices_parse`) |
| Each desktop binary embeds its own platform's file, and a shell can't omit it | `Platform::notices()` is required; the three `include_str!`s |
| Settings → Open-source licences opens a page listing every package and text | `gawk_ui::licences` tests (row per package, text and source); desktop: the owner's pass (OL6) |
| The iOS app carries its file across the boundary and shows it | `gawk-core` `the_app_shows_its_own_file_whole`; `CoreTests.testTheCoreCarriesTheAppsNotices`; `SettingsUITests.testSettingsShowsTheOpenSourceLicences` |

## 4. Chunks

| Chunk | What | Criteria |
|---|---|---|
| OL1 | Generator: the `ios` component, `NATIVE_LIBRARIES` (libopus, libvpx), the MPL text fallback and Source code section, vendored-crate detection by path; files regenerated | First two rows of §3 |
| OL2 | `gawk-ios/rust/deny.toml` with the scoped uniffi exception; the `licenses` job runs it | Third row |
| OL3 | `gawk_engine::notices` parser and its tests over every committed file | Fifth row |
| OL4 | Desktop: `Platform::notices()`, the Settings row, page 5, `gawk_ui::licences` | Sixth and seventh rows |
| OL5 | iOS: `third_party_notices()` in the core, `LicencesScreen`, the About row, tests | Last row |
| OL6 | The owner opens the page on Windows, macOS, Linux and an iPhone | Visual: every package and text present and readable |

## 5. Risks

- **The parser reads the generator's layout, not Markdown.** A generator
  change that renames a section or reshapes a table breaks the page. The
  committed-file tests make that a red build in the desktop workspace,
  which `licenses-fresh` regenerates into on the same PR.
- **libopus's version is not pinned by metadata.** `audiopus_sys` ships
  opus 1.3 in its crate and declares no version key, so the `1.3` in
  `NATIVE_LIBRARIES` is read from its README. The text itself is read from
  the crate on every run, so a bump changes the text automatically, not the
  label.
- **Linux may link the system libopus.** `audiopus_sys` uses pkg-config
  when it finds opus. Listing Xiph's notice anyway over-attributes; it never
  under-attributes.
