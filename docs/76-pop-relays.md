# R74 — PoP relays: a one-click LAN relay that federates with the home fleet (docs/76)

**Status**: designed 2026-10-21 in an owner interview (decisions OD1–OD14
below); revised 2026-10-22 for R73's reliable uplink (OD15–OD16: both legs
carry deltas on R73's carrier); **implemented 2026-10-22**: PP2–PP14 are
built and their automated acceptance rows pass, and PP8's uplink lab ran
(§11.2: the carrier stays the default). The rows still open
need the owner, hardware or a real release, and none of them is reported
as passed: PP0's measurements (the harness has not landed either), PP1's
sign-off, PP4's notarization and first real release, PP8's time to first
frame at 40 ms netem, PP9's downlink lab run, PP12's staging issuance,
PP14's device prompts, the PP15 party pass, and every Swift/Xcode build,
which this host cannot run (§11.1). Chunks **PP0–PP15** (`PP` = PoP; unused). Three phases,
each releasable on its own: **A** — the standalone relay (PP0–PP5), **B** —
pairing, federation, the party wall and the named address (PP6–PP12), **C** —
home suggests the local relay and clients switch to it (PP13–PP14), then the
party pass (PP15).

Touches `gawk-server` (a new `cmd/gawk-relay`, the `/pop/*` routes, a public
`pops` package), `gawk-admin` (a PoPs view, its API, a certificate and DNS
reconciler), `gawk-app` (pinned relay links, the automatic switch),
`gawk-broadcast-desktop` and `gawk-ios` (certificate pinning, the switch,
Apple's local-network permission) and the wire (types `0x18`/`0x19`, close
codes `4012`/`4013`). Builds on R17 ([docs/22](22-relay-scale-out.md)), R18
([docs/23](23-live-viewer-count.md)), R37 ([docs/40](40-relay-server-picker.md),
whose D3 it revises), R39 ([docs/42](42-admin-moderation-portal.md)), R42
([docs/44](44-rooms.md)), R57 ([docs/59](59-close-notice.md)) and R73
([docs/75](75-reliable-uplink.md), whose carrier both legs use).

## 1. Purpose

At a LAN party today, everyone streams through the home fleet
(`api.gawk.ioio.fi`) over the venue's internet connection. A broadcast goes
up the venue's uplink once, and comes back down once for every guest who
watches it, even though the broadcaster and the viewer are on the same
switch. Thirty guests, six of them streaming at 8 Mbps, each guest watching
two streams: 48 Mbps up and about 480 Mbps down through a line that is
usually the scarcest thing at the venue.

R74 adds a relay you bring to the party:

- **`gawk-relay`**, the relay in a standalone, zero-configuration form: a
  download for Windows, macOS and Linux (and a multi-arch container), which
  runs with LAN defaults and its own certificate when you double-click it, and
  shows a console with the join QR code.
- **Federation with the home fleet.** A paired relay — a *PoP* — shares the
  home fleet's broadcast codes. Party guests stream and watch over the LAN.
  People outside watch party streams through the home fleet, and a stream
  crosses the venue's uplink **once, and only while someone outside is
  watching**, within a budget the operator sets. The reverse holds too: a home
  stream watched by ten guests crosses the venue's downlink once.
- **A party wall**: one link where outsiders see every party stream in the
  existing room grid.
- **Home suggests the local relay.** A guest who opens `gawk.ioio.fi` (or a
  native app) at the party is recognised by the party's public address, and
  their client moves to the PoP by itself.
- **PoPs in `gawk-admin`**: create one, pair it, watch it, set its budget,
  moderate its streams, revoke it — backed by a `Pop` custom resource.

docs/22 left this open on purpose: its non-goals say a geo PoP "is the same
protocol over WAN, designed later". That holds, with one twist that shapes
most of this doc: a party relay sits behind the venue's NAT. So **the PoP
dials out**, and the home fleet asks it for streams over a control link,
rather than pulling them itself.

## 2. Owner decisions (2026-10-21; OD15–OD16 2026-10-22)

| # | Question | Decision |
|---|---|---|
| OD1 | Timeline | **No date pressure.** Design the whole thing; build in dependency order. The party is a use case, not a deadline. |
| OD2 | What the host runs | **A standalone binary** (`gawk-relay`, Windows/macOS/Linux) plus a multi-arch container, with a status console on `localhost`. A "Host a relay" switch inside the desktop broadcaster is deferred (§13). |
| OD3 | How a guest ends up on the PoP, once home can suggest it | **Switch automatically.** Native apps switch silently. A browser switches silently once the site has the local-network permission; the first time, the browser's own prompt is the only click (§3, §6.12). |
| OD4 | Which party streams outsiders can watch | **Any code, on demand.** As everywhere in gawk, the code is the access. A party stream crosses the uplink only while at least one outside viewer watches it, exactly once, within the budget. |
| OD5 | Certificates | **Both tiers.** A pinned self-signed certificate (always), and a named certificate `p-<label>.<zone>` (e.g. `p-7k2m9x.pop.gawk.ioio.fi`) issued through the home fleet's ACME issuer, with the PoP's private key never leaving the PoP (§6.11). |
| OD6 | Pairing | **A token from `gawk-admin`.** The operator creates the PoP and gets a one-time pairing string; the first link exchanges it for a long-lived credential. |
| OD7 | Moderation | **Home reaches the party.** A kill or ban in `gawk-admin` ends a party broadcast at the party too; revoking a PoP stops forwarding and suggestions at once. |
| OD8 | The party wall | **Part of federation v1** (phase B), not a later phase. |
| OD9 | The named certificate's A record | **external-dns, CRD source.** `gawk-admin` writes a `DNSEndpoint` per PoP; the deployment's external-dns publishes it. |
| OD10 | How the wall spends the uplink | **The existing grid, capped by the budget.** Tiles beyond the budget say "Party-only right now"; the first outsiders take the slots. |
| OD11 | Which broadcasts appear on the wall | **All, with an opt-out.** The broadcaster sees "On the party wall" and a Hide switch. |
| OD12 | A certificate hash in links | **Allowed, beside a private-range IP literal only.** Revises docs/40 D3 (§6.1). |
| OD13 | The uplink budget | **The operator sets a ceiling; the host can lower it.** Default 25 Mbps and 3 streams. The downlink is unlimited by default. |
| OD14 | Screening party streams | **Only what home serves.** R40 screens a party stream whenever the home fleet carries it; LAN-only streams are the host's responsibility (§6.9). |
| OD15 | How the uplink leg carries a party stream (asked once R73 had landed) | **R73's carrier whenever home offers it.** Keyframes keep their own streams; deltas and parity ride one carrier per GOP. The leg is leg A for every outside viewer, on a venue uplink shared with the guests' own traffic: docs/75 table O's case (§3). Datagrams stay one flag away (`-pop-leg-delivery`), and PP8 measures the PoP's own sender before the default ships (item 18). *Measured 2026-10-22: the carrier stays (§11.2).* |
| OD16 | How the downlink leg carries a home stream | **The carrier too, in v1.** Home writes the leg the way the PoP writes its uplink, and the PoP reads it with R73's carrier reader (item 23). |

## 3. Measured and verified before writing

Following the measure-first rule, the claims this design leans on were tested
or sourced on 2026-10-21. The local tests used the relay built from `main`
(`-dev-cert`, two instances: one whose certificate's SANs include the LAN
address `10.0.2.2`, one whose SANs do not), a static test page, and Chrome
for Testing **151.0.7922.34** headless through Playwright. A page was made
"public" for Chrome's address-space checks with
`--ip-address-space-overrides=127.0.0.1:8080=public`.

| # | Claim | How | Result |
|---|---|---|---|
| M1 | A public page cannot open WebTransport to a LAN address without permission | public page → `https://10.0.2.2:4433/echo`, pinned | **Blocked.** The console logs `net::ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS`; script sees only `WebTransportError: Opening handshake failed`. A dial to `127.0.0.1` is blocked the same way |
| M2 | Granting the permission unblocks it | `Browser.setPermission({name: 'local-network'})` (and `loopback-network`) for the page's origin and browser context | **Connects** (7 ms). `navigator.permissions.query` knows `local-network`, `local-network-access` and `loopback-network`, and reports `prompt` before the grant |
| M3 | A page served from a LAN address needs no permission for a LAN relay | page at `http://10.0.2.2:8081` (made a secure context by flag) → `10.0.2.2:4433` | **Connects** without any grant |
| M4 | A loopback page needs no permission for a LAN relay | page at `http://127.0.0.1:8080`, default address space | **Connects** |
| M5 | A pinned certificate need not name the address | relay whose cert SANs are `localhost` only, dialed as `https://10.0.2.2:4434` with its hash | **Connects.** Chrome checks the hash, the validity span and the key type, not the name |
| M6 | The Rust clients can pin | wtransport 0.7.1 docs | `ClientConfigBuilder::with_server_certificate_hashes` exists, with the same rules (ECDSA P-256, ≤ 2 weeks, within validity) |
| M7 | The relay will not start without a certificate | `certSource` (`cmd/gawk-server/main.go:644`) | Zero flags exits with `no certificate configured`. Its defaults are `-max-broadcasts=5`, `-max-subscribers=15`, `-max-total-subscribers=50` |

Sourced, not measured here (primary sources gathered 2026-10-21):

- **Chrome** gated `fetch` behind the Local Network Access prompt in 142 and
  extended it to WebTransport and WebSockets in **147** (stable 2026-04-07).
  "Local" is judged by the IP address the connection uses, so a public DNS
  name that resolves to a private address is still local. Chromium does not
  restrict local→local. The grant is a per-origin site setting. Android also
  needs the OS "nearby devices" permission. Enterprise policies can pre-allow
  origins (`LocalNetworkAllowedForUrls`, 146+).
- **Firefox** ships LNA by default from 153, with WebSockets covered from 154.
  **WebTransport is not enforced in release yet** (bug 2042527; the tests
  were enabled on autoland on 2026-10-21), so expect the same prompt there
  within a few releases.
- **Safari** shipped WebTransport in **26.4** (2026-03-24, macOS and iOS), and
  WebKit implements `serverCertificateHashes`. Hash pinning against a
  webtransport-go relay is **unproven**: one upstream report (hyperium/h3#347)
  describes Safari rejecting hash-pinned handshakes that Chrome accepts.
  Safari with a public certificate works against this relay (verified
  2026-09-24, `docs/gotchas.md`). WebKit has no LNA prompt yet.
- **Public certificates can never name a private address.** The CA/B Baseline
  Requirements forbid reserved IP ranges, and Let's Encrypt's IP certificates
  (GA 2026-01-15) are public-address only. Let's Encrypt's **6-day
  `shortlived` profile** is also GA. Its rate limits: 50 new certificates per
  registered domain per week, and 5 duplicates. ARI renewals are exempt.
- **DNS rebinding protection** (pfSense and OpenWrt by default, FRITZ!Box,
  OPNsense, some ISP resolvers) drops public names that resolve to private
  addresses. Plex, whose `*.plex.direct` scheme is the precedent, documents
  it as an occasional failure with a fallback.
- **Apple Local Network privacy** (TN3179, revised 2026-10-06) prompts the first
  time an app sends unicast UDP to an on-link address: on iOS 14+, and on
  macOS 15+ for Developer ID apps, which covers the desktop broadcaster.
  Safari and WKWebView are exempt. Chrome on macOS needs the OS permission
  for itself.
- **Windows Firewall** prompts the first time an app listens with no allow
  rule, and new networks default to the *Public* profile. An inbound rule
  that covers only Private silently drops QUIC on a Public-profile LAN.
- **R73's lab** ([docs/75](75-reliable-uplink.md) §11: quinn's sender into
  quic-go through a shaping proxy; read here 2026-10-22). Beside a
  household's TCP upload, on Cubic, the Balanced viewer saw
  98.0 / 82.5 / 91.1 % of frames on the carrier against 88.9 / 18.6 / 30.1 %
  on datagrams, on 30 / 15 / 80 Mbps uplinks (table O2, F17, measured
  2026-10-18). The carrier costs nothing on a clean path (table A) and keeps GOPs
  whole through bursts and outages (F2). quic-go, the PoP's stack, ships
  Cubic and no BBR, and R73 never measured it as a sender.

Not measured, and therefore PP0's job before anything is built on it: Safari
26.x pinned dials against this relay, the headful Chrome prompt and how long
its grant persists, Windows Firewall against an unsigned `gawk-relay.exe`,
the macOS prompt for the desktop app dialing a LAN relay, and time to first
frame for an outside viewer over a real WAN. The PoP's own carrier sender
on a shared uplink is PP8's to measure, and the downlink's PP9's.

| # | Claim | How | Result |
|---|---|---|---|
| PP0 | Safari 26.x pinned dial by IP literal; headful Chrome prompt copy and grant persistence; Firefox 153+; Windows Firewall vs an unsigned `gawk-relay.exe`; the macOS desktop app's prompt | — | **Not yet measured (needs owner hardware).** The decisions that depend on them stand as designed: Safari is pinned-unproven, so §6.12 keeps the named-tier caveat |

PP8 measured the PoP's own carrier sender in R73's lab on 2026-10-22
(§11.2). PP9's downlink run is still pending.

## 4. Vocabulary

| Term | Meaning |
|---|---|
| **Home fleet** | The gawk deployment a PoP is paired with: its relay pods, its `gawk-admin`, its web app. For the reference deployment, `api.gawk.ioio.fi` and `gawk.ioio.fi`. |
| **PoP** | A `gawk-relay` paired with a home fleet. Before pairing, or after a revoke, it is a **standalone relay**. |
| **Link** | A PoP's long-lived control session to the home fleet (`CONNECT /pop/link`). Lands on any relay pod, its **link-home pod**. |
| **Uplink leg** | A session that carries one party broadcast from the PoP to the home fleet, opened on demand (`CONNECT /pop/publish/{id}`). |
| **Downlink leg** | A session that carries one home broadcast from the home fleet to the PoP, opened on demand (`CONNECT /pop/subscribe/{id}`). |
| **Carrier** | R73's per-GOP reliable stream of delta and parity records (docs/75 D1). Both legs use it when the far side supports it (OD15, OD16). |
| **Reservation** | The home fleet's record that a broadcast ID belongs to a PoP. It exists whether or not the broadcast is forwarded at the moment. |
| **Budget** | How much the PoP may push up (uplink) or pull down (downlink) at once: Mbps plus a stream count. |
| **Party wall** | A static room at home that every party broadcast joins automatically. |
| **Offer** | The home fleet telling a client, in-band, "there is a local relay on your network, here is how to reach it". |
| **Pinned / named** | The two ways to reach a PoP: its IP literal plus certificate hash, or its public name with a publicly trusted certificate. |

## 5. Architecture

```
                      venue LAN                         │  internet  │            home fleet (k8s)
                                                        │            │
 guest browsers ──┐                                     │            │      UDP LB :4433 (one VIP)
 desktop apps ────┼── UDP 4433 ──► gawk-relay (PoP)     │            │   ┌──────────┬──────────┬──────────┐
 iOS app ─────────┘   (pinned or   ├ hubs: party ───────┼─ link ─────┼──►│ pod A    │ pod B    │ pod C    │
                       named)      │   broadcasts       │ (control)  │   │ link-home│ outside  │ origin   │
 venue QR ── TCP 4433 ─► web UI    │ ├ edge hubs: home  │            │   │ for PoP  │ viewer   │ for a    │
 (named tier only)      (embedded) │   broadcasts       │            │   │          │ of X ────┼─►party X │
                                   │ └ console          │            │   │          │ edge-pull│ (uplink  │
 host's own machine ── 127.0.0.1 ──┘   127.0.0.1:4480   │            │   └──────────┴──────────┴──────────┘
                                                        │            │      ▲ uplink leg X (on demand)
                                   gawk-relay ──────────┼────────────┼──────┘ lands on any pod; that pod
                                                        │            │        becomes X's origin (Lease)
                                                        │            │      ▼ downlink leg Y (on demand)
                                   gawk-relay ◄─────────┼────────────┼────── any pod serves it, as for a viewer
                                                        │            │
                                                        │            │   gawk-admin: Pop CR, pairing, budget,
                                                        │            │   CertificateRequest + DNSEndpoint
```

- The PoP is **one process**: the same relay code as a home pod, with its
  own registry, hubs, rooms and DVR. Its broadcasts are its own, and their
  IDs are reserved at home.
- Every connection between the PoP and the home fleet is **outbound from the
  PoP to the home fleet's public UDP endpoint**: the link, every uplink leg
  and every downlink leg. Nothing at home ever dials the PoP.
- **Ownership fixes direction.** The PoP pushes the broadcasts it owns and
  pulls only broadcasts the home fleet owns. No configuration can make a
  stream loop.

## 6. Design

### 6.1 The standalone relay (`gawk-relay`)

1. **One codebase, two entry points.** `gawk-relay` is a new
   `cmd/gawk-relay` in the `gawk-server` module. It runs the same
   `internal/transport`, `internal/hub` and `internal/roomsrv` code, with a
   different profile and an embedded console. `gawk-server` keeps failing
   closed when its certificate is missing. docs/41 §4.2.1 calls that row
   load-bearing: a deployment whose certificate Secret failed to mount must
   crash, never self-sign. A distinct binary is what lets a party host
   double-click something that self-signs without weakening that. It is
   released under the `gawk-server` component tag, as the distribution
   `gawk-relay` (the docs/46 distinction).

2. **The profile changes defaults, never mechanisms.** Every value below is
   an existing or new flag with its `GAWK_*` env, plumbed through
   `registryOptions` (the R2 rule). The profile only picks LAN-shaped
   defaults:

   | Knob | `gawk-server` default | `gawk-relay` default | Why |
   |---|---|---|---|
   | certificate | none: refuse to start | pinned self-signed, persisted, rotated (item 3) | zero setup |
   | `-max-broadcasts` | 5 | 50 | a party's whole roster can stream |
   | `-max-subscribers` (per broadcast) | 15 | 100 | a projector, a hall |
   | `-max-total-subscribers` | 50 | 500 | the host's NIC is the real cap |
   | `-rooms` | off | on (in-memory) | rooms are useful at a party with or without home |
   | `-resume-token-key` | derived from the publish secret, else per-process random | generated once, **persisted** in the state directory | a relay restart resumes every broadcast (R17 W2), with or without a publish secret |
   | `-stats-key` | per-process random | generated once, persisted | `/statusz` and metric keys stay the same across restarts |
   | `-stateless-reset-key` | empty: stateless reset **off** | generated once, persisted: **on** | after a crash, clients get a reset in about one RTT instead of waiting out the idle timeout (docs/22 D3) |
   | `-metrics-addr` | `:2112` | `127.0.0.1:2112` | the ops surface stays on the host |
   | `-allowed-origins` | empty (allow all) | empty (allow all) | a LAN relay is gated by the LAN; docs/40 D13 still applies |
   | state directory | — | `%LOCALAPPDATA%\gawk-relay`, `~/Library/Application Support/gawk-relay`, `$XDG_STATE_HOME/gawk-relay`; `/data` in the container | one place for keys, certificates, credential, logs |

   R73's knobs keep `gawk-server`'s defaults: `-uplink-carriers` on, so a
   native broadcaster on the venue's Wi-Fi can use the carrier to the PoP
   (docs/75 D4), and `-uplink-max-bitrate` 50 Mbps, which also sizes the
   PoP's downlink dialer (item 23). The binary is built from `gawk-server/`
   like the relay, so it carries the vendored 512-deep datagram queue
   (docs/75 D7) that browser and *Lowest latency* broadcasters need.

   The relay listens on UDP 4433 on every interface. The console listens on
   `127.0.0.1:4480` and opens in the default browser on the first interactive
   start.

   *Built:* the profile is `config.RelayProfile` (`ParseRelayFlags`), and
   the relay-only knobs are a separate `config.RelayConfig` that
   `gawk-server` never registers. The relay body both binaries share lives
   in `internal/serve` since PP2 (`registryOptions` in `options.go`, the
   subsystem order in `wiring.go`); `cmd/gawk-server/main.go` keeps flag
   parsing and the fail-closed `certSource`. An empty `-state-dir` means
   the per-OS default (`relaystate.DefaultDir`). On the relay, an explicit
   `-cert-file`/`-key-file` pair overrides the rolling pinned certificate,
   and `-dev-cert` is ignored with a warning.

3. **The pinned certificate.** It is an ECDSA P-256 certificate with a
   13-day validity, generated into the state directory (the docs/41
   `LoadOrGenerate` rules, unchanged). SANs: every LAN address, plus the
   named host when there is one; M5 shows they are not checked when pinned.
   It **rolls while running**. Each certificate is minted six days before
   it starts serving and serves for six days, through the existing
   `GetCertificate` reloader. The first start mints two. The relay always
   publishes the hash it serves and the next one (offers, console, link). A
   client pinning both accepts either, so a rotation never breaks a session
   or a fresh offer, and a printed link or QR code keeps working for **at
   least six days** after it was made (up to twelve). Twelve days of use
   fits inside the 13-day validity.

   *Built:* the schedule is anchored. Rotation k happens at
   `mintedAt + k·6d`, and the certificate it mints is issued at that
   scheduled time, so a late rotation (no handshake, a suspended laptop)
   still promotes a certificate valid for its whole serve window. A relay
   that was off for longer than both windows mints a fresh pair. State
   lives in `<state-dir>/certs/{cert,key}-{a,b}.pem` and `rolling.json`; a
   torn rotation re-mints the next slot on load. The SANs come from the LAN
   watcher at each mint, so a DHCP change reaches the certificate only at
   the next rotation, which M5 makes harmless.

4. **LAN address.** The primary address is the IPv4 address of the
   interface that holds the default route. Loopback, link-local, Docker and
   virtual bridges, and CGNAT/VPN ranges (`100.64/10`) are excluded. The
   console lists the candidates and lets the host pick another. A DHCP change
   is picked up and republished (offers, console, DNS record).

   *Built:* only RFC 1918 IPv4 and IPv6 ULA (`fc00::/7`) are candidates,
   since pinned links are accepted only for private literals; a public or
   CGNAT address is never one. With no default route (a LAN party with no
   internet) the first IPv4 candidate in interface-name order is primary.
   The excluded interfaces are container and VM bridges and host-only
   adapters (Docker, libvirt, VMware, VirtualBox, macOS `bridge100`+) and
   VPN tunnels (`utun`, Tailscale, WireGuard `wg*`, `tun*`/`tap*`,
   ZeroTier); the full list is `excludedIfacePrefixes` in
   `internal/lanaddr`. Interface-name exclusions are case-sensitive
   (`veth`, not Windows' `vEthernet`, which can be the real uplink behind a
   Hyper-V switch). The console's override lasts until restart;
   `-lan-address` is the persistent form. A linked PoP re-sends its hello
   when the address or the pinned hashes change, so offers, `status.link`
   and the DNS record follow without a relink; a relay off port 4433
   reports `ip:port`.

5. **Standalone guests.** With no home fleet there is no named certificate,
   so browsers reach the relay by IP literal and hash. The console's join QR
   code and link are
   `https://gawk.ioio.fi/#/?relay=https://192.168.1.20:4433&certHash=<hex>,<hex>`
   (the two published hashes):
   any gawk UI that allows custom relays (docs/40 D6, default on) loads the
   page, and the landing page offers **Save and use this server**.
   **This revises docs/40 D3** (OD12). `certHash=` is accepted only beside a
   `relay=` whose host is an IP literal in a private range (RFC 1918,
   `fc00::/7`). D3's reasoning was that "a cert hash is useless
   cross-device"; for a LAN relay it is the opposite. A hash is pinning data,
   not a credential, and a crafted link could already name any relay with a
   real certificate. Restricting it to private literals keeps it from
   becoming a way to point people at self-signed relays on the public
   internet (rejected in §13). Native apps get the same through
   `gawk://server?relay=…&certHash=…&label=…`, a new form in the R66 grammar
   that adds a saved server and selects it.

   *Built, web:* the rule is `parseCertHashList` plus `isPinnableRelay` in
   `gawk-app/src/routing.ts`. `certHash=` is one or two comma-separated
   lowercase 64-hex values, and a repeat collapses to one. It is kept only
   beside a `relay=` whose `URL.hostname` is an IPv4 literal in 10/8,
   172.16/12 or 192.168/16, or an IPv6 literal in `fc00::/7`. Anything
   else, including a hostname that merely looks like such an address,
   drops the whole list into `droppedParams` with a quiet note. The landing
   route carries the query; its **Save and use this server** adds the relay
   with its hashes and selects it, and the page's Join and Start-a-stream
   hops keep the link's relay until then. Share links and in-app hops add
   `&certHash=` only beside a private literal.

   *Built, native:*
   `gawk://server?relay=<https-origin>[&certHash=<hex>[,<hex>]][&label=<name>]`.
   `relay` is required and must not be the default fleet, else the link is
   rejected with `no-relay`. `certHash` follows the web rule above; outside
   it the hashes are dropped as `invalid` and the link still opens. `label`
   follows the nickname rules, cut to 63 bytes. In the canonical form,
   `relay` and `label` are encoded like `encodeURIComponent` and the hashes
   are written raw with a comma; the https form is the standalone landing
   link `https://<app>/#/?relay=…&certHash=…`, with no label. The vectors
   are `gawk-broadcast-desktop/crates/engine/tests/link-vectors.json`
   (`server/`, `certhash/`, `label/`), and the console's `ServerLink`
   matches the canonical form byte for byte.

   A saved server on the desktop and iOS may carry `certHashes`. When it
   is non-empty, every dial to that server is pinned through wtransport's
   hash verifier (publish, reclaims, the room session, the probe and Test
   connection, and on iOS the viewer too), and the system store is not
   consulted. The verifier enforces the WebTransport rules: at most 14
   days' validity, an ECDSA P-256 key, no name check (M5). An unknown
   server from a link is added only after a click, and new pins for a
   saved server need a "Trust and switch" click. A link without hashes
   never unpins, moving a profile to another origin drops its pins, and
   nothing changes while live. Pins saved from a link stay valid for at
   least six days (item 3); after that the saved server stops connecting
   until a fresh link, or a PoP offer, re-pins it.

6. **The host's own machine** can open the PoP-served web app at
   `http://127.0.0.1:4480/app/` (loopback is a secure context, and M4 shows
   it reaches the relay without a prompt). This is how a host with no
   internet at all still has a working broadcaster and a projector view.
   Its generated `config.js` names the relay by LAN address with its
   hashes, in a new `relayCertHashes` field: the `devCertHashHex`
   mechanism, generalised to two hashes and to a non-default URL. It also
   sets `requirePublishSecret: false` explicitly, because on a loopback
   origin the fallback would prompt for a secret (the docs/41 §4.2.2 trap).

   *Built:* `relayCertHashes` (string[]) pins `relayUrl` only. Unlike
   `devCertHashHex` it is not dev-gated, and it outranks `devCertHashHex`;
   a hash saved or typed for the server still wins over both. The gawk-app
   chart renders `config.relayCertHashes` (default `[]`), and
   `dev/config-gen.sh` renders it from an optional `RELAY_CERT_HASHES`. The
   console's app is the gawk-app build embedded at
   `cmd/gawk-relay/webapp`, built with `vite build --base=./` so the same
   bytes serve under `/app/` and at `/` (the named tier). `stage.sh` strips
   the static `config.js`, so the console always serves a generated one. A
   tree with nothing staged embeds a placeholder page carrying
   `<meta name="gawk-relay-webapp" content="placeholder">`, which release
   builds refuse (`GAWK_RELAY_REQUIRE_APP=1`).

### 6.2 Pairing

7. **The operator creates the PoP in `gawk-admin`**: a name (the CR name and
   default wall code, e.g. `assembly-2026`; at most 63 characters, because it
   is also a label value, item 14), a display name, an optional end
   date, the budget and the guest options (§7.2). `gawk-admin` writes the
   `Pop` CR and shows **one pairing string, once**:
   `gawk-pair:api.gawk.ioio.fi:4433/K7M2Q-9XDA4-…` (the home relay endpoint,
   then 128 random bits in base32: RFC 4648 without padding, upper case, a
   `-` every five characters; a pasted string tolerates lower case, missing
   dashes and surrounding whitespace, and the hash is taken over the
   dash-stripped, upper-cased token). Only its SHA-256 is stored
   (`spec.pairing.tokenSHA256`), with an expiry (default 72 h,
   `-pop-pairing-ttl`).

8. **The host pastes it** into the console, or starts the container with
   `GAWK_PAIR=<string>`. The PoP opens `CONNECT /pop/link?proto=1&pair=<token>`.
   The pod that receives it checks the string's hash and expiry, and that
   it is unused: no credential yet records this string
   (`status.credential.pairingTokenSHA256` ≠ `spec.pairing.tokenSHA256`).
   It mints a 256-bit credential and writes its SHA-256, `pairedAt` and
   `pairingTokenSHA256` into `status.credential`, with a CAS on the CR's
   `resourceVersion`, so a string pairs exactly one PoP.
   `spec.pairing.expiresAt` only limits how long an unused string can be
   redeemed; it never ends a credential that was paired in time. The pod
   then returns the credential once in the first link record. The PoP
   stores it in the state directory (DPAPI on Windows, `0600` elsewhere),
   and from then on authenticates every link and leg with
   `Authorization: Bearer <credential>`. Status carries the
   credential's fingerprint, never the credential itself: the R42 precedent
   (`status.creatorTokenFingerprint`).

9. **Re-pair and revoke.** *Re-pair* writes a new `spec.pairing` and
   nothing else. A credential is honoured only while the string it was
   paired with is the current one
   (`status.credential.pairingTokenSHA256` = `spec.pairing.tokenSHA256`),
   so the old credential stops working at once. Every string is random, so
   two Re-pairs in the same second still differ, which a whole-second
   `metav1.Time` would not. Every pod checks this on each link and leg. The
   link-home pod sends `bye{repair}` and closes the live link with `4006`,
   and the console says "Re-pair needed". `gawk-admin` never writes
   `status.credential`: the relays own it (§9). *Revoke* sets
   `spec.revoked` (or deletes the CR): see §6.9. A pairing string that has
   been used, or has expired, is refused with a status the console can show
   (Go reads the HTTP status, docs/22 D9). In `-pops-file` mode there is no
   pairing: the entry's credential hash is the credential, the check above
   is skipped, and rotating means editing the file.

   *Built:* a presented credential's hash is compared first, in constant
   time, so a wrong credential gets `401` whatever state the PoP is in;
   only a matching one learns `403` (revoked, or past `notAfter`) or `401`
   (re-paired). A `-pops-file` home has no pairing string, so the host
   gives the relay `gawk-cred:<host:port>/<credential>` instead (`-pair` or
   the console), and editing the entry's `credentialSHA256` ends the live
   link with `bye{repair}`.

### 6.3 The link

10. **`CONNECT /pop/link?proto=1`** goes to the home fleet's public UDP
    endpoint. It carries the credential header, and the PoP's Origin is
    `gawk-relay://pop`, honoured only on `/pop/*` routes (the docs/22 finding
    12 pattern for `internalEdgeOrigin`). The session carries one
    bidirectional control stream of length-prefixed JSON records. This is a
    Go↔Go protocol between two builds of the same module, like
    `/internal/subscribe`. It lives in `internal/poplink`, is versioned by
    `proto` (an unknown major gets a plain `426`, per docs/22 D16) and is
    **not a wire type**: no browser or native client ever speaks it, so it is
    not mirrored. Unknown record types are ignored, so minor additions are
    additive. Further bidirectional streams on the same session carry
    proxied room control (§6.10). QUIC idle timeout 15 s, keepalive 5 s.

11. **Records** (PP7 pins the schema). PoP → home: `hello` (version, LAN
    address, pinned hashes, named host, health), `reserve`/`release`
    (broadcast IDs), `state` (live/away per broadcast), `wall` (attach/detach
    with label), `refuse` (a want it can't meet, with a reason), `csr`,
    `stats` and `health`. Home → PoP:
    - `config`: display name, home app origin, budget ceilings, wall code,
      auto-switch, the R40 screening notice, end date, whether home runs
      `-rooms` (item 16), and the **assigned named host** once
      `status.tls.host` is set (item 38).
    - `reserved`: `ok`, `conflict` or `limit` (item 14).
    - `want`, `ban` and `certificate`.
    - `bye{reason}`: the last record before home closes the link with
      `4006`. The reasons are `repair`, `revoked` and `expired`. `4006`
      alone cannot tell them apart, and the console needs to. If the `bye` is
      lost, the PoP re-dials and reads the HTTP status: `401` for a
      credential that is not current (re-pair), `403` for a revoked or
      expired PoP.

    `hello`'s named host is the name the PoP already holds a certificate
    for, so home can tell a fresh install from a restart.

    *Built:* a `/pop/*` refusal carries a `Gawk-Pop-Reason` header, one of
    `unknown`, `used`, `pairing-expired`, `repair`, `revoked`, `expired`,
    `limited` or `proto`; a used and an expired string are both `410`, and
    the header tells them apart. A pairing dial is checked before the
    upgrade and redeemed after it, so a failed upgrade never burns the
    string; if the redeem loses a CAS race after the upgrade, the session
    is closed with code `410`. A linked PoP sends `?pop=<name>` on its dials
    so home checks one `Pop`, and a credential that misses the informer
    cache is re-read from the API before a `401`. `/pop/*` has its own
    failure limiter: 20 failures a minute per source address, then `429`.
    `config`'s home app origin is the first `https://` entry of home's
    `-allowed-origins`, so no new knob was added for it.

12. **One holder.** The link-home pod records itself in `status.lease`
    (holder, pod address, generation, renewedAt). These are the R42
    room-lease semantics: generation CAS, renewal every 5 s, and a
    **force-take on a fresh link**, because the PoP connecting is the ground
    truth (the resume-token argument in docs/22 D8). A pod that sees its
    generation superseded closes its stale link. On drain the holder sends
    `4002`, the PoP relinks at once and lands wherever the load balancer
    sends it. Single-pod home: no lease. PoPs come from `-pops-file`, the
    docs/42 file-source shape, with static credential hashes and a generated
    `uid` per entry (item 42); pairing tokens
    and the named tier need the Kubernetes path.

    *Built:* a superseded link (another pod force-took the lease, or the
    PoP relinked to the same pod) closes with code `0`, and the PoP relinks
    on anything that is not `4006`. After a `bye`, or a `401`/`403`/`426`,
    the PoP retries every 30 s, so a mistaken revoke that is undone heals
    by itself.

13. **The PoP learns both public addresses.** The link reveals one family.
    When the PoP has the other family too, it opens a short-lived
    `CONNECT /pop/addr` over it, so the home fleet records both (§6.12 matches
    on them).

### 6.4 One namespace

14. **A PoP broadcast's ID is reserved at home before the broadcaster sees
    it.** A local `/publish` mint on a linked PoP sends `reserve{id}`. The
    link-home pod creates the broadcast's Lease (`gawk-bc-<id>`, the docs/22
    registry) with **no holder** and the **label** `gawk/pop=<name>`, beside
    the registry's own `gawk/lease=broadcast` (`cluster.go` selects Leases by
    label and keeps per-broadcast data in annotations; a selector cannot
    match an annotation).
    Creation is the atomic reservation. Two changes at home keep a home
    mint off it. Today `Claim` (`cluster.go:223`) takes any Lease with no
    holder (`cluster.go:244-246`), so a home `/publish` mint that drew a
    reserved ID would silently become its origin (`server.go:878-889`).
    So `Claim` refuses a Lease labelled `gawk/pop` to every claimant except
    that PoP's `/pop/publish` (item 18), with or without `force`. And a home
    mint that meets a reservation draws another ID, where a collision today
    ends the mint with a `500`. The PoP announces the ID to its broadcaster
    after the reply, which costs one WAN round trip on the mint. Dormant
    reservations are renewed by the link-home pod, which takes them over on
    a link move by listing the label. A reservation counts against the
    PoP's `spec.limits.maxBroadcasts` (default 40, set in the wizard), not
    against the fleet's `-max-broadcasts`. That takes a third change:
    `createLease` enforces the fleet cap by counting every
    `gawk/lease=broadcast` Lease (`cluster.go:282-291`), so it counts with
    `gawk/lease=broadcast,!gawk/pop` instead. A mint past the PoP's limit
    gets `reserved{limit}` and stays **local-only**, as an unlinked
    collision does (item 15). The console says "Over this PoP's limit at
    gawk.ioio.fi: party-only". The PoP's own `-max-broadcasts` (50) still
    caps the party itself. In single-pod home mode a reservation is an
    in-memory entry. The registry's `IDReserved` hook (`hub.go:280-284`,
    set today only with `-rooms`) is also set with `-pops` and reports it,
    so a home mint never draws it, and it stays out of the registry's own
    broadcast count.

    *Built:* `claimMint` returns the ID and publisher it settled on. A home
    mint whose claim meets a reservation (`cluster.ErrReserved`) removes the
    refused hub outright and redraws; after 8 redraws it answers `503`. The
    hub's mint-time `IDReserved` already skips IDs the Lease cache shows as
    reserved, so a redraw is the backstop for a cache that has not synced.
    A token-bearing `/publish/{id}` on a PoP's broadcast answers `409`,
    refused from the Lease cache before any come-home `StopEdge`, and by the
    claim itself on both the resume and the takeover path.
    `cluster.Delete` is a no-op on a `gawk/pop` Lease, so a hub's grace
    expiry never ends a reservation; only `DeleteReservation` (release or
    revoke) or the janitor's sweep of reservations whose renewal went stale
    removes one. On a **single-pod home**, reservations are in memory and
    lost on a restart (the PoP re-reserves on relink), and a token resume
    is not checked against them: only cluster mode answers `409` there.

15. **Unlinked mints stay local.** With no link, the PoP mints locally and
    reserves on relink. A collision (a 1-in-31⁶ chance against the live set)
    leaves that broadcast **local-only** until it ends, and the console says
    so. The alternative, a PoP prefix in the ID, would change the ID format
    in every client. R42 D3 rejected the same idea for rooms.

16. **Codes typed on the PoP resolve upwards.** A local miss on a linked PoP
    tries the home fleet (§6.6). Rooms resolve the same way **when the home
    fleet runs rooms**: while linked, all room control is proxied to home
    (§6.10), so a dynamic room minted at the party lives at home and is
    joinable from anywhere. `config` says whether home runs `-rooms`
    (item 11). When it doesn't, rooms stay local to the PoP while linked,
    exactly as when unlinked, because home registers no room routes then
    (`server.go:325-334`). The wizard offers no wall in that case (item 33).
    Unlinked, rooms are always local to the PoP.

### 6.5 Uplink on demand

17. **Demand.** An outside viewer of party broadcast X lands on pod B. B's
    local miss (`handleSubscribe`, `server.go:1238`) resolves X's Lease and
    finds a reservation with no origin. B opens (or reuses)
    `CONNECT /internal/pop/{name}?psk=…&gen=…` to the link-home pod A, at
    the address in the `Pop` CR's `status.lease` (the cluster PSK and
    generation fence of `/internal/subscribe`), and sends `want{X}`. A
    forwards it on the link. The PoP **admits** X if it has a live local
    publisher and the budget allows (§6.8), replies, and dials the uplink
    leg (item 18). With no answer inside `-pop-demand-timeout` (5 s), the
    viewer waits in the ordinary "broadcaster away" state.

    **B upgrades the viewer before it asks.** Today a Lease with no holder
    is a miss. `EnsureEdge` answers it with `ErrNotFound`
    (`edge.go:209-211`), and `handleSubscribe` turns that into a `404`
    before the upgrade (`server.go:1233-1248`). That would leave no session
    to wait in or to close with `4012`, and the browser would see only
    "Opening handshake failed". So for a Lease labelled `gawk/pop`,
    `EnsureEdge` instead creates the local edge hub in the state an edge hub
    is in after losing its upstream: no prime, viewers attached and waiting.
    The viewer upgrades onto it, and B sends `want{X}`. When C claims the
    Lease, B's informer starts the edge pull into that hub. A refusal ends
    the hub's viewers with `4012`, after `SessionClosing` (item 28). With no
    answer, or while the PoP's broadcaster is away, they wait as for any
    absent broadcaster, for the relay's grace period (5 min by default),
    and then get `4000`. Viewers on other pods do the same, and A forwards
    one `want` per broadcast however many pods ask.

    *Built:* on a `gawk/pop` Lease with no holder, `EnsureEdge` creates a
    waiting edge hub (an edge publisher claim with no prime) and then fires
    the demand hook. The pull re-resolves on each `OnOriginClaimed` kick, or
    every second, until a holder appears. A viewerless waiting hub lingers
    out like any edge hub; deleting the reservation ends waiting viewers
    with `4000`; a hook error is the viewer's answer and leaves no hub. A
    Lease that returns to a reservation (a `4013` linger, a drain release)
    puts the pull back into waiting, and the demand loop asks again every
    `-pop-demand-timeout` while viewers wait on a reservation no leg has
    claimed.

18. **The uplink leg**, `CONNECT /pop/publish/{id}`, lands on any pod C. C
    checks the credential and that X is reserved for *this* PoP. That check
    stands in for the resume token as the proof of ownership; an ordinary
    `/publish/{id}` never claims a reservation. C then claims X's Lease as
    origin (`cluster.go:223`), and B's informer sees the origin
    and edge-pulls from C as it would for any broadcast. On the PoP side the
    leg is a publisher fed from the local hub. It starts with the hub's
    **join-prime** (cached keyframe with embedded config, and the clock
    mapping, rewritten per §6.7), then forwards keyframe streams
    byte-identical and every other datagram type (audio, `TimeSync`,
    `ClockMapping`, `ViewerCount`, `DecoderConfig`) as a datagram: the
    docs/22 D9 edge rules, pointed the other way. The first outside viewer
    starts from the prime, not from the next GOP.

    **Deltas ride R73's carrier (OD15).** `/pop/publish` is a publish
    session, so C advertises `CapUplinkCarriers` on it as on any other
    (`server.go:1024`). The PoP then writes each GOP's deltas and parity as
    carrier records, as a native broadcaster in *Reliable* mode does
    (docs/75 D1), and C reads them with R73's reader (`server.go:1131`,
    `hub/uplink.go:59`), so nothing at home is new. A loss on the venue's
    uplink is retransmitted instead of lost for every outside viewer, and a
    burst or a short outage delays a GOP instead of cutting it short
    (docs/75 F2). Without the bit, or with `-pop-leg-delivery=datagram`, the
    leg forwards deltas and parity as datagrams, verbatim.

    **The writer follows R73's sender rules** (docs/75 D2, D3):
    - at most two carriers live, so C's three slots
      (`hub.MaxUplinkCarriers`) never refuse a GOP;
    - a superseded carrier is finished at rotation and reset 150 ms later;
    - **no reset within a GOP**: a slow write waits, and only the next
      keyframe ends it;
    - a write still blocked on an older GOP's carrier is abandoned when the
      next keyframe fans out (its write deadline set to now), and that
      carrier is reset at once (F10);
    - nothing queued for an older GOP is written once a newer keyframe has
      gone, and a GOP superseded before its carrier opened never gets one.

    The hub's reliable-delivery drain (`drainReliable`, `hub.go:3029`)
    already writes per-GOP carriers from a hub, but it breaks three of
    those rules, each right for a viewer. It only closes a superseded
    carrier (`retireCarrier`, `hub.go:3355`). It gives every record a
    500 ms deadline (`CarrierWriteTimeout`) and on a timeout resets the
    carrier and drops the rest of its GOP (`writeCarrier`), so a congested
    line cuts GOPs short. And it notices a rotation only between writes. It
    also puts audio on a carrier of its own (docs/20), and every other
    type it diverts nowhere: `ViewerCount`, which the count pump fans to
    every subscriber, internal legs included, at least every 5 s
    (`hub.go:1442-1445`), and a live `DecoderConfig` go onto the video
    carrier as records. R73's reader takes only `VideoChunk` and
    `ParityChunk` records and ends the carrier on anything else
    (`hub/uplink.go:117-134`), so home's count would cut the leg's GOP
    every few seconds. So the legs get that drain with R73's rules, and
    with **only `VideoChunk` and `ParityChunk` on the carrier**: every
    other type goes as a datagram. This is an option; viewers keep today's
    drain. The downlink uses the same writer (item 23).

    **Timers.** Both legs dial with the link's QUIC timers (idle 15 s,
    keepalive 5 s, item 10), so an outage shorter than 15 s does not end a
    leg. The uplink's effective idle timeout is the lower of the PoP's and
    home's (`-max-idle-timeout`, 30 s by default).

    Three differences from the engine come from Go's stack. quic-go gives
    no signal that a finished stream was fully acknowledged (the engine
    waits on wtransport's `finish()`, which gives one), so the 150 ms reset
    is a plain timer. webtransport-go v0.13's streams do not expose
    quic-go's `SetPriority`, so the keyframe stream cannot be put ahead of
    the carrier (D3); PP8 measures whether a GOP's first frames pay for
    that. And quic-go ships Cubic only, with no BBR (D5), so the leg runs
    Cubic. Table O's Cubic rows (§3) are the case for OD15, and PP8 repeats
    them with this sender.

    *Built:* the writer is `hub.SubscribeLeg` (`internal/hub/leg.go`). A
    leg is an internal subscriber: exempt from the caps, outside the local
    viewer count, served every parity symbol, with no audio lane. With
    `LegOptions.Carrier` its records have no write deadline; the superseded
    carrier is finished at rotation and reset `LegResetDelay` (150 ms)
    later, and the oldest is reset first if a third would open. Slow
    keyframe streams on a leg are logged and counted but never evict it.
    Without `Carrier` the leg is byte-identical to `SubscribeInternal`. The
    edge pump (`classifyUniStream`, `transport/edge_streams.go`) reads each
    upstream uni stream's prologue: a carrier gives its keyframe slot back
    and goes to `Publisher.IngestUplinkCarrier`, and a refused or failed
    stream is reset with `CancelRead`. **PP8 measured this sender** (§11.2):
    datagrams were never ahead of the carrier by more than the run-to-run
    spread in any cell, so `-pop-leg-delivery` stays `carrier`. Whether the
    missing keyframe priority costs a GOP's first frames was not measured
    separately.

19. **The leg is open exactly while the broadcast is wanted, live and
    admitted.** If the party broadcaster goes away, the PoP closes the leg,
    C enters the ordinary grace and outsiders see the "broadcaster away"
    state. When the broadcaster returns, the PoP redials, the new leg
    force-takes the Lease, and the docs/22 origin-move path re-homes the
    edges. A leg that dies mid-stream is the same case.

20. **Release.** When C's hub for X has had no downstream (local viewers or
    edge sessions) for `-pop-uplink-linger` (default 30 s), C closes the leg
    with **`4013 PopNoDemand`** and returns the Lease to a reservation
    (holder cleared, generation kept, as on drain). The PoP keeps X local and
    waits for the next `want`.

21. **No hairpin for guests about to switch.** A party guest opening a home
    link for a party broadcast first reaches home. Their client says on its
    `CONNECT` that it will follow an offer (`lr=1`, §6.12). For a session
    that matches a PoP's public address and carries `lr=1`, B holds the demand
    for up to `-pop-offer-defer` (default 2 s). A guest who leaves for the PoP
    within that window costs the venue nothing. A guest who stays (an isolated
    Wi-Fi client, a denied permission) is served after the delay. The
    deferral needs address matching and clients that send `lr=1`, so it is
    built with them, in PP13.

    *Built:* `lr=1` gates only the deferral. An offer goes to every
    matching session whether or not it sent `lr=1`, so a client that will
    not follow one can still show the chip. Sessions without `lr=1`, or
    without a match, are never held.

22. **Time to first frame** for the first outside viewer is the miss, two
    in-cluster hops to A, one WAN trip down the link, a fresh QUIC + HTTP/3 +
    CONNECT handshake up (about 2 WAN RTTs), a Lease write, an informer
    event and an in-cluster edge pull. The target is **≤ 1.5 s at 40 ms WAN
    RTT**, measured in PP8, not assumed. Later viewers join as for any home
    broadcast. Pooling the legs on the link's QUIC connection would save the
    handshake. It is deferred until PP8's numbers say it matters (§13),
    because it trades per-leg congestion control for one shared controller
    on a constrained uplink, and that also needs measuring. *Not measured
    yet*: no lab cell for it has been written or run (§11.1).

### 6.6 Downlink on demand

23. A party viewer of home broadcast Y misses locally on the PoP. If linked,
    the PoP opens **`CONNECT /pop/subscribe/{id}`** to the home fleet's
    public endpoint. Any pod serves it, origin or edge, exactly as it serves
    a viewer: this is not `/internal/subscribe`, so docs/22's
    depth-2-by-construction guard is untouched and a PoP is a third tier
    below it. The PoP's hub is an ordinary edge hub (`EnsureEdge`, with a
    dialer that targets the public endpoint and presents the credential).
    That gives it the edge rules for free: join-prime on attach, prime
    invalidation on upstream loss and a 15 s linger. A `404` from home is a
    `404` to the viewer. One leg per broadcast, whatever the number of party
    viewers.

    **The leg is a carrier too (OD16).** The PoP asks for it with
    `?delivery=carrier`, and home then writes `/pop/subscribe` with the
    uplink leg's writer (item 18): keyframes on their own streams, deltas
    and parity on per-GOP carriers under R73's sender rules, every other
    datagram type as a datagram. Without the parameter home serves an
    edge's datagrams, as before R73. The carrier is opt-in so that home,
    which deploys itself, can keep serving a PoP that updates late, should
    the reader ever change; `-pop-leg-delivery=datagram` just leaves the
    parameter off. It is not R19's `?delivery=reliable`: R73's reader
    refuses its audio carrier, and the drain's close-only retire and
    in-GOP resets break the rules above (§13).

    On the PoP, the edge pump hands a `0x0A` stream to
    `Publisher.IngestUplinkCarrier` and any other stream to the keyframe
    reader, as a publish session does (`server.go:1131`). As there, a
    carrier gives back its keyframe slot once classified
    (`server.go:1162`); otherwise two live carriers and the keyframe
    streams fill the pump's four slots. A stream refused for want of a slot
    is reset (`CancelRead`, as `server.go:1143` does), not dropped unread
    as the pump does today (`edge.go:566-571`), because an unread stream
    keeps holding the session's flow-control credit. The PoP never opts
    parity down: its own viewers' leg B needs the symbols (docs/75 D1).

    The dialer is not the edge dialer's in-cluster setup. It uses the link's
    timers (item 18) rather than `edgeIdleTimeout` / `edgeKeepAlive`
    (4 s / 1 s, `edge.go:45-46`), which would end the leg, and invalidate
    the PoP's prime, on any outage over 4 s. It sizes its receive windows
    with R73's function (`uplinkReceiveWindows`, `transport/uplink.go`),
    or a 1440p GOP blocks on flow control (docs/75 D10, table H). The
    function's 10 ms RTT term is kept on purpose. At 50 Mbps the 2 MB
    stream window is 320 ms of the stream at its ceiling: it holds a 1440p
    GOP's deltas (~1.8 MB), and auto-tuning grows it from there. Home's
    listener applies the same windows to the uplink leg. PP9 checks this
    under a 40 ms RTT.

    *Built:* home's answers on `/pop/subscribe` are `404` (not here, or the
    PoP's own reservation), `426` (proto), `401`/`403`/`429` (credential,
    failure limiter) and `429` with `Gawk-Reason: downlink-limit` (the
    `maxDownlinkStreams` cap). At the PoP, `404` and `429` reach the party
    viewer, `503` retries, and a home that predates the leg (`501`) reads as
    `404`. No session notices or close notice are sent on `/pop/subscribe`.
    An edge hub's only internal subscribers are PoP downlink legs, and they
    count as demand: linger-out counts every subscriber, and the upward
    `ViewerCount` includes the legs' reported party viewers.

24. **Downlink budget**, optional, enforced at the PoP. Separately, the
    home fleet caps a PoP's concurrent downlink legs
    (`spec.limits.maxDownlinkStreams`, default 20) so a compromised PoP
    cannot drain the fleet's egress. The cap counts distinct broadcasts,
    so a second leg for one broadcast (a re-attach racing its
    predecessor's end, possibly on another pod) fits.

    **The cap is fleet-wide, not per pod.** The PoP's link-home pod
    holds the one count. Any other pod serving `/pop/subscribe/{id}` takes
    a slot there before it upgrades, over `/internal/pop/{name}?leg={id}`,
    behind item 17's PSK and generation fence:
    - `404`: the pod asked does not hold the link;
    - `409`: an old generation;
    - `429` `downlink-limit`: over the cap.

    The slot *is* that internal session. Ending the leg frees it, and a pod
    that dies without closing loses its slots at the edge idle timeout
    (4 s).

    The cap fails closed. No link, an unreachable link home, or a lease
    still stale after 2 s each refuse the leg with the cap's `429`.

    When the link moves, a live leg takes its slot again at the new link
    home within 15 s, or ends with `4003` and re-dials into the cap. A
    single-pod home counts locally.

    Verified by `internal/popsrv/downlink_cap_test.go`:
    - the 21st distinct broadcast is refused across three pods;
    - a slot is freed when its leg ends;
    - a crashed holder's slots expire;
    - the cap fails closed with no link;
    - slots follow the link.

### 6.7 Counts and clocks across the WAN

25. **Viewer counts** stay "real viewers, counted once" (docs/23).
    *Downlink*: the PoP reports its local count up the leg with
    `ViewerCount`. Home accepts it on `/pop/subscribe` exactly as on
    `/internal/subscribe` (docs/23 D6's "only from a relay peer", with the
    credential as the peer proof), so home's `G` includes party viewers.
    *Uplink*: origin C's `G` (home viewers) arrives at the PoP down the leg
    as it would to any publisher. The PoP shows its broadcaster
    `local + G_home`, and its local viewers the same total.

26. **Clocks.** The PoP runs the edge's TimeSync estimator (`edge.go:57`)
    over each leg and rewrites the 10-byte `ClockMapping` per hop: forward on
    a downlink leg (as edges do), backward on an uplink leg (broadcaster →
    PoP mapping composed with PoP → home). No shared clock (docs/22 D12).
    The error grows by the leg's path asymmetry, a few milliseconds on a
    typical WAN, small against capture→render.

### 6.8 The budget

27. **Admission, not shaping.** The PoP admits a `want` when the forwarded
    stream count is below `maxStreams` and the measured 10-second ingress
    rate of the forwarded broadcasts plus the candidate's stays at or below
    `maxMbps`. The ceiling comes from the `Pop` spec (OD13: 25 Mbps,
    3 streams). The host may lower it in the console and can never raise it.
    The current value travels to home in `stats` so `gawk-admin` shows the
    effective budget. First come, first served; a pinned "featured" stream
    is deferred (§13). The budget counts stream rate, not wire bytes: the
    carrier's retransmissions (OD15) ride on top, as many as the venue's
    line loses. The leg's writer also charges the relay's pod-wide
    `-max-bandwidth` bucket, as every subscriber drain does. That flag caps
    the host's NIC as a whole and is off (`0`) in the profile; the venue's
    line is limited by this budget alone.

28. **Refusal is visible and temporary.** A refused `want` returns to B,
    which closes the waiting viewer with **`4012 PopUplinkFull`**, sent
    in-band first as `SessionClosing` (docs/59). The code joins
    `noticedCloseCode`, or Chrome would never read it. It is **not
    terminal**: the viewer shows "This stream is at a LAN party with limited
    internet. Outside viewing is full right now; we'll keep trying." and
    retries every 20 s. The `4012` numbering assumes R43 keeps 4008–4011
    (docs/45).

    *Built:* `4012` is the first non-terminal code in the noticed set. The
    web viewer retries it on a fixed 20 s cadence that does not spend the
    reconnect budget, and the native viewers do the same with the shared
    copy.

### 6.9 Moderation reach

29. **Kills and ID bans reach the party** (OD7). Relay pods already watch
    `Ban` CRs (`moderationsrc/k8s.go`). The link-home pod forwards every ID
    ban, kill cooldowns included, that names one of its PoP's reservations,
    as `ban{id, until}`. The PoP ends the broadcast with `4006` (its viewers
    and its publisher see docs/59's moderator copy) and refuses reclaims of
    that ID until the ban ends. Party broadcasts appear in `gawk-admin`'s
    Broadcasts view from their reservations, forwarded or not, with the PoP
    badge (§7.2), so they can be killed before anyone outside watches.

    *Built:* lifting a forwarded ban at home (an unban, or a cooldown ended
    early) reaches the PoP on the next sweep as a `ban` whose `until` is
    already past; the PoP drops its copy, and keeps a ban the host set
    itself. A ban lifted while the link was down stays at the PoP until it
    ends or the relay restarts: the new link re-sends only what still holds.

30. **PoP traffic is exempt from IP bans and the per-IP limiter.** The
    credential is the PoP's identity. An IP ban on a venue's public address,
    whether placed against a guest who reached home directly or by accident,
    must not cut every party stream. Revoking the PoP is the control for the
    PoP itself. For a party broadcast, `gawk-admin` shows the **LAN address**
    the PoP reports for the publisher, on the admin-only surface where
    docs/42 allows publisher addresses; events never carry it (§8).
    Bans scoped to one device at a PoP are deferred (§13).

31. **Revocation** (`spec.revoked`, CR deletion or `spec.notAfter` passing)
    closes the link with `bye{revoked}` (or `bye{expired}`) and `4006`. It deletes the PoP's reservations, which
    ends outside copies fleet-wide through the informers, stops offers,
    deletes the `DNSEndpoint` and stops renewing the named certificate. The
    PoP keeps running as a standalone relay for the people in the room: it
    is the host's machine. Its named certificate dies within six days on the
    `shortlived` profile (item 38). The console shows "Unpaired: revoked by
    `gawk.ioio.fi`".

32. **Screening covers what home serves** (OD14). A forwarded party
    broadcast has an ordinary origin hub at home with a cached keyframe, so
    R40's sampler sees it with no change. While linked, the PoP sets the R40
    screening notice in its own `RelayIdentity` (`0x11`) whenever home's is
    set, so party broadcasters see the same disclosure as anyone streaming to
    home: anything they stream may be screened when it is carried outside.

    *Built (items 29–32):* the link-home pod forwards bans on the PoP's
    reservations **and on every ID the PoP has named on the link**
    (reserve, state, wall, stats), so a party-only broadcast past
    `limits.maxBroadcasts` is reached too. Forwarding is idempotent per
    link: a ban-source resync re-fires every record but sends nothing new,
    and a fresh link gets each active ban once more. The PoP keeps a
    forwarded ban until it ends, even after unlinking or a revoke, re-adds
    it if the host's own `-moderation-source` reload drops it, and never
    shortens a longer local ban. The `/pop/*` routes never call the per-IP
    limiter or the IP-ban check (tests pin it). Reservations are released
    on a revoke or CR deletion, and for a `spec.notAfter` that passes with
    no event, by a sweep every lease renewal. A linked PoP mirrors home's
    screening notice through `transport.Server.SetScreeningNotice`; the
    override only ever adds the notice, so a host's own
    `-screening-notice` stays on, and an unlinked or revoked PoP drops the
    mirrored bit.

### 6.10 The party wall

33. **A static room at home, created with the PoP.** When the operator
    enables the wall (the home fleet must run `-rooms`), `gawk-admin`
    creates a `Room` CR (R42 D4) with `spec.pop: <name>` and the wall code
    (default: the PoP name). It also sets two caps:
    - `spec.maxBroadcasts`, the existing R42 override, copied from the
      Pop's `limits.maxBroadcasts`. The fleet's `-max-room-broadcasts`
      default of 4 would refuse the fifth party broadcast
      (`roomsrv/registry.go:937-943`).
    - An optional `spec.maxParticipants`. The R42 default of 50 is small for
      a wall, and this override is a new optional field in the public
      `rooms` package, so it needs a `gawk-admin` commit.

    **A room with `spec.pop` accepts vouched attaches only** (item 34). An
    ordinary `Attach` is refused as a wrong attach secret would be, whoever
    sends it. Without that rule the wall would be a static room with no
    attach secret, which under R42 D8 means anyone may attach
    (`rooms/rooms.go:158-159`). Its code is shown to every outside viewer
    and defaults to the guessable PoP name, so any outsider could fill its
    slots with their own home broadcasts. In every other respect the wall is
    an ordinary static room: home pod, proxying, adoption, and `4007` on
    deletion.

    *Built:* `gawk-admin` creates the wall with the Pop:
    `spec.maxBroadcasts` = min(`limits.maxBroadcasts`, 64), the optional
    `spec.maxParticipants`, no attach secret, and an ownerReference to the
    Pop, so deleting the PoP deletes its wall. A taken wall code fails the
    create (`409 wall_code_taken`) and the Pop is removed again. Every
    successful edit of the Pop writes its `limits.maxBroadcasts` (as the
    wall's `spec.maxBroadcasts`) and its display name onto the wall room,
    so a raised or lowered cap and a rename reach the room; an edit still
    does not add, remove or rename the wall's code, and a revoke leaves the
    room in place. `spec.maxParticipants` overrides
    `-max-room-participants` exactly as `spec.maxBroadcasts` overrides
    `-max-room-broadcasts` (0 = the fleet default), and the `-rooms-file`
    entry gained the same field.

34. **Every party broadcast joins it, with an opt-out** (OD11). At publish
    time the PoP attaches the broadcast through its link: a **vouched
    attach**, accepted by the room's home because it arrived on the PoP's
    authenticated link for an ID that PoP reserved. The label is the
    broadcaster's nickname when the client sends one (the R42/R71 name), else
    "Stream 1, 2, …". No roster entry is created. The PoP tells the
    broadcaster with `LocalRelayInfo` (`0x18`): "On the party wall · Hide".
    *Hide* sends an R42 `Detach` for its own broadcast through the PoP; *Show*
    sends `Attach`. The PoP checks the broadcaster's local resume token and
    forwards the command vouched. A remembered Hide travels as `wall=hidden`
    on the next `/publish`, so a hidden broadcaster never flashes onto the
    wall. An explicit attach to another room moves the broadcast off the
    wall, because R42 D1 allows one room per broadcast and the broadcaster's
    own choice wins.

    *Built:* a vouched attach or detach reaches the wall's home pod on a
    new `POST /internal/roomvouch/{code}`, PSK-gated like `/internal/room`
    and registered only with `-pops`, rather than a `?vouched=1` on
    `/internal/room`. Hide/Show is intercepted in `transport/room_link.go`
    once the broadcaster's resume token proves the broadcast; an `Attach`
    with a valid token proves the ID for the rest of that guest session. A
    home with `-rooms` off has no wall, and the PoP's rooms stay local.
    The broadcaster's choice moves the broadcast off home's wall either
    way: a room minted at the PoP (the PoP's registry reports it, and the
    PoP Hides it), or an `Attach` to a room at home through the PoP. The
    PoP's resume token never travels home: the PoP zeroes it on an `Attach`
    it verified, and home accepts the link's vouch for a broadcast reserved
    for that PoP (`?vouch=` on `/internal/room` when the room's home pod is
    another, never taken from a public join).

35. **Grid capped by the budget** (OD10). Each tile is an ordinary
    per-broadcast `/subscribe`. For an outside participant that is an
    on-demand pull, so the first tiles take the budget's slots. A tile past
    the budget gets `4012`, draws "Party-only right now" with the label, and
    retries every 20 s, joining when a slot frees. This needs **no change to
    the room protocol**. A per-attachment flag was considered, but the room
    parsers reject reserved bits and any `RoomHello` protocol other than 1
    (`wire/room.go:607`), so it would need a coordinated protocol bump across
    every client and an older PoP. It is deferred with that cost stated (§13).
    For party guests, the same tiles resolve on the PoP and cost no uplink.

36. **Party guests in the wall.** Their room control session goes to the
    PoP. The PoP proxies it to home on a bidirectional stream of its link,
    and home routes it to the room's home pod as R42 proxies any non-home
    join. Their media stays local. With no link, the PoP serves a read-only
    local wall: its own broadcasts as tiles, no roster. On relink,
    attachments are resynced from the PoP's list.

    *Built:* on the link's room stream the PoP writes a `RoomOpen` record,
    then the guest's raw room records. Home answers with a u16 status (200
    = joined, otherwise the join route's HTTP status), then raw room
    records, and ends with a close record (a zero length prefix, a u32
    close code, a u8 reason length and the reason). The PoP turns that into
    the guest's session close, so `4007` stays terminal and a lost home
    reads as `4002`.

### 6.11 Certificates: pinned and named

37. **Pinned, always** (§6.1 item 3). Offers and the console carry the
    hashes. This tier needs no DNS and no CA and works with no internet. It
    serves Chrome, Firefox and the native apps; for Safari, it waits on PP0.

38. **Named, when the home fleet is set up for it** (OD5, OD9).
    `gawk-admin` is configured with a zone (`-pop-dns-zone`, e.g.
    `pop.gawk.ioio.fi`) and a cert-manager issuer (`-pop-cert-issuer`). For
    each PoP with `spec.namedAddress.enabled`:
    - The host name is **`p-<random6>.<zone>`**, a random label that
      `gawk-admin` chooses once and stores in `status.tls.host`, and **never
      the PoP's name**. Certificate
      Transparency logs publish every issued name, and the PoP name is the
      default wall code, which R42 treats as a joinable secret.
    - The PoP learns its assigned name from `config` (item 11). It generates
      a P-256 key in its state directory and sends a CSR for that name on
      the link, and the link-home pod writes it to `status.tls.csr`. The
      `gawk-admin` leader validates it (exactly one SAN, the assigned name;
      P-256) and creates a cert-manager `CertificateRequest` against the
      issuer. cert-manager's ACME issuer solves DNS-01 as it does today.
    - **One issuance per CSR.** On `Ready`, `gawk-admin` writes the chain
      to `status.tls.certificate` (certificates are public) and the CSR's
      SHA-256 to `status.tls.issuedForCSR`. It issues only for a CSR whose
      hash is not already there. The link-home pod sends a chain down only
      when `issuedForCSR` matches the current `tls.csr`. So a renewal CSR
      written over the old one is never issued twice and never answered
      with the previous chain. **The private key never leaves the PoP.**
    - The issuer should use Let's Encrypt's **`shortlived` (6-day)
      profile**. The PoP renews at half-life while linked, which stays inside
      the 5-duplicates-per-week limit, and a revoked PoP's certificate dies
      on its own. A 90-day issuer works too, but then a revoke leaves a valid
      certificate for that name on the host for up to its lifetime;
      `gawk-admin` says so when it detects one.
    - `gawk-admin` maintains an external-dns **`DNSEndpoint`**
      (`externaldns.k8s.io/v1alpha1`): one A record, the PoP's primary LAN
      address, TTL 60. external-dns must run with `--source=crd`. Without the
      zone or issuer configured, PoPs are pinned-only and the wizard says
      why.

    - *Built:* `-pop-cert-issuer` takes `Issuer/<name>` or
      `ClusterIssuer/<name>` and must be set together with
      `-pop-dns-zone`. The link-home pod stores a PoP's CSR only if its
      single SAN is `status.tls.host`, and sends a chain only when
      `issuedForCSR` equals sha256 of the current CSR **and** the CSR the
      PoP asked for on that link. `gawk-admin` refuses a chain that does not
      answer the CSR, retries a failed request after an hour, and deletes
      the `DNSEndpoint` on revoke, end, opt-out or Pop deletion. The PoP
      keeps its P-256 key in the state directory and renews at half-life
      with a fresh key. The named certificate is chosen by SNI on the
      shared QUIC listener, so `https://<named>:4433` is one origin for the
      page and the relay.

39. **What the name buys.** The PoP serves the embedded web app over HTTPS
    on TCP 4433 at `https://p-<label>.<zone>:4433`, one origin for the page
    and the relay. Its generated `config.js` names the PoP as `relayUrl`, the
    home app as the share origin, and the home fleet's terms URL. A guest who
    scans the venue QR code loads the page **from a local address**, so
    Chrome does not prompt (M3). Safari has a publicly trusted certificate,
    and native apps verify normally.

40. **DNS rebinding self-check.** The PoP resolves its own name through
    the system resolver and through a public resolver. If the public answer
    names its LAN address and the local one doesn't, the venue's resolver is
    filtering. The console then says "This network's DNS hides the relay's
    name. Guests will use `gawk.ioio.fi` instead", and the venue QR code
    switches to the home link, where the offer still carries the pinned
    candidate.

    *Built:* the public resolver is 1.1.1.1 over plain UDP DNS. The QR
    moves to the named URL only when the venue's resolver returns the PoP's
    LAN address; a certificate alone never moves it.

### 6.12 Home suggests the local relay; clients switch by themselves

41. **Who gets an offer.** A home pod compares a client session's remote
    address with every linked PoP's public addresses: IPv4 exact, IPv6 on
    the /64. This needs **real client addresses at the relay**: under
    `externalTrafficPolicy: Cluster` with SNAT, home sees node addresses and
    no offer is ever sent. The homelab runs `Local` (docs/22 finding 10); the
    self-hosting doc states it, as it already does for IP bans
    (`docs/self-hosting.md` §9.4). A PoP with `spec.guests.autoSwitch: false`
    never offers.

42. **`LocalRelayOffer` (`0x19`)**, home → client, on a server-opened uni
    stream right after the upgrade on `/echo`, `/publish`, `/subscribe` and
    `/room` sessions (the 0x11/0x12 precedent: the browser API exposes no
    response headers). It carries:
    - an opaque PoP handle: the Pop CR's `metadata.uid` (in `-pops-file`,
      the entry's generated `uid`). It is random, derived from neither the
      name nor any key, and stays the same across pods, restarts and
      re-pairs; only recreating the PoP changes it. The stats key would not
      do: it defaults to per-process random, so the handle, and every
      opt-out remembered against it (item 43.6), would change on each
      restart and differ between pods;
    - the display name and a validity in seconds;
    - candidates: the named URL when issued, and the IP-literal URL with its
      pinned hashes.

    Offers also reach clients whose address merely shares a CGNAT exit with
    a venue. That reveals a LAN address, two hashes and a display name to
    strangers, which is minor and stated in §8. They cannot reach the address,
    so nothing happens.

    *Built:* every home pod offers, since a guest's session can land on any
    of them. The candidates are, in order, `https://<named host>:4433`,
    only once a certificate has been issued for that host and only while
    `namedAddress` is on (publicly trusted, so no hashes; always
    `status.tls.host`, never the host the PoP names in its hello), then
    `https://<LAN IP>:4433` with the PoP's current and next hashes (port
    4433 when the link reports none; both candidates take the port a relay
    off 4433 reports). An
    offer is valid for one hour. Metrics: `gawk_pop_offers_total` and
    `gawk_pop_offer_defers_total`, with no PoP label, because a PoP's name
    is its default wall code; `/internal/admin/pops` rows carry
    `offersSent`, counted per pod.

43. **The switch** (OD3), in every client:
    1. **Only on a fresh offer.** A remembered PoP never triggers a switch on
       its own, so a laptop back home never dials a dead LAN address.
       Remembered state is consent only.
    2. **Probe the candidates in parallel** over `/echo`, first success
       wins. Native apps pin with `with_server_certificate_hashes`.
    3. **Viewer and room participant**: reconnect to the PoP with the same
       code through the R17 `4002` path. That is one freeze of at most about
       1 s, once. **Broadcaster**: switch only **before going live**. The web
       broadcaster, the desktop Ready page (R62's probed server status) and
       the iOS start screen probe home's `/echo` on open, so the offer arrives
       in time. A live broadcast is never moved; it would need cross-fleet
       resume tokens. A broadcaster already live on home sees "Assembly 2026
       has a local relay. Your next broadcast will use it."
    4. **Browser permission.** Query `navigator.permissions` for
       `local-network`, falling back to `local-network-access` (M2):
       - `granted`: switch silently.
       - `prompt`: probe anyway, which shows Chrome's prompt, while an in-app
         toast explains it (§7.3).
       - `denied`: stay on home and show a quiet note with how to allow it.
       - A browser whose Permissions API doesn't know the name: just probe.
    5. **Failure**: stay on home, mark the PoP unreachable for this tab or
       app session, no retry loop.
    6. **Opt-out**: the chip's "Use gawk.ioio.fi instead" is remembered per
       PoP handle; offers from that PoP are then ignored until the user picks
       it again.
    7. Clients that will follow an offer send `lr=1` on every `CONNECT` to
       home (item 21).

    *Built, web:* the permission is queried under `local-network`, then
    `local-network-access`; a browser that knows neither name just probes.
    The candidates' `/echo` are probed in parallel and the first that
    answers wins. A failed PoP is not retried in that tab, and "Use
    <home> instead", remembered per PoP handle, also stops `lr=1` for that
    PoP. Notices on the `/echo` probe each ride their own uni stream, and
    home's `0x19` can arrive after the `0x10` identity, so the probe keeps
    reading after the identity.

    *Built, native:* the desktop Ready page and the iOS start screen and
    player probe home with `lr=1`, switch to a fresh offer's first pinned
    candidate before going live, remember "Use … instead" per PoP handle,
    and leave a PoP that fails or refuses for the app session. On iOS a
    held PoP applies only to the home relay that offered it, so picking
    another server dials that server; the desktop keeps its own policy.
    **Not built:** the iOS room participant switch (the room client sends
    no `lr=1` on `/room` and ignores `0x18`/`0x19`, assumed rather than
    tested).

44. **Apple platforms.** The iOS app gains `NSLocalNetworkUsageDescription`,
    which it lacks today. Per TN3179, the first dial after an offer may be
    denied before the user answers, so it retries once after the prompt.
    While backgrounded and unanswered it never dials. The desktop app on
    macOS gets one OS prompt. Chrome on macOS needs the OS permission too.
    When a probe fails there, the note names it.

    *Built:* iOS retries the switch once after the local-network prompt and
    never dials while not active (`LocalRelayPrompt`, unit tests). The
    prompt on a device is PP14's pending device pass.

45. **`LocalRelayInfo` (`0x18`)**, PoP → client on the same session kinds.
    It carries the display name, whether the PoP is linked, the home app
    origin, the wall code and, on publish sessions, the on-wall state. It is
    allocated in **PP5 (phase A)** with its whole layout, mirrors and
    vectors, and a standalone PoP sends it with only the name, `linked`
    clear. PP7 fills in the link and home origin, and PP11 the
    wall fields, so no later phase changes the layout. It serves three
    purposes:
    - **Share links on a linked PoP are home links**, with no `?relay=`.
      They work for a guest at the party (who switches) and for anyone
      outside (who pulls on demand).
    - **On a standalone PoP**, share links carry `?relay=<ip>&certHash=`.
    - The chip and the wall line show the PoP's name.

    *Built:* the name is home's display name for the PoP while linked, else
    `-server-name`, else the host name. 0x18 is sent on `/echo`,
    `/publish`, a viewer's `/subscribe` and `/room`, never on stripe legs,
    `/internal/*` or `/pop/*`, and home sends no offer to a session whose
    Origin is `gawk-relay://pop`. It is re-sent when the on-wall state
    changes. `/room` control sessions now receive server-opened uni streams,
    so room clients must ignore uni stream types they do not know. iOS does
    not yet build a standalone PoP's `?relay=&certHash=` share link from
    0x18; its live view keeps the home link, which is right for a linked
    PoP.

### 6.13 Failure modes

| Event | What happens |
|---|---|
| The venue's internet drops | Every LAN session continues. Outside viewers see "broadcaster away" until the reservation's grace (5 min) ends, then `4000`. The wall is unreachable from outside and read-only at the party. Open browser tabs keep working, and native apps can start new sessions. **A guest who has not loaded the web app cannot load it**: `gawk.ioio.fi` is unreachable and so is public DNS for the named host. Stated, not solved (offline first load is deferred, §13). On relink the PoP re-reserves its IDs (same codes) and outside links work again. |
| The PoP host restarts | Persisted keys and certificate: broadcasters auto-resume (R17 W2) on the same codes, and the link re-establishes. |
| The link-home pod drains | `4002` on the link, an immediate relink, and the new holder force-takes and adopts the reservations. Forwarded legs are unaffected: they are separate sessions on other pods. |
| An uplink leg dies | The origin's ordinary grace; the PoP redials while still wanted; Lease force-take; docs/22 origin move. |
| A burst, or an outage shorter than the legs' 15 s idle timeout, on the venue's line | On the carrier a GOP arrives late instead of cut short. A carrier older than the current GOP is reset, so nothing stale is resent when the line recovers (docs/75 D3, F10). The same holds on a downlink leg. With `-pop-leg-delivery=datagram`, parity repairs random loss and an outage costs every viewer on the far side the rest of the GOP. |
| Venue Wi-Fi isolates clients | Probes fail and guests stay on home. The console shows "offers sent N · guests here M"; a large gap is the hint. |
| Venue DNS filters private answers | The named host fails at the venue. The self-check (item 40) moves the QR code to the home link, and offers still carry the pinned candidate. |
| Permission denied in the browser | The guest stays on home with a note; streams cost the venue's internet as today. |
| Budget full | `4012` to the next outside viewer; wall tiles show "Party-only right now". |
| PoP and home on different link majors | `426`; the console shows "Update gawk-relay"; the PoP runs standalone meanwhile. |
| Home runs with `-rooms` off | `config` says so; the PoP keeps rooms local while linked, and there is no wall. Party rooms work, but outsiders can't join them (item 16). |
| A credential leaks | The holder can publish under that PoP's reservations, reserve up to its `maxBroadcasts` and pull up to `maxDownlinkStreams`. It learns no fleet secret. Re-pair revokes it at once. |

## 7. UX

The surfaces below fix what each one must say and do. Layout and visual
detail are PP1's design pass.

### 7.1 The console (`http://127.0.0.1:4480`)

- **Header**: "gawk relay · Assembly 2026" and one state:
  - **Linked to gawk.ioio.fi**
  - **Standalone**
  - **Offline: the party keeps working, outside viewers are paused**
  - **Waiting to pair**
  - **Revoked**
- **Join from this network**: a large QR code and its URL, with a
  *Show on a projector* full-screen mode.
  - Named tier: `https://p-7k2m9x.pop.gawk.ioio.fi:4433`, "no app needed".
  - Linked, pinned only: "Open gawk.ioio.fi. Devices here switch to this
    relay by themselves."
  - Standalone: the `?relay=…&certHash=…` link, and the `gawk://server`
    form for the apps.
- **Live here**: one row per broadcast:
  - the label;
  - viewers here · outside;
  - outside state: *Forwarded · 6.1 Mbps*, *Not requested*, or
    *Party-only (budget full)*;
  - wall state: *On the wall* or *Hidden*.
- **Party internet**: "Up 14 / 25 Mbps · 2 of 3 streams" with a slider that
  only lowers it, and "Down 9 Mbps · 1 stream".
- **Health**, each line with its fix:
  - Windows Firewall: blocked on this network's *Public* profile, with an
    **Allow on this network** button. It elevates through UAC and adds a
    rule scoped to this program, UDP and TCP 4433, the local subnet, and the
    Private and Public profiles.
  - Certificate: pinned, rotates in *n* days; named, renews automatically.
  - The DNS self-check.
  - The LAN address, with an override.
  - Version, and "update available" from `releases/gawk-relay/latest.json`.
- **Pairing**: paste a pairing string, or *Run standalone*.

*Built* (the layout and copy are a provisional baseline until PP1's
sign-off):
- The console refuses a non-loopback `-console-addr` at startup. Every
  request needs a loopback peer **and** a loopback `Host` header
  (`localhost` or a loopback literal; `*.localhost` is refused), the
  DNS-rebinding defence. State changes are same-origin JSON POSTs, checked
  on `Origin`, `Sec-Fetch-Site` and the JSON content type.
- On first start the header reads *Waiting to pair* until the host pairs
  or picks *Run standalone*; that choice survives restarts
  (`<state-dir>/standalone`).
- The LAN override and lowered budgets last until restart;
  `-lan-address` and `-uplink-budget`/`-downlink-budget` are the persistent
  forms.
- "Update available" reads `releases/gawk-relay/latest.json` every 6 h
  (dev builds never check); nothing is downloaded.
- The console opens in the default browser once, on the first start with
  a terminal (`<state-dir>/console-opened`). Linux and BSD without
  `DISPLAY`/`WAYLAND_DISPLAY` never open one.
- The relay draws the QR code itself: no network, no JS library.
- The Windows Firewall helper is one elevated
  `cmd.exe /d /c "<delete & add UDP & add TCP>"`, so the host sees one UAC
  prompt; it refuses relative paths and paths containing `%`, quotes or
  newlines. It has never run on Windows (PP15).

### 7.2 `gawk-admin`

- **Nav: PoPs.** A card per PoP:
  - state: *Linked 3 h*, *Offline since 21:04*, *Waiting for pairing*,
    *Revoked* or *Ended*;
  - the dates and the version;
  - "7 live · 2 forwarded", outside viewers, and an uplink gauge against
    the budget;
  - guests here, and offers sent against guests connected.
- **New PoP**, in four steps:
  1. Name, display name, optional end date.
  2. Party internet: uplink Mbps and streams; downlink; how many party
     broadcasts home will carry (`limits.maxBroadcasts`, default 40).
  3. Guests: auto-switch on or off, the wall on or off with its code, and
     the named address (offered only when configured, otherwise "needs
     `-pop-dns-zone` and `-pop-cert-issuer`").
  4. **Pair**: the pairing string once, with copy buttons, download links
     per OS, a `docker run --network host -e GAWK_PAIR=…` line, and when
     the string expires.
- **PoP detail**:
  - Header actions: Edit, Re-pair, Revoke.
  - Live broadcasts: label, publisher LAN address, forwarded or not,
    outside viewers, wall, and Watch / Kill / Ban (the R39/R70 shared
    sequence).
  - The uplink over time and the link history.
  - The PoP's health lines.
  - Addresses: public v4/v6, LAN, named host, certificate expiry.
- **Broadcasts view**: party broadcasts carry the PoP's badge and *party-only*
  or *forwarded*, and can be killed either way (item 29).
- **Audit**: create, edit, Re-pair, Revoke and delete are moderation rows,
  `pop.created`, `pop.updated`, `pop.repaired`, `pop.revoked` and
  `pop.deleted`, in the events feed and delivered to webhooks as
  `fi.ioio.gawk.pop.*` (the `events` contract). They carry the actor, the
  PoP's name (the `subject`) and its display name, and nothing else about
  it: no address, pairing string or credential. They are not the PoP
  lifecycle bus events (linked, unlinked), which stay deferred (§13).
- **Not a breach of docs/42 D10.** That decision makes the portal read-only
  for *relay configuration*, which belongs to the chart. A `Pop` is a
  runtime resource like a `Room` or a `Ban`, and its CR is its source of
  truth. The fleet's own flags (`-pops`, the zone, the issuer) stay in
  the charts.

*Built* (the copy is provisional until PP1's sign-off):
- `gawk-admin` serves `/api/v1/pops` (operator only, `-pops`). Create
  returns the pairing string once as `{pop, pairingString, expiresAt}`, and
  only `spec.pairing.tokenSHA256` is stored. Re-pair writes `spec.pairing`
  alone, Revoke sets `spec.revoked`, and Delete is bound to the UID it read.
- Card states: `waiting-for-pairing`, `linked`, `offline`, `revoked`,
  `ended`. A card says `waiting-for-pairing` whenever
  `status.credential.pairingTokenSHA256` ≠ `spec.pairing.tokenSHA256`,
  including right after a Re-pair while `status.link.connected` is still
  true: that credential is already refused, and the link-home pod closes
  the link with `bye{repair}`.
- `/me` gains `features.pops` and `features.popNamedAddress`. The portal
  reads `/internal/admin/pops` as a fourth relay scrape; a relay predating
  R74 answers `404` and stays reachable.
- Broadcasts rows carry a `pop` object `{name, forwarded, label,
  publisherLanAddr, localViewers, outsideViewers}`. A party-only row has an
  empty key, no pods and no `publisherRemoteIp`, and its ban state reflects
  ID bans only (PoP traffic is exempt from IP bans). It can be killed like
  any other row. `publisherLanAddr` is on the admin API only, in no event.
- Create, edit, re-pair, revoke and delete are audited as
  `pop.created`, `pop.updated`, `pop.repaired`, `pop.revoked` and
  `pop.deleted` (`fi.ioio.gawk.pop.*` on the webhook channel, with schemas
  and golden vectors in `gawk-server/events`). Their data is the actor,
  the PoP's name and its display name: no address, pairing string or
  credential.
- **Not built:** Kill/Ban on the PoP detail page (it links to the
  Broadcasts view, which offers Kill on party rows); and per-OS download
  links in the wizard (it links to the releases page filtered to
  `gawk-server/`).

### 7.3 Guests (web, desktop, iOS)

- **Chip** on the session screens, the R37 in-session indicator: "●
  Assembly 2026 · local relay", with a menu item **Use gawk.ioio.fi
  instead**.
- **First switch in a browser that will prompt**: a toast beside the
  prompt: "Assembly 2026 has a local relay. Your browser will ask to let
  gawk.ioio.fi connect to devices on this network. Allow it to stream and
  watch over the LAN."
- **Denied**: "Your browser blocked the local relay. Streams still work
  through gawk.ioio.fi, over the party's internet." with a link on how to
  allow it.
- **Broadcaster on a PoP**: "Live on Assembly 2026's local relay", the
  viewer total, and "On the party wall · Hide".
- **Outside viewer refused**: the `4012` copy in item 28. **Wall tile past the
  budget**: "Party-only right now" with the label.

## 8. Security and privacy

- **No fleet secret reaches a PoP.** No resume-token key, internal PSK,
  stats key, stateless-reset key or telemetry key. A PoP holds its own keys
  and one credential that is scoped to its reservations and its legs. A
  party machine is not part of the fleet.
- **`/pop/*` routes** require the credential, and are exempt from the per-IP
  limiter and IP bans (item 30). An unauthenticated or wrong-credential
  `/pop/*` request is refused before upgrade and logged at debug level.
  Repeated failures are rate-limited per source address, like the other
  routes' limiter.
- **The PoP's public address is a publisher address** for every party
  broadcast. It lives in the `Pop` CR status (the Kubernetes API is internal,
  as for `Ban` CRs) and on the admin surface. **No event carries it**, and
  neither does the LAN address a PoP reports. Broadcast lifecycle events for
  party broadcasts gain an additive `pop` field (the PoP name) in the public
  `events` package, so a `gawk-admin` commit lands with it.
- **Offers expose a LAN address, two hashes and a display name** to any
  client sharing the venue's public address, including strangers behind the
  same CGNAT exit. Minor, and stated.
- **CT logs** reveal named hosts, hence random labels (item 38). The public
  A record reveals a private address for that label, which is harmless.
- **Raw broadcast IDs** stay off public surfaces: the console is
  loopback-only, and `/statusz` and metrics key PoP broadcasts by the PoP's
  own HMAC, as on any relay.
- **Moderation authority** runs one way: home can end party broadcasts, and
  a PoP can never act on a home broadcast.

## 9. Wire, routes, knobs

**Wire** (allocated in `gawk-server/wire/wire.go`, mirrored in `wire.ts`
and `crates/wire` with byte-identical golden vectors, in the same PR):

| Allocation | Direction | Meaning |
|---|---|---|
| `0x18 LocalRelayInfo` | PoP → client, server uni stream after upgrade | display name; a strict flags byte (0) and a tolerant `state` byte (bit0 `linked`, bit1 `onWall`, unknown bits ignored); home app origin (`https://host[:port]`, or `http://localhost`/`http://127.0.0.1[:port]` in development; empty when unlinked); wall code; trailing bytes tolerated (the 0x11 extension seam) |
| `0x19 LocalRelayOffer` | home → client, server uni stream after upgrade | PoP handle, display name, validity, candidates (URL + 0–2 SHA-256 hashes); trailing bytes tolerated |
| `4012 PopUplinkFull` | home → viewer | not terminal; retry every 20 s; in-band `SessionClosing` first |
| `4013 PopNoDemand` | home → PoP uplink leg | not terminal; the PoP waits for the next `want` |

Layouts as built:
- `0x18`: `01 18 00 state | u8 nameLen (0-64) name | u8 originLen (0-128)
  origin | u8 wallLen (0-32) code | rest ignored`, at least 7 bytes.
- `0x19`: `01 19 00 | u8 handleLen (1-64, printable ASCII) handle | u8
  nameLen (0-64) name | u32 BE validitySec (>=1) | u8 candCount (1-4) |
  per candidate: u16 BE urlLen (1-256) + https URL (no userinfo, query or
  fragment), u8 hashCount (0-2) + 32-byte SHA-256 each | rest ignored`.

The origin and URL rules are hand checks, byte for byte the same in Go, TS
and Rust, with no platform URL parser. The golden vectors live in
`gawk-server/wire/localrelay_test.go` and are restated in `wire.test.ts`
and `crates/wire/tests/golden.rs`. Code: `wire/localrelay.go`, `wire.go`
(4012/4013), `internal/transport/session_notices.go` (`SetLocalRelay` on a
PoP, `SetOfferer` at home), `closenotice.go` (4012 in `noticedCloseCode`).

The link records are JSON in `internal/poplink`, not wire types (item 10).
The room protocol does not change (item 35). The legs need no allocation:
they use R73's carrier (`TypeReliableCarrier`, `0x0A`) and its
`CapUplinkCarriers` bit as they are (items 18, 23).

**Routes.** On the home fleet, only with `-pops`:
- `CONNECT /pop/link`
- `CONNECT /pop/addr`
- `CONNECT /pop/publish/{id}`
- `CONNECT /pop/subscribe/{id}` (`?delivery=carrier` for the carrier leg;
  without it, an edge's datagrams)
- `CONNECT /internal/pop/{name}` (pod → link-home pod)
- `POST /internal/roomvouch/{code}` (a PoP's wall attach or detach,
  forwarded to the wall's home pod; PSK-gated like `/internal/room`)

On the home fleet's **ops listener**, beside the existing `/internal/admin/*`
routes and under the same admin token or OIDC gate, **never routed
publicly** (CLAUDE.md):
- `GET /internal/admin/pops`: per PoP, the link state, live and forwarded
  counts, outside viewers, budget use and offers sent, from the pod that
  holds the link. `gawk-admin` merges it into the relay scrape
  (`internal/relayscan`) for the §7.2 cards. It may carry the PoP's public
  and LAN addresses, which the admin routes are allowed to carry and events
  never are (§8).

On a PoP, the ordinary relay routes, plus the TCP UI listener in the named
tier and the loopback console.

**Knobs**, each a flag, a `GAWK_*` env and a Helm value, plumbed through
`registryOptions` and the admin's config:

| Component | Knob | Default |
|---|---|---|
| `gawk-server` | `-pops` | off |
| | `-pops-file` | empty (single-pod source) |
| | `-pop-uplink-linger` | 30s |
| | `-pop-offer-defer` | 2s |
| | `-pop-demand-timeout` | 5s (a want with no answer leaves the viewer in "broadcaster away") |
| `gawk-relay` | `-pair` | empty |
| | `-state-dir` | per OS (§6.1) |
| | `-console-addr` | 127.0.0.1:4480 |
| | `-ui-addr` | :4433 (TCP, named tier only) |
| | `-lan-address` | auto |
| | `-pop-leg-delivery` | `carrier` (`datagram` is a support lever, not in the console; home's `-uplink-carriers=false` also turns the uplink leg's carrier off) |
| | `-uplink-budget` | the ceiling (lower only) |
| | `-downlink-budget` | the ceiling (lower only) |
| `gawk-admin` | `-pops` | off |
| | `-pop-home-endpoint` | empty; required with `-pops` (the `host:port` a pairing string names, e.g. `api.gawk.ioio.fi:4433`) |
| | `-pop-dns-zone` | empty (pinned only) |
| | `-pop-cert-issuer` | empty (`Issuer/<name>` or `ClusterIssuer/<name>`; set together with `-pop-dns-zone`) |
| | `-pop-dns-ttl` | 60s |
| | `-pop-pairing-ttl` | 72h |

**The `Pop` CR** (`gawk.ioio.fi/v1alpha1`, namespaced, status
subresource; the Go types live in a public `gawk-server/pops` package that
`gawk-admin` imports, the `rooms`/`moderation` precedent):

```yaml
apiVersion: gawk.ioio.fi/v1alpha1
kind: Pop
metadata:
  name: assembly-2026
spec:                                  # written by gawk-admin (or kubectl)
  displayName: "Assembly 2026"
  notAfter: 2026-11-09T00:00:00Z       # optional: links refused after
  revoked: false
  pairing:
    tokenSHA256: "…"                    # the string is shown once, never stored
    issuedAt: 2026-10-21T18:00:00Z
    expiresAt: 2026-10-24T18:00:00Z
  uplink:   {maxMbps: 25, maxStreams: 3}
  downlink: {maxMbps: 0,  maxStreams: 0}   # 0 = unlimited at the PoP
  limits:   {maxBroadcasts: 40, maxDownlinkStreams: 20}   # defaults; enforced at home
  guests:   {autoSwitch: true}
  wall:     {roomCode: assembly-2026}  # absent = no wall
  namedAddress: {enabled: true}
status:                                # two writers on disjoint paths, see below
  credential: {sha256: "…", pairedAt: 2026-10-21T18:04:11Z, pairingTokenSHA256: "…"}
  lease: {holder: gawk-server-7c9f, addr: 10.42.0.17:4433, generation: 4, renewedAt: …}
  link:
    connected: true
    since: …
    version: 0.44.0
    proto: 1
    publicAddrs: ["203.0.113.7", "2001:db8:1:2::/64"]
    lanAddr: 192.168.1.20
    pinnedHashes: ["…", "…"]
  tls: {host: p-7k2m9x.pop.gawk.ioio.fi, csr: "…", certificate: "…", issuedForCSR: "<sha256>", notAfter: …}
```

The two status writers patch disjoint paths, so their merge patches never
collide:
- the link-home pod owns `credential`, `lease`, `link` and `tls.csr`;
- `gawk-admin` owns `tls.host`, `tls.certificate`, `tls.issuedForCSR` and
  `tls.notAfter`.

Credential invalidation goes through `spec` (item 9), so `gawk-admin` never
touches the relays' fields. Live counts are not in status (API write
amplification). `gawk-admin` reads them from a new
`/internal/admin/pops` relay route, alongside the existing relay scrape.
RBAC: relays get `pops` get/list/watch and `pops/status` get/update (their
writes are `resourceVersion`-fenced `UpdateStatus` calls). `gawk-admin`
gets `pops` CRUD **and `pops/status` patch**, because with a status
subresource the API server ignores status written through the main
resource. It also gets `certificaterequests` create/get and `dnsendpoints`
CRUD.

*Built:* in the relay chart the `Pop` CRD (`templates/crd-pop.yaml`)
renders with `pops.enabled`, and the `-pops` Role (`pops` get/list/watch,
`pops/status` get/update) and the `GAWK_POP*` envs with `pops.enabled` and
`config.clusterMode`; the chart refuses `pops.enabled` without
`clusterMode`. 0.36.0 shipped `pops/status` get/patch, so every status
write was forbidden and every pairing ended in a bare `503`;
`popsrc/rbac_test.go` now checks the rule against the writer's calls,
because the fakes never check RBAC and the kind tier runs no `gawk-relay`. `-pops-file` is for non-Kubernetes deployments and is not a
chart value, as `-rooms-file` is not. The CRD keeps the name a DNS-1123
label with a CEL rule, because the name is the `gawk/pop` label value. In
the gawk-admin chart, `pops.enabled` requires `pops.homeEndpoint` and
renders `GAWK_ADMIN_POPS`, `GAWK_ADMIN_POP_HOME_ENDPOINT`,
`GAWK_ADMIN_POP_PAIRING_TTL`, `GAWK_ADMIN_POP_DNS_ZONE`,
`GAWK_ADMIN_POP_CERT_ISSUER` and `GAWK_ADMIN_POP_DNS_TTL`. Every `-pop*`
knob of the relay appears in `/internal/admin/config` as a public value. A
PoP dials with Origin `gawk-relay://pop`, which the relay accepts only on
`/pop/*` paths, so no deployment adds it to `allowedOrigins`. The
operator-facing setup is [`docs/self-hosting.md`](self-hosting.md) §14.

## 10. Where it plugs in

- `gawk-server/cmd/gawk-server/main.go:644` `certSource` (zero-flag refusal
  stays), `:526` `registryOptions`; `internal/config/config.go:347-476`
  (flags).
- `internal/tlsutil/devcert.go:42` / `:174` (generation, `LoadOrGenerate`),
  `reload.go:57` (the per-handshake reloader the rotation rides).
- `internal/transport/server.go:314-334` (routes), `:1198` / `:1238`
  (`handleSubscribe`'s miss → `EnsureEdge`), `:393` (`CheckOrigin` and the
  internal origin), `telemetry.go:115` (`sendRelayIdentity`, the model for
  0x18/0x19), `closenotice.go` (`noticedCloseCode`).
- R73's pieces the legs reuse: `server.go:1024` (`sendRelayCapabilities`:
  `CapUplinkCarriers` on publish sessions), `:1131-1193` (a publisher's
  streams: carrier or keyframe, then `ingestUplinkCarrier`),
  `internal/hub/uplink.go:59` (`IngestUplinkCarrier`, the reader at both
  ends), `internal/hub/hub.go:3029` / `:3355` (`drainReliable` and
  `retireCarrier`, the writer that gains R73's rules as an option),
  `internal/transport/uplink.go` (the receive windows the PoP's dialer
  copies).
- `internal/transport/edge.go:201` `EnsureEdge`, `:722-752` the dialer
  and `:45-46` its in-cluster timers (which the PoP's must not inherit),
  `:57-122` TimeSync + `rewriteClockMapping`, `:561-571` the pump's stream
  accept and its four slots (where the downlink's carriers go), `:605-620`
  the count report.
- `internal/cluster/cluster.go:223` `Claim` and `:244-246` its "no holder
  is takeable" rule (which must refuse `gawk/pop` Leases to anyone but
  their PoP), `:282-291` `createLease`'s fleet-cap count (which must skip
  them), `:568` `Lookup`, `:720` the janitor (dormant reservations must
  survive it); `internal/transport/server.go:878-889` the home mint's
  claim (which redraws on a reservation); `internal/hub/hub.go:280-284`
  the `IDReserved` hook, `:945` `EdgePublish`, `:1406-1455` the count
  fan-out; `edge.go:209-211` `EnsureEdge`'s "no holder is a miss" (which
  a reservation is not, item 17).
- `internal/transport/roomcluster.go:75,124,186` (room proxying),
  `room.go:129,205`; `wire/room.go:607` (the strict `RoomHello` check that
  item 35 avoids).
- `internal/moderationsrc/k8s.go:79-136` (the Ban informer item 29 reads).
- `gawk-admin/internal/kube/rooms.go:209` (`CreateStatic`, the shape for
  `Pop` + wall room), `kube/reconcile.go` and `kube/leader.go:104` (the
  leader that runs the cert/DNS reconciler), `internal/api/api.go:363-444`
  (routes), `ui/src/App.tsx:25` (nav), `internal/relayscan` (the relay
  scrape that gains `/internal/admin/pops`, §9).
- `gawk-app/src/routing.ts:47-66` (`parseQuery`: `certHash`),
  `features/servers/relayOverride.ts`, `state/transportStore.ts:48-59,286-345`
  (resolution precedence; the offer becomes a session override that ranks
  like a link's), `features/servers/probe.ts:90-131`. *As built:*
  `state/transportStore.ts` resolves a `certHashes` list, with
  `certHashHex` (the first hash) kept on disk for older builds, and a
  link's pins outrank a saved entry's for the same relay;
  `transport/connection.ts` `webTransportInit` pins every hash, from a
  comma-separated `certHashHex` and/or `certHashes`; `probe.ts` finds
  `RelayIdentity` by its type byte, not by stream order.
- R74's own code, as built: `cmd/gawk-relay` (features register through
  `registerPart` in their own files, `console.go` and `pop.go`),
  `internal/serve` (the shared relay body), `internal/relayconsole`,
  `internal/relaystate`, `internal/lanaddr`, `internal/tlsutil/rolling.go`,
  `internal/poplink`, `internal/popclient` (the PoP side),
  `internal/popsrv` (the home side: pairing, the link, reservations,
  offers, the wall), the public `pops` package (CR types, pairing, CSR and
  chain rules) and `gawk-admin`'s `PopReconciler`.
- `gawk-broadcast-desktop/crates/engine/src/transport.rs:213-232`
  (`connect_once`: add pinned hashes beside `with_native_certs`),
  `crates/engine/src/link.rs` (the `gawk://server` form), `config.rs`
  (per-server hashes).
- `gawk-ios/app/project.yml` (no `NSLocalNetworkUsageDescription` today).
- `.github/workflows/release-please.yml:100-118` (server ships an
  amd64-only image and a chart, no binaries), `broadcast-desktop.yml:740-836`
  (the notarization the macOS `gawk-relay` reuses),
  `.github/actions/publish-release-manifest`.

## 11. Chunks and acceptance criteria

Phase A ships a useful standalone relay with no home-fleet change. Phase B
needs A. Phase C needs B's link (PP7) and offers nothing without it.

| Chunk | Scope | Acceptance |
|---|---|---|
| **PP0** — measure first | Safari 26.x (macOS, iOS) pinned dial to the relay by IP literal; headful Chrome: prompt copy, grant persistence across reload and restart, denied behaviour; Firefox 153+ today; Windows Firewall vs an unsigned `gawk-relay.exe` on a Public-profile network and the scoped rule; the macOS desktop app dialing a LAN relay; the measuring harness landed under `tools/` | A measured table appended to §3, each row with method and build. A "no" changes the decision it names before PP2: Safari fails pinning → Safari guests use the named tier only, and §6.12 says so; a Firewall rule that can't be scoped → §7.1's button changes |
| **PP1** — design pass | Canvases for the console, the admin PoP pages, the guest switch surfaces, the wall tile states | Owner sign-off; §7 updated to match, dated |
| **PP2** — the profile | `cmd/gawk-relay`, state directory, persisted keys, pinned certificate with rolling rotation, LAN address detection, §6.1 defaults; every knob a flag + env through `registryOptions` | Zero-flag start in CI on Linux; Windows and macOS builds and unit tests; a restart test resumes a broadcast with its resume token; a fake-clock rotation test handshakes with either pinned hash across the overlap; **after a `kill -9` and a restart, a connected client gets a stateless reset within about one RTT** instead of waiting out the idle timeout (the docs/22 W1 in-test UDP proxy); **`gawk-server` with zero flags still exits non-zero** (regression test) |
| **PP3** — the console | Loopback-only listener; join, live, party-internet, health and pairing sections; the Windows Firewall helper; `/app/` for the host | The console refuses non-loopback connections (test); QR payloads golden-tested per tier; the firewall helper's command construction unit-tested and run once on Windows (PP15) |
| **PP4** — packaging | Binaries for windows/amd64, macOS universal (signed + notarized), linux amd64/arm64; multi-arch image `ghcr.io/tuhis/gawk-relay`; release attach; `releases/gawk-relay/latest.json`; a downloads card on the site. Built from `gawk-server/` with its vendored quic-go: the image copies `third_party/quic-go/go.mod` before `go mod download`, and `go install …@version` is no distribution, because it refuses a module with a `replace` (`docs/gotchas.md`) | Release dry-run produces every artifact; `go version -m` on each binary shows quic-go replaced by `third_party/quic-go`; the manifest test passes; the arm64 image starts under emulation in CI |
| **PP5** — pinned links in clients | `gawk-app`: `certHash=` (private literal rule), landing-page "Save and use this server", share links on a standalone PoP; desktop + iOS: per-server hash pinning, the `gawk://server` form; **`LocalRelayInfo` (`0x18`) allocated with its full layout** (item 45), mirrors and vectors, sent by every `gawk-relay` | `0x18` vectors byte-identical in all three copies, and a standalone relay sends it on every session kind; routing vectors accept and reject per the rule; tier-1 e2e: headless Chrome with a CDP-granted `local-network` permission joins a relay at a non-loopback address by a hash link; Rust dial test against a pinned relay; link vectors |
| **PP6** — `Pop` resources | CRD + `gawk-server/pops` package + chart RBAC; `-pops-file`; `gawk-admin` `/api/v1/pops` CRUD + pairing string, OpenAPI, the PoPs list, wizard and detail | The chart renders the CRD gated on `pops.enabled`; the pairing string is shown once and only its hash is stored; `gawk-server/pops` reaches no quic-go (a `go list -deps` check in CI: `gawk-admin` builds it, and the `replace` does not cross modules); OpenAPI drift check green; UI tests; each successful create, edit, Re-pair, Revoke and delete records its `pop.*` row and a refused one records nothing, and no row or delivery carries the pairing string, the credential or a link address (`TestPopChangesAreAudited`, the notify contract and poison tests, the `events` vectors) |
| **PP7** — the link | `/pop/link`, pairing exchange, credential, status lease and force-take, `hello`/`config`/`stats`/`health`/`bye`, reconnect, `/pop/addr`; console pairing; `0x18`'s `linked` flag and home origin; `GET /internal/admin/pops` (link state; PP8, PP9 and PP13 add their counts) and the PoP cards reading it; local rooms when home runs `-rooms` off | In-process test (3 home pods, fake clientset with the docs/22 finding-11 reactors) + one PoP: pair; relink after a drain lands on another pod, which force-takes; used, expired and wrong tokens refused with legible statuses; a credential paired in time outlives `spec.pairing.expiresAt`; Re-pair refuses the old credential at once, also when two Re-pairs land in the same second; the console tells `bye{repair}` from `bye{revoked}`, and recovers the reason from `401`/`403` when the `bye` is lost; `-pops-file` credentials work with no pairing; a `proto` major mismatch → `426`; a paired PoP's `0x18` has `linked` set and the home origin, and an unpaired or revoked one has it clear; `/internal/admin/pops` answers on the ops listener behind the admin gate and is absent from the public mux (test); with home `-rooms` off, `/room/new` on a linked PoP mints a local room |
| **PP8** — reservations and uplink | Reserve-before-announce, unlinked mints, `/internal/pop`, `/pop/publish`, admission and `4012` (+ notice), linger and `4013`, counts both ways, the clock rewrite, broadcast events with `pop`; the leg writer (R73's sender rules, only video and parity chunks on the carrier, an option on the hub's reliable drain); the upgrade-first demand path (item 17); `Claim` refusing `gawk/pop` Leases to anyone but their PoP, the home mint redrawing on a reservation, and the fleet cap skipping them (item 14), the uplink carrier, `-pop-leg-delivery`, and a `tools/uplinklab` sender driven by the leg writer | In-process: an outside viewer on pod B receives a byte-identical prime and deltas; **zero bytes on the uplink without demand** (counter); linger closes with `4013` and the Lease is back to a reservation; the fourth stream is refused and Chrome reads `4012` (tier-1 e2e); a mint past `limits.maxBroadcasts` gets `reserved{limit}` and stays party-only; a home mint that draws a reserved ID redraws and never claims it, and a resume-token reclaim on `/publish/{id}` cannot force it either (fake clientset); 40 dormant reservations do not count against the fleet's `-max-broadcasts`; an outside viewer of a reservation is upgraded and waits, rather than getting a pre-upgrade `404`; the count is right on both sides; **time to first frame measured at 40 ms netem RTT and recorded here** (target ≤ 1.5 s); event schemas and vectors updated, no address field; against a pod advertising `CapUplinkCarriers`, deltas and parity leave only as carrier records, no more than two carriers are ever live, and a superseded one is reset 150 ms after it finished (fake clock); a `ViewerCount` or `DecoderConfig` arriving mid-GOP goes as a datagram and does not end the carrier; a 400 ms write stall within a GOP does not reset its carrier, and a write blocked across a rotation is abandoned when the new keyframe fans out and its carrier reset (fake clock); a 10 s outage (netem) does not end the leg; with home's `-uplink-carriers=false` or `-pop-leg-delivery=datagram` the leg is datagrams, byte-identical to before; **the PoP's sender measured in the uplink lab**, carrier against datagrams, on docs/75 table O's 30 and 15 Mbps cells beside a TCP upload and on table B's bursts and outages, three runs a cell, recorded here together with whether the missing keyframe priority costs a GOP's first frames. The carrier stays the default unless datagrams show the Balanced viewer more frames in one of those cells by more than the cell's run-to-run spread; if they do, datagrams become the default and item 18 says so |
| **PP9** — downlink | `/pop/subscribe`, the PoP edge dialer, upward count reports, the downlink budget, `maxDownlinkStreams` at home; the downlink carrier: `?delivery=carrier` and the leg writer on `/pop/subscribe`, R73's reader in the PoP's edge pump (a carrier frees its keyframe slot, a refused stream is reset), the link's timers and R73's receive windows on the PoP's dialer | Ten party viewers of one home broadcast → one leg (counter); `G` at home includes them; the leg cap refuses the 21st distinct broadcast; a 1440p home stream's GOPs cross without blocking on flow control under a 40 ms RTT (the in-process UDP proxy, as docs/75 table H); audio, `ViewerCount` and `DecoderConfig` reach the PoP as datagrams, never as carrier records, and a count mid-GOP does not end the carrier; without `?delivery=carrier` home serves datagrams (test); a stream the pump refuses is reset, not left unread (test); a 10 s outage does not end the leg, and the PoP's prime survives it; **the downlink measured in the uplink lab beside a saturating TCP download** (a game update's shape), carrier against datagrams, three runs a cell, recorded here |
| **PP10** — moderation reach | Ban forwarding and local enforcement, revoke, IP-ban and limiter exemption, the admin badge and party-only rows, the screening-notice relay | A kill in `gawk-admin` ends a party-only broadcast at the PoP with `4006` for its viewers and broadcaster; a banned ID cannot be reclaimed at the PoP; an IP ban on the PoP's public address does not cut a leg; **after a revoke**, the PoP's reservations are gone, an outside viewer of a party code gets `4000` (attached) or `404` (new dial), no offer is sent, the `DNSEndpoint` is deleted, and the PoP keeps serving the LAN as a standalone relay; the PoP's `0x11` carries the screening bit while home's does |
| **PP11** — the party wall | Wall room creation with the PoP, `spec.pop` / `spec.maxBroadcasts` / `spec.maxParticipants`, vouched-only attach and detach, Hide/Show and `wall=hidden`, `0x18`'s wall code and on-wall state, link-proxied room control, the local read-only wall | Extended `rooms-assert`: party broadcasts appear on the wall, **a fifth and later party broadcast included**; **an ordinary `Attach` to the wall (an outsider's home broadcast) is refused**; Hide removes and Show restores; a hidden-remembered broadcaster never appears; an outside participant's tile past the budget shows "Party-only right now" and joins when a slot frees; a protocol-1 client parses every snapshot; unlinked, party guests see the local wall; a broadcaster's `0x18` carries the wall code and an on-wall state that follows Hide, Show and `wall=hidden` |
| **PP12** — the named tier | CSR on the link, the `gawk-admin` reconciler (`CertificateRequest`, `DNSEndpoint`), random labels, the PoP's HTTPS UI and generated `config.js`, renewal at half-life, the rebinding self-check, QR selection | Reconciler unit tests against fake cert-manager and external-dns objects: a CSR with a wrong SAN is refused; the PoP's CSR names the host it got in `config`; a renewal CSR is issued exactly once (`issuedForCSR`), and the PoP never receives the previous chain for it; one staging issuance on the reference deployment; a phone on the LAN loads the PoP UI with no prompt (PP15) |
| **PP13** — offers at home | Address matching (v4 exact, v6 /64), `0x19` on the four session kinds, mirrors and vectors, the `lr=1` demand deferral (item 21, `-pop-offer-defer`), offer counts on `/internal/admin/pops`, metrics | Go tests: a matching client gets the offer, a non-matching one never does, `autoSwitch: false` suppresses it; **the handle is identical across pods and across a home restart with no `-stats-key` set**; a matching `lr=1` session's demand is held for at most `-pop-offer-defer`, and a non-matching or `lr`-less session's is not held; vectors byte-identical in all three copies |
| **PP14** — the switch | Web (permission query, parallel probes, reconnect, chip, opt-out, denied note), desktop (Ready page), iOS (broadcaster + player, usage string, first-deny retry) | Web unit tests for every permission state; tier-1 e2e with a CDP-granted permission: a viewer switches with one freeze ≤ 1 s, and with the permission denied stays on home; native tests for offer parsing and pinning; the Apple prompts on device (PP15) |
| **PP15** — the party pass | A real or staged LAN party against the reference deployment | §12's list, recorded here with dates and builds |

### 11.1 Status (2026-10-22)

Built on `feat/r74-pop-relays`. "Tested" means an automated test that ran
on the agent host or in CI. Nothing in the right-hand column has been run,
measured or passed.

| Chunk | Built and tested | Still open |
|---|---|---|
| **PP0** | Nothing: the `tools/` harness has not landed | Every measurement (owner hardware), §3 |
| **PP1** | The surfaces, built to §7's copy | Owner sign-off; console, admin and guest copy and layout are provisional |
| **PP2** | Zero-flag start, restart-resume and the kill-9 stateless reset (`cmd/gawk-relay` tests: in-process, and a subprocess SIGKILL with a fresh-key control); `gawk-server` with zero flags still exits non-zero; every target GOOS/GOARCH builds and vets with cgo off; the portable packages' unit tests run for Windows under Wine (`relay-windows`, DPAPI included) and natively on macOS (`relay-darwin`) | Windows host runtime: a `TerminateProcess` kill and the QUIC listener (Wine cannot open one) have never run on Windows itself; `lanaddr.Detect` on a real multi-NIC Windows or macOS host |
| **PP3** | Non-loopback refusal (test); QR payloads golden-tested per tier; the firewall helper's command construction; the CI zero-flag smoke probes `127.0.0.1:4480` | The firewall helper run once on Windows (PP15) |
| **PP4** | `ci.yml`'s `relay` job (windows/amd64, darwin amd64+arm64, linux amd64+arm64; `CGO_ENABLED=0`, `-trimpath`, `main.version` from the release manifest), `relay-macos` (one universal binary, Developer ID signed and notarized, never on pull requests), `relay-release` (attaches to the `gawk-server/vX.Y.Z` release and publishes `releases/gawk-relay/latest.json`); `ghcr.io/tuhis/gawk-relay` from `gawk-server/deploy/Dockerfile.relay` (repo-root context, no chart); `go version -m` checks on every binary and the image; the image's zero-flag start; `tools/releases` `test_relay_releases_under_the_server_tag` | Notarization, unexercised until the first tagged release; a real release producing every artifact, the manifest and the multi-arch GHCR push; the arm64 image start under QEMU runs in CI only; the site's downloads card is not built |
| **PP5** | Routing vectors; `e2e/pop-pinned.mjs` (headless Chromium 151.0.7922.34: with Local Network Access granted the view link decodes frames, without it the dial fails with `ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS`, and the landing save stores the hashes), wired into `ci.yml`; desktop and iOS-core pinning and the `gawk://server` link vectors; the Rust dial against a pinned relay (an in-test wtransport server, and the ignored real-relay suite CI runs); `0x18` vectors in all three copies | The iOS Swift wiring is unbuilt here; a device pass |
| **PP6** | The CRD gated on `pops.enabled`; the pairing string shown once with only its hash stored; the `go list -deps` check in CI; the OpenAPI drift check; UI tests | PP1 sign-off on the admin pages |
| **PP7** | Every row, in-process | — |
| **PP8** | Every in-process row; `e2e/pop-uplink-full.mjs` (the fourth outside stream reads `4012` in-band) passes on Chrome for Testing 151.0.7922.34 and runs in `ci.yml`; the uplink lab, §11.2 | **Time to first frame at 40 ms netem RTT: not measured** (no harness cell yet); the keyframe-priority cost was not measured separately |
| **PP9** | Every in-process row, and the leg cap across three pods (item 24) | **The downlink beside a saturating TCP download: not measured** (needs root and netem) |
| **PP10** | Every row, in-process (`popsrv/bans_test.go`, `transport/pop_moderation_test.go`, `serve/pops_moderation_test.go`, gawk-admin's `popbroadcasts_test.go` and `BroadcastsView.test.tsx`) | — |
| **PP11** | Vouched-only wall rooms, the vouched route, link-proxied room control, the local read-only wall with its resync, Hide/Show and `wall=hidden`, and `0x18`'s wall fields, in-process (`popsrv/wall_test.go`, `transport/room_link_test.go`), because the kind tier runs no `gawk-relay`; `rooms-assert` step 3b (a wall grants no attach, an attach secret gets `403`, an outsider's ordinary attach leaves it empty), passed in `e2e-cluster` on the real chart with 2 pods (run 39866366271, 2026-10-23, `6b733252`) | The wall's PoP-side rows in the kind tier, which runs no `gawk-relay`; "Party-only right now" checked end to end |
| **PP12** | Reconciler tests against fake cert-manager and external-dns objects; the CSR, renewal and chain rules; the HTTPS UI; the DNS self-check | **One staging issuance on the reference deployment**; a phone loads the UI with no prompt (PP15) |
| **PP13** | Every row, in Go tests | — |
| **PP14** | Web unit tests; `e2e/pop-switch.mjs` (headless Chromium 151.0.7922.34, 2026-10-22, on a scratch merge made before Wave 3 was complete: granted → one switch, longest gap between drawn frames 432 ms; denied → stays on home with the note); the engine's `localrelay` tests and `relay_integration`'s pinned switch against the real relay; iOS-core `localrelay` tests | `pop-switch.mjs` re-run on the final tree (it runs in `ci.yml`, and passed on the post-Wave-3 tree before the review fixes); Swift `LocalRelayTests` (first run: the `ios.yml` xcode job); the Apple prompts on device, Safari/iOS, Firefox and the headful Chrome prompt; the iOS room participant switch is not built |
| **PP15** | — | All of §12 |

No Swift was compiled or run on this host, and neither was the Apple-only
`broadcast.rs`; `ios.yml`'s xcode job is their first build.

**Where the build departs from the text above**, beyond the *Built* notes
in §6 and §7:
- **The PoP's downlink budget admits on measured ingress.** A new pull is
  admitted while the running pulls are below `maxStreams` and their
  measured 10 s ingress is below `maxMbps`. The candidate's own rate is
  unknown before it is pulled, so, unlike the uplink's item 27, it is not
  added in. A budget the host lowers in the console reaches this
  admission and the uplink's.
- **The home mint redraws** with the refused hub removed, and answers
  `503` after 8 draws (item 14).
- **A reservation always gets a waiting hub**, and the demand hook is a
  notification once it exists (item 17).

### 11.2 PP8 lab: the PoP's leg writer (2026-10-22)

Measured with `tools/uplinklab` on the agent host. The sender is
`gawk-uplinklab legsend`, the hub's `SubscribeLeg` writer, on quic-go's
Cubic. The lab binary is byte-identical (`cmp`) to a fresh `go build` of
`./cmd/gawk-uplinklab` at `ccb2f870` (r74/w3-uplink; `04f9c177` after it
changes no Go). Go's embedded VCS stamp reads `d0da1e64` for every build in
that worktree, which is wrong (`docs/gotchas.md`). Every cell's CPU steal
was at or below 0.3 %. Table B ran three runs a cell (seeds 7–9), three
runs at a time, 100 s each. Table O ran three runs a cell at 1080p and two
at 1440p. Only table O's 80 Mbps second runs were run after the last
commit; every table B cell and the other table O cells were reused from an
earlier, interrupted attempt on the same branch, from the same lab
directory. The leg writer exposes no byte or loss counter, so an alone cell's
wire rate comes from that run's `tc` class counters, less pings and IP/UDP
headers (within 1 % of quinn's `udp_tx_bytes` on existing quinn cells),
and the packets-lost column reads "–".

**Table B, the PoP leg on table B's cells** (docs/75 §11):

| RTT | Path | Delivery | Runs | Undecodable | Balanced: shown % (range) | Datagrams ahead by more than the spread? |
|---|---|---|---|---|---|---|
| 10 ms | bursts (GE, ≈1 %) | datagram | 3 | 22.9 % | 73.1 (70.6–75.0) |  |
| 10 ms | bursts (GE, ≈1 %) | carrier | 3 | 0.2 % | 83.2 (71.8–92.5) | no |
| 10 ms | radio away 20 ms/1.07 s | datagram | 3 | 19.7 % | 81.4 (78.7–83.5) |  |
| 10 ms | radio away 20 ms/1.07 s | carrier | 3 | 0.1 % | 89.5 (87.0–91.4) | no |
| 10 ms | radio away 80 ms/1.07 s | datagram | 3 | 21.1 % | 78.6 (77.1–79.7) |  |
| 10 ms | radio away 80 ms/1.07 s | carrier | 3 | 1.4 % | 94.0 (92.8–95.0) | no |
| 40 ms | bursts (GE, ≈1 %) | datagram | 3 | 78.4 % | 17.3 (14.8–21.1) |  |
| 40 ms | bursts (GE, ≈1 %) | carrier | 3 | 34.5 % | 55.4 (53.4–57.0) | no |
| 40 ms | radio away 20 ms/1.07 s | datagram | 3 | 27.1 % | 68.4 (62.4–74.2) |  |
| 40 ms | radio away 20 ms/1.07 s | carrier | 3 | 4.9 % | 85.6 (85.2–86.4) | no |
| 40 ms | radio away 80 ms/1.07 s | datagram | 3 | 50.7 % | 41.9 (37.4–50.1) |  |
| 40 ms | radio away 80 ms/1.07 s | carrier | 3 | 16.8 % | 75.9 (75.2–77.1) | no |
| 100 ms | bursts (GE, ≈1 %) | datagram | 3 | 96.7 % | 0.3 (0.1–0.5) |  |
| 100 ms | bursts (GE, ≈1 %) | carrier | 3 | 90.2 % | 8.6 (7.0–10.7) | no |
| 100 ms | radio away 20 ms/1.07 s | datagram | 3 | 96.7 % | 0.1 (0.0–0.2) |  |
| 100 ms | radio away 20 ms/1.07 s | carrier | 3 | 92.0 % | 4.1 (3.3–4.8) | no |
| 100 ms | radio away 80 ms/1.07 s | datagram | 3 | 96.6 % | 0.0 (0.0–0.0) |  |
| 100 ms | radio away 80 ms/1.07 s | carrier | 3 | 94.8 % | 0.7 (0.6–0.9) | no |

A clean 100 ms path is fine: 0 % undecodable, decode-ready p50 51 ms. The
lossy 100 ms collapse, in both modes, fits Cubic's loss-limited rate
(docs/75 §11); the PoP's quic-go has no BBR (§13).

**Table O, the PoP leg on a shared uplink** (`tools/uplinklab/fair`: one
uplink at 30, 15 or 80 Mbps with a 50 ms drop-tail queue, the downlink
unshaped; the broadcast for 80 s, a bulk TCP upload, kernel Cubic, from
20 s; mean, range in brackets).

O1, what the household's upload gets. *Of its fair share* is against its
max-min fair share; ping is the household's, while the upload runs.

| Uplink | Broadcast | TCP Mbps | Of its fair share | TCP retransmits | Ping p50 / p95 ms |
|---|---|---|---|---|---|
| 30 Mbps, 1080p (9.5 Mbps mean, 12 ceiling) | none | 28.6 (28.6–28.7) | – | 160 (154–163) | 60 (60–61) / 67 |
| | PoP leg: datagram · Cubic | 17.9 | 101 % (101–101) | 117 (105–133) | 44 / 65 |
| | PoP leg: carrier · Cubic | 17.9 (17.8–17.9) | 101 % (101–101) | 113 (104–124) | 43 (43–44) / 64 |
| 15 Mbps, 1080p | none | 14.4 (14.3–14.4) | – | 126 (118–133) | 68 (67–69) / 75 |
| | PoP leg: datagram · Cubic | 4.2 (4.1–4.4) | 59 % (58–61) | 167 (136–190) | 63 / 72 (71–72) |
| | PoP leg: carrier · Cubic | 4.8 (4.5–5.3) | 67 % (63–73) | 157 (133–172) | 65 (64–65) / 73 (72–74) |
| 80 Mbps, 1440p (38.9 Mbps mean, 50 ceiling) | none | 76.6 (76.5–76.6) | – | 94 (93–94) | 53 / 61 |
| | PoP leg: datagram · Cubic | 36.3 (35.4–37.1) | 95 % (92–97) | 114 (113–116) | 49 (46–51) / 61 |
| | PoP leg: carrier · Cubic | 37.0 (36.7–37.4) | 97 % (96–98) | 85 (72–98) | 52 (52–53) / 62 |

O2, what the broadcast gets, alone on the uplink → beside the upload.
Undecodable and decode-ready cover the upload (25–80 s), the viewer
columns its last 40 s. *Undecodable* is strict; the viewers skip one lost
delta per GOP (R29), so they can show more.

| Uplink | Broadcast | quinn: packets lost | Undecodable | Decode-ready p99 ms | Balanced: shown · stalled | Balanced: on-screen latency p50 ms | Lowest latency: stalled |
|---|---|---|---|---|---|---|---|
| 30 Mbps, 1080p (9.5 Mbps mean, 12 ceiling) | PoP leg: datagram · Cubic | – → – | 0.3 % (0.0–0.9) → 1.6 % (0.9–2.4) | 89 (89–90) → 399 (269–617) | 98.1 (98.0–98.2) · 1.8 (1.5–2.1) → 89.0 (86.7–92.4) · 12.1 (7.8–14.9) % | 67 → 195 (167–233) | 12.4 % (12.3–12.6) → 36.2 % (36.1–36.3) |
|  | PoP leg: carrier · Cubic | – → – | 0.0 % → 0.1 % (0.1–0.2) | 88 (88–89) → 252 (247–259) | 97.8 (97.3–98.2) · 1.8 (1.5–2.4) → 88.0 (85.0–90.2) · 12.1 (10.9–13.1) % | 67 → 145 (133–150) | 12.3 % (12.3–12.4) → 31.6 % (30.4–32.3) |
| 15 Mbps, 1080p | PoP leg: datagram · Cubic | – → – | 0.6 % (0.0–0.9) → 41.6 % (35.1–49.2) | 246 (245–246) → 1173 (1061–1268) | 88.0 (86.3–89.0) · 12.3 (12.1–12.4) → 38.3 (36.6–39.4) · 61.9 (60.2–63.7) % | 161 (150–167) → 356 (350–367) | 31.1 % (30.6–31.7) → 78.4 % (77.1–79.5) |
|  | PoP leg: carrier · Cubic | – → – | 0.2 % → 19.1 % (11.6–28.4) | 199 → 500 (497–502) | 93.3 (92.1–94.4) · 7.6 (6.3–8.8) → 65.3 (54.2–71.8) · 36.7 (29.7–47.8) % | 156 (150–167) → 350 | 27.8 % (27.3–28.3) → 63.7 % (58.8–68.9) |
| 80 Mbps, 1440p (38.9 Mbps mean, 50 ceiling) | PoP leg: datagram · Cubic | – → – | 0.0 % → 11.5 % (4.9–18.0) | 89 (88–89) → 397 (385–409) | 97.6 (97.2–98.1) · 2.5 (2.0–2.9) → 78.8 (68.4–89.2) · 21.5 (11.6–31.3) % | 67 → 350 (334–367) | 10.7 % (10.0–11.5) → 44.1 % (34.4–53.8) |
|  | PoP leg: carrier · Cubic | – → – | 0.0 % → 5.3 % (4.1–6.5) | 86 (84–89) → 283 (257–309) | 97.9 (97.7–98.1) · 2.0 (1.7–2.3) → 92.6 (91.9–93.2) · 8.3 (7.9–8.7) % | 75 (67–83) → 217 (200–233) | 10.4 % (9.8–11.0) → 24.9 % (23.7–26.2) |

**Verdict: `-pop-leg-delivery` stays `carrier`** (OD15, now on
measurement). In no table B cell are datagrams ahead of the carrier by
more than the run-to-run spread, and the carrier does as well or better in
every table O cell; by PP8's rule the carrier stays the default. Beside
the upload on 15 Mbps, the PoP's carrier showed the Balanced viewer
65.3 % of frames, where docs/75 recorded 82.5 % for quinn's carrier
sender, also on Cubic (§3). The cause of that gap was not investigated.

## 12. Verification (PP15)

On a real venue network, or a staged one with a NAT gateway and a shaped
uplink (25 Mbps up):

1. Windows host: download, double-click, allow the firewall on a
   Public-profile network from the console, pair from `gawk-admin`.
2. Guests: Chrome (Windows, macOS, Android), Firefox, Safari on macOS and
   iPhone, the desktop app on each OS, the iOS app. Each opens `gawk.ioio.fi`
   or scans the venue QR code, and ends up on the PoP. Record per platform
   which prompt appeared and how many clicks it took.
3. Six broadcasters, thirty viewers on the LAN: **zero bytes on the uplink**
   with nobody outside (router counters).
4. Four outside viewers on four different streams: three forwarded, the
   fourth sees `4012`. Uplink ≈ the sum of the three streams, and stays there
   as more outside viewers join those three.
5. The wall from outside: three live tiles, the rest "Party-only right now".
   One broadcaster hides; their tile goes.
6. Pull the venue's uplink for two minutes: LAN unaffected, outsiders see
   "broadcaster away", then recover on the same links.
7. Kill a party-only broadcast from `gawk-admin` on a phone: it ends at the
   party.
8. Roll the home fleet: the link moves pods, and forwarded streams blip at
   most once (≤ 1 s, docs/22's budget).
9. Load the venue's line: a bulk upload (a guest's cloud sync) while three
   streams are forwarded, and a game download while party viewers watch a
   home stream. Record the frames shown on each far side, carrier against
   `-pop-leg-delivery=datagram`.

## 13. Rejected, deferred, non-goals

### Rejected

- **Forwarding every party stream at once** (OD4's third option): uplink for
  streams nobody outside watches.
- **A trunk connection carrying all media with a per-datagram channel tag.**
  It eats into the ~1200-byte budget, invents framing and gives up the reuse
  of every existing route. (Pooling *sessions* on one connection is a
  different, deferred thing.)
- **Giving a PoP the fleet's secrets** (resume-token key, internal PSK) so
  it can act like a pod. A machine at a party is not the fleet.
- **The home fleet dialing the PoP.** The PoP is behind NAT; pods have no
  public addresses of their own.
- **A local CA, or IP certificates**: the first measured not to work for
  WebTransport (docs/41 §6), the second forbidden for private addresses.
- **A PoP prefix in broadcast IDs.** It changes the ID format in every client
  (R42 D3's reasoning).
- **The PoP name as its DNS label**, because CT logs would publish the
  default wall code.
- **Switching from remembered state**: a laptop at home would dial a dead
  LAN address.
- **Moving a live broadcast from home to the PoP.** It needs cross-fleet
  resume tokens, for a case the next broadcast fixes.
- **A self-signed relay on a public address by link** (OD12's wider
  option): it would turn `certHash=` into a general way around the WebPKI.
- **A NATS backplane** (docs/22 Option A). Its revisit trigger is a
  multi-region *backbone*; a PoP is one edge, and the WebTransport cascade
  covers it with its existing datagrams and R73's carrier.
- **R19's reliable delivery (`?delivery=reliable`) as the downlink.** It
  puts audio on a carrier of its own, which R73's reader refuses. It only
  closes a superseded carrier, which can fill the reader's three slots on a
  lossy line. And it resets a carrier whose record waits 500 ms, cutting
  the GOP short. The legs share one writer with R73's rules instead
  (item 18).
- **A budget measured at link-up** (OD13): a noisy burst test on a busy line.

### Deferred (wanted, not now)

- **A "Host a relay" switch in the desktop broadcaster**, launching the same
  binary (OD2).
- **Self-update for `gawk-relay`** (the R47 machinery); v1 only shows that an
  update exists.
- **Telemetry from PoP sessions.** A PoP holds no telemetry key; per-PoP
  derived keys would be the shape.
- **Device-scoped bans at a PoP** (a LAN address, for the evening).
- **Screening party-only streams** with keyframe samples over the link
  (≈ 10–40 kbps per stream; OD14's alternative).
- **A featured stream** that always gets an uplink slot.
- **Wall thumbnails** (R42's low-rate tiles) and a per-attachment
  party-only flag. Both need a room protocol bump (item 35).
- **Pooling legs on the link's QUIC connection.** Do it if PP8's time to
  first frame says the handshake matters, and measure the shared congestion
  controller first.
- **BBR on the legs.** quic-go has none. If PP8 finds Cubic losing table O's
  thin cell on the carrier, the options are upstream's or our own, both
  out of scope here (docs/75 §9). PP8's numbers are in §11.2.
- **Keyframe priority on the legs.** It needs webtransport-go to expose
  quic-go's `SetPriority`; do it if PP8 finds a GOP's first frames paying
  for its absence.
- **Public PoPs**: a friend's server with a public address and its own
  certificate, offered by address prefix rather than exact match. The design
  minus NAT.
- **Offline first load for browser guests** (an installable web app that
  works with no internet).
- **Bus events for the PoP lifecycle** (linked, unlinked, budget exhausted).

### Non-goals

- A multi-process PoP, or PoP-to-PoP links: a PoP talks only to its home.
- Geo routing or anycast.
- Any change to the media wire or chunking, or to the delivery modes
  viewers choose. The legs use R73's carrier as it is.
- New stateful infrastructure: the only new object kinds are a CRD and the
  external-dns and cert-manager objects the deployment already runs.
