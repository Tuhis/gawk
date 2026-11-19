# R87 — Striped deep-buffer delivery (docs/91)

**Status**: designed 2026-11-19 from one field case (§2). **SG0's lab half
and its instruments were done 2026-11-19** (§11.1): in the lab a
per-connection ceiling is real, it is set by loss, and each extra connection
adds its own share; Firefox's per-stream window is not the limit up to
150 ms. **SG1–SG3 were implemented 2026-11-20** with SG4's browser e2e pass,
after the owner chose to build ahead of SG0's field half (OD1); §12 records
where the code differs from the plan. Open: SG0's field run and SG5, the
owner pass, both on the owner's mobile path.

**Scope**: the relay's DVR drain and stripe-leg negotiation
(`gawk-server/internal/hub`, `internal/transport`), one capability bit in
`gawk-server/wire` (mirrored in `wire.ts` and `crates/wire`), the web
viewer's deep-buffer path and its Striping setting (`gawk-app`), and
telemetry (`gawk-telemetry`: two fields, one new rule, one rule fix). No iOS
change: the iOS viewer has no DVR mode ([docs/67](67-ios-app.md) D13).

The *Most stable* preset carries a viewer's video from the relay's DVR ring
([docs/26](26-relay-dvr-buffer.md)) over **one** QUIC connection, one GOP
at a time. R30's stripe legs ([docs/35](35-connection-interleaving.md))
spread live-edge datagrams across up to four connections, and are refused
in reliable and DVR modes on the reasoning that "carriers retransmit, so
striping there is pure waste" (docs/35 §3). That reasoning holds for burst
loss, the problem R30 solved. It does not hold if a single connection's
**throughput** is capped below the stream bitrate while the link has room
to spare, and the field case below suggests exactly that.

This milestone serves a deep-buffer viewer's GOPs across several
connections: each GOP still travels whole, keyframe and records, but GOP
*k* goes to connection *h(k) mod N*. Each connection then needs 1/N of the
bitrate.

## 1. Why, and what "done" means

### 1.1 The problem in one line

A deep-buffer viewer on a mobile link received a 10.7 Mbit/s stream at
5.05 Mbit/s for 55 minutes. The relay jumped it to the newest keyframe 622
times, and a second connection from the same browser received exactly as
much again at the same time.

### Milestone acceptance criteria

| # | Goal | Verified by |
|---|---|---|
| G1 | We know which cause it is: a per-connection ceiling (§5 A), a per-stream window (§5 B), or neither. Nothing after SG0 is built without that answer | SG0's report against §9 |
| G2 | On a path whose single connection carries less than the stream bitrate but whose link carries more, a *Most stable* viewer plays with no DVR resyncs | SG4: the netem lab path, plus SG5 on the owner's mobile path |
| G3 | On a clean path nothing changes: no legs dialed, byte-identical relay behaviour, same latency | SG4 regression run; the relay's existing DVR tests pass unmodified |
| G4 | Striping never makes a viewer worse: if added connections don't raise the aggregate, the viewer stops adding them and drops back | SG2 unit tests on the growth rule; SG4's link-limited control path |
| G5 | Telemetry can tell undersupply apart from a per-connection ceiling, and no longer reports undersupply for every deep-buffer viewer | SG3's rule tests against the §2 session's recorded facts |

## 2. The field case (2026-11-19)

The owner was watching a room from a Windows laptop on a mobile connection
in Firefox 161, while also broadcasting into the same room. Everything below
comes from the relay's logs and Prometheus series and from gawk-telemetry's
recorded sessions.

**The stream.** A 1080p60 H.264 broadcast from the Windows native
broadcaster, about **10.7 Mbit/s**: 5.9 Mbit/s of deltas and 4.7 Mbit/s of
keyframes, with **a keyframe every 0.5 s** (GOP = 30 frames).

**Live mode, 16:14–16:41, striped ×4.** The burst-loss rule passed: R30 did
its job on the deltas. The viewer still saw only the keyframe cadence for
long stretches (`keyframe-only-delivery`, 2,560 reorder-gap resyncs). The
relay dropped **387 keyframes** for this subscriber, and keyframes ride the
primary connection's reliable streams, not the legs. About 4.7 Mbit/s on
one connection was not getting through.

**Deep-buffer mode, 16:41–17:36.** The viewer switched to *Most stable*:
two DVR subscriptions, the 1080p stream and a second room stream, each on
its own connection.

| Signal | Value | Source |
|---|---|---|
| Goodput, 1080p subscription | **5.05 Mbit/s** average over 54 min (`videoBytesReceived` 42 MB → 2,075 MB between t=69 s and t=3,287 s) | client |
| Goodput, the other subscription, same window | **5.05 Mbit/s** (107 MB → 1,834 MB between t=170 s and t=2,905 s) | client |
| Goodput, 1080p subscription after the other ended at 17:33:23 | **~5.2 Mbit/s** (17:32:43–17:35:54) | client |
| DVR resyncs | **622**, about 11 a minute, steady from the first 10 minutes to the last | relay |
| Frame rate | below half its baseline **58 %** of the window, 341 separate collapses, longest 36 s | client |
| RTT (TimeSync, primary) | 26–73 ms, median about 35 ms | client |
| Session end | `no keyframe for 8006ms while frames are still arriving` → reconnect | client |
| Relay-side drops, send errors, node UDP errors | **0** | relay, node exporter |

**What the numbers say.** Two connections on the same link held
5.05 Mbit/s each at the same time, so the link carried at least
10.1 Mbit/s plus the owner's own 3.8 Mbit/s upload. When one connection
ended, the other did not take up the freed capacity. A full link would give
the survivor most of the freed 5 Mbit/s. A ceiling per connection would
leave it where it was, and it stayed where it was.

**A mechanism that fits, unproven.** The relay is the sender, so the
congestion controller limiting the download is quic-go's, on the relay.
Loss-based congestion control at RTT *R* and random loss *p* settles near
`1.22 · MSS / (R · √p)` (Mathis et al.). At MSS 1,200 B and *R* = 35 ms,
5 Mbit/s corresponds to *p* ≈ 0.45 %: an ordinary loss rate on a mobile
radio link that isn't caused by congestion. Each connection backs off on its
own losses, so N connections get about N times the rate until the link
itself fills. This is the same reason parallel-TCP downloaders exist.

**What this case does not prove.**
- **The rise after one connection ended covers 2.5 minutes** and one
  sample. It is suggestive, not a measurement.
- **The loss rate was not measured.** No browser ships WebTransport
  `getStats()`, so `connection` stats are null on every client
  (`net-stats.ts`), and the relay exports no per-connection QUIC stats.
- **A per-stream receive window explains the same numbers**, and that
  would call for a different fix (§5 B). The DVR drain keeps one video
  carrier open at a time (`drainDVR`, `hub/dvr.go`). Throughput through one
  stream is bounded by its receive window over the RTT *under load*. A
  window of a few hundred KB at a few hundred milliseconds of queueing delay
  also gives about 5 Mbit/s, and TimeSync's RTT is measured on the
  primary's control traffic, not under the carrier's load.
- **The viewer reached the relay through the home network.** It appeared
  under a home-LAN source address, not its mobile one, so a tunnel may sit
  on the path. A tunnel puts every connection into one outer flow: a
  carrier that polices per flow would defeat striping, while a
  per-connection congestion ceiling would not.

SG0 exists to settle these before any of §6 is built.

**A side finding: a rule that cannot fail.** `resilient-undersupply`
(`gawk-telemetry/internal/rules/playbook.go`) fires when `playoutOffsetMs ≥
resilientClampMs − 50`, that is ≥ 1,950 ms. A deep-buffer viewer's playout
floor *is* its DVR buffer, 3,000 ms by default (`DVR_PLAYOUT_PROFILE`,
`playout.ts`), so the rule fires for **every** deep-buffer viewer, healthy
or not. It fired on the §2 session, and its verdict ("the link cannot carry
the stream bitrate") was the wrong conclusion. SG3 fixes it.

## 3. Owner decisions

| # | Question | Options | Recommendation | Decided |
|---|---|---|---|---|
| OD1 | Build before SG0, or let SG0 gate the build? | (a) SG0 first, kill criteria binding; (b) build A and measure on the result | **(a)**. Two designs fit the evidence and they share almost no code; R30 built ahead of its ST1 and its on-hardware proof is still pending (ROADMAP R30) | **(b)**, 2026-11-20: the owner chose to build A once SG0's lab half had ruled out B and the field case's two concurrent connections, through the owner's VPN, had each held their own share. §9's field criteria still apply to SG5 |
| OD2 | How is a GOP assigned to a connection? | (a) static hash of the keyframe's frameID, mod N; (b) a scheduler on the primary's pod; (c) work stealing between connections | **(a)**: no coordination, correct when legs land on other pods (D2) | built as recommended, with the build (2026-11-20); not separately decided |
| OD3 | When does striping engage? | (a) Auto: on measured undersupply, growing only while it helps; (b) always in *Most stable*; (c) only when switched On | **(a)**, with the existing Auto/On/Off setting enabled for *Most stable* (D10) | built as recommended, with the build (2026-11-20); not separately decided |
| OD4 | Does *Smoother* (plain reliable, no ring) get striping too? | (a) no, DVR only; (b) both | **(a)**. Without a ring a connection has no cursor to resume from, and docs/24's drop-at-GOP behaviour would have to be redone per leg. Revisit if SG0 shows *Smoother* viewers hitting the same ceiling | built as recommended, with the build (2026-11-20); not separately decided |

## 4. How SG0 measures it

Two instruments, both cheap, both kept afterwards.

1. **The relay's view of each connection.** Export quic-go's per-connection
   statistics (smoothed RTT, congestion window, bytes in flight, packets
   sent and lost) into `/statusz` `subscriberDetails`, per subscriber,
   behind the existing stats key. If quic-go's API in the pinned version
   doesn't expose congestion state, export what it has and record what is
   missing. This turns "a ceiling consistent with Mathis" into a measured
   cwnd and loss rate on the real path.
2. **A K-connection bench.** `e2e/dvr-throughput-profile.mjs`, alongside
   docs/35's `datagram-loss-profile.mjs`: from one browser it opens K
   deep-buffer subscriptions to one broadcast (K = 1, 2, 3, 4, each a
   separate WebTransport session, as docs/34 finding 5 did for datagrams)
   and reports per-session and aggregate goodput, resyncs and RTT. Run it
   on:
   - **the owner's mobile path**, against the live fleet with a 1080p60
     broadcast, with and without the tunnel if one is in use;
   - **two netem lab paths**: *per-connection-limited* (40 ms, 0.5 % random
     loss, 50 Mbit/s bottleneck) and *link-limited* (40 ms, 0 % loss,
     8 Mbit/s bottleneck). These are the controls for SG4.

   To separate §5 A from §5 B, the bench also runs K = 1 against a
   lab-only relay flag that lets the DVR drain keep two GOP carriers open
   at once on one connection. That's the cheapest test of whether a
   per-stream window is the limit.

   *Revised 2026-11-19, as built:*
   - **Instrument 1.** quic-go v0.62's `ConnectionStats` reports RTT, bytes,
     packets sent and packets lost, but not cwnd or bytes in flight.
     `/statusz` therefore carries `pathSrttMs`, `pathMinRttMs`,
     `pathBytesSent`, `pathPacketsSent` and `pathPacketsLost` per
     subscriber, and nothing for congestion state.
   - **Instrument 2** is `e2e/dvr-throughput-profile.mjs` (Firefox) plus
     `gawk-loadgen -session-csv` (quic-go's client), driven by
     `tools/dvrlab`. Relay and publisher share one container, so the
     uplink is never shaped. netem on the relay's egress shapes every
     viewer's download.
   - **The two-carrier flag was not built.** A per-stream receive window W
     caps one stream at W/RTT whatever the loss, so a loss-free sweep over
     RTT tests the same thing for free: if W were the limit, goodput would
     fall as the RTT grew (§11).

## 5. Design options

**A — Stripe GOPs across connections (§6).** The fix if the ceiling is per
connection. It reuses R30's legs, owner token, lease and make-before-break
transitions, with a different filter (GOP residue instead of chunk
residue) and a ring cursor per connection.

**B — Pipeline GOP carriers on one connection.** The fix if the ceiling is
per stream. `drainDVR` would open GOP *k+1*'s keyframe stream and carrier
while GOP *k*'s carrier drains, keeping up to two in flight as docs/24
Decision 5 already allows live reliable subscribers. No wire change and no
legs. It is a fraction of A's cost, and it is what SG0's two-carrier flag
tests. If SG0 points here, this doc is revised to B before anything is
built.

**C — Lower the bitrate.** Always available to the viewer and the
broadcaster: a lower rung, or a longer keyframe interval (§2's 0.5 s GOP
spends 44 % of the bitrate on keyframes). It is complementary, not a fix: a
path that carries 20 Mbit/s should not be held to 5 by how gawk uses it.

**D — Change the relay's congestion control.** The ceiling, if SG0 confirms
it, comes from quic-go's loss-based controller on the relay. A
loss-tolerant controller would raise it for every mode at once. Rejected
for this milestone: quic-go offers no pluggable controller, and gawk does
not fork quic-go. Recorded so it isn't re-derived.

## 6. Recommended design (option A)

### D1 — The unit is the GOP

A DVR subscriber is already served one GOP at a time, each on its own
keyframe stream and carrier (`drainDVR`, `dvrSendKeyframe`,
`dvrWriteRecord`). A GOP is independently decodable from its keyframe.
Striping at GOP granularity keeps every existing stream type, keeps each
GOP's records in order on one carrier, and leaves R30's chunk-ordinal
filter for live mode alone. Chunk-level striping of carriers was rejected:
it would interleave one GOP's records across connections, and the client
could not decode any of them until the slowest connection caught up.

### D2 — Assignment: a static hash of the keyframe's frameID (OD2)

GOP *k* belongs to connection `mix32(keyframeFrameID) mod N`, where
`mix32` is a fixed integer mixer (the murmur3 finalizer), pinned in all
three wire copies with golden vectors. *Revised 2026-11-20:* pinned in Go
only (`wire.StripeGOPResidue`, with golden vectors). Only relays compute
it; the client reorders whatever arrives and never needs a residue.
- **The publisher's frameID, not the ring's `gopSeq`**, because `gopSeq`
  is local to a pod and legs land on any pod in cluster mode (docs/35
  §5.7). Every pod sees the same frameID for the same keyframe.
- **Hashed rather than raw**, because a fixed GOP length makes
  `frameID mod N` collapse onto a few residues: 30 mod 4 alternates
  between 0 and 2.
- **What hashing costs:** short runs on one connection. With N = 2, three
  consecutive GOPs on the same connection happen a quarter of the time.
  That is 1.5 s of video, well inside the 3 s buffer.

A scheduler on the primary's pod (OD2 b) would balance better but needs
the legs on the same pod, or a cross-pod feed. Work stealing (OD2 c) needs
a feedback channel that docs/15 Decision 6 rules out in spirit. Both can
replace D2 later without wire changes beyond the capability bit's
semantics.

### D3 — Every connection is a full DVR subscriber over its residue

The primary and each leg get their own `DVRCursor`, their own `drainDVR`
loop and their own `dvrFellBehind` / `dvrResync`. A GOP outside a
connection's residue is skipped when the cursor reaches it. A connection
that falls behind resyncs alone, to the newest GOP *of its residue*, and
only its GOPs are lost. `DVRProgressTimeout` treats a skipped GOP as
progress, so a leg whose residue had no GOP for a while is not evicted.

### D4 — The catch-up pacer is shared out

`dvrPacer` caps a subscriber at `-dvr-max-catchup × LiveRateBps`. A striped
connection's pacer runs at that cap divided by N, so the group's catch-up
never exceeds what one unstriped viewer may take. The egress budget
(`consumeBandwidth`) and every docs/07 cap are unchanged. Legs already
count against subscriber caps (`hub.go`, R30).

### D5 — Audio, control and telemetry stay on the primary

The audio ring keeps its own carrier on the primary (docs/26 DV5). Its
skew throttle (`AudioSkewBudget`, 500 ms) runs against the primary's own
video cursor time: the connections advance through the same timeline at
the same rate, so the primary's cursor is a fair proxy, and the cost of
being wrong is the same audio ring overflow the throttle already handles.
DeliveryAck, capabilities, close notices and the telemetry hello stay on
the primary, as for R30 legs.

### D6 — The primary carries residue 0

In R30 the primary carries no deltas at all, so that every burst stays
short. In DVR mode there is no burst to protect, so the primary serves
residue 0 and legs `j = 1 … N−1` serve the rest. Striping ×2 therefore
costs one extra connection, not two.

### D7 — The primary's striped state rides StripeState (0x10)

`ApplyStripeState` stops returning early for a DVR primary that has an
owner. On a DVR primary, *striped* means "serve residue 0 of `StripeN`".
`StripeN` becomes authoritative there; it stays informational for live
mode, so the message format doesn't change. It keeps R30's level-state rule:
refreshed at 1 Hz, expiring after `StripeStateTTL` (5 s). An expired or
lost state sends the primary back to serving every GOP: duplicates, never
holes. The client deduplicates by frameID. The primary needs the owner
token, which `SubscribeDVR` does not receive today (`server.go`), so it
gets the same `owner` argument `SubscribeParity` already does.

### D8 — Negotiation: one capability bit

`CapStripedDVR = 1 << 4` in `RelayCapabilities`, appended per the
append-never-renumber rule. A relay that sets it:
- accepts a leg dial that carries `delivery=reliable&buffer=…`;
  `NegotiateStripe` keeps refusing plain reliable legs (OD4);
- applies D2's filter to it and gives it a DVR cursor.

A relay without the bit still answers 400, as it does today, and a client
never dials DVR legs without the bit. The leg dial URL is the primary's
plus `stripe=N&leg=j&owner=…`, which `dialLeg` already builds.

### D9 — The client reorders GOPs against the playout deadline

The resilient reorder buffer orders by frameID (`reorder-buffer.ts`). Two
things change in striped DVR mode:
- **GOP *k+1* can arrive entirely before GOP *k*.** The buffer holds a
  complete later GOP while an earlier one is still arriving.
  `RESILIENT_MAX_BUFFERED_FRAMES` (256) covers 4.3 s at 60 fps, which is
  more than the 3 s buffer.
- **A missing GOP is waited for until its playout deadline**, not for
  `RESILIENT_DELTA_GAP_GRACE_MS` (250 ms). That constant exists for
  stragglers within one connection's carrier rotation. Across connections a
  GOP can trail by much more and still be on time. When the deadline
  passes, the buffer skips to the next complete keyframe, and a GOP that
  arrives after that is dropped.

### D10 — Engagement: Auto grows only while it helps (OD3)

The Advanced *Striping* setting (Auto/On/Off, `playbackPresets.ts`) is
enabled for *Most stable* when the relay sets `CapStripedDVR`.
- **When Auto engages.** It engages ×2 when either of these holds for 10 s:
  - complete-GOP goodput stays below 90 % of the stream's live rate. The
    live rate comes from the stream's own frame timestamps and sizes, so no
    new signal is needed.
  - the client sees a relay resync, a jump to a newer keyframe with a GOP
    gap.
- **Growing:** it grows by one leg, up to `MaxStripeLegs` (4), only while
  the previous step raised aggregate goodput by at least 20 %.
- **Stepping back:** if a step does not help, it returns to the previous
  width and holds there for the session.

This rule is G4 in code: on a full link, extra connections take nothing
more, so Auto stops at ×1 or ×2. It is also the production version of
SG0's discriminator: in telemetry, "engaged, grew, helped" against "engaged,
didn't help" counts per-connection ceilings in the field. *On* engages ×2
at join and grows by the same rule. *Off* never stripes.

### D11 — Transitions reuse R30's make-before-break

`transitionStripe` dials the new set of legs before sending the new
StripeState. While the set changes, a GOP's owner can change between the
old N and the new N. The worst case is one GOP served twice (dropped by
frameID) or one GOP served by nobody (skipped at its deadline, D9). SG4
counts both.

### D12 — Limits that don't change

`MaxStripeLegs` stays 4: it is a constant bound to docs/34 finding 5, not
a knob. The leg lease (20 s), owner reap and close code 4005 are reused
unchanged. The number of bytes the relay sends is unchanged; it is only
spread over more connections. Opening streams spreads too: BUGS.md's ~4
uni streams per second per deep-buffer viewer become ~4/N per connection.

## 7. What users see

- **Nothing on a good path.** Auto never engages, and the overlay shows
  what it shows today.
- **On a constrained path,** the overlay's delivery line reads
  `ring-backed (buffer 3000 ms) · striped ×N`, the stripe width R30 already
  shows for live mode.
- **The Striping setting is enabled for *Most stable*** when the relay
  supports it. Its hint changes from "Not used in this mode" to "Spreads
  the stream over several connections when one can't carry it."

## 8. Chunks and acceptance criteria

Order: SG0 → (decision per §9) → SG1 ∥ SG2 ∥ SG3 → SG4 → SG5.

### SG0 — Measure the ceiling

| Acceptance criterion | Verified by |
|---|---|
| `/statusz` `subscriberDetails` carries per-connection smoothed RTT, cwnd (if quic-go exposes it), bytes sent, packets sent and lost; documented in docs/13's playbook | relay unit test on the JSON shape; one field read on the reference deployment |
| `e2e/dvr-throughput-profile.mjs` reports per-session and aggregate goodput, resyncs and RTT for K = 1–4 deep-buffer sessions from one browser | a run on the dev stack |
| Runs recorded on the owner's mobile path and both netem paths (§4), including K = 1 with the two-carrier flag | the report appended to this doc as §11 |
| A verdict against §9: A, B, or stop | this doc revised in place, with a dated note |

### SG1 — Relay

| Acceptance criterion | Verified by |
|---|---|
| `CapStripedDVR` is advertised when striping and DVR are both enabled; a DVR leg dial is accepted only then; plain reliable legs are still refused | `NegotiateStripe` table tests, including today's "reliable combination" and "dvr combination" rows, inverted only for DVR |
| Each connection serves exactly its residue's GOPs, keyframe and records, in order; across N connections every GOP is served exactly once while StripeState is fresh | hub test: N cursors on one ring, 600 GOPs, the union equals the ring and the pairwise intersections are empty |
| StripeState expiry sends the primary back to every GOP; no GOP goes unserved across an expiry | hub test with a stopped refresh |
| A leg that falls behind resyncs alone and the other connections keep their cursors | hub test: one leg's writes blocked past the buffer |
| The pacer share is `max-catchup × live / N`; the group's catch-up egress never exceeds one unstriped subscriber's | pacer test |
| A leg on another pod serves the same GOPs it would on the primary's pod | e2e-cluster: primary and leg pinned to different pods, frameIDs compared |
| Existing DVR, R30 and R19 tests pass unmodified | CI |

### SG2 — Web viewer

| Acceptance criterion | Verified by |
|---|---|
| The viewer dials DVR legs only with `CapStripedDVR`, only in *Most stable*, only when Striping is not Off | unit tests on the gate (`viewer.ts`, `stripe.ts`) |
| The reorder buffer plays GOPs in frameID order when a later GOP completes first, waits for a missing GOP until its playout deadline, then skips, and drops a GOP that arrives after the skip | reorder-buffer unit tests with interleaved GOP arrival |
| Auto engages, grows and steps back as D10 says | unit tests against scripted goodput series: a per-connection-limited series grows to ×3; a link-limited series stays at ×1 or returns to it |
| Duplicates from StripeState expiry or transitions are dropped by frameID | unit test |
| The overlay and the Advanced setting match §7 | component tests |

### SG3 — Telemetry

| Acceptance criterion | Verified by |
|---|---|
| `resilient-undersupply` compares the offset against the mode's own clamp (`dvrBufferMs` for DVR) plus a cadence condition; it does not fire on a healthy deep-buffer session | rule tests: a recorded healthy DVR session (silent) and the §2 session (fires on goodput, not on the offset) |
| The client reports goodput per connection (`connectionBytesReceived`, primary first) and the stripe width over time in DVR mode (`dvrStripeWidth`) | ViewerStats unit test; ingest round trip |
| A new rule, `per-connection-ceiling`, fires when the aggregate rose with width while each connection's goodput stayed flat, or when the relay's per-connection stats show a loss-limited sender (loss present, `pathSrttMs` within 25 % of `pathMinRttMs`: §9.3, since quic-go exposes no cwnd) | rule tests on synthetic facts for both signatures |

### SG4 — End to end

| Acceptance criterion | Verified by |
|---|---|
| On the per-connection-limited netem path, a 1080p60 *Most stable* viewer shows 0 resyncs over 10 minutes with Auto; the unstriped control resyncs | e2e run, paired |
| On the link-limited path, Auto ends at ×1 or ×2 and the viewer is no worse than the control | e2e run, paired |
| On a clean path no leg is dialed and `/statusz` matches the unstriped control | e2e run |
| Width transitions under load lose at most one GOP each | e2e: scripted ×2 → ×3 → ×2 while counting GOPs |

### SG5 — The owner pass

| Acceptance criterion | Verified by |
|---|---|
| The §2 path (mobile, Firefox, 1080p60 in *Most stable*) plays without DVR resyncs, or the remaining resyncs are explained by the link's total capacity | the owner, with the telemetry session linked here |

## 9. Kill criteria (registered before SG0 runs)

Read from SG0's runs on the owner's mobile path, with K = 1–4 deep-buffer
sessions of a 1080p60 broadcast.

1. **Stop**, and record why, if the aggregate goodput at K = 2 is below
   1.5× the K = 1 goodput. Extra connections don't buy throughput there,
   so neither A nor B can help on that path.
2. **Revise to option B** if K = 1 with two carriers in flight reaches at
   least 1.5× plain K = 1. In that case a per-stream window is the limit,
   and pipelining is the cheaper fix.
3. **Build option A** if (1) and (2) both fail to apply and the relay's
   per-connection stats show a loss-limited sender: cwnd well below the
   bandwidth-delay product needed for the stream, with loss present.
   *Revised 2026-11-19, before any field run:* quic-go exposes no cwnd
   (§4). The test is now loss present while `pathSrttMs` stays within 25 %
   of `pathMinRttMs`. That is the loss-limited signature §11 measured: a
   full link instead pushes RTT up 4–5× and loses packets in its queue.
4. **For (2), the loss-free RTT sweep replaces the two-carrier flag (§4).**
   Option B applies if a loss-free path holds K = 1 below the stream
   bitrate at an RTT the field path actually shows.
5. **Check the tunnel before deciding.** If the owner's path goes through a
   tunnel, run (1) both through it and outside it. If striping helps only
   outside the tunnel, A still ships, and the doc records that
   tunnel-wrapped viewers don't benefit.

## 10. Risks, and out of scope

- **Fairness.** N connections claim N shares at a shared bottleneck. This
  is bounded: the group never sends faster than the stream plus its
  catch-up share (D4), and Auto stops adding connections that don't help
  (D10).
- **Cost on the relay.** Up to four DVR cursors and drain goroutines per
  constrained viewer, all reading one shared ring. Per-connection overhead
  is the same as R30 legs, which already count against the subscriber
  caps.
- **Flow-level policing.** A carrier that polices per user rather than per
  flow defeats this, as does a tunnel. SG0 (§9.5) checks it on the one
  path we have.
- **Out of scope:** iOS (no DVR mode), *Smoother* (OD4), relay congestion
  control (§5 D), adaptive bitrate.

## 11. Measurements

### 11.1 SG0 lab, 2026-11-19

**Setup** (`tools/dvrlab`, rerun with `build.sh` then `matrix.sh`):
- **Relay:** the branch's relay with per-connection stats, quic-go v0.62,
  stock Cubic.
- **Stream:** the §2 shape at 10.7 Mbit/s with a 0.5 s GOP (`gawk-pubsim
  -pad-*-bytes`).
- **Viewers:** deep buffer at 3000 ms.
- **Paths:** netem on the relay's egress only, so "delay" is the whole RTT.
- **Clients:** quic-go's client (`gawk-loadgen`, one connection per session)
  and Firefox 159 headless (`e2e/dvr-throughput-profile.mjs`, Playwright
  1.65.1 image).
- **Window:** 90 s runs (60 s for the RTT sweep), goodput over 20 s to the
  end.
- **Columns:** "relay RTT/loss" is the relay's own `pathSrttMs` and
  `pathPacketsLost/pathPacketsSent` per connection, from `/statusz` halfway
  through the run. Resyncs are the relay's DVR resyncs during the run.

| Path | Client | K | Goodput per connection, Mbit/s | Total | Resyncs | Relay RTT ms / loss % |
|---|---|---|---|---|---|---|
| 40 ms, clean | quic-go / Firefox | 1 | 11.24 / 11.24 | 11.24 | 0 / 0 | 40 / 0 |
| 40 ms, 0.2 % loss | quic-go | 1 → 4 | 11.31 · 11.2–11.3 · 11.2 · 11.0–11.3 | 11.3 → 44.8 | 0 | 41–47 / 0.16–0.25 |
| 40 ms, 0.2 % loss | Firefox | 1, 2 | 11.29 · 11.16 11.23 | 11.3, 22.4 | 0 | 41 / 0.2 |
| 40 ms, 0.5 % loss | quic-go | 1 → 4 | 8.33 · 8.28 8.57 · 8.0–8.6 · 8.3–9.1 | 8.3 → 16.9 → 25.0 → 34.9 | 8 → 28 | 41 / 0.4–0.56 |
| 40 ms, 0.5 % loss | Firefox | 1, 2 | 8.61 · 8.79 8.09 | 8.6, 16.9 | 7, 16 | 41 / 0.45–0.53 |
| 40 ms, 1 % loss | quic-go | 1 → 4 | 6.11 · 5.77 6.09 · 6.1–6.3 · 5.8–6.3 | 6.1 → 11.9 → 18.6 → 24.3 | 15 → 61 | 41 / 0.87–1.07 |
| 40 ms, 1 % loss | Firefox | 1, 2 | 5.89 · 6.19 6.26 | 5.9, 12.5 | 16, 29 | 41 / 0.88–1.06 |
| 40 ms, 8 Mbit/s link | quic-go | 1 → 4 | 7.59 · 3.63 3.96 · 2.4–2.8 · 1.6–2.1 | **7.6 flat** | 11 → 107 | **152 → 220** / 0.06 → 1.95 |
| 75 ms, clean | quic-go / Firefox | 1 | 11.24 / 11.24 | 11.24 | 0 | 76 / 0 |
| 150 ms, clean | quic-go / Firefox | 1 | 11.24 / 11.24 | 11.24 | 0 | 151–152 / 0 |

**What the lab shows:**
1. **A per-connection ceiling, set by loss.** At the same 40 ms, one
   connection carries the whole 11.3 Mbit/s stream at 0.2 % loss (its
   ceiling is above the stream), but gets 8.4 Mbit/s at 0.5 % and 6.0 at
   1 %. Halving the loss from 1 % to 0.5 % raises the ceiling 1.4×, the
   `1/√p` shape of §2. §2's 5.05 Mbit/s sits just below the 1 % row, so the field
   path is consistent with roughly 1–1.5 % random loss at ~35 ms.
2. **Every added connection brings its own share.** At 1 % loss the total
   grows 6.1 → 11.9 → 18.6 → 24.3 Mbit/s over K = 1 → 4 while each
   connection stays at ~6. Two connections at the field's ceiling carry
   §2's 10.7 Mbit/s stream with room to spare.
3. **On a full link, connections add nothing**, and the relay can tell:
   - the total stays at 7.6 Mbit/s whatever K is;
   - the relay's RTT rises from 40 to 150–220 ms as the queue fills, and
     its loss comes from the queue.

   Under random loss, RTT stays at its minimum. This is the discriminator
   §9.3 now uses. The field case's TimeSync RTT also stayed at 26–73 ms on
   the loaded connection, which fits loss more than a full link.
4. **Firefox matches quic-go's client** everywhere, because the relay's
   Cubic is the sender either way.
   - **Separate connections:** Firefox opens every WebTransport session on
     its own QUIC connection (distinct source ports in the relay's log), so
     legs are real extra connections in Firefox.
   - **Per-stream window not a limit:** a single Firefox connection carries
     the full stream loss-free at 75 and 150 ms. So W ≥ 11.24 Mbit/s ×
     150 ms ≈ 210 KB, and Firefox's per-stream window isn't the limit at
     any RTT the field case showed (§9.4: option B does not apply).

**What is still open:** the kill criteria are read on the owner's mobile
path (§9). That run doesn't need a deploy for goodput. On the live fleet,
open the same 1080p broadcast in *Most stable* in one, then two, then three
Firefox tabs on the mobile link, three minutes each, and read each
session's `videoBytesReceived` from telemetry. The relay-side RTT and loss
(§9.3) need this branch's `/statusz` change deployed first. The tunnel
question (§9.5) has to be answered first.

## 12. Where the code differs from the plan (2026-11-20)

**Relay (SG1)**
- **Leg numbering in deep mode.** The primary serves share 0 and legs are
  `leg=1…N−1` (D6). The relay answers 400 to `leg=0` with a buffer, and to
  any plain reliable leg. A deep-mode leg whose buffer would not negotiate
  the ring is also refused, rather than served in another mode.
- **The knob.** `-striped-dvr` / `GAWK_STRIPED_DVR` / Helm
  `config.stripedDvr`, on by default. It takes effect only with
  `-striped-delivery`, and the relay advertises `CapStripedDVR` (`1 << 4`)
  only when both are on.
- **A GOP whose keyframe does not parse** has no frameID, so it counts as
  share 0 and the primary serves it. No connection loses it.
- **D3, resync.** A connection that falls behind lands on the newest GOP and
  then skips forward to the next GOP of its own share. It does not jump back
  to an older one of its share, so the lag check cannot loop. It costs at
  most one more of that connection's GOPs.
- **D3, eviction.** A skipped GOP counts as idleness, so a leg with nothing
  to send is not evicted. The exception is a connection whose last write
  failed: there a skip does not refresh the progress timer, or a dead leg
  that keeps resyncing onto other connections' GOPs would never be evicted.
- **D7, the primary filters only GOPs that reached the ring after its
  current striped period began.** A period starts at an arm, at a re-arm
  after expiry, and at a width change. A primary usually engages seconds
  behind live, while a new leg starts at the newest GOP. Filtering the GOPs
  in between would leave several of them served by nobody. With this rule,
  engaging or widening costs some duplicate GOPs and never a gap.
- **D4, a pacer fix that was latent before R87.** A write larger than the
  whole token-bucket burst now passes once the bucket is full, as debt
  repaid at the ceiling's rate. Before, any keyframe larger than a quarter
  of a second at the ceiling wedged the cursor forever. That was possible
  unstriped, and dividing the rate by N made it likely, for example a
  low-motion 2 Mbit/s stream with a 2 s GOP at N = 4. Found test-first.
- **Counters.** `dvrSubscribers` and `reliableSubscribers` count viewers,
  so they exclude legs. `dvrResyncs` includes leg resyncs, and
  `gawk_broadcast_stripe_legs` counts deep-buffer legs.
- **Cluster.** An in-process two-pod test checks that legs on another pod
  serve their own share. The real e2e-cluster run, with the primary and a
  leg pinned to different pods, has not been done.

**Web viewer (SG2)**
- **The gate.** A session stripes only when all four hold: it is a
  deep-buffer session, the relay advertised `CAP_STRIPED_DVR`, the
  DeliveryAck granted the ring, and Striping is not Off.
- **D9, as built.** Once legs have been requested, the reorder buffer keeps
  its striped behaviour for the rest of the session:
  - It waits for a missing frame until the frame held right after the gap
    is due, and never less than the old 250 ms grace. Then it skips to the
    next keyframe by frameID and counts a hole skip.
  - A keyframe behind the decode position, within the playout offset plus
    5 s of media time and 2,048 frames, is a late GOP and is dropped, not a
    broadcaster restart.
  - The reassembler drops duplicates through a set of the last 512 frames
    it emitted, instead of dropping late deltas against its watermark.
- **D10, as built.** The policy is a pure module (`dvr-stripe.ts`). Its
  constants, none of which D10 named:
  - Undersupply means complete-*frame* goodput (not GOP goodput) below 90 %
    of the live rate, measured over 2 s, or a hole skip in the last 10 s.
    It must hold for 10 s before ×2 engages.
  - The live rate is bytes over media time across runs of consecutive
    frames between keyframes. A skipped GOP cannot drag the live rate down
    to the goodput.
  - A step is judged 3 s after its legs are up, by mean goodput over 10 s
    against the 10 s before the step.
  - The policy grows only while still undersupplied. A step whose legs are
    not up within 10 s counts as not helping. After a leg dies, it waits
    5 s and asks for the same width again.
  - *On* never drops below ×2.
- **The resync signal is the client's own hole-skip count.** In striped
  mode it also counts GOPs that nobody served during a width transition.
- **Stats.**
  - `dvrStripeWidth` is the width the transport actually has up.
  - `connectionBytesReceived` has one slot per connection. A replacement
    leg keeps adding to its slot, and the array's length always equals the
    width.
  - Also added: `stripeDvrCapable`, `dvrStripePhase` (`held` means a step
    was undone), `dvrDuplicateFrames` and `dvrLateFramesDropped`.
- **The undeliverable-video watch** counts only the primary's keyframes. The
  notice is about the primary, and leg keyframes arrive out of order.
- **Known risk.** A broadcaster restart could be read as a late GOP and
  freeze playback for up to the window above. That needs both an old
  session under about 34 s old and a new timeline that starts within about
  8 s behind the old one.

**Telemetry (SG3)**
- **`resilient-undersupply`** compares the offset against the mode's own
  clamp. That is 2,000 ms for reliable, and `max(2000, dvrBufferMs)` for
  DVR, with the relay's buffer, then the client's, then 3,000 ms as the
  source. It also requires goodput below 0.75 × the stream bitrate. The
  reliable mode had the same flaw in a milder form: its stated
  "while cadence stays bad" was never checked.
  - Without relay coverage, or without 10 s of goodput span, the rule is
    unavailable rather than firing.
  - When the path is loss-limited, its verdict says "on this connection, not
    a full link".
- **The stream bitrate** is derived from the relay's ingress byte counters,
  which `relayscrape` now mirrors. Nothing exported one before. It includes
  audio, parity and framing, hence the quarter margin.
- **`per-connection-ceiling`** needs the same goodput shortfall, and then
  one of two signatures:
  - (a) relay loss ≥ 0.1 % with `pathSrttMs` ≤ 1.25 × `pathMinRttMs`
    (confidence 0.8);
  - (b) a striped session whose aggregate at its widest stripe is at least
    1.5 × its width-1 goodput, while its slowest connection kept at least
    0.75 × the width-1 rate (confidence 0.6).

  Both together give 0.9. A full link (RTT 4–5× its minimum) and a stripe
  that split one total stay silent. Signature (a) reads only the primary's
  path for now.

**End to end (SG4)**
- **Tier-1 browser e2e.** A `deep-striped` pass: Most stable with Striping
  On on the clean loopback, asserting the capability, width ≥ 2, legs
  dialed, no leg deaths and bytes on every connection. It passed on
  2026-11-20: ×2, bytes split 1.50 MB / 1.41 MB, 0 duplicate and 0 late
  frames, 0 gap resyncs.

## 13. References

- [docs/24](24-viewer-network-resilience.md) — reliable carriers, one per
  GOP, Decision 5 (≤ 2 open).
- [docs/26](26-relay-dvr-buffer.md) — the DVR ring, cursor, resync and
  pacer; stall coverage `B/(B−S)`.
- [docs/34](34-live-edge-forward-parity.md) finding 5 — per-connection
  headroom for datagrams, the method SG0's bench copies.
- [docs/35](35-connection-interleaving.md) — stripe legs, owner token,
  lease, StripeState, cross-pod legs; §3 "Why live-edge only", which this
  milestone narrows.
- [docs/88](88-browser-reliable-uplink.md) D2 — the keyframe-at-head GOP
  stream; a later option to halve DVR stream opens, not used here.
- `BUGS.md` — the deep-buffer stream-open rate.
- M. Mathis, J. Semke, J. Mahdavi, T. Ott, "The macroscopic behavior of
  the TCP congestion avoidance algorithm", CCR 27(3), 1997.
