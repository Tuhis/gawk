# R84 — Browser Reliable uplink: measured first (docs/88)

**Status**: designed 2026-11-10 from a lab pass on Chrome's own QUIC stack
(§4, §11), building on [docs/75](75-reliable-uplink.md)'s F18. Owner
decisions OD1–OD6 taken 2026-11-10 (§3): the keyframe inline (K3), the
recommendations for the rest, *Lowest latency* the default until BR0 and
BR5 hold. The owner chose to build BR1–BR4 before BR0 (§8 *Order*); BR0
and BR5 gate the default, not the code. BR1–BR4 built 2026-11-10, opt-in;
on the shipping code G1, G2, G4 and G5 are met on Chrome 151, 157 (stable)
and 158, G3 on 157 and 158 and missed by 0.02 point on 151 (§8 BR4, §11
tables S5–S6). BR0 and BR5 not run. `BR` = Browser Reliable.

Touches `gawk-app` (the browser broadcaster's send path, one setting,
Diagnostics, telemetry fields), and, because OD1 took the inline keyframe
(K3), `gawk-server` (one stream type and one capability bit on RU1's
carrier ingest) and the wire mirrors. No viewer change.

---

## 1. Why

The browser broadcaster still sends every delta as a datagram. Since
2026-11-05 the native broadcasters send them on R73's per-GOP carrier by
default (docs/75 RU3, D4), so a burst or a radio outage on their uplink is
retransmitted instead of lost for every viewer. A browser broadcaster's
viewers get no such protection, and the ones who chose the most stability
are the most exposed:

- `BUGS.md` "Smoother and Most stable viewers freeze on uplink loss that
  Balanced viewers never see": reliable delivery has no loss allowance, so
  a chunk lost before the relay is a freeze to the next keyframe. RU8's
  parity repairs the random-loss half (docs/75 table K2); bursts and
  outages are still open, and for the browser broadcaster the entry's "Fix
  would start" is this milestone.
- docs/75 F14: on a datagram uplink with bursts or outages, *Smoother* and
  *Most stable* show 70–77 % of frames (table J), 81–84 % under bursts even
  with parity (table K), outages unchanged; on the carrier, 94–100 %.
- docs/75 F18 and table P3, the browser itself on Chrome 151: *Smoother*
  sees 62.5 % of a browser broadcaster's frames under bursts at 10 ms and
  70.8 % with the radio away 80 ms in every second (no parity, as before
  RU8); behind a browser carrier prototype, 98.5–99.9 % at 10 ms.

docs/75 OD5 (owner, 2026-10-19) took the browser in two steps: RU9 bounded
the browser's send queue (merged 2026-10-21), and "later, as a milestone of
its own: a browser Reliable mode on RU1's relay ingest". It named two
things this milestone has to settle and one way to measure:

1. **Explain the intermittent *Smoother* dip behind the keyframe gate** at
   1440p on a clean path: 99.5, 99.8 and 73.3 % over three runs (table P8).
2. **Design away or accept the gate's round trip**: holding a GOP's deltas
   until its keyframe is acknowledged costs about one RTT per GOP on a long
   clean path — the Balanced viewer's on-screen latency 100 against 67 ms
   at 40 ms RTT and 183 against 100 ms at 100 ms (table P1).
3. **Measure on a real Chrome on the gaming PC first**, because Chrome's
   per-datagram step (F18, table P6) may depend on the machine.

This doc answers 1 and 2 from a new lab pass (§2, §11) and turns 3 into
chunk BR0, a measurement gate with its procedure, because it cannot run on
the lab host.

## 2. What the lab found (2026-11-10)

Numbers are from §11's tables (Chrome for Testing 151 unless said; viewer
figures the median over four presentation phases, F1). "Balanced" is the
default viewer; "today" is RU9's shipping datagram path.

**F1 — docs/75's *Smoother* dip is the lab's viewer replay, not the gate.**
The replay presents a 60 fps source on an exact 60 Hz vsync and steps its
clock in 0.25 ms; nothing drifts, so the phase between frame targets and
vsyncs is fixed for the whole run. When the targets land within about a
millisecond of the sink's half-vsync due boundary, every third vsync finds
two frames due and shows only the newer — 40 of 60 frames a second, with
no stall, for as long as the playout offset holds. This pass caught it on
an unrelated sender: the inline carrier's Balanced viewer at 1440p, clean,
72.7 % shown and 0.7 % stalled, with transport figures identical to its
neighbours (table S1, r1). Replaying the same arrival trace shifted by
1 ms read 98.0 %; by 2, 4 and 8 ms, 97.3, 99.4 and 99.3 %. Over this pass,
24 of 276 run–preset pairs on the buffered presets had one phase out of
four 5 or more points below the median, with stalled time unchanged,
across every sender (datagrams, gate, inline) — about one in eleven,
which is docs/75's "one run in three" on a single cell. The gate itself
held *Smoother* at 99.5–99.9 % (no parity) in all eight 1440p clean runs
(table S1), and the ungated and inline carriers at 99.6–99.9 %. Single
replays of this lab therefore carry an error of up to a third of the
frames for a reason that has nothing to do with the uplink; §11 reports
the median of four phases (`tools/uplinklab/phase.py`), and docs/75's
tables, single-phase, were not re-scored (their runs are not on this
host). The production sink uses the same half-vsync rule
(`render-sink.ts`) but real clocks drift, so a whole-session lock cannot
happen there (§9).

**F2 — The gate's round trip is real, and the inline keyframe removes
it.** On a clean path the gate adds about a round trip to what the
Balanced viewer sees: 83 against 67 ms at 40 ms RTT, 167 against 100 ms at
100 ms (table S3; docs/75 P1 had 100 and 183 ms). A *Lowest latency*
viewer stalls 5.8 and 22.6 % of the time behind it, against 0.8 and 0.4 %
on datagrams. With the keyframe at the head of the GOP's stream (K3) the
Balanced viewer is at 67 and 100 ms — today's figures — and the live-edge
viewer stalls 0.9 and 3.3 %. At 10 ms all three are at 50 ms.

**F3 — Inline keeps everything the gate bought, and does better where the
gate hurt.** At 10 ms with 0.1–0.5 % loss the Balanced viewer sees
99.1–99.8 % on either carrier (today: 99.3 and 94.6 %); under bursts and
80 ms outages at 10 ms, 97.8–99.4 % on either (today: 84.0 and 76.6 %).
At 40 ms the two part: under bursts inline holds Balanced at 95.7 % and
*Smoother* at 95.3 %, where the gate falls to 83.0 / 83.6 % — below
today's datagrams (84.4 / 78.9 %), as docs/75 F18 found (74.0 against
78.8 %); with outages, 97.6 / 97.9 % against the gate's 94.8 / 95.1 %; at
0.1 % loss 99.9 / 99.6 % against 98.9 / 98.5 %. At 1440p inline is within
one point of the gate or above it on every cell (table S2). Its keyframes
are no slower: p50 23 against 21 ms at 10 ms, 36 against 35 ms at 40 ms,
clean.

**F4 — A browser carrier protects *Smoother* and *Most stable* from the
uplink, as the native one does** (BUGS.md). On today's datagrams, even with
RU8's parity, *Smoother* sees 79.0 % of a browser broadcaster's frames
under bursts and 71.6 % with the radio away 80 ms at 10 ms, 78.9 and
71.0 % at 40 ms. On the inline carrier, 99.9 and 99.0 % at 10 ms, 95.3 and
97.9 % at 40 ms; *Most stable* 100 / 99.9 % and 95.6 / 99.2 % (table S2).

**F5 — Where Chrome's Cubic collapses, nothing carries the stream, and the
retire rule decides whether keyframes still land.** At 40 ms with 0.5 %
loss and at 100 ms with 1 %, every sender loses most frames (Balanced
0.0–63.5 %). Three things differ:

- *The gate is unbounded there*: at 100 ms with 1 % loss its keyframes
  took p50 22 s and the Balanced viewer was 33 s behind, because the
  prototype's keyframe streams are never cancelled and the records wait for
  them (docs/75 F18 saw 13 s).
- *Inline with docs/75 D3's 150 ms reset kills its own keyframes*: of
  ~200 keyframes produced, 63 were acknowledged at 40 ms 0.5 % and **6** at
  100 ms 1 % — the rest abandoned with a blocked write or reset at
  rotation + 150 ms. That is docs/38 F-12's livelock; a viewer joining then
  waits for a keyframe that never lands. Its lower latency on those cells
  (on-screen 367 and 583 ms) is what killing keyframes buys.
- *Inline with RU9's rule (§5.3, `retire=f12`)* landed 199 of 199 and 129
  of 200, as many as today's datagrams (200 and 134), with the Balanced
  viewer at 56.8 and 4.8 % shown (today 46.7 and 3.7 %) and its latency
  bounded at 384 ms and 1.2 s on screen (today 433 and 917 ms).

**F6 — Chrome 158 changes none of it.** Its net-log still configures
`CUBIC_BYTES` for `congestionControl: 'low-latency'`; it still exposes no
`WebTransportSendStream`, `sendOrder` or `getStats()` (151, 156 and 158
probed); at 1440p its datagrams still fall behind (86 % of offered datagram
frames sent, Balanced 75.7 %, the figures docs/75 P10 measured on 151 with
RU9), where its inline carrier put 41.7 Mbps on the wire with the
renderer at 11.7 % of a core and the Balanced viewer at 99.4 % (table S4).

**F7 — Chrome's per-datagram step is not a constant of the machine.** On
the same host and Chrome 151, this pass measured datagram packets 199 µs
apart within a burst (p50) at 10 ms, where docs/75 table P6 measured
442–479 µs at 10 ms. The bytes on the wire, the datagrams sent and every
viewer figure agree between the two passes, so the spacing is not what
limited a 1080p stream here; why it halved is not known. It is one more
reason the gaming PC must be measured rather than inferred (BR0).


## 3. Owner decisions (taken 2026-11-10)

The owner decided OD1 on 2026-11-10 in their own words: **"Get rid of the
extra round trip."** That is K3, the keyframe at the head of its GOP's
stream (D2), not the K1 gate that waits for the keyframe stream's
acknowledgement. The same day the owner took the recommendation below for
OD3–OD6, and for OD2 the half that can be decided before the PC and field
passes: *Lowest latency* stays the default, *Reliable* opt-in, because BR0
and BR5 have not run. Each row keeps the options, what the lab said and
the recommendation that was taken.

| # | Question | Options | What the lab says | Recommendation | Decided (2026-11-10) |
|---|---|---|---|---|---|
| OD1 | How does a GOP's keyframe get ahead of its deltas (§5.2)? | **K1** the gate on acknowledgement, relay unchanged; **K3** the keyframe inline at the head of the GOP's stream, one stream type and one capability bit on the relay (BR1); **K1 now, K3 later** | K1 costs about a round trip on long clean paths (F2: 83 vs 67 ms at 40 ms, 167 vs 100 ms at 100 ms) and falls below today's datagrams under bursts at 40 ms (F3: 83.0 vs 84.4 %). K3 costs nothing on a clean path and matches or beats K1 on every lossy cell measured (F3, F4) | **K3**. It is the only option that meets OD5's "design away" without a heuristic, and the relay change is small, contained in RU1's own carrier reader. On a relay that advertises `CapUplinkCarriers` but not the new bit, the browser sends datagrams (one carrier path to maintain, not two); fleet relays upgrade with the release. K1 first would ship a mode with a known per-GOP cost and then change its wire form. | **K3** (owner: "Get rid of the extra round trip.") |
| OD2 | What does a browser broadcaster get by default? | **Reliable** once BR4 and BR5 hold, as the native apps since 2026-11-05 (docs/75 D4); **Lowest latency**, Reliable opt-in; **Reliable from the first release** | With K3, no cost on a clean path at 10–100 ms (F2), better for every preset under light loss, bursts and outages (F3, F4), no worse where Cubic collapses (F5). Unmeasured: the gaming PC and Chrome on Windows (BR0), a real capture and encode beside it | **Reliable, after BR4's lab criteria and BR5's field pass**, shipping opt-in until then — the order docs/75 D4 first set for the native apps. *Lowest latency* stays a real choice (docs/75 F3's live-edge trade). | **Lowest latency by default, Reliable opt-in**, until BR0 and BR5 hold; the flip to Reliable is decided then (BR5) |
| OD3 | Does the browser get a Delivery setting? | **A** the native control and copy under *Stream quality*; **B** none, Reliable whenever the relay can | The modes differ only for a live-edge viewer on a long lossy path (docs/75 F3); that is the case for a choice, and the native apps offer it | **A**: one control in both apps, the same words (§7). | **A**, as recommended |
| OD4 | Which browsers engage it? | **Chromium only**, Firefox and Safari each after its own BR0-B pass; **every browser with WebTransport streams** | Only Chrome measured (§5.8). Firefox's and Safari's `close()`, schedulers and congestion control are unknown here | **Chromium first** (Chrome, Edge, and other Chromium browsers), Firefox and Safari each when its measurement holds; datagrams until then. | **Chromium only**, as recommended |
| OD5 | Under K3, how is a retired GOP stream ended (§5.3)? | **RU9's rule**: FIN at rotation, queued records shed, aborted only if unacknowledged 2 s after it opened; **docs/75 D3's**: reset 150 ms after the FIN | Under D3's reset, 136 of 199 and 194 of 200 keyframes were never acknowledged where Cubic collapses (F5) — F-12's livelock. RU9's rule landed as many as today's datagrams, at a bounded latency (384 ms and 1.2 s on screen) | **RU9's rule**. It keeps the browser's one keyframe guarantee, and the stale records it lets drain are bounded by what was written before the rotation. | **RU9's rule**, as recommended |
| OD6 | Do shed GOPs step the browser's auto-ladder down? | **No**: the notice asks the broadcaster (RU9's choice); **yes**: a shed GOP is a rung-down signal for `FallbackController` | Not measured. RU9 deferred it here ("the browser Reliable milestone revisits it") | **No, not in this milestone.** No gawk sender's ladder reacts to the network (docs/09, docs/38 D11); a shed GOP under a radio outage is not a capacity signal (docs/75's warning lesson, `docs/gotchas.md`). Revisit with BR5's telemetry. | **No**, as recommended |

## 4. How it was measured

The harness is docs/75 §4's, unchanged except where this section says:
Chrome for Testing replays the lab's real-encoder frame-size traces through
gawk-app's shipping packetizer and parity, over the lab's scripted UDP
path, into the lab receiver (the relay's `webtransport.Server` limits and
`quic.Config`, here at quic-go's 4096-deep session queue, as docs/75's
Chrome columns); the receiver's arrival log is replayed through the
shipping viewer code once per preset.

| | |
|---|---|
| Host | the docs/75 lab host: 8 vCPU (Sandy Bridge-class Xeon), Linux 6.18, loopback; one run at a time (`--jobs 1`) |
| Commit | `32437418` (main, 2026-11-10) plus this PR's lab changes under `tools/uplinklab`. The branch was later rebased onto a newer `main`; nothing the lab runs changed in between (gawk-app's packetizer, `DatagramSender`, `KeyframeSender`, playout and reorder buffer, and the lab receiver) |
| Chrome | Chrome for Testing **151.0.7922.34** for every comparison (docs/75 F18's version), **158.0.8127.18** for table S4 |
| Traces | regenerated by `traces.sh` (x264, the 1080p60 9/12 Mbps and 1440p60 37.5/50 Mbps rungs) |
| Viewer | `gawk-app/lab/uplink-replay.lab.test.ts` at this commit. *Smoother* and *Most stable* are the relay as it ships since RU8 (`resilient-parity`, `deep-parity`: served parity); "Smoother, no parity" is docs/75 table P8's column |
| Host load | not idle: a 7-day-old headless Chrome from another tool held about one core throughout (left running; not this pass's). 1-min load average p50 2.8 (min 1.1, max 11.2) over the pass; CPU steal ≤ 3.2 % in every kept run (median 0.1 %), and the five runs over `matrix.py`'s 5 % were set aside and re-run automatically. Run 2026-11-10 06:40–09:00 EET |

**What is new in the lab** (all under `tools/uplinklab`, this PR):

- `browser/carrier-inline.lab.ts` (+ `.html`): the carrier with the keyframe
  at the head of its GOP's stream (§5.2, K3). The relay as shipped forwards
  a keyframe-flagged `VideoChunk` without caching it
  (`hub.HandleDatagram`), so the page sends the keyframe's StreamFrame
  bytes as keyframe-flagged chunk records: the bytes, and their order on
  the stream, of K3's wire form, plus 22 bytes a 1 024-byte chunk (≈ 2 % of
  a keyframe, which costs K3 slightly here, not the gate). Two
  lifecycles: by default the docs/75 prototype's (D3: queued records of a
  superseded GOP shed, a blocked delta write abandoned, a retired stream
  reset 150 ms after its FIN, plus a one-deep slot for a keyframe produced
  before the current one is handed over); with `retire=f12`, §5.3's rule
  (RU9's limits and 2 s abort on the GOP stream, no reset at retirement).
  Table S1's inline runs are the default; table S2's are `retire=f12` but
  where marked D3. `build.sh` builds it beside the shipping page.
- `analyze.py`: a keyframe whose `F` line declares chunks is complete when
  they have all arrived (the receiver logs records by frame and index), as
  K3's relay would hand it on once its declared length is read.
- `phase.py`: the replay at three more presentation phases (F1).
- `matrix.py`: sets `r84-a`, `r84-b`, `r84-c158`, and modes
  `c<major>-{dgram,carrier,kfa,inline,inline-f12}` that pin a Chrome for Testing by
  major version (`chrome_for`; `CHROME_<major>` overrides).
- `browser/sender.mjs`: `UPLINK_SENDER_PAGE` names the page to load,
  `--retire` is passed to it; a
  request for a directory is a 404 (Chrome 151 asked for `/` mid-run, and
  the server crashed on the read, orphaning the run's Chrome).
- `r84tables.py`: §11's tables, generated from the runs.

BR4 (2026-11-10) adds, for the pass on the shipping code:

- `gawk-app/lab/uplink-sender.lab.ts`: `mode=reliable`, the shipping
  Reliable mode — `KeyframeSender`'s GOP streams and the shipping
  `CarrierSender`, as `BroadcastPipeline` runs them on a relay with both
  carrier bits.
- `gawk-server/cmd/gawk-uplinklab` (`recv`): reads a 0x1F keyframe
  carrier, logging the keyframe complete once its declared length is in,
  as the relay hands it on, then the records.
- `matrix.py`: mode `c<major>-reliable` (on 151, 157 and 158) and set
  `r84-br4` (r84-b's cells and the clean 1440p one, in both delivery
  modes); `r84tables.py`: tables S5 and S6 and G1–G5's verdicts;
  `r84persec.py`: the per-second counters of BR2's notice table test.

**Comparisons.** *Datagrams (today, RU9)* is gawk-app's shipping send path
(RU9's bounded `DatagramSender` and `KeyframeSender`). *Carrier, no gate*
and *carrier + keyframe gate* are docs/75's prototype (one uni stream per
keyframe, fire and forget; records of a GOP held until its keyframe
stream's `close()` resolves, `kforder=ack`). *Carrier, keyframe inline* is
the new page.

**Reproducing** (about 2 h 30, alone on the host):

```sh
tools/uplinklab/build.sh             # or its Go and page halves only: the Rust sender is not used here
tools/uplinklab/traces.sh            # ~15 min
tools/uplinklab/steal.sh             # idle and loaded steal; above ~5 % run at night
python3 tools/uplinklab/matrix.py r84-a --jobs 1      # §11 table S1, ~40 min
python3 tools/uplinklab/matrix.py r84-b --jobs 1      # tables S2, S3, ~1 h 40
python3 tools/uplinklab/matrix.py r84-c158 --jobs 1   # table S4, ~10 min
python3 tools/uplinklab/phase.py --jobs 3             # F1's four phases, ~2 min
python3 tools/uplinklab/r84tables.py                  # §11
```

**Limits.** docs/75 §4's, and: one host and loopback; Chrome headless on
Linux, its renderer doing nothing but send, where the shipping page also
captures and encodes on that thread whenever docs/16's worker path does not
engage; one 100 s run a cell outside table S1, so differences inside docs/75
table M's spread (~5 points Balanced, ~9 Smoother) are not findings. The
inline page's keyframe is ≈ 2 % larger than K3's would be. Nothing here ran
on Windows, macOS, Firefox or Safari.

## 5. Design options

### 5.1 The carrier on RU1's relay ingest — what does not change

RU1 already ingests a publisher's carrier: a uni stream whose prologue is
`version ‖ TypeReliableCarrier (0x0A)`, then `uint16 len ‖ datagram`
records, each ingested exactly as the same bytes sent as a datagram
(`hub.IngestUplinkCarrier`, docs/75 D1–D2), behind `CapUplinkCarriers`
(1 << 2), which gawk-app already names (`CAP_UPLINK_CARRIERS`, `parity.ts`)
and already receives in `RelayCapabilities`. A browser carrier that sends
the records the browser's datagram mode would send is, to the relay, a
native one:

- **Records**: `VideoChunk` and `ParityChunk`, byte for byte what
  `packetizeFrameWithParity` produces at `maxDatagramSize` (1 024 in
  Chrome), parity included (leg B needs it, RU8 serves it to every
  preset). D1's rule, so the relay's accounting, fan-out, stripes, parity
  prefixes and DVR ring see no difference.
- **Limits**: one carrier per GOP; the sender keeps at most two live and
  resets the older before opening a third; the relay accepts three
  (`MaxUplinkCarriers`) and drops records behind its newest keyframe
  (docs/75 D2). The receive windows (D10: 2 MB per stream, 3 MB per
  connection at the 50 Mbps default) hold a 1440p GOP; the browser's ladder
  tops out at 10 Mbps unless its bitrate override is used.
- **Datagrams stay** for audio, the audio config, `TimeSync` and clock
  mappings, and for every delta when the relay has not advertised the bit.

So with the keyframe gate (K1) or no gate (K0) the relay is unchanged. Only
K3 adds to it (§5.2).

### 5.2 Keyframe first — the options

A GOP's deltas are useless until its keyframe lands. The engine puts the
keyframe stream at priority 1 and the carrier at 0 (D3). Chrome cannot: no
version in this pass exposes `WebTransportSendStream` or a `sendOrder`
(§2), so a keyframe stream and a carrier share the link as Chrome's
scheduler pleases. docs/75 F18 measured what that costs: without help the
browser carrier loses to Chrome's own datagrams on light loss at 10 ms,
because keyframes wait behind the carrier (p50 43 / p95 77 ms against
33 / 48 on datagrams).

| | Option | Relay | What it costs | Measured |
|---|---|---|---|---|
| K0 | **No gate**: keyframe on its own stream, records written as they come | unchanged | keyframes share the link with the GOP's records: 97.9 % of frames to Balanced on a clean 10 ms path, 91.7 % at 0.1 % loss (docs/75 P1); 97.2–98.6 % at 1440p, clean (§11 S1) | docs/75 P1–P5; §11 S1 |
| K1 | **Gate on acknowledgement**: a GOP's records wait until its keyframe stream's `close()` resolves (docs/75's `kforder=ack`) | unchanged | one round trip plus the keyframe's drain per GOP before the first delta moves; on-screen latency 83 vs 67 ms at 40 ms, 167 vs 100 ms at 100 ms (§11 S3; docs/75 P1: 100 and 183 ms); unbounded where Cubic collapses (F5) | docs/75 P1–P5, P8; §11 S1–S3 |
| K2 | **Gate on handover**: records wait for the keyframe's `write()` to resolve | unchanged | Chrome takes the data before it sends it, so the records still overtake it on the wire; 93.9–98.6 % on docs/75's five cells, below K1 | docs/75 P5 |
| K3 | **Keyframe inline**: one stream per GOP whose first message is the keyframe, the GOP's records after it | **new**: a stream type and a capability bit (below) | QUIC delivers a stream in order, so the keyframe is first by construction and the deltas follow it at once; the GOP's fate is tied to one stream (§5.3) | §11 S1–S3 (lab stand-in, §4) |
| K4 | **Stream priority** (`sendOrder`) | unchanged | not available: Chrome 151–158 expose none (§2); Firefox's is unmeasured here (§5.8) | — |
| K5 | **Gate with a deadline**: K1, but release the records at most *x* ms after the handover | unchanged | a heuristic: any fixed *x* is wrong on some RTT, and Chrome gives a page no RTT to scale it by (no `getStats()`) | not measured |

**K3's wire form.** A new uni-stream type, `TypeKeyframeCarrier`
(`0x1F`: still free when BR1 landed, checked across every file of `gawk-server/wire`, where `TestWireTypesAreDistinct` now guards it), whose prologue `version ‖ 0x1F` is followed by one
StreamFrame message exactly as a keyframe stream carries it today (header,
embedded decoder config, payload; its length declared in its header,
bounded by `MaxKeyframeBytes`), then RU1's records to the FIN. The relay
peeks the prologue where it already tells a carrier from a keyframe stream
(`classifyUniStream`, `internal/transport/edge_streams.go`), hands the
StreamFrame to the keyframe path (`onKeyframe`: cache, priming, R8 stream
fan-out) **as soon as its declared length is read** rather than at the FIN,
and reads the rest as a carrier: it counts as a carrier (`MaxUplinkCarriers`)
and never takes a keyframe-stream slot. A new capability bit
`CapKeyframeCarriers` (`1 << 3`), advertised only beside
`CapUplinkCarriers`, gates it, because today's relay rejects an unknown
prologue as bad input (docs/75 D2's reason for its own bit). Mirrored in
`wire.ts` and `crates/wire` with golden vectors (CLAUDE.md). Not a record
type inside the 0x0A carrier: a record is bounded by `MaxDatagramSize`, and
the relay's record reader would have to grow a second framing.

### 5.3 Lifecycle: the F-12 rules meet the GOP stream

RU9 gave the browser the engine's keyframe rules (`keyframe-sender.ts`,
docs/38 F-12): one keyframe stream written at a time, at most two in
Chrome, a newer keyframe waits in a one-deep slot, a stream is aborted
only if it is still unacknowledged 2 s after it opened, a keyframe is
never dropped for age, and a GOP's deltas give way while its keyframe
waits behind a late one. The carrier's own rules are docs/75 D3's: nothing
of a superseded GOP survives (queued records shed, a write blocked on a
superseded carrier abandoned and the carrier reset), and a retired carrier
is reset if it is not acknowledged 150 ms after its FIN.

- **With K0/K1 the two never touch.** Keyframes stay on `KeyframeSender`'s
  streams, untouched; the carrier is a second lane whose GOP follows the
  keyframe's outcome: if the keyframe waits in the slot, is replaced,
  discarded, aborted or fails, that GOP's records are shed (RU9's
  superseded-GOP rule already says so for its datagrams).
- **With K3 they share a stream, so they must agree.** D3's 150 ms reset
  after a FIN would kill a keyframe that RU9 would have let drain for up to
  2 s, and on a link slower than a keyframe per GOP every keyframe would
  then die at rotation — F-12's livelock again. Two rules keep K3 on RU9's
  side:
  - *A GOP stream opens only when `KeyframeSender` would start its
    keyframe* (nothing being written, fewer than two in Chrome); a keyframe
    that waits leaves its GOP without a stream, so its deltas are dropped
    (RU9's rule, unchanged).
  - *A retired GOP stream is never reset for its deltas' sake.* At rotation
    its queued records are shed and it is FINned; a write blocked on it
    finishes (Chrome has taken the bytes; nothing more of that GOP is
    written); only RU9's rules end it early (the 2 s age, and as built a
    newer keyframe's registration superseding it while its own keyframe is
    still being written, §8 BR2). The cost is that on a
    link that cannot keep up, up to one GOP of records already handed to
    Chrome is still sent after the next keyframe has started — bounded by
    what was written before rotation, never growing.
  The lab page has both (`retire=f12`, the rule above; and D3's, its
  default). On clean paths they are the same — no GOP stream was still
  unacknowledged at rotation + 150 ms (table S1: 0 of 199 expired in every
  D3 run). Where Chrome's Cubic collapses, D3's reset killed most
  keyframes and this rule kept them (F5); table S2's inline rows are this
  rule.

### 5.4 RU9's bounded queue in Reliable mode

RU9's `DatagramSender` bounds what waits in gawk's queue: video and audio
older than one GOP are dropped, keyframes are never dropped for age, and
deltas give way while their keyframe waits behind a late one. In Reliable
mode deltas leave `DatagramSender` for the carrier lane, so:

- **`DatagramSender` keeps audio and control** (the audio config,
  `TimeSync`, clock mappings), with RU9's 500 ms bound on audio unchanged.
  It is no longer where a backlog of video can form.
- **The carrier lane's bound is the GOP**, docs/75 D3's: a GOP's records
  are shed once a newer keyframe exists, which bounds what gawk queues to
  about one GOP, the same bound RU9 gives datagrams by age. Records already
  handed to Chrome are Chrome's, as datagrams already handed to Chrome are
  (RU9 says the same of its own bound).
- **Writes per frame, not per datagram.** A frame's records go to the
  stream as one `write()` (the prototype's shape), where `DatagramSender`
  awaits each datagram's write. docs/75 table P6: at 1440p the carrier put
  41.3 Mbps on the wire with the renderer's busiest thread at 12.8 % of a
  core, where datagrams managed 22.2 Mbps at 35.9 %. On the main-thread
  pipeline that is CPU handed back to capture and encode (§5.6).
- **The notice** ("Your upload can't keep up", RU9 §6) keeps its rule
  (five bad seconds raise it, fifteen good clear it) with the carrier's
  counters as the stale-drop input: a second is bad on any shed record,
  on two or more keyframe streams superseded or failed, or on a sent rate
  under 75 % of the encode rate — the native monitor's Reliable inputs
  (docs/75 RU5 as built). The native "every retiring carrier expired"
  input is left out: R75 had to restrict it to RTTs under 50 ms (docs/85),
  and a page has no RTT to restrict it by; under K3 there is no 150 ms
  expiry at all (§5.3).

### 5.5 Chrome's constraints

- **Cubic, whatever the page asks** (docs/75 F18, `docs/gotchas.md`); §2
  re-checks it on 158. No delivery mode fixes a path where Cubic cannot
  carry the stream (docs/75 F4, P1's 40 ms 0.5–1 % and 100 ms lossy rows):
  the carrier bounds latency there by shedding and the notice tells the
  broadcaster; the native app stays the answer on long lossy paths (OD5).
- **Per-datagram cost** (F18, table P6): ~375–480 µs between datagram
  packets within a burst on the lab host (199 µs in this pass, F7), about 1 800 datagrams a second at
  most. Stream data does not pay it (P6's carrier row: 50 writes a second,
  41.3 Mbps). On the gaming PC this is BR0's first question.
- **No stream priority, no transport stats** (§2): no `sendOrder`, no
  `getStats()` on 151–158. The only clocks a page has are its own: when
  `write()` resolves (handover to Chrome's network process) and when
  `close()` resolves (acknowledged, `docs/gotchas.md`). K1 runs on the
  second, RU9's late-keyframe rule on the first.
- **One stream per GOP is two streams a second**, with keyframe streams
  beside them (K0/K1) or not (K3); Chrome FINs in a frame of its own and
  the relay reads to the FIN (`docs/gotchas.md`), so stream credit is
  returned. A 24 h broadcast opens ~170 k–350 k streams, inside QUIC's
  2⁶⁰ (docs/75 §10).

### 5.6 The pipeline split (docs/16)

`BroadcastPipeline` runs wherever docs/16 places it — in the broadcast
worker where `MediaStreamTrackProcessor` is available there (Safari), on the
main thread otherwise (Chrome on macOS and Firefox, measured; `BUGS.md`
"Broadcaster falls back to the main-thread pipeline"). The carrier lane
lives inside it, beside `DatagramSender` and `KeyframeSender`, so it runs in
the same realm as the encoder and adds no `postMessage` per frame (docs/16
Decision 9). On the main-thread path it shares the thread with capture and
encode: one `write()` per frame instead of one per datagram is the reason
the carrier is cheaper there (§5.4), and the reason BR0 measures the
shipping page, not only the lab page, whose renderer does nothing but send.

### 5.7 The setting, and the default

The native apps have **Delivery: Reliable · Lowest latency** on the Quality
page, Reliable the default since 2026-11-05 (docs/75 D4, §6). The browser
has no delivery setting. Options:

- **A — the same control** in the broadcaster's Settings, under *Stream
  quality* (where `LadderPicker` already is), with the native copy and its
  hints word for word, persisted beside the other broadcast settings
  (`broadcastSettingsStore`), applied at the next keyframe while live, no
  restart. One mental model for broadcasters who use both apps.
- **B — no control**: Reliable whenever the relay advertises the bit. The
  browser's audience is the zero-install first run; one fewer choice.
- **C — the control, Lowest latency by default** until a field pass.

Whichever default: *Lowest latency* is today's path (RU9), unchanged, and
an operator's `-uplink-carriers=false` turns the carrier off for every
broadcaster on the fleet, browser included, as it does for native ones.

### 5.8 Firefox and Safari

gawk's browser broadcaster also runs on Firefox (main-thread pipeline) and
Safari (worker path). Neither was measured here, and the lab's sender
drives only Chrome. Whether either exposes stream priority was not probed;
K3 works on any WebTransport stack with ordered streams, so the design does
not depend on it. Their congestion controllers, per-datagram costs and `close()`
semantics are unknown: BR0's second half records them, and the mode stays
off on a browser until it is measured — D3's two-in-flight limit and 2 s
abort both read `close()` as the acknowledgement.

## 6. Recommended design

Assuming the recommendations of §3; each decision names the OD it rests
on.

### D1 — The browser carrier is RU1's carrier (OD1)

A per-GOP uni stream whose records are byte for byte the datagrams the
browser's datagram mode would send for the same frames — `VideoChunk`
then `ParityChunk`, at `maxDatagramSize`, parity included (§5.1). Engaged
for a GOP when the relay advertised `CapUplinkCarriers` and
`CapKeyframeCarriers`, the setting is Reliable and the browser is one OD4
allows; otherwise RU9's datagrams, unchanged. Audio, the audio config,
`TimeSync` and clock mappings stay datagrams. One `write()` per frame.

### D2 — The keyframe leads its GOP's stream (K3, OD1)

The GOP stream's prologue is `version ‖ TypeKeyframeCarrier (0x1F)`, then
the keyframe's StreamFrame exactly as a keyframe stream carries it (header,
embedded decoder config, payload), then the GOP's records. The relay hands
the keyframe on when its declared length is read and ingests the rest as
RU1's carrier (§5.2). No gate, no separate keyframe stream in Reliable
mode: QUIC's in-order delivery is the priority Chrome does not expose.

### D3 — The GOP stream follows RU9's keyframe rules (OD5)

- It opens only when `KeyframeSender`'s rule would start its keyframe:
  nothing being written (the previous keyframe handed over) and fewer than
  two GOP streams in Chrome. A keyframe that waits is in RU9's one-deep
  slot; its GOP has no stream and its deltas are dropped, and a newer
  keyframe replaces it.
- At rotation the old stream's queued records are shed, any write in
  progress finishes, nothing more of that GOP is written, and it is FINned.
  It is never reset for its deltas.
- When a keyframe is produced, every GOP stream still unacknowledged 2 s
  after it opened is aborted (RU9's abort check), counted as a failed
  keyframe; an abort discards the waiter.
- A keyframe is never dropped for age. Resume: a new session starts with
  no stream; nothing queued on the old one is sent; the forced keyframe
  opens the first.

### D4 — Queues (RU9)

`DatagramSender` keeps audio and control, with RU9's bounds. The carrier
lane's bound is the GOP (D3). Nothing waits in gawk for longer than one GOP
in either mode.

### D5 — The notice's inputs in Reliable mode

RU9's rule and copy; a second is bad on any shed record, on two or more
keyframe streams superseded or failed, or on sent fps under 75 % of encode
fps (§5.4). Not packet loss (a page cannot see it, and `docs/gotchas.md`
says why it would mislead), not carrier expiry (there is none under D3).

### D6 — Setting and default (OD2, OD3, OD4)

The Delivery control (§7) under *Stream quality*, persisted with the other
broadcast settings, applied at the next keyframe. Ships with *Lowest
latency* as its default; the default becomes *Reliable* once BR4 and BR5
hold, with a blank stored value following it. Engaged on Chromium only
until Firefox and Safari each pass BR0-B. `-uplink-carriers=false` keeps
every browser on datagrams.

### D7 — Telemetry and Diagnostics

docs/75 D8's names, so the playbook rows read a browser broadcaster as a
native one: `uplinkMode`, `uplinkModeRequested`, `uplinkCarriersOpened`,
`uplinkCarriersExpired` (here: GOP streams aborted by the 2 s rule),
`uplinkRecordsShed`. The field registry already has them. RU9's
`staleFramesDropped` and `keyframeStreamsSuperseded` stay.

### D8 — Not a ladder signal (OD6), not a back-channel

Shed GOPs do not reach `FallbackController`. Nothing requests a keyframe or
changes the cadence (docs/15 Decision 6); the only new relay → publisher
signal is one capability bit on a message that already exists.

### What this does not fix

Long lossy paths. Chrome runs Cubic, and at 40 ms with 0.5 % loss or
100 ms with any loss no delivery mode carries the stream (F5, docs/75
F18). Reliable mode bounds the latency and keeps keyframes landing there;
the native app is the answer for a broadcaster on such a path (docs/75
OD5).

## 7. What users see

**The broadcaster** (the browser's Settings panel, under *Stream quality*,
OD3 took option A):

| Element | Copy |
|---|---|
| Label | **Delivery** |
| Segmented control | **Reliable** · **Lowest latency** |
| Hint under *Reliable* | "If your connection drops some video, viewers wait a moment for it instead of losing the picture." |
| Hint under *Lowest latency* | "Viewers never wait for dropped video. If your connection drops some, they see brief freezes." |

- The native copy word for word (`crates/engine/src/carrier.rs`
  `Delivery::label`/`hint`, docs/75 §6), so a broadcaster who uses both apps
  reads one control. No dialog, no warning, no restart: a change applies at
  the next keyframe, live or not.
- The control is the request, as natively (docs/75 D4): on a relay that
  has not advertised the bit, deltas go as datagrams whatever it says, and
  Diagnostics shows the delivery in use beside the one requested. A blank
  stored value follows the default (OD2), as a blank `uplinkDelivery` does
  in the native `broadcast.json`.
- **The notice** ("Your upload can't keep up", RU9) keeps its copy and its
  five-seconds-in, fifteen-out rule; only its inputs change in Reliable
  mode (D5).
- **Diagnostics** (the stats overlay and *Copy diagnostics*): delivery
  requested and in use, carriers opened and aborted, records shed, beside
  RU9's counters. Exempt from docs/57's principle 3, as natively.

**Viewers** choose nothing new and their app does not change. What a
browser broadcaster's viewers get, by preset, from the lab (§2, §11; Chrome
151 on the lab host, BR0 to confirm on the gaming PC):

- **On a clean path** nothing changes at 10–100 ms RTT: the Balanced viewer
  sees the same frames at the same latency as on today's datagrams (F2).
- **Balanced**: light loss at 10 ms stops costing frames (99.1 against
  94.6 % at 0.5 %); bursts and outages stop breaking GOPs (97.8–99.4
  against 76.6–84.0 % at 10 ms; 95.7–97.6 against 75.0–84.4 % at 40 ms).
- **Smoother and Most stable** gain the most: 95.3–100 % where today's
  uplink gives them 71.0–80.1 % under bursts and outages (F4).
- **Lowest latency**: as the native carrier, it can wait briefly more
  often on a long path with steady loss (docs/75 F3); the broadcaster who
  cares picks *Lowest latency* delivery too.

## 8. Chunks and acceptance criteria

### Order

1. **BR0**: the gaming-PC measurement. As designed it came first and
   alone, because it can change OD1 (if Chrome on Windows schedules
   streams or datagrams differently) and OD2. **The owner chose on
   2026-11-10 to build BR1–BR4 before it.** Building behind an opt-in
   setting is safe because nothing changes for anyone who does not pick
   *Reliable*, and BR0 and BR5 still gate the default. BR0 now gates OD2
   and G8, not the code; if it contradicts OD1, OD1 is revisited with its
   numbers.
2. **BR1 and BR2 together** (BR1 because OD1 took K3), behind the
   setting, with *Lowest latency* the default until BR5.
3. **BR3 and BR4** with them: the UI, Diagnostics, telemetry, and the lab
   and CI criteria on the shipping code.
4. **BR5**, the field pass; then the default, per OD2.

### Milestone goals

Pre-registered. "Lab" is §4's harness on the shipping code (BR4), "PC" is
BR0's procedure on the gaming PC, "field" is BR5.

| # | Goal | Verified by |
|---|---|---|
| G1 | **No cost on a clean path**: at 10, 40 and 100 ms RTT, Reliable against *Lowest latency* (RU9's datagrams) — the Balanced viewer's on-screen latency p50 within one vsync (17 ms) and shown within 1 point (four-phase medians) | lab + PC |
| G2 | **Light loss where Chrome's Cubic holds**: at 10 ms with 0.1 and 0.5 % loss, the Balanced viewer ≥ 99 % shown | lab + PC |
| G3 | **Bursts and outages stop freezing the stable presets**: at 10 ms (bursts, radio away 80 ms) *Smoother* and *Most stable* ≥ 98 % shown, and at 40 ms ≥ 95 % (BUGS.md's entry) | lab |
| G4 | **The high rung passes the datagram ceiling**: 1440p at 10 ms, clean and behind 80 Mbps, Balanced ≥ 97 % shown | lab + PC |
| G5 | **Latency stays bounded, and keyframes land, where nothing can carry the stream**: on the cells where Chrome's Cubic collapses (40 ms 0.5 %, 100 ms 1 %), decode-ready p50 under 1.5 s; keyframes acknowledged no fewer than RU9's datagrams on the same cell, less 5 % of those produced; no keyframe ended by a rule other than RU9's 2 s (as built, RU9's newest-registration supersede can also end a retired GOP stream's keyframe, §8 BR2; the lab logs both as aborted, and BR4 judges what ended any other way); the notice raises (its table test, on the lab's counters) | lab + unit |
| G6 | **Compatibility**: on a relay without the bit(s) the browser sends today's datagrams byte for byte; with `-uplink-carriers=false` likewise; an old browser on a new relay is unchanged | integration (real `gawk-server`) + unit |
| G7 | **Viewers untouched**: no viewer code changes; the relay's records from a browser carrier are byte-identical to the datagrams the same frames would have produced | review + unit |
| G8 | **The gaming PC confirms the lab**: BR0's cells within table M's spread of §11's (5 points Balanced, 9 Smoother) or the doc records why not | PC |

Where this pass stands, on the lab page (`retire=f12`) rather than the
shipping code: G1 met (67 / 100 ms at 40 / 100 ms, as datagrams; shown
within 0.4 points), G2 met (99.2, 99.1 %), G3 met (10 ms: 99.0–100 %;
40 ms: 95.3–99.2 %, near its bound under bursts), G4 met (1440p clean
98.9–99.2 % in the four-phase medians of table S1's inline runs; 98.4 %
behind 80 Mbps), G5 met on keyframes (199 / 129 against 200 / 134) and
latency (decode-ready p50 290 ms and 1.07 s). G6–G8 need the code and the
PC.

### BR0 — The gaming-PC measurement (gate)

The pass docs/75 OD5 asked for first. It cannot run on the lab host: it
needs the owner's gaming PC (Windows, Chrome stable, the machine the
browser broadcaster is used on). Two halves.

**A — the lab page on the PC.** Everything on the PC's loopback, so sender,
path and receiver share one clock, as on the lab host:

1. On the PC, a checkout at the BR0 commit, Node, Go and Python.
   `sender.mjs` gains a Windows path in this chunk: `/proc` CPU sampling
   skipped off Linux (an `I,cpu` line of zeros), Chrome found from `CHROME`
   (`C:\Program Files\Google\Chrome\Application\chrome.exe`); `run.sh`'s
   three processes run from Git Bash. Build the Go lab with
   `GOOS=windows` (`build.sh`'s Go half), the page with `build.sh`'s page
   half, the traces on the lab host (`traces.sh`) and copy them over.
2. Record Chrome's version (`chrome://version`), the CPU and GPU, the power
   plan, and that nothing else runs (no game, no capture).
3. Run, one at a time, with `CHROME` and `CHROME_151` both pointing at
   stable Chrome (the `c151-*` modes then run it; the tables are labelled
   with the version from step 2): `matrix.py chrome-ceiling` (table P6's
   ceiling cells), then `r84-b --only rtt10-`, `--only rtt40-clean`,
   `--only rtt100-clean` and `--only hi-`, then `phase.py` and
   `r84tables.py`.
4. Copy `out/results` back; §11 records the PC's tables beside the lab
   host's.

**B — the shipping page with real capture and encode.** The question A
cannot answer: whether capture and encode on the same renderer thread
(docs/16's main-thread path, which Chrome takes today) cost the carrier or
the datagrams anything. On the lab host, `relay.sh`'s real relay
(`gawk-server` at this tree's depth) behind the lab proxy with `SUB=1` (a
lab viewer logging arrivals through the relay); on the PC, the production
broadcaster page from the dev stack (docs/41), publishing over the LAN to
that relay with a game or a 60 fps test scene captured, at 1080p60 (the
default rung) and at 1440p60 with the bitrate override at 37.5 Mbps; each
for 3 min, in each delivery mode, on a clean path and at 10 ms with radio
away 80 ms. Recorded: the broadcaster's own stats (encode fps, sent fps,
`staleFramesDropped`, records shed, keyframes sent/superseded/failed,
whether the notice raised) and, per preset, what the lab viewer saw. The
viewer replay schedules frames by frame number (`sched`), so a small
converter, part of this chunk, builds its trace from the sub log alone:
shown, stalls and the playout offset are measured on the lab host's clock;
absolute glass-to-glass latency is not (two clocks), and the half reports
none.

| Acceptance criterion | Verified by |
|---|---|
| Half A's cells run on the PC with the stable Chrome recorded, every run's steal or CPU contention noted, §11 gains the PC's tables beside the lab host's | manual, recorded |
| Half B's runs recorded as above; in both modes on the clean path the shipping page sends what it encodes (sent fps within 5 % of encode fps, no stale drops, no shed records, notice quiet) at 1080p60 | manual, recorded |
| G8 judged: where the PC differs from the lab by more than the spread, §2 says how, and OD1/OD2 are revisited before BR2 starts | review |
| `sender.mjs` and `run.sh` run on Windows (the Linux runs unchanged: the matrix's own commands still reproduce §11 on the lab host) | manual |

### BR1 — Relay and wire: the keyframe carrier (OD1 took K3)

| Acceptance criterion | Verified by |
|---|---|
| `TypeKeyframeCarrier = 0x1F` and `CapKeyframeCarriers = 1 << 3` allocated in `gawk-server/wire` and mirrored in `wire.ts`/`parity.ts` and `crates/wire`, golden vectors byte-identical in all three (CLAUDE.md) | unit (mirror tests) |
| The relay advertises `CapKeyframeCarriers` only beside `CapUplinkCarriers`; `-uplink-carriers=false` sends neither and rejects a 0x1F stream as bad input, byte-identically to today | unit (diff) |
| A 0x1F stream's StreamFrame reaches `onKeyframe` (cache, priming, R8 fan-out) **when its declared length has been read**, before the stream's FIN; its records after it are ingested as RU1's; red first: a test that holds the FIN back and expects the keyframe in the cache | unit, red first |
| A 0x1F stream counts against `MaxUplinkCarriers` and never takes one of `acceptKeyframeStreams`' four slots; a StreamFrame larger than `MaxKeyframeBytes`, a malformed one, or one with the keyframe flag clear ends the stream (`CancelRead`) with nothing cached | unit |
| A hostile publisher holding a hole inside the keyframe part keeps at most its connection receive window of relay memory (RU1's test, extended) | unit |
| A commit touching `gawk-broadcast-desktop/` in the same PR for the `crates/wire` mirror (CONTRIBUTING) | review |

**As built (2026-11-10).** `0x1F` and `1 << 3` were still free (every
`Type*` constant in `gawk-server/wire`, not only `wire.go`'s header list;
`TestWireTypesAreDistinct` walks the package's sources so a collision now
fails a test). Golden vectors `011f` (prologue), `010f000f02` (all four
capability bits, parity 2) and a whole stream head (prologue, the golden
StreamFrame, one record) are byte-identical in `wire/keyframe_carrier_test.go`,
`wire.test.ts`/`parity.test.ts` and `crates/wire/tests/golden.rs`.

- **No new knob.** The bit rides `-uplink-carriers`: on, the relay
  advertises `CapUplinkCarriers | CapKeyframeCarriers` on publish sessions;
  off, neither, and a 0x1F stream fails the keyframe reader as bad input
  as before (`TestKeyframeCarrierRejectedWithCarriersOff`; the publisher's
  capabilities stay `010f000302`).
- **Where.** `hub.IngestKeyframeCarrier` (`internal/hub/uplink.go`) reads the
  prologue and the StreamFrame, calls `onKeyframe` as soon as the declared
  length is in, then runs RU1's record loop (`ingestUplinkRecords`, shared
  with `IngestUplinkCarrier`). `classifyUniStream` routes 0x1F to it on a
  publisher's session only, giving the keyframe-stream slot back after the
  2-byte peek as it does for 0x0A; an edge's upstream never sends one, so an
  edge still reads 0x1F as an unreadable keyframe stream
  (`TestEdgeStreamsKeyframeCarrierIsNotRead`). A PoP's own publish route is
  the relay's (`handlePublish`), so a browser behind a PoP gets the bit and
  the PoP's leg to home carries the keyframe as a keyframe stream, unchanged;
  the leg's own capabilities (`sendLegCapabilities`) do not carry it.
- **Rejections.** A head that is not a keyframe StreamFrame (another type,
  a bad version, the keyframe flag clear) is counted as bad input, one over
  `MaxKeyframeBytes` as oversize; a stream cut short inside its keyframe
  (RU9's 2 s abort) is neither. All three are reset with nothing cached or
  fanned out (`TestKeyframeCarrierRejectsBadKeyframes`).
- **Memory.** The keyframe part is read into a buffer that grows with the
  bytes read (64 KiB steps, doubling), never allocated whole from the
  declared length. `TestKeyframeCarrierHostileHoleIsBounded`: three
  keyframe carriers each declaring an 8 MiB keyframe with a hole 64 KiB in;
  the relay's QUIC held **2.80 MB** past the holes (2 803 308 bytes, RU1's
  figure) and the process heap grew **4.4 MB**, inside the 3 MB initial /
  15 MiB maximum connection window. Allocating the declared length up front
  grew it **29.4 MB** (the test's red run).
- **Tests** (red first, before the code): the keyframe in the cache and on
  a subscriber's keyframe stream while the FIN is held back
  (`TestKeyframeCarrierKeyframeLandsBeforeFIN`, and over a real quic-go
  session `TestKeyframeCarrierOverRealSession`); RU1's property test over
  random GOPs, keyframe carriers against keyframe streams plus datagrams
  (`TestKeyframeCarrierIngestsExactlyAsKeyframeStreamAndDatagrams`); the
  shared `MaxUplinkCarriers` and no keyframe-stream slot taken
  (`TestKeyframeCarrierCountsAgainstMaxUplinkCarriers`,
  `TestKeyframeCarriersTakeNoKeyframeStreamSlot`).
- **gawk-ios** compiles `crates/wire` by path but neither uses nor
  receives the new type or bit (the native engine does not open keyframe
  carriers, §10), so the PR carries no `gawk-ios/` commit, as RU1's bit
  did not; its Rust gates were run against the change.

### BR2 — The browser's carrier lane

A `CarrierSender` beside `DatagramSender` and `KeyframeSender` in
`transport/`, owned by `BroadcastPipeline`, so it runs wherever the
pipeline runs (docs/16).

| Acceptance criterion | Verified by |
|---|---|
| **Engage** (D1, D6): capability (both bits under K3) ∧ Reliable ∧ a browser D6 allows ⇒ the carrier for the next GOP; anything else ⇒ RU9's datagrams, byte-identical to today; a change applies at the next keyframe; the first GOP of a session goes as datagrams if `RelayCapabilities` has not arrived when its keyframe is produced, as natively (docs/75 RU5 as built) | unit, test-first |
| **Records** are byte-identical to the datagrams datagram mode sends for the same frame, parity included, chunked at `maxDatagramSize`; a frame's records go to the stream in one `write()` | unit |
| **Keyframe first** (D2): under K3 a GOP stream's first message is the keyframe's StreamFrame (prologue 0x1F); under K1 a GOP's records wait for its keyframe stream's acknowledgement; red first with a fake transport that delivers stream bytes in write order | unit, red first |
| **Lifecycle** (D3), with a fake transport whose writes block, under fake timers: a GOP stream opens only when `KeyframeSender` would start its keyframe; a keyframe that waits leaves its GOP without a stream and its deltas are dropped; at rotation queued records are shed, the stream FINned, a blocked write left to finish and nothing more of that GOP written; a stream is aborted only by RU9's rules (the 2 s age; newest registration, as built), never for its deltas; at most two GOP streams are in Chrome; the encoder never waits | unit (fake clock) |
| **Resume**: a new session starts with no carrier; nothing queued on the old one is sent on the new; the forced keyframe opens the first GOP stream once the capability is known | unit |
| **Counted** (D7): carriers opened, aborted; records shed; delivery requested and in use — in `BroadcastStats`, Diagnostics and telemetry with docs/75 D8's names (`uplinkMode`, `uplinkModeRequested`, `uplinkCarriersOpened`, `uplinkCarriersExpired` for aborts, `uplinkRecordsShed`) | unit |
| **The notice's inputs** (D5) in Reliable mode: a second is bad on any shed record, on two or more keyframe streams superseded or failed, or on sent fps under 75 % of encode fps; table tests that replay the lab's per-second counters | unit |
| No `postMessage` per frame added on the worker path; the main-thread path's per-frame work is one `write()` | review |

**As built (2026-11-10).** One F-12 state machine for keyframes in both
modes: a keyframe produced with `carrier` (`KeyframeSender.produce(gop,
msg, { carrier })`, `keyframe-sender.ts`) opens a GOP stream instead of a
keyframe stream — it writes `01 1f` and the StreamFrame, counts toward the
two in Chrome, must be handed over before the next keyframe starts and is
ended early only by RU9's rules. Unlike a keyframe stream it is not closed on
handover: the carrier lane (`CarrierSender`, `carrier-sender.ts`) is told
it can take records, and when a newer keyframe is produced (of either mode)
it is retired — its queued records shed, any write in progress left to
finish, then FINned; never reset for its deltas.

- **Records.** A delta's `packetizeFrameWithParity` output at
  `wt.datagrams.maxDatagramSize`, framed as records and written as **one**
  `write()`; the same datagrams datagram mode sends, byte for byte (a
  pipeline test runs both modes over the same frames). One write in flight
  per GOP stream, so a retired GOP's queue can still be shed; a GOP's
  deltas queue while its stream opens and its keyframe is written.
- **Engage** (`engageCarrier`): both relay bits on this session ∧
  *Reliable* ∧ `isChromium(navigator.userAgent)`, decided when each keyframe
  is produced. The capability flags are per session (cleared on every
  resume), so a GOP whose keyframe beats them goes as datagrams. A change
  of setting applies at the next keyframe.
- **Shed.** A delta whose GOP has no stream (its keyframe waits in the slot,
  or was replaced, discarded, aborted or failed) or whose stream was retired
  is shed, and the rest of that GOP with it. Counted as records
  (`uplinkRecordsShed`, docs/75 D8's unit), not in RU9's
  `staleFramesDropped`, which stays the datagram queue's.
- **Counters** (`BroadcastStats`, so Diagnostics, *Copy diagnostics* and
  telemetry): `uplinkModeRequested`, `uplinkMode`, `uplinkCarriersOpened`,
  `uplinkCarriersExpired` (GOP streams aborted by the 2 s rule, also counted
  in `keyframeStreamsFailed`), `uplinkRecordsShed`. One more RU9 rule can
  reset a GOP stream: when two keyframes open at once (a forced keyframe
  right after a cadence one), the newer one's registration supersedes the
  older, already retired and still writing its keyframe — RU9's "newest
  registration wins", counted superseded and failed but not as an expiry.
  A retired keyframe never waits again, and one that opens after a newer
  GOP stream registered releases its own empty stream instead of
  superseding the newer, live one (review of #765). Neither is a
  reset for the deltas' sake. A GOP must stay well under the 2 s abort age:
  a GOP stream is unacknowledged until its rotation, so at the 500 ms default
  it has about 1.5 s after its FIN (a test pins the default). gawk-telemetry already
  types them (R73); its catalogue descriptions now say what each means
  from a browser.
- **The notice** reads `staleFramesDropped + staleAudioDropped +
  uplinkRecordsShed` as its drop input, so in Reliable mode a second is bad
  on any shed record. Replayed second by second over this doc's lab runs
  (`c151-inline-f12`, reduced by `tools/uplinklab/r84persec.py` into
  `uplink-monitor.r84-fixture.ts`): quiet on every clean path at 10–100 ms,
  0.5 % loss and bursts at 10 ms, and outages at 10 and 40 ms; raised at
  40 ms with 0.5 % loss (from second 13), 100 ms with 1 % (second 9), and
  under 40 ms bursts (second 32, up 58 of 100 s, where records were shed).
- **Worker path.** `setDelivery` is a command beside `setEncoderSettings`
  and rides the start command; the lane runs inside `BroadcastPipeline`, so
  nothing is posted per frame.
- **Tests.** `carrier-sender.test.ts` (red first: K3's stream head, records
  byte-identical in one write, the lifecycle under fake timers with a
  transport whose writes block, resume, engage, Chromium detection);
  `broadcaster-carrier.test.ts` (the real pipeline: Reliable end to end,
  byte-identical records against Lowest latency, today's bytes on every
  non-engaging relay, setting and browser, late capabilities, a live change,
  shed and aborted GOPs, the notice, resume); `uplink-monitor.test.ts`
  (the lab table).

### BR3 — What users see (§7)

| Acceptance criterion | Verified by |
|---|---|
| The Delivery control (OD3 = A) with §7's exact copy, under *Stream quality*; persists in `broadcastSettingsStore`; a blank value follows the default; applies at the next keyframe without a restart | unit + manual |
| No docs/57 principle-3 term in a main-flow string (the existing string test, extended to the new strings) | unit |
| Diagnostics and *Copy diagnostics* show §7's rows | unit |
| The notice reads as RU9's, reachable by keyboard and screen reader as RU9 requires | unit |

**As built (2026-11-10).** `DeliveryPicker` (`features/stream`) under
*Stream quality*, after the ladder: a two-choice radio group labelled
**Delivery**, *Reliable* · *Lowest latency*, the selected choice's hint under
it (and describing the group for screen readers), §7's words, tested
against the native strings. Stored as `gawk.uplinkDelivery`; blank follows
`DEFAULT_DELIVERY` (*Lowest latency*), and a word is stored only once the
broadcaster picks something other than what blank shows, as the native
Quality page does. The screen passes it to the pipeline before start and
live on change. Diagnostics: *Delivery (requested)*, *Delivery (in use)*,
*Carriers opened*, *Carriers aborted*, *Records shed*; *Copy diagnostics*
carries the stats fields. The string test is the native one
(`lossnotice.rs`'s list, plus "datagram") over the new strings; the notice's
copy and its accessibility are RU9's, unchanged and still tested there.

### BR4 — Lab and CI on the shipping code

| Acceptance criterion | Verified by |
|---|---|
| The lab page (`gawk-app/lab/uplink-sender.lab.ts`) gains a Reliable mode that runs the shipping `CarrierSender`, as RU9's datagram mode runs the shipping `DatagramSender` | review |
| `r84-b`'s cells re-run on it (Chrome 151 and the current stable), four-phase medians: G1–G5 judged and recorded in §11 | lab |
| The e2e harness's browser-broadcast mode (`e2e/run.mjs --browser-broadcast`) gains an uplink-loss pass in Reliable mode: the R29 forwarder between the browser broadcaster and the relay drops bursts, and a *Smoother* viewer sees no freeze where the same pass on *Lowest latency* does (flow-shaped, docs/25 Decision 6) | e2e (tier 1) |

**Run (2026-11-10).** The lab page's `mode=reliable` on this tree,
`r84-br4` on this host (§4's: 8 vCPU, loopback, one run at a time), Chrome
for Testing **151.0.7922.34** (docs/88's), **157.0.8103.50** (Linux stable
that day) and **158.0.8127.18**, then `phase.py`; 116 runs of 100 s, CPU
steal ≤ 2.2 % in every kept run (`UPLINK_STEAL_MAX=3`; eight set aside
and re-run). The Reliable cells first run with the page built before
#765's review fixes were re-run on the page built from `00d0f29c` (#765's
head); every Reliable figure below is that code. §11 tables S5 and S6.

- **G1 — met on all three.** Reliable and *Lowest latency* put the
  Balanced viewer on screen at the same p50, 50 / 67 / 100 ms at 10 / 40 /
  100 ms, on 151, 157 and 158; shown within 0.2 point (four-phase medians,
  99.7–100.0 %). The keyframe gate's round trip (docs/75 P1, §11 S3: 83 and
  167 ms) is gone on the shipping code.
- **G2 — met**: Balanced 99.2–99.5 % at 10 ms with 0.1 and 0.5 % loss
  (*Lowest latency* 74.2–97.3 %).
- **G3 — met on 157 and 158; on 151 missed by 0.02 point** at 40 ms under
  bursts (*Smoother* 94.99 %, *Most stable* 95.5 %; 157 95.3 / 95.6 %, 158
  95.3 / 95.7 %), one 100 s run, inside docs/75 table M's ~9-point
  *Smoother* spread. Everywhere else 98.0–100 % (*Lowest latency* 69.1–
  80.7 %).
- **G4 — met**: 1440p at 10 ms, clean 99.3–99.6 % and behind 80 Mbps
  98.6–98.8 % (*Lowest latency* 69.2–77.3 %, past Chrome's datagram
  ceiling).
- **G5 — met**: where Cubic collapses, decode-ready p50 251–260 ms (40 ms,
  0.5 %) and 888–928 ms (100 ms, 1 %); keyframes acknowledged 133–136 of
  200 against *Lowest latency*'s 136–140, within 5 % of those produced;
  none ended by anything but RU9's rules. The notice half is BR2's table
  test.
- *Lowest latency*'s own figures at 10 ms with light loss read lower than
  §11 S2's (Balanced 95.6 / 74.2 % against 99.3 / 94.6 % at 0.1 / 0.5 %
  on 151). Its code did not change; why the pass differs is not known, and
  no goal compares against it there.

**The e2e pass (2026-11-10).** `e2e/run.mjs --browser-broadcast` gains two
uplink-outage passes, a browser broadcaster behind a forwarder that drops
every uplink packet for 150 ms of each 1.5 s, on *Lowest latency* and on
*Reliable* (seeded through the Delivery setting's storage key), each
watched by a *Smoother* viewer for 12 s. The criterion as written — no
freeze on Reliable — **does not hold literally**, and the reason is in the
design, not the code: when an outage straddles a GOP rotation, the next
GOP's stream can land its keyframe at the relay before QUIC has
retransmitted the old GOP's tail, and the relay drops that tail as stale
(docs/75 D2), so the viewer jumps to the newer keyframe (a gap resync onto
a keyframe it holds, not a wait for one). That is the share of table S5's
98–99 % that is not 100. Six local runs: *Lowest latency* 7–9 resyncs, every
one a freeze to the next keyframe; *Reliable* 0–6, in every run that logged it with stale records
dropped at the relay, nothing shed, the carrier engaged. The pass asserts
exactly that (the control froze; Reliable engaged, shed nothing, resynced
less than the control, and only where the relay dropped a stale tail). The
relay's ingress-loss window, which would show the uplink losing nothing,
finalizes a frame ~34 s later and cannot judge a 12 s pass. Forwarding a
GOP's stale tail to reliable viewers instead of dropping it is a relay
question for BR5's telemetry, not this milestone's.

### BR5 — The field pass, then the default

| Acceptance criterion | Verified by |
|---|---|
| The owner broadcasts from Chrome on the gaming PC to the official fleet for 10 min in each mode, over the home uplink and over one real path ≥ 40 ms to the fleet; telemetry compares leg-A damaged frames (`ingressFrameLossRatio`), the viewers' gap resyncs per preset and the notice | manual, telemetry |
| The default (OD2) flips only after BR4's G1–G5 and this pass hold; the reason for anything that does not flip is recorded in §2 | review |
| `BUGS.md`'s entry updated in the same PR: the browser half fixed, the entry removed if nothing else is left open | review |

## 9. Risks

- **The lab is one host and headless Chrome on Linux.** Chrome's
  per-datagram step and its scheduler between streams may differ on
  Windows and macOS; the renderer here does nothing but send. BR0 exists
  for this. The code ships before it, opt-in (§8 *Order*); the default
  does not change before it.
- **Chrome can change underneath.** No priority API, no stats, Cubic
  regardless (§5.5): a future Chrome that adds `sendOrder` or BBR changes
  the trade-offs (K4 becomes possible, F4's collapse moves). Table S4 is the
  check to repeat per Chrome major; the design does not depend on either.
- **K3 ties a keyframe to its GOP's deltas** (§5.3). The F-12 rules keep
  the keyframe safe, at the cost of up to one GOP of stale records still
  draining after a rotation on a link that cannot keep up. Measured on the
  collapse cells (table S2), not on a real thin uplink.
- **The viewer replay's phase lock** (F1) means single-run "shown" figures
  in this lab can be off by up to a third of the frames for reasons that
  have nothing to do with the uplink. §11 reports the four-phase median;
  docs/75's tables are single-phase and were not re-scored (their runs are
  not on this host).
- **The production sink has the same half-vsync rule** (`render-sink.ts`).
  A real display and a real capture clock drift, so the replay's whole-run
  lock cannot happen there, but a source and display at the same rate could
  sit near the boundary for a while. Not measured; a viewer question, not
  this milestone's.
- **Long lossy paths stay broken in the browser.** Chrome's Cubic collapses
  at 40 ms with 0.5 % loss and at 100 ms with any loss (docs/75 F18); the
  carrier bounds the latency there, it cannot create bandwidth. The native
  app is the answer (OD5).
- **Firefox and Safari are unmeasured** (§5.8). Engaging the mode on a
  browser whose stream semantics differ could cost what it means to save;
  D6 keeps it off there until BR0's second half has run.

## 10. Out of scope

- **A viewer change.** Viewers receive RU1's records and R8's keyframe
  streams exactly as today; nothing in the viewer changes. The replay's phase lock (F1) is the lab's; whether
  the real sink has a cousin of it belongs to a viewer milestone.
- **A home-grown congestion controller in the browser.** Chrome's QUIC
  stack decides; a page cannot replace it (docs/75 §9's reason, and the
  browser cannot even choose).
- **Datagram-mode changes.** *Lowest latency* is RU9's path, unchanged.
- **The native engine adopting K3.** It has stream priority and does not
  need it; one sender per mechanism.
- **A keyframe request or a cadence change** of any kind (docs/15
  Decision 6). Rejected for good.
- **Changing the rung on loss** (OD6, docs/09, docs/38 D11): the notice asks
  the broadcaster.
- **WebRTC, MoQ**, and everything else on CLAUDE.md's rejected list.

## 11. Measurements

Generated by `tools/uplinklab/r84tables.py` from the runs of §4 (one 100 s
run a cell, viewer figures the second half); nothing below is transcribed
by hand. "Undecodable" counts frames a viewer could not decode for want of
data. *Deltas shed*: delta frames the sender dropped (`D` lines: RU9's
stale drops for datagrams, records shed for a carrier). Runs set aside for
CPU steal over 5 % (`*.steal<n>` beside a cell) are not in the tables.

#### Table S1 — docs/75 F18's Smoother dip: 1440p, 10 ms RTT, clean, run by run

`matrix.py r84-a`, Chrome for Testing 151, then `phase.py`. Each viewer cell: shown at the matrix's own
replay (phase 0) → shown · stalled, the median over four presentation phases (F1), with the worst
phase when it is 5 points or more below the median. *Superseded*: frames the Balanced and the
Smoother (no parity) sinks replaced before showing them, at phase 0.

| Run | Sender | Balanced | Smoother, no parity | Smoother | Most stable | Superseded: Balanced · Smoother | Decode-ready p50 / p99 ms · undecodable | Keyframes p50 / p95 ms | Carriers expired | Steal |
|---|---|---|---|---|---|---|---|---|---|---|
| r1 | carrier + keyframe gate | 99.8 % → 99.6 % · 0.2 % | 99.6 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 23 · 52 | 9 / 52 · 0.0 % | 33 / 64 | 0 of 199 | 0.1 % |
| r2 | carrier + keyframe gate | 99.8 % → 99.6 % · 0.2 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 13 · 22 | 10 / 52 · 0.0 % | 36 / 61 | 0 of 199 | 0.1 % |
| r3 | carrier + keyframe gate | 99.7 % → 99.6 % · 0.1 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 15 · 22 | 10 / 51 · 0.0 % | 36 / 61 | 0 of 199 | 0.1 % |
| r4 | carrier + keyframe gate | 99.7 % → 99.4 % · 0.4 % (one phase 85.4) | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 18 · 22 | 10 / 52 · 0.0 % | 35 / 69 | 0 of 199 | 0.1 % |
| r5 | carrier + keyframe gate | 99.7 % → 99.5 % · 0.3 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 25 · 22 | 10 / 54 · 0.0 % | 34 / 66 | 0 of 199 | 0.1 % |
| r6 | carrier + keyframe gate | 99.8 % → 99.6 % · 0.2 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 143 · 151 | 10 / 53 · 0.0 % | 36 / 65 | 0 of 199 | 0.1 % |
| r7 | carrier + keyframe gate | 99.7 % → 99.6 % · 0.1 % (one phase 91.7) | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 19 · 22 | 10 / 51 · 0.0 % | 33 / 61 | 0 of 199 | 0.1 % |
| r8 | carrier + keyframe gate | 99.7 % → 99.5 % · 0.3 % | 99.5 % → 99.8 % · 0.1 % | 99.8 % · 0.1 % | 99.9 % · 0.1 % | 214 · 253 | 10 / 54 · 0.0 % | 33 / 64 | 0 of 199 | 0.1 % |
| r1 | carrier, no gate | 98.3 % → 98.9 % · 0.6 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 63 · 21 | 10 / 51 · 0.0 % | 36 / 67 | 0 of 199 | 0.1 % |
| r2 | carrier, no gate | 98.6 % → 99.1 % · 0.7 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 158 · 112 | 9 / 53 · 0.0 % | 36 / 73 | 0 of 199 | 0.1 % |
| r3 | carrier, no gate | 97.2 % → 98.0 % · 1.6 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 71 · 22 | 10 / 52 · 0.0 % | 36 / 69 | 0 of 199 | 0.1 % |
| r4 | carrier, no gate | 98.3 % → 98.7 % · 0.9 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 62 · 22 | 10 / 51 · 0.0 % | 37 / 68 | 0 of 199 | 0.1 % |
| r1 | carrier, keyframe inline (D3) | 72.7 % → 99.1 % · 0.4 % (one phase 72.7) | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 828 · 21 | 10 / 51 · 0.0 % | 37 / 63 | 0 of 199 | 0.1 % |
| r2 | carrier, keyframe inline (D3) | 99.1 % → 99.1 % · 0.7 % | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 57 · 21 | 10 / 53 · 0.0 % | 38 / 69 | 0 of 199 | 0.1 % |
| r3 | carrier, keyframe inline (D3) | 99.6 % → 99.2 % · 0.4 % (one phase 85.5) | 99.9 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 24 · 22 | 10 / 56 · 0.0 % | 38 / 72 | 0 of 199 | 0.1 % |
| r4 | carrier, keyframe inline (D3) | 98.5 % → 98.9 % · 0.6 % | 99.6 % → 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 152 · 148 | 10 / 53 · 0.0 % | 37 / 70 | 0 of 199 | 0.1 % |

#### Table S2 — the keyframe gate against the keyframe inline, with today's datagrams

`matrix.py r84-b`, Chrome for Testing 151, one 100 s run a cell, then `phase.py`. Viewer columns:
shown · stalled, the median over four presentation phases (F1); the Balanced viewer's on-screen
latency p50 at phase 0. *Inline* is `retire=f12` (§5.3) unless marked D3.

| RTT | Path | Sender | Balanced | Lowest latency | Smoother | Most stable | Decode-ready p50 / p99 ms · undecodable | Keyframes p50 / p95 ms | Deltas shed | Steal |
|---|---|---|---|---|---|---|---|---|---|---|
| 10 ms | clean | datagrams (today, RU9) | 100.0 % · 0.0 % · 50 ms | 96.0 % · 0.8 % (one phase 90.3) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 26 · 0.0 % | 20 / 38 | 0 | 0.1 % |
| 10 ms | clean | carrier + keyframe gate | 99.9 % · 0.0 % · 50 ms | 96.2 % · 2.4 % (one phase 88.2) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 6 / 30 · 0.0 % | 21 / 43 | 0 | 0.1 % |
| 10 ms | clean | carrier, keyframe inline | 99.9 % · 0.0 % (one phase 86.9) · 50 ms | 96.7 % · 1.8 % (one phase 89.9) | 99.7 % · 0.0 % (one phase 86.7) | 100.0 % · 0.0 % (one phase 86.0) | 6 / 28 · 0.0 % | 23 / 40 | 0 | 0.1 % |
| 40 ms | clean | datagrams (today, RU9) | 100.0 % · 0.0 % · 67 ms | 94.8 % · 0.8 % (one phase 86.7) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 23 / 38 · 0.0 % | 34 / 47 | 0 | 0.1 % |
| 40 ms | clean | carrier + keyframe gate | 99.7 % · 0.1 % · 83 ms | 92.1 % · 5.8 % | 99.7 % · 0.0 % | 100.0 % · 0.0 % | 21 / 64 · 0.0 % | 35 / 55 | 0 | 0.1 % |
| 40 ms | clean | carrier, keyframe inline | 100.0 % · 0.0 % · 67 ms | 97.1 % · 0.9 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 21 / 39 · 0.0 % | 36 / 49 | 0 | 0.1 % |
| 100 ms | clean | datagrams (today, RU9) | 100.0 % · 0.0 % · 100 ms | 94.0 % · 0.4 % (one phase 88.5) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 54 / 71 · 0.0 % | 67 / 78 | 0 | 0.1 % |
| 100 ms | clean | carrier + keyframe gate | 99.6 % · 0.4 % · 167 ms | 79.0 % · 22.6 % (one phase 71.3) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 52 / 158 · 0.0 % | 68 / 92 | 0 | 0.1 % |
| 100 ms | clean | carrier, keyframe inline | 99.6 % · 0.2 % · 100 ms | 95.7 % · 3.3 % (one phase 86.6) | 99.8 % · 0.0 % | 100.0 % · 0.0 % | 51 / 79 · 0.0 % | 70 / 98 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | datagrams (today, RU9) | 99.3 % · 0.4 % · 117 ms | 89.6 % · 11.8 % (one phase 82.8) | 99.7 % · 0.0 % | 100.0 % · 0.0 % | 9 / 167 · 0.0 % | 56 / 98 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | carrier + keyframe gate | 99.8 % · 0.3 % (one phase 66.6) · 100 ms | 87.2 % · 11.5 % (one phase 81.5) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 94 · 0.0 % | 57 / 102 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | carrier, keyframe inline | 99.2 % · 0.5 % · 83 ms | 85.5 % · 12.5 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 79 · 0.0 % | 58 / 94 | 0 | 0.1 % |
| 10 ms | 0.5 % loss | datagrams (today, RU9) | 94.6 % · 5.5 % · 167 ms | 66.0 % · 34.2 % | 98.9 % · 0.8 % | 100.0 % · 0.0 % | 10 / 312 · 1.4 % | 120 / 189 | 70 | 0.1 % |
| 10 ms | 0.5 % loss | carrier + keyframe gate | 99.6 % · 0.3 % · 133 ms | 77.9 % · 19.0 % | 99.8 % · 0.1 % | 100.0 % · 0.0 % | 11 / 133 · 0.0 % | 98 / 150 | 0 | 0.1 % |
| 10 ms | 0.5 % loss | carrier, keyframe inline | 99.1 % · 0.7 % (one phase 83.3) · 133 ms | 77.7 % · 19.4 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 11 / 132 · 0.0 % | 97 / 147 | 0 | 0.1 % |
| 10 ms | bursts (GE) | datagrams (today, RU9) | 84.0 % · 17.7 % · 83 ms | 69.6 % · 33.9 % (one phase 63.6) | 79.0 % · 22.6 % | 80.1 % · 19.6 % | 9 / 120 · 20.1 % | 85 / 141 | 0 | 0.1 % |
| 10 ms | bursts (GE) | carrier + keyframe gate | 99.0 % · 0.7 % · 117 ms | 80.3 % · 17.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 9 / 112 · 0.0 % | 81 / 132 | 0 | 0.1 % |
| 10 ms | bursts (GE) | carrier, keyframe inline | 99.4 % · 0.4 % · 100 ms | 82.2 % · 15.6 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 101 · 0.0 % | 74 / 119 | 0 | 0.1 % |
| 10 ms | radio away 80 ms | datagrams (today, RU9) | 76.6 % · 24.9 % · 117 ms | 66.2 % · 36.0 % | 71.6 % · 29.6 % | 71.6 % · 31.0 % | 9 / 82 · 28.1 % | 56 / 182 | 0 | 0.1 % |
| 10 ms | radio away 80 ms | carrier + keyframe gate | 97.8 % · 2.2 % · 150 ms | 79.2 % · 22.3 % (one phase 73.7) | 98.7 % · 1.1 % (one phase 84.8) | 99.5 % · 0.6 % | 7 / 162 · 0.4 % | 56 / 173 | 4 | 0.3 % |
| 10 ms | radio away 80 ms | carrier, keyframe inline | 97.8 % · 2.6 % · 133 ms | 80.0 % · 21.8 % (one phase 72.6) | 99.0 % · 0.8 % | 99.9 % · 0.1 % | 7 / 155 · 0.1 % | 53 / 180 | 4 | 0.3 % |
| 40 ms | 0.1 % loss | datagrams (today, RU9) | 90.4 % · 9.8 % · 217 ms | 66.5 % · 34.5 % | 94.3 % · 6.8 % | 100.0 % · 0.0 % | 28 / 262 · 0.0 % | 172 / 292 | 0 | 0.1 % |
| 40 ms | 0.1 % loss | carrier + keyframe gate | 98.9 % · 1.2 % · 267 ms | 70.1 % · 33.0 % | 98.5 % · 1.4 % | 98.9 % · 1.2 % | 45 / 255 · 1.1 % | 143 / 227 | 18 | 0.1 % |
| 40 ms | 0.1 % loss | carrier, keyframe inline | 99.9 % · 0.0 % · 217 ms | 75.3 % · 26.9 % | 99.6 % · 0.1 % | 100.0 % · 0.0 % | 28 / 220 · 0.0 % | 141 / 224 | 0 | 0.1 % |
| 40 ms | bursts (GE) | datagrams (today, RU9) | 84.4 % · 15.8 % · 367 ms | 44.9 % · 55.2 % | 78.9 % · 21.9 % | 79.9 % · 20.0 % | 135 / 571 · 20.3 % | 268 / 453 | 201 | 0.1 % |
| 40 ms | bursts (GE) | carrier + keyframe gate | 83.0 % · 17.6 % · 350 ms | 56.8 % · 46.1 % | 83.6 % · 16.8 % | 83.1 % · 17.6 % | 126 / 378 · 14.9 % | 197 / 352 | 370 | 0.1 % |
| 40 ms | bursts (GE) | carrier, keyframe inline | 95.7 % · 4.7 % · 367 ms | 63.4 % · 39.2 % | 95.3 % · 4.9 % | 95.6 % · 5.4 % | 93 / 483 · 5.9 % | 215 / 400 | 334 | 0.1 % |
| 40 ms | radio away 80 ms | datagrams (today, RU9) | 75.0 % · 26.4 % · 283 ms | 48.3 % · 53.4 % | 71.0 % · 30.2 % | 73.5 % · 28.7 % (one phase 66.2) | 28 / 284 · 27.5 % | 179 / 318 | 0 | 0.1 % |
| 40 ms | radio away 80 ms | carrier + keyframe gate | 94.8 % · 5.4 % · 300 ms | 61.7 % · 41.8 % | 95.1 % · 4.9 % | 96.1 % · 4.2 % (one phase 89.2) | 77 / 327 · 3.2 % | 155 / 301 | 58 | 0.1 % |
| 40 ms | radio away 80 ms | carrier, keyframe inline | 97.6 % · 2.9 % · 250 ms | 66.5 % · 35.7 % | 97.9 % · 2.2 % | 99.2 % · 0.8 % | 46 / 294 · 0.5 % | 154 / 308 | 26 | 0.1 % |
| 100 ms | 0.1 % loss | datagrams (today, RU9) | 62.2 % · 37.7 % · 450 ms | 47.0 % · 43.6 % | 71.2 % · 30.0 % (one phase 65.3) | 75.8 % · 24.5 % | 397 / 1093 · 27.0 % | 416 / 1067 | 1521 | 0.1 % |
| 100 ms | 0.1 % loss | carrier + keyframe gate | 60.9 % · 43.1 % · 400 ms | 39.0 % · 64.1 % | 61.9 % · 41.5 % | 63.3 % · 41.6 % | 241 / 494 · 44.7 % | 290 / 483 | 1535 | 0.2 % |
| 100 ms | 0.1 % loss | carrier, keyframe inline | 77.1 % · 23.1 % · 400 ms | 57.4 % · 37.3 % | 80.6 % · 21.3 % | 81.1 % · 19.3 % | 257 / 662 · 21.6 % | 358 / 588 | 1233 | 0.5 % |
| 10 ms | 1440p 0.1 % loss | carrier + keyframe gate | 98.5 % · 1.4 % (one phase 86.3) · 134 ms | 81.5 % · 16.3 % | 99.0 % · 0.7 % | 99.3 % · 0.7 % | 20 / 140 · 0.8 % | 83 / 145 | 26 | 0.7 % |
| 10 ms | 1440p 0.1 % loss | carrier, keyframe inline | 99.7 % · 0.2 % · 150 ms | 81.5 % · 16.6 % | 99.7 % · 0.1 % | 99.9 % · 0.1 % | 21 / 171 · 1.3 % | 93 / 153 | 76 | 3.2 % |
| 10 ms | 1440p bursts | carrier + keyframe gate | 89.9 % · 11.9 % · 234 ms | 74.1 % · 21.8 % | 89.7 % · 12.2 % | 89.9 % · 12.4 % (one phase 67.2) | 79 / 250 · 13.9 % | 113 / 209 | 551 | 1.0 % |
| 10 ms | 1440p bursts | carrier, keyframe inline | 90.7 % · 9.8 % (one phase 85.7) · 217 ms | 73.8 % · 20.4 % | 90.8 % · 9.8 % | 90.8 % · 9.9 % | 76 / 253 · 7.7 % | 122 / 209 | 438 | 1.1 % |
| 10 ms | 1440p 80 Mbps | carrier + keyframe gate | 99.2 % · 0.6 % · 83 ms | 89.6 % · 10.3 % | 99.7 % · 0.0 % | 100.0 % · 0.0 % | 14 / 82 · 0.0 % | 55 / 92 | 0 | 0.2 % |
| 10 ms | 1440p 80 Mbps | carrier, keyframe inline | 98.4 % · 1.3 % · 83 ms | 89.3 % · 11.3 % | 99.7 % · 0.0 % | 100.0 % · 0.0 % (one phase 94.2) | 14 / 81 · 0.0 % | 58 / 103 | 0 | 0.2 % |
| 10 ms | 1440p 80 Mbps + outages | carrier + keyframe gate | 95.5 % · 5.4 % · 200 ms | 76.0 % · 25.7 % | 95.3 % · 5.1 % (one phase 86.1) | 95.8 % · 4.6 % | 17 / 197 · 4.6 % | 72 / 206 | 150 | 1.6 % |
| 10 ms | 1440p 80 Mbps + outages | carrier, keyframe inline | 97.4 % · 3.1 % · 200 ms | 76.5 % · 25.6 % | 97.0 % · 3.4 % (one phase 82.8) | 97.7 % · 2.5 % | 17 / 194 · 2.3 % | 76 / 211 | 134 | 1.8 % |
| 40 ms | 0.5 % loss | datagrams (today, RU9) | 46.7 % · 54.3 % · 433 ms | 34.9 % · 61.0 % | 57.2 % · 44.4 % | 64.6 % · 36.5 % | 360 / 1057 · 33.7 % | 410 / 1072 | 1903 | 1.8 % |
| 40 ms | 0.5 % loss | carrier + keyframe gate | 53.9 % · 49.3 % · 367 ms | 39.1 % · 63.9 % | 55.5 % · 47.5 % | 55.1 % · 47.3 % | 219 / 445 · 43.7 % | 267 / 463 | 1321 | 1.3 % |
| 40 ms | 0.5 % loss | carrier, keyframe inline | 56.8 % · 42.6 % · 384 ms | 47.5 % · 48.9 % | 61.8 % · 39.7 % | 60.6 % · 40.0 % | 290 / 671 · 32.4 % | 368 / 631 | 1883 | 3.1 % |
| 40 ms | 0.5 % loss | carrier, keyframe inline (D3) | 63.5 % · 38.2 % · 367 ms | 43.9 % · 57.8 % | 64.5 % · 37.2 % | 64.6 % · 35.4 % | 178 / 437 · 32.1 % | 279 / 459 | 875 | 0.8 % |
| 100 ms | 1 % loss | datagrams (today, RU9) | 3.7 % · 99.2 % · 917 ms | 3.6 % · 99.4 % | 5.3 % · 97.2 % | 5.7 % · 97.0 % | 916 / 2457 · 94.7 % | 1370 / 2257 | 5338 | 1.7 % |
| 100 ms | 1 % loss | carrier + keyframe gate | 0.0 % · 100.0 % · 33000 ms | 0.0 % · 100.0 % | 0.0 % · 100.0 % | 0.0 % · 99.9 % | 475 / 59347 · 96.5 % | 22286 / 57163 | 5276 | 2.8 % |
| 100 ms | 1 % loss | carrier, keyframe inline | 4.8 % · 99.0 % · 1167 ms | 4.8 % · 99.2 % | 5.7 % · 96.7 % | 5.7 % · 96.7 % | 1071 / 2376 · 94.6 % | 1422 / 2230 | 5325 | 0.1 % |
| 100 ms | 1 % loss | carrier, keyframe inline (D3) | 2.7 % · 98.3 % · 583 ms | 2.4 % · 98.9 % | 2.8 % · 98.1 % | 2.9 % · 98.1 % | 342 / 533 · 96.9 % | 392 / 533 | 4890 | 0.1 % |

#### Table S3 — what the gate costs on a clean path, and what inline costs

Phase 0. *Latency*: the Balanced viewer's on-screen latency (encoder output to the screen, leg B
taken as instant), p50 / p95.

| RTT | Sender | Balanced latency p50 / p95 ms | Balanced offset mean ms | Lowest latency: shown · stalled | Decode-ready p50 / p95 / p99 ms |
|---|---|---|---|---|---|
| 10 ms | datagrams (today, RU9) | 50 / 50 | 50 | 96.0 % · 0.8 % (one phase 90.3) | 8 / 15 / 26 |
| 10 ms | carrier + keyframe gate | 50 / 50 | 50 | 96.2 % · 2.4 % (one phase 88.2) | 6 / 14 / 30 |
| 10 ms | carrier, keyframe inline | 50 / 50 | 50 | 96.7 % · 1.8 % (one phase 89.9) | 6 / 10 / 28 |
| 40 ms | datagrams (today, RU9) | 67 / 67 | 50 | 94.8 % · 0.8 % (one phase 86.7) | 23 / 30 / 38 |
| 40 ms | carrier + keyframe gate | 83 / 84 | 59 | 92.1 % · 5.8 % | 21 / 45 / 64 |
| 40 ms | carrier, keyframe inline | 67 / 67 | 50 | 97.1 % · 0.9 % | 21 / 24 / 39 |
| 100 ms | datagrams (today, RU9) | 100 / 100 | 50 | 94.0 % · 0.4 % (one phase 88.5) | 54 / 62 / 71 |
| 100 ms | carrier + keyframe gate | 167 / 167 | 123 | 79.0 % · 22.6 % (one phase 71.3) | 52 / 138 / 158 |
| 100 ms | carrier, keyframe inline | 100 / 100 | 50 | 95.7 % · 3.3 % (one phase 86.6) | 51 / 57 / 79 |

#### Table S4 — Chrome for Testing 158 beside 151

`matrix.py r84-c158` beside the same cells' 151 runs where this pass has them. *Controller*: the
net-log's `QUIC_CONGESTION_CONTROL_CONFIGURED`. Packet gap: within a burst (under 5 ms), p50.

| Path | Chrome | Sender | Controller | Datagram frames sent / offered | Mbps on the wire | Packet gap p50 µs | CPU: network · renderer % | Balanced shown · stalled | Decode-ready p50 / p99 ms · undecodable |
|---|---|---|---|---|---|---|---|---|---|
| 10 ms clean | 151 | datagrams (today, RU9) | CUBIC_BYTES | 94 170 / 94 170 | 10.7 | 199 | 25.6 · 15.1 | 100.0 % · 0.0 % | 8 / 26 · 0.0 % |
| 10 ms clean | 158 | datagrams (today, RU9) | CUBIC_BYTES | 83 090 / 83 090 | 10.9 | 244 | 26.8 · 17.7 | 100.0 % · 0.0 % | 9 / 26 · 0.0 % |
| 40 ms clean | 151 | carrier, keyframe inline | CUBIC_BYTES | 5 079 / 5 079 | 10.8 | 59 | 18.4 · 5.9 | 100.0 % · 0.0 % | 21 / 39 · 0.0 % |
| 40 ms clean | 158 | carrier, keyframe inline | CUBIC_BYTES | 5 079 / 5 079 | 11.0 | 62 | 18.4 · 7.1 | 99.9 % · 0.0 % | 21 / 41 · 0.0 % |
| 10 ms 1440p clean | 158 | datagrams (today, RU9) | CUBIC_BYTES | 307 607 / 356 198 | 36.7 | 232 | 69.5 · 44.7 | 75.7 % · 24.9 % | 38 / 518 · 8.5 % |
| 10 ms 1440p clean | 158 | carrier, keyframe inline | CUBIC_BYTES | 5 079 / 5 079 | 41.7 | 50 | 48.1 · 11.7 | 99.4 % · 0.4 % | 9 / 48 · 0.0 % |

#### Table S5 — BR4: the shipping code, Lowest latency against Reliable

`matrix.py r84-br4`, one 100 s run a cell, then `phase.py`. *Lowest latency* is RU9's datagram path
as it ships; *Reliable* the shipping `KeyframeSender` GOP streams and `CarrierSender` (BR2) on the
lab page. Viewer columns: shown · stalled, the median over four presentation phases (F1); the
Balanced viewer's on-screen latency p50 at phase 0. *Keyframes*: acknowledged of produced, and
how many the 2 s rule aborted.

| RTT | Path | Chrome | Sender | Balanced | Lowest latency | Smoother | Most stable | Decode-ready p50 / p99 ms · undecodable | Keyframes acked / produced · aborted | Deltas shed | Steal |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 10 ms | clean | 151 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 50 ms | 95.9 % · 0.8 % (one phase 89.4) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 27 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | clean | 151 | Reliable (shipping) | 99.9 % · 0.0 % · 50 ms | 96.6 % · 1.6 % (one phase 86.8) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 6 / 25 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | clean | 157 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 50 ms | 96.2 % · 0.9 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 24 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | clean | 157 | Reliable (shipping) | 99.9 % · 0.0 % · 50 ms | 96.8 % · 1.5 % (one phase 89.5) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 6 / 26 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | clean | 158 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 50 ms | 96.0 % · 1.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 24 · 0.0 % | 200 / 200 · 0 | 0 | 0.0 % |
| 10 ms | clean | 158 | Reliable (shipping) | 100.0 % · 0.0 % · 50 ms | 96.8 % · 1.5 % (one phase 89.3) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 6 / 25 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 151 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 67 ms | 94.9 % · 0.8 % (one phase 87.6) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 23 / 39 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 151 | Reliable (shipping) | 100.0 % · 0.0 % · 67 ms | 96.4 % · 2.5 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 21 / 47 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 157 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 67 ms | 95.6 % · 0.7 % (one phase 87.2) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 23 / 37 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 157 | Reliable (shipping) | 99.9 % · 0.0 % · 67 ms | 97.1 % · 1.6 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 21 / 39 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 158 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % · 67 ms | 95.5 % · 0.4 % (one phase 88.8) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 23 / 37 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | clean | 158 | Reliable (shipping) | 99.9 % · 0.0 % · 67 ms | 96.2 % · 3.3 % (one phase 91.1) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 21 / 45 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 100 ms | clean | 151 | datagrams (Lowest latency, RU9) | 100.0 % · 0.0 % (one phase 86.1) · 100 ms | 93.2 % · 0.9 % | 99.9 % · 0.0 % (one phase 86.0) | 100.0 % · 0.0 % (one phase 85.2) | 54 / 73 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 100 ms | clean | 151 | Reliable (shipping) | 99.9 % · 0.0 % · 100 ms | 96.1 % · 2.6 % (one phase 85.8) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 52 / 73 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 100 ms | clean | 157 | datagrams (Lowest latency, RU9) | 99.9 % · 0.0 % (one phase 91.2) · 100 ms | 93.5 % · 1.1 % | 99.9 % · 0.0 % (one phase 91.1) | 100.0 % · 0.0 % (one phase 90.6) | 54 / 71 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 100 ms | clean | 157 | Reliable (shipping) | 99.7 % · 0.0 % · 100 ms | 95.8 % · 2.9 % (one phase 88.8) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 51 / 75 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 100 ms | clean | 158 | datagrams (Lowest latency, RU9) | 99.9 % · 0.0 % (one phase 86.1) · 100 ms | 93.2 % · 0.6 % | 99.9 % · 0.0 % (one phase 86.0) | 100.0 % · 0.0 % (one phase 85.2) | 54 / 71 · 0.0 % | 200 / 200 · 0 | 0 | 0.7 % |
| 100 ms | clean | 158 | Reliable (shipping) | 99.9 % · 0.0 % · 100 ms | 96.3 % · 2.4 % (one phase 87.1) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 51 / 73 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | 151 | datagrams (Lowest latency, RU9) | 95.6 % · 3.8 % · 50 ms | 87.3 % · 13.9 % (one phase 80.1) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 9 / 73 · 0.0 % | 200 / 200 · 0 | 0 | 0.6 % |
| 10 ms | 0.1 % loss | 151 | Reliable (shipping) | 99.5 % · 0.1 % · 67 ms | 88.0 % · 10.3 % (one phase 82.2) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 7 / 64 · 0.0 % | 199 / 200 · 0 | 0 | 0.4 % |
| 10 ms | 0.1 % loss | 157 | datagrams (Lowest latency, RU9) | 97.3 % · 1.9 % · 50 ms | 89.7 % · 11.8 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 71 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | 157 | Reliable (shipping) | 99.5 % · 0.3 % · 67 ms | 87.3 % · 10.8 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 7 / 69 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.1 % loss | 158 | datagrams (Lowest latency, RU9) | 97.3 % · 2.6 % · 67 ms | 87.5 % · 12.8 % | 99.4 % · 0.4 % | 100.0 % · 0.0 % | 9 / 105 · 0.0 % | 200 / 200 · 0 | 0 | 1.6 % |
| 10 ms | 0.1 % loss | 158 | Reliable (shipping) | 99.3 % · 0.6 % · 83 ms | 86.5 % · 11.7 % (one phase 80.4) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 7 / 79 · 0.0 % | 199 / 200 · 0 | 0 | 2.2 % |
| 10 ms | 0.5 % loss | 151 | datagrams (Lowest latency, RU9) | 74.2 % · 27.9 % · 100 ms | 63.1 % · 38.8 % | 98.8 % · 1.1 % | 99.4 % · 0.6 % | 9 / 151 · 0.3 % | 200 / 200 · 0 | 0 | 0.2 % |
| 10 ms | 0.5 % loss | 151 | Reliable (shipping) | 99.2 % · 0.7 % · 133 ms | 77.4 % · 19.1 % | 99.8 % · 0.1 % | 100.0 % · 0.0 % | 11 / 135 · 0.0 % | 199 / 200 · 0 | 0 | 2.1 % |
| 10 ms | 0.5 % loss | 157 | datagrams (Lowest latency, RU9) | 76.7 % · 25.5 % · 100 ms | 64.5 % · 37.3 % | 99.5 % · 0.4 % | 100.0 % · 0.0 % | 9 / 152 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.5 % loss | 157 | Reliable (shipping) | 99.2 % · 0.6 % · 133 ms | 78.0 % · 18.7 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 11 / 129 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.5 % loss | 158 | datagrams (Lowest latency, RU9) | 75.0 % · 27.0 % · 84 ms | 63.5 % · 38.1 % | 98.5 % · 1.2 % | 99.0 % · 0.9 % | 9 / 154 · 0.7 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 0.5 % loss | 158 | Reliable (shipping) | 99.3 % · 0.6 % · 133 ms | 78.0 % · 18.3 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 11 / 125 · 0.0 % | 199 / 200 · 0 | 0 | 0.2 % |
| 10 ms | bursts (GE) | 151 | datagrams (Lowest latency, RU9) | 82.5 % · 20.7 % · 67 ms | 70.2 % · 33.6 % | 80.4 % · 20.7 % | 80.7 % · 21.1 % | 9 / 120 · 20.9 % | 200 / 200 · 0 | 0 | 0.2 % |
| 10 ms | bursts (GE) | 151 | Reliable (shipping) | 99.4 % · 0.3 % · 100 ms | 82.7 % · 15.4 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 9 / 101 · 0.0 % | 199 / 200 · 0 | 0 | 1.8 % |
| 10 ms | bursts (GE) | 157 | datagrams (Lowest latency, RU9) | 87.3 % · 15.5 % · 83 ms | 72.7 % · 30.1 % | 78.6 % · 22.1 % | 78.2 % · 21.5 % | 8 / 118 · 21.7 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | bursts (GE) | 157 | Reliable (shipping) | 99.1 % · 0.8 % (one phase 73.7) · 100 ms | 82.0 % · 15.2 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 8 / 98 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | bursts (GE) | 158 | datagrams (Lowest latency, RU9) | 88.6 % · 12.7 % · 100 ms | 70.0 % · 31.2 % (one phase 64.5) | 80.7 % · 21.0 % | 80.6 % · 19.7 % | 9 / 122 · 19.8 % | 200 / 200 · 0 | 0 | 1.1 % |
| 10 ms | bursts (GE) | 158 | Reliable (shipping) | 99.2 % · 0.8 % · 100 ms | 82.7 % · 15.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 9 / 97 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | radio away 80 ms | 151 | datagrams (Lowest latency, RU9) | 76.4 % · 25.1 % · 134 ms | 66.0 % · 35.9 % | 71.6 % · 29.6 % | 71.7 % · 30.9 % | 9 / 92 · 28.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 10 ms | radio away 80 ms | 151 | Reliable (shipping) | 98.3 % · 2.0 % · 133 ms | 80.5 % · 21.4 % (one phase 75.3) | 99.1 % · 0.6 % | 99.9 % · 0.1 % | 7 / 149 · 0.1 % | 199 / 200 · 0 | 4 | 0.1 % |
| 10 ms | radio away 80 ms | 157 | datagrams (Lowest latency, RU9) | 76.4 % · 25.2 % · 134 ms | 65.7 % · 35.8 % | 71.5 % · 29.7 % | 71.6 % · 30.0 % | 8 / 107 · 27.6 % | 200 / 200 · 0 | 0 | 0.0 % |
| 10 ms | radio away 80 ms | 157 | Reliable (shipping) | 97.6 % · 2.9 % · 133 ms | 80.4 % · 22.2 % (one phase 73.6) | 99.0 % · 0.8 % | 99.9 % · 0.1 % | 7 / 156 · 0.1 % | 199 / 200 · 0 | 4 | 0.1 % |
| 10 ms | radio away 80 ms | 158 | datagrams (Lowest latency, RU9) | 75.2 % · 26.1 % · 217 ms | 64.0 % · 37.5 % | 71.3 % · 30.1 % | 73.9 % · 27.9 % | 8 / 438 · 26.5 % | 200 / 200 · 0 | 38 | 0.1 % |
| 10 ms | radio away 80 ms | 158 | Reliable (shipping) | 98.0 % · 2.4 % · 133 ms | 78.7 % · 22.2 % | 99.1 % · 0.8 % | 99.9 % · 0.1 % | 7 / 154 · 0.1 % | 199 / 200 · 0 | 6 | 1.8 % |
| 40 ms | 0.1 % loss | 151 | datagrams (Lowest latency, RU9) | 92.5 % · 6.8 % (one phase 85.1) · 167 ms | 64.9 % · 34.7 % | 93.6 % · 7.5 % (one phase 80.7) | 100.0 % · 0.0 % (one phase 86.2) | 27 / 269 · 0.0 % | 200 / 200 · 0 | 0 | 0.2 % |
| 40 ms | 0.1 % loss | 151 | Reliable (shipping) | 99.2 % · 0.4 % · 200 ms | 75.7 % · 26.5 % | 99.1 % · 0.5 % | 99.7 % · 0.4 % | 28 / 207 · 0.2 % | 199 / 200 · 0 | 9 | 0.3 % |
| 40 ms | 0.1 % loss | 157 | datagrams (Lowest latency, RU9) | 97.1 % · 3.3 % · 234 ms | 69.8 % · 32.0 % | 95.0 % · 5.7 % | 100.0 % · 0.0 % (one phase 94.9) | 26 / 273 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | 0.1 % loss | 157 | Reliable (shipping) | 99.7 % · 0.2 % (one phase 90.3) · 217 ms | 75.9 % · 27.0 % | 99.5 % · 0.3 % | 99.8 % · 0.2 % | 28 / 211 · 0.1 % | 199 / 200 · 0 | 5 | 0.1 % |
| 40 ms | 0.1 % loss | 158 | datagrams (Lowest latency, RU9) | 97.2 % · 3.5 % · 234 ms | 70.3 % · 32.2 % (one phase 64.5) | 95.2 % · 5.9 % | 100.0 % · 0.0 % (one phase 85.3) | 25 / 275 · 0.0 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | 0.1 % loss | 158 | Reliable (shipping) | 99.8 % · 0.2 % · 217 ms | 75.5 % · 26.6 % | 99.5 % · 0.3 % | 99.9 % · 0.1 % (one phase 85.2) | 28 / 207 · 0.0 % | 199 / 200 · 0 | 2 | 1.4 % |
| 40 ms | bursts (GE) | 151 | datagrams (Lowest latency, RU9) | 77.6 % · 21.8 % · 367 ms | 41.6 % · 60.2 % | 74.4 % · 26.0 % | 75.9 % · 23.7 % | 139 / 556 · 22.5 % | 200 / 200 · 0 | 244 | 0.1 % |
| 40 ms | bursts (GE) | 151 | Reliable (shipping) | 95.3 % · 5.2 % · 367 ms | 64.2 % · 37.9 % | 95.0 % · 5.3 % | 95.5 % · 5.2 % | 94 / 448 · 6.1 % | 199 / 200 · 0 | 345 | 1.4 % |
| 40 ms | bursts (GE) | 157 | datagrams (Lowest latency, RU9) | 81.9 % · 17.2 % · 367 ms | 43.6 % · 57.4 % | 79.2 % · 22.2 % | 80.6 % · 20.8 % | 126 / 739 · 20.8 % | 200 / 200 · 0 | 344 | 0.1 % |
| 40 ms | bursts (GE) | 157 | Reliable (shipping) | 95.8 % · 4.9 % · 367 ms | 63.6 % · 39.3 % | 95.3 % · 5.2 % | 95.6 % · 5.6 % | 90 / 463 · 6.0 % | 199 / 200 · 0 | 339 | 0.1 % |
| 40 ms | bursts (GE) | 158 | datagrams (Lowest latency, RU9) | 82.6 % · 16.9 % · 367 ms | 42.8 % · 58.1 % | 80.7 % · 19.2 % | 82.0 % · 18.4 % | 128 / 811 · 19.0 % | 200 / 200 · 0 | 307 | 0.1 % |
| 40 ms | bursts (GE) | 158 | Reliable (shipping) | 95.8 % · 4.7 % · 367 ms | 63.5 % · 39.4 % | 95.3 % · 4.8 % | 95.7 % · 5.5 % | 91 / 436 · 5.9 % | 199 / 200 · 0 | 333 | 0.1 % |
| 40 ms | radio away 80 ms | 151 | datagrams (Lowest latency, RU9) | 75.3 % · 26.1 % · 317 ms | 48.2 % · 53.9 % | 72.2 % · 29.4 % | 73.9 % · 28.3 % (one phase 64.4) | 29 / 300 · 27.1 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | radio away 80 ms | 151 | Reliable (shipping) | 97.7 % · 2.5 % · 250 ms | 67.0 % · 35.4 % | 98.0 % · 1.7 % | 99.5 % · 0.5 % | 45 / 302 · 0.3 % | 199 / 200 · 0 | 19 | 0.1 % |
| 40 ms | radio away 80 ms | 157 | datagrams (Lowest latency, RU9) | 76.8 % · 24.1 % · 333 ms | 47.9 % · 54.1 % | 70.9 % · 30.6 % | 73.1 % · 29.0 % | 27 / 301 · 27.9 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | radio away 80 ms | 157 | Reliable (shipping) | 98.1 % · 2.2 % · 250 ms | 67.8 % · 34.6 % | 98.2 % · 1.8 % | 99.6 % · 0.3 % (one phase 84.9) | 46 / 278 · 0.2 % | 199 / 200 · 0 | 14 | 0.1 % |
| 40 ms | radio away 80 ms | 158 | datagrams (Lowest latency, RU9) | 75.9 % · 25.7 % · 250 ms | 48.3 % · 54.6 % | 69.8 % · 31.2 % | 73.2 % · 28.9 % (one phase 62.8) | 27 / 290 · 27.8 % | 200 / 200 · 0 | 0 | 0.1 % |
| 40 ms | radio away 80 ms | 158 | Reliable (shipping) | 97.8 % · 2.4 % · 250 ms | 67.5 % · 34.7 % | 97.9 % · 2.1 % | 99.4 % · 0.5 % | 44 / 284 · 0.4 % | 199 / 200 · 0 | 23 | 0.1 % |
| 100 ms | 0.1 % loss | 151 | datagrams (Lowest latency, RU9) | 58.9 % · 36.7 % · 400 ms | 47.4 % · 43.6 % | 73.2 % · 27.6 % | 77.3 % · 22.4 % | 389 / 1105 · 26.9 % | 200 / 200 · 0 | 1532 | 0.1 % |
| 100 ms | 0.1 % loss | 151 | Reliable (shipping) | 80.5 % · 19.2 % · 400 ms | 60.6 % · 34.0 % | 82.9 % · 18.3 % | 82.9 % · 18.4 % | 267 / 677 · 23.2 % | 199 / 200 · 0 | 1315 | 0.1 % |
| 100 ms | 0.1 % loss | 157 | datagrams (Lowest latency, RU9) | 63.6 % · 35.8 % · 484 ms | 46.9 % · 44.3 % | 73.9 % · 28.4 % | 80.7 % · 20.7 % | 374 / 1152 · 24.8 % | 200 / 200 · 0 | 1412 | 0.2 % |
| 100 ms | 0.1 % loss | 157 | Reliable (shipping) | 78.6 % · 21.7 % · 400 ms | 59.7 % · 35.2 % | 81.0 % · 21.0 % | 80.9 % · 20.2 % | 269 / 650 · 22.9 % | 199 / 200 · 0 | 1310 | 0.2 % |
| 100 ms | 0.1 % loss | 158 | datagrams (Lowest latency, RU9) | 65.5 % · 32.7 % · 400 ms | 49.0 % · 42.0 % | 76.5 % · 23.9 % | 81.1 % · 19.8 % | 355 / 1142 · 25.5 % | 200 / 200 · 0 | 1443 | 0.1 % |
| 100 ms | 0.1 % loss | 158 | Reliable (shipping) | 77.6 % · 22.1 % · 400 ms | 60.4 % · 33.5 % | 80.9 % · 20.4 % | 80.7 % · 20.6 % | 277 / 692 · 24.9 % | 199 / 200 · 0 | 1420 | 0.1 % |
| 10 ms | 1440p clean | 151 | datagrams (Lowest latency, RU9) | 75.4 % · 24.8 % · 367 ms | 59.5 % · 36.4 % | 85.6 % · 14.8 % | 89.6 % · 10.0 % | 92 / 519 · 9.5 % | 200 / 200 · 0 | 600 | 0.1 % |
| 10 ms | 1440p clean | 151 | Reliable (shipping) | 99.3 % · 0.4 % · 50 ms | 93.7 % · 6.6 % (one phase 85.4) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 10 / 48 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p clean | 157 | datagrams (Lowest latency, RU9) | 77.3 % · 23.4 % · 367 ms | 67.2 % · 29.4 % | 87.4 % · 13.1 % | 95.0 % · 5.0 % | 20 / 514 · 4.7 % | 200 / 200 · 0 | 291 | 0.1 % |
| 10 ms | 1440p clean | 157 | Reliable (shipping) | 99.6 % · 0.1 % · 50 ms | 94.5 % · 5.6 % (one phase 86.7) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 9 / 44 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p clean | 158 | datagrams (Lowest latency, RU9) | 75.9 % · 24.2 % · 350 ms | 64.9 % · 31.3 % | 86.3 % · 14.0 % | 92.4 % · 7.6 % | 28 / 518 · 6.7 % | 200 / 200 · 0 | 423 | 0.1 % |
| 10 ms | 1440p clean | 158 | Reliable (shipping) | 99.5 % · 0.3 % · 50 ms | 93.7 % · 5.9 % (one phase 84.8) | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 10 / 45 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p 0.1 % loss | 151 | datagrams (Lowest latency, RU9) | 58.9 % · 44.1 % · 367 ms | 47.7 % · 53.9 % | 76.8 % · 23.5 % (one phase 69.1) | 81.3 % · 19.3 % | 240 / 522 · 15.3 % | 200 / 200 · 0 | 921 | 0.0 % |
| 10 ms | 1440p 0.1 % loss | 151 | Reliable (shipping) | 98.9 % · 0.9 % · 133 ms | 82.7 % · 15.5 % | 99.6 % · 0.3 % | 99.7 % · 0.3 % | 18 / 132 · 0.5 % | 199 / 200 · 0 | 26 | 0.1 % |
| 10 ms | 1440p 0.1 % loss | 157 | datagrams (Lowest latency, RU9) | 73.8 % · 27.1 % · 367 ms | 56.7 % · 44.0 % | 85.7 % · 14.7 % | 90.1 % · 9.5 % | 112 / 518 · 8.6 % | 200 / 200 · 0 | 510 | 0.1 % |
| 10 ms | 1440p 0.1 % loss | 157 | Reliable (shipping) | 98.9 % · 0.9 % · 133 ms | 82.4 % · 15.5 % | 99.4 % · 0.3 % | 99.7 % · 0.3 % (one phase 94.5) | 18 / 130 · 0.5 % | 199 / 200 · 0 | 28 | 0.0 % |
| 10 ms | 1440p 0.1 % loss | 158 | datagrams (Lowest latency, RU9) | 73.6 % · 27.2 % · 367 ms | 57.4 % · 43.2 % | 84.5 % · 16.1 % | 88.7 % · 11.7 % | 123 / 517 · 10.4 % | 200 / 200 · 0 | 599 | 0.0 % |
| 10 ms | 1440p 0.1 % loss | 158 | Reliable (shipping) | 99.0 % · 0.8 % · 133 ms | 82.4 % · 15.6 % | 99.5 % · 0.2 % | 99.7 % · 0.2 % (one phase 94.6) | 19 / 128 · 0.4 % | 199 / 200 · 0 | 23 | 0.1 % |
| 10 ms | 1440p bursts | 151 | datagrams (Lowest latency, RU9) | 29.5 % · 75.8 % · 367 ms | 22.9 % · 81.7 % | 27.6 % · 75.8 % | 29.1 % · 72.9 % | 257 / 536 · 68.6 % | 200 / 200 · 0 | 1436 | 0.0 % |
| 10 ms | 1440p bursts | 151 | Reliable (shipping) | 93.1 % · 7.1 % · 184 ms | 75.9 % · 18.3 % | 93.5 % · 7.0 % | 93.7 % · 7.2 % | 66 / 219 · 5.9 % | 199 / 200 · 0 | 338 | 0.1 % |
| 10 ms | 1440p bursts | 157 | datagrams (Lowest latency, RU9) | 38.8 % · 65.3 % · 367 ms | 28.2 % · 76.3 % | 39.4 % · 63.5 % | 39.9 % · 63.6 % | 215 / 521 · 62.0 % | 200 / 200 · 0 | 929 | 0.1 % |
| 10 ms | 1440p bursts | 157 | Reliable (shipping) | 94.1 % · 6.4 % · 184 ms | 76.7 % · 18.2 % | 94.4 % · 6.1 % | 94.3 % · 6.4 % | 63 / 220 · 5.5 % | 199 / 200 · 0 | 308 | 0.1 % |
| 10 ms | 1440p bursts | 158 | datagrams (Lowest latency, RU9) | 35.0 % · 69.7 % · 367 ms | 27.6 % · 76.2 % | 35.4 % · 67.6 % | 38.0 % · 65.9 % | 217 / 521 · 62.9 % | 200 / 200 · 0 | 1071 | 0.1 % |
| 10 ms | 1440p bursts | 158 | Reliable (shipping) | 93.3 % · 7.2 % · 200 ms | 75.9 % · 18.3 % | 93.3 % · 7.2 % | 93.4 % · 7.5 % | 65 / 217 · 5.9 % | 199 / 200 · 0 | 331 | 0.1 % |
| 10 ms | 1440p 80 Mbps | 151 | datagrams (Lowest latency, RU9) | 69.2 % · 30.9 % · 367 ms | 54.5 % · 43.6 % | 85.2 % · 15.1 % | 90.1 % · 9.8 % | 107 / 518 · 8.2 % | 200 / 200 · 0 | 529 | 0.1 % |
| 10 ms | 1440p 80 Mbps | 151 | Reliable (shipping) | 98.8 % · 1.0 % · 83 ms | 90.3 % · 10.6 % | 99.7 % · 0.0 % | 100.0 % · 0.0 % (one phase 94.2) | 13 / 77 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p 80 Mbps | 157 | datagrams (Lowest latency, RU9) | 71.2 % · 29.5 % · 367 ms | 59.8 % · 39.0 % | 86.4 % · 13.5 % (one phase 72.7) | 94.6 % · 5.3 % | 30 / 518 · 4.8 % | 200 / 200 · 0 | 314 | 0.0 % |
| 10 ms | 1440p 80 Mbps | 157 | Reliable (shipping) | 98.6 % · 1.2 % · 83 ms | 90.2 % · 10.6 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 13 / 76 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p 80 Mbps | 158 | datagrams (Lowest latency, RU9) | 73.7 % · 27.1 % · 367 ms | 61.8 % · 36.7 % | 86.5 % · 14.0 % | 94.7 % · 5.2 % | 25 / 511 · 4.8 % | 200 / 200 · 0 | 312 | 0.0 % |
| 10 ms | 1440p 80 Mbps | 158 | Reliable (shipping) | 98.8 % · 1.1 % · 83 ms | 90.1 % · 10.5 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 13 / 77 · 0.0 % | 199 / 200 · 0 | 0 | 0.1 % |
| 10 ms | 1440p 80 Mbps + outages | 151 | datagrams (Lowest latency, RU9) | 52.2 % · 50.5 % · 367 ms | 39.3 % · 62.3 % | 65.4 % · 35.9 % (one phase 58.6) | 68.4 % · 33.1 % | 259 / 510 · 31.0 % | 200 / 200 · 0 | 1017 | 0.0 % |
| 10 ms | 1440p 80 Mbps + outages | 151 | Reliable (shipping) | 97.2 % · 3.3 % · 183 ms | 77.6 % · 24.7 % | 97.3 % · 3.2 % | 97.9 % · 2.3 % | 16 / 179 · 2.0 % | 199 / 200 · 0 | 115 | 0.1 % |
| 10 ms | 1440p 80 Mbps + outages | 157 | datagrams (Lowest latency, RU9) | 57.6 % · 44.3 % · 367 ms | 42.4 % · 58.3 % | 63.5 % · 38.1 % | 67.9 % · 33.6 % | 153 / 515 · 30.2 % | 200 / 200 · 0 | 577 | 0.0 % |
| 10 ms | 1440p 80 Mbps + outages | 157 | Reliable (shipping) | 97.5 % · 3.1 % · 183 ms | 77.9 % · 24.4 % | 97.5 % · 3.0 % | 98.1 % · 2.1 % | 16 / 175 · 1.8 % | 199 / 200 · 0 | 106 | 0.0 % |
| 10 ms | 1440p 80 Mbps + outages | 158 | datagrams (Lowest latency, RU9) | 52.2 % · 50.3 % · 367 ms | 39.2 % · 62.0 % | 61.1 % · 40.4 % | 64.4 % · 36.3 % | 208 / 516 · 33.8 % | 200 / 200 · 0 | 880 | 0.1 % |
| 10 ms | 1440p 80 Mbps + outages | 158 | Reliable (shipping) | 96.9 % · 3.6 % · 183 ms | 77.3 % · 24.8 % | 97.0 % · 3.3 % | 97.7 % · 2.4 % | 16 / 181 · 2.1 % | 199 / 200 · 0 | 118 | 0.1 % |
| 40 ms | 0.5 % loss | 151 | datagrams (Lowest latency, RU9) | 40.4 % · 60.5 % · 367 ms | 31.9 % · 65.4 % | 55.3 % · 46.2 % | 64.0 % · 38.2 % | 365 / 1079 · 32.5 % | 200 / 200 · 0 | 1841 | 0.1 % |
| 40 ms | 0.5 % loss | 151 | Reliable (shipping) | 64.2 % · 36.3 % · 367 ms | 49.0 % · 49.3 % | 68.2 % · 33.8 % | 68.2 % · 32.7 % | 251 / 625 · 28.3 % | 199 / 200 · 0 | 1615 | 0.1 % |
| 40 ms | 0.5 % loss | 157 | datagrams (Lowest latency, RU9) | 43.4 % · 55.3 % · 417 ms | 36.0 % · 58.6 % | 57.3 % · 45.0 % | 64.7 % · 35.5 % | 397 / 1194 · 31.5 % | 200 / 200 · 0 | 1776 | 0.1 % |
| 40 ms | 0.5 % loss | 157 | Reliable (shipping) | 63.4 % · 35.3 % · 367 ms | 50.3 % · 45.8 % | 67.7 % · 34.0 % | 67.4 % · 33.4 % | 260 / 665 · 29.4 % | 199 / 200 · 0 | 1672 | 0.1 % |
| 40 ms | 0.5 % loss | 158 | datagrams (Lowest latency, RU9) | 45.4 % · 54.0 % · 383 ms | 36.1 % · 58.2 % | 57.3 % · 44.8 % | 64.9 % · 35.5 % | 347 / 1165 · 31.6 % | 200 / 200 · 0 | 1785 | 0.1 % |
| 40 ms | 0.5 % loss | 158 | Reliable (shipping) | 63.5 % · 36.1 % · 367 ms | 50.0 % · 46.8 % | 66.8 % · 34.8 % | 67.4 % · 33.7 % | 260 / 664 · 29.5 % | 199 / 200 · 0 | 1682 | 0.1 % |
| 100 ms | 1 % loss | 151 | datagrams (Lowest latency, RU9) | 3.9 % · 99.3 % · 717 ms | 3.8 % · 99.5 % | 6.3 % · 96.4 % | 6.4 % · 96.4 % | 1096 / 2395 · 94.0 % | 140 / 200 · 3 | 5299 | 0.1 % |
| 100 ms | 1 % loss | 151 | Reliable (shipping) | 4.2 % · 99.2 % · 1000 ms | 4.0 % · 99.7 % | 4.8 % · 97.7 % | 4.8 % · 97.7 % | 927 / 2420 · 95.1 % | 136 / 200 · 4 | 5358 | 0.1 % |
| 100 ms | 1 % loss | 157 | datagrams (Lowest latency, RU9) | 3.6 % · 99.3 % · 1117 ms | 3.5 % · 99.5 % | 5.5 % · 97.1 % | 5.6 % · 97.1 % | 1079 / 2407 · 94.2 % | 136 / 200 · 7 | 5297 | 0.1 % |
| 100 ms | 1 % loss | 157 | Reliable (shipping) | 4.6 % · 99.1 % · 1084 ms | 4.5 % · 99.7 % | 5.2 % · 97.3 % | 5.2 % · 97.3 % | 888 / 2223 · 94.8 % | 133 / 200 · 7 | 5314 | 0.1 % |
| 100 ms | 1 % loss | 158 | datagrams (Lowest latency, RU9) | 3.9 % · 99.3 % · 934 ms | 3.8 % · 99.2 % | 5.8 % · 96.9 % | 6.1 % · 96.7 % | 922 / 2339 · 94.2 % | 136 / 200 · 6 | 5302 | 0.1 % |
| 100 ms | 1 % loss | 158 | Reliable (shipping) | 4.9 % · 99.2 % · 1017 ms | 4.8 % · 99.5 % | 5.5 % · 97.0 % | 5.6 % · 97.0 % | 928 / 2332 · 94.5 % | 133 / 200 · 4 | 5324 | 0.1 % |

#### Table S6 — BR4's goals on the shipping code

G1–G5 (§8) judged on table S5's runs. G1: Reliable against Lowest latency, the Balanced viewer's
on-screen latency p50 (phase 0) within one vsync (17 ms) and shown (four-phase median) within 1
point. G5's notice half is the unit table test (`uplink-monitor.test.ts`).

| Goal | Chrome | Cell | Lowest latency | Reliable | Bound | Met |
|---|---|---|---|---|---|---|
| G1 | 151 | 10 ms clean | 50 ms · 100.0 % | 50 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 151 | 40 ms clean | 67 ms · 100.0 % | 67 ms · 100.0 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 151 | 100 ms clean | 100 ms · 100.0 % | 100 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G2 | 151 | 10 ms 0.1 % loss | 95.6 % | 99.5 % | Balanced ≥ 99 % | yes |
| G2 | 151 | 10 ms 0.5 % loss | 74.2 % | 99.2 % | Balanced ≥ 99 % | yes |
| G3 | 151 | 10 ms bursts (GE) | 80.4 · 80.7 % | 99.90 · 100.00 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 151 | 10 ms radio away 80 ms | 71.6 · 71.7 % | 99.15 · 99.86 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 151 | 40 ms bursts (GE) | 74.4 · 75.9 % | 94.98 · 95.47 % | Smoother · Most stable ≥ 95 % | **no** |
| G3 | 151 | 40 ms radio away 80 ms | 72.2 · 73.9 % | 98.01 · 99.49 % | Smoother · Most stable ≥ 95 % | yes |
| G4 | 151 | 10 ms 1440p clean | 75.4 % | 99.3 % | Balanced ≥ 97 % | yes |
| G4 | 151 | 10 ms 1440p 80 Mbps | 69.2 % | 98.8 % | Balanced ≥ 97 % | yes |
| G5 | 151 | 40 ms 0.5 % loss | 365 ms · 200 of 200 keyframes | 251 ms · 199 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |
| G5 | 151 | 100 ms 1 % loss | 1096 ms · 140 of 200 keyframes | 927 ms · 136 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |
| G1 | 157 | 10 ms clean | 50 ms · 100.0 % | 50 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 157 | 40 ms clean | 67 ms · 100.0 % | 67 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 157 | 100 ms clean | 100 ms · 99.9 % | 100 ms · 99.7 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G2 | 157 | 10 ms 0.1 % loss | 97.3 % | 99.5 % | Balanced ≥ 99 % | yes |
| G2 | 157 | 10 ms 0.5 % loss | 76.7 % | 99.2 % | Balanced ≥ 99 % | yes |
| G3 | 157 | 10 ms bursts (GE) | 78.6 · 78.2 % | 99.90 · 100.00 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 157 | 10 ms radio away 80 ms | 71.5 · 71.6 % | 98.98 · 99.89 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 157 | 40 ms bursts (GE) | 79.2 · 80.6 % | 95.32 · 95.58 % | Smoother · Most stable ≥ 95 % | yes |
| G3 | 157 | 40 ms radio away 80 ms | 70.9 · 73.1 % | 98.15 · 99.60 % | Smoother · Most stable ≥ 95 % | yes |
| G4 | 157 | 10 ms 1440p clean | 77.3 % | 99.6 % | Balanced ≥ 97 % | yes |
| G4 | 157 | 10 ms 1440p 80 Mbps | 71.2 % | 98.6 % | Balanced ≥ 97 % | yes |
| G5 | 157 | 40 ms 0.5 % loss | 397 ms · 200 of 200 keyframes | 260 ms · 199 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |
| G5 | 157 | 100 ms 1 % loss | 1079 ms · 136 of 200 keyframes | 888 ms · 133 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |
| G1 | 158 | 10 ms clean | 50 ms · 100.0 % | 50 ms · 100.0 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 158 | 40 ms clean | 67 ms · 100.0 % | 67 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G1 | 158 | 100 ms clean | 100 ms · 99.9 % | 100 ms · 99.9 % | Δ ≤ 17 ms · Δ ≤ 1 pt | yes |
| G2 | 158 | 10 ms 0.1 % loss | 97.3 % | 99.3 % | Balanced ≥ 99 % | yes |
| G2 | 158 | 10 ms 0.5 % loss | 75.0 % | 99.3 % | Balanced ≥ 99 % | yes |
| G3 | 158 | 10 ms bursts (GE) | 80.7 · 80.6 % | 99.90 · 100.00 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 158 | 10 ms radio away 80 ms | 71.3 · 73.9 % | 99.09 · 99.86 % | Smoother · Most stable ≥ 98 % | yes |
| G3 | 158 | 40 ms bursts (GE) | 80.7 · 82.0 % | 95.35 · 95.72 % | Smoother · Most stable ≥ 95 % | yes |
| G3 | 158 | 40 ms radio away 80 ms | 69.8 · 73.2 % | 97.95 · 99.38 % | Smoother · Most stable ≥ 95 % | yes |
| G4 | 158 | 10 ms 1440p clean | 75.9 % | 99.5 % | Balanced ≥ 97 % | yes |
| G4 | 158 | 10 ms 1440p 80 Mbps | 73.7 % | 98.8 % | Balanced ≥ 97 % | yes |
| G5 | 158 | 40 ms 0.5 % loss | 347 ms · 200 of 200 keyframes | 260 ms · 199 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |
| G5 | 158 | 100 ms 1 % loss | 922 ms · 136 of 200 keyframes | 928 ms · 133 of 200 keyframes, 0 ended otherwise | decode-ready p50 < 1.5 s; keyframes ≥ datagrams' − 5 %; only RU9's rules | yes |

## 12. References

- [docs/75](75-reliable-uplink.md) — R73: the carrier (D1–D3), RU1's relay
  ingest, RU3's engine sender, RU9's browser send queue, F14, F18, OD5 and
  tables J, K, P
- [docs/24](24-viewer-network-resilience.md) — R19, the carrier format
- [docs/26](26-relay-dvr-buffer.md) — R21, the DVR ring *Most stable* reads
- [docs/57](57-wifi-uplink.md) — R55, where the uplink problem was found
- [docs/16](16-broadcaster-worker-offload.md) — the broadcaster's worker
  and main-thread pipelines
- [docs/38](38-windows-native-broadcaster.md) — F-12, the keyframe rules
- [docs/85](85-upload-warning-full-uplink.md) — R75, the native upload
  warning's carrier inputs
- `BUGS.md` — "Smoother and Most stable viewers freeze on uplink loss that
  Balanced viewers never see"
