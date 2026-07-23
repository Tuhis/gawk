# R21 — Relay DVR ring buffer for resilient mode

**Status**: designed 2026-07-23, not started. Supersedes the delivery half of
docs/24 Decision 5 (drops-over-stalls at GOP granularity) **for resilient
subscribers only**; the datagram path and non-resilient reliable delivery are
untouched.

Chunks are **DV1–DV6**. Every single-letter chunk prefix A–Z is already claimed
by an earlier milestone (`O` is the only survivor and reads as zero), so this
milestone uses a two-letter prefix. Future milestones should do the same rather
than reusing a letter.

## Goal

Make resilient mode do what its name promises: **ride out a multi-second
connectivity stall with no freeze and no lost frames**, by having the relay
retain a short window of each broadcast and serve every resilient subscriber
from its own cursor into that window.

Success criterion, pre-registered: a viewer with a 3 s playout offset survives a
**2 s total blackout** with zero discarded frames and no visible freeze, given
recovery bandwidth ≥ 3× the stream bitrate (see Decision 6 for where that
number comes from). Below that bandwidth it degrades to today's behaviour — a
freeze — rather than to something worse.

Latency is the accepted trade. This mode is explicitly not live-edge.

## Background: why resilient mode freezes today

From the 2026-07-22 paired capture (BUGS.md) and docs/24 finding 17.

The viewer's playout buffer is not made of extra data — it is made of *delay*.
`releasableAt = captureTs + arrivalBaseline + offset`, so a 2 s offset means
every frame sits 2 s in the viewer before it is presented, and the "buffer" is
just the frames occupying that window. Nothing pre-fetches anything.

So during a stall the viewer coasts on frames it already holds, and the frames
captured *during* the stall are the ones it needs next. Today the relay destroys
exactly those:

- `CarrierWriteTimeout` (500 ms) fails the parked write and marks the GOP dead.
- docs/24 finding 17's purge then discards that GOP's queued deltas outright.

Both are correct for live-edge delivery — a late frame is worthless there — and
both are fatal for a mode whose entire premise is that late frames are *fine*.

The consequence is that deepening the viewer's buffer alone does not remove the
freeze, it relocates it:

| Wall time | 2 s blackout, viewer offset 2 s, relay as it is today |
|---|---|
| 0 s | Blackout. Viewer coasts on its held frames. Smooth. |
| 0 → 2 s | Relay's carrier writes park, hit 500 ms, GOPs declared dead, ~4 GOPs destroyed. |
| 2 s | Network back. Viewer's held frames exhausted. The next 2 s of content no longer exists. |
| 2 → 4 s | Fresh frames arrive with `captureTs ≈ 2 s`; they are not due until `≈ 4 s`. **Freeze.** |

Same total freeze, now with 2 s of permanent added latency. The trade bought
nothing.

**The capacity was never the problem.** Since docs/24 finding 17 the
per-subscriber queue is 1024 datagrams ≈ 2.6 s of 1080p — already the right
order of magnitude. What is missing is three structural things:

1. a drop policy that destroys the window instead of preserving it,
2. keyframes, which live outside that queue entirely (their own streams), so a
   cursor into it has no decodable starting point,
3. any notion of *where a given subscriber is* in the stream.

R21 is mostly about (2) and (3). (1) is a policy reversal for this mode.

## What changes conceptually

Today a reliable subscriber owns **a queue of things not yet sent to it**. After
R21 a broadcast owns **a window of itself**, and each resilient subscriber owns
**a cursor into that window**.

That inverts the memory story: one copy of the bytes per broadcast instead of
one per subscriber.

It also reduces fan-out cost, but **only for the subscribers that opt in** —
`fanOutLocked` still walks every datagram and non-DVR-reliable subscriber to
enqueue, so the real cost becomes `O(non-DVR subscribers)` plus one ring
append. On a hot broadcast whose viewers are mostly resilient that is a
meaningful saving against the R17 scale target; on a mixed broadcast it is
close to a wash. It is a side effect, not a reason to do this.

## Decisions

**1. The ring is per broadcast; subscribers hold cursors.**
One copy of the bytes, N cursors. At 15 subscribers this is the difference
between ~1 MB and ~15 MB per broadcast. It is also what makes the window
affordable enough to be on by default for the mode.

**2. The ring stores GOPs, not datagrams.**
An entry is `{ gopSeq, keyframeMsg []byte, records [][]byte, complete bool }`.
Two reasons, and the second is the load-bearing one:

- A cursor must be able to *start*, and the only decodable start is a keyframe.
  A ring of loose delta datagrams cannot be entered at an arbitrary point.
- It maps 1:1 onto machinery that already exists. The carrier already rotates
  per GOP; replaying a GOP is "open a keyframe stream, write the cached
  message, open a carrier, write the records" — which is exactly what the live
  path does today, with the bytes coming from the ring instead of from the
  wire. A replayed GOP is **byte-identical on the wire to a live one**, so the
  viewer needs no concept of replay.

**3. Cursor = `{gopSeq, recordIdx}`; it only ever moves forward.**
Advanced on successful write. The one exception is the resync in Decision 4,
which jumps it forward, never back.

**4. Falling off the tail is the only frame loss this mode has.**
If a subscriber's cursor points at a GOP the ring has already evicted, it is
resynced to the newest complete keyframe — one freeze-to-keyframe, exactly
today's failure. The whole design converts *"stalls longer than 500 ms lose
frames"* into *"stalls longer than the ring window lose frames"*. That is the
entire product claim, and it should be stated that plainly in the release notes.

**5. Resilient subscribers lose the per-subscriber queue entirely.**
No queue means no overflow, so docs/24 finding 11's `carrierQueueOverflow`,
finding 14's drop-oldest and finding 17's dead-GOP purge all become unreachable
for this mode. They stay in force for the datagram path and are **not** deleted
— see Decision 12. The replacement metric is `dvrResyncs` (fell off the tail).

**6. Catch-up is bounded, and the bound is arithmetic, not taste.**

Let `R` = stream bitrate, `S` = stall length, `B` = viewer buffer (playout
offset), `ρ` = catch-up rate multiplier.

At recovery the viewer holds `B − S` seconds. The relay must clear an `S`-second
backlog before that runs out, while still carrying live at `R`. Backlog drains
at `(ρ−1)R`, so it clears in `S/(ρ−1)`, and we need:

```
S / (ρ − 1)  ≤  B − S        ⟹        ρ  ≥  B / (B − S)
```

| Buffer `B` | Stall `S` | Required burst `ρ` |
|---|---|---|
| 2 s | 2 s | ∞ — **does not work** |
| 3 s | 2 s | 3× |
| 4 s | 2 s | 2× |
| 4 s | 1 s | 1.33× |

**The buffer must strictly exceed the stall**, and the excess sets the burst
requirement. This is the single most important number in the design and it kills
the naive "set the buffer to the stall length" instinct — 2 s of buffer cannot
cover a 2 s stall, at any bandwidth.

The relay therefore enforces a per-subscriber `ρ` ceiling (default **2×**) so one
recovering viewer cannot starve the others sharing the pod's egress budget, and
the ring window is sized from `B`, not from `S`.

**7. Negotiation is a subscribe query param: `?delivery=reliable&buffer=<ms>`,
and the value is the viewer's guaranteed FLOOR, not its current offset.**

Query params are the established negotiation surface (publish secret, then
`delivery` itself) because the WebTransport JS API cannot set headers.

`buffer` carries `RESILIENT_PLAYOUT_PROFILE.minMs` — the viewer's *minimum*
playout offset, not the one it happens to be running. The distinction is
load-bearing: the viewer's offset is adaptive (`PlayoutController` slews between
`minMs` and `maxMs` on measured jitter), so any snapshot is stale the moment it
is sent, while the floor is a static property of the profile. Because it is a
floor, every error is conservative — the relay never assumes more buffer than
the viewer guarantees.

The relay uses it for one decision, which subsumes both the staleness bound and
the catch-up ceiling: **is the backlog still recoverable?** If the cursor's lag
exceeds `buffer`, the oldest unsent record is already past due at the viewer,
so sending it is waste — resync (Decision 4) instead of spending the link on
frames that will be late-dropped.

**A query param must never reject the session.** It is a hint from a client that
may be older, newer, or simply wrong:

| Value | Behaviour |
|---|---|
| absent | today's carrier path, no ring (Decision 12) |
| < `MinDvrBufferMs` (~1000) | **downgrade, not error** — today's carrier path. A 150 ms viewer cannot use a DVR: every replayed frame would arrive past due. |
| in range | DVR enabled, `buffer` as the staleness bound |
| > the configured ring window | clamp to the window; no extra logic needed, because the ring's tail binds before the staleness rule can fire |
| unparseable, negative, zero, absurd | treat as absent (strict parsing, R2 discipline) |

**7a. Open question: the mode is currently unobservable to the viewer.**
Decision 2 makes a replayed GOP byte-identical to a live one, which is what
keeps the viewer free of any concept of replay — and also means the viewer
**cannot tell whether its `buffer` request was honoured, downgraded, or served
by a relay too old to know the param**. R19 deliberately ships
`deliveryMode: 'reliable-requested'` vs `'reliable'` for exactly this reason,
and that distinction is a large part of why the 2026-07-22 investigation was
tractable at all (BUGS.md). Shipping an unobservable mode into a project whose
last week was spent suffering from unobservability deserves a deliberate
choice, not a default.

Three options, in order of preference:

1. **Bend Decision 14 minimally**: one new relay→viewer control message
   (`TypeDeliveryAck`, version ‖ type ‖ uint16 accepted-buffer-ms — 4 bytes),
   sent once at join. The media format stays untouched; the viewer reports
   `dvrBufferMs` in its diagnostics and the R19 truthful-mode row extends
   naturally. Costs a wire allocation and both-side parsing.
2. **Operator-only visibility**: DV4's `/statusz` `subscriberDetails.dvr{...}`
   already shows it. Adequate for someone with cluster access, useless in the
   Copy-diagnostics blob that is how this project actually receives bug
   reports.
3. **Inference**: under a stall a DVR subscriber shows no `reorderGapResyncs`
   where a non-DVR one does. Weak, and only distinguishable during the failure
   it is meant to diagnose.

Recommendation: option 1. The whole point of Decision 14 was avoiding changes
to the *media* format; a 4-byte join-time control message is not that, and the
alternative repeats a mistake this milestone exists downstream of.

**8. Audio gets its OWN ring, indexed by time, with its cursor slaved to the
video cursor by timestamp.**

Audio and video do not map 1:1 and must not be made to. A GOP exists because a
delta frame is undecodable without its keyframe — that dependency is what forces
the ring's entry unit. Audio has no such structure: every Opus packet is an
independent entry point, so there is no natural audio GOP and no reason to
invent one. Four ways the two media differ, each of which would be a compromise
if they shared a structure:

| | Video | Audio |
|---|---|---|
| Entry unit | GOP (dependency-forced) | any packet |
| Eviction unit | whole GOPs | by age |
| Arrival timing | later (reassembly, keyframe wait) | earlier — docs/20 finding 4 |
| Bandwidth share | ~3 Mbps | 128 kbps — **~4 %** |

Binning audio into GOP entries also fails operationally: audio consistently
arrives *before* the video whose GOP it would belong to, so entries would have
to be created speculatively or strays parked somewhere. And it would couple the
lifetimes — evicting a GOP would take audio the audio cursor had not reached.

Memory makes the separation free: 3 s of audio is ~48 kB against ~1.1 MB of
video.

What relates the two rings is **`timestampUs` on the broadcaster's
`performance.now()` clock** — docs/20 Decision 3's load-bearing sync decision,
which exists precisely so cross-medium alignment is a subtraction. The relay
does **not** align A/V: the viewer already does that from timestamps, with video
as master (docs/20 finding 4). The relay's only obligation is to keep the two
cursors close enough that the viewer's audio buffer can absorb the difference.

**8a. Audio in the ring implies audio on a reliable stream.**
A DVR is useless for a medium whose delivery cannot recover loss. Audio rides
unreliable datagrams today (docs/20 finding 5), and during a blackout those
packets are simply gone — retaining them in a ring buys nothing. So including
audio *necessarily* means putting it back on a stream, which reopens finding 5.

Survivably, though: finding 5's harms were head-of-line blocking behind video
deltas **on the shared carrier**, and GOP-clumped tail drops. Give audio its
**own** unidirectional stream and neither applies — QUIC streams are
independent, and with no GOPs there is no tail to drop. One long-lived stream
per session, no rotation: a resync is just a timestamp discontinuity, which
`AudioJitterBuffer` already handles (gap → conceal; > `FORWARD_RESTART_MS` →
re-anchor).

**8b. The audio cursor must be throttled to the video cursor, or the viewer
throws the audio away.**
Audio is ~4 % of the bitrate, so after a stall it catches up almost instantly
while video is still draining its backlog. The viewer holds audio against the
video presentation schedule, and `AudioJitterBuffer`'s overflow ceiling is
`max(targetMs, establishedDepthMs) + OVERFLOW_SLACK_MS` (200) — so audio
arriving ~2 s ahead of its video is **overflow-dropped on arrival**. The DVR
would rescue the audio and the viewer would discard it.

The relay therefore serves audio only up to
`videoCursorTimestamp + AudioSkewBudget` (~500 ms, well inside the viewer's
`RESILIENT_AUDIO_PROFILE.maxMs` of 2000 and its `MAX_ALIGNMENT_HOLD_MS` of
3000). Throttling 4 % of the pipe costs nothing, and it is the only coupling
the two rings need.

**8c. A resyncing audio cursor re-emits the cached audio config first.**
`TypeAudioConfig` is re-sent by the broadcaster at 1 Hz and cached by the hub;
after a cursor jump the decoder may need it before the next natural re-send.
Mirrors the video path, where config rides every keyframe.

Because 8a partially reverses a field finding learned the hard way, the whole
audio lane ships **behind its own flag** (`-dvr-audio`, independently
disable-able without disabling the video DVR) and DV5 must measure `avSkewMs`
before and after. A documented decision to leave audio live-edge is a valid DV5
outcome.

**9. Health is "is the cursor advancing?", not "is it at live?".**
A DVR subscriber legitimately sits behind live — that is the feature. Every
existing liveness signal that keys on lag must be re-expressed in terms of
cursor progress, or the eviction machinery will start killing healthy viewers.
Specifically: `KeyframeSlowEvictThreshold`, `CarrierOpenFailEvictThreshold` and
the R19 write deadline all need a DVR-aware reading (DV4).

**10. The window is bounded by duration AND bytes.**
3 s at 50 Mbps is 18 MB per broadcast; five of those is 90 MB on a pod sized for
none of it. `-dvr-window` (duration, default 3 s) and `-dvr-max-bytes` (per
broadcast, default 24 MB) — whichever binds first evicts. The byte cap is the
one that protects the pod; the duration cap is the one that expresses the
product intent.

**11. The ring is allocated lazily and freed with the last DVR subscriber.**
A fleet with no resilient viewers pays nothing — the R19 discipline that mode-off
is byte-identical, carried forward. A broadcast with one DVR viewer pays one
window regardless of how many more join.

**12. Today's drop machinery stays, for everything else. There are three
delivery modes after R21, not two.**

| Request | Delivery | Changed by R21 |
|---|---|---|
| `/subscribe/{id}` | unreliable datagrams, live-edge, drop-newest queue | no |
| `?delivery=reliable` | per-GOP carrier, 500 ms deadline, finding 17's dead-GOP purge | no |
| `?delivery=reliable&buffer=<ms>` | ring + cursor | the new one |

Three and not two because a viewer running a 150 ms offset genuinely cannot use
a DVR — every replayed frame would arrive past due — so the deep buffer and the
ring have to be enabled together or neither.

**Where the modes are not isolated**, and each is a thing to watch rather than
a thing to fix:

- **Memory.** The ring is per *broadcast*. One DVR subscriber joining a
  broadcast whose other ten viewers are on datagrams makes that broadcast
  allocate a window. No behaviour change for them; the pod still pays.
- **Egress.** `-max-bandwidth` is one process-wide bucket (docs/24 finding 13),
  so a DVR subscriber's catch-up burst debits the same budget serving datagram
  viewers on that pod. A recovering resilient viewer can therefore cause
  bandwidth drops for normal-mode ones. Bounded by the `ρ` ceiling (Decision 6)
  and visible in the existing bandwidth-drop counters; no new mechanism in v1.
- **Eviction code.** Decision 9 re-expresses health as cursor progress. That
  logic is shared, so the rewrite must be scoped to DVR subscribers or it
  silently changes when the other two modes evict — see DV4's criteria.

**13. In cluster mode the ring lives on the pod serving the subscriber.**
Origin→edge stays datagram-based and live, per docs/24's existing interop
argument (in-cluster links are effectively lossless; the lossy leg is the last
mile). An edge builds its own ring from what it receives. A stall on the
origin→edge leg is out of scope, by the same argument that puts
broadcaster→relay ingress loss out of scope.

**14. Zero changes to the media wire format.**
No change to `VideoChunk`, `StreamFrame`, the carrier framing or any close code.
A replayed GOP is indistinguishable from a live one on the wire, which is what
keeps the viewer free of any concept of replay. The only protocol-visible change
is one more query param, which an old relay ignores — degrading a new viewer to
today's behaviour rather than breaking it.

Decision 7a proposes the one deliberate exception: a 4-byte join-time
`TypeDeliveryAck` so the viewer can report what it was actually served. That is
a control message, not a media one, and it is recommended precisely because the
alternative is an unobservable mode.

## What the viewer needs (less than you would think)

The viewer already accepts frames whose `releasableAt` is in the future — that
is what the playout offset *is*. So the DVR needs no viewer-side protocol work.
Three tuning changes, all in `playout.ts`:

- `RESILIENT_PLAYOUT_PROFILE.minMs` / `seedMs` raised so the mode actually holds
  the buffer the relay is now able to fill (Decision 6's `B`). Provisional
  3000 ms, to be set by DV6's measurement.
- Confirm the R19 reorder capacity (256 frames ≈ 8.5 s at 30 fps) still bounds
  the deeper buffer. It should; DV6 verifies rather than assumes.
- `RESILIENT_AUDIO_PROFILE.maxMs` (2000) and `MAX_ALIGNMENT_HOLD_MS` (3000) must
  both exceed `B`, or audio cannot hold long enough to stay aligned with a video
  playhead that is now `B` behind (docs/20 field finding 4 makes video the
  master clock).

## End-to-end path: a 2 s blackout, `B = 3 s`, ring 3 s

| Wall time | Relay | Viewer |
|---|---|---|
| −∞ → 0 | Cursor at live; ring holds the last 3 s, unused. | Presents `t−3`; holds 3 s. |
| 0 | Blackout. Carrier write parks. | Coasts. |
| 0 → 2 | Ring keeps appending live GOPs. Cursor frozen at `t=0`. Nothing is discarded — the write deadline no longer kills the GOP for DVR subscribers (DV2). | Coasts; buffer drains 3 s → 1 s. **No freeze.** |
| 2 | Link back. Drain resumes **from the cursor**, at up to 2×. | Frame `t=0.1` arrives ~2.1 s, due 3.1 s — early. |
| 2 → 4 | Backlog drains at `(ρ−1)R = 1×`; 2 s of backlog cleared in 2 s. | Buffer refills 1 s → 3 s. **Still no freeze.** |
| 4+ | Cursor back at live; rate falls to 1×. | Steady state restored. |

And the case that still fails, stated honestly: a stall longer than the ring
window (or than `B`) drops the cursor off the tail, the subscriber resyncs to
the newest keyframe, and the viewer freezes exactly as it does today. The mode
moves the cliff; it does not remove it.

## Chunks and acceptance criteria

| Chunk | Scope | Acceptance criteria (verified by) |
|---|---|---|
| **DV1** | `internal/hub/dvr.go`: the ring — GOP entries, append, cursor read, duration+byte eviction. Pure, no I/O. | Unit tests: append/read round-trips a GOP byte-identically; eviction fires on whichever bound binds first; a cursor into an evicted GOP reports fall-off rather than reading freed memory; concurrent append + N readers is race-clean under `-race`. **Ownership invariant**: a test mutates the caller's datagram buffer after append and asserts the ring's copy is unchanged — today's queue already assumes per-datagram ownership, and this makes it checked rather than assumed. |
| **DV2** | Drain rewrite: a DVR subscriber reads from the ring at its cursor instead of from a queue. Per-GOP replay = keyframe stream + carrier. Write deadline no longer kills the GOP. | A blackout test (userspace UDP forwarder, `resilient_loss_test.go` precedent) drops all relay→viewer packets for 2 s: **every** delta is delivered verbatim afterwards, in order, zero holes, and `dvrResyncs == 0`. A control subscriber on today's path loses its GOPs over the same window. A 5 s blackout against a 3 s ring resyncs exactly once and never wedges. |
| **DV3** | Negotiation (`buffer` param + clamp + downgrade), `-dvr-window` / `-dvr-max-bytes` / `-dvr-max-catchup` / `-dvr-min-buffer` flags + `GAWK_*` envs + Helm values + `registryOptions` wiring, lazy alloc/free. Decision 7a's ack if taken. | The full param matrix from Decision 7 is table-driven and **no row rejects the session**: absent, below-minimum (downgrades to the carrier path), in range, above the window (clamps), and each garbage shape (empty, negative, zero, `"abc"`, overflowing uint) all yield a working subscriber. Every knob walked flag → env → chart → **production call site** (the R2 finding: a knob wired only into the test helper is a no-op). A broadcast with no DVR subscriber allocates no ring (asserted on the type, not inferred). Catch-up ceiling holds: a recovering subscriber never exceeds `ρ×R` measured over a 1 s window. |
| **DV4** | Health + observability: cursor-progress-based eviction, `/statusz` `subscriberDetails.dvr{lagMs, gopSeq, resyncs}`, ring occupancy per broadcast, Prometheus `gawk_broadcast_dvr_*`, docs/13 playbook row. | A subscriber sitting 2.5 s behind but **advancing** is not evicted (this is the regression the current thresholds would cause). A subscriber whose cursor has not moved for the eviction window still is. **Mode isolation is asserted, not assumed**: the existing datagram and reliable-without-buffer eviction tests must pass unmodified, and a test covers a broadcast serving all three modes at once — the DVR subscriber's lag must not influence when the other two are evicted. Every new counter appears in both `/statusz` and `/metrics`, and the playbook names the symptom→signature pair. |
| **DV5** | Audio ring (time-indexed, own cursor), audio on its own long-lived reliable stream, cursor throttled to `videoCursorTs + AudioSkewBudget`, config re-emit on resync. All behind `-dvr-audio`. | With the flag on, a 2 s blackout delivers every audio packet **and the viewer keeps them**: `audioBuffer.overflowDrops` must not rise during catch-up — the throttle in 8b is what this criterion exists to prove, since an unthrottled audio cursor would be rescued by the relay and discarded by the viewer. `avSkewMs` median ≤ 60 ms / p95 ≤ 120 ms (docs/20's existing bar). Audio on its own stream must not head-of-line block behind video: asserted by content, never by stream-accept order (docs/22 finding 9). With the flag off, behaviour is byte-identical to docs/20 finding 5. **A documented decision to ship it off is a valid outcome** — the criterion is the measurement, not the direction. |
| **DV6** | Verification + tuning pass: set `minMs`, ring default and `ρ` from measurement; real-network blackout drills. | The headline criterion at the top of this doc, on real hardware: 3 s buffer, 2 s blackout, zero discarded frames, no visible freeze. Plus the honest negative: the measured bandwidth below which it degrades, recorded as a number. |

Ordering: DV1 → DV2 is the minimal path and is independently valuable (it is
what removes the freeze). DV3 makes it operable, DV4 makes it safe to run,
DV5 is separable and may be declined, DV6 closes it.

## Verification plan

**Automated (CI).** The `resilient_loss_test.go` forwarder already models a lossy
relay→viewer leg with a clean publisher; a blackout is the same harness with
100 % loss for a bounded window. That is the whole DV2 criterion and it runs in
an unprivileged container — no `tc netem`, no NET_ADMIN, consistent with docs/24
finding 10's reasoning. The R20 tier-1 browser harness gains a third viewer pass
with `buffer` set, covering the negotiation end to end (no loss injected there —
the Go test owns behaviour under loss).

**Manual (owner).** Real mobile link, real blackouts: lift/drop the interface,
walk into the known dead spot. The numbers that matter are the ones CI cannot
produce — actual recovery bandwidth on a real LTE re-attach, which is what
decides whether `ρ = 2` is a real ceiling or a fantasy.

## Risks

- **Memory.** Bounded by Decision 10, but the byte cap is the only thing between
  a 50 Mbps broadcaster and a pod OOM. DV3's criterion must include the cap
  actually binding, not just being parsed.
- **Egress bursts, across modes.** `ρ = 2` per subscriber times 15 subscribers
  is 30× the stream bitrate in the worst case, all recovering together after a
  shared network event. The global `-max-bandwidth` cap already exists and will
  simply drop — which is correct, but the bucket is shared with the datagram
  and reliable-without-buffer viewers on that pod, so the cost of a DVR
  recovery is not confined to DVR viewers, and the *first* viewers to reconnect
  win. Worth a playbook note rather than a mechanism in v1.
- **The audio reversal** (Decision 8a) contradicts a field finding that was
  learned the hard way. Flagged, gated, and measured. The specific thing to
  watch is that finding 5's harms really are absent on a *separate* stream —
  QUIC stream independence is the whole argument, and it is worth confirming
  empirically rather than from the spec.
- **The audio throttle is a new coupling** between two otherwise independent
  rings (Decision 8b). Get it wrong in the loose direction and the viewer
  overflow-drops the audio the DVR just rescued; wrong in the tight direction
  and audio stalls waiting for video it does not need.
- **It does not fix the unknown.** BUGS.md's parked writes may be a *viewer*
  that has stopped reading (the WebKit stream-path wedge). A DVR buys such a
  viewer more time; it does not make it read. If that hypothesis is right, R21
  raises the stall length the system tolerates and leaves the underlying bug
  exactly where it is.

## Non-goals

- Seeking, scrubbing, pause/resume, or any DVR depth beyond seconds.
- Relay-side A/V alignment. The viewer already aligns from timestamps with
  video as master (docs/20 finding 4); the relay only bounds how far apart the
  two cursors may drift (Decision 8b).
- Recording to disk or any persistence — the ring dies with the process.
- Catching up faster than the egress budget allows.
- A ring on the origin covering origin→edge stalls (Decision 13).
- Changing the datagram path, the broadcaster, or the wire format.

## Rejected

- **Deepen the viewer buffer only.** The Background timeline: it relocates the
  freeze and adds permanent latency. This is what prompted R21 and is the thing
  most likely to be re-proposed as "simpler".
- **Per-subscriber copies of the window.** O(subscribers) memory for identical
  bytes; Decision 1 exists precisely to avoid it.
- **One ring holding both media, with audio binned into GOP entries.** Tempting
  because it makes the cursor a single number and A/V advance together. It
  fails on arrival order — audio consistently arrives *before* the video whose
  GOP it would belong to, so entries would need to be created speculatively or
  strays parked — and it couples the eviction lifetimes, so evicting a GOP
  would take audio the audio cursor had not reached. Decision 8's table lists
  the four ways the two media differ; a shared structure compromises on all of
  them to save a coordinate that timestamps already provide.
- **Reuse the existing 1024-slot queue as the window.** It is nearly the right
  size, which makes it tempting, but a cursor into it cannot start (keyframes
  live outside it) and it has no eviction bound expressible in seconds. Fixing
  both turns it into the ring anyway, minus the shared-copy property.
- **A NACK/retransmit back-channel.** Rejected for R19 (docs/24 Decision 6, "for
  good") and nothing here changes that argument: QUIC already retransmits, and
  the problem was never detection.
- **Unbounded catch-up.** One recovering viewer would take the pod's whole
  egress budget at exactly the moment every other viewer is also recovering.
