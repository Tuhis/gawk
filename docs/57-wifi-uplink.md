# R55 — Broadcasting over Wi-Fi: it just works, and speaks only when viewers are affected

**Status**: designed 2026-09-23, owner decisions taken 2026-09-24 (§2).
**WU3 implemented 2026-09-29, ahead of WU2** (OD7), its manual pass
owner-pending; WU5 and WU6 not started; **WU4 is deferred** until
the carrier's measurement shows whether residual Wi-Fi harm still justifies
it (OD4).

**Split 2026-10-17** (owner direction): the uplink transport — the carrier,
its relay ingest, its engage policy and its costs (D1–D6, D9, WU0–WU2 and
the transport goals G1–G7, G10–G13) — moved to **R73,
[docs/75](75-reliable-uplink.md)**, because it is not specific to Wi-Fi.
docs/75 measured it before deciding and revises several of the decisions
below. This doc keeps the Mac's side: the status line (WU3), Improve (WU4),
QoS marking (WU5) and the Wi-Fi hardware pass (WU6).

**Relationship to earlier work**: this is R19 ([docs/24](24-viewer-network-resilience.md))
turned around. R19 made the **relay → viewer** leg (leg B) survive a lossy
path by carrying deltas on reliable streams; its Decision 1 left the
**broadcaster → relay** leg (leg A) "exactly as is", because every leg-A
reading until now was clean (docs/34 records 0.00–0.03 %). The first macOS
broadcast made leg A the problem. Wherever this doc says "the carrier", the
mechanism is docs/24's, verbatim; since 2026-10-17 it is specified in
docs/75.

---

## 1. Why, and what "done" means

### The finding (2026-09-23)

The first live pass of the macOS broadcaster (R52, [docs/54](54-macos-native-broadcaster.md))
streamed from an M1 MacBook on 5 GHz Wi-Fi (802.11ac, −43 dBm, 866 Mbps
link) to a Windows Firefox viewer. The viewer stuttered: `receivedFps` held
~50 while `decoderFps` collapsed to ~4 every few seconds —
`keyframe-only-delivery`, 157 gap resyncs against 244 keyframes in two
minutes.

- **The broadcaster was clean.** 55–57 fps captured, encoded and sent,
  `framesDroppedAtSend` 0, ~8 Mbps average at 1728×1080.
- **Leg A was not.** The relay counted `ingressLossRatio` **3.7 %** —
  frames that never arrived. That understates it: `IngressFramesLost` counts
  only frames with *no* chunk seen; a frame missing some chunks lands in
  `IngressChunksLost` instead (`gawk-server/internal/hub/ingress.go`).
- **Every earlier broadcast passed leg A**, including the Windows native app
  on the same Rust engine (wired) and Chrome broadcasters.
- **AWDL was most of it.** `awdl0` (AirDrop, Sidecar, Universal Control)
  periodically takes the Wi-Fi radio off-channel. With it down
  (`sudo ifconfig awdl0 down`) the viewer's broken frames fell from 20–170
  per minute to 0–4, and every diagnosis turned `ok`.

One lost chunk costs the rest of the GOP: the viewer has no reference for
the next delta and waits up to 500 ms for a keyframe. So a sub-percent
*packet* loss on a 20-packet frame is a visible freeze several times a
minute.

### Why the fix belongs in gawk

"Use Ethernet" is correct and not a product answer: the Mac is a laptop,
and most Mac broadcasters will be on Wi-Fi. Turning AWDL off needs root and
breaks AirDrop until reboot. And AWDL is only the loss we identified; the
same Wi-Fi link drops bursts for other reasons (a microwave, a neighbour's
channel, roaming).

The property that makes this solvable: **on the path that loses, the RTT is
tiny.** The Mac measured `timeSyncRttMs` ≈ 10 ms to the relay. QUIC can
retransmit a lost packet 3–5 times inside a budget a viewer never notices —
but only for stream data. Datagrams are never retransmitted, by design.

### Milestone acceptance criteria

Pre-registered. "Manual" means the reference setup: an Apple Silicon Mac on
Wi-Fi with AWDL **on**, broadcasting to the production fleet, one Windows
viewer, 10 minutes at the default rung.

*Revised 2026-10-17*: G1–G7 and G10–G13 were the transport's goals and moved
with it to docs/75 §8, rewritten there against the lab's measurements. G8
and G9 stay.

| # | Goal | Verified by |
|---|---|---|
| G8 | The app stays quiet unless viewers are affected: on a clean Wi-Fi link (AWDL up, carrier coping) nothing appears; with sustained harm the D7 line appears within 15 s and stays until the harm stops (revised 2026-09-29, OD7: not dismissible, and it may return later in the same broadcast); every string matches D7's copy table, and no **main-flow** string (status line, sheets, Settings) contains a D7 principle-3 banned term — Help and Diagnostics are exempt as principle 3 scopes them | unit (policy + string test) + manual |
| G9 | Improve (only if WU4 ships, OD4): from the amber line to "Wi-Fi improved" is **Improve → Continue → the system toggle**, nothing else; afterwards it is automatic on every Wi-Fi broadcast; AirDrop comes back with **no user action** after Stop, `kill -9` of the app, `kill -9` of the helper and a reboot mid-broadcast | manual |

## 2. Owner decisions (taken 2026-09-24)

| # | Decision | Choice |
|---|---|---|
| OD1 | Transport for leg-A deltas | *Moved 2026-10-17 to [docs/75](75-reliable-uplink.md) §3, which reopens it on measurements.* Was: **R19's carrier, reversed**: one reliable uni stream per GOP from the broadcaster, records of `uint16 len ‖ datagram`, reset at a deadline (D1–D3). Not per-frame streams, not NACK/ARQ over datagrams, not parity alone. |
| OD2 | When it engages | *Moved 2026-10-17 to [docs/75](75-reliable-uplink.md) §3, which reopens it on measurements.* Was: **Always, whenever the relay advertises it** — no RTT gate (D4). The escape hatch is a user-facing **advanced setting with a warning**, plus the relay's flag, env var and Helm value. |
| OD3 | The deadline | *Moved 2026-10-17 to [docs/75](75-reliable-uplink.md) §3, which reopens it on measurements.* Was: **150 ms** from a GOP's oldest unacknowledged record, capped by the GOP (D3). A knob, not a constant. |
| OD4 | "Improve" (pause AirDrop/Handoff via a helper) | **Deferred until after WU2.** The carrier ships first; WU6-style measurement then decides whether residual AWDL harm justifies a privileged helper. Until then the status line has **no button** and Help carries the remedies (D7). |
| OD5 | Which broadcasters | *Moved 2026-10-17 to [docs/75](75-reliable-uplink.md) §3, which reopens it on measurements.* Was: **The Rust desktop engine first** (Windows + macOS share it). The relay side serves any producer; the Go Linux broadcaster and the browser follow as separate chunks if measurement justifies them. *Revised 2026-09-24:* the Go Linux app is frozen (docs/58 OD6); R56's Rust Linux shell inherits WU2 with the shared engine. |
| OD6 | QoS marking | **Ship it unconditionally** (D8) — no stop rule. WU5 still proves the marking actually reaches the wire. |
| OD7 | *(2026-09-29)* The Mac app never noticed AWDL loss | **WU3 ships before WU2**, on QUIC's packet counters alone (D5's local loss, pulled forward as `uplinkPacketsSent`/`uplinkPacketsLost`). Without the carrier the line fires whenever datagram loss is sustained, which is exactly when viewers pause. **Help names the fix**: it carries `sudo ifconfig awdl0 down` and what it turns off, which revises principle 3 for Help only. The status line and Settings stay jargon-free. OD4 stands: no Improve button, nothing privileged. **No dismissal**: connection problems stay on screen until they stop, because a broadcaster must remain aware while viewers lose video. This revises principle 6 and G8's "at most once", and also takes Dismiss off the upload-bandwidth warning. |

## 3. Non-goals

- **Transport settings in the main UI.** The delivery choice is R73's
  (docs/75 D4); there is no deadline slider, no per-broadcast picker, and
  nothing on the Share card.
- **Telling users what's wrong with their router** in the main flow — Help
  only (D7 principle 7).

- **Pacing or frame-size capping.** Both were rejected for leg B by owner
  decision (docs/34 Finding 4, docs/35) because they add latency or cost
  quality. The carrier does not pace: records go out as the encoder
  produces them, and QUIC's congestion controller is the only pacer, as
  today.
- **Relay-side reconstruction** of parity on ingest — rejected in docs/34;
  the relay stays a byte forwarder.
- **A keyframe request of any kind.** docs/15 Decision 6 stands: nothing in
  this doc asks for a keyframe (§4 D6).
- **A viewer-side change.** Leg B already has R19 and R30.
- **Changing the rung on loss** (an auto-ladder for native apps) — docs/38
  D11 / R14 Decision 9 still say no ladder; this doc fixes delivery, not the
  bitrate.
- **Turning AWDL off without the user asking**, or leaving it off after the
  broadcast.

## 4. Decisions

### D1 — The uplink carrier: docs/24's stream, publisher → relay

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D1, unchanged.

### D2 — The relay: accept carriers from a publisher, behind a capability bit

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D2, extended (the stale-record rule, the knob).

### D3 — The broadcaster: carrier lifecycle and the deadline

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D3, **revised**: quinn exposes no per-offset acknowledgement, so the "oldest unacknowledged record" deadline cannot be built; the deadlines the engine can enforce are at the GOP boundary, and superseded GOPs are shed at the sender.

### D4 — When the carrier engages: always, when the relay supports it

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D4, reopened by the owner's 2026-10-17 direction (the broadcaster can choose the lowest latency) and decided there on measurements.

### D5 — Telemetry: say which uplink ran and what it cost

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D8, extended.

### D6 — Not a back-channel, and not docs/15 D6

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 D9, unchanged.

### D7 — The experience: it just works, and speaks only when viewers are affected

The bar is an Apple app: the right thing happens by default, and the rare
time the app speaks, it says one plain sentence and offers one button.
FaceTime's "Poor connection" is the model — it appears when the call is
actually suffering, names no protocol, and goes away by itself.

**Principles** (each one is an acceptance criterion in WU3/WU4):

1. **Silent by default.** The carrier (D1–D4) needs no setting, no toggle and
   no explanation. On a good network, nothing about R55 is visible.
2. **Speak only on measured harm.** Being on Wi-Fi is not a problem, and
   AWDL being up is not a problem. The app speaks only when viewers are
   losing video *despite* the carrier: carriers expiring at the deadline, or
   `uplinkLossPct` above a threshold, sustained for 10 s while at least one
   viewer is watching.
3. **No jargon where the user acts.** On the **main-flow surfaces** — the
   Share card status line, any sheet, and Settings (Advanced included) — no
   string says AWDL, channel, packet, uplink, QUIC, carrier, DSCP or Wi-Fi
   band. They talk about what the user sees and knows: Wi-Fi, AirDrop,
   Handoff, viewers, pauses. **Help** is exempt only for router vocabulary
   (it may say "channel" and "5 GHz", because principle 7 sends router
   advice there) and **Diagnostics** is exempt entirely — it is the
   technical truth, for us. *Revised 2026-09-29 (OD7):* Help may also
   name AWDL and give the `ifconfig` command that takes it down, because
   until WU4 that command is the fix it would otherwise withhold.
4. **One action, and it's reversible without thinking.** Anything gawk turns
   off comes back by itself when the broadcast ends, whatever happens to the
   app.
5. **Never interrupt the game.** No system notification, no sound, no modal
   while live. The message lives in the gawk window; the user sees it when
   they look.
6. **Ask once.** A dismissed message stays dismissed for that broadcast.
   Dismissed in two broadcasts in a row, it stops appearing and the option
   lives only in Settings. *Revised 2026-09-29 (OD7):* this applies to
   offers (Improve), never to the harm line. Connection problems can't be
   dismissed; they clear when the loss stops.
7. **Advice that needs a router belongs in Help,** not in the app's flow.

**The copy** (normative; wording changes go through review like code):

| Moment | What the user sees |
|---|---|
| Live on Wi-Fi, video getting through | Nothing |
| Viewers affected, on Wi-Fi — **until OD4 is decided** (WU3) | Amber line on the Share card: **"Your Wi-Fi is dropping some video. Viewers may see brief pauses."** No button; a small **?** opens Help. Same rules: quiet, ask once, never a notification. |
| Viewers affected, on Wi-Fi — **if WU4 ships**, mode never enabled | The same line with buttons **Improve** · Not Now. |
| After **Improve**, first time | A sheet: **"Improve Wi-Fi while you're live"** — "gawk can pause AirDrop and Handoff while you broadcast. They come back as soon as you stop." Buttons: **Continue** · Cancel. **Continue** opens System Settings at Login Items, where macOS asks for its own approval; the sheet waits and closes by itself when approval lands. |
| Approved | The amber line turns into a green check, **"Wi-Fi improved"**, for 3 s, then disappears. From now on this happens automatically whenever a broadcast is live on Wi-Fi. |
| Viewers affected, on Ethernet (or mode already on) | Amber line: **"Your network is dropping some video. Viewers may see brief pauses."** No button — there's nothing to fix on this Mac. **?** opens Help. |
| Settings (if WU4 ships) | One checkbox: **"Pause AirDrop and Handoff while live on Wi-Fi"**. Unchecking it stops the behaviour; the helper stays installed but idle. |
| Quality page | The delivery choice, R73's (docs/75 §6). *Revised 2026-10-17*: it was "Settings → Advanced → Video delivery: Automatic · Legacy"; docs/64 found there is no Advanced page, and docs/75 renames the choice after what it trades. |
| Help page | Why it happens in one paragraph; then, in order: use a cable if you can; turn off AirDrop while you stream (Control Center → AirDrop → Off — or, once WU4 ships, let gawk do it); if you manage your router, channel 149 (or 44) on 5 GHz avoids the problem without turning anything off. |
| Diagnostics / stats panel only | The technical truth for us: uplink mode, loss %, carriers expired, AWDL state, Wi-Fi channel. |

**How it's built** (what the copy sits on; the helper half is WU4, deferred by OD4):

- *Detection (WU3), unprivileged.* The route's interface is Wi-Fi
  (Network.framework path monitor against CoreWLAN's interface names);
  `awdl0` is `IFF_UP` (`getifaddrs`); `uplinkLossPct` and carrier expiries
  from D5; the current channel from CoreWLAN, for Diagnostics and Help only.
  The policy is one pure function over those inputs.
- *The helper (WU4), privileged, opt-in.* A root daemon registered with
  `SMAppService.daemon`; the approval is the system's Login Items toggle, so
  gawk never asks for a password itself. One XPC Mach service with two calls,
  `hold()` and `release()`, refused unless the caller is signed with the
  app's Team ID. `hold()` brings `awdl0` down (`SIOCSIFFLAGS`, no shelling
  out to `ifconfig`) and keeps it down while held — it watches the routing
  socket, because macOS raises AWDL again on demand.
- *Crash safety, the `gawk-pw-helper` way (docs/39).* The hold is the XPC
  connection: release, quit, crash or `kill -9` invalidates it and the helper
  restores AWDL. A marker written before taking AWDL down makes the helper
  restore on its own next start, covering a helper crash and a reboot. AWDL
  is never down without a live holder — principle 4 holds even when
  everything else fails.
- The app takes the hold when a broadcast goes live **on Wi-Fi** with the
  setting on, and releases it at Stop or when the route moves to Ethernet.
- Signed and notarized inside the bundle (docs/54 D13/D14):
  `Contents/Library/LaunchDaemons/` + its plist, same identity, same release
  unit.

**Windows** has no AWDL. The "Your network is dropping some video" line,
with no button, applies there as a later, small chunk.

### D8 — QoS marking: shipped unconditionally

OD6. Mark the broadcaster's UDP socket as interactive video: on macOS
`SO_NET_SERVICE_TYPE = NET_SERVICE_TYPE_VI`, which maps to the Wi-Fi WMM
video access category (priority airtime) and a DSCP. It needs no privilege
and is standard for real-time media, so it ships without a stop rule.

- **It must actually reach the wire.** quinn-udp sets a per-packet
  `IP_TOS`/`IPV6_TCLASS` control message carrying only the ECN bits, which
  likely overrides a socket-level DSCP. `SO_NET_SERVICE_TYPE` may survive it
  (the service class is a socket attribute, not the TOS byte). WU5 proves it
  with a packet capture; if it does not survive, the fix is a patch to the
  vendored stack recorded in `vendor/wtransport/GAWK-PATCH.md`, not dropping
  the feature.
- **Windows**: the equivalent is a DSCP on the socket, which Windows is
  believed to ignore for applications without a QoS policy. WU5 records
  what a capture shows; if it is ignored, Windows ships no marking and says
  so here.
- **Expectation, not a gate.** WMM priority does not stop AWDL taking the
  radio off-channel. WU6 records the effect of marking against the carrier
  alone, for the record.

### D9 — Cost, the tradeoff, and scaling to high bitrates

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md).* docs/75 §2 (measured, replacing this section's estimates) and D10 (receive windows). Two statements here were wrong and are corrected there: the default viewer is *adaptive*, not live-edge (`playbackPresets.ts`: `balanced`), and Cubic's reaction to random loss is not a side effect to watch for but the dominant cost on any path longer than a LAN.

## 5. Architecture

*Moved 2026-10-17 to [docs/75](75-reliable-uplink.md) §7.*

## 6. Chunks and acceptance criteria

### WU0 — Baseline, and the telemetry that can see it

*Moved 2026-10-17 to docs/75 as **RU0**.* Its first row (the `relayscrape.ScrapeOnce` double-append) was done 2026-10-08, test-first (`TestRoundCarriesEachObservationOnce`, `TestScrapedRoundsYieldWindowedRelayFacts`).

### WU1 — Relay: carrier ingest behind `CapUplinkCarriers`

*Moved 2026-10-17 to docs/75 as **RU1**.*

### WU2 — Rust engine: carrier sender, deadline, engage policy

*Moved 2026-10-17 to docs/75 as **RU3**.*

### WU3 — macOS: the quiet status line

| Acceptance criterion | Verified by |
|---|---|
| The D7 policy (Wi-Fi × harm sustained 10 s × viewers > 0 × dismissed × mode state) is a pure function with a table test; the platform probes only translate values | unit |
| Every D7 row renders with its exact copy; a string test fails if any **main-flow** string (status line, sheets, Settings incl. Advanced) contains a banned term (AWDL, channel, packet, uplink, QUIC, carrier, DSCP, Wi-Fi band); Help is checked for everything except router vocabulary; Diagnostics is not checked | unit |
| On clean Wi-Fi with AWDL up nothing appears; with injected loss the line appears within 15 s; ~~Not Now holds for the broadcast; two consecutive dismissals stop it~~ the line can't be dismissed and clears by itself (OD7) | unit (fake clock) + manual |
| No notification, sound or modal is raised by this feature while live | unit + review |
| Help page (README macOS section, linked from **?**) in D7's order: cable, Improve, router channel | review |
| Diagnostics shows uplink mode, loss %, carriers expired, AWDL state and channel | unit |

**As built (2026-09-29, OD7)** — ahead of WU2, so the policy's harm input is
QUIC's packet loss alone, with no carrier expiries yet:

- *Harm*: packets lost in ≥ 4 of the last 10 s while someone is watching
  (an unknown viewer count does not suppress it); cleared after 30
  loss-free seconds. The unit is lossy seconds, not a percentage: at gawk's
  rates each lossy second is likely a visible pause
  (`crates/engine/src/lossnotice.rs`, table tests).
- *Counters*: `quinn::ConnectionStats::path` sent/lost packets through a
  `RelaySession::path_counters` seam, summed across resumes by the sender;
  `uplinkPacketsAvailable`/`Sent`/`Lost` in the stats, the diagnostics dump
  and telemetry (field registry + stored-shape golden). These are
  cumulative counters instead of D5's `uplinkLossPct`, so a rate can be
  derived over any window.
- *Probe*: unprivileged and framework-free (`crates/app-macos/src/network.rs`).
  A connected UDP socket names the route's local address, `getifaddrs` maps
  it to an interface, `SIOCGIFMEDIA` says 802.11, and `awdl0`'s `IFF_UP`
  flag gives the AWDL state. The Windows platform returns no facts, so
  the line stays off there (D7's later chunk).
- *A VPN hides the radio* (found on the first hardware run): the reference
  Mac reaches `api.gawk.ioio.fi` through a WireGuard split tunnel
  (`utun7`), which has no media, so the first probe said "not Wi-Fi" while
  every packet crossed en0. A tunnel route (`IFF_POINTOPOINT`) is now
  looked through, first to the interface a public address routes via,
  and failing that (a full tunnel) to "an associated Wi-Fi link has an
  address and no wired link does". `vpn` is reported beside `wifi`. The
  decision is a pure `classify` with table tests.
- *Surface*: a Live-page banner with **Help** (a labelled button rather
  than a bare **?**) and no Dismiss (OD7). The same goes for the
  bandwidth warning, which takes precedence when both apply. The
  connection tile reads "Upload is struggling" while harm is measured.
- *Not built*: the Wi-Fi channel in Diagnostics (CoreWLAN; not needed for
  the line), and the carrier rows, which wait for WU2.
- *Revised 2026-10-17 (docs/75 RU5)*: once the carrier is engaged, QUIC's
  packet loss stops being harm — the carrier repairs it, and the line
  would warn about losses no viewer saw. The harm input becomes the frames
  the uplink actually lost (expired carriers, shed records); packet loss
  stays the input in the lowest-latency mode, where it is still harm.
- *Manual pass, 2026-09-29* (§8): the line appeared with AWDL up and
  cleared after `sudo ifconfig awdl0 down`, on the reference Mac through
  its VPN. It rose again on a burst with AWDL still down. (That run's
  build still had a Dismiss button, which the owner then removed.)

### WU4 — macOS: Improve (deferred — OD4 is decided after WU2)

Not started until the carrier (docs/75 RU3) has been measured on the
reference setup. If residual AWDL harm with the carrier is within docs/75's
goals, WU4 is dropped and D7's no-button line stays; otherwise it is built to
these criteria.

| Acceptance criterion | Verified by |
|---|---|
| The flow is **Improve → Continue → system toggle**; the sheet closes itself when `SMAppService` reports approval; cancelling at any step leaves everything as it was | manual |
| After approval the Settings checkbox is on and every later Wi-Fi broadcast holds AWDL down with no prompt; on Ethernet no hold is taken; route change to Ethernet mid-broadcast releases it | unit (hold policy) + manual |
| XPC caller verification: a binary not signed with the app's Team ID is refused | unit (signature check against a test binary) + manual |
| AWDL restored with no user action after Stop, `kill -9` of the app, `kill -9` of the helper, reboot mid-broadcast; AirDrop works afterwards (G9) | manual, each case recorded |
| Bundle layout, signing and notarization pass the existing MB7 checks (`codesign --verify --deep --strict`, `spctl --assess`) with the helper inside | CI (signed run) |

### WU5 — QoS marking

| Acceptance criterion | Verified by |
|---|---|
| macOS: the socket is marked `NET_SERVICE_TYPE_VI`; a packet capture shows the marking on the wire despite quinn-udp's per-packet TOS cmsg (patching the vendored stack if needed, recorded in `GAWK-PATCH.md`) | manual capture, recorded in §8 |
| Windows: a capture records whether a socket DSCP reaches the wire; if not, no marking ships there and §4 D8 says so | manual capture, recorded |
| The effect on leg-A loss and retransmits is recorded against the carrier alone (for the record; not a gate) | manual |

### WU6 — The on-hardware acceptance pass

| Acceptance criterion | Verified by |
|---|---|
| G8 (and G9 if WU4 ships) pass on the reference setup, with the carrier engaged; the transport goals are docs/75's and run in its RU7 on the same Mac | manual |
| §8 records every measured number against the docs/75 RU0 baseline | review |

## 7. Risks

*Revised 2026-10-17*: the transport's risks — retransmit latency in
playout, congestion control's reaction to loss, high rungs on thin links,
quinn's scheduling — moved to docs/75 §10, most of them now measured.

- **Apple and AWDL.** The helper depends on bringing an interface down from
  root. Apple can change that in any release; the status line and the carrier do
  not depend on it, which is why WU4 is separable.
- **A privileged component** is new attack surface. XPC caller verification,
  two calls, no arguments, no media — the same "owns nothing" shape as
  `gawk-pw-helper`.

## 8. Measurements

Empty until WU0. Baseline observations from the 2026-09-23 session (not
the WU0 protocol, recorded for context): AWDL on, datagram mode —
`ingressLossRatio` 3.7 % (whole frames only — a **lower bound** on the
`ingressFrameLossRatio` G1 is stated in, since partly received frames are
not in it; WU0 re-baselines in G1's unit), viewer 20–170 incomplete frames/min;
AWDL off — viewer 0–4 incomplete frames/min. The Mac's Wi-Fi was on
channel 100, not one of AWDL's social channels (44/149 on 5 GHz), so the
radio was hopping.

Published AWDL timing, for WU0 to confirm or refute on this hardware: 16 TU
(~16.4 ms) availability windows on a ~1.05 s channel sequence, at least 25 %
of airtime and up to 50–75 % when active, ~13 % throughput loss with the AP
on a different channel (Stute et al., MobiCom '18, arXiv 1808.03156); field
reports of 50–100 ms stalls about once a second, or ~80–90 ms stalls in 1–2 s
bursts every 10–12 s. Most reports describe **delay** rather than loss, while
this session's relay counted **loss** — WU0 records both, and the spike
length, because D3's deadline and G3's bound depend on it.

**WU3 manual pass (2026-09-29).** This is not the WU0 protocol. The
reference M1 on Wi-Fi reached the production relay through a WireGuard
split tunnel. It shared one Firefox window at 1778×1080@60, 12 Mbps,
with 1–2 viewers. The counts are QUIC packets as the broadcaster's stack
declared them lost (`uplinkPacketsLost`):

| Time (UTC) | State | Packets lost / sent |
|---|---|---|
| 18:37:12 | live; `wifi: true, vpn: true, awdl_up: true` | — |
| 18:38:12 | AWDL up | 51 / 48,070 (0.11 %) |
| 18:38:59 | **line raised**: 24 lost in the last 10 s | 93 / 87,174 |
| 18:39:12 | AWDL up | 115 / 97,242 |
| 18:39:36 | `sudo ifconfig awdl0 down`; probe sees it within 1 s | — |
| 18:40:11 | **line cleared**: 0 lost in the last 10 s | 124 / 126,675 |
| 18:41:12 | AWDL down, a full minute | 129 / 174,831 |
| 18:41:30 | **line raised again**, AWDL still down: 18 lost in the last 10 s | 147 / 190,795 |
| 18:41:51 | stopped (the line had been dismissed; that build still had Dismiss) | — |

With AWDL up the loss ran at ≈ 64 packets a minute (≈ 0.13 %). With it
down, the first minute lost 5 (≈ 0.01 %), too sparse to raise the line.
Then a burst of 18 in ten seconds raised it again. So AWDL is the steady
part of this Mac's leg-A loss, but not all of it: bursts of ordinary
Wi-Fi loss remain, which is the case the carrier (WU2) exists for. Four
minutes is not enough to put a share on it.

## 9. References

- [docs/24](24-viewer-network-resilience.md) — R19 carriers, the format reused here
- [docs/34](34-live-edge-forward-parity.md) — R29 parity; leg-A cleanliness; pacing rejected
- [docs/35](35-connection-interleaving.md) — R30, the leg-B burst threshold
- [docs/38](38-windows-native-broadcaster.md) — F-12, quinn's datagram-first packing
- [docs/15](15-viewer-live-edge.md) — Decision 6, the rejected back-channel
- [docs/39](39-linux-app-sharing.md) — the `gawk-pw-helper` crash-safety shape
- [docs/54](54-macos-native-broadcaster.md) — R52, where the finding came from
- `BUGS.md` — the live relay-facts bug WU0 fixes
