# R55 — Broadcasting over Wi-Fi: it just works, and speaks only when viewers are affected

**Status**: designed 2026-09-23, owner decisions taken 2026-09-24 (§2).
Chunks **WU0–WU6**, none started; **WU4 is deferred** until WU2's
measurement shows whether residual Wi-Fi harm still justifies it (OD4).

**Relationship to earlier work**: this is R19 ([docs/24](24-viewer-network-resilience.md))
turned around. R19 made the **relay → viewer** leg (leg B) survive a lossy
path by carrying deltas on reliable streams; its Decision 1 left the
**broadcaster → relay** leg (leg A) "exactly as is", because every leg-A
reading until now was clean (docs/34 records 0.00–0.03 %). The first macOS
broadcast made leg A the problem. Wherever this doc says "the carrier", the
mechanism is docs/24's, verbatim, and is not re-argued here.

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
viewer, 10 minutes at the default rung. WU0 records the baseline every row
compares against.

| # | Goal | Verified by |
|---|---|---|
| G1 | Leg-A frame loss with the carrier uplink: **damaged frames** (never arrived *or* arrived with a chunk missing) ≤ **0.1 %** of frames expected, over 10 min, AWDL on — the `ingressFrameLossRatio` WU0 defines. Chunk loss is reported beside it as a diagnostic, not a gate | manual, relay `/statusz` counters (history API) |
| G2 | The viewer stops stuttering: `reorderGapResyncs` ≤ 2/min and no `keyframe-only-delivery` finding over the same 10 min | manual, viewer telemetry |
| G3 | Latency cost bounded: viewer `capToRenderMs` p50 within **+20 ms**, p95 within **+50 ms** of the same Mac on Ethernet in datagram mode | manual, paired runs |
| G4 | No regression where nothing was wrong: the Windows app on wired Ethernet with the carrier uplink engaged shows fps, latency and leg-A loss within noise of datagram mode | manual, paired runs |
| G5 | Compatibility: a carrier-capable broadcaster against a relay without the capability sends datagrams, unchanged; an old broadcaster against a new relay is unchanged; with `-uplink-carriers=false` the relay's `/statusz`, metrics and wire are byte-identical to pre-R55 | integration (real `gawk-server`) + diff assertion |
| G6 | Wire parity: the new capability bit and any new constant are in `gawk-server/wire`, `wire.ts`, `gawk-broadcast/internal/wirecheck` and `crates/wire`, golden vectors byte-identical | unit (existing mirror tests) |
| G7 | Viewers untouched: no change in `gawk-app` beyond the `wire.ts` mirror | review |
| G8 | The app stays quiet unless viewers are affected: on a clean Wi-Fi link (AWDL up, carrier coping) nothing appears; with sustained harm the D7 line appears within 15 s, at most once per broadcast; every string matches D7's copy table, and no **main-flow** string (status line, sheets, Settings) contains a D7 principle-3 banned term — Help and Diagnostics are exempt as principle 3 scopes them | unit (policy + string test) + manual |
| G9 | Improve (only if WU4 ships, OD4): from the amber line to "Wi-Fi improved" is **Improve → Continue → the system toggle**, nothing else; afterwards it is automatic on every Wi-Fi broadcast; AirDrop comes back with **no user action** after Stop, `kill -9` of the app, `kill -9` of the helper and a reboot mid-broadcast | manual |
| G10 | Telemetry says which uplink ran: the broadcaster reports `uplinkMode`, carrier counters and local QUIC loss; the relay reports carrier ingest per broadcast | unit + one real session in the dashboard |
| G11 | High bitrate: at 50 Mbps / 1440p60 on a Wi-Fi link with ≥ 3× headroom, G1 and G2 hold; on a link without headroom the carrier expires (it does not queue unboundedly) and the D7 status line appears | manual, paired runs |
| G12 | Always-on is never worse: on a shaped 80–100 ms RTT path with 1 % loss, carrier mode's viewer freezes, `capToRenderMs` p50/p95 and leg-A loss are no worse than datagram mode's | integration (shaped loopback) + manual over a real WAN |
| G13 | The escape hatch works end to end: Settings → Advanced → Legacy (after its warning) sends datagrams; `-uplink-carriers=false` on the relay does the same for every broadcaster | unit + manual |

## 2. Owner decisions (taken 2026-09-24)

| # | Decision | Choice |
|---|---|---|
| OD1 | Transport for leg-A deltas | **R19's carrier, reversed**: one reliable uni stream per GOP from the broadcaster, records of `uint16 len ‖ datagram`, reset at a deadline (D1–D3). Not per-frame streams, not NACK/ARQ over datagrams, not parity alone. |
| OD2 | When it engages | **Always, whenever the relay advertises it** — no RTT gate (D4). The escape hatch is a user-facing **advanced setting with a warning**, plus the relay's flag, env var and Helm value. |
| OD3 | The deadline | **150 ms** from a GOP's oldest unacknowledged record, capped by the GOP (D3). A knob, not a constant. |
| OD4 | "Improve" (pause AirDrop/Handoff via a helper) | **Deferred until after WU2.** The carrier ships first; WU6-style measurement then decides whether residual AWDL harm justifies a privileged helper. Until then the status line has **no button** and Help carries the remedies (D7). |
| OD5 | Which broadcasters | **The Rust desktop engine first** (Windows + macOS share it). The relay side serves any producer; the Go Linux broadcaster and the browser follow as separate chunks if measurement justifies them. *Revised 2026-09-24:* the Go Linux app is frozen (docs/58 OD6); R56's Rust Linux shell inherits WU2 with the shared engine. |
| OD6 | QoS marking | **Ship it unconditionally** (D8) — no stop rule. WU5 still proves the marking actually reaches the wire. |

## 3. Non-goals

- **Transport settings in the main UI.** The one delivery override (OD2)
  lives under Advanced, behind a warning (D4); there is no deadline slider,
  no per-broadcast picker, and nothing on the Share card.
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

A carrier is a unidirectional QUIC stream opened by the **broadcaster**,
starting with the existing prologue `0x01 0x0A` (`ReliableCarrier`), then
records of `uint16 len ‖ datagram`. Each record is **byte-for-byte the
datagram that would otherwise have been sent**: `VideoChunk` (0x01) and
`ParityChunk` (0x0E). So the relay's ingest for a record is exactly its
ingest for a datagram, and everything downstream — accounting, the ingress
window, fan-out to datagram viewers, R19 carriers for resilient viewers, R30
striping, R29 parity prefixes, the DVR — is untouched.

- **One carrier per GOP.** A new carrier opens at each keyframe. It holds
  that GOP's deltas and their parity, nothing else.
- **Why per GOP, not per frame.** Head-of-line blocking is harmless *inside*
  a GOP: every delta depends on the one before it, so a delta that arrives
  before its predecessor is undecodable anyway. Per-GOP streams also keep
  the stream rate at ~2/s instead of ~60/s. docs/24 rejected per-frame
  streams for the same reason (unproven stream-credit behaviour at 60/s).
- **What stays a datagram**: audio (`AudioFrame`, 20 ms Opus — a lost packet
  is one concealment, not a freeze), `TimeSync`, and everything when the
  carrier is not engaged.
- **Keyframes stay on their own streams** (`StreamFrame`, R8) with docs/38
  F-12's rules unchanged.
- **Not a `StreamFrame` with the keyframe flag clear.** The flag is
  "reserved" in `wire.go`, but today's `ParseStreamFrameHeader` does not
  reject `Keyframe=false` and `onKeyframe` caches whatever arrives as the
  join-priming keyframe. A delta sent that way to a current relay would
  overwrite the priming cache. The carrier type avoids that trap entirely.

### D2 — The relay: accept carriers from a publisher, behind a capability bit

- **Capability.** `RelayCapabilities` (0x0F) gains
  `CapUplinkCarriers = 1 << 2` — capability growth is new bits in the flags
  word, never new bytes (the rule in `server.go`). A broadcaster opens
  carriers **only** after it has seen the bit. A relay without the bit
  treats a `0x0A` stream from a publisher as bad input (`countBad` +
  `CancelRead`) today, so the gate is not optional.
- **Acceptance.** The publish session's uni-stream accept loop recognises the
  `0x0A` prologue and hands the stream to a carrier reader, separate from
  `acceptKeyframeStreams` and outside its `maxConcurrentKeyframeStreams = 4`
  budget. At most **2** carriers per publisher at once (the current GOP and
  the one being drained); a third is `CancelRead`, counted.
- **Ingest.** Each record is validated as a datagram would be (type,
  version, `MaxDatagramSize`) and passed to `Publisher.HandleDatagram`. A
  malformed record ends the carrier (`CancelRead`) and is counted; earlier
  records have already been forwarded.
- **Staleness.** A record whose `frameID` precedes the most recent keyframe
  the relay has ingested is dropped and counted rather than forwarded: its
  GOP is over, and forwarding it spends leg-B bandwidth on frames every
  viewer will discard.
- **The knob.** `-uplink-carriers` (default **true** once WU1 ships; the
  flag exists so an operator can turn it off), `GAWK_UPLINK_CARRIERS`, Helm
  value `config.uplinkCarriers`, plumbed through `registryOptions` in
  `cmd/gawk-server/main.go` and asserted by `TestRegistryOptionsCarryAllLimits`
  — the CLAUDE.md invariant.
- **Flow control.** Sized from time and a maximum bitrate, not from a GOP
  — D9 has the rule, the quic-go defaults it replaces, and the memory
  budget.
- **Cluster mode.** Only the origin ingests publishers, so edges are
  unaffected. The origin forwards to edges exactly as before.

### D3 — The broadcaster: carrier lifecycle and the deadline

In `crates/engine/src/sender.rs`, beside the keyframe writer:

- **Open** a carrier when a keyframe is sent (the new GOP starts); finish
  (`FIN`) the previous carrier once its last record is written.
- **Write** each delta's chunks, then its parity, as records, in order. The
  write is non-blocking from the encoder's point of view: records go into
  the carrier's queue and a writer task drains it, as the keyframe writer
  does.
- **The deadline.** If the carrier's oldest *unacknowledged* record is older
  than `uplinkDeadlineMs` (default **150**, OD3), the carrier is **reset**
  (`RESET_STREAM`, a new code `UPLINK_CARRIER_EXPIRED`). The GOP is lost
  from that point, exactly as a lost datagram loses it today. Later deltas
  of that GOP are discarded locally (they are undecodable without the lost
  one), and the next keyframe opens a fresh carrier. The deadline turns
  "reliable" into "reliable while it still matters". Because it never blocks
  the encoder and never outlives a GOP, "favour dropped frames over stalled
  playback" still holds — frames drop 150 ms later instead of immediately.
- **Why 150 ms.** At the measured 10 ms RTT, QUIC's loss detection plus
  retransmission is ~2–3 RTT per attempt, so 150 ms allows several attempts
  and still ends well inside a 500 ms GOP. The viewer's adaptive playout
  (docs/12) absorbs the occasional retransmit delay; G3 bounds what it adds.
- **Stream priority.** quinn packs datagrams ahead of *all* stream data
  (docs/38 F-12). With deltas on a stream that stops mattering for video,
  but the keyframe stream and the carrier now compete with each other. The
  keyframe stream gets the higher `set_priority`: the carrier's deltas are
  useless until their keyframe has landed.
- **Superseded GOP.** When a new keyframe opens a carrier while the previous
  one still has unacknowledged records, the old carrier keeps its deadline
  (it may still complete), but it never delays the new one.

### D4 — When the carrier engages: always, when the relay supports it

OD2. Engaged whenever the relay advertised `CapUplinkCarriers` and the
advanced setting is on its default. No RTT gate, no hysteresis, no policy to
flap: one rule, easy to reason about in telemetry.

**On a long path.** At 80–100 ms RTT a 150 ms deadline leaves room for
about one retransmission, so the carrier recovers less there than on a LAN.
It must still be **no worse** than datagrams: without loss it costs ≈ 0
(D9), and with loss it degrades to the datagram outcome at the deadline —
the GOP is dropped either way. G12 checks exactly that on a shaped path,
because this is the case the rejected RTT gate existed for.

**The escape hatch**, for when a network misbehaves in a way nobody
predicted:

- *Broadcaster*: Settings → Advanced → **Video delivery**:
  **Automatic (recommended)** · **Legacy**. Choosing Legacy shows, before it
  takes effect: *"Only change this if you've been asked to. Legacy delivery
  makes viewers see pauses whenever your network hiccups."* with **Use
  Legacy** · Cancel. The setting persists in `broadcast.json`
  (`uplinkDelivery: "auto" | "legacy"`), is shown in Diagnostics, and is
  reported in telemetry (`uplinkModeRequested`), so a support conversation
  can see it. It lives in the shared `main.slint` Settings card, so Windows
  gets it too, identically.
- *Relay*: `-uplink-carriers` / `GAWK_UPLINK_CARRIERS` / Helm
  `config.uplinkCarriers` (D2). Off means the capability bit is never sent
  and every broadcaster uses datagrams.

The user otherwise sees nothing. The mode is one row in Diagnostics, for
us, not in the main window (D7 principle 1).

### D5 — Telemetry: say which uplink ran and what it cost

- Broadcaster fields (added to the field registry, `gawk-telemetry/internal/schema`):
  `uplinkMode` (`datagram` | `carrier`), `uplinkCarriersOpened`,
  `uplinkCarriersExpired`, `uplinkRecordsDiscarded`, and
  `uplinkLossPct` — QUIC's own lost/sent packet ratio from quinn
  `ConnectionStats`. That last one is the first time the broadcaster can see
  leg-A loss **locally**, without a relay report.
- Relay `/statusz` per broadcast: `uplinkCarriers`, `uplinkRecords`,
  `uplinkRecordsStale`, `uplinkCarriersRejected`.
- A playbook row: leg-A loss with `uplinkMode=datagram` says "the carrier
  uplink would recover this — is the relay advertising it, or is the
  broadcaster set to Legacy (`uplinkModeRequested`)?".
- **Prerequisite**: the live view's windowed relay facts read zero today
  (BUGS.md, "Telemetry live view: every windowed relay fact reads zero"), so
  a live leg-A row cannot fire until that is fixed. WU0 fixes it first.

### D6 — Not a back-channel, and not docs/15 D6

docs/15 Decision 6 rejected a viewer → server keyframe request because it
produces **more** keyframes in the congested case. Nothing here requests a
keyframe or changes the cadence. The only new relay → publisher signal is one
capability bit on a message that already exists. D6's remark that the design
is one-way ("the relay never talks back to the broadcaster") was already
superseded by relay → publisher messages: R18's `ViewerCount` datagram, then
R28's `TelemetryHello` and R29's `RelayCapabilities` on server-opened uni
streams; this adds nothing to it. The NACK/ARQ-over-datagrams
rejections (docs/24, docs/26) are respected too: retransmission is QUIC's,
on a stream, not a gawk protocol.

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
   technical truth, for us.
4. **One action, and it's reversible without thinking.** Anything gawk turns
   off comes back by itself when the broadcast ends, whatever happens to the
   app.
5. **Never interrupt the game.** No system notification, no sound, no modal
   while live. The message lives in the gawk window; the user sees it when
   they look.
6. **Ask once.** A dismissed message stays dismissed for that broadcast.
   Dismissed in two broadcasts in a row, it stops appearing and the option
   lives only in Settings.
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
| Settings → Advanced | **Video delivery**: Automatic (recommended) · Legacy, with D4's warning before Legacy takes effect. |
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

**What the carrier costs** (estimated from the protocol; WU0/WU6 measure it):

| | No loss | During loss | Scales with |
|---|---|---|---|
| Bandwidth | ≈ +0.5–1 %: a 2-byte record length plus QUIC's stream-frame header (stream id, offset, length) — ~6–10 B per ~1200 B packet | plus the retransmitted bytes, ≈ the loss rate | bitrate × loss |
| Delay | ≈ 0 — records are written as encoded and forwarded as read, no per-frame store-and-forward | one lost packet: ≈ 1.5 × RTT (≈ 15–20 ms at 10 ms); an AWDL absence: its length (published 50–100 ms) + RTT; worst: the 150 ms deadline, then the GOP is dropped as today | outage length, link headroom |
| CPU, broadcaster | negligible — quinn tracks stream offsets instead of firing-and-forgetting | retransmission | packets/s |
| CPU, relay | one stream read + record split, then the same `HandleDatagram` as today | none extra | packets/s |
| Memory, broadcaster | unacknowledged bytes ≈ bitrate × RTT (≈ 15–25 KB at 12 Mbps) | ≤ bitrate × deadline (≈ 225 KB at 12 Mbps, ≈ 940 KB at 50 Mbps) | bitrate × deadline |
| Memory, relay | ≈ 0 — in-order records are consumed immediately | out-of-order data held behind a hole until it fills, ≤ bitrate × deadline per carrier | bitrate × deadline × publishers |

A lost packet also bumps the chunk's frames by the recovery time; later
deltas of the GOP arrive behind it and are then presented back to back.
Viewers in the default live-edge playout (`playout.ts` mode `off`) see a
brief hitch and no lasting latency; viewers in **adaptive** playout (target =
p95 arrival jitter + 34 ms, slewing up 50 ms/s and down 5 ms/s after 15 s)
will carry a buffer about one recovery time larger for as long as loss
persists. G3 bounds both.

**The tradeoff.** Gained: a loss costs a bounded delay instead of the rest
of the GOP — and that gain grows with bitrate, because frames get longer. At
0.2 % packet loss a 12 Mbps frame (~21 packets at 60 fps) arrives intact 96 %
of the time; a 50 Mbps frame (~87 packets) 84 % — datagram delivery over Wi-Fi
is effectively unwatchable there. Given up:

1. *Freshness during an outage.* Datagrams deliver whatever is newest when
   the radio returns; a stream drains its backlog in order first. The
   deadline caps how stale that backlog can be.
2. *Loss becomes queueing.* Loss-based congestion control (quinn's Cubic)
   cuts its window ~30 % on Wi-Fi loss. For datagrams that surfaces as drops
   in quinn's datagram buffer; for a stream it surfaces as queued data — and
   the deadline converts a queue that lasts too long back into a drop.
3. *Complexity*: a second ingest path, a mirrored capability bit, deadline
   and priority logic in the engine — contained by G5's byte-identical-when-off
   rule.

**Headroom, not the deadline, is the limit at high bitrate.** After an
outage the backlog drains through the link's spare capacity:

```
recovery ≈ outage × (1 + bitrate / (link capacity − bitrate))
```

| Bitrate | Link goodput | Backlog after an 80 ms absence | Drain | Recovery |
|---|---|---|---|---|
| 12 Mbps | 300 Mbps | 120 KB | ~3 ms | ~85 ms — inside the deadline |
| 50 Mbps | 300 Mbps | 500 KB | ~16 ms | ~100 ms — inside |
| 50 Mbps | 80 Mbps | 500 KB | ~130 ms | ~210 ms — expires |

On a link without headroom the carrier cannot save a high rung: every
outage expires a GOP. That is the D7 status line's case exactly ("Your Wi-Fi
is dropping some video"), and the honest remedies are the same — a cable,
Improve, or a lower quality setting. The carrier never queues unboundedly
(G11).

**Flow-control windows: sized from time and a maximum bitrate.** The relay
sets no windows today (`internal/transport/server.go` builds one
`quic.Config` for every connection), so quic-go v0.62.0's defaults apply: a
stream starts at **512 KB** and auto-tunes up to **6 MB**; a connection starts
at 768 KB and tunes up to **15 MB**. At 50 Mbps one deadline's worth of data is
≈ 940 KB — above the initial stream window, and above the initial
**connection** window too, which the keyframe stream (0.5–1 MB at that rate)
shares. quic-go sets the connection window from its own default, not from
the stream window, and only raises it to 1.5 × the stream window when
auto-tuning runs. So both must be set, or the first GOPs of a broadcast
stall until auto-tuning catches up. The rule:

```
stream window     ≥ uplinkMaxBitrate × (deadline + RTT) × 2
                  = 50 Mbps × (150 + 10) ms × 2            ≈ 2 MB
connection window ≥ stream window + one keyframe at that rate
                  ≈ 1.5 × stream window (quic-go's own ratio) ≈ 3 MB
```

set as `InitialStreamReceiveWindow` and `InitialConnectionReceiveWindow` on
the relay's `quic.Config`, both derived from a new knob
**`-uplink-max-bitrate`** (default 50 Mbps; `GAWK_UPLINK_MAX_BITRATE`, Helm
`config.uplinkMaxBitrate`, plumbed through `registryOptions`). The maxima
stay quic-go's (6 MB / 15 MB), so the memory ceiling below is unchanged. A window is an allowance, not an allocation — the
relay only holds bytes that actually arrive out of order — so raising the
initial window costs nothing on a clean link, including for viewer
connections that share the config.

**Relay memory budget.** A publisher can hold relay memory by leaving holes
on purpose, up to its connection's receive window (15 MB). The ceiling it can
reach **today** is already higher than that: keyframe ingest is
store-and-forward (`IngestKeyframeStream` reads the whole frame), so four
concurrent keyframe streams at `MaxKeyframeBytes` (8 MiB) can pin ~32 MiB of
application memory besides the window. Carriers add nothing to either: their
records are forwarded as read (no application-side buffering), and their
out-of-order bytes live inside the same connection window. So the pod's
worst case per publisher does not rise, and docs/07's caps and the pod
memory limit are what already have to cover it. WU1 measures it with a
hostile-publisher test rather than trusting the arithmetic.

**Keyframes at high bitrate.** A 50 Mbps keyframe can be 0.5–1 MB. The
keyframe stream's priority over the carrier (D3) means the next GOP's deltas
wait for it — correct, since they are undecodable without it — and F-12's
2 s in-flight rule already covers a keyframe that cannot finish.

## 5. Architecture

```
 broadcaster (Rust engine)                          relay (origin)
 ────────────────────────                          ──────────────
 keyframe ──► StreamFrame uni stream (R8, F-12) ──► acceptKeyframeStreams ──► onKeyframe (cache, DVR, fan-out)
 deltas   ──► carrier uni stream per GOP       ──► acceptUplinkCarrier (new)
               0x01 0x0A ‖ len ‖ dgram ‖ …           └─ per record ─► Publisher.HandleDatagram ─► (unchanged)
               reset at uplinkDeadlineMs
 audio    ──► datagram (unchanged)             ──► HandleDatagram
 (carrier not engaged: deltas as datagrams, exactly as today)
```

## 6. Chunks and acceptance criteria

### WU0 — Baseline, and the telemetry that can see it

| Acceptance criterion | Verified by |
|---|---|
| The BUGS.md double-append in `relayscrape.ScrapeOnce` is fixed test-first; the live view shows non-zero `framesRelayed` and a `framesRelayedPerSec` | unit (red first) + live dashboard |
| Leg-A loss is reported in **frames**, counting partly received ones: the relay's ingress window gains `IngressFramesDamaged` (frames seen with ≥ 1 chunk missing at `finalize`), and telemetry derives `ingressFrameLossRatio = (IngressFramesLost + IngressFramesDamaged) / frames expected` (the window's frameID span). `IngressChunksLost` stays a chunk count, reported as `ingressChunkLossRatio` over chunks expected — one unit per ratio, never summed. The leg-A playbook row reads the frame ratio | unit (red first: a window with one fully lost and one partly received frame reports 2 damaged frames, not 1 + missing-chunk count) |
| The baseline is recorded in §8: 10 min each, the reference Mac, datagram mode, AWDL on / AWDL off / Ethernet — leg-A frames and chunks lost, viewer resyncs, `capToRenderMs` p50/p95 | manual, recorded |

### WU1 — Relay: carrier ingest behind `CapUplinkCarriers`

| Acceptance criterion | Verified by |
|---|---|
| `CapUplinkCarriers` allocated in `wire.go` and mirrored in `wire.ts`, `wirecheck`, `crates/wire`; golden vectors byte-identical | unit (mirror tests) |
| A carrier's records are ingested exactly as the same bytes sent as datagrams: same fan-out, same accounting, same DVR contents (property test over random frames) | unit, test-first |
| Without the capability configured, a publisher `0x0A` stream is rejected as today; with `-uplink-carriers=false`, `/statusz`, metrics and wire are byte-identical to pre-R55 (diff-asserted, the R28 pattern) | unit |
| Third concurrent carrier rejected and counted; malformed record ends the carrier, earlier records forwarded; stale record (pre-latest-keyframe) dropped and counted | unit |
| A full GOP at **50 Mbps** passes one carrier without a flow-control stall from the first GOP of a session (the initial window, not auto-tuning, covers it) | unit (real quic-go loopback) |
| `-uplink-max-bitrate` / `GAWK_UPLINK_MAX_BITRATE` / `config.uplinkMaxBitrate` derives **both** `InitialStreamReceiveWindow` and `InitialConnectionReceiveWindow` per D9 (≈ 2 MB / ≈ 3 MB at the default); `TestRegistryOptionsCarryAllLimits` grows the field | unit |
| Hostile publisher: a carrier with a deliberate hole holds at most the connection window of relay memory, and the carrier is dropped when the publisher's deadline would have expired it (`RESET_STREAM` or stale-keyframe rule) — measured, recorded in §8 | unit |
| `-uplink-carriers` / `GAWK_UPLINK_CARRIERS` / `config.uplinkCarriers` plumbed; `TestRegistryOptionsCarryAllLimits` grows the field; README flags table updated | unit + review |

### WU2 — Rust engine: carrier sender, deadline, engage policy

| Acceptance criterion | Verified by |
|---|---|
| Carrier opened per keyframe, FIN after the GOP's last record; records are byte-identical to the datagrams datagram mode would send (same frame) | unit, test-first |
| Deadline: with a fake transport that withholds acks, the carrier resets at `uplinkDeadlineMs`, later deltas of that GOP are discarded, the next keyframe opens a fresh carrier, the encoder is never blocked | unit (fake clock) |
| Engage policy: capability present and setting Automatic ⇒ carrier; capability absent or setting Legacy ⇒ datagrams; the setting takes effect at the next keyframe | unit |
| The Advanced → Video delivery setting: Legacy is applied only after the warning is confirmed; Cancel leaves Automatic; the value persists in `broadcast.json`, shows in Diagnostics and is reported as `uplinkModeRequested` | unit + manual |
| Keyframe stream priority above the carrier's; F-12's 2 s rule unchanged (its tests still pass) | unit |
| Against a real `gawk-server` with injected loss (the `vt_to_relay` harness): with 2 % random packet loss, carrier mode delivers every frame to a subscriber and datagram mode does not | integration, ignored-by-default like `vt_to_relay` |
| Outage injection at 50 Mbps: an 80 ms total blackout every second recovers inside the deadline with 300 Mbps of link capacity, and expires cleanly (no unbounded queue, next keyframe opens a fresh carrier) with 80 Mbps — D9's headroom table, measured | integration (shaped loopback) |
| Broadcaster memory for unacknowledged data stays ≤ bitrate × deadline + one GOP's keyframe under the outage test | integration |
| D5 broadcaster fields reported; the field registry and stored-shape golden updated | unit |

### WU3 — macOS: the quiet status line

| Acceptance criterion | Verified by |
|---|---|
| The D7 policy (Wi-Fi × harm sustained 10 s × viewers > 0 × dismissed × mode state) is a pure function with a table test; the platform probes only translate values | unit |
| Every D7 row renders with its exact copy; a string test fails if any **main-flow** string (status line, sheets, Settings incl. Advanced) contains a banned term (AWDL, channel, packet, uplink, QUIC, carrier, DSCP, Wi-Fi band); Help is checked for everything except router vocabulary; Diagnostics is not checked | unit |
| On clean Wi-Fi with AWDL up nothing appears; with injected loss the line appears within 15 s; Not Now holds for the broadcast; two consecutive dismissals stop it | unit (fake clock) + manual |
| No notification, sound or modal is raised by this feature while live | unit + review |
| Help page (README macOS section, linked from **?**) in D7's order: cable, Improve, router channel | review |
| Diagnostics shows uplink mode, loss %, carriers expired, AWDL state and channel | unit |

### WU4 — macOS: Improve (deferred — OD4 is decided after WU2)

Not started until WU2's carrier has been measured on the reference setup. If
residual AWDL harm with the carrier is within G1/G2, WU4 is dropped and D7's
no-button line stays; otherwise it is built to these criteria.

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
| G1–G4, G8, G10–G13 (and G9 if WU4 ships) pass on the reference setup (G11 on a 1440p60 source at 50 Mbps); the Windows app's G4 run on the gaming PC | manual |
| §8 records every measured number against the WU0 baseline | review |

## 7. Risks

- **Retransmit latency leaks into playout.** A retransmitted delta arrives
  one or two RTTs late, and the viewer's adaptive playout may grow its
  buffer to absorb that. G3 is the bound; if it fails, the deadline goes
  down before the design is changed.
- **Congestion control reacts to Wi-Fi loss.** Datagrams are congestion
  controlled in quinn too, so this is not new, but a stream makes the effect
  visible as queueing rather than loss (D9). The deadline caps how long a
  queue can hold a GOP. If WU6 shows Cubic's reaction to non-congestive Wi-Fi
  loss dominating, quinn's BBR controller is the next rung to evaluate — not
  a longer deadline.
- **High rungs on thin links.** Without headroom the carrier cannot help
  (D9); it must fail as cleanly as datagrams do, which G11 checks.
- **quinn's scheduling.** F-12 showed quinn's packet assembly can starve a
  stream. Moving deltas onto a stream removes the datagram flood that caused
  it, but WU2's integration test has to show the keyframe stream is not
  starved by the carrier either.
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

## 9. References

- [docs/24](24-viewer-network-resilience.md) — R19 carriers, the format reused here
- [docs/34](34-live-edge-forward-parity.md) — R29 parity; leg-A cleanliness; pacing rejected
- [docs/35](35-connection-interleaving.md) — R30, the leg-B burst threshold
- [docs/38](38-windows-native-broadcaster.md) — F-12, quinn's datagram-first packing
- [docs/15](15-viewer-live-edge.md) — Decision 6, the rejected back-channel
- [docs/39](39-linux-app-sharing.md) — the `gawk-pw-helper` crash-safety shape
- [docs/54](54-macos-native-broadcaster.md) — R52, where the finding came from
- `BUGS.md` — the live relay-facts bug WU0 fixes
