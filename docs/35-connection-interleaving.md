# R30 — Connection interleaving (striped delivery) for live-edge

**Status**: designed 2026-07-29; not started. Chunks **ST1–ST7** ("stripe" —
every single-letter prefix A–Z is claimed, and `IL` was rejected as visually
colliding with R27's `LI`).

The mechanism's working name in this doc is **striping**: each delta frame's
datagrams are split round-robin across several WebTransport connections so
that no single connection ever carries a burst long enough to overflow the
~8-packet per-connection receive buffer measured in R29. "Connection
interleaving" (the ROADMAP title) and "striped delivery" name the same thing.

---

## 1. The evidence

Carried from [docs/34](docs/34-live-edge-forward-parity.md) findings 2–5 and
the R30 ROADMAP entry; summarized here so the design is self-contained, but
docs/34 holds the full chain including the two conclusions that were
withdrawn on measurement.

**The loss is a burst-length threshold, not a lossy network** (finding 4,
2487 frames, measured on the affected viewer's own machine against the live
fleet):

| chunks in the frame | 1–8 | 9 | 10 | 11 | 12 | 13 | 14 | 16 | 18 |
|---|---|---|---|---|---|---|---|---|---|
| chunk loss | **0.00 %** | 0.9 % | 2.1 % | 3.8 % | 4.8 % | 6.0 % | 6.8 % | 8.3 % | 8.5 % |

986 frames of eight chunks or fewer lost **not one datagram**. Loss lands on
the **head** of each burst (index 2 worst at 8.7 %, zero from index 10 on),
which is why parity — written last by the producer — loses 0.00 % while data
chunks lose 3.76 %. The drop sits in a buffer roughly eight packets deep
evicting oldest-first, **below anything JavaScript can reach**: leg A is
clean, the relay drops nothing, the receiver's drain rate is irrelevant, and
Firefox's receive-buffer attribute is inert (finding 3).

**The bottleneck is per-connection, not shared** (finding 5): four
simultaneous full-copy subscriptions — 4× the aggregate traffic through the
same host and path — cost about 10 % more per-connection loss
(3.55 % → 3.90 %), nowhere near proportional. Each connection has its own
headroom.

**The composition, stated as the risk it is**: "a connection loses nothing
below eight chunks" plus "connections do not compete for that headroom"
implies striping works — but nothing has yet split a real send side and
measured it. That experiment is ST1, it runs before anything is hardened, and
it must be able to fail (§10 criterion 1).

Loss *magnitude* varied 3.8 % → 12.6 % between sessions on the same machine
while the *shape* (threshold, head-of-burst, parity spared) was stable — so
everything in this design keys on **burst length**, never on a loss rate.

---

## 2. Goal

Eliminate the burst-length datagram loss for live-edge viewers **without
adding latency and without lowering quality**: split each delta frame's
datagrams across N connections so every connection's per-frame burst stays at
or under a target below the measured threshold.

Acceptance is the instrument that produced the evidence:
`e2e/datagram-loss-profile.mjs` (§8), run striped on the affected machine
against the live fleet, reporting **~0 % chunk loss on >8-chunk frames**
where it now reports 3.8–12.6 % — with the unstriped control still
reproducing the threshold in a paired run.

**Constraint inherited from the ROADMAP entry, spent nowhere in this design:
viewer-side self-measurability.** A receiver must stay able to answer "which
datagrams should have arrived, and on which connection?" from the wire alone:

- `chunkCount` stays **frame-global** (bytes 10–11 of the 20-byte header,
  `wire.go:19–26`) — the relay remains a byte forwarder for `0x01` and
  `0x0E`; no header is rewritten.
- The chunk→connection mapping is a **deterministic rule over values the
  receiver already has** (§5.2), never a sender-side choice it would have to
  be told about per frame.
- Both instruments still run after R30; updating them **is part of this
  item** (§8), not a follow-up.

---

## 3. Why live-edge only

Resilient and Deep-buffer delivery ride per-GOP **reliable** carrier streams
(R19/R21) where QUIC retransmits anything lost — the burst-threshold buffer
is a datagram-path property and those modes never expose frames to it. Same
argument as parity (docs/34 §3): striping there is pure waste. The relay
rejects stripe parameters for any non-datagram subscriber, and the viewer
offers the control only in live delivery (the `deliveryMode === 'live'`
conditional the parity menu group already uses,
`ViewerScreen.tsx:683–704`).

---

## 4. Owner decisions (2026-07-29)

Settled before this doc was written; recorded so they are never re-derived:

1. **Frame-size capping and pacing are rejected** (docs/34 finding 4, owner
   decision): both pay with exactly what the product exists to deliver.
   Minimal latency, maximal quality — the harder option is the right one.
2. **N is adaptive in-session** — sized from the *measured* per-frame
   datagram count off the wire (never from a bandwidth estimate, which is a
   strictly worse proxy for a number every datagram already carries), grown
   when the measurement crosses the target, **grow-only** within a session
   (§5.4). This is the option that *ensures* the burst bound under mid-stream
   bitrate/rung changes.
3. **Engagement is auto-detect plus a manual override** (§5.5): the client
   detects the finding-4 loss signature on itself and engages; a persisted
   tri-state menu control (`auto` default / `on` / `off`) forces or forbids
   it. No default-on for all live-edge viewers — that would multiply
   per-subscriber relay state fleet-wide (the R17 axis) for viewers that
   measured 0.00 % loss. No UA sniffing (docs/30's rule): the detector is
   the gate, and it is browser-agnostic.
4. **The per-connection burst target is 6 chunks** — ~25 % headroom under
   the measured ~8. The threshold is one path's property and the magnitude
   varied between sessions; margin costs at most one extra connection at
   observed frame sizes (a 24-chunk frame: 4 legs instead of 3).

---

## 5. Mechanism

### 5.1 Topology — primary plus stripe legs

A striped viewer holds one **primary** subscribe session (exactly today's)
plus 1–4 **stripe legs** — additional `CONNECT /subscribe/{id}` sessions that
carry **only** this viewer's share of delta-video datagrams (`0x01` chunks
and `0x0E` parity). Everything else stays on the primary: keyframe streams
(0x04), audio (0x07/0x08), DecoderConfig (0x02), ClockMapping (0x06),
ViewerCount (0x0B), DeliveryAck (0x0C), TelemetryHello (0x0D),
RelayCapabilities (0x0F), TimeSync (0x05), and session identity generally.

**Decision 1: while striping is engaged, the primary carries no delta share
at all — legs carry every delta datagram.** The alternative (primary as
member 0 of the stripe) saves one connection but was rejected for three
reasons:

- **Burst purity.** Audio datagrams, config re-emits and TimeSync pongs
  interleave with whatever else the primary sends; a delta share on the same
  connection would let a frame's share plus coincident audio packets exceed
  the target the design exists to enforce. With legs carrying only deltas,
  each leg's per-frame burst is exactly its share — countable and bounded by
  construction — and the primary's datagram flow is small control/audio
  traffic that never bursts.
- **Immutable legs make N changes trivially safe.** Each leg's identity is
  the pair `(member j, stripe N)`, fixed at dial time (§5.3). Growing N is
  "dial a fresh set, then retire the old set" — no in-band re-striping
  message, no per-frame effective-from coordination, no window where the
  primary's share disagrees with the legs' (§5.6).
- **Cross-pod correctness for free.** Legs may land on different relay pods
  (§5.7); a per-leg static filter needs no cross-pod agreement about when a
  stripe change takes effect, because nothing ever changes on a live leg.

The cost is one connection more than a primary-participating scheme at equal
share size (e.g. 12-chunk frames: primary + 2 legs instead of primary + 1).
Accepted — connection count is the item's main expense either way and the
correctness simplification is worth one leg.

**Decision 2: a leg is an ordinary `hub.Subscriber` with a filter, not a new
kind of session.** It reuses the existing queue/drain machinery
(`enqueueLocked` `hub.go:2348`, `drain` `hub.go:2476`) unchanged — never
reliable, never DVR, no audio lane. What a leg is *excluded* from, all at
existing exclusion points:

| Excluded | Where (precedent) |
|---|---|
| keyframe fan-out | `onKeyframe`'s snapshot already excludes DVR subs (`hub.go:2029–2043`) — legs join that exclusion |
| join priming (ClockMapping / ViewerCount / AudioConfig / cached keyframe) | `subscribeOpts` `hub.go:1383–1411` |
| audio, config, clock, count datagrams | `fanOutLocked` — legs receive only `0x01`/`0x0E` matching their ordinal (§5.2) |
| DeliveryAck + re-announce, TelemetryHello, RelayCapabilities | `handleSubscribe` `server.go:1048–1082` — a leg is not a viewer; the primary already got all three |
| R18 viewer count | `globalViewersLocked` `hub.go:1129–1142` skips legs (§5.8) |

### 5.2 The mapping — receiver-derivable by construction

Define the **datagram ordinal** `d` of a frame's delta datagrams:

```
data chunk i        →  d = i             (i ∈ [0, chunkCount))
parity symbol r     →  d = chunkCount + r
```

**Decision 3: leg `j` of stripe `N` carries exactly the datagrams with
`d mod N == j`** (round-robin, not contiguous blocks). Parity keeps its
per-subscriber prefix filter — a leg forwards parity symbol `r` only when
`r < parityK` **and** `(chunkCount + r) mod N == j`, so the R29 `?parity=`
negotiation composes unchanged.

Why round-robin: parity ordinals are the highest in every frame, so on every
leg the parity symbols land at the **tail** of that leg's burst — preserving
per-connection the exact "written last, never lost" property finding 4
measured. Contiguous blocks would put parity wholly on the last leg and,
under a transient under-provision (frame bigger than `N × target` before a
grow completes), concentrate the overflow on one leg instead of spreading one
extra chunk per leg. With head-drop, position within each connection's burst
is the variable that matters; round-robin degrades it most gracefully.

The receiver derives everything: it knows `chunkCount` (frame-global, on
every chunk), its own `parityK`, its own `N` and each leg's `j` (it dialed
them). Expected-per-leg is `|{d : d mod N == j}|`; a datagram arriving on the
wrong leg is a protocol violation the instrument counts (§8). Per-connection
loss attribution — the property that made the whole investigation tractable —
survives.

The relay-side filter is a few byte reads under `Registry.mu` in
`fanOutLocked` (`hub.go:2145–2184`): `chunkIndex` at bytes 8–9 of `0x01`,
`parityIndex`/`chunkCount` at bytes 6/7–8 of `0x0E` — both already validated
shapes (`wire.go:405–428`, `parity.go:379–407`). No reassembly, no new state
on the hot path beyond two `uint8` fields per subscriber.

**Zero changes to `0x01` and `0x0E` themselves.** The relay stays a byte
forwarder; a striped viewer's datagrams are byte-identical to an unstriped
viewer's — just distributed.

### 5.3 Negotiation and wire changes

Three small allocations. Next free type is `0x10` (verified —
`wire/wire.go:54–183` ends at `0x0F`).

**Leg dial parameters** — `?stripe=N&leg=j` on `CONNECT /subscribe/{id}`,
following the `?delivery=`/`?parity=` precedent (parsed only in
`handleSubscribe`, `server.go:994–1000`; client side appended in the
`viewer.ts:637–657` URL block via `ConnectOptions`,
`connection.ts:25–45`). Validation is pre-upgrade and strict, because a
malformed leg is useless to both sides: `1 ≤ N ≤ wire.MaxStripeLegs`,
`0 ≤ j < N`, delivery mode must be datagrams (a `?delivery=reliable` +
`?stripe=` combination is a 400), else 400. `MaxStripeLegs = 4` is a **wire
package constant** mirrored into `wire.ts` (the `MaxChunkCount` pattern), not
a knob: it is evidence-bound — finding 5 measured coexistence at 4
connections and no further — and 4 legs × target 6 = 24 chunks covers every
frame size observed (~23 max). Revisit only with a new finding-5-shaped
measurement at higher N.

**`0x10 TypeStripeState`** — client→relay datagram, 5 bytes, accepted only on
an external, datagram-delivery, non-leg subscribe session:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version |
| 1 | 1 | type = `0x10` |
| 2 | 1 | flags — bit 0 = `striped` (deltas suppressed on this session) |
| 3 | 1 | `stripeN` — informational, for `/statusz`/metrics; 0 when bit 0 clear |
| 4 | 1 | reserved (0) |

This is the primary-suppression signal: with bit 0 set, the relay stops
sending `0x01`/`0x0E` datagrams on the primary (everything else unchanged).
It is **level state, not an edge**: the viewer sends it immediately on every
change and re-sends at 1 Hz while striped (the R15 audio-config 1 Hz
precedent — datagrams get lost, a lost level refresh costs nothing).
**Decision 4: suppression expires** — if no refresh arrives for
`StripeStateTTL = 5 s`, the relay resumes deltas on the primary. Fail-open
toward brief duplicates, never toward holes: a wedged viewer state machine
(or a lost "unstripe" message) degrades to today's behavior instead of a
starved session. The reassembler dedupes the overlap for free
(`reassembler.ts:293–296`, counted in `duplicateChunks`).

The relay-side read loop change is one branch beside `maybeAnswerTimeSync`
(`server.go:878–888`, the only client→relay datagram accepted today); the
subscriber gains an atomic suppression flag + deadline that `fanOutLocked`
reads. Sessions where `0x10` is invalid (reliable, DVR, internal, legs)
silently discard it, exactly as every unrecognized datagram is discarded
today.

**`CapStripedDelivery = 1 << 1`** in the existing `0x0F
TypeRelayCapabilities` flags word (15 bits free, `parity.go:58–64`) — **not**
a new byte: `parseRelayCapabilities` is a strict 5-byte parse in the
broadcaster mirror (`broadcaster.ts:637–642`), and docs/34 §4.4's rule stands
— extending an existing message breaks old readers mid-skew. The message
stays 5 bytes. `sendRelayCapabilities` (`server.go:797–812`) currently
returns early when `ParityDefault ≤ 0`; that condition becomes "no capability
to advertise" (parity off **and** striping off), preserving both
byte-identity properties: everything off ⇒ no `0x0F` at all (pre-R29 wire);
striping off, parity on ⇒ R29's exact bytes.

**The viewer gains a `0x0F` reader.** Today it has none: `readOneServerStream`
(`connection.ts:242–254`) accepts only `0x04`/`0x0A`/`0x0D` and counts
anything else `malformed` — which means **every current production viewer
session counts one malformed stream and logs a warning**, because the relay
already sends `0x0F` on the subscribe route whenever parity is enabled
(`server.go:1077`, and the fleet default is `parity.defaultLevel: 2`). §12
records this as a pre-implementation finding; adding the dispatch branch
fixes it and gives striping its version-skew gate in one move. The viewer
engages striping **only after** seeing `CapStripedDelivery` — an old relay
never advertises, so a new viewer against it never dials a leg and never
sends `0x10`, and is byte-identical to today. The reverse direction is free:
an old viewer never dials legs, and a new relay serving it is byte-identical.

### 5.4 Sizing — the adaptive controller

All constants are named, live in one module (`transport/stripe.ts`, the
`resilient.ts`/`playout.ts` module-state pattern), and are initial values to
be tuned in ST5/ST7 — the *shapes* are the design.

- **Measured input**: the per-frame expected datagram total
  `burst = chunkCount + expectedParity` — both already computed by the
  reassembler per assembly (`reassembler.ts:90–104`). Tracked in a
  `WindowedQuantileTracker` (`live-edge.ts:83–160` — the constructor already
  takes window/bucket/bin/range, so a `(10 s, 1 s, bin 1, range 64)`
  geometry is reuse, not new machinery). The sizing statistic is the
  window's **p99**, not the mean — bursts are what overflow.
- **Need**: `stripeNeeded = clamp(ceil(p99 / STRIPE_TARGET_CHUNKS), 1,
  MaxStripeLegs)` with `STRIPE_TARGET_CHUNKS = 6` (owner decision 4).
  Engagement in **any** mode is held while `stripeNeeded < 2`: a stripe of
  one leg moves the whole burst to a different connection without shortening
  it — a connection spent for nothing. `'on'` with small frames therefore
  waits, and engages the moment frames grow past one share.
- **Grow-only**: when engaged with N legs and `p99 > N × 6` holds for
  `STRIPE_GROW_DWELL_MS = 5 s` and `N < MaxStripeLegs`, transition to N+1
  (§5.6). N never shrinks in-session — an over-provisioned stripe after a
  bitrate drop costs idle-ish legs (each carrying a share of small frames),
  which is relay state but not a user-visible harm; a reconnect resets.
  Shrinking would reintroduce transition churn for no quality win.
- **Beyond the cap**: a frame larger than `MaxStripeLegs × 6 = 24` chunks
  overshoots the target gracefully — round-robin spreads the excess one
  chunk per leg, parity covers residual loss, and the R29 loss allowance
  bounds what an unrecoverable frame costs. No hard guarantee is claimed
  past the cap, and §7's stats make the overshoot visible
  (`stripeOvershootFrames`).

### 5.5 Engagement — auto-detect plus manual override

Persisted setting `gawk:stripe-mode` ∈ `'auto' | 'on' | 'off'`, default
`'auto'` (the `gawk:*` convention; read like `loadDeliveryMode`,
`ViewerScreen.tsx:103–114`).

- **`auto`** (default): engage when the client detects the finding-4
  signature on itself, over a rolling `STRIPE_DETECT_WINDOW_MS = 30 s`:
  - loss among chunks of **large** frames (`expected > 8`) ≥
    `STRIPE_ENGAGE_LOSS = 1 %`, and
  - loss among chunks of **small** frames (`expected ≤ 8`) ≤
    `STRIPE_SMALL_LOSS_CEILING = 0.1 %`, and
  - at least `STRIPE_MIN_LARGE_CHUNKS = 500` large-frame chunks observed.

  The discriminator is the *shape* — large frames losing while small frames
  are clean — which is what separates the per-connection buffer from an
  actually lossy network (where parity/Resilient are the answers and
  striping buys ~nothing). The detector's inputs are the reassembler's
  per-frame arrival sets, i.e. the in-client port of what
  `datagram-loss-profile.mjs` computes. Once engaged, **sticky for the
  session** (no disengage-on-quiet — the buffer is a path property, and
  flapping is a video-cadence hazard R12/R27 already taught us to dwell
  against).
- **`on`**: engage as soon as sizing data exists (~2 s of frames), loss or
  not. The deterministic path for e2e (§8) and for a viewer who knows their
  path.
- **`off`**: never engage; the escape hatch if striping itself misbehaves.

Menu: a three-entry radio group, "Connection striping: auto / on / off",
following the delivery-mode radio precedent exactly
(`ViewerScreen.tsx:655–674` — checkmark appended to the label, one choose
callback, cost note in the label), visible only when
`deliveryMode === 'live'` (the parity group's conditional spread,
`683–704`), reachable via the "⋮" More-options button (docs/24 finding 9 —
touch devices fire no `contextmenu`). Unlike delivery-mode changes, flipping
this setting is **not** a reconnect: engagement is in-band (dial legs, send
`0x10`), so the change applies live; `off` while engaged sends the unstripe
`0x10` and closes legs.

### 5.6 Transitions and failure semantics

**Engage** (and every grow), make-before-break:

1. Dial the new leg set — all `N'` legs `(j, N')`, staggered
   `STRIPE_DIAL_STAGGER_MS = 100 ms` apart (rate-limiter friendliness,
   §5.8). Legs that fail to dial abort the transition (see below).
2. When every new leg's session is ready, send `0x10 {striped, N'}` on the
   primary (first engage) — for a grow, nothing to send: the primary is
   already suppressed and `stripeN` updates on the next 1 Hz refresh.
3. Close the old legs (grow only).

Between steps 1 and 3 the viewer briefly receives overlapping coverage (old
striping + new striping, or primary + legs on first engage) — **duplicates,
never holes**, at every instant either the old or the new complete cover is
active. Duplicates are dropped and counted by the reassembler
(`reassembler.ts:293–296`) and the reorder buffer (`insert`,
`reorder-buffer.ts:335–340`) — verified-safe today, and ST4 pins it with a
transition test.

**Leg death** (the ROADMAP's "failure of a secondary must degrade rather
than tear down"): on any leg's `onClosed`, the viewer immediately sends
`0x10 {unstriped}` — restoring full delta flow on the primary within one
RTT — closes remaining legs, and re-enters the engagement path with the
session's reconnect-flavored backoff (`reconnectDelayMs` shape,
`reconnect.ts:25–31`, but private to the stripe controller — leg churn must
never consume the *session's* 10-attempt budget). The viewer loses at most
the in-flight frames between the leg's death and the `0x10` landing, which
freeze-on-gap + parity already handle. **Primary death** is exactly today's
session death: `ViewerSession` reconnects, legs are torn down with the
pipeline (they are owned by the transport, which is per-attempt).

**Relay-side safety** is Decision 4's TTL: any state where the viewer
believes it is unstriped while the relay believes it is striped self-heals
in ≤ 5 s; the opposite state (relay unsuppressed, viewer striped) costs
duplicates only.

### 5.7 Cluster mode

Legs dial the same public `/subscribe/{id}` through the same UDP
LoadBalancer, so **a leg may land on a different pod than the primary** —
5-tuple hashing (MetalLB BGP/ECMP) makes same-pod affinity unenforceable,
and the design **requires no affinity**:

- A leg landing on a pod that doesn't hold the broadcast triggers the
  existing edge pull (`EnsureEdge`, `server.go:945–952`) exactly as any
  subscriber does; the pod serves the leg's share from its edge hub.
- The filter is per-leg static (`(j, N)` from the dial, Decision 1), so no
  cross-pod agreement about stripe membership ever exists to get wrong. Each
  pod filters what it serves; the union across pods is complete because each
  cover set is complete by construction.
- The `0x10` suppression applies only to the primary's own session on the
  primary's pod — a per-session flag, nothing fleet-wide.
- Origin→edge internal sessions are **never striped**: `fanOutLocked`'s leg
  filter applies only to leg subscribers, and internal subs continue to
  receive every datagram (`hub.go:2164–2180`'s internal exemption pattern).
  The origin→edge leg measured clean (docs/34 finding 4's leg-A evidence);
  striping is strictly a serving-pod→viewer concern, the same boundary R19
  drew for reliable conversion.

Cost note: a viewer whose legs land on k distinct pods can hold up to k edge
sessions against the origin where today it holds at most 1. Bounded by
`MaxStripeLegs + 1` and by pod count; visible in `/statusz` edge counts; the
scale check is ST7's.

### 5.8 Limits, counting, and the rate limiter

- **Caps**: legs count against `MaxSubscribers` and `MaxTotalSubscribers`
  (`externalSubsLocked` `hub.go:1113–1121`, `totalExternalSubsLocked`
  `1216–1222`) — they are real per-subscriber state, and counting them is
  what keeps an attacker from opening unbounded "legs" with a known
  broadcast ID. This changes the caps' operational meaning from "viewers"
  toward "connections"; the chart's values documentation says so, and the
  defaults (15/50) may deserve a raise on fleets expecting striping — an
  operator decision, informed by §7's metrics. A leg rejected on caps (429)
  simply aborts the engage/grow transition: the viewer stays at its current
  stripe (or unstriped) — capacity pressure degrades striping before it
  degrades admission.
- **R18 viewer count**: legs are excluded from `globalViewersLocked`
  (`hub.go:1129–1142`) — one watching human is one count. The leg marker is
  relay-derived (the `?stripe=` params), not client-trusted-for-exclusion in
  any new way: a client mislabeling its primary as a leg only removes
  itself from an informational count (the R18 spoof analysis unchanged).
- **Rate limiter**: legs pass through the normal per-IP bucket
  (`rateLimited`, `server.go:167–184`; 3/s burst 10 default). A striping
  join is ≤ 5 dials (primary + 4 legs) staggered 100 ms — inside the burst
  for one viewer; two simultaneous striping viewers behind one NAT touch
  the burst ceiling, and a 429'd leg degrades per the caps rule above
  (retry on the stripe controller's backoff). Deliberately **no** limiter
  bypass for legs: a bypass keyed on anything client-declared would be a
  rate-limiter hole. The R17 trusted-CIDR bypass remains available to
  operators whose NAT topology needs it.
- **`/statusz`**: `subscriberDetails[]` gains `role: "viewer" | "leg"`,
  `stripe: {n, member}` on legs, and `striped: true` + `stripeN` on a
  suppressed primary — the operator-visible join of one viewer's sessions
  (they share nothing else; `statsKey` stays per-session random by design).

---

## 6. Relay cost — the R17 axis, priced

Per additional leg: one `Subscriber` (struct `hub.go:2196–2336` minus the
audio/DVR/carrier arms), one bounded queue (`QueueDepth`), one `drain`
goroutine, one QUIC connection + WebTransport session + keepalives, one
`enqueueLocked` call under `Registry.mu` per delta datagram — **but only for
datagrams matching the leg's ordinal**, so total enqueue work for a striped
viewer is ~the same as for an unstriped one (each delta datagram goes to
exactly one of its sessions); the added `Registry.mu` cost is the per-leg
mod-check on the skip path, a few compares per subscriber per datagram.
Keyframe, audio-lane, DVR and carrier machinery are never instantiated on
legs.

The multiplier that matters is **connection count**: worst case ×5 per
viewer (primary + 4). It is bounded by engagement (only viewers whose path
measures the threshold, or who force `on`), by `MaxStripeLegs`, and by the
subscriber caps. The 1000-viewer hot-broadcast target with (say) 10 %
striped at N=3 is 1300 sessions + 300 goroutines — within the R17 envelope
but not free; ST7 measures it with `gawk-loadgen` before the feature is
called done, and §10 criterion 2 is the pre-registered response if the
measurement disagrees.

The operator switch is **`-striped-delivery`** (bool, default **true**) /
`GAWK_STRIPED_DELIVERY` / chart `config.stripedDelivery`, plumbed through
`registryOptions` in `cmd/gawk-server/main.go` (the R2 rule;
`TestRegistryOptionsCarryAllLimits` must grow the field) — off ⇒ no
capability bit, leg dials 400, `0x10` ignored, byte-identical to pre-R30.
Default-on is safe because the relay cost is zero until a viewer engages.

---

## 7. Observability

Requested-vs-active everywhere, the R19/R29 rule — what the viewer wants and
what it is getting diverge under old relays, caps pressure, and `off`:

**`ViewerStats`** (new fields, flowing to telemetry as typed rollup fields
per D15):

| Field | Meaning |
|---|---|
| `stripeMode` | `'auto' \| 'on' \| 'off'` — the setting |
| `stripeCapable` | relay advertised `CapStripedDelivery` |
| `stripeActive` | 0 = unstriped, else N (legs live and primary suppressed) |
| `stripeNeeded` | the controller's current `ceil(p99/6)` — active < needed is the caps-pressure / cap-clamp signature |
| `stripeLegs[]` | per-leg `{member, datagramsReceived, mismapped}` |
| `stripeTransitions` | engage/grow/fallback counter |
| `stripeOvershootFrames` | frames whose burst exceeded `stripeActive × 6` |
| `stripeDetector` | `{largeLossPct, smallLossPct, largeChunks}` — the auto gate's own inputs, so a non-engaging detector is arguable from a diagnostics blob |

Overlay: a **Striping** group in the Delivery section (conditional-row
spread, `StatsOverlay.tsx:101–106` idiom) — mode, active/needed, per-leg
datagram rates, transitions. `stripeActive = 0` while the detector fires is
the row operators need.

**Relay**: `/statusz` per §5.8; Prometheus `gawk_broadcast_stripe_legs`
(gauge, per broadcast) and `gawk_relay_stripe_transitions_total`
(suppress/unsuppress edges observed); legs' egress rides the existing
per-subscriber byte counters. A docs/13 playbook row: symptom "large frames
lose head chunks, small frames clean" → signature `stripeDetector` firing →
lever striping (or Resilient if striping is active and loss persists).

**`diagnose()`**: one new rule, **`burst-threshold-loss`** — the finding-4
signature computed from the typed detector fields. Action text
distinguishes: signature + `stripeActive == 0` ⇒ "path shows per-connection
burst overflow; striping should engage — check capability/caps/mode";
signature + `stripeActive > 0` ⇒ "striping active yet threshold loss
persists — the per-connection composition does not hold on this path; use
Resilient mode" (which is also §10 criterion 1's field echo). R29's
`parity-ineffective` rule's action text gains a pointer to the new rule for
the bursty branch. Client-only provenance caps confidence as ever (D8).

---

## 8. Instruments and CI

**`e2e/datagram-loss-profile.mjs`** (the acceptance instrument) gains a
striped mode, `GAWK_FF_STRIPE=N` (absent/0 ⇒ byte-identical to today): it
dials the primary + N legs itself, sends `0x10` on the primary's datagram
writer, tags every arrival with the connection that produced it, and reports
per-connection loss against the **derived per-leg expectation**
(`|{d : d mod N == j}|` per frame, frame-global `chunkCount` retained) plus
the aggregate profile unchanged. It also counts **`mismapped`** — a datagram
arriving on a leg whose ordinal rule doesn't cover it — turning the mapping
from an implementation detail into an asserted protocol property. This
update is ST1's tooling, not an afterthought.

**`e2e/datagram-connection-scaling.mjs`** is deliberately **unchanged**: its
experiment — N *independent full-copy* subscribers, is the headroom shared
or per-connection — remains exactly as meaningful after R30, and it is the
tool that re-checks the composition's second half if a new path misbehaves.
Both instruments therefore "still run after R30" (the ROADMAP constraint):
one updated to understand striping, one whose semantics striping does not
touch.

**CI (tier-1)**: the burst mechanism is reproducible in CI, unlike R19's
cellular physics — the harness's in-process UDP forwarder
(`startLossyLink`, `e2e/run.mjs:438–479`) gains a **burst-buffer mode**: per
5-tuple, an 8-deep queue draining at line rate with oldest-first eviction —
the measured buffer, modeled. Because legs are distinct 5-tuples, the
forwarder gives each connection its own buffer with no extra work. The
striped pass (seeded `gawk:stripe-mode = 'on'` via `addInitScript`, the
`run.mjs:549–588` precedent) asserts from Copy-diagnostics:
`stripeActive ≥ 2`, `framesDroppedIncomplete ≈ 0`, `mismapped == 0`,
duplicates bounded; a control pass through the same forwarder unstriped must
show large-frame loss (the forwarder proving it bites — the docs/24
finding-10 control-lane lesson). What stays manual: the real Firefox buffer
on the affected machine (ST1/ST7), per docs/27 Decision 10's
coverage-boundary honesty.

---

## 9. Chunks

| Chunk | Work | Acceptance criteria | Status |
|-------|------|---------------------|--------|
| **ST1** | **The composition experiment** — minimal vertical slice, branch-quality allowed: relay-side leg params + ordinal filter + `0x10` (hardcoded paths acceptable), the loss-profile instrument's striped mode (§8), deployed to the homelab fleet by the owner; paired striped/unstriped runs from the affected machine | Striped run: chunk loss on >8-chunk frames ≤ 0.1 % with `mismapped == 0` at N = 3; unstriped control in the same hour still reproduces the threshold (≥ 1 % on >8-chunk frames); **a recorded go/no-go verdict in this doc** — if no-go, §10 criterion 1 applies and ST2–ST6 do not start | not started |
| **ST2** | **Wire, two + wirecheck mirrors**: `0x10 TypeStripeState` (5 B) encode/parse, `CapStripedDelivery` bit, `MaxStripeLegs` constant, in `gawk-server/wire`, `wire.ts`, and `gawk-broadcast` `wirecheck` (shared-package vectors; the native broadcaster itself is untouched) | Golden vectors byte-identical across mirrors; strict-parse rejection for truncated/oversize/bad-flags `0x10`; capabilities with both bits set parse in old-message shape (5 B unchanged — a skew test pins that the R29 parser accepts the new flags word); malformed input never panics | not started |
| **ST3** | **Relay**: leg subscribe (param validation → 400, caps counting, all §5.1 exclusions), ordinal filter + parity-composition in `fanOutLocked`, `0x10` handling + suppression TTL, viewer-count exclusion, `/statusz` + Prometheus, `-striped-delivery` plumbing (config → main → hub Options → chart values → deployment env → README flags table, incl. the two rows found missing there — §12), capability-send condition | Test-first: a 3-leg subscriber set receives exactly the ordinal partition (property test over n ∈ [1, 30], parityK ∈ {0,1,2}); legs never receive keyframes/audio/config/prime/ack/hello; suppression stops `0x01`/`0x0E` on the primary within one datagram and **expires after TTL without refresh** (fake clock); `0x10` from reliable/DVR/internal/leg sessions is inert; caps: leg counts against both limits, 429 on exhaustion, viewer count excludes legs (the R18 cluster assertion extended); **`-striped-delivery=false` ⇒ `/statusz`, metrics and wire byte-identical to pre-R30** (diff-asserted, R28 pattern); `TestRegistryOptionsCarryAllLimits` grows the field; no measurable `Registry.mu` regression (benchmark vs baseline) | not started |
| **ST4** | **Viewer transport**: `0x0F` dispatch branch (+ the §12 wart fix), `StripedViewerTransport` behind the `ViewerTransportFactory` seam (`viewer-transport.ts:75`) wrapping primary + legs for both the nested-transport-worker and in-process paths (legs skip TimeSync clients); `0x10` sender + 1 Hz refresh; make-before-break transitions; leg-death fallback | Test-first against fake transports: merged datagram flow reaches the pipeline regardless of source leg; engage/grow produces duplicates and **zero holes** (transition test asserting every ordinal covered at every step); leg death ⇒ unstripe `0x10` sent before any leg teardown, session reconnect budget untouched; `0x0F` no longer counts malformed (regression test on today's behavior); unstriped path byte-identical (existing transport suites green unmodified) | not started |
| **ST5** | **Controller + UI**: sizing tracker (quantile-tracker reuse), auto-detect, `gawk:stripe-mode` tri-state + menu radio group (live-only, "⋮"-reachable), live apply without reconnect; `ViewerStats` fields + overlay Striping group | Detector fires on a synthetic finding-4 profile (large lossy, small clean) and **not** on uniform loss, not on a clean link, not below the sample floor; sticky once engaged; `on` engages without loss; `off` disengages live; grow at `p99 > N×6` after dwell, capped at 4; stats expose mode/capable/active/needed/detector; jsdom menu tests follow the delivery-group suite | not started |
| **ST6** | **Observability + CI**: telemetry typed fields, `burst-threshold-loss` rule + `parity-ineffective` action-text pointer, dashboard surfacing, docs/13 playbook row; burst-buffer forwarder mode + tier-1 striped pass with unstriped control (§8) | Rule fires with distinct action text for `stripeActive == 0` vs `> 0` synthetic sessions, silent on healthy ones; pre-R30 sessions diagnose without `schemaAnomalies`; 32 KB ceiling still met; e2e: control pass shows large-frame loss through the burst forwarder, striped pass shows `stripeActive ≥ 2`, `mismapped == 0`, `framesDroppedIncomplete ≈ 0`; both instruments run (§8) | not started |
| **ST7** | **Verification**: acceptance on the affected machine (§2), scale sanity with `gawk-loadgen` (striped-fraction sweep vs R17 baselines), kill criteria evaluated with recorded verdicts; ROADMAP/README/BUGS sync | The §2 acceptance number, paired; loadgen: relay CPU/lock metrics at 200 viewers × N=3 within the pre-registered envelope (set from R17's scale-proof numbers before the run); §10 verdicts recorded; docs updated | not started |

Ordering: **ST1 → ST2 → (ST3 ∥ ST4) → ST5 → ST6 → ST7.** ST1 is the gate —
its instrument work (loss-profile striped mode) is deliberately inside it so
the experiment and the acceptance tool are the same code. ST3/ST4 can land in
parallel against ST2's wire; ST5 needs ST4's transport; ST6 needs ST5's
stats.

---

## 10. Kill criteria (pre-registered)

A documented rejection is a valid completion.

1. **The composition fails** (ST1): if a real split send still loses > 1 %
   of >8-chunk-frame chunks at ≤ 6 chunks/connection in paired runs, the
   per-connection-buffer model is wrong somewhere that matters and the item
   stops. Response: route the affected path to Resilient mode (its designed
   fallback), keep the detector + `burst-threshold-loss` rule as
   diagnostics, and record what the split *did* change — that number is the
   next design's finding 1. Do **not** respond by pacing or frame-capping
   (owner-rejected, §4).
2. **Relay cost at scale**: if ST7's loadgen shows the striped-fraction
   sweep breaching the pre-registered envelope (set from R17's scale-proof
   baselines before the run), flip `-striped-delivery` default to off and
   ship it as a per-deployment opt-in; revisit `MaxStripeLegs`.
3. **Auto-detect misfires**: if the detector engages on any clean-link CI
   run, or on uniform (non-threshold) loss in the synthetic suite, raise
   thresholds or demote `auto` — never ship a default that stripes viewers
   whose path didn't earn it.
4. **Transition glitches**: if make-before-break transitions measurably
   cost gap resyncs or visible stutter in e2e, freeze N at first engage
   (drop the grow path) — the initial sizing already covers the observed
   frame range, and grow is the smaller half of the win.

---

## 11. Non-goals

- **Pacing and frame-size capping** — rejected by owner decision (docs/34
  finding 4); restated so this doc never reopens them.
- **Striping Resilient/Deep-buffer delivery** (§3), **keyframe streams**, or
  **audio** — reliable streams retransmit; audio never bursts.
- **Relay-side burst reshaping** (relay pacing datagrams into the
  connection) — pacing in disguise; adds the latency this item exists to
  refuse.
- **Multipath in the network sense** — all connections share one host, one
  path, one LB VIP; this is buffer-parallelism, not path diversity. MoQ /
  QUIC multipath stay out of scope (CLAUDE.md).
- **Same-pod affinity for legs** — unenforceable through the UDP LB and
  deliberately not needed (§5.7); do not add a redirect/proxy layer to get
  it.
- **A per-frame stripe-membership wire field** — the mapping is derivable
  (§5.2); carrying it would spend header bytes to duplicate arithmetic.
- **Operator-tunable target/cap knobs** — `STRIPE_TARGET_CHUNKS` and
  `MaxStripeLegs` are constants beside the eviction-threshold precedent
  (docs/24 finding 12's `CarrierWriteTimeout` rule): path physics, not
  fleet policy. The one knob is the on/off switch (§6).
- **In-session stripe shrink** (§5.4) and **cross-session persistence of
  the engaged state** — the detector re-earns engagement each session in
  `auto`; a path property measured yesterday is not a config.

---

## 12. Pre-implementation findings

### Finding 1 — the relay already sends `0x0F` to viewers, and the viewer counts it malformed (2026-07-29)

Found while grounding §5.3. `handleSubscribe` sends `RelayCapabilities` on
every subscribe session when parity is enabled (`server.go:1077`;
`sendRelayCapabilities` `797–812`, gated only on `ParityDefault > 0` — the
fleet default is 2), with the stated intent that the viewer's overlay could
show "requested 2 / active 1". But the viewer's uni-stream dispatch
(`readOneServerStream`, `connection.ts:242–254`) accepts only
`0x04`/`0x0A`/`0x0D`: every `0x0F` stream lands in `carrier.malformed++`
with a per-session console warning, and no viewer code ever reads the fleet
parity level — `parityActive` is derived from arriving parity datagrams
instead, which is why R29's overlay works anyway. Harm today: a cosmetic
malformed count of 1 per viewer session and a noisy warning; the R29 e2e
assertions never looked at `malformed` on healthy passes, which is how it
shipped green. ST4 fixes it by giving `0x0F` a real branch (which R30 needs
regardless); until then it is a known wart, not a BUGS.md entry — nothing
user-visible misbehaves.

Recorded second (smaller): the `gawk-server/README.md` flags table is
missing `-parity-default` and `-live-edge-audio-on-reliable-stream`; ST3's
plumbing criterion folds the two rows in with the new flag's.
