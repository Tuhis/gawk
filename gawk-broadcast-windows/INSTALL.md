# Installing gawk-broadcast for Windows (prebuilt test build)

You've been handed `gawk-broadcast.exe`. This is a **test build of unreleased
software** — if it falls over, that is the expected outcome and the reason
you're running it. `BUILD-INFO.txt` next to it records exactly which commit
it came from.

## Does it run on your machine?

| Requirement | Check | If not |
|---|---|---|
| **Windows 10 version 2004 (build 19041) or newer, x86-64** | `winver` | No build for you yet — per-app audio capture needs 2004+ |
| **A hardware H.264 encoder** (any NVIDIA/AMD/Intel GPU from the last ~decade) | just run it; it tells you | Use the browser broadcaster at the app URL instead — this app has no software encoder, on purpose |
| Nothing else | — | The exe is fully static (one file, no runtimes, no installer) |

Windows-version fine print:

| Windows build | What changes |
|---|---|
| 19041 (Win10 2004) – 20348 | Everything works; captured windows/screens show the system's **yellow capture border** (the API to remove it doesn't exist there) |
| 20348+ (Win11) | Border removed; otherwise identical |

## SmartScreen will complain

The binary is unsigned on purpose (known-operator distribution, no
certificate money spent on a test build). On first run Windows shows
"Windows protected your PC" — click **More info → Run anyway**. If the
button is missing: right-click the exe → Properties → tick **Unblock** →
OK, then run it again.

## First broadcast

1. Run the exe. Defaults already point at the production relay — touch
   nothing.
2. Pick what to share: **Share an app** (that window + only that app's
   audio) or **Share a screen** (desktop + whole-system audio).
3. **Start broadcast**, then **Copy link** and paste it to whoever's
   watching.

Closing the window ends the broadcast — there is no tray icon, no
background presence.

## When it doesn't work

- **"No hardware H.264 encoder was found"** — the app refuses rather than
  software-encode. Check GPU drivers are installed (a fresh VM has none);
  otherwise use the browser broadcaster, which software-encodes fine.
- **Sharing an app, viewers hear nothing** — some games play audio through
  a helper process the per-app capture can't see. The app shows a hint
  after ~10 s of silence with a one-click switch to whole-system audio.
- **Shared window went black/frozen for viewers** — minimized windows
  aren't composited, so nothing can capture them. Restore the window
  (occluded/covered is fine — that's the whole point).
- **"Could not reach the relay"** — network/VPN/firewall: the relay speaks
  UDP on port 4433 (QUIC). Corporate networks that block UDP block this.
- **Toasts don't appear during a fullscreen game** — Focus Assist eats
  them; the in-window status is the truth.
- Anything else: expand **Details**, click **Copy diagnostics**, and send
  the JSON along with what you expected to happen.

## Settings that matter (all optional)

Blank always means "the default" — the app follows the fleet when it moves.
`Telemetry URL` set to `off` sends no diagnostics at all; by default a
session reports to the reference collector only when broadcasting to the
default relay.

## Licensing

`gawk-broadcast.exe` is [Apache-2.0](https://github.com/Tuhis/gawk/blob/main/LICENSE);
`THIRD-PARTY-NOTICES.md` in the repository lists every third-party component
compiled into it, with licences and copyright holders.

The GUI is built with **[Slint](https://slint.dev)** under its Royalty-free
Desktop License v2.0.

<a href="https://slint.dev"><img alt="Made with Slint" width="106"
src="https://raw.githubusercontent.com/Tuhis/gawk/main/docs/assets/MadeWithSlint-logo-light.svg"></a>
