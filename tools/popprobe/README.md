# popprobe: the R74 PP0 measurements

PP0 ([docs/76](../../docs/76-pop-relays.md) §3, §11) measures what a party
guest's browser and the host's OS do with a `gawk-relay` on the LAN before
anything is built on it. Each row below ends in a result line, `PP0 <row> |
<method> | <build> | <result>`, for the §3 table. Paste it as it is and add
what the line can't see (a prompt's copy, a screenshot).

What is here:

- `main.go` + `probe.html`: a stdlib-only Go server for one static page.
  The page dials the relay's `/echo` by IP literal with
  `serverCertificateHashes`, the way a guest's browser does, round-trips a
  datagram, and prints the browser build, `isSecureContext`, the
  Local Network Access permission states before and after, and the outcome.
  It has two controls: a dial with a wrong pin, which must fail, and a
  `fetch` of popprobe's `/ping`, which shows whether LNA gates `fetch` on
  that page. Results are also posted back to popprobe, which prints them, so
  a phone's results land in your terminal.
- `headless.sh`: the Chrome and Firefox rows run headless, with no
  WebDriver (the page posts its own results).
- `firewall.ps1`: the Windows Firewall row.
- The macOS desktop app row is the checklist at the end.

## Setup (once)

1. Start `gawk-relay` on a machine with a LAN address (zero flags is fine).
   Its console at `http://127.0.0.1:4480` shows the join link.
2. On the same machine, run popprobe. It reads the relay and its two pins
   from the console:

   ```sh
   cd tools/popprobe && go run . -lan-https :8443 -lan-http <lan-ip>:8091
   ```

   On another machine, give it the console's join link instead:
   `go run . -join 'https://gawk.ioio.fi/#/?relay=…&certHash=…'`. You can
   also pass `-relay https://<ip>:4433 -cert-hash <hex>`.
   - `http://127.0.0.1:8090/` is the page on this machine. Loopback is a
     secure context, so WebTransport is available.
   - `https://<lan-ip>:8443/` is the same page for a phone. Its
     certificate is a throwaway self-signed one, so accept the warning once.
     The page reports whether the browser still counts it as a secure
     context.
   - `http://<lan-ip>:8091/ping` is the LNA `fetch` control's target. Each
     request that arrives is logged as a `ping` line, so a blocked fetch
     leaves no line.
   - The page also works on any static host: open `probe.html` with
     `?relay=https://<ip>:4433&certHash=<hex>[,<hex>]`. This is the true
     public-origin case. Results then show only on the page; use **Copy
     result lines**.

Page parameters: `row=<label>`, `autorun=dial,wrong,ping` (runs on load, no
click needed), `timeout=<s>` per step (default 60), and `control=<url>` (the
`fetch` target).

## Safari 26.x, macOS and iOS

The row asks whether a pinned dial by IP literal works against this relay.
hyperium/h3#347 reports Safari rejecting hash-pinned handshakes.

- **macOS:** in Safari, open `http://127.0.0.1:8090/?row=safari-macos` on
  the popprobe machine. Click **Dial**, then **Dial with a wrong pin**.
- **iOS:** on an iPhone on the same Wi-Fi, open
  `https://<lan-ip>:8443/?row=safari-ios`, accept the certificate, and do
  the same. If the page says `isSecureContext: false` or
  `WebTransport: false`, the throwaway certificate cost the secure context.
  Repeat from a public HTTPS host for `probe.html` (above) and record both
  results.

Record the Safari and iOS versions (Settings → General → About) beside the
line. A pass needs both: **Dial** connected with `datagram echo ok`, and the
wrong pin FAILED. If **Dial** fails while the wrong pin also fails, Safari
cannot pin against this relay, and §6.12 changes: Safari guests use the
named tier only (PP0 acceptance).

## Headful Chrome: the Local Network Access prompt

The flag below marks the loopback page as *public*, so the dial is public →
private, as from `https://gawk.ioio.fi` (M1). Use a throwaway profile:

```sh
# macOS
open -na "Google Chrome" --args --user-data-dir=/tmp/pp0-chrome \
  --ip-address-space-overrides=127.0.0.1:8090=public "http://127.0.0.1:8090/?row=chrome-lna"
# Windows (PowerShell)
& "$env:ProgramFiles\Google\Chrome\Application\chrome.exe" --user-data-dir=$env:TEMP\pp0-chrome `
  --ip-address-space-overrides=127.0.0.1:8090=public "http://127.0.0.1:8090/?row=chrome-lna"
```

1. Click **Dial**. Screenshot the prompt and copy its text. Click **Allow**,
   and the line should read `connected` with the permissions `granted`.
2. **Reload persistence:** load `…/?row=chrome-lna-reload&autorun=dial`.
3. **Restart persistence:** quit Chrome completely (all windows), relaunch
   with the same command and `&autorun=dial`.
4. **Denied:** repeat with a fresh `--user-data-dir` and click **Block**.
   Then reload and restart as above, and note whether the prompt returns
   and how fast the dial fails.

Opening `probe.html` from a public HTTPS host, with no flag, gives the
real-origin version of the same row. On Android, also note the OS "nearby
devices" prompt.

## Firefox 153+

Firefox has no flag; the pref below does the same. Set it in `about:config`
on a throwaway profile (`firefox -P`):
`network.lna.address_space.public.override` = `127.0.0.1:8090`. Then open
`http://127.0.0.1:8090/?row=firefox-public&autorun=dial,wrong,ping&control=http://<lan-ip>:8091/ping`.
Without the pref, the same page is the loopback-origin row.

Headless, on any desktop OS with bash:

```sh
./headless.sh firefox /path/to/firefox public  https://<ip>:4433 <hash>[,<hash>] dial,wrong,ping <lan-ip>:8091
./headless.sh firefox /path/to/firefox loopback https://<ip>:4433 <hash>       dial,wrong,ping <lan-ip>:8091
./headless.sh chrome  /path/to/chrome  public  https://<ip>:4433 <hash>       dial,wrong,ping <lan-ip>:8091
```

`POPPROBE_PREFS='user_pref("network.lna.enabled", false);'` is the control:
with LNA off, a failure that LNA caused goes away. Headless browsers answer
no prompt: Chrome denies it, and Firefox leaves it pending until the step
times out.

## Windows Firewall vs an unsigned `gawk-relay.exe`

On the Windows host, use an **unsigned** build (`GOOS=windows go build
./cmd/gawk-relay` in `gawk-server`, or the CI artifact) and a network set
to **Public** (Settings → Network → the connection → Public network). On a
second LAN machine, build `gawk-echo` (`go build ./cmd/gawk-echo` in
`gawk-server`). In an elevated PowerShell on the Windows host:

1. Start `gawk-relay.exe`. Record SmartScreen and the *Windows Security
   Alert* prompt: its copy, and which of Private/Public are ticked by
   default. Answer **Cancel**.
2. `powershell -ExecutionPolicy Bypass -File firewall.ps1 -Watch 60 -Label "prompt cancelled"`.
   While it watches, run this on the other machine:
   `gawk-echo -url https://<win-ip>:4433/echo -cert-hash <hash> -count 3`,
   using the hash from the console's join link. The result line lists the
   profile, the rules naming the program, the UDP drops to 4433 and the
   relay's accepted `/echo` sessions.
3. In the console (`http://127.0.0.1:4480`), use **Allow on this network**
   and answer the UAC prompt. Repeat step 2 with `-Label "console rule"`.
   The rule should be scoped to the LAN subnet, UDP 4433 and TCP for the
   UI. The line shows its profile and remote address.
4. Optionally, repeat from a clean slate, answering the alert with
   **Allow**, to see which profiles Windows' own rule covers.

If no rule can be scoped this way (step 3 fails, or the rule doesn't cover
Public), §7.1's button changes (PP0 acceptance).

## macOS desktop app dialing a LAN relay (TN3179)

On macOS 15+, a Developer ID app gets the Local Network prompt the first
time it sends unicast to an on-link address. Use the signed desktop
broadcaster from a release, and a `gawk-relay` on **another** LAN machine.

1. **Before:** System Settings → Privacy & Security → Local Network. Note
   whether gawk is listed. A listed app was already asked, so the prompt
   will not show again; record its toggle and use a fresh macOS user
   account to see the first-run prompt.
2. Optionally, in a Terminal, start
   `log stream --info --predicate 'eventMessage CONTAINS[c] "local network"'`.
3. Open the console's server link: `open 'gawk://server?relay=…&certHash=…'`
   (the console shows it). Then open the app's Ready page, where it probes
   the server.
4. Record: whether the prompt appears and when (on link open, on probe, on
   Go live), its exact copy, and the app's state while it is up.
5. **Allow:** the server shows as reachable, and a broadcast starts.
   **Don't Allow** (from a fresh account): record what the app shows and
   whether the error points the user to System Settings.
6. Write the line by hand:
   `PP0 macos-app | gawk-broadcast <version>, server link to https://<ip>:4433 pinned | macOS <version> (<build>) | <prompt copy; allowed → …; denied → …>`.
