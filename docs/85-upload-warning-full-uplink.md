# R75 — An upload warning that sees a full uplink (docs/85)

**Status**: implemented 2026-11-05, chunks **UW1–UW4** (§6), measured
first in the uplink lab (§2) and accepted there (§7) with one Cubic run
5 s late; UW5, the owner's pass on real hardware, is open.

**Relationship to earlier work**: the native broadcasters' "Your upload
can't keep up" warning is docs/38's `uplink` monitor (D12 amendment,
2026-07-31). R73 made it a capacity signal judged per delivery mode and
took packet loss out of its inputs (docs/75 RU4, revised 2026-10-21), which
fixed radio outages but left it silent where BBR fills a thin uplink:
docs/75 table O3, and the owner moved that half of RU4's criterion here
on 2026-10-21. The warning's copy, its hysteresis (5 bad seconds to raise,
15 clean to clear) and where it shows are unchanged.

---

## 1. Why, and what "done" means

With BBR, the engine's default since 2026-10-21 (docs/75 D5), a thin
uplink shows nothing on the send side. BBR holds the stream's rate and the
network drops what does not fit, so the send buffer does not back up,
keyframes still finish inside their GOP and carriers mostly do not expire.
Every signal RU4 left the monitor reads the send side, so none of them
moves. Table O3: on 15 Mbps beside a household's upload, datagrams on BBR
left up to 98 % of frames undecodable and raised the warning in 1 run of
3; the carrier on BBR never did.

The roadmap's direction was to compare the stream's bitrate with the
connection's own bandwidth estimate (quinn's congestion window over RTT,
or BBR's bottleneck bandwidth). §2 measured that and found that neither
estimate works for a video stream. The signal this milestone ships is
measured by the stream itself: how fast each keyframe drains.

### Milestone acceptance criteria

Refined from ROADMAP.md's R75 entry. *Verified by* names the evidence.

| Goal | Verified by |
|---|---|
| On table O's 15 Mbps cell beside the upload, the warning shows within its 5 s hysteresis of the upload's start, in both delivery modes, with BBR and with Cubic, in every run. *§7: met with BBR in all six runs; with Cubic one run of six was 5 s late* | lab (§7 table U3), table tests |
| It does not show on a 30 Mbps uplink alone (room for the stream), in either mode with either controller | lab (U3) |
| RU4's criterion is kept: it does not show on the lossless cells with either controller, on the lossy ones (random loss, bursts, radio outages) with BBR, or on the 1080p outage cells at 10 ms with Cubic | lab (§7 table U4), table tests |
| On the 15 Mbps cell alone (the trace outgrows the uplink partway, not in every run) and on 30 Mbps beside the upload, what it does is recorded, not gated | lab (U3) |
| The new inputs reach telemetry: `keyframeStreamsSlow` and `uplinkMinRttMs` on the stored row, the iOS sample included | unit (schema, stored shape, iOS) |
| The lab reproduces §2 and §7 from §8's commands | manual, recorded |

---

## 2. What the lab found (2026-11-05)

The engine-backed lab (docs/75 RU6, `bin/sender-engine`) with three
additions: the sender logs quinn's `ControllerMetrics` every 20 ms (`B`
lines: the pacing rate, and the window over smoothed RTT), `udp_tx` bytes
and the minimum RTT on its `T` lines, and the new engine counters on its
`Y` lines. Table O's shared-uplink harness (`fair/run.sh`: a 50 ms
drop-tail queue behind a tbf, a household's bulk TCP upload from 20 s) at
15 and 30 Mbps, and docs/75 table R's matrix (24 cells at 10, 40 and
100 ms, each delivery mode with each controller). The 1080p60 trace:
9.5 Mbps mean, 12 Mbps ceiling, about 11–12 Mbps on the wire with parity
and audio. Host CPU steal stayed under 1 %.

**F1 — quinn's estimates do not measure the uplink for a video stream.**
Table U1 (below), two runs a cell:

- *BBR's bottleneck bandwidth* (quinn exposes it only as the pacing rate,
  `bw × pacing_gain`; the medians of its 20 ms samples average the gain
  out) reads **7.5 Mbps on a clean 30 Mbps uplink** under an 11.4 Mbps
  stream that lost nothing, and **0 for a whole run** on the carrier. quinn's
  max filter takes only samples that were not application-limited
  (`bbr/bw_estimation.rs`), and a video stream is application-limited
  almost all the time, so the estimate comes from the odd keyframe burst,
  or never. On the thin cell beside the upload it reads 7.7–7.8 Mbps,
  which looks right, but the clean 30 Mbps reading is just as low, so it
  cannot tell the two apart.
- *The window over RTT*: Cubic's window grows without bound while the
  stream is application-limited (114–125 Mbps on a 30 Mbps link), and
  BBR's includes its ack-aggregation allowance (up to 38 Gbps). quinn
  paces at the window over RTT, not at BBR's pacing rate, so a window that
  large is also why BBR's datagrams overrun the 15 Mbps queue alone.

**F2 — a keyframe's drain rate separates a full uplink from a lossy
one.** A keyframe is tens of kilobytes on a reliable stream whose `finish`
resolves only when every byte is acknowledged. Its bits over the time to
acknowledgement, less one minimum RTT, is what the path delivered while it
carried the stream. Beside the upload at 15 Mbps it drains at a third to
two thirds of the stream's wire rate (table U1); on 30 Mbps alone at more
than twice it, and almost no keyframe drains slower than the stream. The
matrix's lossy cells stretch a keyframe now and then (a lost packet costs
a round trip or two) but rarely every keyframe in a second.

**F3 — "every keyframe in the second slower than 1.2× the stream, two
round trips allowed, five seconds running" is the rule.** Each threshold
replayed over every run made before the engine had it (`r75.py sweep`):
the 32 runs of the 15 Mbps cell beside the upload and the 63 alone runs
(the exploration's two a cell, then three acceptance passes, the first two
on thresholds this table replaced), and the matrix's 58 runs that RU4 says
must not raise: every BBR run, Cubic's lossless ones and its 10 ms outage
ones. With one minimum RTT taken off a keyframe's time:

| Slow below | 15 Mbps beside the upload: raised within 5 s | 15 Mbps alone, damaged runs: raised | Alone, undamaged (< 1 %; 15, 30, 80 Mbps): raised | Matrix runs that must not raise and do |
|---|---|---|---|---|
| 0.75 × wire rate | 26 of 32 | 3 of 19 | 0 of 44 | 0 of 58 |
| 0.85 × wire rate | 30 of 32 | 7 of 19 | 0 of 44 | 0 of 58 |
| 1.00 × wire rate | 32 of 32 | 15 of 19 | 1 of 44 | 2 of 58 (rtt100-loss0.01 eng-dgram-bbr, rtt100-loss0.02 eng-dgram-bbr) |

With two:

| Slow below | 15 Mbps beside the upload: raised within 5 s | 15 Mbps alone, damaged runs: raised | Alone, undamaged (< 1 %; 15, 30, 80 Mbps): raised | Matrix runs that must not raise and do |
|---|---|---|---|---|
| 0.75 × wire rate | 26 of 32 | 1 of 19 | 0 of 44 | 0 of 58 |
| 0.85 × wire rate | 28 of 32 | 4 of 19 | 0 of 44 | 0 of 58 |
| 1.00 × wire rate | 30 of 32 | 9 of 19 | 1 of 44 | 0 of 58 |
| 1.10 × wire rate | 32 of 32 | 16 of 19 | 2 of 44 | 0 of 58 |
| 1.20 × wire rate | 32 of 32 | 17 of 19 | 4 of 44 | 0 of 58 |
| 1.50 × wire rate | 32 of 32 | 19 of 19 | 13 of 44 | 0 of 58 |
| 2.00 × wire rate | 32 of 32 | 19 of 19 | 18 of 44 | 0 of 58 |
| 2.50 × wire rate | 32 of 32 | 19 of 19 | 28 of 44 | 0 of 58 |

The rule took three passes to find, and each one moved it.

- The exploration alone (two runs a cell) chose *under 0.75 of the wire
  rate, one RTT off*.
- The first acceptance pass then raised 4 and 6 s late in one run of
  three on each BBR mode. In those seconds one of the two keyframes
  drained just over the line, which broke the streak. Moving to 0.85
  fixed those runs.
- The second pass, on 0.85, raised up to 11 s late in two of three
  carrier · BBR runs. Once the link is congested the stream's own wire
  rate falls, and the threshold falls with it.

A link with room drains a burst faster than the stream's average; on
30 Mbps alone keyframes drain at about three times the wire rate (U1). So the
threshold belongs above 1. With one RTT off, though, a factor of 1 already
raises on 100 ms paths with 1–2 % loss: a keyframe that meets a loss
there waits out a recovery of about one more round trip. Taking two RTTs
off removes that at every factor tried. It costs almost nothing at 10 ms,
where 20 ms is small beside a thin link's 80–300 ms drains. That leaves a
plateau, from 1.1 to 2.0, where every gated row passes. Above 1.5 the
alone runs with room start to raise (80 Mbps under a 39.5 Mbps stream at
2.0, 30 Mbps at 2.5). 1.2 sits low on the plateau. The four undamaged
alone runs it raises on are all 15 Mbps carrier · BBR at 0.3–1.0 %
undecodable, a link only 1.27× the stream's wire rate.

Comparing against the encoder's own rate instead of the wire rate (the
frames' sizes, before parity and overhead; `R75_RATE=media`) was worse at
every factor: about 20 % lower, so fewer keyframes read as slow, and only
25 of 32 thin runs raised in time even at 1.0.

Requiring every keyframe in the second, not half of them, is what keeps
lossy paths out. Replayed at 0.75 with one RTT off, "at least half slow"
fixed the late runs but raised on six 100 ms random-loss runs.

**F4 — RU4's carrier rule raises on a long lossy path.** At 100 ms with
2 % random loss the carrier · BBR run raised the warning at 13 s and held
it while viewers lost 1.6 % of frames (docs/75 table R's one false raise
with BBR, reproduced). The rule is "every carrier that retired in the
second expired". A lost packet costs a carrier about three round trips
(the first try, loss detection at 9/8 RTT, the retransmission's
acknowledgement), and at 100 ms that is twice the 150 ms deadline, so
every carrier that meets a loss expires, however much capacity the uplink
has. Reading expiries only while the minimum RTT is at most a third of the
deadline (50 ms) clears that row. Cubic's lossy 100 ms carrier cells, where
Cubic's collapse means the connection really does not carry the stream
(78–99 % undecodable), still raise through the drain rule and shed
records: three of them a second later than before, the 1 % loss cell at
14 s instead of 8 s. On the 15 and
30 Mbps cells the path is 10 ms, so nothing there changes.

**F5 — what the warning still cannot see.** On 15 Mbps alone the stream's
keyframe bursts overrun the 50 ms queue and the network drops deltas
(18–22 % undecodable on datagrams), while keyframes still drain at 12 Mbps.
The uplink carries the stream's average but not its bursts. Nothing on the
send side tells that apart from random loss, which is the loss notice's
case (docs/75 D6). It is recorded, not gated, as table O3 recorded it.

#### U1 — the exploration's runs: what quinn's controller says, and what a keyframe says (medians over 25–80 s; Mbps)

| Uplink | Delivery · controller | Upload | Runs | Undecodable | Wire Mbps | BBR pacing rate | Window / RTT | Keyframe drain | Keyframes slow |
|---|---|---|---|---|---|---|---|---|---|
| 15 Mbps | carrier · BBR | alone | 2 | 0.9 % (0.9–1.0) | 11.9 | 16.1 | 100 | 15.7 | 35 % |
| 15 Mbps | carrier · BBR | beside | 2 | 18.4 % (13.0–23.8) | 11.7 | 7.8 | 21 | 7.7 | 100 % |
| 15 Mbps | carrier · Cubic | alone | 2 | 0.1 % (0.1–0.1) | 11.2 | – | 38 | 16.3 | 16 % |
| 15 Mbps | carrier · Cubic | beside | 2 | 14.6 % (13.6–15.6) | 10.0 | – | 11 | 7.9 | 100 % |
| 15 Mbps | datagram · BBR | alone | 2 | 18.7 % (17.7–19.8) | 12.8 | 8.4 | 216 | 13.2 | 75 % |
| 15 Mbps | datagram · BBR | beside | 2 | 70.0 % (61.8–78.2) | 9.3 | 7.7 | 11 | 4.2 | 97 % |
| 15 Mbps | datagram · Cubic | alone | 2 | 21.6 % (20.7–22.5) | 11.1 | – | 43 | 13.5 | 47 % |
| 15 Mbps | datagram · Cubic | beside | 2 | 44.2 % (42.4–46.0) | 9.5 | – | 10 | 2.4 | 100 % |
| 30 Mbps | carrier · BBR | alone | 2 | 0.0 % (0.0–0.0) | 11.8 | 0.0 | 38715 | 33.7 | 0 % |
| 30 Mbps | carrier · BBR | beside | 2 | 10.4 % (4.5–16.2) | 11.1 | 7.1 | 111 | 12.7 | 49 % |
| 30 Mbps | carrier · Cubic | alone | 2 | 0.0 % (0.0–0.0) | 11.1 | – | 114 | 38.9 | 0 % |
| 30 Mbps | carrier · Cubic | beside | 2 | 6.8 % (1.0–12.5) | 10.7 | – | 16 | 11.0 | 66 % |
| 30 Mbps | datagram · BBR | alone | 2 | 1.8 % (0.9–2.7) | 11.4 | 7.5 | 1122 | 37.1 | 1 % |
| 30 Mbps | datagram · BBR | beside | 2 | 49.9 % (46.7–53.2) | 8.3 | 12.4 | 9 | 2.7 | 87 % |
| 30 Mbps | datagram · Cubic | alone | 2 | 3.6 % (1.8–5.3) | 11.1 | – | 125 | 39.2 | 0 % |
| 30 Mbps | datagram · Cubic | beside | 2 | 7.8 % (6.3–9.2) | 11.0 | – | 18 | 8.5 | 91 % |

*Keyframe drain* takes two minimum RTTs off (D3). *Keyframes slow* is
under 1.2× the run's median wire rate, which only approximates the
engine's rolling five-second rate. *Undecodable* is
`analyze.py`'s from 25 s on, live-edge, without the viewer replay.

---

## 3. Decisions

### D1 — The signal is the keyframe's drain, not quinn's estimate (F1, F2)

`crates/engine/src/drain.rs` times each keyframe stream the engine
already sends, from the start of its write to its acknowledgement
(`KeyframeOutcome::Sent`), and computes its drain rate: its bytes over
that time less the path's minimum RTT. It needs nothing from the relay and
no new traffic, and it works the same whatever the controller and the
delivery mode, because keyframes ride a reliable stream in both modes.
A keyframe acknowledged within one minimum RTT drained as fast as the path
goes. A keyframe that never finished (superseded, failed) is already a bad
second through the F-12 signal.

### D2 — Against the stream's own wire rate

The comparison is with what the connection is being asked to carry:
quinn's `udp_tx` bytes (media, parity, audio, retransmissions and QUIC
overhead), averaged over the last 5 s and sampled at each keyframe's
acknowledgement. It is not compared with the rung's target bitrate,
because the encoder's actual rate is what the uplink has to carry, and
because a stream's first second has no rate history: a keyframe is not
judged until the samples span 1 s. A resume's connection restarts its
counters, so the history restarts with it (`set_relay`, and a counter that
goes backwards).

### D3 — Every keyframe in the second under 1.2× that rate (F3)

A keyframe is *slow* when it drained at under `SLOW_DRAIN_FACTOR` = 1.2
times the wire rate, its drain time being its time to acknowledgement
less `DRAIN_RTTS` = 2 minimum RTTs (one every write costs, one for a loss
recovery). A
second is bad in both delivery modes when at least one keyframe was
acknowledged in it and all of those were slow. The monitor's hysteresis
does the rest. Every factor from 1.1 to 2.0 meets both the thin cell
and RU4's criterion. 1.2 is low on that range, so an alone uplink with
room stays quiet.

### D4 — Telemetry carries the inputs

`keyframeStreamsSlow` (a counter, a subset of `keyframeStreamsSent`) and
`uplinkMinRttMs` (a gauge) join the native broadcaster's stats, the
telemetry schema, the catalogue, the rollup's counters and the stored
shape, so a session's row shows why the warning raised. The shells
already record the warning itself as an event (docs/77 D13).

### D5 — Carrier expiries count only on a short path (F4)

The *Reliable* rule "every carrier that retired in the second expired"
applies only while the minimum RTT is at most
`CARRIER_EXPIRY_MAX_MIN_RTT_MS` = 50 ms, a third of
`CARRIER_RETIRE_DEADLINE`. On a longer path a full uplink still shows as
records shed and slow keyframes.

### D6 — Not done

- No new copy, no new UI. The warning means what it meant; it is now right
  in more places. Diagnostics shows no new row; the counters are in the
  telemetry row and the diagnostics JSON's engine stats only.
- No ladder step. The warning advises; docs/38's rung stays the
  broadcaster's choice.
- Nothing for the browser broadcaster. RU9's send queue is its capacity
  signal, and Chrome exposes no acknowledgement time for a stream write.
- F5's case (a link that carries the average but not the bursts) is not
  addressed.

---

## 4. What users see

Nothing new to read. Where the uplink cannot carry the stream with BBR,
which is most broadcasters since 2026-10-21, the existing pill and
sentence ("Your upload can't keep up…") now show, on the desktop apps and
on iOS, which runs the same engine. They stay off on radio outages and
random loss, as RU4 made them. On a long lossy path in *Reliable* delivery
(100 ms, 2 % loss) they no longer show while viewers see the picture.

---

## 5. Architecture

```
keyframe writer (sender.rs) ──Sent──► KeyframeDrain::observe ──slow?──► Stats.keyframe_streams_slow
        │ write start, ack time           ▲ wire_bytes, min_rtt_us
        │                                 │
        └──────────── RelaySession::path_counters (quinn ConnectionStats)
Stats (1 Hz) ──► UplinkMonitor::observe ──► the shell's warning (desktop, iOS)
```

`PathCounters` gains `wire_bytes` and `min_rtt_us`. `Stats` gains
`keyframe_streams_slow` and `uplink_min_rtt_ms`. The monitor's inputs stay
cumulative counters read at 1 Hz, so a second read of `stats()` (the
diagnostics, the telemetry tick) cannot reset a window.

---

## 6. Chunks and acceptance criteria

| Chunk | What | Acceptance criterion | Verified by |
|---|---|---|---|
| UW1 | `drain.rs`; the keyframe writer times each acknowledged keyframe; `PathCounters.wire_bytes` / `min_rtt_us`; `Stats.keyframe_streams_slow` | Under 1.2× the 5 s wire rate is slow, at or over is not; two minimum RTTs are not drain time; nothing is judged under 1 s of history or without counters; a resume restarts the history; through the real writer, keyframes held 300 ms on a 12 Mbps stream count and ones held 20 ms do not | unit (`drain::tests`, `sender::tests`) |
| UW2 | The monitor: D3's rule in both modes; D5's gate | Every keyframe slow warns in both modes; one of two, none, or none acknowledged does not; the thin cell's lab seconds with BBR warn in both modes within the streak (red first against RU4's monitor); the 100 ms 2 % loss carrier seconds do not on a 100 ms path and do on a 10 ms one (red first); every RU4 table test unchanged | unit (`uplink::tests`, lab replays) |
| UW3 | Telemetry (D4); iOS's sample | `keyframeStreamsSlow` and `uplinkMinRttMs` typed, catalogued and in the stored shape; the engine's key-set test; iOS's sample keeps them | unit (gawk-telemetry, engine, gawk-ios) |
| UW4 | The lab: `B` lines and `T`/`Y` columns in `sender-engine`; `tools/uplinklab/r75.py`; §2 and §7 | §1's lab rows pass on §7's run; the tables come from `r75.py` | lab, recorded |
| UW5 | The hardware pass | On a real thin uplink beside an upload (the reference homelab's shaped port, or a phone hotspot) the warning shows with BBR in both delivery modes and stays off on Wi-Fi with AWDL | owner, hardware |

---

## 7. Acceptance run (2026-11-05)

The shipped engine (`SLOW_DRAIN_FACTOR` 1.2, `DRAIN_RTTS` 2, D5's gate)
in `bin/sender-engine`, on table O's harness (three runs a 1080p cell, two
at 80 Mbps 1440p, the 1440p cells alone on the host) and on docs/75 table
R's matrix with its 1440p cells. Every warning column is what the engine
in the run showed (its `Y` lines), not a replay.

**Result.**

- **The thin cell beside the upload passes with BBR in both delivery
  modes**, the case R75 exists for. The warning shows by 25 s in all six
  runs, 20 s in most. In some runs it shows before the upload, because
  15 Mbps alone is only 1.27× this stream's wire rate.
- **With Cubic, one run of six is late**: carrier · Cubic raised at 30 s,
  5 s past the gate. Second 25 had one fast keyframe out of two, which
  broke a streak that had started at 21. Before R75 that cell raised at
  26–74 s (docs/75 table O3). This is the one row of §1 that fails; F3's
  three passes show that moving the threshold again would be fitting this
  lab's noise.
- **No alone uplink with room raises**: 30 and 80 Mbps alone, every mode
  and controller. 15 Mbps alone raises in the damaged datagram runs and in
  two of three carrier · BBR runs (≈1 % undecodable; §9).
- **No matrix row RU4 gates raises** (U4), the 1440p cells included.
  Table R's false raise on the 100 ms, 2 % loss carrier · BBR cell is
  gone.
- **Beside the upload at 30 and 80 Mbps** it raises in every run, mostly
  late. Viewers there lose 6–70 % of frames, except carrier · Cubic at
  30 Mbps, which raised with 0.6 % lost: the upload's queue slows its
  keyframes while the carrier still delivers them. That is recorded, not
  gated.
- **One new raise on a recorded row**: 1440p, 80 Mbps with outages,
  carrier · Cubic, at 16 s for 38 % of the window, 2.4 % undecodable.
  RU4 records that row and gates only the 1080p outage cells with Cubic.

#### U1 — the acceptance runs (medians over 25–80 s; Mbps)

| Uplink | Delivery · controller | Upload | Runs | Undecodable | Wire Mbps | BBR pacing rate | Window / RTT | Keyframe drain | Keyframes slow |
|---|---|---|---|---|---|---|---|---|---|
| 15 Mbps | carrier · BBR | alone | 3 | 1.2 % (0.4–1.9) | 12.0 | 10.6 | 98 | 15.5 | 38 % |
| 15 Mbps | carrier · BBR | beside | 3 | 6.1 % (4.8–8.0) | 12.7 | 8.8 | 24 | 8.3 | 100 % |
| 15 Mbps | carrier · Cubic | alone | 3 | 0.1 % (0.1–0.2) | 11.1 | – | 37 | 16.2 | 13 % |
| 15 Mbps | carrier · Cubic | beside | 3 | 14.6 % (11.9–19.8) | 10.0 | – | 10 | 8.1 | 100 % |
| 15 Mbps | datagram · BBR | alone | 3 | 18.1 % (16.6–19.9) | 13.1 | 7.6 | 457 | 13.1 | 79 % |
| 15 Mbps | datagram · BBR | beside | 3 | 75.2 % (73.3–76.8) | 9.7 | 6.8 | 12 | 5.9 | 96 % |
| 15 Mbps | datagram · Cubic | alone | 3 | 17.9 % (16.2–19.5) | 11.1 | – | 41 | 13.4 | 51 % |
| 15 Mbps | datagram · Cubic | beside | 3 | 52.5 % (47.3–60.4) | 8.9 | – | 9 | 1.9 | 100 % |
| 30 Mbps | carrier · BBR | alone | 3 | 0.0 % (0.0–0.0) | 11.7 | 2.5 | 25385 | 34.6 | 0 % |
| 30 Mbps | carrier · BBR | beside | 3 | 8.4 % (0.7–19.9) | 11.2 | 6.1 | 2173 | 13.9 | 48 % |
| 30 Mbps | carrier · Cubic | alone | 3 | 0.0 % (0.0–0.0) | 11.1 | – | 125 | 39.1 | 0 % |
| 30 Mbps | carrier · Cubic | beside | 3 | 0.6 % (0.2–1.2) | 11.0 | – | 19 | 12.4 | 63 % |
| 30 Mbps | datagram · BBR | alone | 3 | 0.3 % (0.0–0.9) | 11.4 | 8.2 | 2982 | 35.1 | 1 % |
| 30 Mbps | datagram · BBR | beside | 3 | 45.1 % (42.9–47.5) | 9.1 | 13.1 | 9 | 2.8 | 90 % |
| 30 Mbps | datagram · Cubic | alone | 3 | 4.8 % (3.6–5.4) | 11.1 | – | 133 | 39.3 | 0 % |
| 30 Mbps | datagram · Cubic | beside | 3 | 7.5 % (2.6–12.1) | 11.0 | – | 21 | 9.9 | 82 % |
| 80 Mbps | carrier · BBR | alone | 2 | 0.1 % (0.0–0.3) | 39.8 | 16.5 | 1911 | 100.9 | 2 % |
| 80 Mbps | carrier · BBR | beside | 2 | 42.6 % (37.9–47.3) | 28.1 | 46.8 | 31 | 22.4 | 87 % |
| 80 Mbps | carrier · Cubic | alone | 2 | 0.0 % (0.0–0.0) | 39.5 | – | 1181 | 101.9 | 0 % |
| 80 Mbps | carrier · Cubic | beside | 2 | 6.5 % (3.5–9.5) | 38.6 | – | 66 | 36.5 | 76 % |
| 80 Mbps | datagram · BBR | alone | 2 | 0.4 % (0.0–0.8) | 39.8 | 48.4 | 286 | 90.7 | 1 % |
| 80 Mbps | datagram · BBR | beside | 2 | 70.0 % (66.8–73.3) | 26.6 | 44.8 | 28 | 4.9 | 96 % |
| 80 Mbps | datagram · Cubic | alone | 2 | 0.8 % (0.8–0.8) | 39.5 | – | 1316 | 102.4 | 0 % |
| 80 Mbps | datagram · Cubic | beside | 2 | 19.4 % (10.9–28.0) | 39.7 | – | 54 | 17.9 | 98 % |

#### U3 — the warning on the shared-uplink cells, as the engine showed it

*Judged*: 15 Mbps beside the upload must raise within 5 s of the upload
(by 26 s after the first frame) in every run; alone uplinks with room
must not raise; the rest is recorded. *Late* counts runs past that.

| Uplink | Delivery · controller | Upload | Undecodable | Warning | Judged |
|---|---|---|---|---|---|
| 15 Mbps | carrier · BBR | alone | 1.2 % | 2/3, first at 20–20 s, shown 67 % | recorded (every damaged run) |
| 15 Mbps | carrier · BBR | beside | 6.1 % | 3/3, first at 20–25 s, shown 100 % | pass |
| 15 Mbps | carrier · Cubic | alone | 0.1 % | never (0/3) | recorded (every damaged run) |
| 15 Mbps | carrier · Cubic | beside | 14.6 % | 3/3, first at 25–30 s (1 late), shown 98 % | **fail** |
| 15 Mbps | datagram · BBR | alone | 18.1 % | 3/3, first at 20–20 s, shown 100 % | recorded (every damaged run) |
| 15 Mbps | datagram · BBR | beside | 75.2 % | 3/3, first at 20–20 s, shown 100 % | pass |
| 15 Mbps | datagram · Cubic | alone | 17.9 % | 3/3, first at 20–20 s, shown 94 % | recorded (every damaged run) |
| 15 Mbps | datagram · Cubic | beside | 52.5 % | 3/3, first at 20–20 s, shown 100 % | pass |
| 30 Mbps | carrier · BBR | alone | 0.0 % | never (0/3) | pass |
| 30 Mbps | carrier · BBR | beside | 8.4 % | 3/3, first at 67–75 s (3 late), shown 19 % | recorded |
| 30 Mbps | carrier · Cubic | alone | 0.0 % | never (0/3) | pass |
| 30 Mbps | carrier · Cubic | beside | 0.6 % | 3/3, first at 25–66 s (1 late), shown 76 % | recorded |
| 30 Mbps | datagram · BBR | alone | 0.3 % | never (0/3) | pass |
| 30 Mbps | datagram · BBR | beside | 45.1 % | 3/3, first at 25–25 s, shown 100 % | recorded |
| 30 Mbps | datagram · Cubic | alone | 4.8 % | never (0/3) | pass |
| 30 Mbps | datagram · Cubic | beside | 7.5 % | 3/3, first at 25–34 s (1 late), shown 95 % | recorded |
| 80 Mbps | carrier · BBR | alone | 0.1 % | never (0/2) | pass |
| 80 Mbps | carrier · BBR | beside | 42.6 % | 2/2, first at 27–31 s (2 late), shown 94 % | recorded |
| 80 Mbps | carrier · Cubic | alone | 0.0 % | never (0/2) | pass |
| 80 Mbps | carrier · Cubic | beside | 6.5 % | 2/2, first at 45–45 s (2 late), shown 65 % | recorded |
| 80 Mbps | datagram · BBR | alone | 0.4 % | never (0/2) | pass |
| 80 Mbps | datagram · BBR | beside | 70.0 % | 2/2, first at 26–26 s (1 late), shown 98 % | recorded |
| 80 Mbps | datagram · Cubic | alone | 0.8 % | never (0/2) | pass |
| 80 Mbps | datagram · Cubic | beside | 19.4 % | 2/2, first at 30–36 s (2 late), shown 86 % | recorded |

#### U4 — the warning on the engine matrix, as the engine showed it

Must not raise (RU4, kept): lossless cells with either controller, lossy ones with BBR, the 10 ms outage cells with Cubic too. Other rows are recorded.

| Cell | Delivery | Cubic: undecodable · warning | BBR: undecodable · warning | Judged |
|---|---|---|---|---|
| hi-rtt10-clean | datagram | 0.0 % · no | 0.0 % · no | pass |
| hi-rtt10-clean | carrier | 0.0 % · no | 0.0 % · no | pass |
| hi-rtt10-ge1pct | datagram | 88.3 % · at 9 s, 100 % | 60.4 % · no | pass |
| hi-rtt10-ge1pct | carrier | 42.2 % · at 9 s, 100 % | 0.4 % · no | pass |
| hi-rtt10-loss0.001 | datagram | 30.3 % · at 9 s, 100 % | 0.0 % · no | pass |
| hi-rtt10-loss0.001 | carrier | 10.7 % · at 13 s, 100 % | 0.0 % · no | pass |
| hi-rtt10-rate80M-out80drop | datagram | 21.3 % · at 77 s, 7 % | 23.4 % · no | pass |
| hi-rtt10-rate80M-out80drop | carrier | 2.4 % · at 16 s, 38 % | 1.4 % · no | pass |
| hi-rtt10-rate80M | datagram | 0.0 % · no | 0.0 % · no | pass |
| hi-rtt10-rate80M | carrier | 0.0 % · no | 0.0 % · no | pass |
| hi-rtt10-rate300M-out80drop | datagram | 20.7 % · no | 26.1 % · no | pass |
| hi-rtt10-rate300M-out80drop | carrier | 1.7 % · no | 0.1 % · no | pass |
| hi-rtt10-rate300M | datagram | 0.0 % · no | 0.3 % · no | pass |
| hi-rtt10-rate300M | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt10-clean | datagram | 0.0 % · no | 0.0 % · no | pass |
| rtt10-clean | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt10-ge1pct | datagram | 16.2 % · no | 15.6 % · no | pass |
| rtt10-ge1pct | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt10-loss0.001 | datagram | 0.0 % · no | 0.0 % · no | pass |
| rtt10-loss0.001 | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt10-loss0.01 | datagram | 1.6 % · at 14 s, 100 % | 1.2 % · no | pass |
| rtt10-loss0.01 | carrier | 3.5 % · no | 0.0 % · no | pass |
| rtt10-loss0.02 | datagram | 36.3 % · at 8 s, 100 % | 3.7 % · no | pass |
| rtt10-loss0.02 | carrier | 22.4 % · at 14 s, 100 % | 0.0 % · no | pass |
| rtt10-loss0.005 | datagram | 0.3 % · no | 0.0 % · no | pass |
| rtt10-loss0.005 | carrier | 0.1 % · no | 0.0 % · no | pass |
| rtt10-out20drop | datagram | 22.4 % · no | 22.4 % · no | pass |
| rtt10-out20drop | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt10-out80drop | datagram | 22.4 % · no | 27.5 % · no | pass |
| rtt10-out80drop | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt40-clean | datagram | 0.0 % · no | 0.0 % · no | pass |
| rtt40-clean | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt40-ge1pct | datagram | 82.1 % · at 13 s, 100 % | 15.8 % · no | pass |
| rtt40-ge1pct | carrier | 54.0 % · at 14 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.01 | datagram | 99.2 % · at 8 s, 100 % | 0.6 % · no | pass |
| rtt40-loss0.01 | carrier | 88.5 % · at 9 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.001 | datagram | 23.5 % · at 14 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.001 | carrier | 15.5 % · at 16 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.02 | datagram | 99.8 % · at 8 s, 100 % | 3.6 % · no | pass |
| rtt40-loss0.02 | carrier | 93.7 % · at 9 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.005 | datagram | 93.3 % · at 9 s, 100 % | 0.0 % · no | pass |
| rtt40-loss0.005 | carrier | 78.1 % · at 12 s, 100 % | 0.0 % · no | pass |
| rtt40-out20drop | datagram | 43.8 % · at 14 s, 100 % | 22.1 % · no | pass |
| rtt40-out20drop | carrier | 17.7 % · at 15 s, 100 % | 0.0 % · no | pass |
| rtt40-out80drop | datagram | 47.4 % · at 14 s, 100 % | 27.8 % · no | pass |
| rtt40-out80drop | carrier | 23.1 % · at 15 s, 100 % | 0.0 % · no | pass |
| rtt100-clean | datagram | 0.0 % · no | 0.0 % · no | pass |
| rtt100-clean | carrier | 0.0 % · no | 0.0 % · no | pass |
| rtt100-ge1pct | datagram | 96.5 % · at 13 s, 100 % | 21.2 % · no | pass |
| rtt100-ge1pct | carrier | 86.4 % · at 15 s, 100 % | 0.1 % · no | pass |
| rtt100-loss0.001 | datagram | 90.1 % · at 14 s, 100 % | 0.0 % · no | pass |
| rtt100-loss0.001 | carrier | 73.0 % · at 15 s, 100 % | 0.0 % · no | pass |
| rtt100-loss0.01 | datagram | 99.9 % · at 9 s, 100 % | 1.2 % · no | pass |
| rtt100-loss0.01 | carrier | 97.5 % · at 9 s, 100 % | 0.5 % · no | pass |
| rtt100-loss0.02 | datagram | 100.0 % · at 9 s, 100 % | 3.1 % · no | pass |
| rtt100-loss0.02 | carrier | 99.0 % · at 9 s, 100 % | 2.0 % · no | pass |
| rtt100-loss0.005 | datagram | 97.5 % · at 11 s, 100 % | 0.2 % · no | pass |
| rtt100-loss0.005 | carrier | 92.9 % · at 13 s, 100 % | 0.3 % · no | pass |
| rtt100-out20drop | datagram | 95.5 % · at 14 s, 100 % | 23.0 % · no | pass |
| rtt100-out20drop | carrier | 87.4 % · at 15 s, 100 % | 0.1 % · no | pass |
| rtt100-out80drop | datagram | 96.1 % · at 14 s, 100 % | 28.8 % · no | pass |
| rtt100-out80drop | carrier | 92.1 % · at 15 s, 100 % | 0.6 % · no | pass |

0 row(s) fail.

---

## 8. Commands

```sh
# Build the lab and the traces (docs/75 §4), then:
tools/uplinklab/build.sh && tools/uplinklab/traces.sh
# Table O's shared-uplink cells with the engine (three runs a 1080p cell, two at 80 Mbps):
FAIR_CCS="engine-cubic engine-bbr" FAIR_NONE=0 tools/uplinklab/fair/matrix.sh
tools/uplinklab/r75.py fair                 # U1 and U3 as the runs showed them
# docs/75 table R's matrix with the engine, and the 1440p cells alone on the host:
tools/uplinklab/matrix.py engine && tools/uplinklab/matrix.py engine-hi --jobs 1
tools/uplinklab/r75.py matrix               # U4
# §2's replay over runs made before the engine had the rule, and F3's sweep:
R75_SLOW_SHARE=0.5 tools/uplinklab/r75.py fair <dir> --replay
tools/uplinklab/r75.py seconds <run-dir>    # one run, a row a second
```

---

## 9. Risks

- **A keyframe is the only probe.** A stream with a long GOP would be
  judged that rarely: a second with no keyframe acknowledged is never bad
  by this rule, so the streak needs a keyframe every second. The engine's
  rungs use a 500 ms GOP (docs/38 D11), and a forced IDR only adds
  keyframes.
- **Keyframe size.** A tiny keyframe (a static screen) drains inside one
  RTT and reads as fast; at those sizes the stream is far under any uplink.
- **quinn upgrades.** The drain is measured by the stream, not read from
  quinn's controller, so F1's quirks cannot move it; `udp_tx.bytes` and
  `path.min_rtt` are stable public stats.
- **The threshold was tuned on one lab.** Three passes moved it (F3). The
  plateau it sits on is wide in the lab, 1.1 to 2.0, but its floor is set
  by the thin cell's noise and its ceiling by how much room an uplink has.
  A real uplink's noise could differ, and UW5 is where that shows.
- **A nearly full uplink can raise it.** At 1.2 a link only 1.27× the
  stream's wire rate (15 Mbps alone) raised it in 4 of 8 carrier · BBR
  runs, at 0.3–1.0 % undecodable. A lower rung is a reasonable answer there.
- **One host, one shaper.** The lab is a tbf with a 50 ms queue on loopback.
  UW5 is the real-network check.

---

## 10. References

- [docs/75](75-reliable-uplink.md) — R73: RU4 (the warning as a capacity
  signal), D5 (BBR), D6 (the loss notice), tables O, O3 and R
- [docs/38](38-windows-native-broadcaster.md) — D12, the upload monitor
- [docs/77](77-broadcaster-memory.md) — D13, the warning as a telemetry event
- quinn-proto 0.11.19, `congestion/bbr/bw_estimation.rs` (the
  application-limited filter) and `connection/pacing.rs` (window-based pacing)
