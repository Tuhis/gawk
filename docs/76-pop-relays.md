# R74 — PoP relays: a one-click LAN relay that federates with the home fleet (docs/76)

**Status**: designed 2026-10-21 in an owner interview (decisions OD1–OD14
below); revised 2026-10-22 for R73's reliable uplink (OD15–OD16: both legs
carry deltas on R73's carrier); not started. Chunks **PP0–PP15** (`PP` = PoP; unused). Three phases,
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
| OD15 | How the uplink leg carries a party stream (asked once R73 had landed) | **R73's carrier whenever home offers it.** Keyframes keep their own streams; deltas and parity ride one carrier per GOP. The leg is leg A for every outside viewer, on a venue uplink shared with the guests' own traffic: docs/75 table O's case (§3). Datagrams stay one flag away (`-pop-leg-delivery`), and PP8 measures the PoP's own sender before the default ships (item 18). |
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

4. **LAN address.** The primary address is the IPv4 address of the
   interface that holds the default route. Loopback, link-local, Docker and
   virtual bridges, and CGNAT/VPN ranges (`100.64/10`) are excluded. The
   console lists the candidates and lets the host pick another. A DHCP change
   is picked up and republished (offers, console, DNS record).

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

6. **The host's own machine** can open the PoP-served web app at
   `http://127.0.0.1:4480/app/` (loopback is a secure context, and M4 shows
   it reaches the relay without a prompt). This is how a host with no
   internet at all still has a working broadcaster and a projector view.
   Its generated `config.js` names the relay by LAN address with its
   hashes, in a new `relayCertHashes` field: the `devCertHashHex`
   mechanism, generalised to two hashes and to a non-default URL. It also
   sets `requirePublishSecret: false` explicitly, because on a loopback
   origin the fallback would prompt for a secret (the docs/41 §4.2.2 trap).

### 6.2 Pairing

7. **The operator creates the PoP in `gawk-admin`**: a name (the CR name and
   default wall code, e.g. `assembly-2026`; at most 63 characters, because it
   is also a label value, item 14), a display name, an optional end
   date, the budget and the guest options (§7.2). `gawk-admin` writes the
   `Pop` CR and shows **one pairing string, once**:
   `gawk-pair:api.gawk.ioio.fi:4433/K7M2Q-9XDA4-…` (the home relay endpoint,
   then 128 random bits in base32). Only its SHA-256 is stored
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
    Creation is the atomic reservation, so home mints and PoP mints can never
    collide. The PoP announces the ID to its broadcaster after the reply,
    which costs one WAN round trip on the mint. Dormant reservations are
    renewed by the link-home pod, which takes them over on a link move by
    listing the label. A reservation counts against the PoP's
    `spec.limits.maxBroadcasts` (default 40, set in the wizard), not against
    the fleet's `-max-broadcasts`. A mint past that limit gets
    `reserved{limit}` and stays **local-only**, as an unlinked collision does
    (item 15). The console says "Over this PoP's limit at gawk.ioio.fi:
    party-only". The PoP's own `-max-broadcasts` (50) still caps the party
    itself. In single-pod home mode a reservation is an in-memory entry.

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

18. **The uplink leg**, `CONNECT /pop/publish/{id}`, lands on any pod C. C
    checks the credential and that X is reserved for *this* PoP. That check
    stands in for the resume token as the proof of ownership; an ordinary
    `/publish/{id}` never claims a reservation. C then claims X's Lease as
    origin (`cluster.go:223`), and B's informer sees the origin
    and edge-pulls from C as it would for any broadcast. On the PoP side the
    leg is a publisher fed from the local hub. It starts with the hub's
    **join-prime** (cached keyframe with embedded config, and the clock
    mapping, rewritten per §6.7), then forwards keyframe streams
    byte-identical and audio, `TimeSync` and `ClockMapping` as datagrams:
    the docs/22 D9 edge rules, pointed the other way. The first outside
    viewer starts from the prime, not from the next GOP.

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
    also puts audio on a carrier of its own (docs/20), which R73's reader,
    taking only video and parity records, refuses. So the legs get that
    drain with R73's rules and audio as datagrams, as an option; viewers
    keep today's. The downlink uses the same writer (item 23).

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

22. **Time to first frame** for the first outside viewer is the miss, two
    in-cluster hops to A, one WAN trip down the link, a fresh QUIC + HTTP/3 +
    CONNECT handshake up (about 2 WAN RTTs), a Lease write, an informer
    event and an in-cluster edge pull. The target is **≤ 1.5 s at 40 ms WAN
    RTT**, measured in PP8, not assumed. Later viewers join as for any home
    broadcast. Pooling the legs on the link's QUIC connection would save the
    handshake. It is deferred until PP8's numbers say it matters (§13),
    because it trades per-leg congestion control for one shared controller
    on a constrained uplink, and that also needs measuring.

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
    and parity on per-GOP carriers under R73's sender rules, audio and the
    clock messages as datagrams. Without the parameter home serves an
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

24. **Downlink budget**, optional, enforced at the PoP. Separately, the
    home fleet caps a PoP's concurrent downlink legs
    (`spec.limits.maxDownlinkStreams`, default 20) so a compromised PoP
    cannot drain the fleet's egress.

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

### 6.9 Moderation reach

29. **Kills and ID bans reach the party** (OD7). Relay pods already watch
    `Ban` CRs (`moderationsrc/k8s.go`). The link-home pod forwards every ID
    ban, kill cooldowns included, that names one of its PoP's reservations,
    as `ban{id, until}`. The PoP ends the broadcast with `4006` (its viewers
    and its publisher see docs/59's moderator copy) and refuses reclaims of
    that ID until the ban ends. Party broadcasts appear in `gawk-admin`'s
    Broadcasts view from their reservations, forwarded or not, with the PoP
    badge (§7.2), so they can be killed before anyone outside watches.

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

44. **Apple platforms.** The iOS app gains `NSLocalNetworkUsageDescription`,
    which it lacks today. Per TN3179, the first dial after an offer may be
    denied before the user answers, so it retries once after the prompt.
    While backgrounded and unanswered it never dials. The desktop app on
    macOS gets one OS prompt. Chrome on macOS needs the OS permission too.
    When a probe fails there, the note names it.

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
- **Not a breach of docs/42 D10.** That decision makes the portal read-only
  for *relay configuration*, which belongs to the chart. A `Pop` is a
  runtime resource like a `Room` or a `Ban`, and its CR is its source of
  truth. The fleet's own flags (`-pops`, the zone, the issuer) stay in
  the charts.

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
| `0x18 LocalRelayInfo` | PoP → client, server uni stream after upgrade | display name, flags (`linked`, `onWall`), home app origin, wall code; trailing bytes tolerated (the 0x11 extension seam) |
| `0x19 LocalRelayOffer` | home → client, server uni stream after upgrade | PoP handle, display name, validity, candidates (URL + 0–2 SHA-256 hashes); trailing bytes tolerated |
| `4012 PopUplinkFull` | home → viewer | not terminal; retry every 20 s; in-band `SessionClosing` first |
| `4013 PopNoDemand` | home → PoP uplink leg | not terminal; the PoP waits for the next `want` |

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
| | `-pop-dns-zone` | empty (pinned only) |
| | `-pop-cert-issuer` | empty |
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
RBAC: relays get `pops` get/list/watch and `pops/status` patch. `gawk-admin`
gets `pops` CRUD **and `pops/status` patch**, because with a status
subresource the API server ignores status written through the main
resource. It also gets `certificaterequests` create/get and `dnsendpoints`
CRUD.

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
- `internal/cluster/cluster.go:223` `Claim`, `:568` `Lookup`, `:720` the
  janitor (dormant reservations must survive it); `internal/hub/hub.go:945`
  `EdgePublish`, `:1406-1455` the count fan-out.
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
  like a link's), `features/servers/probe.ts:90-131`.
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
| **PP6** — `Pop` resources | CRD + `gawk-server/pops` package + chart RBAC; `-pops-file`; `gawk-admin` `/api/v1/pops` CRUD + pairing string, OpenAPI, the PoPs list, wizard and detail | The chart renders the CRD gated on `pops.enabled`; the pairing string is shown once and only its hash is stored; `gawk-server/pops` reaches no quic-go (a `go list -deps` check in CI: `gawk-admin` builds it, and the `replace` does not cross modules); OpenAPI drift check green; UI tests |
| **PP7** — the link | `/pop/link`, pairing exchange, credential, status lease and force-take, `hello`/`config`/`stats`/`health`/`bye`, reconnect, `/pop/addr`; console pairing; `0x18`'s `linked` flag and home origin; `GET /internal/admin/pops` (link state; PP8, PP9 and PP13 add their counts) and the PoP cards reading it; local rooms when home runs `-rooms` off | In-process test (3 home pods, fake clientset with the docs/22 finding-11 reactors) + one PoP: pair; relink after a drain lands on another pod, which force-takes; used, expired and wrong tokens refused with legible statuses; a credential paired in time outlives `spec.pairing.expiresAt`; Re-pair refuses the old credential at once, also when two Re-pairs land in the same second; the console tells `bye{repair}` from `bye{revoked}`, and recovers the reason from `401`/`403` when the `bye` is lost; `-pops-file` credentials work with no pairing; a `proto` major mismatch → `426`; a paired PoP's `0x18` has `linked` set and the home origin, and an unpaired or revoked one has it clear; `/internal/admin/pops` answers on the ops listener behind the admin gate and is absent from the public mux (test); with home `-rooms` off, `/room/new` on a linked PoP mints a local room |
| **PP8** — reservations and uplink | Reserve-before-announce, unlinked mints, `/internal/pop`, `/pop/publish`, admission and `4012` (+ notice), linger and `4013`, counts both ways, the clock rewrite, broadcast events with `pop`; the leg writer (R73's sender rules and audio as datagrams, an option on the hub's reliable drain), the uplink carrier, `-pop-leg-delivery`, and a `tools/uplinklab` sender driven by the leg writer | In-process: an outside viewer on pod B receives a byte-identical prime and deltas; **zero bytes on the uplink without demand** (counter); linger closes with `4013` and the Lease is back to a reservation; the fourth stream is refused and Chrome reads `4012` (tier-1 e2e); a mint past `limits.maxBroadcasts` gets `reserved{limit}` and stays party-only; the count is right on both sides; **time to first frame measured at 40 ms netem RTT and recorded here** (target ≤ 1.5 s); event schemas and vectors updated, no address field; against a pod advertising `CapUplinkCarriers`, deltas and parity leave only as carrier records, no more than two carriers are ever live, and a superseded one is reset 150 ms after it finished (fake clock); a 400 ms write stall within a GOP does not reset its carrier, and a write blocked across a rotation is abandoned when the new keyframe fans out and its carrier reset (fake clock); a 10 s outage (netem) does not end the leg; with home's `-uplink-carriers=false` or `-pop-leg-delivery=datagram` the leg is datagrams, byte-identical to before; **the PoP's sender measured in the uplink lab**, carrier against datagrams, on docs/75 table O's 30 and 15 Mbps cells beside a TCP upload and on table B's bursts and outages, three runs a cell, recorded here together with whether the missing keyframe priority costs a GOP's first frames. The carrier stays the default unless datagrams show the Balanced viewer more frames in one of those cells by more than the cell's run-to-run spread; if they do, datagrams become the default and item 18 says so |
| **PP9** — downlink | `/pop/subscribe`, the PoP edge dialer, upward count reports, the downlink budget, `maxDownlinkStreams` at home; the downlink carrier: `?delivery=carrier` and the leg writer on `/pop/subscribe`, R73's reader in the PoP's edge pump (a carrier frees its keyframe slot, a refused stream is reset), the link's timers and R73's receive windows on the PoP's dialer | Ten party viewers of one home broadcast → one leg (counter); `G` at home includes them; the leg cap refuses the 21st distinct broadcast; a 1440p home stream's GOPs cross without blocking on flow control under a 40 ms RTT (the in-process UDP proxy, as docs/75 table H); audio reaches the PoP as datagrams and never as a carrier record; without `?delivery=carrier` home serves datagrams (test); a stream the pump refuses is reset, not left unread (test); a 10 s outage does not end the leg, and the PoP's prime survives it; **the downlink measured in the uplink lab beside a saturating TCP download** (a game update's shape), carrier against datagrams, three runs a cell, recorded here |
| **PP10** — moderation reach | Ban forwarding and local enforcement, revoke, IP-ban and limiter exemption, the admin badge and party-only rows, the screening-notice relay | A kill in `gawk-admin` ends a party-only broadcast at the PoP with `4006` for its viewers and broadcaster; a banned ID cannot be reclaimed at the PoP; an IP ban on the PoP's public address does not cut a leg; **after a revoke**, the PoP's reservations are gone, an outside viewer of a party code gets `4000` (attached) or `404` (new dial), no offer is sent, the `DNSEndpoint` is deleted, and the PoP keeps serving the LAN as a standalone relay; the PoP's `0x11` carries the screening bit while home's does |
| **PP11** — the party wall | Wall room creation with the PoP, `spec.pop` / `spec.maxBroadcasts` / `spec.maxParticipants`, vouched-only attach and detach, Hide/Show and `wall=hidden`, `0x18`'s wall code and on-wall state, link-proxied room control, the local read-only wall | Extended `rooms-assert`: party broadcasts appear on the wall, **a fifth and later party broadcast included**; **an ordinary `Attach` to the wall (an outsider's home broadcast) is refused**; Hide removes and Show restores; a hidden-remembered broadcaster never appears; an outside participant's tile past the budget shows "Party-only right now" and joins when a slot frees; a protocol-1 client parses every snapshot; unlinked, party guests see the local wall; a broadcaster's `0x18` carries the wall code and an on-wall state that follows Hide, Show and `wall=hidden` |
| **PP12** — the named tier | CSR on the link, the `gawk-admin` reconciler (`CertificateRequest`, `DNSEndpoint`), random labels, the PoP's HTTPS UI and generated `config.js`, renewal at half-life, the rebinding self-check, QR selection | Reconciler unit tests against fake cert-manager and external-dns objects: a CSR with a wrong SAN is refused; the PoP's CSR names the host it got in `config`; a renewal CSR is issued exactly once (`issuedForCSR`), and the PoP never receives the previous chain for it; one staging issuance on the reference deployment; a phone on the LAN loads the PoP UI with no prompt (PP15) |
| **PP13** — offers at home | Address matching (v4 exact, v6 /64), `0x19` on the four session kinds, mirrors and vectors, the `lr=1` demand deferral (item 21, `-pop-offer-defer`), offer counts on `/internal/admin/pops`, metrics | Go tests: a matching client gets the offer, a non-matching one never does, `autoSwitch: false` suppresses it; **the handle is identical across pods and across a home restart with no `-stats-key` set**; a matching `lr=1` session's demand is held for at most `-pop-offer-defer`, and a non-matching or `lr`-less session's is not held; vectors byte-identical in all three copies |
| **PP14** — the switch | Web (permission query, parallel probes, reconnect, chip, opt-out, denied note), desktop (Ready page), iOS (broadcaster + player, usage string, first-deny retry) | Web unit tests for every permission state; tier-1 e2e with a CDP-granted permission: a viewer switches with one freeze ≤ 1 s, and with the permission denied stays on home; native tests for offer parsing and pinning; the Apple prompts on device (PP15) |
| **PP15** — the party pass | A real or staged LAN party against the reference deployment | §12's list, recorded here with dates and builds |

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
  out of scope here (docs/75 §9).
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
