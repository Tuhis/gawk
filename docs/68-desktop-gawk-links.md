# R66 — `gawk://` links in the desktop broadcaster (docs/68)

**Status**: proposed 2026-10-03. Owner decisions OD1–OD4 (§2) were taken
the same day. Chunks **LH1–LH6** (§9) are not started. Status lives in
[`ROADMAP.md`](../ROADMAP.md).

**Relationship to earlier work**

- R65 ([docs/67](67-ios-app.md) D20) reserved the `gawk://` scheme for the
  iOS app, with `watch/<CODE>` and `room/<name>` paths, but never wrote down
  a grammar. **This doc defines the grammar for every native app** (D1). The
  parser lives in the desktop workspace's `engine` crate, which the iOS app
  reuses by path, so both apps parse links with the same code.
- R67 ([docs/69](69-app-desktop-handoff.md)) is the other half: `gawk-app`
  offering to open a broadcast in the desktop app. R67 builds links, R66
  parses them, and both are tested against D2's vectors.
- The desktop app already reads pasted room links (`engine::parse_room_input`,
  R42 / [docs/44](44-rooms.md) §4.8) and already opens the browser *from*
  the app ("Open room view", `#/room/<code>?rt=<grant>`). Nothing opens the
  app from the browser today.
- `gawk-broadcast://windows|macos|linux` remains the **Origin label** the
  desktop apps send (docs/54 D16, docs/58 OD10). It is not a URL scheme,
  and nothing registers it (D14).

---

## 1. Why, and what "done" means

- A room's "Start streaming" link (the Mumble bot's button, R42's
  2026-10-02 revision: `#/broadcast?room=<code>`) always opens the browser
  broadcaster. If you have the desktop app, which captures better (native
  encode, app audio), you have to start it yourself and type or paste the
  room.
- A link can't open the desktop app today. No platform registers a
  handler, the app reads no arguments, and a second launch starts a second
  process that shares the first one's `broadcast.json`.

### Milestone acceptance criteria

"Each OS" means Windows 11, macOS 14+ and Ubuntu 24.04 (GNOME, Wayland),
with Chrome and Firefox on each, plus Safari on macOS.

| # | Goal | Verified by |
|---|---|---|
| G1 | With the app **not running**, clicking `gawk://broadcast?room=<code>&nick=<n>` launches it with the room pending ("Joins when you go live") and the nickname filled in. Nothing is captured or started. | each OS (LH6) + shell tests |
| G2 | With the app **running**, the same click brings the existing window to the front with the same prefill. No second process remains. | each OS (LH6) + IPC tests |
| G3 | Launching the app a second time with no link raises the running window and exits (OD1) | each OS (LH6) + IPC tests |
| G4 | A link's `relay=` selects a matching saved server. An unknown server is never used without a click on "Add and switch" (OD2). | shell tests + LH6 |
| G5 | While a broadcast is live or paused, a link never changes the broadcast, its room or its server without a click | shell tests |
| G6 | `gawk://watch/<CODE>` and `gawk://room/<code>` open the matching `https://` page in the default browser. A cold start for one of them leaves no app window behind. | shell tests + LH6 |
| G7 | A malformed, oversized or hostile link shows a notice and changes nothing; the parser never panics | engine unit + fuzz-style tests |
| G8 | Windows: registration writes only `HKCU`, refreshes the path after the EXE moves, and never takes over a `gawk` key that another program wrote | unit tests on the decision function + LH6 |
| G9 | The `.deb` registers the scheme. After install, `xdg-mime query default x-scheme-handler/gawk` names our desktop entry in a fresh container. | `test-deb.sh` in CI |
| G10 | "Install and relaunch" after an update does not apply the launch link again | shell test |

## 2. Owner decisions (2026-10-03)

| # | Decision |
|---|---|
| OD1 | **The app becomes single-instance**, always. A second launch, with or without a link, hands over to the running window and exits. Two processes racing on one `broadcast.json` was never safe. |
| OD2 | **`relay=` matches a saved server, or offers to add it.** A matching origin selects that server. An unknown one shows a confirm card. The desktop gets no session-only override layer (the web has one, docs/40 §4.1). |
| OD3 | (R67) The SPA offers a button and remembers the choice; see docs/69 |
| OD4 | **Windows registers on every launch, automatically**, per user under `HKCU`. It refreshes a moved path and never takes the scheme from another program. |

## 3. Alternatives considered and rejected

| Alternative | Why not |
|---|---|
| Universal links (macOS Associated Domains) / "apps for websites" (Windows) | Windows needs MSIX packaging, and the app ships as a portable EXE (docs/38 OD6). macOS needs a provisioning profile and an AASA file. Linux has no equivalent. Too much for partial coverage. docs/67 made the same call for iOS v1. |
| A loopback HTTP server the page probes | Chrome's Private Network Access blocks it, and it opens a local attack surface |
| A separate scheme per platform or app (`gawk-broadcast://`, `gawk-ios://`) | No device runs two of our apps, so one scheme is enough. A second scheme would make R67 build two links, and `gawk-broadcast://` is already an Origin label (D14). |
| Lock-file single instance (flock / `O_EXCL`) | Detects a running copy but can't send it the link; we need IPC anyway, and the IPC endpoint doubles as the lock |
| Registration by an installer | There is no installer on Windows (portable EXE) or macOS (a zipped `.app`). The `.deb` does register, through its desktop entry (D11). |

## 4. Decisions

### D1 — The grammar

One grammar for every native app; this section supersedes the prose in
docs/67 D20.

```
gawk://broadcast[/][?room=<code>][&nick=<nickname>][&relay=<https-origin>]
gawk://watch/<BROADCAST-ID>[?relay=<https-origin>]
gawk://room/<code>[?nick=<nickname>][&relay=<https-origin>]
```

- **Scheme and path are case-insensitive** (`GAWK://Broadcast` works).
  `<BROADCAST-ID>` is upper-cased and validated like the SPA's
  `isValidBroadcastId`: six characters from the wire alphabet. `<code>`
  keeps its case and is validated like `isValidRoomCode`: a 3–32 character
  `[A-Za-z0-9-]` slug, or a broadcast ID.
- **Query values are percent-decoded UTF-8.** `nick` is sanitized exactly as
  the SPA's `sanitizeNickname` does: whitespace collapsed, trimmed, capped at
  `MAX_ROOM_NICKNAME_LEN` (32) bytes. Blank counts as absent. `relay` is
  normalized as docs/40 §4.2 says: https only, no credentials, path `/`
  or none, no query or fragment, reduced to the origin.
- **No secrets, ever.** `rt=`, `secret=` and any credential-shaped parameter
  are dropped with a note. Grants and publish secrets never travel in
  links (docs/40 D3, docs/31 D1, docs/44 D8).
- **Unknown parameters are dropped with a quiet note**, not rejected
  (docs/31 D7). An invalid value for a known parameter drops that
  parameter. An invalid path or ID rejects the whole link.
- **A link longer than 2,048 bytes is rejected** before parsing.
- `broadcast` is the only path a broadcaster-only app acts on itself.
  `watch` and `room` are viewer intents (D3).

### D2 — `engine::link`: one parser, golden vectors

A new portable module `gawk-broadcast-desktop/crates/engine/src/link.rs`:

```rust
pub enum Link {
    Broadcast { room: Option<String>, nick: Option<String>, relay: Option<String> },
    Watch { id: String, relay: Option<String> },
    Room { code: String, nick: Option<String>, relay: Option<String> },
}
pub struct Parsed { pub link: Link, pub dropped: Vec<Dropped> }
pub fn parse(raw: &str) -> Result<Parsed, LinkError>;
impl Link {
    pub fn to_gawk(&self) -> String;                 // canonical form
    pub fn to_https(&self, app_url: &str) -> String; // #/broadcast?room=…, #/view/<ID>?relay=…, #/room/<code>?…
}
```

- It reuses what exists: `parse_room_code`, `room::truncate_utf8`, and the
  relay normalization in `config.rs` (made `pub(crate)`). It also needs a
  public broadcast-ID validator. `wire::room::normalize_broadcast_id` is
  private today; make it public in the Rust wire crate. That is a helper,
  not a wire type, so no other mirror changes.
- **Golden vectors**: `crates/engine/tests/link-vectors.json` holds input,
  expected `Link` (or error), `dropped`, the canonical `to_gawk` and
  `to_https`. R67's TS builder restates them, and R65 IO6 uses the module
  directly. Vectors are restated, never imported, like the wire vectors.
- `parse_room_input` (the Room sheet's paste box) also accepts
  `gawk://room/<code>` and `gawk://broadcast?room=<code>`, so a pasted
  native link works like a pasted web link.

### D3 — What the desktop app does with each link

| Link | Action |
|---|---|
| `broadcast` | Prefill (D4), raise the window |
| `watch`, `room` | Open `to_https(app_url)` in the default browser with the existing opener ("Open room view"). On a **cold start** for one of these, the app opens the browser and exits without showing its window. On a **warm** handoff the window isn't raised. |
| rejected | Show the window with a notice: "This link couldn't be opened: <reason>" |

The desktop has no viewer, so a viewer link always ends in the browser. A
`watch` link shared from an iPhone still works when opened on a PC.

### D4 — Prefill, never start

A `broadcast` link fills in the form. Only **Go live** starts a broadcast.
That gate already exists: `start_broadcast` runs only from the button, and
the source must be chosen first. How a link applies depends on the state:

| State | `room` | `nick` | `relay` |
|---|---|---|---|
| **Idle** (incl. the ended screen) | `choose_room` with no grant. It becomes the pending room and goes into recents. A saved attach secret for that code in `recent_rooms` is reused. | Set the `room-nickname` UI property **and** `cfg.nickname`. `read_settings` reads the property at Start, so setting `cfg` alone would be overwritten (§8). | D5 |
| **Starting / stopping** | Queued and applied when the state settles; the latest link wins | queued | queued |
| **Live / paused** | Confirm card: "Join room `<code>` now?" [Join] [Not now]. `choose_room` joins immediately while live, so that call happens only on the click. | Applied with the room on [Join]. Without a room, it's the same confirm card. | Notice only: "End the broadcast to switch server". The broadcast is never touched. |

After an Idle prefill, a one-line notice says what changed ("Room `<code>`
and nickname from link"). Every dropped parameter is listed in a quiet
note, as on the web.

### D5 — `relay=`: match a saved server or offer to add it (OD2)

- **The default fleet's origin**: select the default.
- **Exactly matches a saved profile's normalized origin**: select that
  profile. Its per-server secret applies (docs/40 D4). The non-default
  strip (docs/64 D3) shows it because it's now the selected profile.
- **No match**: a confirm card says "This link uses the server `<host>`.
  Add it and switch to it?" [Add and switch] [Keep `<current>`]. "Add"
  creates a profile named after the host with no secret, via
  `add_custom_server`. A secret prompt comes later from the existing flow
  if the server asks for one. Until the click, the link's room and
  nickname still apply, against the current server.
- The probe restarts on a switch, as `on_server_selected` does.

### D6 — How a link reaches the UI thread

- **Cold start**: the shell's `main` finds the link argument (D7), or on
  macOS the Apple Event (D10), and hands `Option<String>` to `shell::run`.
  It is applied after `seed_settings` and after the crash-resume check
  (shell.rs:660-681), so a resume offer and a link prefill don't fight. The
  resume prompt is shown first.
- **Warm handoff**: the single-instance listener (D8) or the macOS event
  handler sends `ShellMsg::OpenLink(String)` on the existing `msg_tx`. The
  250 ms pump applies it. There is no new thread-to-UI path.
- A new `Platform::raise_window(&self, activation: Option<String>)` brings
  the window to the front. The argument is the Wayland activation token
  (D8). The fake test platform counts calls.

### D7 — Arguments

- Windows registers `"<exe>" "%1"`, and the Linux desktop entry uses
  `%u`. The shell scans argv for **exactly one** argument whose scheme is
  `gawk:` (case-insensitive) and ignores everything else. Two link
  arguments mean the link is rejected.
- **The update relaunch strips link arguments.** `install_target()` stores
  argv verbatim and passes it back on "Install and relaunch"
  (shell.rs:1757-1776, 1873-1875), which would apply the launch link a
  second time (G10).

### D8 — Single instance (OD1)

One protocol over a per-user endpoint. The endpoint is also the lock:
whoever owns it is the primary.

- **Startup**: try to connect. On success, send the message and exit 0.
  If nothing answers, become the primary and listen. If two launches
  race, the platform lock decides and the loser retries the connect once.
- **Messages**: one UTF-8 line, `raise` or `open <link>`, at most 4 KiB.
  The answer is `ok`. The primary treats the content as untrusted and
  passes it through D2's parser like any other link.
- **Platforms**:
  - **Windows**: the named mutex `Local\fi.ioio.gawk.broadcast` decides
    who is primary. The primary listens on the named pipe
    `\\.\pipe\fi.ioio.gawk.broadcast.<user SID>`, with
    `PIPE_REJECT_REMOTE_CLIENTS` and a DACL that grants only the current
    user. Before sending, the secondary calls
    `AllowSetForegroundWindow(GetNamedPipeServerProcessId(..))`. The
    secondary is the process the user just launched, so it holds the
    foreground right and can pass it on. Without that call,
    `SetForegroundWindow` in the primary only flashes the taskbar button.
  - **Linux**: the session D-Bus name `fi.ioio.gawk.broadcast`, requested
    with `DO_NOT_QUEUE`. The object `/fi/ioio/gawk/broadcast` serves the
    interface `fi.ioio.gawk.Broadcast1` with `Raise(s token)` and
    `OpenUrl(s url, s token)`. `token` is the secondary's
    `XDG_ACTIVATION_TOKEN`, which the launcher sets, so that the primary
    can take focus on Wayland. The connection uses zbus's blocking API on
    its own thread. **zbus's `tokio` feature stays off** (docs/gotchas.md,
    docs/58 F-2). With no session bus, the app runs without single-instance
    and logs why.
  - **macOS**: LaunchServices already sends a second launch of the bundle
    (Finder, Dock, `open`, a link) to the running process, so there is no
    mechanism of our own. Running the binary directly, or `open -n`,
    bypasses it. That case is accepted and documented.
- The frozen Go `gawk-broadcast` also uses `broadcast.json` and is not
  covered. It is removed at LX7 (docs/58).

### D9 — Windows registration (OD4)

The primary does this at every startup, after D8:

```
HKCU\Software\Classes\gawk
    (default)            = "URL:gawk"
    URL Protocol         = ""
    GawkOwner            = "gawk-broadcast-desktop"
    DefaultIcon\(default) = "<exe>",0
    shell\open\command\(default) = "<exe>" "%1"
```

- **Decision function** (pure, unit-tested; G8). Write the keys when they
  are absent. Rewrite them when `GawkOwner` is ours and the command path
  differs. Leave them alone when the key exists without our marker
  (another program owns the scheme): log it and skip.
- **Don't register from a transient path**: under `%TEMP%`, which is where
  Explorer extracts a "run from inside the zip" launch, or from the update
  staging dir `.gawk-update`.
- The in-place update keeps the path (docs/48 `swap`), so registration
  survives updates.
- No unregistration. Deleting the portable EXE leaves a dangling key, and
  Windows then shows its "can't find the program" message. That is
  documented in the README's Windows section.
- Adds the `windows` feature `Win32_System_Registry` to `app-windows`.

### D10 — macOS

- `tools/macos/Info.plist.in` gains `CFBundleURLTypes` with one entry:
  `CFBundleURLName` = `fi.ioio.gawk.broadcast.link` and
  `CFBundleURLSchemes` = [`gawk`]. `bundle.sh`'s `plutil -lint` covers it.
  LaunchServices registers the scheme when the bundle is first launched or
  copied. The entitlements stay empty.
- **The link arrives as an Apple Event (`kInternetEventClass`/`kAEGetURL`),
  not in argv.** winit 0.30 owns the `NSApplicationDelegate` and doesn't
  forward `application:openURLs:`. AppKit also installs its own `GURL`
  handler during `finishLaunching`, which would replace one installed
  earlier. So:
  - `main` registers an `NSNotificationCenter` observer for
    `NSApplicationWillFinishLaunchingNotification` before `shell::run`.
  - The observer installs our handler with `NSAppleEventManager
    setEventHandler:andSelector:forEventClass:andEventID:`.
  - The handler is a `define_class!` object (objc2 0.6, following
    `notify.rs`'s pattern). It sends the URL string to D6's inbox.

  A cold-start link arrives as an event, not a startup argument, so
  `shell::run` gets `None` on macOS and the link follows within one pump.
- A dev binary outside a bundle (no `bundleIdentifier`) skips all of this,
  as `notify::init` does.

### D11 — Linux

- `tools/linux/fi.ioio.gawk.broadcast.desktop` gets
  `Exec=gawk-broadcast-linux %u` and `MimeType=x-scheme-handler/gawk;`.
- `install-desktop.sh` appends ` %u` after the quoted absolute path it
  already writes (:108-109). It then runs `xdg-mime default
  fi.ioio.gawk.broadcast.desktop x-scheme-handler/gawk` best-effort, next
  to its `update-desktop-database`. `--uninstall` leaves `mimeapps.list`
  alone: with the entry gone the association is dead, and editing the
  user's file isn't worth the risk.
- The `.deb` relies on desktop-file-utils' dpkg trigger, as today
  (build-deb.sh:26-28). With one handler installed, `xdg-open` resolves it
  from `mimeinfo.cache`, so the package sets no default.
- `build-deb.sh`'s and `test-deb.sh`'s exact checks on `Exec=` change to
  `Exec=gawk-broadcast-linux %u`. `test-deb.sh` also checks `MimeType=`
  and G9's `xdg-mime query default`.
- A bare tarball run without `install-desktop.sh` registers nothing. That
  is documented and consistent with how the launcher entry works today.

### D12 — Where the code lives

| Crate | Gets |
|---|---|
| `engine` | `link.rs` (D1, D2), the vectors, `parse_room_input`'s new forms |
| `wire` (Rust mirror) | `normalize_broadcast_id` made public |
| `ui` | `ShellMsg::OpenLink`, `apply_link` (D3–D5), the confirm cards and notices in `main.slint`, the startup parameter, D7's argument scan and relaunch stripping |
| `ui` (portable half of D8) | `instance.rs`: the message format, the `Instance` trait (`try_handoff`, `listen`), the primary/secondary decision |
| `app-windows` | the mutex + pipe `Instance`, D9's registration |
| `app-linux` | the D-Bus `Instance`, the activation token in `raise_window` |
| `app-macos` | D10's observer and Apple Event handler, a no-op `Instance` |

### D13 — Security posture

- A link is untrusted input from any web page. It can do no more than the
  user could by typing into the Room sheet and the nickname field. It
  never captures, never starts, never carries a secret, and never switches
  to an unknown server without a click.
- A link **does** persist a room choice and a nickname while Idle
  (`choose_room` saves). This matches the web's `#/broadcast?room=`, and
  the pending-room chip shows the choice before Go live. The notice in D4
  makes the change visible.
- The IPC endpoints are per-user (D8). Their messages go through the same
  parser and caps.
- **No raw broadcast IDs in logs.** A `watch` link is logged as its kind
  only (CLAUDE.md, docs/67 §6).

### D14 — `gawk://` vs `gawk-broadcast://`

`gawk-broadcast://windows|macos|linux|native` is an Origin label in the
relay's allowlist (`engine/src/lib.rs:67-89`), and the iOS app sends
`Origin: gawk://ios` (docs/67 D5). Neither is a URL scheme handler.
Nothing registers `gawk-broadcast`, and the Origin labels don't change.

## 5. Non-goals

- A desktop viewer. `watch`/`room` links go to the browser (D3).
- Universal links / Associated Domains / MSIX app links (§3).
- The frozen Go `gawk-broadcast` (docs/58: `fix` commits only).
- A Settings toggle or unregister button for the handler.
- The SPA's side (R67, docs/69) and the iOS app's handling (R65 IO6, on
  this grammar).
- R26 quick-start parameters (`res=`, `fps=`, …) in `gawk://broadcast`.
  R26 isn't built; when it is, the grammar can grow the same parameters
  under docs/31 D6.

## 6. What deliberately does not exist

No auto-start from a link; no secret in a link; no session-only server
override on desktop (OD2); no unregistration; no network listener for
IPC; no raw broadcast ID in a log line.

## 7. UX flows

**Cold**: the Mumble bot's "Start streaming" → R67 offers the desktop app
→ the browser's "Open gawk-broadcast?" prompt → the app starts with room
`<code>` pending and the nickname filled in → pick the source → Go live →
it attaches to the room as soon as it's live.

**Warm**: the same click while the app is open → its window comes to the
front with the same prefill.

**Live**: a link arrives during a broadcast → "Join room `<code>` now?" →
Join attaches the live broadcast, Not now leaves it alone.

**Unknown server**: a link with `relay=https://relay.friend.example` →
the room and nickname are applied, plus a card "This link uses the server
relay.friend.example. Add it and switch to it?"

**Viewer link on a PC**: `gawk://watch/ABC234` → the browser opens
`https://gawk.ioio.fi/#/view/ABC234`. No app window appears.

## 8. Risks

| Risk | Mitigation |
|---|---|
| AppKit replaces our `GURL` handler, or winit swallows it | D10 installs it from `WillFinishLaunching`; V-1 checks cold and warm on a real bundle |
| Wayland focus-stealing prevention leaves the window behind | D8 passes the activation token; V-2. If winit 0.30 can't apply a token to an existing window, the fallback is a desktop notification ("Link opened in gawk broadcast") via the existing `notify.rs` |
| Windows foreground lock: the primary can't raise itself | `AllowSetForegroundWindow` from the secondary (D8); V-3 |
| A prefill set only in `cfg` is overwritten by `read_settings` at Start | D4 sets the UI property too; a shell test starts after a link prefill and asserts the nickname survived |
| The update relaunch re-applies a link | D7 strips it (G10) |
| A stale Windows key after the EXE is deleted | Documented (D9); the next launch from the new location repairs it |
| Old app versions don't handle links | Unavoidable; R67's "didn't open?" fallback covers it (docs/69) |

## 9. Chunks and acceptance criteria

LH1 lands first: R67's HO1 restates its vectors. LH2–LH5 can then run in
parallel. LH6 waits for a release that contains all of them.

| Chunk | Scope | Accepted when |
|---|---|---|
| **LH1** | `engine::link` (D1, D2), the vectors, `parse_room_input`'s `gawk://` forms, the public broadcast-ID validator | The vectors pass. Every D1 rule has at least one accepting and one rejecting vector (case, alphabet, slug bounds, nick sanitizing, relay normalization, secret dropping, unknown params, the length cap). A table-driven garbage test over truncated and mutated vectors never panics (G7). Pasting either `gawk://` form into the Room sheet chooses the room. |
| **LH2** | Shell integration: D3–D7, the portable half of D8, the `main.slint` cards and notices | Shell tests on the fake platform cover each row of D4's table, every case in D5, D3's cold and warm viewer links (cold exits without `show()`), the nickname surviving a later Start, G10's relaunch stripping, and the `Instance` decision with a fake endpoint |
| **LH3** | Linux: D11, D8's D-Bus `Instance`, the activation token | `test-deb.sh` passes G9 on both containers. An app-linux test on a private `dbus-daemon --session` runs a primary and a secondary: the secondary's `OpenUrl` reaches the shell inbox and the secondary exits 0. `install-desktop.sh` writes `%u` and survives a path with spaces. |
| **LH4** | Windows: D9, D8's mutex + pipe `Instance` | The D9 decision function is unit-tested for all four cases (absent, ours with the same path, ours with a moved path, foreign). The pipe message codec is tested on the Linux host. `cargo xwin clippy` and `build` are green. The real pipe and registry are checked in LH6, since CI has no Windows runner (docs/38 D18). |
| **LH5** | macOS: D10 | `bundle.sh` output's `Info.plist` carries `CFBundleURLTypes` (checked with `plutil -extract`). `macos-check` is green. In CI on `macos-latest`, after `lsregister -f` on the freshly bundled, ad-hoc-signed app, `open 'gawk://broadcast?room=lh5-test'` produces the expected log line. |
| **LH6** | The owner's pass on each OS: G1–G6 and G8 with Chrome and Firefox (plus Safari on macOS), cold and warm, then V-1 to V-4 | Results recorded in §12 |

## 10. V-items (verified in LH5/LH6, recorded in §12)

| # | Question | Decides |
|---|---|---|
| V-1 | Does D10's handler receive the URL on a cold start and while running, with winit 0.30 owning the delegate? | D10 |
| V-2 | Does the Linux primary take focus on GNOME Wayland with the forwarded `XDG_ACTIVATION_TOKEN`? On KDE? | D8, §8's fallback |
| V-3 | Does `AllowSetForegroundWindow` from the secondary let the Windows primary raise itself when the launch came from Chrome/Firefox? | D8 |
| V-4 | Does each browser pass the full query through unchanged (`&`, `%20`, non-ASCII nicknames) to argv on Windows and Linux? | D1, D7 |

## 11. Open questions

None.

## 12. Deviations and field findings

None yet.
