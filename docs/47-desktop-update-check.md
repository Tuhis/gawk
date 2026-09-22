# R45 — Update notification for the desktop broadcasters (docs/47)

**Status**: designed 2026-09-15; **not started**. Chunks **AU1–AU5** (`AU` =
Auto-Update; two-letter prefix per the R21+ convention). Client code only:
the version source shipped with R46 (docs/46 D1, D5). Nothing here touches
the wire format, the relay, the engine pipeline or the browser app.
Installing the update from inside the app is a separate milestone, **R47**
([docs/48](48-signed-in-place-update.md)), split out 2026-09-15 because it is
gated on a release signing key that does not exist today; this document is
written so R47 builds on it without reopening anything here.

**2026-09-22 (R52 MB1)**: a third desktop distribution exists —
`gawk-broadcast-macos`, asset `gawk-broadcast-macos-arm64.zip`, manifest
`releases/gawk-broadcast-macos/latest.json` ([docs/54](54-macos-native-broadcaster.md)
D14/D17) — and D5's per-platform table gains that row. The Rust side's
identity per distribution (name, origin, asset, os) now lives in
`gawk-broadcast-desktop`'s `engine::defaults::{WINDOWS, MACOS, THIS}`, so
AU3's validator compares against `defaults::THIS.name` and
`defaults::THIS.asset` rather than restating the Windows strings; the
workspace itself was renamed from `gawk-broadcast-windows` (R52 MB0), which
changes nothing in the manifest names above.

## 1. Purpose

Both desktop broadcasters are distributed as fixed-name GitHub Release assets
that someone downloads once. Both show their build in the window
(`gawk-broadcast-gui` top-right, `cmd/gawk-broadcast-gui/main.go:655`;
`gawk-broadcast.exe` status card, `crates/app/ui/main.slint:287-295`) and
report the bare release to telemetry, but nothing anywhere compares that
against what is published. Every fix to the encoder cascade, the portal path
or a wire mirror reaches only the people who happen to re-download. The relay
and the web app are redeployed cluster-side on every release; the desktop
apps are the one place a release does not arrive.

docs/38 OD6 lists auto-update as a non-goal for R34 and pencils in "a
version-check toast" as a later convenience. This is that item, with its
scope decided: a check at launch and an in-window notice in both apps that
links to the release page. The user still downloads by hand; R47 removes
that step.

What already exists, so the reader does not go looking:

- **The version source.** `releases/<component>/latest.json` on the orphan
  `badges` branch, written by the attach jobs only after a successful attach
  (docs/46 D2), validated by `tools/releases/manifest.py validate`, served by
  `raw.githubusercontent.com` over TLS with no API and no quota. Its shape is
  docs/46 D5: `schema`, `component`, `version`, `tag`, `published_at`,
  `release_url`, the primary `asset` and every attached file under `assets`
  with `size`, `sha256`, `url`.
- **The compiled-in version.** `version.Release` in
  `gawk-broadcast/internal/version/version.go:42` and `version::RELEASE` in
  `gawk-broadcast-windows/crates/app/src/version.rs:24`, both maintained by
  release-please (`release-please-config.json` extra-files). Both are bare
  `X.Y.Z`; the displayed string appends `+g<sha>` (and `.dirty` on Linux)
  as SemVer build metadata precisely so it never affects precedence.
- **An HTTP client.** Go's `net/http` (the telemetry reporter uses it,
  `internal/telemetry/reporter.go:180`) and `ureq 3` on rustls in
  `gawk-engine` (`crates/engine/src/telemetry.rs:382-398`). Neither app
  sets a `User-Agent` today.
- **A settings store** each, same JSON shape and key style
  (`~/.config/gawk/broadcast.json`, `%APPDATA%\gawk\broadcast.json`), both
  tolerant of unknown keys, both saved atomically.
- **A way to open a URL** in each GUI (`openInBrowser`, `main.go:595`;
  `open_in_browser`, `main.rs:1786`, exposed to Slint as `open-link`).

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **The source is the R46 manifest at a compiled-in URL**: `https://raw.githubusercontent.com/Tuhis/gawk/badges/releases/<component>/latest.json`, with `<component>` = `gawk-broadcast` or `gawk-broadcast-windows`. A constant next to the default relay URL, not a runtime setting. The relay's `RelayIdentity` (0x11) is **not** a version source. | docs/46 D1 already argued the manifest against the GitHub API. A relay must never steer which binary a broadcaster runs — a third-party relay (docs/40) is a peer, not an authority, and the update check must behave identically whichever relay is selected. Like `SITE_URL` in the web app (docs/46 §6), the URL describes the project every build comes from; a fork edits the constant. |
| D2 | **The request carries nothing identifying.** A plain `GET` of the static file: no query string, no conditional headers (`If-None-Match`, `If-Modified-Since`), no cookies, and a **fixed** `User-Agent` of `gawk-broadcast-update-check` with no version in it. 10 s timeout, no retry, failure is silent (debug log only). | Roadmap owner decision 2026-09-10. This is the first outbound request the apps make that is neither to a relay nor to telemetry (which is default-off outside the default fleet), so the bar is "a static file fetch that reveals nothing but that a gawk broadcaster exists". A conditional header would encode which version the app last saw; the file is under 2 KB, so re-fetching it costs nothing. A version-bearing UA is exactly the leak the rule forbids. |
| D3 | **Once at launch, at most once per 24 h, no periodic re-check.** The check runs after the config is loaded and the window is up (GUI) or concurrently with the dial (CLI), off the UI thread. `lastUpdateCheck` (RFC 3339 UTC) in the config gates it; a check that fails to reach the manifest does not advance the timestamp. Nothing schedules a later check, so no check ever starts mid-broadcast; the timestamp is the only state. | One small request per day per machine is the whole budget. The CLI starts broadcasting immediately, so "at launch" and "while broadcasting" coincide there; a 2 KB GET on a broadband uplink is not contention, and the rule the roadmap actually protects — nothing *repeats* during a live session — holds. |
| D4 | **Compare release parts only, numerically, strictly greater.** Parse `X.Y.Z` from `version.Release` / `version::RELEASE` and from `manifest.version`; notify only when the manifest is strictly newer. Equal or older says nothing. No SemVer library: three integers, hand-parsed, in each language. | A `1.14.0+g1a2b3c4.dirty` dev build of the current release must never be nagged about itself (roadmap). The manifest writer already rejects prereleases and build metadata (`manifest.py:37`), so the client never sees them; a manifest that fails the `X.Y.Z` regex is treated as invalid (D5). An *older* manifest is what a retraction looks like (D11) and is not a downgrade prompt. |
| D5 | **Validate the manifest like the site does, and treat any failure as "no update".** `schema == 1`; `component` equals the app's own; `version` matches `^\d+\.\d+\.\d+$`; `release_url` starts with `https://github.com/Tuhis/gawk/releases/tag/`; `asset.url` starts with `https://github.com/Tuhis/gawk/releases/download/`; `asset.name` equals the platform's fixed asset name (`gawk-broadcast-linux-amd64.tar.gz`, `gawk-broadcast-windows-x86_64.exe`); `asset.sha256` is 64 hex. Unknown keys are ignored. The shape check is pinned in each app's tests against the docs/46 D5 example — restated, not shared, the `wirecheck` convention. | `site/site.js:26-32` is the precedent; the consumer restates the contract so a writer change is loud on the consumer side. Nothing from the manifest is executed or rendered as markup; the only things the app does with it are compare a version and open a URL it has prefix-checked. |
| D6 | **Opt-out lives in the config, with a CLI flag and an env for the Linux CLI.** Config key `disableUpdateCheck` (bool; zero value = on, the `disableAudio` convention). Linux CLI: `-no-update-check` (one-shot, does not write the config) and `GAWK_NO_UPDATE_CHECK=1`. Both GUIs: a **"Check for updates at launch"** checkbox in the settings card, persisted to the key. Windows also honours `GAWK_NO_UPDATE_CHECK=1` — it is read with `std::env::var_os`, no console needed — but gets no flag: the EXE is `windows_subsystem = "windows"` and parses no arguments (docs/38 "No `--version` flag"). | The Linux GUI parses no flags at all (`cmd/gawk-broadcast-gui/main.go:77-104`), so a GUI-reachable opt-out must be a config key. One key name in both stores keeps the two apps' `broadcast.json` mutually readable, which they are today. Default on is the owner decision; the checkbox is the first persisted boolean either GUI wires end to end, and the pattern to copy is the R37 server picker (Slint: `checked <=> root.set-update-check` + `settings-edited`; Gio: a `widget.Bool` on `ui` read in `handleEvents`). |
| D7 | **The notice is an in-window line under the version badge; no toast.** Both GUIs: a muted, clickable line **"v1.15.0 available — release notes"** that opens `release_url`, with a **"dismiss"** affordance beside it. The Linux CLI logs one `slog` line at info: `update available` with `current`, `latest`, `url`. Dismissal is per version: `dismissedUpdateVersion` in the config; a newer version than the dismissed one shows again. | Toasts are unreliable exactly when a broadcaster is at the keyboard: KDE's portal inhibits normal-urgency notifications for the whole ScreenCast session (`internal/notify/notify.go:1-22`) and Focus Assist eats them during a fullscreen game (`gawk-broadcast-windows/INSTALL.md:56-57`). An update is a normal-urgency fact; escalating it to critical to get through would be abuse of the channel. The in-window line is where the version already is and where the screenshot question gets asked (`main.go:636-641`). |
| D8 | **Where the logic lives.** Go: a new `internal/update` package (fetch, validate, compare, the config-gated `Due`/`Record` helpers) with an `http.Client` injection seam, state held in `internal/app` behind the mutex the GUI already reads (`App.State()`, `App.Stats()`) and surfaced with `Invalidate`. Rust: `crates/engine/src/update.rs`, shell-free, taking the current release as a `&str` the way `Reporter::new(version::RELEASE, …)` does, with the URL constant in `engine::defaults`; the app crate wires it to Slint properties (`update-version`, `update-url`) and callbacks (`open-link`, `dismiss-update`). | Mirrors where the telemetry reporter sits in each tree — the one existing "speaks HTTP, has no GUI" module — and keeps `gawk-engine`'s "no GUI, no COM/WinRT" contract (`lib.rs:6-11`) so a future Windows CLI shares the check. Tests: `httptest.Server` in Go (the `reporter_test.go` fixture), the one-shot `TcpListener` server the telemetry tests use in Rust (`telemetry.rs:462-470`); no new dev-dependency. |
| D9 | **No new dependencies, no toolchain surface.** Go: stdlib only. Rust: `ureq 3` is already in `gawk-engine`, on rustls with `webpki-roots`; the app crate calls the engine function rather than adding an HTTP client of its own. No `semver` crate (D4). | docs/38 D18: the Windows build cross-compiles on Linux with cargo-xwin and clang-cl, and `native-tls`/`schannel` would drag in a Windows SDK link surface the runner image does not carry. rustls is what already ships and needs nothing. Adding any crate trips `licenses` and `notices` in CI; not adding one trips neither. |
| D10 | **`-insecure` does not reach the update client, and neither does the relay's TLS config.** The check uses the platform trust store (Go) / the bundled Mozilla roots (rustls) and verifies normally, always. | `-insecure` exists for a dev relay's self-signed cert (`internal/engine/relay.go:157-170`) and is scoped to that dial; the telemetry client already ignores it. An update source that could be pointed at a MITM by a dev flag would undermine R47 before it exists. |
| D11 | **A bad release is retracted by republishing the manifest at the previous version, plus a fix release.** `tools/releases/manifest.py build` against the previous release's downloaded assets, pushed to `badges` (the docs/46 D7 seeding procedure) stops new downloads; a `fix:` release is what reaches people who already have the bad build. The notice never says "downgrade". | The manifest is a pointer, and the only thing it can retract is *future* traffic; the clients that already updated can only be moved forward. This is also why D4 ignores an older manifest: the retraction state must not read as a prompt. |
| D12 | **Terms.** The R23 terms (`docs/29`, `BundledTerms.tsx` §7) describe "the Service", and the desktop apps link to them from their settings cards. The update check is a request to GitHub, not to the Operator, so the edit is one added sentence in §7: the native broadcaster applications may fetch a small static release-manifest file from GitHub at launch to learn whether a newer version exists; the request carries no identifier; it can be turned off in the application's settings. **Recommendation: do not bump `termsVersion`.** | The sentence discloses a separate application's behaviour and changes nothing the Service does to a web user; re-prompting every broadcaster for it would be noise. This is the one item the owner should confirm before AU5 lands. |

### Rejected

- **The GitHub Releases API** as the version source — docs/46 D1 (wrong
  release in a monorepo; rate-limited per IP unauthenticated).
- **A relay-advertised version** via `RelayIdentity` or a new wire type — a
  relay must not choose the broadcaster's binary, and it would put the check
  on every relay the user might add (docs/40).
- **A minimum client version the relay enforces** — compatibility is
  handled by the wire mirrors' golden vectors and close codes, never by
  refusing old clients (roadmap non-goal).
- **A toast as the only surface** — D7.
- **Conditional requests** (`If-None-Match`) to save bandwidth — D2; the
  file is tiny and the header is a version hint.
- **Downloading the asset in this milestone** ("download ready, open the
  folder") — without R47's signature check the app would be fetching an
  executable it cannot verify beyond a hash served over the same TLS
  path; the release page in the browser is the honest surface until then.

## 3. Where it plugs in

| Piece | Where it is today | What AU changes |
|---|---|---|
| Version source | `.github/actions/publish-release-manifest` → `badges:releases/<component>/latest.json` (docs/46) | Nothing. |
| Compiled-in version | `internal/version/version.go:42` `Release`; `crates/app/src/version.rs:24` `RELEASE` | Read, never written. The compare uses these, not `String()` / `display()`. |
| Linux logic | — | **New** `gawk-broadcast/internal/update`: `Manifest`, `Fetch(ctx, client, url)`, `Validate`, `Newer(current, latest) bool`, `Due(cfg, now) bool`. |
| Linux config | `internal/config/config.go` `Config` struct, `Load`/`Save`/`Migrate` | Three `omitempty` keys: `disableUpdateCheck` (bool), `lastUpdateCheck` (string, RFC 3339), `dismissedUpdateVersion` (string). No migration needed. |
| Linux CLI | `cmd/gawk-broadcast/main.go:48-126` flag set + env merge; `:166-175` startup log | `-no-update-check` bool + `GAWK_NO_UPDATE_CHECK`; a goroutine after the config load; one info line when newer. |
| Linux GUI | `cmd/gawk-broadcast-gui/main.go` `header()` `:633-731` (badge at `:655`), `settings()` `:1082-1180`, `handleEvents` `:411-512`, `openURL` seam `:181` | A conditional `layout.Rigid` under the badge (the encode-line idiom, `:682-711`) with two clickables; a `widget.Bool` checkbox in settings; state via `internal/app` (`App.Update()` + `Invalidate`). |
| Windows logic | — | **New** `crates/engine/src/update.rs` + `defaults::UPDATE_MANIFEST_URL`; mirrors the Go package's functions. |
| Windows config | `crates/engine/src/config.rs` `Config` (`serde(default)`, camelCase) | The same three keys. |
| Windows GUI | `crates/app/ui/main.slint:287-295` version text; settings card `:606-613`; `seed_settings` `main.rs:383`, `read_settings` `:420`, `on_settings_edited` `:671`; `open_in_browser` `:1786` | `update-version` / `update-url` properties and a clickable `Text` under the badge; `dismiss-update` callback; a `CheckBox` bound to `set-update-check`; env opt-out read at startup. |
| Docs | both READMEs' diagnostics sections (the Windows INSTALL.md was removed 2026-09-21; the Linux one keeps only its one-line opt-out); the R23 terms `BundledTerms.tsx` §7 | One paragraph each naming the check, the destination (GitHub), what it carries (nothing), and the opt-out. The terms gain one sentence (D12). |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **AU1** | `gawk-broadcast/internal/update` (D1, D2, D4, D5, D8) and the three config keys (D6, D7) | Unit tests with `httptest.Server`: a valid manifest newer than `Release` → `Newer` true; equal, older, a `+g` current build against its own release → false; each D5 failure (bad schema, wrong component, prerelease version, foreign `release_url`, wrong asset name) → "no update", never an error surfaced; the request has no query string, no `If-*` headers and UA exactly `gawk-broadcast-update-check`; `Due` is false within 24 h of `lastUpdateCheck`, true after, true when unset; a transport failure leaves `lastUpdateCheck` unchanged. `config_test.go`: the three keys round-trip and a file without them loads as "check on, never checked, nothing dismissed". |
| **AU2** | Linux CLI flag/env + startup line; Linux GUI line, dismiss, settings checkbox, `internal/app` state (D3, D6, D7) | `main_test.go` (GUI harness): with an update recorded, `idleFrames` shows the line and requests no free-running redraw; clicking it calls the `openURL` seam with `release_url`; dismiss hides it and writes `dismissedUpdateVersion`; the checkbox toggles `disableUpdateCheck` in the saved config. CLI: `-no-update-check` and `GAWK_NO_UPDATE_CHECK=1` make no request (the test client's handler is never hit); `-version` still prints before any network. `go test -race ./...` green; coverage floor holds. |
| **AU3** | `crates/engine/src/update.rs` + `defaults::UPDATE_MANIFEST_URL` + config keys (D1, D2, D4, D5, D8, D9) | The same table as AU1 as inline `#[cfg(test)]` tests against the one-shot `TcpListener` server; the manifest deserializer ignores unknown keys; a `version.rs`-style self-check pins `UPDATE_MANIFEST_URL` to the `component` string `gawk-broadcast-windows`; `Cargo.lock` unchanged (no new crate). `cargo clippy -D warnings` on both the msvc target and the host pass. |
| **AU4** | Windows GUI: Slint line + dismiss + checkbox + env opt-out (D6, D7) | `seed_settings`/`read_settings` round-trip the checkbox; `GAWK_NO_UPDATE_CHECK=1` makes no request; the line is a `Text` under the version badge that fires `open-link` with `release_url`; manual: on a Windows 10 VM with a manifest ahead of the build, the line appears within a few seconds of launch and opens the release page in the default browser. |
| **AU5** | Docs: both READMEs, the terms sentence (D12), `docs/gotchas.md`, this document's status, the ROADMAP row and entry | Review; `TermsPage.test.tsx` / `sanitize.test.ts` still pass; the READMEs' diagnostics sections name three destinations (relay, telemetry, GitHub) and the opt-out for each. |

Success criterion, end to end: run a `gawk-broadcast` built from a release
commit whose `Release` is one minor behind `latest.json` — the GUI shows
"vX.Y.Z available — release notes" under its badge within seconds of launch
and opens the right release page; dismissing it survives a restart; the CLI
prints one `update available` line; a build of the current release shows
nothing; `-no-update-check` makes no connection (verify with
`strace -e trace=connect` or a packet capture: the only peers are the relay
and, if enabled, telemetry).

## 5. Security considerations

- **This milestone executes nothing and trusts nothing.** The manifest
  yields one comparison and one URL, prefix-checked against the project's
  own releases path before it is handed to the browser. A manifest an
  attacker could write (repository write access) can at worst show a
  version number and open a page under `https://github.com/Tuhis/gawk/releases/`.
- **The request reveals nothing but its existence** (D2). No version,
  no ID, no conditional header, fixed UA, TLS with normal verification, and
  `-insecure` cannot weaken it (D10). Frequency is once a day per machine.
  The opt-out is in the config, the CLI and the environment (D6).
- **Nothing is written outside the config file.** No download, no
  staging directory, no change to the running binaries.

## 6. Operations

- **Retracting a release** (D11): download the previous release's assets,
  run `tools/releases/manifest.py build` for that tag, push to `badges`
  (docs/46 D7 has the exact commands), then cut a `fix:` release.
- **A release with no manifest** — an attach job that did not run (the
  `needs:` rename trap in docs/38, or a failed build) writes no manifest
  (docs/46 D2), so every client keeps seeing the previous version. That
  is the intended failure mode: a client is never pointed at a release
  without bytes. It also means "nobody is being told about vX" is the
  first thing to check when a release does not seem to reach anyone.
- **Staleness**: `raw.githubusercontent.com` may serve a copy a few
  minutes old. Against a daily check that is invisible; nothing in the
  client tries to defeat it (D2).
