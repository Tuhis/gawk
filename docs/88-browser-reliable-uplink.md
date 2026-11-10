# R84 — Browser Reliable uplink: measured first (docs/88)

**Status**: designed 2026-11-10 from a lab pass on Chrome's own QUIC stack
(§4, §11), building on [docs/75](75-reliable-uplink.md)'s F18. Owner
decisions OD1–OD6 pending (§3). No chunk started; BR0, the gaming-PC
measurement, comes first and gates the rest (§8). `BR` = Browser Reliable.

Touches `gawk-app` (the browser broadcaster's send path, one setting,
Diagnostics, telemetry fields), and — only if OD1 picks the inline keyframe
(K3) — `gawk-server` (one stream type and one capability bit on RU1's
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


## 3. Owner decisions (pending)

None of these is decided. Each row gives the options, what the lab says,
and a recommendation.

| # | Question | Options | What the lab says | Recommendation |
|---|---|---|---|---|
| OD1 | How does a GOP's keyframe get ahead of its deltas (§5.2)? | **K1** the gate on acknowledgement, relay unchanged; **K3** the keyframe inline at the head of the GOP's stream, one stream type and one capability bit on the relay (BR1); **K1 now, K3 later** | K1 costs about a round trip on long clean paths (F2: 83 vs 67 ms at 40 ms, 167 vs 100 ms at 100 ms) and falls below today's datagrams under bursts at 40 ms (F3: 83.0 vs 84.4 %). K3 costs nothing on a clean path and matches or beats K1 on every lossy cell measured (F3, F4) | **K3**. It is the only option that meets OD5's "design away" without a heuristic, and the relay change is small, contained in RU1's own carrier reader. On a relay that advertises `CapUplinkCarriers` but not the new bit, the browser sends datagrams (one carrier path to maintain, not two); fleet relays upgrade with the release. K1 first would ship a mode with a known per-GOP cost and then change its wire form. |
| OD2 | What does a browser broadcaster get by default? | **Reliable** once BR4 and BR5 hold, as the native apps since 2026-11-05 (docs/75 D4); **Lowest latency**, Reliable opt-in; **Reliable from the first release** | With K3, no cost on a clean path at 10–100 ms (F2), better for every preset under light loss, bursts and outages (F3, F4), no worse where Cubic collapses (F5). Unmeasured: the gaming PC and Chrome on Windows (BR0), a real capture and encode beside it | **Reliable, after BR4's lab criteria and BR5's field pass**, shipping opt-in until then — the order docs/75 D4 first set for the native apps. *Lowest latency* stays a real choice (docs/75 F3's live-edge trade). |
| OD3 | Does the browser get a Delivery setting? | **A** the native control and copy under *Stream quality*; **B** none, Reliable whenever the relay can | The modes differ only for a live-edge viewer on a long lossy path (docs/75 F3); that is the case for a choice, and the native apps offer it | **A**: one control in both apps, the same words (§7). |
| OD4 | Which browsers engage it? | **Chromium only**, Firefox and Safari each after its own BR0-B pass; **every browser with WebTransport streams** | Only Chrome measured (§5.8). Firefox's and Safari's `close()`, schedulers and congestion control are unknown here | **Chromium first** (Chrome, Edge, and other Chromium browsers), Firefox and Safari each when its measurement holds; datagrams until then. |
| OD5 | Under K3, how is a retired GOP stream ended (§5.3)? | **RU9's rule**: FIN at rotation, queued records shed, aborted only if unacknowledged 2 s after it opened; **docs/75 D3's**: reset 150 ms after the FIN | Under D3's reset, 136 of 199 and 194 of 200 keyframes were never acknowledged where Cubic collapses (F5) — F-12's livelock. RU9's rule landed as many as today's datagrams, at a bounded latency (384 ms and 1.2 s on screen) | **RU9's rule**. It keeps the browser's one keyframe guarantee, and the stale records it lets drain are bounded by what was written before the rotation. |
| OD6 | Do shed GOPs step the browser's auto-ladder down? | **No**: the notice asks the broadcaster (RU9's choice); **yes**: a shed GOP is a rung-down signal for `FallbackController` | Not measured. RU9 deferred it here ("the browser Reliable milestone revisits it") | **No, not in this milestone.** No gawk sender's ladder reacts to the network (docs/09, docs/38 D11); a shed GOP under a radio outage is not a capacity signal (docs/75's warning lesson, `docs/gotchas.md`). Revisit with BR5's telemetry. |

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
(`0x1F`, the next free on 2026-11-10 — BR1 takes whatever is free when it lands), whose prologue `version ‖ 0x1F` is followed by one
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
    written); it is aborted only by RU9's 2 s rule. The cost is that on a
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
if OD3 takes option A):

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

1. **BR0 first, and alone**: the gaming-PC measurement. Nothing below
   starts until it is recorded, because it can change OD1 (if Chrome on
   Windows schedules streams or datagrams differently) and OD2.
2. **BR1 and BR2 together** (BR1 only if OD1 picks K3), behind the
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
| G5 | **Latency stays bounded, and keyframes land, where nothing can carry the stream**: on the cells where Chrome's Cubic collapses (40 ms 0.5 %, 100 ms 1 %), decode-ready p50 under 1.5 s; keyframes acknowledged no fewer than RU9's datagrams on the same cell, less 5 % of those produced; no keyframe ended by a rule other than RU9's 2 s; the notice raises (its table test, on the lab's counters) | lab + unit |
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

### BR1 — Relay and wire: the keyframe carrier (only if OD1 picks K3)

| Acceptance criterion | Verified by |
|---|---|
| `TypeKeyframeCarrier = 0x1F` and `CapKeyframeCarriers = 1 << 3` allocated in `gawk-server/wire` and mirrored in `wire.ts`/`parity.ts` and `crates/wire`, golden vectors byte-identical in all three (CLAUDE.md) | unit (mirror tests) |
| The relay advertises `CapKeyframeCarriers` only beside `CapUplinkCarriers`; `-uplink-carriers=false` sends neither and rejects a 0x1F stream as bad input, byte-identically to today | unit (diff) |
| A 0x1F stream's StreamFrame reaches `onKeyframe` (cache, priming, R8 fan-out) **when its declared length has been read**, before the stream's FIN; its records after it are ingested as RU1's; red first: a test that holds the FIN back and expects the keyframe in the cache | unit, red first |
| A 0x1F stream counts against `MaxUplinkCarriers` and never takes one of `acceptKeyframeStreams`' four slots; a StreamFrame larger than `MaxKeyframeBytes`, a malformed one, or one with the keyframe flag clear ends the stream (`CancelRead`) with nothing cached | unit |
| A hostile publisher holding a hole inside the keyframe part keeps at most its connection receive window of relay memory (RU1's test, extended) | unit |
| A commit touching `gawk-broadcast-desktop/` in the same PR for the `crates/wire` mirror (CONTRIBUTING) | review |

### BR2 — The browser's carrier lane

A `CarrierSender` beside `DatagramSender` and `KeyframeSender` in
`transport/`, owned by `BroadcastPipeline`, so it runs wherever the
pipeline runs (docs/16).

| Acceptance criterion | Verified by |
|---|---|
| **Engage** (D1, D6): capability (both bits under K3) ∧ Reliable ∧ a browser D6 allows ⇒ the carrier for the next GOP; anything else ⇒ RU9's datagrams, byte-identical to today; a change applies at the next keyframe; the first GOP of a session goes as datagrams if `RelayCapabilities` has not arrived when its keyframe is produced, as natively (docs/75 RU5 as built) | unit, test-first |
| **Records** are byte-identical to the datagrams datagram mode sends for the same frame, parity included, chunked at `maxDatagramSize`; a frame's records go to the stream in one `write()` | unit |
| **Keyframe first** (D2): under K3 a GOP stream's first message is the keyframe's StreamFrame (prologue 0x1F); under K1 a GOP's records wait for its keyframe stream's acknowledgement; red first with a fake transport that delivers stream bytes in write order | unit, red first |
| **Lifecycle** (D3), with a fake transport whose writes block, under fake timers: a GOP stream opens only when `KeyframeSender` would start its keyframe; a keyframe that waits leaves its GOP without a stream and its deltas are dropped; at rotation queued records are shed, the stream FINned, a blocked write left to finish and nothing more of that GOP written; a stream is aborted only by RU9's 2 s rule; at most two GOP streams are in Chrome; the encoder never waits | unit (fake clock) |
| **Resume**: a new session starts with no carrier; nothing queued on the old one is sent on the new; the forced keyframe opens the first GOP stream once the capability is known | unit |
| **Counted** (D7): carriers opened, aborted; records shed; delivery requested and in use — in `BroadcastStats`, Diagnostics and telemetry with docs/75 D8's names (`uplinkMode`, `uplinkModeRequested`, `uplinkCarriersOpened`, `uplinkCarriersExpired` for aborts, `uplinkRecordsShed`) | unit |
| **The notice's inputs** (D5) in Reliable mode: a second is bad on any shed record, on two or more keyframe streams superseded or failed, or on sent fps under 75 % of encode fps; table tests that replay the lab's per-second counters | unit |
| No `postMessage` per frame added on the worker path; the main-thread path's per-frame work is one `write()` | review |

### BR3 — What users see (§7)

| Acceptance criterion | Verified by |
|---|---|
| The Delivery control (if OD3 = A) with §7's exact copy, under *Stream quality*; persists in `broadcastSettingsStore`; a blank value follows the default; applies at the next keyframe without a restart | unit + manual |
| No docs/57 principle-3 term in a main-flow string (the existing string test, extended to the new strings) | unit |
| Diagnostics and *Copy diagnostics* show §7's rows | unit |
| The notice reads as RU9's, reachable by keyboard and screen reader as RU9 requires | unit |

### BR4 — Lab and CI on the shipping code

| Acceptance criterion | Verified by |
|---|---|
| The lab page (`gawk-app/lab/uplink-sender.lab.ts`) gains a Reliable mode that runs the shipping `CarrierSender`, as RU9's datagram mode runs the shipping `DatagramSender` | review |
| `r84-b`'s cells re-run on it (Chrome 151 and the current stable), four-phase medians: G1–G5 judged and recorded in §11 | lab |
| The e2e harness's browser-broadcast mode (`e2e/run.mjs --browser-broadcast`) gains an uplink-loss pass in Reliable mode: the R29 forwarder between the browser broadcaster and the relay drops bursts, and a *Smoother* viewer sees no freeze where the same pass on *Lowest latency* does (flow-shaped, docs/25 Decision 6) | e2e (tier 1) |

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
  for this, and nothing ships before it.
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
