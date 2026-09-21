# R47 — Signed in-place update for the desktop broadcasters (docs/48)

**Status**: designed 2026-09-15; **not started**. Chunks **SU1–SU5** (`SU` =
Signed Update; two-letter prefix per the R21+ convention). **Depends on
R45** ([docs/47](47-desktop-update-check.md)): this milestone turns R45's
"vX.Y.Z available" line into an "install and relaunch" button, and reuses
its manifest fetch, validation, compare, opt-out and dismissal unchanged.
Gated on one prerequisite that does not exist today: **a release signing
key** (D1). Split out of R45 on 2026-09-15 so the notify-only half can ship
while the signing question is settled.

## 1. Purpose

R45 tells a broadcaster that a newer build exists and opens the release
page. The user then downloads a tarball or EXE, replaces the files they
unpacked weeks ago, and re-runs — the exact step that keeps every fix from
reaching most of them. This milestone does that step from inside the app:
download, verify, replace, relaunch on click.

The reason it is a separate milestone is trust. Both binaries are unsigned
by design (docs/38 D17; the Linux release job's own comment: "the binaries
are unsigned by design, so a checksum is the only integrity check a
downloader gets"). That is acceptable for a manual download from a page the
user is looking at. It is not acceptable for code that replaces itself over
a chain whose only trust anchor is TLS to `raw.githubusercontent.com` and
`github.com`: anyone who can write the `badges` branch — repository write
access today, any broader-scoped token tomorrow — could point every
broadcaster at an arbitrary binary. So the release set gets a signature
first, and the installer verifies it before it touches a file.

What R45 leaves in place for this milestone to use: the manifest's `assets`
map, which lists every attached file with `size`, `sha256` and `url`
(docs/46 D5, "so R45 phase 2 can fetch and verify by name"); the D5
prefix checks on every URL; the D4 strictly-newer compare; `App.State()` /
`busy` as the "is a broadcast live" gate; and the settings card's disabled
state while live (docs/19 Decision 9).

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Prerequisite: detached minisign (ed25519) signatures over `SHA256SUMS`, produced in CI, verified against public keys compiled into the apps.** Both attach jobs gain a signing step: `SHA256SUMS.minisig` is attached beside `SHA256SUMS`, and therefore appears in `manifest.assets` with no change to the R46 writer. The secret key and its passphrase are repository secrets used only in the attach jobs. **docs/38 D17 is revised in place with a dated note for this milestone**: the binaries stay unsigned in the Authenticode sense; the *release set* becomes signed. | Signing one file (`SHA256SUMS`) instead of each asset keeps the CI step to one command and the client to one signature plus one hash. Nothing about `badges` publishing changes, so docs/43 §7's "CI writing to the repo cannot loop" argument is untouched — it hinges on `GITHUB_TOKEN`, which a signing secret does not replace. |
| D2 | **Two public keys compiled in, current + next; rotation is a release.** The minisign key ID in the signature selects which compiled-in key verifies. Rotating: publish a release signed by "next", then ship a build whose keys are (next, new-next). A build that knows neither key ID refuses to install and falls back to the R45 notice. | A single compiled-in key makes rotation impossible without a manual re-download by everyone — the failure mode this milestone exists to remove. Two keys from day one make the first rotation an ordinary release. |
| D3 | **Verification order: signature, then hash, then anything touches the install directory.** Download the asset, `SHA256SUMS` and `SHA256SUMS.minisig` from `manifest.assets[...].url` (each prefix-checked per docs/47 D5) into a staging location; verify `.minisig` over `SHA256SUMS` with a known key; verify the asset's SHA-256 against its `SHA256SUMS` line; only then rename. The manifest's own `sha256` is a cross-check, not the authority. **Refuse to install any version ≤ the running release.** | The manifest is unsigned and stays that way (signing it would put the key in the badges writer's path); what it can do at worst is point at a *different signed release*. The ≤-current rule turns that into a no-op, so a manifest compromise cannot roll a fleet back to a signed-but-vulnerable build. |
| D4 | **"Download ready — install and relaunch." The app never restarts itself unprompted.** When R45 finds a newer version and the app is not live, it downloads and verifies in the background and the notice becomes a button. The button is disabled while a broadcast is live and while the resume supervisor holds a session. Clicking it performs the swap and relaunches. Failure at any step reverts to the R45 line with the release-page link. | Roadmap recommendation, adopted. A gaming PC that restarts its broadcaster on its own during a session is worse than one running last week's build. Downloading ahead of the click keeps the click fast; it is tens of MB on the uplink the app already broadcasts over, and only when not live. |
| D5 | **Windows: rename-swap next to the EXE, no helper process.** Download to `<exe>.new`; on click, rename the running `<exe>` to `<exe>.old` (Windows permits renaming a mapped executable), rename `.new` into place, spawn the new EXE, exit; the new build deletes `.old` at its next launch. Files the app writes itself carry no Mark-of-the-Web, so the SmartScreen "Unblock" step from INSTALL.md is **expected not** to recur — that is an acceptance criterion to verify on a real machine (SU3), not an assumption to ship on. | The `self-replace` pattern; the EXE is the whole product (docs/38 D17) so there is exactly one file to move. If MOTW does recur, minisign is not the fix (Authenticode would be) and Windows stops at "download ready, here is the file" — recorded as an outcome, not designed around. |
| D6 | **Linux: extract to a staging dir beside the binaries, verify, rename the set; fall back to the R45 notice when the directory is not writable.** The tarball's three binaries (`gawk-broadcast`, `gawk-broadcast-gui`, `gawk-pw-helper`) move together — `gawk-pw-helper` is found next to the app and must match it (docs/39). `rename(2)` over a running executable replaces the directory entry while the running process keeps its inode, so no `ETXTBSY`. If the install directory is read-only, or the running binary is not in a directory the app may write (a distro-managed path), the notice stays as R45 renders it and names the tarball to download. | INSTALL.md has no install location: the binaries run from wherever the tarball was unpacked, often `~/Downloads`. The app must never write anywhere but the directory it runs from, and never partially: the staging dir + three renames is the smallest atomic-enough unit, and a failure between renames is recoverable by re-running the install (the staging dir is kept). |
| D7 | **Where the logic lives**: alongside R45's. Go: `internal/update` gains `Download`, `Verify` (minisign + sha256; a minimal ed25519 verifier over the minisign envelope with `crypto/ed25519`, no dependency), `Install`; the swap for the running GUI lives in `internal/app`, which knows the live state. Rust: `crates/engine/src/update.rs` gains the same; the swap lives in `crates/app` (the only place that knows its own path), gated on `busy`. The public keys are constants beside the manifest URL. | Same module boundaries as docs/47 D8. Rust minisign verification: `ring` is already in `Cargo.lock` (rustls pulls it in) and `ring::signature::ED25519` verifies a detached ed25519 signature, so parsing the minisign envelope and calling it adds a direct dependency line but no new crate; if that turns out untrue at SU2, the `minisign-verify` crate is the fallback and trips the `licenses`/`notices` gates exactly once. |
| D8 | **Terms and READMEs.** The R45 sentence in the R23 terms (docs/47 D12) already discloses the manifest fetch; this milestone adds the download to it: "…and, at the user's request, download the newer version from GitHub". the Windows README's SmartScreen note is revisited per SU3's outcome. | Same recommendation as docs/47 D12 on `termsVersion`: no bump unless the owner decides otherwise. |

### Rejected

- **Auto-restart** after install — D4.
- **Signing each asset separately, or signing the manifest** — D1/D3: one
  signature over `SHA256SUMS` covers every asset; the manifest is a pointer
  whose worst case is already a no-op.
- **Authenticode / a code-signing certificate** — it would settle the
  SmartScreen question, but it is a paid, identity-bound certificate for a
  known-operator distribution (docs/38 D17) and it does nothing for Linux.
  Revisit only if D5's expectation fails at SU3.
- **A helper updater process** on Windows — the rename-swap needs none;
  a second binary is a second thing to sign, ship and explain.
- **winget / flatpak / AppImage / MSI** — the portable-binary posture
  stands (docs/38 OD6, docs/19 Decision 24).
- **Installing while live, with a deferred relaunch** — the window between
  "files swapped" and "process relaunched" is one where a crash-and-resume
  would start the new build mid-session; not worth the seconds it saves.

## 3. Where it plugs in

| Piece | Where it is today | What SU changes |
|---|---|---|
| CI signing | `ci.yml` `attach-broadcast-release` and `broadcast-windows.yml` `attach-release`: `sha256sum ./* > SHA256SUMS`, then `attach-release-assets`, then `publish-release-manifest` | SU1: a `minisign -S` step between checksum and attach, secrets `MINISIGN_SECRET_KEY` / `MINISIGN_PASSWORD`; a verify step against the checked-in public keys (`tools/releases/keys/`); `SHA256SUMS.minisig` in the asset dir so the manifest lists it. |
| Manifest writer | `tools/releases/manifest.py` | Nothing: `assets` already lists every file in the directory. |
| Linux logic | `internal/update` (R45) | SU2: `Download`, `Verify`, `Install`; `internal/app`: download-when-not-live, the swap, the relaunch. |
| Linux GUI | The R45 line under the badge | SU4: becomes a button once a download is verified; disabled while live; the fallback text names the tarball when the dir is not writable. |
| Windows logic | `crates/engine/src/update.rs` (R45) | SU2: the same functions; `crates/app`: `.new` download, the rename-swap and relaunch, `.old` cleanup at start. |
| Windows GUI | The R45 `Text` under the badge | SU3: a `Button` bound to a new `install-update` callback, `enabled: !root.busy`. |
| Docs | docs/38 D17; both READMEs; `docs/self-hosting.md` or §6 here; `docs/gotchas.md` | SU5. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **SU1** | CI signing (D1, D2): key pair generated offline, public keys checked in under `tools/releases/keys/` (current + next), secret + passphrase as repository secrets; both attach jobs sign `SHA256SUMS` and attach `SHA256SUMS.minisig`; a CI verify step; docs/38 D17 revised in place with a dated note; docs/43 §7 re-read and confirmed unchanged | A release commit attaches `SHA256SUMS.minisig`; the manifest lists it under `assets`; `minisign -Vm SHA256SUMS -p <pub>` succeeds on the downloaded pair; a PR from a fork (no secrets) still builds and attaches nothing (the existing `pull_request` gate); a run without the secret fails the attach loudly rather than attaching unsigned. |
| **SU2** | Download + verify in both apps (D3, D7): fetch asset, `SHA256SUMS`, `.minisig` from `manifest.assets`; verify with the compiled-in keys; refuse ≤ current; staging locations per platform | Unit tests with fixture files signed by a test key: a valid set verifies; a tampered asset, a tampered `SHA256SUMS`, a signature by an unknown key ID, a valid signature over a release whose version ≤ current → each refused *before* any rename, staging cleaned up; the download uses the same fixed UA and no conditional headers as the check (docs/47 D2). |
| **SU3** | Windows swap (D4, D5): `.new` → rename-swap → relaunch → `.old` cleanup on next start; button disabled while `busy` | Manual on a Windows 10 VM: from a signed release N to N+1, the button appears after download, the relaunch lands on N+1 (badge), `.old` is gone after the second launch, and **SmartScreen shows no prompt** for the swapped EXE. If it does, record it here and in BUGS.md and stop at "download ready" for Windows (D5). |
| **SU4** | Linux install (D4, D6): staging dir, three renames, non-writable fallback, `gawk-pw-helper` moved with the set; disabled while live | Unit test with a temp dir: all three binaries replaced atomically, staging removed; a read-only dir → the R45 notice with the tarball name and no writes; a live broadcast (`App.State()` live) → button disabled. Manual: a tarball unpacked in `~/Downloads`, GUI updated in place and relaunched. |
| **SU5** | Docs (D8): the READMEs' install-flow paragraphs, the terms sentence, key-rotation runbook (§6), gotchas, the ROADMAP row and entry | Review; the rotation runbook has been walked once on a throwaway key pair. |

Success criterion, end to end: a broadcaster on release N of either app,
with N+1 published and signed, updates to N+1 with one click and no prompt
other than the app's own; a broadcaster given a manifest that points at a
tampered asset, or at a release older than the one it runs, shows the R45
notice and nothing else, and its install directory is byte-identical
afterwards.

## 5. Security considerations

- **The trust root is the compiled-in public keys** (D1, D2), not the
  manifest and not TLS. Compromise of `badges` yields a no-op (D3's
  ≤-current rule). Compromise of the signing secret yields the ability to
  push a build to every broadcaster that checks — which is why the secret
  is used only in the attach jobs, which never run on `pull_request`
  (docs/19 Decision 24), and why a second key is compiled in from the first
  release so revocation is one release away.
- **Authenticode/SmartScreen reputation is explicitly not what this
  provides** (D1, D5). minisign proves the bytes came from the project's
  CI; it says nothing to Windows.
- **The install writes only beside the running binary** (D5, D6), never to
  a system path, never while live, and never a partial set on Linux.
- **The `badges` loop argument (docs/43 §7) is unchanged**: signing adds a
  secret, not a token, and pushes nothing new to the branch.

## 6. Operations

- **Rotating the signing key** (D2): generate the new pair offline; add
  its public key as "next" in a release; from the following release on,
  sign with it and add a new "next". Never remove a key that a shipped
  build still lists as "current" until a release signed by its successor
  has been out long enough for R45's daily check to have reached the fleet.
- **Retracting a release**: as docs/47 D11 — republish the manifest at the
  previous version and cut a `fix:` release. Clients that already
  installed the bad build take the fix release like any other; D3 keeps
  the retracted manifest from reading as a downgrade prompt.
- **A lost or leaked secret**: rotate per D2 immediately; a leaked key
  cannot be revoked in builds already in the field except by shipping a
  build that drops it, which is itself an update those builds will accept
  from either key. This is the argument for keeping the "next" key's
  secret offline until it is needed.
