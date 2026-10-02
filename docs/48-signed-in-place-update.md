# R47 — Signed in-place update for the desktop broadcasters (docs/48)

**Status**: designed 2026-09-15, refreshed 2026-09-30; **implemented
2026-10-01** (SU1–SU5 in one PR, §7). The first release signed end to end,
and the SU3/SU4 manual passes on real machines, are the owner's. Chunks
**SU1–SU5** (`SU` = Signed Update; two-letter prefix per the R21+
convention). **Depends on R45** ([docs/47](47-desktop-update-check.md)):
this milestone turns R45's "vX.Y.Z available" line into an "install and
relaunch" button, and reuses its manifest fetch, validation, compare,
opt-out and dismissal unchanged. Its prerequisite, **a release signing
key** (D1), was generated 2026-10-01 (key ID `EFB62FBC86D13877`). Split out
of R45 on 2026-09-15 so the notify-only half could ship while the signing
question was settled.

**2026-09-29 (R56 LX6)**: the Linux install path (D6, SU4) now applies to the
Rust app, `gawk-broadcast-linux` ([docs/58](58-linux-desktop-broadcaster.md)
D13). The unpacked directory holds **one** binary, with no helper beside it
(the PipeWire control plane is in-process, docs/58 D8), so D6's "three
renames" becomes one rename of `gawk-broadcast-linux` next to its
`share/`. The Go app is frozen and gets no in-place update (docs/58 OD6).

**2026-09-30 (refresh before implementation)**: four things changed under
this design since it was written, and the decisions below are revised in
place for them:

- **One attach job, not two.** The Go app is frozen (docs/58 OD6), so only
  `broadcast-desktop.yml` `attach-release` signs (D1). It carries the EXE,
  the macOS bundle, the Linux tarball and the `.deb` under one
  `SHA256SUMS`. `ci.yml` `attach-broadcast-release` stays unsigned until
  the Go app is removed (docs/58 LX9).
- **Rust only.** R45 shipped in the desktop workspace alone: the check is
  `crates/engine/src/update.rs` and its UI wiring is `crates/ui/src/shell.rs`
  (D7, §3). There is no Go `internal/update`.
- **D6 is rewritten for the one-binary tarball.** It now covers relaunching
  a binary whose path was renamed over (`/proc/self/exe` reads
  `… (deleted)` after the swap) and what happens to `share/`.
- **A `.deb` now ships beside the tarball** (R61, [docs/63](63-linux-deb-package.md)).
  It installs to `/usr/bin`, which the app may not write, so a `.deb`
  install never swaps in place. New **D9** gives it its own notice
  instead of the tarball's.

macOS is not in this document: its bundle swap is docs/54 D17, which
reuses D1–D4 and SU2 from here.

**2026-10-01 (owner decision)**: **one** compiled-in public key, not two.
D2 is revised in place, and "current + next" moves to Rejected. The cost is
stated in D2 and §6: installs that do not install a rotation's bridge
release (planned, or after a leak) download by hand once, and a **lost**
key means one manual download for everyone. In every case R45's notice
keeps working, so nobody is left without a way to update.

**2026-10-02 (field fix, §8)**: the in-place install never worked in a
release. Every download in 2.2.0–2.4.0 failed and the notice stayed a
release-page link, because the download's byte limit, the manifest's exact
`size`, is one that ureq refuses a body of exactly. Fixed in SU6; installs
on those three versions download the fixed release by hand once. The same
pass moves the notice to the top of the Ready page (SU7, docs/47 D7 and
docs/60 D13 revised), and a notice that falls back to the release page now
says why.

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
the shell's `busy` property as the "is a broadcast live" gate; and the
settings card's disabled state while live (docs/19 Decision 9).

## 2. Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Prerequisite: detached minisign (ed25519) signatures over `SHA256SUMS`, produced in CI, verified against a public key compiled into the apps.** The desktop attach job (`broadcast-desktop.yml` `attach-release`) gains a signing step: `SHA256SUMS.minisig` is attached beside `SHA256SUMS`, and therefore appears in all three distributions' `manifest.assets` with no change to the R46 writer. The secret key and its passphrase are repository secrets used only in that job. The Go app's `ci.yml` `attach-broadcast-release` is not signed (docs/58 OD6: it gets no in-place update). **docs/38 D17 is revised in place with a dated note for this milestone**: the binaries stay unsigned in the Authenticode sense; the *release set* becomes signed. | Signing one file (`SHA256SUMS`) instead of each asset keeps the CI step to one command and the client to one signature plus one hash. Nothing about `badges` publishing changes, so docs/43 §7's "CI writing to the repo cannot loop" argument is untouched — it hinges on `GITHUB_TOKEN`, which a signing secret does not replace. |
| D2 | **One public key compiled in; rotation is a bridge release, or a manual download when the key is gone.** (Revised 2026-10-01, owner decision; was "current + next".) The app verifies only signatures whose minisign key ID matches its one compiled-in key. Any other key ID → refuse, and fall back to the R45 notice. **Rotation by bridge** (the old secret is still held: a planned rotation, or a leak): cut a bridge release that is signed with the old key but compiles in the new one, then sign every later release with the new key. An install that **installed** the bridge verifies everything after it. Opening the app is not enough: under D4 the swap happens only on the user's click, and never while live. An install that did not install the bridge while it was the latest release sees the R45 notice for the next one and downloads by hand once. After a leak the bridge is also the revocation: signing it with the leaked key gives an attacker nothing they did not have, and each install that takes it stops trusting that key. **Lost secret**: no bridge is possible, so every install downloads by hand once, prompted by the R45 notice. | Simplest to operate: one key pair, one secret, one constant, nothing held offline in reserve. Key changes are expected to be rare, and the fallback in every failure case is the R45 notice, which needs no signature. That is today's manual update, not a stranded install. The two-key design bought rotation without a manual step even when the key is lost; the owner judged that not worth a second secret held offline (Rejected). |
| D3 | **Verification order: signature, then hash, then anything touches the install directory.** Download the asset, `SHA256SUMS` and `SHA256SUMS.minisig` from `manifest.assets[...].url` (each prefix-checked per docs/47 D5) into a staging location; verify `.minisig` over `SHA256SUMS` with a known key; verify the asset's SHA-256 against its `SHA256SUMS` line; only then rename. The manifest's own `sha256` is a cross-check, not the authority. **Refuse to install any version ≤ the running release.** | The manifest is unsigned and stays that way (signing it would put the key in the badges writer's path); what it can do at worst is point at a *different signed release*. The ≤-current rule turns that into a no-op, so a manifest compromise cannot roll a fleet back to a signed-but-vulnerable build. |
| D4 | **"Download ready — install and relaunch." The app never restarts itself unprompted.** When R45 finds a newer version and the app is not live, it downloads and verifies in the background and the notice becomes a button. The button is disabled while a broadcast is live and while the resume supervisor holds a session. Clicking it performs the swap and relaunches. Failure at any step reverts to the R45 line with the release-page link. | Roadmap recommendation, adopted. A gaming PC that restarts its broadcaster on its own during a session is worse than one running last week's build. Downloading ahead of the click keeps the click fast; it is tens of MB on the uplink the app already broadcasts over, and only when not live. |
| D5 | **Windows: rename-swap next to the EXE, no helper process.** Download to `<exe>.new`; on click, rename the running `<exe>` to `<exe>.old` (Windows permits renaming a mapped executable), rename `.new` into place, spawn the new EXE, exit; the new build deletes `.old` at its next launch. Files the app writes itself carry no Mark-of-the-Web, so the SmartScreen "Unblock" step from INSTALL.md is **expected not** to recur — that is an acceptance criterion to verify on a real machine (SU3), not an assumption to ship on. | The `self-replace` pattern; the EXE is the whole product (docs/38 D17) so there is exactly one file to move. If MOTW does recur, minisign is not the fix (Authenticode would be) and Windows stops at "download ready, here is the file" — recorded as an outcome, not designed around. |
| D6 | **Linux tarball: extract to a staging dir beside the binary, verify, rename `gawk-broadcast-linux` over itself, relaunch from the path saved at startup; fall back to the R45 notice when the directory is not writable.** (Revised 2026-09-30 for the one-binary tarball.) The staging dir is `.gawk-update-<version>/` in the directory holding the running binary, so the final `rename(2)` never crosses a filesystem. It holds the verified tarball and its extracted contents. Only the binary moves. `share/` and `install-desktop.sh` are left as they are: the launcher entry that `install-desktop.sh` wrote points at the binary's absolute path, which the swap keeps, and the entry and icons it copied into `~/.local/share` would not be refreshed by replacing `share/` anyway. `rename(2)` over a running executable replaces the directory entry while the running process keeps its inode, so there is no `ETXTBSY`. **The relaunch path is `std::env::current_exe()` read once at startup and kept.** On Linux that reads `/proc/self/exe`, which after the swap resolves to `<path> (deleted)`, so reading it at relaunch time would try to spawn a file that no longer exists. The relaunch spawns the saved path with the original arguments and then exits. If the directory is read-only, or the binary is in a system path, the notice stays as R45 renders it. A `.deb` install is that case, and D9 gives it its own text. | INSTALL.md has no install location: the tarball runs from wherever it was unpacked, often `~/Downloads`. The app must never write anywhere but the directory it runs from. One binary makes the swap a single atomic rename. A failure before it leaves the old binary untouched. A leftover staging dir is removed at the next start, so a failed or interrupted install costs a re-download and nothing else. |
| D7 | **Where the logic lives**: alongside R45's, in the desktop workspace only (revised 2026-09-30; there is no Go half, docs/58 OD6). `crates/engine/src/update.rs` gains `download`, `verify` (minisign envelope + ed25519 + SHA-256) and the ≤-current refusal, all platform-neutral and unit-testable on any host. `crates/ui/src/shell.rs`, where R45's check, dismissal and Check now are wired, gains download-when-not-live, the `install-update` callback and the `busy` gate. The swap itself is per platform, because the three layouts differ: `crates/app-windows` (D5), `crates/app-linux` (D6, D9), `crates/app-macos` (docs/54 D17). Each shell hands `shell.rs` its install function the same way it injects its distribution (`engine::defaults::set_this`). The public key is a constant beside the manifest URL. | Same module boundaries as docs/47 D8. Rust minisign verification: `ring` is already in `Cargo.lock` (rustls pulls it in) and `ring::signature::ED25519` verifies a detached ed25519 signature, so parsing the minisign envelope and calling it adds a direct dependency line but no new crate; if that turns out untrue at SU2, the `minisign-verify` crate is the fallback and trips the `licenses`/`notices` gates exactly once. |
| D8 | **Terms and READMEs.** The R45 sentence in the R23 terms (docs/47 D12) already discloses the manifest fetch; this milestone adds the download to it: "…and, at the user's request, download the newer version from GitHub". The Windows README's SmartScreen note is revisited per SU3's outcome. | Same recommendation as docs/47 D12 on `termsVersion`: no bump unless the owner decides otherwise. |
| D9 | **A `.deb` install is never updated in place. The notice names the newer `.deb` and the command that installs it.** (Added 2026-09-30, R61.) When the running binary is `/usr/bin/gawk-broadcast-linux` and dpkg lists it as owned by package `gawk-broadcast` (`/var/lib/dpkg/info/gawk-broadcast.list` names that path), the app downloads nothing. It shows the R45 line with the `.deb` file name built from the validated manifest version (`gawk-broadcast_<version>_amd64.deb`, looked up in `manifest.assets` by exact name, as the site does, docs/63 D2) and `sudo apt install ./gawk-broadcast_<version>_amd64.deb`. If the manifest has no such asset (the `.deb` is a soft attach, docs/63 D8), it falls back to the plain R45 line. Any other non-writable location gets the D6 fallback. | `/usr/bin` belongs to dpkg. A swap there would need root, and dpkg would not know about it, so the next `apt install` or `apt remove` would act on a package whose files it no longer describes. The alternatives cost more than they give (see Rejected). The notice is one click from what INSTALL.md already says to do ("install the newer `.deb` the same way"). An apt repository (docs/63 D7, deferred) is the real in-place path for `.deb` users; if one is built, D9's notice changes to "update with your package manager" and nothing else here changes. |

### Rejected

- **Auto-restart** after install — D4.
- **Two compiled-in keys, current + next** (the 2026-09-15 D2; rejected
  2026-10-01 by the owner). It would make every rotation, including one
  after a lost key, an ordinary release with no manual step, at the cost of a second
  key pair whose secret is kept offline until needed. One key keeps the
  setup to one secret. Its failure cases (D2) end at the R45 notice, never
  at a stranded install. Revisit if a forced rotation actually happens, or
  if the fleet grows past the point where "everyone downloads once" is
  cheap.
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
- **Installing a `.deb` update through `pkexec apt install`** (D9). It
  adds a root password prompt, which breaks the success criterion's "no
  prompt other than the app's own", and it has the app run a package as
  root that dpkg itself has not verified (the `.deb` is unsigned, docs/63
  D7). minisign would vouch for the bytes, but a root-run install step
  inside a GUI is a surface this milestone does not need.
- **Downloading the `.deb` to `~/Downloads` for the user** (D9). That
  writes outside the directory the app runs from (D6), and it saves one
  click in the browser.
- **Swapping the binary under `/usr/bin` behind dpkg's back** (D9).
- **Installing while live, with a deferred relaunch** — the window between
  "files swapped" and "process relaunched" is one where a crash-and-resume
  would start the new build mid-session; not worth the seconds it saves.

## 3. Where it plugs in

(Rewritten 2026-09-30 against the code as it is on `main`.)

| Piece | Where it is today | What SU changes |
|---|---|---|
| CI signing | `.github/workflows/broadcast-desktop.yml` `attach-release`: stages the EXE, the macOS bundle, the Linux tarball and the `.deb`, runs `sha256sum ./* > SHA256SUMS`, then `attach-release-assets`, then `publish-release-manifest` once per distribution (Windows, macOS, Linux) | SU1: a `minisign -S` step between checksum and attach, secrets `MINISIGN_SECRET_KEY` / `MINISIGN_PASSWORD`; a verify step against the checked-in public key (`tools/releases/keys/`); `SHA256SUMS.minisig` in the asset dir so all three manifests list it. |
| Manifest writer | `tools/releases/manifest.py` | Nothing: `assets` already lists every file in the directory. |
| Shared logic | `gawk-broadcast-desktop/crates/engine/src/update.rs` (R45: manifest URL, validate, compare, check, cache) | SU2: `download`, `verify` (minisign + SHA-256), the ≤-current refusal; the public key as a constant. |
| Shared shell | `crates/ui/src/shell.rs` (R45: launch check, 15-minute schedule, dismissal, Check now) | SU2: download-when-not-live after a positive check; the `install-update` callback calling the shell's injected install function; failure → the R45 line. |
| GUI | `crates/ui/main.slint`: the `update-version` line under Go live, the Settings → Updates card, `busy` | SU3/SU4: the line becomes a button once a download is verified, `enabled: !root.busy`; one shared `.slint` (docs/54 D11), with the platform differences as properties. |
| Windows swap | `crates/app-windows` | SU3: `.new` download, rename-swap, relaunch, `.old` cleanup at start (D5). |
| Linux swap | `crates/app-linux` | SU4: staging dir, one rename, relaunch from the path saved at startup (D6); `.deb` detection and its notice (D9). |
| macOS swap | `crates/app-macos` | Not here: docs/54 D17, built on SU1 and SU2. |
| Docs | docs/38 D17; `gawk-broadcast-desktop/README.md`; `tools/linux/INSTALL.md`; docs/63 D7; §6 here; `docs/gotchas.md` | SU5. |

## 4. Chunks and acceptance criteria

| Chunk | Scope | Verified by |
|---|---|---|
| **SU1** | CI signing (D1, D2): key pair generated offline, the public key checked in under `tools/releases/keys/` (one key, D2), secret + passphrase as repository secrets; `broadcast-desktop.yml` `attach-release` signs `SHA256SUMS` and attaches `SHA256SUMS.minisig`; a CI verify step; docs/38 D17 revised in place with a dated note; docs/43 §7 re-read and confirmed unchanged | A release commit attaches `SHA256SUMS.minisig`; all three desktop manifests list it under `assets`; `minisign -Vm SHA256SUMS -p <pub>` succeeds on the downloaded pair; a PR from a fork (no secrets) still builds and attaches nothing (the existing `pull_request` gate); a run without the secret fails the attach loudly rather than attaching unsigned. |
| **SU2** | Download + verify in the desktop workspace (D3, D7): fetch asset, `SHA256SUMS`, `.minisig` from `manifest.assets`; verify with the compiled-in key; refuse ≤ current; staging locations per platform | Unit tests with fixture files signed by a test key: a valid set verifies; a tampered asset, a tampered `SHA256SUMS`, a signature by an unknown key ID, a valid signature over a release whose version ≤ current → each refused *before* any rename, staging cleaned up; the download uses the same fixed UA and no conditional headers as the check (docs/47 D2). |
| **SU3** | Windows swap (D4, D5): `.new` → rename-swap → relaunch → `.old` cleanup on next start; button disabled while `busy` | Manual on a Windows 10 VM: from a signed release N to N+1, the button appears after download, the relaunch lands on N+1 (badge), `.old` is gone after the second launch, and **SmartScreen shows no prompt** for the swapped EXE. If it does, record it here and in BUGS.md and stop at "download ready" for Windows (D5). |
| **SU4** | Linux install (D4, D6, D9): staging dir beside the binary, one rename, relaunch from the startup path, leftover staging removed at start; non-writable fallback; the `.deb` notice; disabled while `busy` | Unit tests with a temp dir: the binary replaced by one rename and staging removed; `share/` untouched; the relaunch path is the one captured before the swap, not a `(deleted)` path; a leftover `.gawk-update-*` dir removed at start; a read-only dir → the R45 line and no writes; a binary at `/usr/bin/gawk-broadcast-linux` listed in a fixture `gawk-broadcast.list` → the D9 notice naming `gawk-broadcast_<version>_amd64.deb` and the apt command, nothing downloaded; the same with no `.deb` in the manifest → the plain R45 line; `busy` → button disabled. Manual: a tarball unpacked in `~/Downloads` and launched from the `install-desktop.sh` entry updates in place, relaunches on N+1 (badge), and the launcher entry still starts it; a `.deb` install of N shows the D9 notice for N+1, and `sudo apt install` of the named file lands on N+1. |
| **SU5** | Docs (D8, D9): the READMEs' and `tools/linux/INSTALL.md`'s update paragraphs, docs/63 D7's pointer to D9, the terms sentence, key-rotation runbook (§6), gotchas, the ROADMAP row and entry | Review; the rotation runbook has been walked once on a throwaway key pair. |

Success criterion, end to end: a broadcaster on release N of the Windows
app or the Linux tarball, with N+1 published and signed, updates to N+1
with one click and no prompt other than the app's own; a broadcaster on a
`.deb` install sees the D9 notice naming N+1's package; a broadcaster given a manifest that points at a
tampered asset, or at a release older than the one it runs, shows the R45
notice and nothing else, and its install directory is byte-identical
afterwards.

## 5. Security considerations

- **The trust root is the compiled-in public key** (D1, D2), not the
  manifest and not TLS. Compromise of `badges` yields a no-op (D3's
  ≤-current rule). Compromise of the signing secret yields the ability to
  push a build to every broadcaster that checks — which is why the secret
  is used only in the attach job, which never runs on `pull_request`
  (docs/19 Decision 24). With one key (D2), revoking a leaked key is one
  bridge release away (§6): each install stops trusting it when it
  installs the bridge, and installs that miss the bridge keep trusting it
  until their user downloads a newer build by hand. Revocation is
  impossible in-band only if the secret is lost as well as leaked. The ≤-current rule still
  applies, so an attacker holding the key has to publish a version higher
  than the one installed, which is visible on the release page.
- **Authenticode/SmartScreen reputation is explicitly not what this
  provides** (D1, D5). minisign proves the bytes came from the project's
  CI; it says nothing to Windows.
- **The install writes only beside the running binary** (D5, D6), never to
  a system path, never while live, and never a partial set on Linux.
- **The `badges` loop argument (docs/43 §7) is unchanged**: signing adds a
  secret, not a token, and pushes nothing new to the branch.

## 6. Operations

- **Rotating the signing key, planned** (D2): generate the new pair
  offline. Cut a bridge release: its code compiles in the new public key,
  and CI still signs it with the old secret. Leave the bridge as the latest
  release for a while (a week or more) so that installs can take it.
  Taking it means clicking install (D4), not just opening the app, so the
  bridge's release notes ask users to install it. `tools/releases/keys/` stays on the old public key for the
  bridge, because CI's verify step checks the bridge's signature, which
  is still the old key's. Then, in one commit, replace the repository
  secret and the public key in `tools/releases/keys/`, and sign every
  later release with the new key. The compiled-in constant (D7) already
  changed in the bridge. Installs that missed the bridge see
  the R45 notice and download by hand once. Say so in the release notes of
  the first release signed with the new key.
- **Retracting a release**: as docs/47 D11 — republish the manifest at the
  previous version and cut a `fix:` release. Clients that already
  installed the bad build take the fix release like any other; D3 keeps
  the retracted manifest from reading as a downgrade prompt.
- **A leaked secret** (D2): rotate by bridge, as for a planned rotation,
  but at once. Signing the bridge with the leaked key is the point: field
  builds already accept anything that key signs, and each one that
  installs the bridge stops trusting it (§5). Keep the window as short as
  it can be while still giving installs a chance to take it, and say in
  the bridge's release notes that the old key is compromised and that
  users should install now. After the switch, sign nothing more with the
  leaked key.
- **A lost secret** (D2): generate a new pair. Then, in one release,
  replace the repository secret, the public key in `tools/releases/keys/`
  (CI's verify step) and the compiled-in constant (D7), and ship it. No
  bridge is possible: installs in the field reject its signature and fall
  back to the R45 notice, so everyone downloads once by hand.

## 7. Implementation (2026-10-01)

Where the code differs from the decisions above, and why. Each is small;
none changes a security property except the first, which adds one.

- **The version comes from the signature (D3, added).** A signature over
  `SHA256SUMS` alone does not say which release it covers, so a manifest
  could point a verified download at an older signed release and call it
  newer, defeating D3's ≤-current rule. CI signs with the trusted comment
  `gawk-broadcast-desktop X.Y.Z`, which minisign's global signature covers;
  `update::verify_release` reads the version from there, requires it to
  equal the manifest's and to be newer than the running build.
- **`minisign-verify`, not `ring` (D7).** minisign 0.11+ signs prehashed
  (BLAKE2b-512) by default, which `ring` does not implement. D7 named this
  crate as the fallback; it is MIT, dependency-free, and by minisign's
  author. The notices gained it (and, for Linux, `tar` and `filetime`).
- **The swap rules live in `gawk-engine` (`install.rs`), not the shell
  crates (D7).** They are plain `std::fs` and so testable on any host,
  while `app-windows` only builds for msvc and `app-linux` needs GStreamer.
  The shared shell (`ui/src/shell.rs`) picks the layout from the injected
  distribution; the platform crates did not change.
- **Windows stages in `.gawk-update/`, not `<exe>.new` (D5).** One staging
  rule for both platforms. The swap is still two renames beside the EXE,
  with the old one parked as `<exe>.old` and removed at the next start.
- **A remembered update re-fetches the manifest at launch.** The config
  caches only version and URL (R45), so a relaunch inside the 15-minute
  window had no file list to download. When the build can install in place,
  that launch asks again instead of waiting.
- **The Linux tarball is unpacked in-process** (`flate2` + `tar`, Linux
  only), extracting only the member `gawk-broadcast-linux`; no path in the
  archive can reach outside the staging directory.
- **CI signing is `tools/releases/sign-sums.sh`.** It downloads minisign
  0.12 pinned by SHA-256 (its tarball's signature was checked against the
  author's key when pinned), signs, and verifies against
  `tools/releases/keys/gawk-release.pub`. `--self-test` runs the same path
  with a throwaway key in the `release-tool` job, because `attach-release`
  never runs on a pull request.

| Chunk | State | Verified |
|---|---|---|
| SU1 | Implemented | Key generated offline, secrets set, public key checked in. `sign-sums.sh` refuses a missing secret, a non-release version and a wrong passphrase, and signed with the real secret verifies against the checked-in key (local run, dummy file, version 0.0.1). `test_sign_sums.py` runs the self-test in CI. Owner-pending: the first release carrying `SHA256SUMS.minisig`, and `minisign -Vm` on the downloaded pair. |
| SU2 | Implemented | `update.rs` tests against vectors from the real minisign (`crates/engine/testdata/r47`): a valid set stages; a tampered asset, another key, an older signed release, the running release, a manifest version that differs from the signed one, and a manifest without signed files are each refused with nothing left staged; the download sends the fixed UA and no conditional headers, and stops at its size limit. |
| SU3 | Implemented; manual pass owner-pending | Swap, rollback on failure and `.old` cleanup unit-tested in `install.rs`; the button's idle-only rules in `shell.rs`. The Windows 10 VM pass from release N to N+1, including whether SmartScreen prompts, needs two signed releases. |
| SU4 | Implemented; manual pass owner-pending | `install.rs`: one rename, `share/` untouched, leftover staging removed at start, a read-only directory writes nothing, the `.deb` detected from dpkg's list, a tarball without the binary refused; `shell.rs`: the D9 note text, no download while live, dismissed, given up or without files. The `~/Downloads` and `.deb` passes need two signed releases. |
| SU5 | Done | The READMEs, `tools/linux/INSTALL.md`, the terms sentence (no `termsVersion` bump, D8), docs/38 D17, gotchas, ROADMAP. |

## 8. Field fix and the notice at the top (2026-10-02)

**What happened.** The owner, on 2.2.0 with 2.3.0 published, saw only the
release-page link: no download, no button. Run against the live 2.3.0
manifest, `update::stage` failed with `the response body is larger than
request limit: 35960832`, which is the EXE's exact size. `stage` passes
the manifest's `size` as the download's byte limit, and ureq's limit
reader errors on the read *after* `limit` bytes, even when that read
would only have found the end. So a body of exactly `limit` bytes always
fails, and every real download is exactly its listed size. The SU2 tests
stubbed the network, and the real `download` was tested only well under
and well over its limit. The shell then gave up for the run without a
word, which D4 allowed ("failure at any step reverts to the R45 line").
That is why the bug looked like a missing feature.

**What changes.**

- **SU6 (the fix).** `fetch` and `download` pass ureq one byte of
  headroom, so the limit means "at most `limit` bytes": a body of exactly
  the limit is whole, and one byte more is still refused. No security
  property moves. The limit only bounds the transfer, and the signed hash
  decides what is installed.
- **SU7 (the notice).** The notice is a card at the top of the Ready page,
  with the other notices (docs/47 D7 and docs/60 D13, revised), instead of
  a 12 px line under the room list. It says which step it is at:
  *downloading* (also while the launch check re-fetches a remembered
  update's file list), *ready* with **Install and relaunch** (disabled
  while starting or live, D4) and **What's new**, or *available* with
  **Download**, which opens the release page. When it falls back to the
  release page it now says why: the `.deb`'s command (D9, unchanged), a
  folder the app can't write (D6), or a download that failed. **Check now
  retries** an install this run gave up on; before this, a failed
  download stayed failed until a restart.

**Who still updates by hand.** Installs on 2.2.0, 2.3.0 and 2.4.0 carry
the broken download, so they reach the fixed release through **Download**
once. From there it is one click. Rewriting the manifests' `size` fields
to rescue them was considered and rejected: it would make a data file
other consumers read (the site, docs/46) lie in order to work around a
client bug, and the fleet is small enough that one manual download is
cheap.

| Chunk | Scope | Verified by |
|---|---|---|
| **SU6** | `update.rs`: a limit is the most a file may be (`ureq_limit`) | `a_file_of_exactly_its_limit_is_whole` (a body of exactly the limit is accepted by `download` and `fetch`; one byte over is refused) and `a_release_stages_over_http_at_its_listed_sizes` (the whole of `stage` over a local HTTP server, with every file at its exact listed size), both failing before the fix with the production error. Live: from a 2.2.0 version string, `check` + `stage` against the published 2.3.0 manifest stage the Windows EXE (hash `4d397b01…`, equal to the signed `SHA256SUMS` line) and the Linux tarball, whose binary unpacks. |
| **SU7** | `main.slint` `UpdateCard` at the top of `ready-top`; `shell.rs` `UpdateState::phase`, the fallback notes, Check now's retry | `the_notice_offers_what_this_copy_can_do` (each phase: nothing shown, re-fetch in flight, offline, downloading, ready, a newer release over a ready one, macOS, given up); `check_now_retries_a_failed_download` (an automatic check keeps the give-up, Check now clears it and the note); `the_bar_stays_in_reach_at_the_minimum_size` (at 440 × 600 the card's **Download**, then **Install and relaunch** and **What's new**, are inside the window). Rendered under Xvfb in all three phases at 520 and 440 px wide. The Windows and Linux manual passes from a fixed release N to N+1 stay SU3/SU4's. |
