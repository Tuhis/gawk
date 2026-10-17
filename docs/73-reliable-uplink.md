# R71 — Reliable broadcaster uplink: measured first (docs/73)

**Status**: designed 2026-10-17 from a measurement pass (§4, §11); owner
decisions OD1–OD6 open (§3); chunks **RU0–RU8** not started (`RU` =
Reliable Uplink). **Split out of R55** ([docs/57](57-wifi-uplink.md)) on
2026-10-17 by owner direction: how deltas cross the broadcaster → relay leg
is not a Wi-Fi feature. docs/57 keeps the Mac's Wi-Fi status line (WU3,
built), Improve (WU4, deferred), QoS marking (WU5) and the Wi-Fi hardware pass
(WU6). Its D1–D5, D9 and chunks WU0–WU2 move here, revised where the lab
contradicted them (§5).

Touches `gawk-server` (carrier ingest on the publish session, one capability
bit, receive windows, and quic-go's per-session datagram queue),
`gawk-broadcast-desktop` (the engine's delta sender, its congestion
controller, one Quality-page setting, telemetry) and therefore `gawk-ios` (same engine, by path), `gawk-telemetry`
(fields and playbook rows) and the wire mirrors.

---

## 1. Why

Leg A is the broadcaster's uplink to the relay. Every viewer of a broadcast
receives what leg A delivered, so one lost chunk there is lost for all of
them, and it costs more than its own frame. A delta that arrives without its
predecessor cannot be decoded, so the viewer skips one frame per GOP (R29's
allowance) and otherwise freezes until the next keyframe, up to 500 ms
later.

What protects leg A today:

- **Parity**: the broadcaster sends up to two P/Q symbols per delta frame
  (R29, `k = 2` by default). The relay forwards them without reconstructing
  anything, so a chunk lost on leg A uses up one of the frame's two erasures
  **for every viewer** before leg B has lost anything.
- **Keyframes ride a reliable stream** (R8) with the F-12 in-flight rule.
- **The transport is quinn's defaults** (`quinn-proto` 0.11.16, read from
  source): Cubic congestion control, a 1 MiB datagram send buffer that drops
  its oldest entry when full, and DATAGRAM frames packed into each packet
  ahead of any stream data.

R55 found leg A failing on a Mac on Wi-Fi (3.7 % of frames lost, AWDL) and
designed a per-GOP reliable stream for deltas, "R19's carrier, reversed". A
leg-A failure is not specific to Wi-Fi, though: a home uplink, a VPN or a
long path to the official relay can lose packets just as well. On 2026-10-17
the owner moved the transport into its own design with three instructions:

1. **Latency and user experience are what count**, measured, not argued.
2. The broadcaster should be able to choose **the absolute lowest latency**.
3. If reliability costs a negligible amount of latency, **reconsider the
   default**.

So this doc starts from measurements (§4), states what they showed (§2), and
only then decides (§3, §5).

## 2. What the lab found (2026-10-17)

Numbers are from §11's tables; "Balanced" is the default viewer, "live
edge" the *Lowest latency* preset.

**F1 — On a clean path the carrier costs nothing measurable.** At 10, 40
and 100 ms RTT, decode-ready latency differs by at most 2 ms at p50, p95
and p99 — the carrier never more than 1 ms behind — bytes on the wire
differ by 0.1 %, and all four viewer presets see the same picture (§11
tables A, J). The carrier even sends ~10 % fewer packets: records
fill full-size packets, while a 1200-byte datagram never shares one.

**F2 — Bursts and outages are where datagrams fail, and the carrier
doesn't.** Parity repairs up to two lost chunks per frame; a burst or a
radio outage takes more. With the radio away 20 or 80 ms every second, or
≈ 1 % loss in bursts of ≈ 4 packets, datagram delivery leaves 19–29 % of
frames undecodable and breaks 36–51 % of GOPs at every RTT; the carrier
leaves 0–0.3 %. The default viewer goes from 71–93 % of frames shown to
93.5–100 %, for a playout offset 0–57 ms higher at 10–40 ms RTT and up to
120 ms higher at 100 ms (§11 table B). This is the R55 case.

**F3 — Random loss is different: parity already repairs most of it, and
waiting costs a round trip.** A lost packet on the carrier holds every later
frame of its GOP for about one RTT; a lost chunk under parity costs nothing
until a frame loses three. At 10 ms RTT that wait is invisible. At 40 ms and
1–2 % random loss the default viewer still does better on the carrier (99.2
vs 95.8 % shown at 1 %), but a live-edge viewer, who presents frames as they
arrive, stalls more: 29.7 vs 12.0 % of the time. At 100 ms and 0.5–2 % the live-edge
viewer stalls about twice as much on the carrier (§11 table C).
**What a broadcaster's delivery choice costs depends on a viewer setting the
broadcaster cannot see.** The default viewer is the one to optimise for.

**F4 — Cubic, not the delivery mode, is the larger problem.** quinn's
default controller treats every random loss as congestion: at 0.5 % loss and
10 ms RTT it took 535 congestion events in 100 s and its window fell from
~446 KB to 15–50 KB. A 230 KB keyframe then needs ~10 round trips and both
delivery modes queue behind it. Throughput falls below the stream's needs
at **≥ 0.5 % loss at 10 ms, and already at 0.1 % at 40 or 100 ms** (5.2–5.6 of
10.8 Mbps at 100 ms) — the loss-limited rate of a Reno-style controller,
roughly MSS/RTT × 1.22/√p. At 40 ms a 0.1 % loss rate left 16–23 % of
frames undecodable whichever mode carried them (§11 table D). 40 ms is an
ordinary RTT from elsewhere in Europe to the Finnish reference fleet.

**F5 — quinn's BBR removes it.** The same paths with `BbrConfig`: 0 %
undecodable at 40 ms and 0.1–0.5 % loss in both modes; the carrier at
10 ms keeps every frame up to 2 % random loss with a decode-ready p99 of
29 ms (Cubic: 24 % undecodable, p99 390 ms). It also trims the clean-path
tail (p99 15 → 10 ms at 10 ms RTT) because keyframes no longer wait for a
window that loss shrank. It is not better everywhere: on datagrams under
radio outages BBR shows 0.6–3.7 points fewer frames than Cubic (73.5 vs
77.2 % at 10 ms with 80 ms outages), at a far lower tail (p99 14 vs 100 ms) —
Cubic backs off after the first loss and *delays* what it sends into an
outage, BBR keeps sending and loses it — and on a clean 1440p path its
faster bursts overflow quic-go's 32-deep queue (F7) more than Cubic's (81.8
vs 89.5 % shown), which D7's longer queue removes (100 % both, F15). On the
carrier, which retransmits, BBR is within the run-to-run spread of Cubic or
better on every cell (§11 table M). §10 lists what makes it a risk.

**F6 — Under a small window, datagram-first packing starves keyframes.**
quinn writes DATAGRAM frames into each packet before STREAM data (docs/38
F-12). With Cubic's window collapsed (0.5 %, 10 ms), datagram mode's deltas
stayed fast (p95 27 ms) while keyframes took p50 125 ms, p95 448 ms — and
nothing in a GOP decodes before its keyframe. The carrier, with the keyframe
at priority 1, got keyframes through at p50 81 / p95 182 ms.

**F7 — The relay drops leg-A datagrams the network delivered.** quic-go's
HTTP/3 layer keeps at most `streamDatagramQueueLen = 32` datagrams per
WebTransport session and silently discards new ones beyond that
(`http3/state_tracking_stream.go`, quic-go 0.62.0). At the high rung a
delta is a burst of 52–84 datagrams, and 32 is about a millisecond of them
at a 300 Mbps arrival rate: any scheduling gap in the reading goroutine
drops the burst's tail, parity included. Measured on the **real relay**
(§11 table G), on a clean 300 Mbps path where quinn counted no lost packet:
at 1440p with 15 viewers the relay's own `ingress_chunks_lost_total` read
**4 252 chunks a minute (≈ 2 %)**, and 182 with no viewers. Raising that
one constant to 4096 in a test build took both to **0**; raising quic-go's
other queue (128, per connection) changed nothing, and a goroutine that
does nothing but drain the queue did not help either (3 957), because it is
starved by the same CPU as the loop it replaced. At 1080p (10–17 datagrams
per delta) the stock relay lost nothing. The signature — chunks lost, no
whole frames — is the one `BUGS.md` recorded in the 2026-09-27 incident
(21–22 % of chunks, 0.01 % of frames); that broadcast's rung is not on
record, so it is consistent with this, not proven to be it. The carrier is
immune: streams are flow-controlled, never dropped.

**F8 — Two quinn facts the design must respect.** quinn's `SendStream` has
no per-offset acknowledgement API (D3), and its MTU black-hole detector
fired on purely random loss — at 2 % loss it cut `max_datagram_size` to 1165
about 17 s in. The engine's existing re-chunk on `TooLarge` recovers; the
lab's first sender did not (§4).

**F9 — The default viewer pays the carrier's cost through its playout
offset, and keeps paying for a minute.** docs/57 D9 assumed live-edge
viewers; the shipping default is `balanced` (adaptive playout,
`playbackPresets.ts`). Its offset follows the p95 of a 60 s arrival window
and descends at 5 ms/s after a 15 s dwell. 10 s of 2 % loss (Cubic, 10 ms)
lifted it to 257 ms on the carrier and to its 350 ms ceiling on datagrams,
and 30 s after the loss stopped it was still 237 / 350 ms (§11 figure). With
BBR on a bursty or briefly interrupted 10 ms path it stayed at its 50 ms
floor.

**F10 — Shedding is not optional, and it has two halves.** On an uplink
slower than the stream (Cubic on a lossy long path), a carrier that queues
instead of dropping turns the shortfall into unbounded delay: the lab's
first sender reached 16–70 s. Shedding queued records of superseded GOPs was
not enough on its own — a write blocked on a starved carrier kept that GOP's
stream alive and trickling for 25–36 s, because shedding only ran between
writes. With both rules (D3) latency stays bounded and the shortfall shows
up as lost GOPs, as it does with datagrams.

**F11 — A smaller datagram send buffer is not worth it.** Cutting quinn's
1 MiB buffer to 150 KB (≈ 100 ms at the default ceiling) barely trimmed
Cubic's 0.5 % tail (p95 285 → 266 ms) and cost 1.5 % of frames, did
nothing measurable with BBR, and turned one BBR throughput dip into four
lost frames on a clean path.

**F12 — At the high rung the carrier matters more, and docs/57 D9's headroom
fear did not hold.** At 1440p60 (38.9 Mbps mean, 50 Mbps ceiling, deltas of
52–84 chunks) datagram delivery lost frames even on a clean path —
12.6 % (Cubic) and 23.2 % (BBR) undecodable on loopback with the run alone
on the host, 7–9 % behind a 300 Mbps bottleneck. Those
losses were not on the path: behind 300 Mbps quinn counted no lost packet
and the path dropped nothing, yet ~500 of 356 000 datagrams never reached
the receiving application — quic-go's 32-deep queue (F7) — and because they
were the tails of the largest frames they broke 7–9 % of frames. (Behind
80 Mbps the bottleneck's own queue drops most of what is lost: 3 %.) The
carrier lost none. With BBR it also kept every GOP through
0.1 % loss and through bursts, and through 80 ms outages behind 300 or
80 Mbps of capacity it left 0.1–0.2 % undecodable with the Balanced viewer
at 95–98 % shown and an offset of 85–121 ms. D9 predicted the 80 Mbps case
would expire a GOP at every outage; with the deadline at the GOP boundary
(D3) 11 of ~200 carriers expired. Cubic collapses at this rung under bursts
(54 % undecodable on the carrier, 91 % on datagrams) (§11 table E).

**F13 — None of it depends on a perfectly steady path.** With 3 ms of
delay jitter at 10 and 40 ms RTT, every comparison above keeps its sign:
Cubic gets more fragile (0.1 % loss at 40 ms: 28 % of frames undecodable on
datagrams, 14 % on the carrier), BBR stays at 0 % on random loss in both
modes, the carrier with BBR stays at 0 % through bursts and outages where
datagrams lose 18–28 %, and F3's trade reappears (40 ms, 0.5 %: the
Balanced viewer sees 99.6 % of frames on the carrier, the live-edge viewer
stalls 23 % of the time against 7 % on datagrams) (§11 table F).

**F14 — The viewers who chose the most stability are the most exposed to
leg A.** *Smoother* and *Most stable* ride reliable delivery (R19, R21), so
the relay serves them **no parity** (`hub.NegotiateParity`: "pure egress
waste"), the DVR ring keeps none, and their reorder buffer has **no skip
allowance**. That was designed for a lossy leg B and a clean leg A. On a
datagram uplink, a chunk lost on leg A — one parity repairs for *Balanced*
viewers without their noticing — is a freeze to the next keyframe for them,
and a 3 s buffer does not help: it waits for frames that never reached the
relay. Replaying the same traces through the shipping code in those modes
(BBR uplink, §11 table J):

- At **0.1 % random loss** — about what the R55 Mac measured with AWDL up —
  *Balanced* shows 100 % of frames; *Smoother* and *Most stable* show
  87.5–89 %, stall 11–13 % of the time and freeze ~14–16 times a minute, at
  10, 40 and 100 ms alike. At 1 % they show 32–39 %.
- Under bursts and outages they are as badly off as live-edge viewers or
  worse: 70–77 % shown.
- **On the carrier uplink they show 94–100 % on every path measured, and
  98.6–100 % at 10–40 ms**, with no freezes of 250 ms or more.

Serving them parity would fix most of the random-loss half and none of the
outage half (§11 table K): 32–89 % → 95.6–100 % at 0.1–1 % loss, 70–72 % →
81–84 % under bursts, outages unchanged, because an outage takes whole
frames. Its cost is parity's share of the video bytes: 10.4 % at 1080p,
2.5 % at 1440p (measured on the lab's traces).

**F15 — BBR and a longer relay queue, without the carrier, fix random loss
for live-edge viewers and nothing else.** Measured as options of their own
(datagrams kept; §11 tables L1–L4, spread in table M):

- *Balanced*, random loss: BBR does the work at every RTT (40 ms, 0.1 %:
  63 → 99.9 %; 100 ms, 0.5 %: 0.1 → 94–97 %). The longer queue changes
  nothing at 1080p, where a delta is 10–17 datagrams, and is what makes the
  high rung work: on a clean 1440p path, 89.5 % today, 81.8 % with BBR
  alone, **100 % with BBR and the queue**, and 100 % at 0.1 % loss.
- *Balanced*, bursts and outages: BBR + queue is the same as BBR alone
  (three seeds agree within 3 points): 90–93 % under bursts at 10–40 ms,
  82 % at 100 ms, 57 % at 1440p; 71–92 % with the radio away. The carrier
  holds 93.5–100 % on the same cells.
- *Lowest latency*: the same split. On random loss over a long path BBR +
  queue stalls less than the carrier (100 ms, 1 %: 28.9 vs 58.7 % of the
  time) — F3's trade.
- *Smoother* and *Most stable* barely move, because they still get no
  parity: 86–89 % at 0.1 % loss, 34–41 % at 1 %, 70–77 % with the radio
  away. Adding OD6's parity lifts random loss to 95–100 % and bursts to
  80–84 %; outages stay at 70–77 %. Only the carrier gives them 94–100 %
  everywhere.
- Not explained: at 40 ms random loss, BBR + queue showed Balanced 3–4
  points lower than BBR alone on all three seeds (89–97 against 92–99.5 %)
  with no difference in delivery (undecodable, latency percentiles). The
  queue does no harm at 1080p that the transport counters can see; the
  viewer-side difference is small and its cause is open.

So the cheap path — BBR plus the queue, which together are one transport
option and one constant — repairs what parity can repair, for viewers who get
parity. Bursts, outages, and every viewer on reliable delivery need the
carrier (or, for random loss only, OD6).

## 3. Owner decisions (open)

Each row states the recommendation and the finding behind it. D4–D7 and
D11 are written as if the recommendation is taken; a different answer changes
those decisions and their chunks, nothing else.

| # | Question | Options | Recommendation |
|---|---|---|---|
| OD1 | What does a broadcaster get by default? | **Reliable** (the carrier, whenever the relay supports it) with *Lowest latency* (datagrams + parity) as a choice; or *Lowest latency* by default with Reliable as the choice. | **Reliable by default** (D4). It costs nothing on a clean path (F1), fixes the bursts and outages that motivated R55 (F2), serves the default viewer at least as well on every lossy path measured — within half a point on random loss, far better on bursts and outages (F2, F3) — and is the only thing measured that protects *Smoother* and *Most stable* viewers from every kind of leg-A loss (F14). *Lowest latency* stays a first-class choice, not a warned-against legacy mode: on a long path with random loss it is what keeps live-edge viewers from waiting (F3). |
| OD2 | Which congestion controller does the engine run? | quinn's Cubic (today); quinn's BBR; a controller of our own. | **BBR for both delivery modes, behind RU4's gates** (D5). F4/F5 make it the larger reliability lever. It helps *Lowest latency* on every random-loss path too, and costs it a few points of frames under outages (F5). It is the one decision here with a production risk (§10): RU4 ships it with the old controller one setting away, and reverts if the gates fail. Our own controller is out of scope (§9). |
| OD3 | Who can choose? | The broadcaster's app; the relay operator; viewers. | **The broadcaster (Quality page) and the operator (`-uplink-carriers`)** (D4, §6). Viewers choose their own buffering (R32 presets) and cannot choose how leg A is carried — it is one stream for all of them. The iOS app follows the engine default with no setting until someone asks for one. |
| OD4 | How does the relay stop dropping delivered datagrams (F7)? | Leave it; drain quic-go's queue into the relay's own; make quic-go's per-session queue longer. | **Make the queue longer** (D7): measured, it is the only one of the three that works (4 252 → 0 chunks a minute; the drain: 3 957). The carrier needs nothing here; *Lowest latency*, the browser broadcaster and every older app do. |
| OD6 | Does the relay serve parity to reliable delivery (*Smoother*, *Most stable*)? | Keep serving them none (docs/34 §3); serve them the fleet's parity like live-edge viewers. | **Serve it** (D11). On any datagram uplink — *Lowest latency*, the browser broadcaster, an older app — it lifts those viewers from 32–89 % to 95.6–100 % of frames under 0.1–1 % loss (F14), for 2.5–10.4 % more egress to them, relay-side only. It does not replace the carrier: it cannot repair an outage. |
| OD5 | Does the browser broadcaster follow? | Now; after measuring Chrome's QUIC stack; not at all. | **After a measurement**, as its own chunk outside this milestone. Chrome's controller and stream scheduling are not quinn's, nothing here measured them, and the browser broadcaster is not where the losing uplinks were found. |

## 4. How it was measured

Everything below ran on one Linux host (8 vCPU, Sandy Bridge-class Xeon)
over loopback, so every log shares one clock. Nothing is simulated except the
network path; both QUIC stacks are the ones that ship.

| Part | What it is | Faithful to |
|---|---|---|
| Sender | `gawk-broadcast-desktop/tools/uplinklab`: a frame-size trace replayed at 60 fps over the vendored `wtransport` and the workspace's pinned quinn 0.11.11 / quinn-proto 0.11.16, with the engine's one transport option (10 s keep-alive). Deltas are chunked and protected with `gawk-wire`'s own `split_frame` and P/Q parity at `k = 2`; keyframes go on their own uni stream under the F-12 rule; a 180-byte audio datagram every 20 ms. | `crates/engine` `sender.rs` / `transport.rs` |
| Receiver | `gawk-server/cmd/gawk-uplinklab recv`: a WebTransport server with the relay's `webtransport.Server` limits and `quic.Config` (quic-go 0.62.0, webtransport-go 0.13.0), logging every datagram, record and keyframe on arrival. | `internal/transport/server.go` |
| Path | `gawk-uplinklab proxy`: a UDP forwarder with a scripted path — one-way delay each way, Bernoulli or Gilbert-Elliott loss, periodic outages in both directions (dropped or held), an uplink bottleneck with a drop-tail queue, delay jitter that keeps order. Seeded, so a scenario's loss process repeats. | — |
| Frame sizes | x264 (`veryfast`, `zerolatency`, no B-frames, keyframe every 30 frames) on a 60 s Mandelbrot zoom, rate-controlled like the broadcaster: **1080p60, 9 Mbps target / 12 Mbps ceiling** (mean 9.5 Mbps; keyframes 230 KB on average, deltas 11.7 KB at p50, 20 KB at p90) and **1440p60, 37.5 / 50 Mbps** (mean 38.9 Mbps; keyframes 495 KB, deltas 61 KB at p50, 98 KB at p90). The 1080p keyframe matches the ~236 KB field measurement in docs/14. | `vt_policy.rs` / `gst_policy.rs` (75 % VBR target, GOP = fps/2) |
| Viewer | `gawk-app/lab/uplink-replay.lab.test.ts`: the measured arrival trace replayed through the **shipping** `ReorderBuffer`, `PlayoutController` and adaptive delta-gap grace under a fake clock, then a model of `PacedPresentationSink` on a 60 Hz vsync (newest due frame wins, three held at most, live edge presents on the next tick), once per viewer preset with that preset's own profile (`setViewerDeliveryMode`). Live-edge delivery is sent every frame the relay could complete with parity; reliable delivery (*Smoother*, *Most stable*) only the frames that reached the relay whole, because the relay serves it no parity. Leg B is taken as clean and instant. | `reorder-buffer.ts`, `playout.ts`, `render-sink.ts`, `resilient.ts`, `hub.NegotiateParity` |

**Delivery modes compared.** *Datagram* is today's engine. *Carrier* sends
the same datagrams, parity included, as `uint16 len ‖ datagram` records on
one uni stream per GOP; a writer task drains a queue so the frame loop never
blocks; the keyframe stream has priority 1 over the carrier's 0; nothing of
a superseded GOP survives (queued records shed, a blocked write abandoned,
D3); and a retired carrier gets 150 ms after its FIN to be fully
acknowledged before it is reset. Each mode ran with
quinn's Cubic (the default) and with quinn's BBR, and datagram mode also with
a 150 KB send buffer instead of 1 MiB.

**Paths.** RTT 10 ms (the measured Mac → relay path), 40 ms (a European
broadcaster to the Finnish reference fleet) and 100 ms (transatlantic);
random loss 0.1–5 %; Gilbert-Elliott bursts (≈ 1 % mean, bursts of ≈ 4
packets); the radio away 20 or 80 ms in every 1.07 s, both directions, either
dropping what is offered or holding it (AWDL's published shapes, docs/57 §8);
2 % loss for 10 s of every 40 s; and, at 1440p, a 300 or 80 Mbps bottleneck
with a 50 ms queue. A second pass repeats the key cells with 3 ms of delay
jitter.

**Metrics.** Per frame: when it became *complete* at the relay (all data, or
any `n` of its `n + k` symbols) and *decode-ready* (its whole GOP prefix
complete). Per viewer, after the controllers settle (the second half of each
100 s run): on-screen latency, hitches (one source frame held ≥ 100 ms),
stalled time, frames shown, and the adaptive playout offset, for all four
presets (`playbackPresets.ts`): *Lowest latency* ("live edge", `playout:
'off'`), *Balanced* (the shipping default, `adaptive`), *Smoother*
(`resilient` delivery: 0.15–2 s adaptive buffer) and *Most stable* (`deep`:
the relay's DVR ring and a 3 s buffer). For *Lowest latency*, "shown" also
falls when two frames land in one vsync (the newer wins), so its stalled time
is the loss measure. Latency here is encoder output → relay arrival (+ the
viewer's own buffering); capture, encode, leg B and decode are constants
outside it.

**Calibration.** A clean run through the proxy adds 0.4 ms at p50 and about
3 ms at p99 over a direct loopback path; Go's `time.Sleep` overshoots
~0.9 ms on short sleeps, so the proxy sleeps then spins its last
millisecond. Three runs at a time against one serial run on the clean path
moved nothing beyond one damaged datagram frame in 100 s (F7 explains it).
An early version phase-locked the outages to the GOP (period 1.000 s against
0.5 s GOPs), so every outage landed at the same point in a GOP; it was
discarded and re-run at 1.07 s.

**Contention and spread.** Every 1440p cell was re-run alone on the host:
three of them at a time, CPU contention starved the lab receiver and
inflated exactly the quic-go queue drops F7 is about (40 % undecodable for
BBR datagrams on a clean path in parallel, 23 % alone). At 1080p the
parallel and serial runs agree. A single 100 s run's viewer figures move with the loss process's seed —
up to ~5 points for *Balanced* and ~9 for *Smoother* — because a few
half-second freezes dominate the 50 s window; table M measures that spread
on four cells, and differences smaller than it are not findings.

**The lab's own defects, found and fixed.** Three sender bugs surfaced
during the pass; each was fixed and every run it could have touched was
re-run, so no table mixes versions. (1) The first carrier queued instead of
shedding (F10). (2) The second shed between writes but not during one
(F10). (3) Datagram mode ignored `TooLarge` where the engine re-chunks, so
quinn's black-hole detector (F8) silently ate every later frame; every
datagram run whose offered count exceeded what quinn sent was re-run.

**Limits.** One host and loopback, so no NIC, driver or Wi-Fi MAC behaviour:
the outage shapes are AWDL's published ones, not a capture. One receiver,
not the relay's fan-out, except in F7's run. Leg B is clean in the replay.
The browser broadcaster's QUIC stack (Chrome's) is not measured.

**Reproducing**: §11 lists the commands.

---

## 5. Decisions

### D1 — The carrier: docs/24's stream, publisher → relay (from docs/57 D1, unchanged)

A carrier is a unidirectional stream opened by the **broadcaster**, starting
with the existing prologue `0x01 0x0A` (`TypeReliableCarrier`), then records
of `uint16 len ‖ datagram`. Each record is **byte for byte the datagram that
would otherwise have been sent**: `VideoChunk` (0x01) and `ParityChunk`
(0x0E). The relay's ingest of a record is exactly its ingest of a datagram,
so everything downstream — accounting, the ingress window, fan-out to
datagram viewers, R19 carriers, R30 stripes, R29 parity prefixes, the DVR —
is untouched.

- **One carrier per GOP**, opened when a keyframe is sent. In-order delivery
  costs nothing inside a GOP: a delta that arrives before its predecessor
  cannot be decoded anyway. Per-frame streams would only add stream churn
  (60/s) and per-stream flow-control warm-up for no decode benefit.
- **Parity stays in the records.** It is useless on leg A once the stream is
  reliable, but leg B needs it, and the relay forwards what it ingests. It
  costs nothing new: the same bytes R29 already sends, and the carrier is
  within 0.1 % of datagram mode on the wire (§11 table A).
- **Datagrams still**: audio, `TimeSync`, and every delta when the carrier is
  not engaged. Keyframes keep their own stream and docs/38 F-12's rules.
- **Not a `StreamFrame` with the keyframe flag clear**: the relay caches
  whatever `ParseStreamFrameHeader` accepts as the join-priming keyframe
  (docs/57 D1's reason, still true).

### D2 — The relay: carrier ingest behind a capability bit (from docs/57 D2, extended)

- **Capability**: `RelayCapabilities` (0x0F) gains `CapUplinkCarriers =
  1 << 2`. A broadcaster opens carriers **only** after it has seen the bit.
  A relay without it rejects a publisher's `0x0A` stream as bad input
  (`ParseStreamFrameHeader` → `ErrBadType` → `countBad`, `CancelRead`;
  read in code 2026-10-17), so the gate is not optional.
- **Accept**: the publisher's uni-stream loop peeks the prologue and hands a
  `0x0A` stream to a carrier reader, outside `acceptKeyframeStreams`'
  four-slot semaphore. At most **2** open carriers per publisher (the
  current GOP and one draining); a third is `CancelRead` and counted.
- **Ingest**: each record is validated as a datagram is (type, version,
  `MaxDatagramSize`) and passed to `Publisher.HandleDatagram`. A malformed
  record ends that carrier (`CancelRead`); records before it were already
  forwarded.
- **Stale records**: a record whose `frameID` precedes (serially) the newest
  keyframe the relay has ingested is dropped and counted. Viewers jump to
  that keyframe and discard the stale delta anyway (`ReorderBuffer`:
  `hasBufferedKeyframeAbove`), so forwarding it would spend leg B on nothing.
- **Knob**: `-uplink-carriers` (default **true**), `GAWK_UPLINK_CARRIERS`,
  Helm `config.uplinkCarriers`, through `registryOptions` and asserted by
  `TestRegistryOptionsCarryAllLimits`. Off means the bit is never sent.
- **Cluster mode**: only the origin ingests publishers; edges are unchanged.

### D3 — The sender: lifecycle, and where the deadline really is (revises docs/57 D3)

docs/57 D3 reset a carrier when its *oldest unacknowledged record* was older
than 150 ms. **quinn cannot report that.** Its public `SendStream` API
(quinn 0.11.11, read from source 2026-10-17) exposes no per-offset
acknowledgement: `stopped()` resolves only after `finish()` once every byte
is acknowledged, and wtransport's `finish()` is that same wait. So the
deadlines the engine can actually enforce are at the GOP boundary, which is
also where R19's relay-side carrier puts its own (`CarrierWriteTimeout` =
one GOP):

- **Open** a carrier when a keyframe is produced; give the keyframe stream
  **priority 1** and the carrier 0 (`set_priority`), because a GOP's deltas
  are useless until its keyframe has landed.
- **Write** each delta's chunks then its parity as records, through a queue
  drained by a writer task, so the encoder never waits on the stream.
- **Nothing outlives its GOP at the sender.** When a newer keyframe has been
  produced: anything still queued for an older GOP is dropped unwritten; a
  carrier for a GOP superseded before it opened is never opened; and a write
  still **blocked** on an older GOP's carrier is abandoned and that carrier
  reset at once. The lab found both halves the hard way (F10): without the
  queue rule a starved uplink queued 16–70 s of video, and without the
  blocked-write rule one GOP's stream kept trickling for 25–36 s after it
  was superseded, because retiring and shedding only happen between writes.
  "Favour dropped frames over stalled playback" has to be enforced here;
  nothing downstream can.
- **Retire** the previous carrier at rotation: `finish()`, then reset it
  (`UPLINK_CARRIER_EXPIRED`) if it is not fully acknowledged **150 ms**
  later. Within a GOP there is no reset: a hole is either repaired by QUIC
  or overtaken by the next keyframe, which the viewer jumps to.
- **Resume**: a new publisher session starts with no carrier; the first
  keyframe after resume (the shell already forces an IDR) opens one.

### D4 — Reliable by default; Lowest latency is a choice, not a fallback (OD1, OD3; revises docs/57 D4)

- **Engaged** when the relay advertised `CapUplinkCarriers` and the
  broadcaster's **Delivery** setting is *Reliable* (the default). Otherwise
  deltas go as datagrams with parity, exactly as today.
- **The setting** is `uplinkDelivery: "reliable" | "lowest-latency"` in
  `broadcast.json` (docs/57's `auto`/`legacy` were never built, so there is
  nothing to migrate). It is reported as `uplinkModeRequested`, shown in
  Diagnostics, and takes effect **at the next keyframe while live** — a
  carrier opens at a keyframe and datagrams need no setup, so unlike a rung
  change it needs no encoder restart.
- **No automatic switching.** F3 tempts a rule ("random loss on a long path
  → datagrams"), but the side that rule would favour is the live-edge
  viewer, which is not the default, and the signal (random vs burst loss) is
  only separable after the fact. One rule, readable in telemetry, as docs/57
  OD2 argued.
- **The operator's switch** stays: `-uplink-carriers=false` never sends the
  capability, so every broadcaster on that fleet uses datagrams.
- **Not a warning dialog.** docs/57 put *Legacy* behind "makes viewers see
  pauses". The measurements make both choices legitimate; §6 describes each
  in one plain sentence instead.

### D5 — BBR, in both delivery modes, behind gates (OD2)

- The engine's QUIC connection uses quinn's `BbrConfig::default()` instead
  of the default Cubic, in both delivery modes: F4/F5 show the controller
  matters more than the mode. *Lowest latency* gains on random loss and
  gives up a few points of frames under outages for a much lower tail (F5);
  one controller for both keeps the support story to one row.
- It is one transport option (`TransportConfig::congestion_controller_factory`),
  set in `crates/engine/src/transport.rs` beside the keep-alive — the engine
  sets nothing else today.
- A `broadcast.json` field `congestion: "bbr" | "cubic"` keeps the old
  controller one edit away. It is deliberately **not** on the Quality page:
  it is a support lever, shown in Diagnostics and reported as
  `uplinkCongestion`.
- **Gates** (RU4) before BBR becomes the default: the §11 cells, a shared
  bottleneck with a competing TCP flow (not measured here — the lab's proxy
  carries one client), and the hardware pass. If a gate fails, Cubic stays
  and the carrier still ships: F2 holds under Cubic at 10 ms, where the
  R55 Mac is.

### D6 — Lowest latency stays today's datagram path

*Lowest latency* is the datagram path with parity, unchanged except for D5's
controller. Its 1 MiB send buffer stays (F11). The R55 status line keeps
QUIC packet loss as its harm signal in this mode, because here every lost
packet can be a viewer's freeze; in Reliable mode the signal becomes the
frames the uplink actually lost (RU5).

### D7 — quic-go's per-session datagram queue gets longer (OD4)

- **What**: `streamDatagramQueueLen` in quic-go's `http3` package goes from
  32 to a length that covers a scheduling gap at the highest rung's burst
  rate — 4096 in the measured build (≈ 5 MB per session at worst, and only
  a publisher's session ever fills it; viewers send almost no datagrams).
- **How**: an upstream change that makes the length configurable is the
  goal; until it lands, a one-line patch carried the way `wtransport` is
  vendored for the Rust side (a `replace` to a pinned fork, the patch
  recorded beside it). RU2 decides which once upstream has answered.
- **Where it matters**: the origin's publisher sessions and the edge's
  pull sessions, which read origin → edge datagrams through the same
  queue, at pod-to-pod speed.
- **Not a relay-side reader**: draining the queue on a dedicated goroutine
  was measured and does not work (F7) — the goroutine is starved by the same
  CPU the loop was. 32 entries are about a millisecond; the queue has to
  hold more than any goroutine can promise.
- **Counting**: quic-go drops silently, so the relay cannot count these.
  D8's playbook row (ingress chunk loss while the broadcaster reports no
  lost packets) is how they show.

### D8 — Telemetry: which uplink ran, and whose loss it was (from docs/57 D5, extended)

- **Broadcaster** (field registry, stored-shape golden): `uplinkMode`
  (`datagram` | `carrier`), `uplinkModeRequested` (the setting, D4),
  `uplinkCongestion` (the controller, D5), `uplinkCarriersOpened`,
  `uplinkCarriersExpired`, `uplinkRecordsShed` (D3), and the cumulative
  `uplinkPacketsSent` / `uplinkPacketsLost` WU3 already reports, plus
  `uplinkDatagramsDropped` — offered minus `frame_tx.datagram`, the drops
  inside quinn's own send buffer, which nothing counts today.
- **Relay** `/statusz` per broadcast and Prometheus: `uplinkCarriers`,
  `uplinkRecords`, `uplinkRecordsStale`, `uplinkCarriersRejected`.
- **Whose loss**: the relay's ingress window cannot tell a chunk the network
  lost from one quic-go dropped before the relay read it (F7). The
  broadcaster's packet counters can. A playbook row: ingress chunk loss with
  the broadcaster reporting no lost packets is the relay's own intake, not
  the uplink.
- **Prerequisite**: docs/57 WU0's frame-unit ingress loss
  (`IngressFramesDamaged`, `ingressFrameLossRatio`) moves here as RU0,
  together with the `BUGS.md` fix it implies ("Telemetry blames leg B for
  broadcaster-uplink loss").

### D9 — Not a back-channel (from docs/57 D6, unchanged)

Nothing here requests a keyframe or changes the cadence (docs/15 Decision 6
stands). The only new relay → publisher signal is one capability bit on a
message that already exists. Retransmission is QUIC's, on a stream; the
NACK/ARQ-over-datagrams rejections (docs/24, docs/26) are respected.

### D10 — Receive windows sized for a GOP at the ceiling (from docs/57 D9, measured)

Every GOP opens a new stream, and quic-go starts each at a 512 KiB receive
window (`DefaultInitialMaxStreamData`; the connection at 1.5× that), auto-
tuning only once the stream is under way. At 1080p a GOP's deltas
(~350 KB) fit; at 1440p (~1.8 MB) they do not, and on a clean path the
carrier's writes blocked 11 times in 100 s, its decode-ready p95 / p99 at
12 / 22 ms. With the windows docs/57 D9 derived — **2 MB per stream, 3 MB
per connection** at a 50 Mbps ceiling — the same run measured 9 / 15 ms and
blocked once (§11 table H). Behind an 80 Mbps bottleneck with
outages the windows stop mattering (the bottleneck dominates).

- The relay sets `InitialStreamReceiveWindow` and
  `InitialConnectionReceiveWindow` from `-uplink-max-bitrate` (default
  50 Mbps; `GAWK_UPLINK_MAX_BITRATE`, Helm `config.uplinkMaxBitrate`,
  through `registryOptions`) by docs/57 D9's rule, stream window ≥ bitrate ×
  (150 ms + RTT) × 2 and connection window ≈ 1.5 × that.
- The maxima stay quic-go's (6 / 15 MiB): a window is an allowance, not an
  allocation, so viewer connections that share the config pay nothing.

### D11 — Reliable delivery gets parity too (OD6; revises docs/34 §3 and §5.1)

docs/34 §3 called parity "redundant" for carrier delivery because R19
already retransmits anything lost **on leg B**. Leg A is the other half: a
chunk lost before the relay is lost for every viewer, and only parity (or
the carrier uplink) brings it back.

- `NegotiateParity` stops clamping reliable and DVR delivery to 0: they get
  the same prefix as a live-edge viewer, `?parity=` still opts down, and the
  fleet default still caps it.
- `drainReliable` already writes whatever is enqueued as carrier records,
  so parity rides the carrier with no new path; the DVR ring retains
  `ParityChunk` records beside `VideoChunk` ones (`fanOutLocked` keeps only
  video chunks today).
- The viewer needs nothing: a carrier record enters the same handler as a
  datagram, and the reassembler already repairs from `ParityChunk` records.
- The loss allowance stays 0 in those modes (unmeasured here; it trades a
  freeze for artifacts, which is the opposite of what those presets
  promise).
- Cost: parity's share of video egress to those viewers, 10.4 % at 1080p
  and 2.5 % at 1440p; the `egress_parity_bytes_total` series already
  measures it.

## 6. What users see

**The broadcaster** (desktop app, Quality page — docs/64 settled that the
delivery switch lands there, beside Resolution, Frame rate and Upload cap):

| Element | Copy |
|---|---|
| Label | **Delivery** |
| Segmented control | **Reliable** · **Lowest latency** |
| Hint under *Reliable* | "If your connection drops some video, viewers wait a moment for it instead of losing the picture." |
| Hint under *Lowest latency* | "Viewers never wait for dropped video. If your connection drops some, they see brief freezes." |

- No dialog, no warning icon, no restart: the change applies at the next
  keyframe, live or not.
- Every string passes docs/57 D7's principle-3 test (no packet, uplink,
  QUIC, carrier, DSCP or band).
- **Diagnostics** (the technical truth, exempt from principle 3): delivery
  mode requested and in use, congestion controller, carriers opened /
  expired, records shed, packets sent / lost, datagrams dropped in the send
  buffer.
- **The R55 status line** ("Your Wi-Fi is dropping some video…") keeps its
  copy and rules; RU5 changes only what counts as harm in Reliable mode
  (D6).
- **iOS**: no setting (OD3); the app runs the engine default.

**Viewers** choose nothing new. With OD1, OD2 and OD6 as recommended, by
preset (§11 tables I, J, K):

- **On a clean path** nothing changes for anyone (F1).
- **Lowest latency**: a burst or a radio outage on the broadcaster's side
  becomes mostly a hitch under ~150 ms instead of a 250–500 ms freeze; on a
  40 ms path with bursty loss the hitches are also more frequent. On a long
  path with steady random loss it waits briefly more often than before (F3)
  — a broadcaster for whom that matters picks *Lowest latency* delivery
  too.
- **Balanced** (the default): the picture keeps moving through bursts and
  outages; its latency rises by up to ~60 ms at 10–40 ms RTT (up to ~120 ms
  at 100 ms) and settles back over about a minute (F2, F9).
- **Smoother and Most stable** gain the most: today a broadcaster's 0.1 %
  random loss costs them about one frame in eight and ~15 freezes a
  minute, and an outage as much as it costs anyone. On the carrier uplink
  they see 94–100 % of frames everywhere measured, and their buffers absorb
  the repair delay. If the broadcaster picks *Lowest latency*, OD6's parity
  still protects them from random loss, not from outages (F14).

## 7. Architecture

```
 broadcaster (Rust engine)                                 relay (origin)
 ─────────────────────────                                 ──────────────
 keyframe ─► StreamFrame uni stream, priority 1 (R8, F-12) ─► acceptKeyframeStreams ─► onKeyframe
 deltas   ─► writer queue ─► carrier uni stream per GOP   ─► carrier reader (new, ≤ 2 open)
              │ sheds a GOP once a newer keyframe exists       │ drops records behind the newest keyframe
              │ retires the old carrier: FIN, reset +150 ms    └─ per record ─► Publisher.HandleDatagram
 audio    ─► datagram (unchanged) ───────────────────────────► datagram loop ─► HandleDatagram
 congestion controller: D5          receive windows: D10       intake: D7
 (lowest-latency mode, or a relay without CapUplinkCarriers: deltas as datagrams, as today)
```

## 8. Chunks and acceptance criteria

### Milestone goals

Pre-registered, rewritten from docs/57 G1–G7 and G10–G13 against §2.
"Lab" means the §4 harness on the §11 cells; "hardware" means RU7.

| # | Goal | Verified by |
|---|---|---|
| G1 | **No cost on a clean path**: Reliable vs Lowest latency differ by ≤ 2 ms in decode-ready p50/p95/p99 and ≤ 1 % in bytes on the wire, at 10/40/100 ms | lab (F1's cells) + hardware (wired) |
| G2 | **Bursts and outages stop breaking GOPs**: on the GE and outage cells at 10 and 40 ms, Reliable leaves ≤ 0.5 % of frames undecodable and the Balanced viewer shows ≥ 97 % of frames (with OD2's controller; at 10 ms with either) | lab |
| G3 | **The latency it costs is bounded and visible**: on the same cells, the Balanced offset rises by ≤ 60 ms at 10–40 ms RTT; Diagnostics shows the mode and the counters that explain it | lab + unit |
| G4 | **The R55 Mac is fixed**: the reference Mac on Wi-Fi with AWDL on, 10 min at the default rung: leg-A damaged frames (`ingressFrameLossRatio`) ≤ 0.1 %, viewer gap resyncs ≤ 2/min, no `keyframe-only-delivery` | hardware |
| G5 | **The controller is not a regression** (if OD2 is taken): on the carrier, BBR is no worse than Cubic on any §11 cell beyond the run-to-run spread (Balanced frames shown − 3 points, table M; decode-ready p99 + 5 ms); on datagrams with D7's queue, the same on clean paths, random loss and bursts, and within 6 points of frames shown on outages at a lower p99 (F5, F15); and it passes RU4's shared-bottleneck gate | lab |
| G6 | **Compatibility**: a new broadcaster on an old relay sends datagrams unchanged; an old broadcaster on a new relay is unchanged; `-uplink-carriers=false` leaves `/statusz`, metrics and wire byte-identical to before | integration (real `gawk-server`) + diff assertion |
| G7 | **Wire parity**: `CapUplinkCarriers` in `gawk-server/wire`, `parity.ts` and `crates/wire`, golden vectors byte-identical in all three | unit (mirror tests) |
| G8 | **Viewers untouched**: no `gawk-app` change beyond the capability constant | review |
| G9 | **Lowest latency is today's path**: with that setting the engine's wire output is byte-identical to today's for the same frames (D6) | unit |
| G10 | **Stable presets are protected**: on the carrier uplink, *Smoother* and *Most stable* show ≥ 98 % of frames on every §11 path at 10–40 ms; on a datagram uplink with OD6's parity, ≥ 99 % at 0.1–1 % random loss and 10–40 ms (F14) | lab (replay) |

### RU0 — Telemetry that can see leg A (docs/57 WU0, moved)

| Acceptance criterion | Verified by |
|---|---|
| Leg-A loss in **frames**, partly received ones counted: `IngressFramesDamaged`, `ingressFrameLossRatio`, `ingressChunkLossRatio`, exactly as docs/57 WU0 specified | unit (red first) |
| `BUGS.md` "Telemetry blames leg B for broadcaster-uplink loss" fixed as its entry prescribes, and the entry deleted | unit (red first, `rules`) |
| The broadcaster reports `uplinkDatagramsDropped` (datagrams offered minus quinn's `frame_tx.datagram`) and the other D8 fields; field registry and stored-shape golden updated | unit |
| A playbook row: relay ingress chunk loss while the broadcaster's QUIC reports no lost packets names the relay's intake, not the uplink (F7) | unit (`rules`) |

### RU1 — Relay: carrier ingest (docs/57 WU1, moved; D2, D10)

| Acceptance criterion | Verified by |
|---|---|
| `CapUplinkCarriers = 1 << 2` allocated beside `CapStripedDelivery` and mirrored (G7) | unit |
| Records are ingested exactly as the same bytes sent as datagrams: same fan-out, accounting and DVR contents (property test over random frames) | unit, test-first |
| Without the capability a publisher `0x0A` stream is rejected as today; `-uplink-carriers=false` is byte-identical (G6) | unit (diff) |
| A third concurrent carrier is rejected and counted; a malformed record ends its carrier with earlier records forwarded; a record behind the newest ingested keyframe is dropped and counted | unit |
| `-uplink-max-bitrate` derives both initial receive windows per D10 (≈ 2 MB / 3 MB at the default); a full 1440p GOP passes one carrier on loopback with no write blocked by flow control from the first GOP of a session | unit (real quic-go loopback) |
| A hostile publisher holding a hole keeps at most its connection receive window of relay memory — measured and recorded | unit |
| `-uplink-carriers` / `GAWK_UPLINK_CARRIERS` / `config.uplinkCarriers` through `registryOptions`; `TestRegistryOptionsCarryAllLimits` grows; README flags table updated | unit + review |

### RU2 — Relay: a datagram queue that does not drop bursts (D7, OD4)

| Acceptance criterion | Verified by |
|---|---|
| A test publisher sends a burst of 200 datagrams at loopback speed while the relay's datagram loop is blocked for 20 ms; all 200 reach the hub. Red first against stock quic-go (it keeps 32) | integration (real quic-go), red first |
| The same for the edge's pull session | integration |
| F7's relay run (1440p, 15 viewers, 300 Mbps, pinned CPUs) shows 0 ingress chunks lost, recorded in §11 | lab |
| The quic-go change is upstream, or carried as a pinned fork with the patch recorded beside the `replace`, and `go mod tidy` stays clean in every module that reaches `gawk-server` | review + CI |

### RU3 — Engine: the carrier sender (docs/57 WU2, moved; D1, D3, D4)

| Acceptance criterion | Verified by |
|---|---|
| A carrier opens per keyframe and its records are byte-identical to the datagrams datagram mode sends for the same frame | unit, test-first |
| Keyframe stream priority 1, carrier 0; F-12's 2 s rule unchanged | unit |
| **Shedding**: with a fake transport whose writes block, records for a superseded GOP are dropped unwritten, a carrier superseded before it opened is never opened, a write blocked on a superseded GOP's carrier is abandoned and the carrier reset when the newer keyframe is produced, and the encoder never blocks | unit (fake clock) |
| Retiring: FIN at rotation; reset with `UPLINK_CARRIER_EXPIRED` if not fully acknowledged 150 ms later; counted | unit |
| Engage policy (D4): capability ∧ Reliable ⇒ carrier, else datagrams; a change applies at the next keyframe | unit |
| Runs on a **current-thread** tokio runtime, as the iOS engine does (docs/67) | unit |
| Against a real `gawk-server` behind a burst-dropping forwarder (`tests/support/udp_gate.rs` extended): a burst that breaks a GOP in datagram mode loses no frame on the carrier | integration (the relay suite CI already runs with `--ignored`) |
| A commit touching `gawk-ios/` in the same PR (CONTRIBUTING: engine semantics) | review |

### RU4 — Engine: the congestion controller (D5, OD2)

| Acceptance criterion | Verified by |
|---|---|
| `congestion: "bbr" \| "cubic"` in `broadcast.json`, reported as `uplinkCongestion`, shown in Diagnostics; `cubic` restores today's transport exactly | unit |
| G5's lab gate, recorded in §11 (this pass meets it) | lab |
| Shared bottleneck: through an 80 Mbps bottleneck with a 50 ms queue and one competing TCP flow, the broadcast holds its rate and the TCP flow keeps ≥ 50 % of its fair share; recorded | lab (the proxy grows a second client) |
| The hardware pass (RU7) shows no regression against Cubic on either machine | hardware |
| Default flips to `bbr` only when all three pass; otherwise this chunk records why and Cubic stays | review |

### RU5 — What users see (§6)

| Acceptance criterion | Verified by |
|---|---|
| The Quality page's Delivery control with §6's exact copy; persists in `broadcast.json`; applies at the next keyframe without a restart | unit + manual |
| No principle-3 banned term in a main-flow string (the existing string test) | unit |
| Diagnostics rows per §6 | unit |
| `lossnotice`'s harm input per mode: Reliable → expired carriers and shed records; Lowest latency → QUIC packet loss as today (table tests) | unit |

### RU6 — The lab

| Acceptance criterion | Verified by |
|---|---|
| `gawk-server/cmd/gawk-uplinklab`, `gawk-broadcast-desktop/tools/uplinklab`, `gawk-app/lab/uplink-replay.lab.test.ts` and `tools/uplinklab/` reproduce §11's tables within noise from §11's commands | manual, recorded |
| None of them enters a shipped binary, image or bundle: the Rust sender is its own workspace (its lockfile checked against the desktop workspace's quinn and wtransport by `build.sh`), the replay sits outside `tsc -b`'s `src` and is skipped unless its environment is set | review + CI |

### RU8 — Relay: parity for reliable delivery (D11, OD6)

| Acceptance criterion | Verified by |
|---|---|
| `NegotiateParity` serves reliable and DVR delivery the same prefix as live-edge delivery; `?parity=` still opts down; red first against today's clamp | unit, red first |
| A reliable subscriber's carrier carries the broadcast's `ParityChunk` records, and a DVR subscriber's ring replay does too (the ring retains them; its byte bound counts them) | unit |
| With a chunk withheld on ingest, a reliable viewer's reassembler (in a real worker, the e2e lossy-link pass) recovers the frame from the carrier's parity record | e2e |
| `egress_parity_bytes_total` counts the new bytes; G10's datagram half holds in the replay | unit + lab |
| `-parity-default 0` still turns parity off for everyone, byte-identically | unit (diff) |

### RU7 — The hardware pass

| Acceptance criterion | Verified by |
|---|---|
| G4 on the reference Mac; G1 on the gaming PC wired; one session over a long real path (≥ 40 ms to the fleet) in both delivery modes | manual |
| §11 records each against the RU0 baseline | review |

## 9. Non-goals

- **A viewer-side change.** Leg B has R19, R29 and R30; nothing in gawk-app
  changes beyond the `wire.ts`/`parity.ts` capability constant. (F9's
  finding about the adaptive offset's slow descent is recorded, not acted
  on here.)
- **Pacing or frame-size capping** on leg A — rejected for leg B by owner
  decision (docs/34 Finding 4, docs/35) because they add latency or cost
  quality; the congestion controller stays the only pacer.
- **Relay-side reconstruction** of parity on ingest (docs/34): the relay
  stays a byte forwarder.
- **A keyframe request of any kind** (docs/15 Decision 6, D9).
- **A home-grown congestion controller.** D5 chooses between the
  controllers quinn ships; writing a media controller (GCC/SCReAM-style) is
  a milestone of its own if D5's gates fail.
- **Changing the rung on loss.** No auto-ladder for the native apps (docs/38
  D11); this fixes delivery, not bitrate.
- **The browser broadcaster.** Chrome's QUIC stack and its controller were
  not measured; OD5 decides whether it follows.

## 10. Risks

- **quinn's BBR is labelled "Experimental! Use at your own risk."**
  (`quinn-proto` 0.11.16, `congestion/bbr`). It is a port of quiche's BBRv1.
  What the lab saw: its window grows without bound on an app-limited flow
  (tens of MB after a minute, harmless while pacing holds the rate, but not
  a bound on anything); one throughput dip in a clean 100 s run that a
  150 KB datagram buffer turned into four lost frames; and behind an 80 Mbps
  bottleneck it overran the 50 ms queue more often than Cubic (175 against
  66 drops in 100 s on the carrier, and 4 400 against none after outages,
  where it re-sends what the outage lost without backing off) — harmless to
  the broadcast (§11 table E), not obviously to anything sharing that queue.
  BBRv1 is also known to be unfair to Cubic flows on a shared bottleneck —
  a broadcaster's own upload competing with the household's. D5's gates are what stand between this and
  production; a revert is one transport option.
- **quinn's MTU black-hole detector misreads random loss** (`mtud.rs`,
  `BLACK_HOLE_THRESHOLD = 3`): more than three loss bursts of
  full-size packets with no larger packet acknowledged since reset the path
  MTU to 1200, and a 1200-byte chunk no longer fits. The engine's existing
  re-chunk on `TooLarge` (`sender.rs`) handles it — the lab's first sender
  did not, and its datagram results were re-run (§4). The shrunk budget
  sticks for the session, so a lossy start costs ~3 % more datagrams for the
  rest of it. Carrier records are unaffected (stream frames split to fit).
- **The adaptive viewer remembers a bad minute.** The carrier turns loss
  into delay, and the Balanced viewer's offset follows its 60 s p95 window,
  then descends at 5 ms/s after a 15 s dwell (F9). A short loss episode can
  hold its latency up for over a minute after the link is clean. It is
  R12's controller doing what it was tuned to; changing it is a viewer
  milestone, not this one.
- **High rungs on thin links.** No delivery mode creates bandwidth: with
  too little headroom the carrier sheds GOPs as datagram mode drops
  datagrams (§11 table E). The upload-cap warning (docs/38) is the remedy.
- **Relay memory**: a publisher can hold data behind a deliberate hole up
  to its connection receive window (15 MiB, quic-go's maximum). That is the
  same ceiling every stream already has; RU1 measures it with a
  hostile-publisher test.
- **Stream limits**: a publisher holds at most two carriers and one or two
  keyframe streams; quic-go's default 100 incoming uni streams leaves room,
  and stream IDs are not reused, so ~4 streams/s for a 24 h broadcast
  (~350 k) is inside QUIC's 2⁶⁰.

## 11. Measurements

Every cell is one 100 s run (§4); transport figures cover seconds 5–99,
viewer figures the second half, after the adaptive controller settled.
"Undecodable" counts frames a viewer could not decode for want of data —
lost, or after a hole in their GOP. Tables are generated from the runs by
`tools/uplinklab/tables.py`; nothing below is transcribed by hand.

**Reproducing** (8 vCPU; about 4 h — 1080p cells three at a time, 1440p
cells alone, see §4):

```sh
tools/uplinklab/build.sh                      # the Go lab, relay and loadgen, their D7 variants; the Rust sender
tools/uplinklab/traces.sh                     # ffmpeg + libx264, ~15 min
for set in core cc jit win seeds; do python3 tools/uplinklab/matrix.py $set --jobs 3; done
python3 tools/uplinklab/matrix.py bigq --jobs 3 --only rtt
python3 tools/uplinklab/matrix.py hi --jobs 1
python3 tools/uplinklab/matrix.py bigq --jobs 1 --only hi-
python3 tools/uplinklab/tables.py             # the tables below
# F7, against the real relay (§11 table G); RELAY_BIN=tools/uplinklab/out/bin/gawk-server-bigq for D7:
RELAY_CPUS=0-2 VIEWER_CPUS=3-4 SENDER_CPUS=5-6 PROXY_ARGS="-rtt 10ms -rate 300e6 -queue 50ms" \
  tools/uplinklab/relay.sh out/relay-1440 tools/uplinklab/out/traces/r1440p60.csv 15 60
```

#### Table A — the clean path (F1)

| RTT | Controller | Delivery | Decode-ready p50 / p95 / p99 ms | Mbps on the wire | Packets/s |
|---|---|---|---|---|---|
| 10 ms | Cubic | datagram | 6 / 9 / 16 | 10.84 | 1125 |
| 10 ms | Cubic | carrier | 6 / 7 / 14 | 10.85 | 1017 |
| 10 ms | BBR | datagram | 6 / 7 / 11 | 10.84 | 1125 |
| 10 ms | BBR | carrier | 6 / 7 / 10 | 10.85 | 1018 |
| 40 ms | Cubic | datagram | 21 / 22 / 30 | 10.85 | 1126 |
| 40 ms | Cubic | carrier | 21 / 22 / 30 | 10.85 | 1018 |
| 40 ms | BBR | datagram | 21 / 22 / 25 | 10.85 | 1127 |
| 40 ms | BBR | carrier | 21 / 22 / 25 | 10.85 | 1021 |
| 100 ms | Cubic | datagram | 51 / 52 / 61 | 10.85 | 1126 |
| 100 ms | Cubic | carrier | 51 / 52 / 62 | 10.85 | 1017 |
| 100 ms | BBR | datagram | 51 / 52 / 57 | 10.85 | 1127 |
| 100 ms | BBR | carrier | 51 / 52 / 56 | 10.85 | 1021 |

#### Table B — bursts and outages, BBR (F2)

| RTT | Path | Delivery | Undecodable | GOPs broken | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |
|---|---|---|---|---|---|---|---|
| 10 ms | bursts (GE, ≈1 %) | datagram | 18.9 % | 36 % | 6 / 7 / 18 | 50 ms · 92.4 % · 9.1 % | 10.7 % |
| 10 ms | bursts (GE, ≈1 %) | carrier | 0.0 % | 0 % | 6 / 11 / 19 | 50 ms · 100.0 % · 0.0 % | 1.2 % |
| 10 ms | radio away 20 ms/1.07 s | datagram | 22.7 % | 45 % | 6 / 7 / 11 | 50 ms · 91.6 % · 9.8 % | 12.1 % |
| 10 ms | radio away 20 ms/1.07 s | carrier | 0.0 % | 0 % | 6 / 10 / 33 | 50 ms · 99.8 % · 0.0 % | 4.1 % |
| 10 ms | radio away 80 ms/1.07 s | datagram | 27.6 % | 51 % | 6 / 8 / 14 | 50 ms · 73.5 % · 27.9 % | 28.4 % |
| 10 ms | radio away 80 ms/1.07 s | carrier | 0.0 % | 0 % | 6 / 51 / 97 | 78 ms · 97.2 % · 3.2 % | 10.2 % |
| 40 ms | bursts (GE, ≈1 %) | datagram | 18.8 % | 36 % | 21 / 30 / 65 | 50 ms · 93.4 % · 7.8 % | 12.9 % |
| 40 ms | bursts (GE, ≈1 %) | carrier | 0.0 % | 0 % | 21 / 52 / 70 | 74 ms · 98.5 % · 1.4 % | 13.1 % |
| 40 ms | radio away 20 ms/1.07 s | datagram | 23.0 % | 46 % | 21 / 22 / 26 | 50 ms · 92.4 % · 8.9 % | 11.3 % |
| 40 ms | radio away 20 ms/1.07 s | carrier | 0.0 % | 0 % | 21 / 31 / 78 | 50 ms · 98.0 % · 1.1 % | 6.5 % |
| 40 ms | radio away 80 ms/1.07 s | datagram | 28.0 % | 51 % | 21 / 23 / 33 | 50 ms · 73.4 % · 28.1 % | 28.6 % |
| 40 ms | radio away 80 ms/1.07 s | carrier | 0.0 % | 0 % | 21 / 96 / 143 | 107 ms · 97.6 % · 1.9 % | 12.3 % |
| 100 ms | bursts (GE, ≈1 %) | datagram | 19.6 % | 37 % | 51 / 123 / 203 | 72 ms · 79.9 % · 18.8 % | 25.3 % |
| 100 ms | bursts (GE, ≈1 %) | carrier | 0.2 % | 1 % | 51 / 151 / 188 | 140 ms · 98.2 % · 2.3 % | 25.7 % |
| 100 ms | radio away 20 ms/1.07 s | datagram | 23.5 % | 45 % | 51 / 54 / 115 | 75 ms · 85.9 % · 15.4 % | 19.0 % |
| 100 ms | radio away 20 ms/1.07 s | carrier | 0.0 % | 0 % | 51 / 135 / 172 | 130 ms · 98.3 % · 1.6 % | 13.0 % |
| 100 ms | radio away 80 ms/1.07 s | datagram | 28.8 % | 51 % | 51 / 54 / 121 | 50 ms · 70.7 % · 31.0 % | 31.8 % |
| 100 ms | radio away 80 ms/1.07 s | carrier | 0.3 % | 3 % | 51 / 193 / 234 | 171 ms · 93.5 % · 4.9 % | 19.0 % |

#### Table C — random loss, BBR (F3)

| RTT | Loss | Delivery | Undecodable | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |
|---|---|---|---|---|---|---|
| 10 ms | 0.1 % | datagram | 0.0 % | 6 / 7 / 14 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | 0.1 % | carrier | 0.0 % | 6 / 7 / 16 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | 0.5 % | datagram | 0.0 % | 6 / 7 / 18 | 50 ms · 100.0 % · 0.0 % | 0.1 % |
| 10 ms | 0.5 % | carrier | 0.0 % | 6 / 16 / 19 | 50 ms · 100.0 % · 0.0 % | 0.2 % |
| 10 ms | 1 % | datagram | 1.7 % | 6 / 16 / 23 | 50 ms · 100.0 % · 0.0 % | 0.4 % |
| 10 ms | 1 % | carrier | 0.0 % | 6 / 17 / 20 | 50 ms · 100.0 % · 0.0 % | 0.6 % |
| 10 ms | 2 % | datagram | 4.1 % | 6 / 9 / 27 | 50 ms · 99.1 % · 1.3 % | 2.9 % |
| 10 ms | 2 % | carrier | 0.0 % | 6 / 18 / 29 | 50 ms · 99.9 % · 0.0 % | 1.5 % |
| 40 ms | 0.1 % | datagram | 0.0 % | 21 / 22 / 29 | 50 ms · 99.9 % · 0.0 % | 0.6 % |
| 40 ms | 0.1 % | carrier | 0.0 % | 21 / 26 / 61 | 50 ms · 99.5 % · 0.4 % | 4.5 % |
| 40 ms | 0.5 % | datagram | 0.1 % | 21 / 27 / 63 | 50 ms · 99.5 % · 0.2 % | 5.5 % |
| 40 ms | 0.5 % | carrier | 0.0 % | 21 / 61 / 68 | 75 ms · 99.5 % · 0.4 % | 20.8 % |
| 40 ms | 1 % | datagram | 0.9 % | 21 / 47 / 66 | 50 ms · 95.8 % · 3.5 % | 12.0 % |
| 40 ms | 1 % | carrier | 0.0 % | 21 / 62 / 78 | 75 ms · 99.2 % · 0.4 % | 29.7 % |
| 40 ms | 2 % | datagram | 3.4 % | 21 / 67 / 115 | 66 ms · 85.7 % · 15.0 % | 25.7 % |
| 40 ms | 2 % | carrier | 0.0 % | 30 / 79 / 124 | 100 ms · 97.0 % · 2.6 % | 51.1 % |
| 100 ms | 0.1 % | datagram | 0.0 % | 51 / 54 / 120 | 50 ms · 90.7 % · 9.9 % | 10.6 % |
| 100 ms | 0.1 % | carrier | 0.0 % | 51 / 106 / 152 | 106 ms · 97.0 % · 2.5 % | 10.0 % |
| 100 ms | 0.5 % | datagram | 0.4 % | 51 / 152 / 236 | 168 ms · 93.8 % · 4.9 % | 18.4 % |
| 100 ms | 0.5 % | carrier | 0.2 % | 51 / 154 / 220 | 136 ms · 95.8 % · 5.0 % | 40.1 % |
| 100 ms | 1 % | datagram | 0.4 % | 51 / 187 / 273 | 276 ms · 94.4 % · 5.9 % | 28.6 % |
| 100 ms | 1 % | carrier | 0.9 % | 88 / 202 / 270 | 207 ms · 95.8 % · 4.0 % | 58.7 % |
| 100 ms | 2 % | datagram | 8.2 % | 52 / 253 / 350 | 346 ms · 95.4 % · 4.0 % | 44.3 % |
| 100 ms | 2 % | carrier | 2.0 % | 134 / 245 / 301 | 231 ms · 97.0 % · 3.5 % | 76.1 % |

#### Table D — Cubic against BBR under random loss (F4, F5)

| RTT | Loss | Delivery | Cubic: Mbps · undecodable · p99 ms | BBR: Mbps · undecodable · p99 ms |
|---|---|---|---|---|
| 10 ms | 0.1 % | datagram | 10.8 · 0.0 % · 41 | 10.8 · 0.0 % · 14 |
| 10 ms | 0.1 % | carrier | 10.9 · 0.0 % · 36 | 10.9 · 0.0 % · 16 |
| 10 ms | 0.5 % | datagram | 10.9 · 0.0 % · 454 | 10.9 · 0.0 % · 18 |
| 10 ms | 0.5 % | carrier | 10.9 · 0.0 % · 168 | 10.9 · 0.0 % · 19 |
| 10 ms | 1 % | datagram | 10.9 · 0.7 % · 671 | 10.9 · 1.7 % · 23 |
| 10 ms | 1 % | carrier | 10.7 · 3.5 % · 273 | 11.0 · 0.0 % · 20 |
| 10 ms | 2 % | datagram | 9.5 · 37.6 % · 1685 | 10.9 · 4.1 % · 27 |
| 10 ms | 2 % | carrier | 9.2 · 23.6 % · 390 | 11.1 · 0.0 % · 29 |
| 40 ms | 0.1 % | datagram | 9.9 · 23.1 % · 2054 | 10.8 · 0.0 % · 29 |
| 40 ms | 0.1 % | carrier | 9.6 · 16.4 % · 431 | 10.9 · 0.0 % · 61 |
| 40 ms | 0.5 % | datagram | 5.2 · 95.3 % · 2224 | 10.9 · 0.1 % · 63 |
| 40 ms | 0.5 % | carrier | 5.2 · 78.0 % · 1069 | 10.9 · 0.0 % · 68 |
| 40 ms | 1 % | datagram | 3.8 · 99.7 % · 2367 | 10.9 · 0.9 % · 66 |
| 40 ms | 1 % | carrier | 3.8 · 88.5 % · 1445 | 11.0 · 0.0 % · 78 |
| 40 ms | 2 % | datagram | 2.8 · 99.9 % · 2195 | 10.9 · 3.4 % · 115 |
| 40 ms | 2 % | carrier | 2.8 · 93.5 % · 1953 | 11.1 · 0.0 % · 124 |
| 100 ms | 0.1 % | datagram | 5.6 · 89.1 % · 2093 | 10.8 · 0.0 % · 120 |
| 100 ms | 0.1 % | carrier | 5.2 · 72.9 % · 1150 | 10.9 · 0.0 % · 152 |
| 100 ms | 0.5 % | datagram | 2.6 · 97.4 % · 1888 | 10.9 · 0.4 % · 236 |
| 100 ms | 0.5 % | carrier | 2.6 · 92.2 % · 2231 | 10.9 · 0.2 % · 220 |
| 100 ms | 1 % | datagram | 1.9 · 100.0 % · 1957 | 10.9 · 0.4 % · 273 |
| 100 ms | 1 % | carrier | 1.9 · 97.0 % · 2448 | 11.0 · 0.9 % · 270 |
| 100 ms | 2 % | datagram | 1.5 · 100.0 % · – | 10.9 · 8.2 % · 350 |
| 100 ms | 2 % | carrier | 1.4 · 99.0 % · 2211 | 11.1 · 2.0 % · 301 |

#### Table E — 1440p60 (38.9 Mbps mean, 50 Mbps ceiling), 10 ms RTT

| Path | Controller | Delivery | Mbps | Undecodable | Decode-ready p50 / p95 / p99 ms | Shed / expired | Balanced: offset · shown · stalled |
|---|---|---|---|---|---|---|---|
| clean | Cubic | datagram | 41.4 | 12.6 % | 7 / 11 / 22 | – | 50 ms · 89.5 % · 11.8 % |
| clean | Cubic | carrier | 41.4 | 0.0 % | 7 / 12 / 24 | 0 / 0 | 50 ms · 100.0 % · 0.0 % |
| clean | BBR | datagram | 41.4 | 23.2 % | 7 / 11 / 23 | – | 50 ms · 81.8 % · 20.1 % |
| clean | BBR | carrier | 41.4 | 0.0 % | 7 / 12 / 22 | 0 / 0 | 50 ms · 99.8 % · 0.2 % |
| 0.1 % loss | Cubic | datagram | 38.0 | 29.2 % | 119 / 917 / 1381 | – | 350 ms · 49.6 % · 52.1 % |
| 0.1 % loss | Cubic | carrier | 37.3 | 11.0 % | 40 / 188 / 240 | 229 / 42 | 218 ms · 86.8 % · 13.8 % |
| 0.1 % loss | BBR | datagram | 41.4 | 6.2 % | 7 / 17 / 22 | – | 50 ms · 97.7 % · 2.7 % |
| 0.1 % loss | BBR | carrier | 41.4 | 0.0 % | 7 / 17 / 22 | 0 / 0 | 50 ms · 100.0 % · 0.0 % |
| bursts (GE) | Cubic | datagram | 26.9 | 90.5 % | 932 / 1668 / 2383 | – | 350 ms · 2.1 % · 99.8 % |
| bursts (GE) | Cubic | carrier | 23.4 | 53.7 % | 187 / 362 / 409 | 1763 / 15 | 350 ms · 41.5 % · 61.2 % |
| bursts (GE) | BBR | datagram | 41.5 | 58.4 % | 7 / 21 / 35 | – | 50 ms · 57.7 % · 46.6 % |
| bursts (GE) | BBR | carrier | 41.7 | 0.0 % | 7 / 18 / 32 | 0 / 0 | 50 ms · 100.0 % · 0.0 % |
| 300 Mbps bottleneck | Cubic | datagram | 41.4 | 8.6 % | 8 / 11 / 26 | – | 50 ms · 96.8 % · 3.6 % |
| 300 Mbps bottleneck | Cubic | carrier | 41.4 | 0.0 % | 8 / 10 / 24 | 0 / 0 | 50 ms · 100.0 % · 0.0 % |
| 300 Mbps bottleneck | BBR | datagram | 41.4 | 6.7 % | 8 / 11 / 26 | – | 50 ms · 98.1 % · 2.5 % |
| 300 Mbps bottleneck | BBR | carrier | 41.4 | 0.0 % | 8 / 10 / 24 | 0 / 0 | 50 ms · 100.0 % · 0.0 % |
| 300 Mbps + outages | Cubic | datagram | 41.5 | 22.2 % | 10 / 78 / 199 | – | 120 ms · 75.5 % · 26.1 % |
| 300 Mbps + outages | Cubic | carrier | 41.9 | 2.0 % | 10 / 110 / 167 | 29 / 20 | 138 ms · 95.8 % · 4.9 % |
| 300 Mbps + outages | BBR | datagram | 41.8 | 28.5 % | 8 / 12 / 26 | – | 50 ms · 72.7 % · 28.7 % |
| 300 Mbps + outages | BBR | carrier | 44.0 | 0.2 % | 8 / 59 / 101 | 0 / 0 | 85 ms · 95.1 % · 3.5 % |
| 80 Mbps bottleneck | Cubic | datagram | 41.4 | 3.0 % | 13 / 46 / 82 | – | 74 ms · 96.5 % · 3.3 % |
| 80 Mbps bottleneck | Cubic | carrier | 41.4 | 0.0 % | 13 / 45 / 84 | 0 / 0 | 73 ms · 97.5 % · 2.1 % |
| 80 Mbps bottleneck | BBR | datagram | 41.4 | 2.6 % | 13 / 46 / 86 | – | 73 ms · 97.0 % · 2.7 % |
| 80 Mbps bottleneck | BBR | carrier | 41.4 | 0.0 % | 13 / 45 / 85 | 0 / 0 | 75 ms · 97.3 % · 2.2 % |
| 80 Mbps + outages | Cubic | datagram | 41.4 | 21.7 % | 13 / 163 / 301 | – | 246 ms · 75.7 % · 25.4 % |
| 80 Mbps + outages | Cubic | carrier | 41.4 | 2.4 % | 14 / 136 / 183 | 28 / 25 | 173 ms · 95.9 % · 4.7 % |
| 80 Mbps + outages | BBR | datagram | 41.9 | 28.2 % | 13 / 56 / 99 | – | 71 ms · 70.5 % · 30.9 % |
| 80 Mbps + outages | BBR | carrier | 44.7 | 0.1 % | 14 / 95 / 143 | 0 / 11 | 121 ms · 98.4 % · 1.6 % |

#### Table F — with 3 ms of path jitter (robustness)

| RTT | Path | Controller | Delivery | Undecodable | Decode-ready p99 ms | Balanced: offset · shown · stalled | Live edge: stalled |
|---|---|---|---|---|---|---|---|
| 10 ms | clean | Cubic | datagram | 0.0 % | 19 | 50 ms · 100.0 % · 0.0 % | 0.1 % |
| 10 ms | clean | Cubic | carrier | 0.0 % | 16 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | clean | BBR | datagram | 0.0 % | 14 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | clean | BBR | carrier | 0.0 % | 13 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | loss0.001 | Cubic | datagram | 0.0 % | 72 | 50 ms · 92.3 % · 7.6 % | 13.8 % |
| 10 ms | loss0.001 | Cubic | carrier | 0.0 % | 64 | 52 ms · 97.8 % · 2.3 % | 8.6 % |
| 10 ms | loss0.001 | BBR | datagram | 0.0 % | 21 | 50 ms · 100.0 % · 0.0 % | 0.3 % |
| 10 ms | loss0.001 | BBR | carrier | 0.0 % | 23 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 10 ms | loss0.005 | Cubic | datagram | 1.1 % | 836 | 350 ms · 75.7 % · 25.2 % | 55.1 % |
| 10 ms | loss0.005 | Cubic | carrier | 3.6 % | 268 | 239 ms · 96.2 % · 4.2 % | 28.6 % |
| 10 ms | loss0.005 | BBR | datagram | 0.0 % | 26 | 50 ms · 100.0 % · 0.0 % | 0.4 % |
| 10 ms | loss0.005 | BBR | carrier | 0.0 % | 26 | 50 ms · 100.0 % · 0.0 % | 0.3 % |
| 10 ms | ge1pct | Cubic | datagram | 21.0 % | 335 | 78 ms · 75.8 % · 26.0 % | 42.0 % |
| 10 ms | ge1pct | Cubic | carrier | 0.2 % | 160 | 126 ms · 98.5 % · 1.5 % | 17.1 % |
| 10 ms | ge1pct | BBR | datagram | 17.8 % | 26 | 50 ms · 89.5 % · 11.8 % | 14.7 % |
| 10 ms | ge1pct | BBR | carrier | 0.0 % | 27 | 50 ms · 100.0 % · 0.0 % | 1.6 % |
| 10 ms | out80drop | Cubic | datagram | 21.8 % | 137 | 65 ms · 75.6 % · 25.8 % | 33.8 % |
| 10 ms | out80drop | Cubic | carrier | 0.0 % | 142 | 104 ms · 93.1 % · 2.4 % | 19.2 % |
| 10 ms | out80drop | BBR | datagram | 27.9 % | 14 | 50 ms · 72.5 % · 28.9 % | 29.0 % |
| 10 ms | out80drop | BBR | carrier | 0.0 % | 102 | 82 ms · 96.9 % · 3.8 % | 10.5 % |
| 40 ms | clean | Cubic | datagram | 0.4 % | 35 | 50 ms · 99.9 % · 0.1 % | 0.2 % |
| 40 ms | clean | Cubic | carrier | 0.0 % | 34 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 40 ms | clean | BBR | datagram | 0.0 % | 28 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 40 ms | clean | BBR | carrier | 0.0 % | 28 | 50 ms · 100.0 % · 0.0 % | 0.0 % |
| 40 ms | loss0.001 | Cubic | datagram | 27.9 % | 1889 | 350 ms · 48.3 % · 52.5 % | 78.4 % |
| 40 ms | loss0.001 | Cubic | carrier | 13.8 % | 354 | 286 ms · 87.1 % · 14.8 % | 38.8 % |
| 40 ms | loss0.001 | BBR | datagram | 0.0 % | 51 | 50 ms · 99.3 % · 0.1 % | 2.6 % |
| 40 ms | loss0.001 | BBR | carrier | 0.0 % | 68 | 50 ms · 98.4 % · 0.1 % | 6.3 % |
| 40 ms | loss0.005 | Cubic | datagram | 99.5 % | 2007 | 350 ms · 0.8 % · 99.7 % | 99.7 % |
| 40 ms | loss0.005 | Cubic | carrier | 83.0 % | 1356 | 350 ms · 13.4 % · 89.4 % | 91.2 % |
| 40 ms | loss0.005 | BBR | datagram | 0.0 % | 71 | 50 ms · 98.2 % · 0.0 % | 7.1 % |
| 40 ms | loss0.005 | BBR | carrier | 0.0 % | 76 | 79 ms · 99.6 % · 0.5 % | 23.3 % |
| 40 ms | ge1pct | Cubic | datagram | 91.3 % | 2319 | 350 ms · 1.2 % · 99.8 % | 99.8 % |
| 40 ms | ge1pct | Cubic | carrier | 67.2 % | 1017 | 350 ms · 28.7 % · 74.7 % | 76.6 % |
| 40 ms | ge1pct | BBR | datagram | 18.5 % | 69 | 50 ms · 91.1 % · 9.6 % | 14.7 % |
| 40 ms | ge1pct | BBR | carrier | 0.0 % | 74 | 69 ms · 99.2 % · 0.6 % | 15.1 % |
| 40 ms | out80drop | Cubic | datagram | 68.0 % | 2272 | 350 ms · 5.3 % · 96.4 % | 97.4 % |
| 40 ms | out80drop | Cubic | carrier | 35.9 % | 523 | 350 ms · 57.6 % · 45.0 % | 59.3 % |
| 40 ms | out80drop | BBR | datagram | 28.0 % | 31 | 50 ms · 72.6 % · 28.9 % | 29.2 % |
| 40 ms | out80drop | BBR | carrier | 0.0 % | 148 | 113 ms · 96.7 % · 4.0 % | 13.3 % |

#### Figure — the Balanced offset through a loss episode (10 ms RTT, 2 % loss for 10 s of every 40 s)

| Second | 0 | 5 | 10 | 15 | 20 | 25 | 30 | 35 | 40 | 45 | 50 | 55 | 60 | 65 | 70 | 75 | 80 | 85 | 90 | 95 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| datagram · Cubic | 150 | 150 | 250 | 350 | 350 | 350 | 350 | 350 | 350 | 350 | 350 | 350 | 345 | 320 | 295 | 270 | 245 | 220 | 195 | 205 |
| carrier · Cubic | 150 | 150 | 181 | 257 | 257 | 257 | 257 | 257 | 237 | 212 | 203 | 245 | 245 | 245 | 245 | 245 | 245 | 245 | 253 | 275 |

#### Table G — the real relay's ingress on a clean path (F7, D7)

`gawk-server` at HEAD, the lab sender publishing a 60 s trace through the
proxy at 10 ms RTT and a 300 Mbps bottleneck, `gawk-loadgen` attached;
the relay pinned to 3 cores, the viewers to 2, the sender and proxy to 2.
In every run quinn counted **0 lost packets** and the proxy dropped
nothing, so every lost chunk was delivered and acknowledged first.

| Run | Rung | Viewers | Relay build | Ingress chunks lost (60 s) |
|---|---|---|---|---|
| R1 | 1440p | 15 | stock | 4 252 (≈ 2 %) |
| R2 | 1440p | 0 | stock | 182 |
| R3 | 1440p | 15 | publisher loop drains into a 1024-deep channel on its own goroutine | 3 957 |
| R-128 | 1440p | 15 | quic-go connection queue 128 → 4096 | 4 263 |
| R-32 | 1440p | 15 | **`http3` session queue 32 → 4096** | **0** |
| R7 | 1440p | 15 | both queues → 4096 | 0 |
| R8 | 1440p | 0 | both queues → 4096 | 0 |
| R4 | 1080p | 15 | stock | 0 |
| R9 | 1080p | 0 | stock | 0 |

The test builds came from scratch worktrees; no relay code changes with
this doc.

#### Table H — receive windows at 1440p, carrier · BBR (D10)

| Path | Windows (stream / connection) | Writes blocked > 1 ms | Decode-ready p50 / p95 / p99 ms | Balanced: offset · shown · stalled |
|---|---|---|---|---|
| clean | 512 KiB / 768 KiB (quic-go default) | 11 | 7 / 12 / 22 | 50 ms · 99.8 % · 0.2 % |
| clean | 2 MB / 3 MB | 1 | 7 / 9 / 15 | 50 ms · 100.0 % · 0.0 % |
| 80 Mbps + outages | 512 KiB / 768 KiB (quic-go default) | 106 | 14 / 95 / 143 | 121 ms · 98.4 % · 1.6 % |
| 80 Mbps + outages | 2 MB / 3 MB | 5 | 14 / 97 / 149 | 120 ms · 98.2 % · 1.9 % |

#### Table I — what a live-edge viewer's hitches look like, BBR (F2, §6)

| RTT | Path | Delivery | Hitches per minute: 50–100 · 100–250 · 250–500 · ≥ 500 ms | Longest |
|---|---|---|---|---|
| 10 ms | bursts (GE) | datagram | 7 · 38 · 5 · 0 | 367 ms |
| 10 ms | bursts (GE) | carrier | 14 · 0 · 0 · 0 | 50 ms |
| 10 ms | radio away 20 ms | datagram | 8 · 41 · 6 · 0 | 483 ms |
| 10 ms | radio away 20 ms | carrier | 47 · 0 · 0 · 0 | 67 ms |
| 10 ms | radio away 80 ms | datagram | 2 · 25 · 34 · 1 | 517 ms |
| 10 ms | radio away 80 ms | carrier | 1 · 55 · 0 · 0 | 117 ms |
| 40 ms | bursts (GE) | datagram | 41 · 36 · 4 · 0 | 367 ms |
| 40 ms | bursts (GE) | carrier | 119 · 5 · 1 · 0 | 450 ms |
| 40 ms | radio away 20 ms | datagram | 11 · 40 · 5 · 0 | 434 ms |
| 40 ms | radio away 20 ms | carrier | 56 · 0 · 0 · 0 | 84 ms |
| 40 ms | radio away 80 ms | datagram | 0 · 25 · 32 · 2 | 500 ms |
| 40 ms | radio away 80 ms | carrier | 0 · 56 · 0 · 0 | 150 ms |

#### Table J — what each viewer preset sees, by uplink delivery, BBR (F14)

Shown · stalled. Lowest latency and Balanced get parity; Smoother and Most stable ride reliable
delivery and are served none (`hub.NegotiateParity`), so they are sent only frames that reached
the relay whole. Freezes: holds ≥ 250 ms per minute. For Lowest latency "shown" also falls when
two frames land in one vsync (the newer wins); its stalled time is the loss measure.

| RTT | Path | Uplink | Lowest latency | Balanced | Smoother | Most stable | Smoother freezes /min |
|---|---|---|---|---|---|---|---|
| 10 ms | clean | datagram | 99.7 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | clean | carrier | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | 0.1 % loss | datagram | 98.9 % · 0.0 % | 100.0 % · 0.0 % | 88.1 % · 12.7 % | 87.5 % · 12.4 % | 16 |
| 10 ms | 0.1 % loss | carrier | 98.8 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | 1 % loss | datagram | 92.8 % · 0.4 % | 100.0 % · 0.0 % | 38.4 % · 64.4 % | 38.6 % · 63.6 % | 91 |
| 10 ms | 1 % loss | carrier | 89.7 % · 0.6 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | bursts (GE) | datagram | 89.7 % · 10.7 % | 92.4 % · 9.1 % | 72.1 % · 29.8 % | 72.4 % · 29.4 % | 40 |
| 10 ms | bursts (GE) | carrier | 95.9 % · 1.2 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | radio away 20 ms | datagram | 89.3 % · 12.1 % | 91.6 % · 9.8 % | 76.9 % · 24.1 % | 77.2 % · 24.7 % | 30 |
| 10 ms | radio away 20 ms | carrier | 96.7 % · 4.1 % | 99.8 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 10 ms | radio away 80 ms | datagram | 72.8 % · 28.4 % | 73.5 % · 27.9 % | 72.7 % · 28.4 % | 72.7 % · 30.0 % | 35 |
| 10 ms | radio away 80 ms | carrier | 91.3 % · 10.2 % | 97.2 % · 3.2 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | clean | datagram | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | clean | carrier | 99.8 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | 0.1 % loss | datagram | 99.3 % · 0.6 % | 99.9 % · 0.0 % | 88.6 % · 12.2 % | 89.0 % · 11.0 % | 14 |
| 40 ms | 0.1 % loss | carrier | 96.7 % · 4.5 % | 99.5 % · 0.4 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | 1 % loss | datagram | 90.7 % · 12.0 % | 95.8 % · 3.5 % | 33.6 % · 68.9 % | 32.4 % · 68.2 % | 90 |
| 40 ms | 1 % loss | carrier | 77.8 % · 29.7 % | 99.2 % · 0.4 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | bursts (GE) | datagram | 88.9 % · 12.9 % | 93.4 % · 7.8 % | 70.5 % · 31.6 % | 71.2 % · 32.2 % | 44 |
| 40 ms | bursts (GE) | carrier | 89.4 % · 13.1 % | 98.5 % · 1.4 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | radio away 20 ms | datagram | 90.1 % · 11.3 % | 92.4 % · 8.9 % | 76.4 % · 24.7 % | 77.1 % · 24.2 % | 31 |
| 40 ms | radio away 20 ms | carrier | 95.0 % · 6.5 % | 98.0 % · 1.1 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 40 ms | radio away 80 ms | datagram | 72.8 % · 28.6 % | 73.4 % · 28.1 % | 71.6 % · 29.5 % | 71.9 % · 30.1 % | 37 |
| 40 ms | radio away 80 ms | carrier | 89.3 % · 12.3 % | 97.6 % · 1.9 % | 98.6 % · 1.5 % | 100.0 % · 0.0 % | 0 |
| 100 ms | clean | datagram | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 100 ms | clean | carrier | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 0 |
| 100 ms | 0.1 % loss | datagram | 89.9 % · 10.6 % | 90.7 % · 9.9 % | 88.4 % · 12.4 % | 88.7 % · 12.0 % | 14 |
| 100 ms | 0.1 % loss | carrier | 91.3 % · 10.0 % | 97.0 % · 2.5 % | 98.9 % · 1.2 % | 100.0 % · 0.0 % | 0 |
| 100 ms | 1 % loss | datagram | 73.3 % · 28.6 % | 94.4 % · 5.9 % | 33.7 % · 68.5 % | 35.8 % · 66.9 % | 96 |
| 100 ms | 1 % loss | carrier | 49.1 % · 58.7 % | 95.8 % · 4.0 % | 94.1 % · 6.7 % | 98.9 % · 1.3 % | 0 |
| 100 ms | bursts (GE) | datagram | 77.8 % · 25.3 % | 79.9 % · 18.8 % | 69.9 % · 32.6 % | 69.8 % · 30.4 % | 46 |
| 100 ms | bursts (GE) | carrier | 77.6 % · 25.7 % | 98.2 % · 2.3 % | 98.3 % · 2.3 % | 99.9 % · 0.2 % | 0 |
| 100 ms | radio away 20 ms | datagram | 82.3 % · 19.0 % | 85.9 % · 15.4 % | 76.4 % · 24.9 % | 76.8 % · 23.3 % | 31 |
| 100 ms | radio away 20 ms | carrier | 88.6 % · 13.0 % | 98.3 % · 1.6 % | 98.5 % · 1.6 % | 100.0 % · 0.0 % | 0 |
| 100 ms | radio away 80 ms | datagram | 69.8 % · 31.8 % | 70.7 % · 31.0 % | 69.9 % · 31.4 % | 71.4 % · 29.4 % | 38 |
| 100 ms | radio away 80 ms | carrier | 82.4 % · 19.0 % | 93.5 % · 4.9 % | 96.4 % · 3.4 % | 99.5 % · 0.6 % | 0 |

#### Table K — what-if: the relay serves parity to reliable delivery too, datagram uplink, BBR (F14, OD6)

| RTT | Path | Smoother today → with parity | Most stable today → with parity | Smoother on the carrier uplink |
|---|---|---|---|---|
| 10 ms | 0.1 % loss | 88.1 % · 12.7 % → 99.9 % · 0.0 % | 87.5 % · 12.4 % → 100.0 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 0.5 % loss | 57.9 % · 44.2 % → 99.9 % · 0.0 % | 56.7 % · 44.7 % → 100.0 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 1 % loss | 38.4 % · 64.4 % → 99.9 % · 0.0 % | 38.6 % · 63.6 % → 100.0 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 2 % loss | 21.9 % · 81.1 % → 96.2 % · 4.9 % | 20.7 % · 81.0 % → 96.1 % · 4.4 % | 99.9 % · 0.0 % |
| 10 ms | bursts (GE) | 72.1 % · 29.8 % → 82.8 % · 18.3 % | 72.4 % · 29.4 % → 82.5 % · 17.3 % | 99.9 % · 0.0 % |
| 10 ms | radio away 20 ms | 76.9 % · 24.1 % → 76.9 % · 24.1 % | 77.2 % · 24.7 % → 77.2 % · 24.7 % | 99.9 % · 0.0 % |
| 10 ms | radio away 80 ms | 72.7 % · 28.4 % → 72.7 % · 28.4 % | 72.7 % · 30.0 % → 72.7 % · 30.0 % | 99.9 % · 0.0 % |
| 40 ms | 0.1 % loss | 88.6 % · 12.2 % → 99.9 % · 0.0 % | 89.0 % · 11.0 % → 100.0 % · 0.0 % | 99.9 % · 0.0 % |
| 40 ms | 0.5 % loss | 53.2 % · 49.4 % → 99.7 % · 0.8 % | 53.3 % · 48.7 % → 99.8 % · 0.2 % | 99.9 % · 0.0 % |
| 40 ms | 1 % loss | 33.6 % · 68.9 % → 99.7 % · 0.3 % | 32.4 % · 68.2 % → 99.7 % · 0.3 % | 99.9 % · 0.0 % |
| 40 ms | 2 % loss | 22.6 % · 80.6 % → 95.2 % · 4.9 % | 21.6 % · 80.6 % → 95.2 % · 5.5 % | 99.7 % · 0.2 % |
| 40 ms | bursts (GE) | 70.5 % · 31.6 % → 84.1 % · 17.1 % | 71.2 % · 32.2 % → 84.0 % · 16.4 % | 99.9 % · 0.0 % |
| 40 ms | radio away 20 ms | 76.4 % · 24.7 % → 76.4 % · 24.7 % | 77.1 % · 24.2 % → 77.1 % · 24.2 % | 99.9 % · 0.0 % |
| 40 ms | radio away 80 ms | 71.6 % · 29.5 % → 71.6 % · 29.5 % | 71.9 % · 30.1 % → 71.9 % · 30.1 % | 98.6 % · 1.5 % |
| 100 ms | 0.1 % loss | 88.4 % · 12.4 % → 99.9 % · 0.0 % | 88.7 % · 12.0 % → 100.0 % · 0.0 % | 98.9 % · 1.2 % |
| 100 ms | 0.5 % loss | 55.2 % · 47.1 % → 98.2 % · 1.9 % | 58.3 % · 43.9 % → 100.0 % · 0.0 % | 97.6 % · 2.7 % |
| 100 ms | 1 % loss | 33.7 % · 68.5 % → 95.6 % · 5.0 % | 35.8 % · 66.9 % → 100.0 % · 0.0 % | 94.1 % · 6.7 % |
| 100 ms | 2 % loss | 16.5 % · 87.0 % → 80.7 % · 20.8 % | 22.5 % · 80.9 % → 89.9 % · 9.9 % | 96.3 % · 4.1 % |
| 100 ms | bursts (GE) | 69.9 % · 32.6 % → 80.9 % · 20.8 % | 69.8 % · 30.4 % → 81.4 % · 18.4 % | 98.3 % · 2.3 % |
| 100 ms | radio away 20 ms | 76.4 % · 24.9 % → 76.4 % · 24.9 % | 76.8 % · 23.3 % → 76.8 % · 23.3 % | 98.5 % · 1.6 % |
| 100 ms | radio away 80 ms | 69.9 % · 31.4 % → 69.9 % · 31.4 % | 71.4 % · 29.4 % → 71.4 % · 29.4 % | 96.4 % · 3.4 % |

#### Table L — what the cheap changes buy without the carrier (shown · stalled)

"today": datagrams, Cubic, quic-go's 32-deep queue. "queue": the receiver built with D7's 4096-deep
queue. "carrier + BBR" is OD1 + OD2 for comparison; its deltas never meet the queue.

#### Table L1 — Balanced (the default viewer)

| RTT | Path | today | BBR only | queue only | BBR + queue | carrier + BBR |
|---|---|---|---|---|---|---|
| 10 ms | clean | 99.9 % · 0.1 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 0.1 % loss | 99.6 % · 0.1 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 0.5 % loss | 94.1 % · 5.6 % | 100.0 % · 0.0 % | 94.7 % · 5.2 % | 100.0 % · 0.1 % | 100.0 % · 0.0 % |
| 10 ms | 1 % loss | 70.4 % · 29.3 % | 100.0 % · 0.0 % | 76.3 % · 22.8 % | 99.8 % · 0.2 % | 100.0 % · 0.0 % |
| 10 ms | 2 % loss | 28.9 % · 73.1 % | 99.1 % · 1.3 % | 31.6 % · 69.6 % | 99.1 % · 1.1 % | 99.9 % · 0.0 % |
| 10 ms | bursts (GE) | 87.8 % · 12.3 % | 92.4 % · 9.1 % | 86.4 % · 14.2 % | 93.0 % · 8.1 % | 100.0 % · 0.0 % |
| 10 ms | radio away 20 ms | 92.2 % · 9.1 % | 91.6 % · 9.8 % | 92.1 % · 9.1 % | 91.2 % · 10.1 % | 99.8 % · 0.0 % |
| 10 ms | radio away 80 ms | 77.2 % · 24.1 % | 73.5 % · 27.9 % | 77.3 % · 24.1 % | 73.1 % · 28.2 % | 97.2 % · 3.2 % |
| 40 ms | clean | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 40 ms | 0.1 % loss | 63.3 % · 37.8 % | 99.9 % · 0.0 % | 62.3 % · 38.4 % | 99.9 % · 0.0 % | 99.5 % · 0.4 % |
| 40 ms | 0.5 % loss | 0.6 % · 99.9 % | 99.5 % · 0.2 % | 0.6 % · 100.0 % | 94.2 % · 5.6 % | 99.5 % · 0.4 % |
| 40 ms | 1 % loss | 0.3 % · 100.0 % | 95.8 % · 3.5 % | 0.4 % · 100.0 % | 89.1 % · 10.7 % | 99.2 % · 0.4 % |
| 40 ms | 2 % loss | 0.1 % · 100.0 % | 85.7 % · 15.0 % | 0.2 % · 100.0 % | 80.9 % · 20.3 % | 97.0 % · 2.6 % |
| 40 ms | bursts (GE) | 1.1 % · 99.5 % | 93.4 % · 7.8 % | 2.4 % · 98.5 % | 92.9 % · 8.5 % | 98.5 % · 1.4 % |
| 40 ms | radio away 20 ms | 20.0 % · 82.5 % | 92.4 % · 8.9 % | 24.6 % · 77.5 % | 91.7 % · 9.6 % | 98.0 % · 1.1 % |
| 40 ms | radio away 80 ms | 18.6 % · 82.3 % | 73.4 % · 28.1 % | 19.0 % · 82.9 % | 73.4 % · 28.0 % | 97.6 % · 1.9 % |
| 100 ms | clean | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 100 ms | 0.1 % loss | 2.7 % · 97.9 % | 90.7 % · 9.9 % | 2.7 % · 98.0 % | 93.0 % · 2.3 % | 97.0 % · 2.5 % |
| 100 ms | 0.5 % loss | 0.1 % · 100.0 % | 93.8 % · 4.9 % | 0.1 % · 100.0 % | 97.1 % · 2.7 % | 95.8 % · 5.0 % |
| 100 ms | 1 % loss | 0.0 % · 100.0 % | 94.4 % · 5.9 % | 0.1 % · 100.0 % | 96.8 % · 2.9 % | 95.8 % · 4.0 % |
| 100 ms | 2 % loss | 0.0 % · 100.0 % | 95.4 % · 4.0 % | 0.0 % · 100.0 % | 94.7 % · 4.8 % | 97.0 % · 3.5 % |
| 100 ms | bursts (GE) | 0.4 % · 100.0 % | 79.9 % · 18.8 % | 0.3 % · 100.0 % | 82.2 % · 19.8 % | 98.2 % · 2.3 % |
| 100 ms | radio away 20 ms | 0.1 % · 100.0 % | 85.9 % · 15.4 % | 0.2 % · 100.0 % | 90.0 % · 11.2 % | 98.3 % · 1.6 % |
| 100 ms | radio away 80 ms | 0.0 % · 100.0 % | 70.7 % · 31.0 % | 0.0 % · 100.0 % | 70.7 % · 30.9 % | 93.5 % · 4.9 % |
| 10 ms | 1440p clean | 89.5 % · 11.8 % | 81.8 % · 20.1 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 99.8 % · 0.2 % |
| 10 ms | 1440p 0.1 % loss | 49.6 % · 52.1 % | 97.7 % · 2.7 % | 42.1 % · 59.5 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 1440p bursts | 2.1 % · 99.8 % | 57.7 % · 46.6 % | 3.9 % · 98.1 % | 57.0 % · 47.4 % | 100.0 % · 0.0 % |
| 10 ms | 1440p 300 Mbps + outages | 75.5 % · 26.1 % | 72.7 % · 28.7 % | 75.6 % · 26.0 % | 73.0 % · 28.4 % | 95.1 % · 3.5 % |
| 10 ms | 1440p 80 Mbps + outages | 75.7 % · 25.4 % | 70.5 % · 30.9 % | 75.9 % · 25.0 % | 71.0 % · 30.3 % | 98.4 % · 1.6 % |

#### Table L2 — Lowest latency (stalled is the loss measure)

| RTT | Path | today | BBR only | queue only | BBR + queue | carrier + BBR |
|---|---|---|---|---|---|---|
| 10 ms | clean | 98.4 % · 0.3 % | 99.7 % · 0.0 % | 99.7 % · 0.0 % | 100.0 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 0.1 % loss | 94.9 % · 3.8 % | 98.9 % · 0.0 % | 96.2 % · 2.6 % | 99.6 % · 0.0 % | 98.8 % · 0.0 % |
| 10 ms | 0.5 % loss | 68.7 % · 32.1 % | 98.2 % · 0.1 % | 68.2 % · 32.9 % | 98.2 % · 0.2 % | 94.0 % · 0.2 % |
| 10 ms | 1 % loss | 43.0 % · 56.9 % | 92.8 % · 0.4 % | 43.9 % · 55.8 % | 93.7 % · 0.9 % | 89.7 % · 0.6 % |
| 10 ms | 2 % loss | 15.7 % · 85.3 % | 95.4 % · 2.9 % | 16.5 % · 84.4 % | 95.5 % · 2.4 % | 84.0 % · 1.5 % |
| 10 ms | bursts (GE) | 77.7 % · 24.7 % | 89.7 % · 10.7 % | 76.0 % · 26.5 % | 89.8 % · 10.0 % | 95.9 % · 1.2 % |
| 10 ms | radio away 20 ms | 86.1 % · 14.3 % | 89.3 % · 12.1 % | 86.2 % · 13.3 % | 89.1 % · 12.3 % | 96.7 % · 4.1 % |
| 10 ms | radio away 80 ms | 73.2 % · 27.2 % | 72.8 % · 28.4 % | 72.9 % · 27.4 % | 72.9 % · 28.4 % | 91.3 % · 10.2 % |
| 40 ms | clean | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 99.6 % · 0.0 % | 100.0 % · 0.0 % | 99.8 % · 0.0 % |
| 40 ms | 0.1 % loss | 38.1 % · 64.2 % | 99.3 % · 0.6 % | 38.4 % · 63.2 % | 99.1 % · 1.0 % | 96.7 % · 4.5 % |
| 40 ms | 0.5 % loss | 0.6 % · 99.9 % | 96.3 % · 5.5 % | 0.5 % · 100.0 % | 90.0 % · 10.8 % | 84.9 % · 20.8 % |
| 40 ms | 1 % loss | 0.3 % · 100.0 % | 90.7 % · 12.0 % | 0.4 % · 100.0 % | 81.0 % · 20.0 % | 77.8 % · 29.7 % |
| 40 ms | 2 % loss | 0.1 % · 100.0 % | 76.0 % · 25.7 % | 0.2 % · 100.0 % | 72.2 % · 29.4 % | 58.6 % · 51.1 % |
| 40 ms | bursts (GE) | 1.1 % · 99.5 % | 88.9 % · 12.9 % | 2.4 % · 98.5 % | 89.4 % · 12.9 % | 89.4 % · 13.1 % |
| 40 ms | radio away 20 ms | 8.5 % · 93.8 % | 90.1 % · 11.3 % | 10.2 % · 91.6 % | 89.5 % · 11.9 % | 95.0 % · 6.5 % |
| 40 ms | radio away 80 ms | 8.8 % · 93.1 % | 72.8 % · 28.6 % | 10.4 % · 91.0 % | 73.1 % · 28.4 % | 89.3 % · 12.3 % |
| 100 ms | clean | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 100 ms | 0.1 % loss | 2.3 % · 98.3 % | 89.9 % · 10.6 % | 1.7 % · 98.9 % | 96.5 % · 3.8 % | 91.3 % · 10.0 % |
| 100 ms | 0.5 % loss | 0.1 % · 100.0 % | 83.0 % · 18.4 % | 0.1 % · 100.0 % | 83.4 % · 18.3 % | 64.7 % · 40.1 % |
| 100 ms | 1 % loss | 0.0 % · 100.0 % | 73.3 % · 28.6 % | 0.1 % · 100.0 % | 73.4 % · 28.9 % | 49.1 % · 58.7 % |
| 100 ms | 2 % loss | 0.0 % · 100.0 % | 59.3 % · 44.3 % | 0.0 % · 100.0 % | 64.3 % · 38.5 % | 32.7 % · 76.1 % |
| 100 ms | bursts (GE) | 0.3 % · 100.0 % | 77.8 % · 25.3 % | 0.3 % · 100.0 % | 76.0 % · 25.5 % | 77.6 % · 25.7 % |
| 100 ms | radio away 20 ms | 0.1 % · 100.0 % | 82.3 % · 19.0 % | 0.2 % · 100.0 % | 76.3 % · 15.8 % | 88.6 % · 13.0 % |
| 100 ms | radio away 80 ms | 0.0 % · 100.0 % | 69.8 % · 31.8 % | 0.0 % · 100.0 % | 68.9 % · 31.9 % | 82.4 % · 19.0 % |
| 10 ms | 1440p clean | 86.0 % · 13.6 % | 77.3 % · 23.0 % | 98.1 % · 0.1 % | 98.3 % · 0.1 % | 96.9 % · 1.3 % |
| 10 ms | 1440p 0.1 % loss | 29.3 % · 70.6 % | 89.8 % · 4.3 % | 24.3 % · 75.6 % | 96.7 % · 1.0 % | 93.1 % · 0.6 % |
| 10 ms | 1440p bursts | 2.0 % · 99.9 % | 48.7 % · 52.9 % | 2.4 % · 99.5 % | 48.5 % · 53.5 % | 87.4 % · 2.5 % |
| 10 ms | 1440p 300 Mbps + outages | 65.4 % · 36.4 % | 70.2 % · 30.4 % | 66.4 % · 35.5 % | 69.9 % · 30.7 % | 88.5 % · 11.5 % |
| 10 ms | 1440p 80 Mbps + outages | 56.7 % · 44.4 % | 59.9 % · 39.9 % | 57.3 % · 43.2 % | 60.6 % · 39.6 % | 78.8 % · 22.8 % |

#### Table L3 — Smoother (no parity today; the last column adds OD6's parity)

| RTT | Path | today | BBR only | queue only | BBR + queue | carrier + BBR | BBR + queue + OD6 parity |
|---|---|---|---|---|---|---|---|
| 10 ms | clean | 99.2 % · 0.7 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 0.1 % loss | 86.7 % · 13.9 % | 88.1 % · 12.7 % | 86.4 % · 14.2 % | 87.5 % · 13.4 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 0.5 % loss | 48.2 % · 53.8 % | 57.9 % · 44.2 % | 50.5 % · 52.1 % | 52.3 % · 49.9 % | 99.9 % · 0.0 % | 99.9 % · 0.4 % |
| 10 ms | 1 % loss | 28.9 % · 73.7 % | 38.4 % · 64.4 % | 28.7 % · 73.4 % | 39.4 % · 63.4 % | 99.9 % · 0.0 % | 99.8 % · 0.2 % |
| 10 ms | 2 % loss | 16.8 % · 85.5 % | 21.9 % · 81.1 % | 16.1 % · 86.7 % | 18.8 % · 84.6 % | 99.9 % · 0.0 % | 95.9 % · 4.1 % |
| 10 ms | bursts (GE) | 69.3 % · 31.9 % | 72.1 % · 29.8 % | 68.8 % · 32.5 % | 70.3 % · 31.4 % | 99.9 % · 0.0 % | 81.9 % · 18.7 % |
| 10 ms | radio away 20 ms | 77.2 % · 23.8 % | 76.9 % · 24.1 % | 78.6 % · 22.3 % | 76.6 % · 24.4 % | 99.9 % · 0.0 % | 76.6 % · 24.4 % |
| 10 ms | radio away 80 ms | 76.7 % · 24.5 % | 72.7 % · 28.4 % | 76.8 % · 24.4 % | 72.1 % · 28.9 % | 99.9 % · 0.0 % | 72.1 % · 28.9 % |
| 40 ms | clean | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 40 ms | 0.1 % loss | 70.8 % · 30.9 % | 88.6 % · 12.2 % | 69.4 % · 31.0 % | 86.1 % · 15.1 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 40 ms | 0.5 % loss | 0.7 % · 99.9 % | 53.2 % · 49.4 % | 0.8 % · 99.7 % | 51.9 % · 50.9 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 40 ms | 1 % loss | 0.4 % · 100.0 % | 33.6 % · 68.9 % | 0.4 % · 100.0 % | 41.1 % · 61.3 % | 99.9 % · 0.0 % | 99.2 % · 0.7 % |
| 40 ms | 2 % loss | 0.1 % · 100.0 % | 22.6 % · 80.6 % | 0.2 % · 100.0 % | 22.0 % · 81.5 % | 99.7 % · 0.2 % | 94.6 % · 5.3 % |
| 40 ms | bursts (GE) | 1.3 % · 99.3 % | 70.5 % · 31.6 % | 5.3 % · 95.9 % | 73.5 % · 28.6 % | 99.9 % · 0.0 % | 83.5 % · 18.0 % |
| 40 ms | radio away 20 ms | 44.3 % · 56.6 % | 76.4 % · 24.7 % | 46.2 % · 55.0 % | 76.1 % · 25.0 % | 99.9 % · 0.0 % | 76.1 % · 25.0 % |
| 40 ms | radio away 80 ms | 36.1 % · 64.7 % | 71.6 % · 29.5 % | 34.5 % · 66.3 % | 71.7 % · 29.5 % | 98.6 % · 1.5 % | 71.7 % · 29.5 % |
| 100 ms | clean | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 100 ms | 0.1 % loss | 5.8 % · 94.3 % | 88.4 % · 12.4 % | 5.6 % · 94.4 % | 89.4 % · 11.3 % | 98.9 % · 1.2 % | 99.8 % · 0.1 % |
| 100 ms | 0.5 % loss | 0.1 % · 100.0 % | 55.2 % · 47.1 % | 0.1 % · 100.0 % | 53.0 % · 49.0 % | 97.6 % · 2.7 % | 98.9 % · 1.0 % |
| 100 ms | 1 % loss | 0.0 % · 100.0 % | 33.7 % · 68.5 % | 0.1 % · 100.0 % | 34.4 % · 68.3 % | 94.1 % · 6.7 % | 95.0 % · 5.7 % |
| 100 ms | 2 % loss | 0.0 % · 100.0 % | 16.5 % · 87.0 % | 0.0 % · 100.0 % | 17.0 % · 86.6 % | 96.3 % · 4.1 % | 86.7 % · 14.2 % |
| 100 ms | bursts (GE) | 0.3 % · 100.0 % | 69.9 % · 32.6 % | 0.3 % · 100.0 % | 69.7 % · 32.6 % | 98.3 % · 2.3 % | 80.2 % · 21.3 % |
| 100 ms | radio away 20 ms | 0.1 % · 100.0 % | 76.4 % · 24.9 % | 0.2 % · 100.0 % | 76.8 % · 24.6 % | 98.5 % · 1.6 % | 76.8 % · 24.6 % |
| 100 ms | radio away 80 ms | 0.0 % · 100.0 % | 69.9 % · 31.4 % | 0.0 % · 100.0 % | 70.3 % · 31.3 % | 96.4 % · 3.4 % | 70.3 % · 31.3 % |
| 10 ms | 1440p clean | 81.3 % · 19.7 % | 69.4 % · 31.4 % | 99.9 % · 0.0 % | 99.6 % · 0.0 % | 99.9 % · 0.0 % | 99.6 % · 0.0 % |
| 10 ms | 1440p 0.1 % loss | 30.9 % · 72.0 % | 50.7 % · 51.5 % | 25.0 % · 78.4 % | 49.8 % · 52.5 % | 99.9 % · 0.0 % | 99.9 % · 0.0 % |
| 10 ms | 1440p bursts | 3.1 % · 98.8 % | 27.2 % · 76.0 % | 6.0 % · 96.1 % | 25.6 % · 78.0 % | 99.9 % · 0.0 % | 38.4 % · 64.2 % |
| 10 ms | 1440p 300 Mbps + outages | 73.7 % · 27.7 % | 71.1 % · 30.0 % | 75.4 % · 26.0 % | 72.0 % · 29.0 % | 99.4 % · 0.5 % | 72.0 % · 29.0 % |
| 10 ms | 1440p 80 Mbps + outages | 73.3 % · 27.9 % | 70.5 % · 30.7 % | 73.9 % · 27.3 % | 71.9 % · 29.2 % | 98.8 % · 1.2 % | 71.9 % · 29.2 % |

#### Table L4 — Most stable (no parity today; the last column adds OD6's parity)

| RTT | Path | today | BBR only | queue only | BBR + queue | carrier + BBR | BBR + queue + OD6 parity |
|---|---|---|---|---|---|---|---|
| 10 ms | clean | 99.3 % · 0.7 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 0.1 % loss | 86.7 % · 14.2 % | 87.5 % · 12.4 % | 85.7 % · 15.2 % | 87.1 % · 12.9 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 0.5 % loss | 50.7 % · 53.0 % | 56.7 % · 44.7 % | 54.7 % · 48.1 % | 52.3 % · 50.7 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 1 % loss | 39.8 % · 63.7 % | 38.6 % · 63.6 % | 36.3 % · 66.4 % | 39.8 % · 62.9 % | 100.0 % · 0.0 % | 99.9 % · 0.2 % |
| 10 ms | 2 % loss | 17.5 % · 83.2 % | 20.7 % · 81.0 % | 16.8 % · 83.9 % | 18.2 % · 84.5 % | 100.0 % · 0.0 % | 95.7 % · 4.1 % |
| 10 ms | bursts (GE) | 69.2 % · 31.0 % | 72.4 % · 29.4 % | 68.6 % · 31.5 % | 68.6 % · 32.4 % | 100.0 % · 0.0 % | 80.9 % · 18.7 % |
| 10 ms | radio away 20 ms | 77.5 % · 24.5 % | 77.2 % · 24.7 % | 79.0 % · 22.0 % | 76.9 % · 25.0 % | 100.0 % · 0.0 % | 76.9 % · 25.0 % |
| 10 ms | radio away 80 ms | 77.3 % · 24.6 % | 72.7 % · 30.0 % | 77.4 % · 24.5 % | 72.1 % · 30.6 % | 100.0 % · 0.0 % | 72.1 % · 30.6 % |
| 40 ms | clean | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 40 ms | 0.1 % loss | 75.8 % · 23.1 % | 89.0 % · 11.0 % | 73.6 % · 25.2 % | 86.5 % · 13.8 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 40 ms | 0.5 % loss | 0.7 % · 99.4 % | 53.3 % · 48.7 % | 0.8 % · 99.7 % | 51.9 % · 50.8 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 40 ms | 1 % loss | 0.4 % · 100.0 % | 32.4 % · 68.2 % | 0.4 % · 100.0 % | 39.6 % · 62.7 % | 100.0 % · 0.0 % | 99.3 % · 0.7 % |
| 40 ms | 2 % loss | 0.1 % · 100.0 % | 21.6 % · 80.6 % | 0.2 % · 100.0 % | 22.7 % · 80.6 % | 100.0 % · 0.0 % | 94.9 % · 5.0 % |
| 40 ms | bursts (GE) | 1.3 % · 99.3 % | 71.2 % · 32.2 % | 7.0 % · 94.3 % | 74.2 % · 28.7 % | 100.0 % · 0.0 % | 83.7 % · 17.1 % |
| 40 ms | radio away 20 ms | 52.5 % · 47.1 % | 77.1 % · 24.2 % | 53.8 % · 46.2 % | 76.8 % · 24.5 % | 100.0 % · 0.0 % | 76.8 % · 24.5 % |
| 40 ms | radio away 80 ms | 45.3 % · 54.8 % | 71.9 % · 30.1 % | 43.7 % · 56.3 % | 72.0 % · 30.1 % | 100.0 % · 0.0 % | 72.0 % · 30.1 % |
| 100 ms | clean | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 100 ms | 0.1 % loss | 6.8 % · 94.3 % | 88.7 % · 12.0 % | 5.2 % · 94.9 % | 89.3 % · 10.4 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 100 ms | 0.5 % loss | 0.1 % · 100.0 % | 58.3 % · 43.9 % | 0.1 % · 100.0 % | 54.6 % · 47.9 % | 99.9 % · 0.2 % | 100.0 % · 0.0 % |
| 100 ms | 1 % loss | 0.0 % · 100.0 % | 35.8 % · 66.9 % | 0.1 % · 100.0 % | 38.1 % · 64.2 % | 98.9 % · 1.3 % | 100.0 % · 0.0 % |
| 100 ms | 2 % loss | 0.0 % · 100.0 % | 22.5 % · 80.9 % | 0.0 % · 100.0 % | 21.2 % · 80.9 % | 98.4 % · 2.2 % | 93.9 % · 5.9 % |
| 100 ms | bursts (GE) | 0.3 % · 100.0 % | 69.8 % · 30.4 % | 0.3 % · 100.0 % | 70.0 % · 31.4 % | 99.9 % · 0.2 % | 80.1 % · 19.9 % |
| 100 ms | radio away 20 ms | 0.1 % · 100.0 % | 76.8 % · 23.3 % | 0.2 % · 100.0 % | 77.4 % · 22.9 % | 100.0 % · 0.0 % | 77.4 % · 22.9 % |
| 100 ms | radio away 80 ms | 0.0 % · 100.0 % | 71.4 % · 29.4 % | 0.0 % · 100.0 % | 71.4 % · 29.5 % | 99.5 % · 0.6 % | 71.4 % · 29.5 % |
| 10 ms | 1440p clean | 81.2 % · 20.1 % | 69.1 % · 32.7 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 1440p 0.1 % loss | 40.1 % · 63.2 % | 49.3 % · 51.1 % | 32.2 % · 71.6 % | 48.2 % · 53.1 % | 100.0 % · 0.0 % | 100.0 % · 0.0 % |
| 10 ms | 1440p bursts | 4.2 % · 97.7 % | 26.7 % · 77.4 % | 7.2 % · 95.0 % | 25.7 % · 77.2 % | 100.0 % · 0.0 % | 37.1 % · 65.1 % |
| 10 ms | 1440p 300 Mbps + outages | 75.5 % · 25.7 % | 71.0 % · 31.6 % | 77.1 % · 23.8 % | 72.0 % · 30.7 % | 99.7 % · 0.4 % | 72.0 % · 30.7 % |
| 10 ms | 1440p 80 Mbps + outages | 77.0 % · 24.6 % | 70.5 % · 32.2 % | 77.9 % · 23.1 % | 72.0 % · 30.7 % | 99.9 % · 0.0 % | 72.0 % · 30.7 % |

#### Table M — run-to-run spread: three loss-process seeds per cell (shown, % of frames)

| Path | Delivery | Balanced: seeds 7 · 8 · 9 | Smoother: seeds 7 · 8 · 9 | Lowest latency stalled: seeds 7 · 8 · 9 |
|---|---|---|---|---|
| 40 ms, 0.5 % loss | BBR only | 99.5 · 96.8 · 99.3 | 53.2 · 49.8 · 51.9 | 5.5 · 8.5 · 6.2 |
| 40 ms, 0.5 % loss | BBR + queue | 94.2 · 96.8 · 95.4 | 51.9 · 50.5 · 50.3 | 10.8 · 8.3 · 9.8 |
| 40 ms, 0.5 % loss | carrier + BBR | 99.5 · 99.6 · 99.2 | 99.9 · 99.9 · 99.9 | 20.8 · 21.4 · 19.5 |
| 40 ms, 1 % loss | BBR only | 95.8 · 92.3 · 96.8 | 33.6 · 36.0 · 34.4 | 12.0 · 16.9 · 10.6 |
| 40 ms, 1 % loss | BBR + queue | 89.1 · 90.8 · 92.8 | 41.1 · 37.2 · 36.9 | 20.0 · 17.3 · 15.1 |
| 40 ms, 1 % loss | carrier + BBR | 99.2 · 99.8 · 99.3 | 99.9 · 99.9 · 99.9 | 29.7 · 31.7 · 31.0 |
| 10 ms, bursts (GE) | BBR only | 92.4 · 92.0 · 89.7 | 72.1 · 63.5 · 66.4 | 10.7 · 12.0 · 13.8 |
| 10 ms, bursts (GE) | BBR + queue | 93.0 · 91.4 · 90.4 | 70.3 · 62.9 · 64.1 | 10.0 · 12.5 · 13.5 |
| 10 ms, bursts (GE) | carrier + BBR | 100.0 · 100.0 · 100.0 | 99.9 · 99.9 · 99.9 | 1.2 · 2.0 · 1.5 |
| 10 ms, radio away 80 ms | BBR only | 73.5 · 72.7 · 72.8 | 72.7 · 71.7 · 71.8 | 28.4 · 28.8 · 28.8 |
| 10 ms, radio away 80 ms | BBR + queue | 73.1 · 73.2 · 73.3 | 72.1 · 72.2 · 72.3 | 28.4 · 28.5 · 28.8 |
| 10 ms, radio away 80 ms | carrier + BBR | 97.2 · 97.2 · 97.1 | 99.9 · 99.9 · 99.8 | 10.2 · 10.3 · 10.2 |

## 12. References

- [docs/57](57-wifi-uplink.md) — R55, the Wi-Fi finding and the Mac's status line
- [docs/24](24-viewer-network-resilience.md) — R19, the carrier format reused here
- [docs/34](34-live-edge-forward-parity.md) — R29, parity and the loss allowance
- [docs/35](35-connection-interleaving.md) — R30, leg-B burst loss
- [docs/38](38-windows-native-broadcaster.md) — F-12, quinn's datagram-first packing
- [docs/12](12-worker-and-reliable-keyframes.md) — R8, reliable keyframes; Decision 1
- [docs/17](17-viewer-playback-smoothing.md) — R12, the adaptive playout controller
- [docs/15](15-viewer-live-edge.md) — Decision 6, the rejected back-channel
- `BUGS.md` — "Telemetry blames leg B for broadcaster-uplink loss"
