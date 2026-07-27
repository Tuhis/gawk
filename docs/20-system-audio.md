# R15 — System Audio: Opus over Datagrams (experimental)

Design doc for [ROADMAP R15](../ROADMAP.md#r15--system-audio) (designed
2026-07-15; **design refreshed 2026-07-19** against everything landed since —
R16 U4 verdict, R17 scale-out, R18 viewer count, R19 resilient mode, R20 CI,
and the R12 defaults flip — see [Design refresh](#design-refresh-2026-07-19);
**N1–N6 implemented 2026-07-19; hardware playback 2026-07-20/07-26 produced
thirteen field findings — all fixed — including the
Decision 10 inversion to **video-master** A/V sync, the Decision 12 reversal
taking audio back off the R19 reliable carrier, a live-edge audio
buffer-depth floor, honest jitter-buffer depth accounting with worklet-stall
recovery (finding 7 — the crackle-then-silence fix), and the two re-anchor
fixes — leaving Deep buffer (finding 10) and toggling paced playback
(finding 11), both reproduced against the homelab; **finding 13** (2026-07-26)
moved the skew measurement to the listener and root-caused **finding 12**'s
long-session `avSkewMs` over-report with it — a metric bug throughout, audio
was actually near-correct — so a long-session capture confirming finding 12,
and the manual verification pass that reached step 3 of 9 (2026-07-24) then
stopped on test-machine performance trouble, both still need a re-run** —
see [Status](#status)). Adds the broadcaster's
**system audio** to the
stream as an **experimental, default-off** feature: WebCodecs `AudioEncoder`
(Opus) on the broadcaster, one Opus packet per WebTransport datagram through
the relay, WebCodecs `AudioDecoder` + an `AudioWorklet` ring buffer on the
viewer, with "good-enough" A/V sync that composes with the R12 playback
modes (paced playback, frame interpolation) rather than fighting them.

Two user decisions anchor the scope (2026-07-15):

1. **Direction**: Opus via WebCodecs over datagrams (Option A below) — the
   path symmetric with video, live-edge friendly, and richest in learning
   value (WebCodecs audio + AudioWorklet are the new tech being explored).
2. **Sync ambition**: *good-enough* sync, working with frame interpolation
   and the other R12 goodies — not full lip-sync-from-day-one (audio as a
   hard master clock everywhere), and not measure-only-defer-sync either.

Plus the experimental-rollout decision: the broadcaster gets an
**"Enable audio (experimental)" toggle in the advanced settings panel,
default off** — off means `getDisplayMedia` requests `audio: false` exactly
as today, byte-identical behavior; and the viewer surfaces audio controls
**only when audio is actually received** in the stream, never
optimistically. Graduation to default-on is a later, explicit decision that
amends this doc.

**That graduation happened on 2026-07-23** (user decision, after audio was
verified working and reliable on real hardware): the toggle is **removed**,
not flipped — the production broadcaster asks for system audio on every
start. See [Graduation](#graduation-2026-07-23-the-toggle-is-removed).

## Goal

A viewer joining a broadcast hears the broadcaster's game audio, in sync
with the video to within casual-viewing tolerance, without giving up the
project's live-edge philosophy: audio drops are concealed as brief silence,
never as growing delay. The broadcaster opts in per the experimental
framing; a video-only broadcast (toggle off, Firefox broadcaster, or the
user unchecking "share audio" in Chrome's picker) behaves exactly as today
on both ends.

## Background: everything about audio is greenfield

- The stream is video-only by construction: `acquireDisplayStream`
  (`gawk-app/src/media/capture.ts:57`) requests `audio: false`, and there
  are zero other audio references in either codebase.
- The relay **whitelists datagram types**: `Publisher.HandleDatagram`
  (`gawk-server/internal/hub/hub.go`) dispatches on byte 1 and drops unknown
  types as bad. Audio is blocked at the relay until a dispatch case lands —
  this is a wire + server change, unlike the recent client-only items.
- The viewer's decoded media **never crosses the worker→main boundary**
  (R8/R10: frames are drawn in-worker on a transferred `OffscreenCanvas`).
  Audio playback needs an `AudioContext`, which is main-thread-only — so
  audio introduces the first decoded-media crossing, by design (Decision 7).
- Platform facts that shape the design:
  - **Audio frames are tiny.** Opus at 48 kHz stereo, 128 kbps, 20 ms
    frames ≈ 320 B/packet, 50 packets/s. Every packet fits a single
    datagram with the 16-byte header — **no chunking, no reassembly, no
    reliable streams, no keyframe concept**.
  - **System-audio capture is Chromium-only.** `getDisplayMedia` audio with
    a whole-screen share delivers system audio on Windows (the gaming-PC
    happy path) and ChromeOS, tab audio elsewhere; Firefox returns no audio
    track at all. Video-only is therefore a first-class graceful state.
    *(Field finding 1, 2026-07-20: "graceful" was an assumption, not a
    guarantee. Where no system-audio source can start, Chromium rejects the
    **whole** `getDisplayMedia` request — video included — rather than
    granting a video-only stream, and that killed the broadcast outright.
    Two causes behind one exception: platform, and Windows device state.
    `capture.ts` now retries once without audio.)*
  - `docs/15` anticipated this: *"when audio lands, its clock story should
    build on Q2's relay-clock mapping"* — and it does (Decision 10). The R6
    viewer UI reserved a volume-control slot (docs/10 non-goals).

## Direction survey (settled 2026-07-15)

| Option | Sketch | Verdict |
|--------|--------|---------|
| **A — Opus/WebCodecs over datagrams** | `AudioEncoder` → one datagram per Opus packet → `AudioDecoder` → `AudioWorklet` | **Chosen** — symmetric with video, live-edge, ~128 kbps, most learning value |
| B — Opus over a reliable uni stream | One long-lived stream per subscriber, length-prefixed packets | Rejected: head-of-line stalls on loss (needs a *bigger* jitter buffer), breaks drops-over-stalls, and a continuous per-subscriber stream fan-out with backpressure is a genuinely new server mechanism (the R8 keyframe path is store-and-forward per message, not continuous) |
| C — Raw PCM datagrams | 5 ms 48 kHz stereo 16-bit frames (960 B), no codec | Rejected: 1.54 Mbps/viewer (~23 Mbps at 15 viewers — affordable but pointless), 200 dgrams/s, and every loss is an audible click unless we hand-write concealment that Opus's design gives us structurally |
| D — MediaRecorder + MSE | Container chunks → `<audio>` via MediaSource | Rejected: hundreds of ms of container/timeslice/MSE buffering, architecturally alien to the pipeline |

## Decisions

1. **One Opus packet per datagram; datagrams only.** Audio is deltas-only
   media — every packet is independently decodable-ish and tiny, so the
   video path's hardest problems (chunking, reassembly, keyframe
   reliability, reorder-to-keyframe resync) simply don't exist for audio. A
   lost packet is a 20 ms gap concealed as silence (Decision 8). Encoder
   config: Opus, 48 kHz, 2 channels, **128 kbps** constant default, 20 ms
   frames, DTX **off** (a constant packet rate keeps gap detection and the
   buffer clock trivial; DTX is a bandwidth optimization we don't need at
   128 kbps). Bitrate is a named constant, not a setting, in v1 — the
   advanced-panel override can come with graduation. Anything ≤ ~470 kbps
   fits the 1184-byte payload budget; the constant stays far under it.

2. **Wire: `TypeAudioFrame` = 0x07, `TypeAudioConfig` = 0x08** (0x01–0x06
   were taken at design time; since then R17 took 0x09 `TypeResumeToken`,
   R19 took 0x0A `TypeReliableCarrier` (a stream-kind discriminator) and R18
   took 0x0B `TypeViewerCount` — all three honored the 0x07/0x08 reservation,
   which `wire.go` records in a comment, so the allocation stands unchanged),
   mirrored byte-identically in Go (`gawk-server/wire`) and TS
   (`transport/wire.ts`) with golden test vectors on both sides, like every
   existing message.

   ```
   0x07 AudioFrame (AudioFrameHeaderSize = 16 bytes of header, then payload):
     byte 0       uint8   version (0x01)
     byte 1       uint8   type (0x07)
     byte 2       uint8   flags (0, reserved)
     byte 3       uint8   reserved (0)
     bytes 4-7    uint32  seq          audio's own sequence space
     bytes 8-15   uint64  timestampUs  broadcaster performance.now() µs clock
     bytes 16+    payload: exactly one Opus packet, ≤ 1184 bytes

   0x08 AudioConfig:
     byte 2       uint8   reserved (0)
     byte 3       uint8   codecLen
     bytes 4..    codecLen bytes ASCII codec string ("opus")
     next 4       uint32  sampleRate
     next 1       uint8   channels
     rest         description (optional; empty for opus)
   ```

   `seq` is a fresh uint32 space independent of video frameIDs; comparisons
   reuse the serial-arithmetic helpers (`wire.frameIdAhead`/`nextFrameId`
   and their TS mirrors) so wrap is handled the same way video learned to
   in R10.

3. **All audio timestamps live on the broadcaster's `performance.now()` µs
   clock — the same clock video capture stamps.** This is the load-bearing
   decision for sync: with one clock on both media, the entire R5 machinery
   (ClockMapping, TimeSync, live-edge drift) applies to audio *unchanged*,
   and A/V skew is a subtraction, not a negotiation. Mechanically the audio
   pump anchors once (`anchor = performance.now()µs − firstAudioData.timestamp`)
   and stamps `anchor + AudioData.timestamp` — the audio media clock
   provides drift-free spacing, the anchor pins it to the shared wall
   clock. If anchor drift vs. `performance.now()` ever exceeds a bound
   (~50 ms), re-anchor and let the viewer buffer absorb the step (expected
   ~never within a session; the check is cheap insurance).
   - *Cluster note (2026-07-19)*: broadcaster-clock timestamps are
     pod-independent, so in R17 cluster mode audio needs **no per-hop
     rewrite** — the edge re-ingests audio datagrams verbatim (like
     ViewerCount, unlike ClockMapping, which is rewritten per hop). A/V skew
     stays a broadcaster-clock subtraction on any serving pod, and the
     per-hop ClockMapping rewrite serves audio's absolute-latency story
     exactly as it serves video's.

4. **Relay: two dispatch cases, one new cache slot, zero new mechanisms.**
   In `HandleDatagram`: `TypeAudioFrame` → strict-parse header + verbatim
   `relayDatagram` fan-out (payload opaque, like everything else);
   `TypeAudioConfig` → strict-parse + copy into a new `cachedAudioConfig` +
   fan out. The cache is join-primed in `subscribe` alongside
   `cachedClockMapping` and `cachedViewerCount` (R18 turned the lifecycle
   into a reusable template) and invalidated at **both** sites the lifecycle
   now has *(amended 2026-07-19 — the second site postdates this design)*:
   `StartPublish` (new publisher session) **and** `InvalidatePrimes` (R17
   W4, edge upstream loss — skipping it would serve origin A's audio config
   beside origin B's packets, exactly the staleness class docs/22
   Decision 10 closes). Audio rides the existing per-subscriber queues,
   bandwidth cap, and drop paths untouched — they are media-agnostic.
   - **Cluster mode works with zero edge code** *(2026-07-19)*: the edge
     pump (`transport/edge.go`) re-ingests every datagram it doesn't
     special-case (TimeSync, ClockMapping) verbatim through the edge hub's
     own `Publisher.HandleDatagram` — so N1's dispatch cases *are* the
     cluster support; the edge then caches, primes, and fans audio for free.
     Audio's trust model mirrors DecoderConfig (publisher-sourced, accepted
     on origin and edge hubs alike), **not** ViewerCount's
     relay-peers-only spoof guard — on an edge the "publisher" is the
     upstream origin, which is exactly who should be speaking.
   - **Version skew** *(2026-07-19; docs/22 makes skew tolerance mandatory)*:
     a pre-N1 pod — an old relay behind a new broadcaster, or an old edge
     pulling a new origin mid-rollout — drops each audio datagram as
     unknown-type bad. Viewers there degrade to video-only (the designed
     graceful state), but `datagramsBad` ticks at the audio packet rate
     (~50/s). Accepted as transient rollout noise; know it before reading
     that counter mid-upgrade.
   - **Audio never touches the video ingress-loss window** (`hub/ingress.go`
     assumes a single frameID space; interleaving audio seqs would read as
     massive loss). v1 adds no audio ingress window — the viewer's own gap
     counter (Decision 8) is the loss signal. `framesRelayed` (and its
     Prometheus family) stays video-only so relay-fps keeps meaning
     video-fps; audio folds into the generic datagram/byte counters.

5. **Config delivery is lossy-tolerant by repetition.** Video re-emits its
   cached DecoderConfig before every keyframe's chunk 0; audio has no
   keyframes to anchor to, so the broadcaster **re-sends AudioConfig at
   1 Hz** (a ~15-byte datagram; idempotent on the viewer — reconfigure only
   when it differs). Late joiners additionally get the hub's cached copy on
   subscribe. Both paths existing patterns, no new protocol.

6. **Broadcaster: audio is a parallel lane in `BroadcastPipeline`, gated by
   the experimental toggle at capture time.**
   - Toggle **on** → `getDisplayMedia` requests
     `audio: { echoCancellation: false, noiseSuppression: false,
     autoGainControl: false }` + `systemAudio: 'include'` and
     `suppressLocalAudioPlayback: false` (game audio is program material,
     not voice — processing off; the broadcaster keeps hearing their game).
     Toggle **off** → `audio: false`, exactly today's call.
   - **No audio track in the granted stream is a state, not an error**:
     the pipeline runs video-only and the UI annotates "no audio shared".
   - The audio lane: audio `MediaStreamTrackProcessor` → timestamp anchor
     (Decision 3) → `AudioEncoder` → `encodeAudioFrame` datagram →
     the existing sender. It mirrors `media/encoder.ts`'s wrapper shape and
     lives behind the same media-source seam; an audio-encode error tears
     down the *audio lane only* (annotated), never the broadcast.
   - **R11 worker path**: the audio `track.clone()` transfers alongside the
     video clone in `workerBroadcastSession.provideCapture()` (teardown
     already iterates all tracks); the worker boot handshake gains an
     `AudioEncoder` probe. A worker that lacks it → **video-only with an
     annotation**, never a pipeline-placement change — audio must not
     demote the whole broadcast to the main thread (theoretical on
     Chromium, where workers have WebCodecs audio wherever the main thread
     does).
   - **The toggle applies on the next broadcast start** — flipping it
     mid-stream cannot conjure an audio track without a new
     `getDisplayMedia` picker prompt, and R13's "no settings change ever
     restarts the stream" rule wins. The panel annotates this ("applies
     when the broadcast starts"). This is the one deliberate exception to
     R13's live-apply story, forced by the capture API, and it's why the
     toggle lives in the advanced panel rather than beside the live pickers.

7. **Viewer: decode in the worker, play on the main thread.** The demux
   point is `reassembler.ts` (new `TYPE_AUDIO_FRAME`/`TYPE_AUDIO_CONFIG`
   cases → `onAudioFrame`/`onAudioConfig` callbacks — the transport worker
   needs zero changes, datagrams are opaque to it). `AudioDecoder` runs in
   the viewer worker beside the video decoder; decoded `AudioData` posts to
   the main thread as a **transferred** object on a new `ViewerWorkerEvent`
   — the first decoded-media crossing, and a deliberate one: `AudioContext`
   cannot exist in a dedicated worker, and `AudioData` transfer is a
   zero-copy move of ~7.7 KB per 20 ms. The main-thread-pipeline fallback
   decodes and plays entirely on the main thread via the same sink.
   *Rejected alternative*: decoding on the main thread (posting encoded
   packets up) — it would put codec work back on the thread R8/S6 spent
   effort clearing, for no simplification (the crossing exists either way).

8. **Playback = an `AudioWorkletProcessor` ring buffer with live-edge
   discipline.** The main-thread sink owns an `AudioContext` (48 kHz) and a
   worklet node; decoded PCM is written into a ring keyed by timestamp:
   - **seq gap** → silence for the missing duration, `audioGapsConcealed++`;
   - **packet older than the playhead** → dropped, `audioLateDrops++`
     (delay never grows to accommodate stragglers);
   - **underrun** → silence, `audioUnderruns++`;
   - **buffer target** is adaptive (Decision 10), overflow beyond it drops
     oldest — drops over stalls, same philosophy, third medium.
   - **No Opus in-band FEC/PLC in v1**: WebCodecs `AudioDecoder` exposes no
     packet-loss hook to exploit them, so FEC would spend bitrate nothing
     can read. Concealment = silence insertion. Revisit only if the gap
     counters say gaps are audible in practice (deferred, recorded here).
   - **Restart/reconnect → flush + re-anchor** *(added 2026-07-19 — R17
     made mid-view session churn routine, so this can't stay implicit)*:
     the sink resets (flush the ring, drop the playhead anchor) on the same
     restart/resync signal the video path already derives (R10's
     backwards-keyframe / reorder-restart machinery). A broadcaster
     **auto-resume** (R17 W2: capture + encoder kept, transport-only
     reconnect) keeps both the seq space and the `performance.now()` origin
     — audio flows through seamlessly, no reset. A genuine broadcaster
     **restart** resets both, and without a flush every new packet would
     read "older than the playhead" and be late-dropped forever. A *viewer*
     reconnect (4002 drain, abrupt loss) rebuilds the transport: the new
     session re-primes AudioConfig from the hub cache and the sink flushes
     and re-anchors like any restart.

9. **Autoplay + conditional controls.** The `AudioContext` is created on
   the viewer's join click (a real user gesture); if the context still
   reports `suspended` (browser policy edge), the viewer shows a
   tap-to-unmute affordance rather than failing. Mute/volume land in the
   fading viewer controls + right-click menu (`gawk:muted`, `gawk:volume`
   localStorage keys — filling the slot R6 reserved), **rendered only when
   `audioPresent` is true** — set on the first AudioConfig/audio frame
   observed, cleared on restart-without-audio. A video-only stream shows
   exactly today's UI. Mute pauses the *sink output*, not the pipeline
   (stats keep flowing; unmute is instant). Volume is a `GainNode` on the
   main thread — no worker crossing.
   - *iOS note (2026-07-19, post-R16)*: iPhone viewers are first-class now
     (the worker pipeline was confirmed on-device), and the U4 verdict made
     CSS **pseudo-fullscreen the shipping path** — good news for audio: the
     fading controls and right-click menu (incl. mute/volume) stay reachable
     in fullscreen, where the rejected native player would have hidden them
     by construction. iOS autoplay policy is stricter than desktop: expect
     **tap-to-unmute to be the normal first-play path there**, not a rare
     edge — the affordance is mainline UI, and the manual verify gets an
     iPhone pass.

10. **A/V sync: shared clock + adaptive audio buffer + audio-master pacing
    where pacing already exists.** **SUPERSEDED 2026-07-20 — see field
    finding 4.** The audio-master half below was inverted by owner decision:
    video is the master clock and is never rescheduled for audio; audio is
    delayed at start to meet the video presentation schedule, and residual
    drift is absorbed by a sub-audible rate trim. The shared capture clock,
    the always-on skew measurement and the adaptive buffer envelope all
    survive unchanged; every sentence below about audio driving
    `displayTargetMs` does not. Kept as written for the record.
    Good-enough, by construction, in three
    layers:
    - **Skew is measured, always**: `avSkewMs` = (video frame timestamp
      presented at the last paint) − (audio timestamp at the playhead),
      both on the broadcaster clock (Decision 3). New `ViewerStats` fields
      + an overlay Audio section row. The audio sink reports
      `{playheadTimestampUs, bufferedMs, underruns, contextTime}` to the
      viewer worker at ~4 Hz over the existing command channel (the reverse
      direction of the stats flow — a tiny, non-media message).
    - **Live-edge default (playout mode `off`)**: video stays undelayed —
      that philosophy does not bend. The **audio jitter-buffer target** is
      the adaptive knob: `clamp(arrivalJitterP95 − min + headroom, 40, 150) ms`
      reusing the R12 `PlayoutController` clamp/slew pattern and the
      `WindowedQuantileTracker` (T1) over audio arrival deltas. Result:
      video typically leads audio by roughly the audio buffer depth
      (~40–80 ms on a clean link) — comfortably inside casual tolerance,
      and video-leads is the perceptually forgiving direction.
    - **Paced modes (`fixed`/`adaptive`) with audio present**: **audio
      becomes the master clock.** *(INVERTED 2026-07-20 — field finding 4.
      Video is the master in every mode now, and audio aligns to it. This
      bullet is the one that changed; the rest of Decision 10 survives.)* The video `displayTargetMs` derives from
      the audio playhead mapping (playhead timestamp ↔ `AudioContext`
      time ↔ `performance.now()`) instead of the reorder buffer's arrival
      baseline; the mapping updates at 4 Hz and is slew-limited so video
      targets never step. Audio absent/suspended/muted-at-context → fall
      back to the arrival baseline (exactly today). `PacedPresentationSink`
      itself is untouched — only the *source of targets* changes, which is
      precisely the seam R12 built. *(2026-07-19: the R12 defaults flip —
      same day as this design, docs/17 Decision 8 as superseded — makes
      `adaptive` + interpolation the production viewer's defaults, so
      audio-master pacing is the **mainline** production path, not an
      opt-in corner. Live-edge `off` remains reachable via the right-click
      menu and stays the `#/debug/*` default; its never-delay-video
      guarantee is unchanged. N5's manual measurement priorities follow:
      default-mode skew first.)*
    - **Resilient mode (R19) with audio present**: governed by Decision 12
      — the audio buffer adopts the resilient envelope so audio-master
      pacing works at resilient depth instead of collapsing it.
    - **Frame interpolation is unaffected by construction**: it synthesizes
      mid-slot frames below the pacing layer, so audio-derived targets feed
      it the same way arrival-derived targets do today. Stating this is an
      acceptance criterion, not a hope (N5).
    - **Numbers**: target median |avSkewMs| ≤ 60 ms, p95 ≤ 120 ms across
      Chrome + Firefox viewers on the reference LAN (casual lip-sync
      noticeability ≈ ±80–100 ms; video-lead is the tolerated sign).

11. **Bandwidth and limits**: 128 kbps + 16 B × 50/s header ≈ 134 kbps per
    viewer — ~2 Mbps total at 15 viewers, noise against the video budget.
    Audio datagrams count against the R2 global bandwidth cap and the
    generic datagram counters (they are datagrams; special-casing them out
    would falsify the cap). No new relay knobs.

12. **R19 resilient-mode interplay** *(added 2026-07-19 — R19 postdates
    this design)*. **⚠️ The carrier-routing half of this decision was
    REVERSED 2026-07-20 by [field finding 5](#field-finding-5-2026-07-20-audio-off-the-r19-carrier)
    — audio no longer rides the carrier. The paragraph below records the
    original reasoning; the profile-carrying half (the last two bullets)
    stands.** For a `?delivery=reliable` subscriber the drain writes
    **everything its fan-out queue carries** as length-prefixed records on
    the per-GOP carrier stream and never calls `SendDatagram`
    (`drainReliable`, docs/24 Decision 5) — so audio automatically rides
    the carrier with **zero additional relay code**, and that is the right
    behavior, not an accident to defend against:
    - Audio loss on the lossy leg is healed by QUIC retransmission —
      resilient viewers get *fewer* concealment gaps, which is the mode's
      entire point. In exchange audio shares the carrier's drop point: a
      dead/rotated carrier drops its GOP tail *including* the audio records
      in it (drops-over-stalls at GOP granularity, third medium).
      *(This premise did not survive hardware: on a real link the GOP-tail
      drop + head-of-line blocking behind video-delta records broke audio up
      worse than concealed single-packet datagram loss — field finding 5.
      Audio now takes the unreliable datagram path even for a resilient
      subscriber; only video deltas ride the carrier.)*
    - Viewer side: carrier records feed the existing datagram path
      (`readServerStreams` → same demux), so N4's reassembler cases serve
      both delivery modes unchanged. No audio-specific carrier code.
    - **The audio jitter-buffer target becomes profile-carrying, like
      `PlayoutController`** (same live-getter-on-the-resilient-flag pattern,
      `transport/playout.ts` / `resilient.ts`): default clamp [40, 150] ms
      as designed; resilient mode adopts the video playout envelope
      (clamp **[150, 2000] ms, seed 500**) so the audio playhead sits at
      resilient depth. *(Field finding 4 keeps this conclusion and changes
      its reason: the envelope no longer governs video at all, but it sizes
      the depth-floor fallback and the overflow ceiling, both of which must
      match a resilient link.)* Without this, Decision 10's audio-master pacing
      would drag video targets back to the ≤150 ms audio playhead and
      **structurally collapse the resilient buffer the moment audio
      appears** — the one place the original design and R19 genuinely
      conflict. Entering/leaving resilient mode is already a deliberate
      reconnect (docs/24 Decision 9), which is exactly a sink
      flush + re-seed boundary (Decision 8).
    - Origin→edge stays datagrams (`SubscribeInternal` is never reliable —
      docs/24 scale-out interop); nothing audio-specific there either.

## End-to-end path

```
Broadcaster (toggle on, Chromium):
  getDisplayMedia(audio:{...})            main thread, one grant with video
    └─ audio track.clone() ──transfer──▶ broadcast worker (R11 path)
         MSTP(audio) → AudioData
         → timestamp anchor (perf.now µs clock, Decision 3)
         → AudioEncoder (opus 48k/2ch/128kbps/20ms, DTX off)
         → encodeAudioFrame (0x07, seq++) → datagram send
         + AudioConfig (0x08) at start + 1 Hz re-send

Relay:
  HandleDatagram: 0x07 validate → fan out verbatim
                  0x08 validate → cachedAudioConfig + fan out
  Subscribe: prime cachedAudioConfig (with the ClockMapping/ViewerCount
             primes, before the keyframe)
  Invalidate: StartPublish (new session) + InvalidatePrimes (edge upstream loss)
  (R19 reliable subscriber: same bytes ride the per-GOP carrier as records;
   cluster edge: re-ingested verbatim through the edge hub — Decisions 4/12)

Viewer:
  transport worker (unchanged, opaque datagrams)
  → viewer worker: reassembler demux 0x07/0x08
      → AudioDecoder → AudioData ──transfer──▶ main thread
  main thread: AudioWorklet ring buffer (adaptive 40–150 ms target)
      gaps→silence, late→drop, GainNode volume, mute = output pause
      ── 4 Hz playhead report ──▶ viewer worker (skew metric; master
                                   clock for paced video modes)
```

## Design refresh (2026-07-19)

Review of this not-yet-started design against everything that landed after
it was written (R16 U4, R17 W1–W6, R18, R19, R20, and the R12 defaults
flip). The direction and chunk structure survive intact; the amendments are
folded into the decisions above and the N-table criteria below:

- **Wire allocations confirmed** (Decision 2): 0x09/0x0A/0x0B went to
  R17/R19/R18 respectively; 0x07/0x08 stayed reserved for R15.
- **Cluster mode (R17)** is nearly free (Decisions 3, 4): the edge
  re-ingests unhandled datagrams verbatim through its own
  `Publisher.HandleDatagram`, so N1's dispatch cases are the cluster
  support; audio timestamps are pod-independent (no per-hop rewrite). The
  one real addition: `cachedAudioConfig` must also be cleared in
  `InvalidatePrimes` (edge upstream loss) — an invalidation site that did
  not exist when Decision 4 said "invalidated in `StartPublish`". Plus a
  recorded version-skew behavior (pre-N1 pods count audio bad at ~50/s,
  viewers there degrade to video-only).
- **Session churn is routine now (R17)** (Decision 8): explicit sink
  flush/re-anchor policy on restart/resync and viewer reconnect;
  broadcaster auto-resume needs no reset (seq + clock continue).
- **R19 resilient mode** (new Decision 12): audio rides the reliable
  carrier automatically (desired); the audio jitter buffer becomes
  profile-carrying — resilient mode adopts the [150, 2000] ms / seed 500
  envelope so audio-master pacing doesn't collapse the resilient buffer.
  This was the only genuine conflict found. *(Post-field-finding-4: the
  envelope survives, now sizing audio's own fallback depth floor rather
  than anything video-side.)*
- **Defaults flip (R12, docs/17 Decision 8 superseded)** (Decision 10):
  adaptive pacing + interpolation are the production defaults, so
  audio-master pacing is the mainline path; verification re-ordered
  accordingly. *(Post-field-finding-4: pacing is video-master in every
  mode, so which mode is default no longer changes who the master is —
  only how much offset video carries. The verification ordering stands.)*
- **iOS (R16 U4)** (Decision 9): pseudo-fullscreen shipping keeps audio
  controls reachable in fullscreen; tap-to-unmute is the expected normal
  path on iPhone; manual verify gains an iPhone pass.
- **Native broadcaster (R14 as shipped)**: the non-goal's "ffmpeg path"
  wording corrected — R14 shipped a GStreamer subprocess, so the future
  audio lane there is a GStreamer element chain emitting the same wire
  messages from the engine.
- **CI (R20)**: `gawk-pubsim` is video-only, so tier-1 `e2e` now
  continuously asserts the no-audio path N1 must keep intact; an
  audio-capable fixture is out of scope (possible later stretch).

## Status

**Implementation status (2026-07-24)**: N1–N6 implemented; automated gates
green in all three modules (`gofmt`/`go vet`/`go test -race`, `npm test` +
`lint` + `build`, `helm lint`). Audio now plays reliably on real hardware
(owner-verified 2026-07-23) — which graduated it from experimental — but the
formal browser verification plan below still needs a full re-run (the
2026-07-24 attempt reached only step 3 of 9 before a test-machine performance
problem stopped it). Getting there took **twelve field findings, eleven
fixed**: **1** (the audio toggle failed the whole broadcast, before a single
Opus packet was encoded); once audio did play, **2** (video froze — two clocks
driving one pipeline) and **3** (the jitter buffer never buffered); **4**
(video-master inversion) and **5** (audio off the R19 carrier); **6**
(live-edge starved for buffer depth); **7** (the depth estimate counted
undelivered audio → crackle-then-silence); **8** (overflow drops and gap
concealment fought each other on Safari); **9** (`avSkewMs` measured buffering
depth + estimator lag, not lip-sync); **10** (leaving Deep buffer stranded
audio ~2.8 s behind) and **11** (toggling paced playback left audio behind).
**Finding 12** (`avSkewMs` still over-reports on long/stressed sessions — a
metric artifact; audio itself is fine) is **open, deferred**. Deviations and
decisions taken during implementation:

1. **Decoded audio crosses as planar PCM, not a transferred `AudioData`**
   (Decision 7 said the latter). The sink must reach an `AudioWorklet`,
   which needs Float32 planar channels, so `copyTo()` has to happen
   somewhere regardless; doing it in the worker keeps the copy off the main
   thread (the one R8/R10 cleared) and sends only plain `ArrayBuffer`s in
   the transfer list. Same "no structured clone of media" property, one
   fewer API assumption (`AudioData` transferability).
2. **The AudioWorklet processor ships as a source string → Blob URL.**
   `addModule` takes a URL, and a `?url` import of a `.ts` file would serve
   raw TypeScript. The Blob keeps the processor in the same module as the
   sink instead of adding a build entry.
3. **The timeline-change threshold is asymmetric** (Decision 8 implied one
   bound): a backwards jump past 1 s is a restart, a forward hole past 5 s
   is a reconnect. A symmetric bound made a short-session restart read as
   lateness and late-drop forever — the R10 docs/14 finding, third medium.
   Found by the unit test, not by inspection.
4. **The audio buffer's profile re-seed is keyed on profile identity, not
   value range.** The default `[40, 150]` and resilient `[150, 2000]`
   envelopes overlap at 150 ms, so a clamp check silently kept the old
   profile's target across a resilient flip (Decision 12's whole point).
5. **`avMaster` was added to `ViewerStats`** beside `avSkewMs`: which clock
   is actually driving display targets is ground truth worth showing, so a
   fallback to the arrival baseline is visible rather than inferred.
6. **The R15 golden vectors are mirrored into `gawk-broadcast`'s
   `wirecheck`** even though the native broadcaster sends no audio (still a
   non-goal): docs/19 Decision 11 makes that mirroring a rule, and R19's
   carrier vectors set the precedent.
7. **Audio UI requires audio to be *playable*, not merely present**: a
   scope without `AudioContext`/`AudioWorkletNode` shows no controls (they
   would be dead) and reports `audioState: 'unsupported'`.

**Status update (2026-07-23, owner)**: with findings 1–9 fixed, audio is
**verified working and reliable on real hardware** — which is what made the
graduation below an explicit decision rather than a guess.

**Verification session (2026-07-24)**: a pass through the verification plan
below, driven against the homelab deployment (Chrome/macOS, tab-audio
substitute per finding 1). Reached **step 3 of 9, then stopped** — the test
machine developed performance trouble that starved the audio worklet and made
further absolute-timing measurements untrustworthy. Outcome:

- **Step 1** (audio-off baseline) is now **obsolete** — the graduation below
  removed the "Enable audio (experimental)" toggle, so there is no audio-off
  state to test on the production broadcaster. The step needs rewriting.
- **Step 2** (audio ON, happy path): audio plays; `avMaster` = video (audio
  aligned), zero wire loss, `overflowDrops`/`resets` low. Two re-anchor
  defects surfaced and were fixed on the way — **[finding 10](#field-finding-10-2026-07-23-leaving-deep-buffer-stranded-audio-28-s-behind)**
  (leaving Deep buffer) and a coherent/re-announced **DeliveryAck** (relay).
- **Step 3** (playout-mode sweep): surfaced and fixed
  **[finding 11](#field-finding-11-2026-07-23-toggling-paced-playback-left-audio-behind)**
  (toggling paced playback did not re-anchor audio).
- **[Finding 12](#field-finding-12-2026-07-24-avskewms-still-over-reports-on-longstressed-sessions--open-deferred)**
  opened: `avSkewMs` still over-reports on long/stressed sessions (a residual
  of finding 9). Left OPEN, deferred — audio is actually near-correct; it is a
  metric-reporting bug, and a clean drift run on a healthy machine is the
  prerequisite for characterising and fixing it.
- **Steps 4–9** (30-min drift, loss/stress, resilient interplay, graceful
  states, autoplay/iPhone) **not run.** Findings 10 and 11 landed as merged PRs
  (#123, #125); their fixes are viewer-only, test-first, zero
  wire/relay/broadcaster change, but reach `gawk.ioio.fi` only once a release
  ships. **The plan still needs a full re-run on a healthy machine**, starting
  with a rewritten step 1.

### Field finding 1 (2026-07-19): the audio toggle killed the broadcast

**First real-hardware attempt, and it never reached the audio code at all.**
Broadcaster with the toggle on, "Share system audio" checked in the picker:

```
BroadcastStartError: Could not start audio source
Caused by: NotReadableError: Could not start audio source
```

`getDisplayMedia` audio is **not best-effort in Chromium**. When the browser
cannot start a system-audio source it does not hand back a video-only
stream — it rejects the *entire* request with `NotReadableError`, video
included. Decision 6 assumed the only degraded shapes were "toggle off" and
"grant carried no audio track"; the grant *failing* was the unhandled third,
and it turned an experimental, default-off toggle into a broadcast-killer
(phase `'capture'`, so the publisher session was torn down and the broadcast
never started).

**This is the structural finding, and it is platform-independent.** The
environment half is not: the first triage here assumed the signature meant
Chromium-on-Linux (which genuinely has no loopback path, as do macOS
screen/window shares) — but the affected machine is **Windows + Chrome
sharing the entire screen**, the exact configuration the verification plan
below targets and one where system audio *is* supported. So there are two
independent failure classes behind one identical exception:

| Class | Where | Fix |
|---|---|---|
| **Platform** — no OS loopback path at all | Linux, macOS (screen/window shares) | none; share a tab instead |
| **Device state** — loopback path exists, the endpoint won't open | Windows | fix the default output endpoint |

Windows candidates for the second class, roughly by likelihood: the audio
checkbox was checked while a **window** (not the entire screen) was
selected; the **default render endpoint can't be opened for loopback** —
held in WASAPI exclusive mode by a game or audio app, disconnected or asleep
(HDMI/TV endpoints on a multi-output gaming PC are the classic), or a
virtual device (Voicemeeter, VB-Cable, streaming "speakers") that refuses
loopback; or a wedged Chrome audio service. `chrome://media-internals`
(Audio tab) while reproducing shows the actual device-open error, and
**tab audio is the discriminator**: it rides Chrome's internal mirroring
path rather than OS loopback, so tab-audio-works + system-audio-fails
isolates it to the endpoint. Root cause on the affected machine is **not
yet identified** — open item for the manual verification pass.

(Signature matches jitsi/jitsi-meet#15417/#15418, closed stale, OS
unspecified — with the same "sharing a tab works flawlessly" observation.)

Fix (test-first, `src/media/capture.test.ts` red first):

- `acquireDisplayStream` retries **once without audio** when an audio-bearing
  grant fails with `NotReadableError` — and only then. A cancelled or denied
  picker (`NotAllowedError`) must never re-prompt, the R1 lesson about
  distinguishing "the server said no" from "the user said no" applied to
  capture.
- The retry needs its own transient activation, which the seconds spent in
  the picker have usually already spent — so the retry commonly cannot
  re-prompt either. That path throws the **original** audio cause plus the
  way out ("turn off Enable audio (experimental)"), never the retry's
  misleading activation complaint.
- A grant that fell back reports `audioState: 'unavailable'` ("Not capturable
  here" on the overlay), *not* `'no-track'` — the latter reads as "the user
  left the picker's audio box unchecked" and would send the next debug
  session down the wrong path. The flag rides the media-source seam, so the
  worker path (`capture` command) reports it identically.
- The advanced-settings note now states the platform matrix next to the
  toggle, so the trap is visible before the picker.

Verification-plan consequence: steps 2–4 need audio to actually flow, and on
the affected machine a screen share cannot supply it yet. **Run them with a
shared tab** ("share tab audio") — that exercises the entire R15 path
(encode → 0x07/0x08 → viewer worklet) and is unblocked today; the endpoint
triage above is a separate, environment-side task. A screen share that
refuses audio is now a graceful video-only broadcast, itself worth
confirming (`audioState: 'unavailable'`).

### Field findings 2 & 3 (2026-07-20): the first time audio actually played

Broadcaster on a Windows machine with a standard audio setup (the field
finding 1 machine's virtual-audio-device stack was the reason nothing
started there — environment, not gawk). Audio reached a viewer for the first
time, and two things went wrong at once:

> as soon as the viewer enabled the audio, video froze (almost) completely
> AND the audio wasn't clean — there were frequent breaks.

Two independent bugs, both invisible to a green suite because each lives in
the seam between two components that were unit-tested apart.

**Finding 2 — video froze: two clocks driving one pipeline.** N5 moved the
*display target* onto the audio playhead (`audioDisplayTargetMs`) but left
the reorder buffer's *release gate* on the arrival baseline. The two
schedules differ by exactly the audio path's extra latency (jitter buffer +
worklet queue + `AudioContext` output latency — a few hundred ms, and always
positive: audio carries buffering the video *minimum* does not). So every
frame reached `PacedPresentationSink` that far before its display slot. The
sink holds `MAX_HELD_FRAMES = 3` and drops the **oldest** over capacity —
i.e. it discarded each frame just as its slot was about to come due, forever.
Not a partial stutter: a total freeze, starting the instant the audio clock
went live, which is precisely when the viewer enabled audio.

`reorder-buffer.ts` already carried the invariant in a comment — the display
target must come "from the same baseline the release gate uses" — and R15
broke it without the comment or a test noticing. The fix restores one
schedule for the whole pipeline: `av-sync.ts` exposes `audioBaselineMs()` (the
audio-derived analogue of `arrivalBaselineMs()`; algebraically identical to
the old display-target formula, since the mapping extrapolates at 1× rate),
and both the release gate and the display target read
`audioBaseline ?? arrivalBaseline`. Live-edge mode is untouched (offset 0 ⇒
release immediately ⇒ audio never delays video, Decision 10 layer 2).

Known, accepted transition: when the audio clock appears or goes stale the
release schedule steps by that same difference — a one-time hold or burst,
bounded and self-healing. Consequence worth stating plainly: **in paced
modes, enabling audio adds the audio path's depth to video latency.** That is
what "audio is the master clock" means; a viewer who wants the old number
switches to live-edge.

**Finding 3 — audio broke up: the jitter buffer never buffered.** Decision 8
specifies an adaptive target of 40–150 ms, and `AudioJitterBuffer` implements
it — as an overflow **ceiling** only. `push()` forwarded every chunk to the
worklet the moment it decoded, so the worklet's queue sat at ~0 ms: it drains
in real time and was fed in real time, with a reflecting barrier at zero
(underruns discard time and can never be won back). Any jitter at all ran it
dry. A target that is only a ceiling is not a jitter buffer.

The fix makes the target a floor as designed: chunks accumulate until the
cushion reaches `targetMs`, then release in order; the sink starts playing
with the target in hand. Re-armed on a genuine underrun (the sink is still
dry when its ~4 Hz report lands — a buffer that already recovered must not
pay a second silence). Concealment silence routes through the same gate, or
it overtakes the audio it was filling behind — caught by the existing gap
test, which is the second time this pass that a pre-existing test earned its
keep.

Both fixed test-first (red first: 4 failing tests across
`reorder-buffer.test.ts` and `audio-buffer.test.ts`). Three existing tests
asserted the *absence* of a cushion as an incidental detail and were updated
to prime first, with their real assertions intact.

**Still unverified**: this is a code-reading diagnosis matched to reported
symptoms, fixed under unit tests. Re-verification on hardware is the next
step — expect `avSkewMs` inside its targets, `underruns` near zero,
`bufferedMs` sitting at the target rather than ~0, and video presenting at
source fps. The overlay's Audio section separates the two remaining
break causes that this fix does *not* address: `gapsConcealed` rising means
datagram **loss** (no FEC in v1 — Decision 11), `underruns` rising means the
cushion is still too thin.

### Field finding 4 (2026-07-20): Decision 10 inverted — video is the master

Owner decision, taken on the finding-2 fix: **video is the master clock and is
never rescheduled for audio.** Audio waits for video, not the reverse.

The reasoning is asymmetry of slack. Audio here is one Opus packet per
datagram — no chunking, no reassembly, no keyframe wait, trivial decode.
Video pays every one of those plus the playout offset, and its keyframes ride
store-and-forward streams that can land hundreds of ms late. Audio therefore
arrives materially earlier, and holding it is nearly free, while holding video
costs the thing the project exists to minimize. Finding 2's fix made both ends
of the video pipeline agree on the audio clock; this goes further and removes
audio from the video schedule entirely, so the two-clock hazard cannot recur.
`av-sync.ts` now exports no video-side lever at all, and a test asserts that
absence at the module surface.

**The load-bearing constraint** (worth stating because it is not obvious, and
it dictates the whole design): the worklet consumes exactly `sampleRate`
samples per second at 1×, so **once playback has started, buffering can no
longer change when a given sample is heard.** Holding chunks longer only
changes queue depth. Alignment is a *start-time* decision, and everything
after it is a *rate* problem. Hence:

1. **Alignment, once, at start.** `AudioJitterBuffer` holds the first chunk
   until the video presentation schedule says it is due, less the device's own
   `outputLatency`. The schedule is `T/1000 + arrivalBaseline + playoutOffset`,
   published by the pipeline as `videoScheduleBaseEpochMs` and rebased onto the
   main thread's `timeOrigin` (every context has its own — README gotcha). The
   hold that results *is* the sink's queue depth for the rest of the session:
   typically a few hundred ms, which also retires finding 3's underruns far
   more robustly than a 60 ms jitter cushion did.
2. **Drift, continuously, by rate.** Clock and soundcard crystals differ by
   tens of ppm ≈ 100 ms/hour. `AudioRateController` turns measured `avSkewMs`
   into a playback rate inside ±0.4% (a semitone is ~6%, so this is well under
   audibility), slewed at 0.0008/s so the correction itself never steps, with a
   deadband at 20 ms and a give-up bound at 2 s (that is a re-anchor, which the
   flush path owns, not something to grind at max rate for minutes). The
   worklet reads at a fractional position with linear interpolation.

Fallbacks, all exercised by tests: no schedule yet ⇒ depth floor (finding 3's
behavior, so audio still plays with a cushion); a schedule that never fires ⇒
released anyway after `MAX_ALIGNMENT_HOLD_MS`; an underrun re-prime ⇒ depth
floor rather than a schedule already in the past, with the residual skew left
to the trim. `avMaster` now reads `'video'` (audio aligned) or `'free'` (audio
running without a schedule), and `alignmentHoldMs` is the number to read when
lip sync is off.

Two bugs the tests caught during this work, both mine, both invisible to
inspection:

- The overflow ceiling charged the deliberate alignment hold as backlog, so it
  dropped the very audio being lined up — and, once the drops kept the pending
  depth under the release cap, audio never started at all. The ceiling is now
  priming-aware.
- The resampler clamped instead of interpolating at chunk edges, holding the
  output flat for one sample every 20 ms. A periodic seam at the packet rate
  is audible *because* it is periodic — the partner sample now comes from the
  next chunk. Caught by a ramp-continuity assertion after the worklet source
  (previously never executed under test, since it ships as a string) was
  instantiated against stubbed worklet globals.

**Still unverified on hardware.**

### Field finding 5 (2026-07-20): audio off the R19 carrier

The first hardware test with a resilient-mode viewer exposed the flaw in
Decision 12's carrier-routing half. In resilient mode audio was *recognizable
but broken up*; in default (datagram) mode it was continuous with a short crack
at the GOP cadence. Decision 12 had assumed reliable delivery would only help
audio ("QUIC heals loss → fewer concealment gaps"). It doesn't: putting the
50 packets/s audio lane onto the same per-GOP reliable stream as the video
deltas means

- **head-of-line blocking** — an audio record queued behind a slow video-delta
  record waits for it, and a constant-rate sink has no slack to wait; and
- **clumped tail drops** — at each keyframe the carrier rotates and any records
  not yet written are dropped *together* (`drainReliable`), so audio vanishes in
  GOP-sized chunks instead of the single concealed 20 ms gaps datagram loss
  would have produced.

Reliable, in-order, bursty delivery is exactly wrong for the third medium —
which is what Decision 1 said in the first place (one Opus packet per datagram,
live-edge, no reliable streams). The premise that resilient audio wants
reliability was never tested before 2026-07-20 because audio had never played
on hardware.

**Fix (relay-only, test-first):** the audio lane never touches the carrier.
`drainReliable` now peeks each dequeued datagram's wire type and routes
`TypeAudioFrame`/`TypeAudioConfig` to the unreliable datagram path
(`sendSidebandDatagram`, byte-for-byte the normal drain's bandwidth-charge +
`SendDatagram` + egress/error accounting), while video deltas ride the carrier
exactly as before. Zero wire changes, zero broadcaster/viewer changes (carrier
records still feed the viewer's datagram path; audio just arrives as datagrams,
same as for a non-resilient viewer and same as the video decoder config always
has). `TestAudioToReliableSubscriberBypassesCarrier` replaces the old
`…RidesCarrier` test; the core R19 `TestReliableSubscriberCarrierDelivery`
(video deltas + video decoder config) is unchanged and green — video resilience
is untouched.

**What survives of Decision 12:** the profile-carrying half. The audio jitter
buffer still adopts the resilient depth envelope ([150, 2000] ms, seed 500)
under `?delivery=reliable`, because audio aligns to the *deep video playhead*
(field finding 4), not because it rides the carrier — video is still delivered
at resilient depth, so audio must sit there too to stay in sync.

**Known residual (not fixed here):** the reliable drain is one goroutine, so a
stalled carrier write still delays the audio datagrams queued behind it until
the stall is cancelled — for up to `CarrierWriteTimeout` (500 ms since docs/24
finding 12; it was `KeyframeWriteTimeout`, 1 s, when this was written) — bounded and transient
(the same lossy moment would be dropping audio anyway), and vastly better than
audio *on* the carrier permanently. Fully decoupling would need a separate audio
queue/goroutine; deferred as a possible follow-up. **Hardware re-verification of
this fix is pending.**

### Field finding 6 (2026-07-21): live-edge audio starves for buffer depth

The first resilient-vs-default comparison on hardware (Safari 26, macOS)
showed default/live-edge audio was **near-silent — the only sound was a crack
at roughly the GOP cadence** — while resilient mode was recognizable. Two
Copy-diagnostics captures settled it:

- **Not packet loss.** `audioPacketsDecoded` climbed at ~50/s (full Opus rate)
  in *both* the "almost OK" and "cracks" captures — audio arrives and decodes
  fine over plain datagrams. This also **validates field finding 5's carrier
  fix as safe**: resilient mode played because of its deeper buffer profile,
  not because the carrier's reliability was covering datagram loss.
- **Both sessions were live-edge** (`playoutMode: "off"`, `playoutOffsetMs: 0`).
- **Severity tracked arrival jitter**: `arrivalJitterMs` 72 → "almost OK",
  158 → "mostly silent". That jitter-sensitivity is the signature of a worklet
  starved for buffer depth.
- `avSkewMs` read **~3–5 s**, which the owner correctly flagged as wrong. It is
  an artifact of the underruns, not a real lip-sync error: the worklet's
  playhead only advances on real samples, so during the frequent silences it
  freezes while video keeps going, and the skew balloons.

**Root cause.** Field finding 4's video-master alignment sets the worklet's
buffer depth to "just enough that audio is heard when its video frame is
presented." In paced modes that hold is a few hundred ms — the design's happy
path. In **live-edge mode the frame is presented on arrival**, so the schedule
resolves to "due now" at ~0 ms hold: the worklet gets essentially no cushion,
and the normal 72–158 ms arrival jitter starves it into near-continuous
underrun. Finding 4 was verified in paced modes; live-edge audio was never
exercised (audio never played on hardware until 2026-07-20, and this is the
first Safari/live-edge session).

**Fix (viewer-only, test-first).** The adaptive jitter target
(`clamp(arrivalJitter + 20, [40, 150])`) is now a **depth floor in every mode**:
`AudioJitterBuffer` releases only when the video schedule is due **and** at
least `targetMs` is buffered. In paced modes the schedule hold already exceeds
the floor, so nothing changes there; in live-edge the floor supplies a
~90–150 ms cushion sized to the measured jitter. The cost is that live-edge
audio now sits a jitter-depth behind video instead of cutting out — bounded, in
the perceptually forgiving direction (audio behind video), and live-edge video
is itself jittery, so tight lip sync was never on offer there. One pure-module
change in `audio-buffer.ts`; the pre-fix behavior was literally what an existing
alignment test asserted (release the first chunk at ~0 depth), so the rewritten
test fails first on old code.

**Observability (same change).** The audio jitter-buffer counters
(`bufferedMs`/`targetMs`/`alignmentHoldMs`/`underruns`/`gapsConcealed`/
`lateDrops`/`overflowDrops`) are now surfaced. They live in the main-thread
`AudioSink` while `ViewerStats` is assembled in the worker, so this capture
couldn't show them — the same split that hides `featureGates`/
`presentationSurface`. Fixed the same way: the viewer connection merges
`AudioSink.getStats()` into the stats object (`ViewerStats.audioBuffer`,
optional) at the one point that holds both, so they reach the overlay's Audio
section **and** Copy-diagnostics uniformly. "Buffer depth *X / target* ms" well
below target with climbing "Underruns" is the starvation signature to look for.

**Follow-up (not in this change).** `avSkewMs` should self-correct once audio
plays continuously (the playhead stops freezing); if it doesn't, it is a
separate metric bug. **Hardware re-verification pending.**

### Field finding 7 (2026-07-21): the depth estimate counted audio the worklet never got

The finding-6 depth floor and its new diagnostics went to hardware (Safari 26,
macOS) and audio now *started* — then, after 5–10 s, degraded to short cracks,
and on a later session cut out entirely. Two Copy-diagnostics captures pinned it,
and both are the **same defect at two stages**:

- **Crackle capture.** `bufferedMs` sat at **300–650 ms** while `targetMs` was
  150 and `alignmentHoldMs` only 60. The overflow ceiling
  (`max(target, establishedDepth) + 200 ≈ 350 ms`) was chronically exceeded, so
  `push()` overflow-dropped **~39 of every 50 incoming packets** — each drop a
  waveform discontinuity → crackle — while `underruns` climbed as the *real*
  worklet queue starved. `audioPacketsDecoded` held a clean 50/s throughout: not
  loss, a buffer-accounting artifact.
- **Silence capture.** `bufferedMs` **frozen at exactly 520**, `underruns`
  frozen at 0, `avMaster: "free"` / `avSkewMs: null` (no playhead report for
  >1.5 s), yet `overflowDrops` grew at ~50/s (every packet) and
  `audioPacketsDecoded` kept climbing. The worklet had stopped draining (the
  AudioContext was suspended — Safari does this at will), so `notePlayed`
  stopped, the depth estimate froze above the ceiling, and every chunk was
  dropped forever with no recovery.

**Root cause.** `AudioJitterBuffer.queuedMs` is a *shadow* of the worklet's real
queue depth: incremented when a chunk is handed to the sink, decremented by the
worklet's ~4 Hz "played" reports. `AudioSink.forward()` **returns without
delivering while the AudioWorklet node is still booting** (async `addModule`) and
swallows a throwing/closed port — but `emitChunk` had already counted those
chunks. Every undelivered-but-counted chunk permanently inflates the shadow by
~one boot window (~150–300 ms). That phantom pushes `bufferedMs` chronically over
the ceiling → spurious overflow drops → crackle. Once the sink stops draining
(suspend/death), `notePlayed` stops and the shadow freezes above the ceiling;
`noteUnderrun`'s `queuedMs === 0` recovery guard can't rescue a stuck-high shadow
and nothing else lowers it → permanent silence.

**Fix (viewer-only, test-first).** Three pure/local changes:

1. **Honest accounting.** `emit` now reports whether the chunk reached the sink
   (`AudioSink.forward()` returns `false` for a null node or a throwing port),
   and `queuedMs`/`establishedDepthMs` count **only delivered** audio. The shadow
   can no longer inflate, so the spurious overflow drops — the crackle — stop.
2. **Sink-ready release gate.** The alignment cushion is held in priming until
   the worklet node exists (`sinkReady: () => this.node !== null`), so it is
   never released into a null node and lost (which would restart the worklet at
   ~0 ms depth — finding 6 redux). `MAX_ALIGNMENT_HOLD_MS` still overrides so a
   sink that never comes up can't mute audio forever.
3. **Stall recovery.** The worklet reports every ~250 ms while its context runs
   (even underrunning); a gap > `STALL_RECOVERY_MS` (1000 ms) with audio still
   arriving means suspend/death. The sink then resumes a suspended context and
   flushes both the buffer and the worklet's own queue, so audio re-anchors at
   the live edge the instant the context runs again instead of never. (A
   backgrounded tab with audio keeps reporting, so this does not false-fire.)

**Observability (same change).** The jitter-buffer `resets` counter (timeline
restarts **plus** stall recoveries) now reaches `ViewerStats.audioBuffer` and a
new overlay **Recoveries** row — a climbing count with audio present is the sink
repeatedly stalling. `AudioSink.flush()` was also hardened to swallow a
closed-port throw (it reaches the worklet on the same never-break-video path as
`forward()`).

**Not covered.** A worklet that is *dead* rather than merely suspended can't be
revived by resume+flush — only a full sink rebuild would, which is out of scope
here; the recovery still keeps the buffer from wedging and surfaces the churn via
`resets`. **Hardware re-verification pending.**

### Field finding 8 (2026-07-23): overflow drops and gap concealment fought each other

Finding 7's honest accounting went to hardware (Safari 26.5, macOS). Video was
fine; audio broke up continuously from the first second. The Copy-diagnostics
capture is unusually clean, and every number in it agrees:

| signal | value |
|---|---|
| `audioPacketsReceived` / `audioPacketsDecoded` | **identical**, a flat 50.1/s for 9.5 s |
| `badDatagrams`, `decodeErrors` | 0 |
| `overflowDrops` | **37.0/s — 74 % of everything that arrived** |
| `gapsConcealed` | 4.2/s |
| `underruns` | 1.5/s |
| `bufferedMs` / `targetMs` | pinned **321–345** / 120–126, `alignmentHoldMs` 100 |

Zero loss on the wire, zero decode failures, and three quarters of the audio
destroyed on the viewer. The equilibrium closes exactly: the ~12.7 packets/s
that survive are 254 ms of real audio per second, and 4.2 concealments/s ×
~178 ms ≈ 746 ms/s of synthesized silence make up the rest of the second the
worklet consumes. **The stream was ~25 % audio and ~75 % silence.**

**Root cause — two policies, each defensible alone, in a loop.** `push()`
counts an overflow drop and returns *before* advancing `nextExpectedUs`, so a
run of dropped packets comes back as a hole; the gap branch then conceals that
hole with exactly as much silence as was dropped — via `emitChunk`, so the
silence re-adds the very depth the drop was meant to shed. Overflow-dropping
therefore **cannot lower the depth**; it can only convert audio into silence,
at whatever rate holds the estimate at the ceiling. Once `bufferedMs` crosses
the ceiling for any reason it never comes back: 37 drops ÷ 4.2 gaps = 8.8
consecutive drops per cycle = the 176 ms of silence per concealment observed.

The reconciliation that should have caught it was gated off. `noteUnderrun`
re-primes only `if (queuedMs === 0)`, so the worklet reporting underruns *while
the buffer believed it held 330 ms* — a contradiction, and proof the estimate
had diverged — was discarded.

**What pushed it over the ceiling.** Two contributors, both fixed:

1. **The ceiling was evaluated against an estimate stale by a full report
   interval.** `queuedMs` grows on every `push()` but is only credited down on
   the worklet's ~4 Hz playhead report, so it over-reads by up to **250 ms**
   while `OVERFLOW_SLACK_MS` is **200 ms**. A perfectly healthy real-time
   producer feeding a real-time sink therefore cleared the ceiling near the end
   of every window — the regression test measures **46 spurious drops/s** with
   this alone. Structural; needs no Safari-specific behaviour.
2. **Any single ~200 ms hiccup latches it permanently.** One genuine gap under
   `BACKWARDS_RESTART_MS` injects its whole duration into `queuedMs` at once,
   and nothing brought it back down. The capture's video path shows
   `arrivalJitterMs` ~100 ms with `receivedFps` swinging 20→48 on the shared
   datagram path; `resets: 2` says the finding-7 stall watchdog had already
   fired twice.

**Fix (viewer-only, test-first, no wire/relay/broadcaster changes).** Five
changes in `audio-buffer.ts`, each mutation-verified load-bearing:

1. **An overflow drop advances `nextExpectedUs`.** The drop becomes an honest
   skip toward live and can never return as a hole to conceal. It is also the
   right answer on its own terms: overflow means the sink holds *more* than
   alignment asked for, i.e. audio is running late, so shedding content pulls
   it back toward the video — which is why the skip is deliberately **not**
   charged to the lead budget below.
2. **Shedding is hysteretic.** Once over the ceiling, drop back to
   `max(target, establishedDepth)` — the depth alignment chose — not merely
   back under the ceiling. Parked at the ceiling, input rate == drain rate
   keeps it there, dropping a packet at the margin forever (chronic crackle)
   and leaving the excess as permanent A/V skew for the rate trim to walk out
   at 0.4 %. One bounded shed is the cheaper trade.
3. **The lead budget (`MAX_AUDIO_LEAD_MS`, 100 ms — owner decision).**
   Concealment exists only because alignment is a start-time decision (finding
   4): skipping a hole moves every later sample earlier by its length. Below
   100 ms of *accumulated* skipped lead that is inaudible and the av-sync rate
   trim absorbs it, so holes are now skipped; past it, one concealment pays the
   whole accrued debt at once so the timeline is exactly restored rather than
   half-corrected. The debt is dropped on `flush()` (it belonged to the dead
   timeline) and on an underrun re-prime (alignment is being rebuilt, and the
   worklet's own silence already pushed the other way).
4. **The depth estimate is extrapolated between reports.** The worklet is known
   to drain at 1×, so `queuedNowMs()` subtracts elapsed-since-last-report and
   the next report corrects it. Capped at `MAX_DRAIN_EXTRAPOLATION_MS` (500 ms)
   so a suspended context cannot decay a real backlog to zero and flood a
   worklet that is not draining — past the cap the estimate freezes and
   finding 7's stall watchdog owns recovery.
5. **The depth is reported, not inferred.** The worklet now measures its own
   queue and sends it in the report it already posted every 250 ms —
   `queuedMs`, summed as each chunk's `frameCount / sampleRate`, i.e. in
   **content ms**, so the figure means the same thing whatever rate the context
   runs at. `AudioJitterBuffer.notePlayed(delta)` becomes `noteDepth(absolute)`.
   This is the actual root of findings 7 *and* 8: the buffer kept a *shadow* of
   a queue it did not own, built from deliveries and drain deltas, and a shadow
   cannot audit itself — undelivered chunks (finding 7), an assumed context rate
   (below), or a suspended worklet all move the two apart with nothing to notice.
   The queue's owner reports it instead, which retires the underrun clamp this
   fix originally carried.
6. **In-flight reconciliation.** A report is generated a message-hop before it
   is read, so chunks delivered in between are in the worklet's queue but not in
   its figure. Both sides keep a cumulative content-ms counter
   (`receivedMs` in the worklet, `deliveredTotalMs` in the sink) and the
   difference *is* the in-flight audio — exact, with no clock and no window.
   Neither counter resets on flush, deliberately: resetting one would race the
   other across the port.

**The sample rate, same change.** `AudioSink.build()` requested
`new AudioContext({ sampleRate })` and never read back what it got, while the
worklet advanced exactly one source sample per output frame. On macOS/Safari a
44.1 kHz context is routine (it is the device rate) against 48 kHz Opus, which
plays **8.8 % slow and a semitone low** — and under-drains by 8 %/s, walking any
inferred depth to the ceiling on its own. Three parts:

- **The worklet resamples.** The base read rate is `chunk.sampleRate /
  contextRate` (the context's rate is an `AudioWorkletGlobalScope` global),
  multiplied by the drift trim. The fractional-read resampler the trim already
  required does the work — the bug was only ever that its base was hardcoded to
  1. Computed per iteration, since the head chunk can change mid-quantum.
- **The context rate is read back, not assumed**, and a browser that *refuses*
  the `sampleRate` option (a throw — which previously took the whole stream
  video-only) falls back to a device-rate context instead.
- **Nothing on the main thread converts frames to ms any more.** Depth arrives
  in content ms, so the context rate cannot enter the accounting even in
  principle. A new overlay **Sink rate** row reports the real rate and annotates
  `(resampling)` when it differs from the stream's — the one number that
  explains "audio sounds slow", and the one this finding had to be diagnosed
  without.

**Hardware re-verification pending**, together with findings 1–7.

### Field finding 9 (2026-07-23): `avSkewMs` measured buffering depth + estimator lag, not lip-sync

A Copy-diagnostics capture from a healthy Chrome/macOS session (audio and video
both near live, sync visually and aurally fine) reported `avSkewMs` at **over
2000 ms and climbing** — the overlay's headline lip-sync number was alarmingly
wrong while nothing was actually wrong. The capture's fingerprint pins the
cause exactly: `audioPacketsReceived == audioPacketsDecoded` (zero loss),
`bufferedMs` a stable ~150–200 ms, every audio event counter (`underruns`,
`overflowDrops`, `resets`) frozen — a steady state — and `avSkewMs` rising
**1939 → 2130 over 9.5 s = exactly 20.0 ms/s**, which is `MAPPING_SLEW_MS_PER_S`
to the digit. Two independent defects, both in how the metric is *taken*, not
in the A/V path it claims to measure.

**Defect 1 — sampled at decode, not at presentation.**
`viewer.ts handleDecoded` called `observeVideoPresented(frame.timestamp)` the
instant a frame left the decoder. But the paced sink holds that frame for the
playout offset before it is on screen, and the audio playhead the metric
subtracts is audio *at the speaker* — aligned to the frame being **presented**,
not the newest one **decoded**. So the subtraction conflated the video
presentation delay and the audio buffer depth with true lip-sync. Fix: the
render sink fires a presentation observer (`PresentationObserver` in
`render-sink.ts`) at each *real* presentation — the same point the cadence
metric is already stamped, and never the interpolated α=0.5 blends, which have
no single source timestamp — with the sink's own clock, and the pipeline routes
that into `observeVideoPresented`. The main-thread fallback (no paced hold, so
present ≈ decode) keeps sampling at decode, gated on a `presentationSampled`
flag so exactly one site samples.

**Defect 2 — the mapping crawled back from re-anchors at the slew cap.**
`av-sync.notePlayhead` slew-limited every correction to 20 ms/s. That is right
for drift and report-arrival jitter, but wrong for a **re-anchor**: when the
audio jitter buffer under-runs and re-primes (`noteUnderrun`) or flushes, the
worklet playhead moves discontinuously — hundreds of ms to seconds — and
`resetAvSync()` fires only on a *broadcaster* restart, not on these buffer-level
re-anchors. So after even one such event the estimate sat up to seconds stale
and closed the gap at 20 ms/s (~100 s for a 2 s jump), which is the 20 ms/s ramp
in the capture. This also fed a bogus 0.4 % correction into the audio **rate
trim** (`AudioRateController`), so the phantom was not purely cosmetic. Fix: a
discrepancy over `MAPPING_REANCHOR_MS` (250 ms — well above report jitter, far
below a re-anchor) snaps the mapping to the report in one step; smaller ones
still slew. Snapping to a late report is correct regardless: the playhead value
is exact, only the arrival-time prediction was stale. This is the self-contained
realization of "reset on re-anchor" — it needs no signal plumbed from the buffer
and catches every discontinuity, including those no explicit signal covers.

Viewer-only, test-first, zero wire/relay/broadcaster changes. **Hardware
re-verification pending**, together with findings 1–8.

### Field finding 10 (2026-07-23): leaving Deep buffer stranded audio ~2.8 s behind

Found during the first pass of the manual verification plan below, and
**reproduced end-to-end against the homelab deployment** (Chrome/macOS, tab
audio, single viewer) rather than inferred from a capture.

**Symptom.** A viewer who switches to Deep buffer and back to Live edge plays
audio ~2.8 s behind the picture, for the rest of the session. Only a page
reload cures it. The three-point sequence:

| | fresh (live) | → Deep buffer | → back to Live edge |
|---|---|---|---|
| `deliveryMode` / `playoutOffsetMs` | datagrams / 147 | dvr / 3000 | datagrams / 130 ✓ |
| `capToRenderMs` | 169 ms | 3028 ms | 152 ms ✓ |
| `resets` | 0 | 1 | 2 |
| `alignmentHoldMs` | 73.5 ms | 2813.5 ms | **2873.5 ms** ✗ |
| `bufferedMs / targetMs` | 176 / 120 | 2942 / 3000 | **2827 / 114.6** ✗ |

Video returns to the live edge correctly; audio stays deep.

**What it was not.** `resets` incrementing 1 → 2 on the switch proves the
re-anchor path fires: `ViewerSession` emits `onAudioReset` on every pipeline
creation, `ViewerWorkerCore` forwards it (its `gen` is bumped *before*
`createSession`, so `current()` is true), and `handleAudioReset` calls
`sink.flush()`. The flush was never the problem — the re-anchor *after* it was.

**Root cause.** `maybeRelease` computed `dueAtMs` only when it was `null`, i.e.
**once per priming cycle**, latching the first schedule it saw. Audio arrives at
50/s; the new session's video baseline only reaches the sink on the ~2 Hz stats
tick (`videoScheduleBaseEpochMs` → `setVideoSchedule`). So the first chunk after
the flush *always* latched the **outgoing** schedule, and the incoming one —
milliseconds later — was never consulted. The buffer then waited for a due time
~3 s out, never became due, and released only via the `MAX_ALIGNMENT_HOLD_MS`
safety net. The observed 2873.5 ms is that cap, not a schedule-derived hold —
the net whose own comment calls it *"NOT a normal release path"*. Alignment is a
start-time decision (finding 4), so the bad anchor is permanent.

**Why only this direction.** Entering Deep buffer latches the *shallow* live
schedule, which says "due now" — but the DVR profile's depth floor
(`seedMs = minMs = B`) then holds release until 3000 ms of depth exists, so the
anchor lands correctly by way of the floor. Leaving Deep buffer has no such
backstop: a stale deep schedule can only make the buffer wait, and the cap
commits it. Deep→live is the broken direction; live→deep is right by accident.

**Fix.** Re-read the schedule on every priming pass instead of latching the
first answer — the release *is* the alignment decision, so only the schedule in
force at that moment may decide it. A momentarily absent schedule keeps the last
known due time rather than clearing it, leaving the no-schedule depth-floor path
untouched. Viewer-only, test-first (the regression test fails on the latch and
passes on the fix), zero wire/relay/broadcaster changes.

**Coverage note.** `onAudioReset` had **no test coverage at all** — no
`viewer-worker-core.test.ts` exists and no test referenced the callback — which
is how a re-anchor defect shipped green. The fix is pinned at the
`AudioJitterBuffer` seam, where the defect actually lived; the reset *wiring*
remains untested and is worth closing separately.

### Field finding 11 (2026-07-23): toggling paced playback left audio behind

Found running **step 3 of the verification plan below** (the playout-mode
sweep), measured against the homelab deployment. Step 3's stated expectation —
*"the same in every mode — skew ≈ 0"* — does not hold.

| | adaptive paced (default) | → Paced playback **off** |
|---|---|---|
| `capToRenderMs` | 204 ms | **65 ms** (video moved 139 ms earlier ✓) |
| `playoutOffsetMs` | 174 | 0 ✓ |
| `alignmentHoldMs` | 160 ms | **160 ms — unchanged** |
| `avSkewMs` | 142 ms | **333 ms** |

Video re-paces correctly and audio does not move at all: the anchor stays where
it was while the picture jumps a playout offset earlier, and the skew grows by
about that much. It is permanent for the session.

**Distinct from finding 10, and not fixed by it.** Finding 10 was a *flush that
re-anchored against a stale schedule*. Here there is no flush to get wrong: a
playout toggle is a worker command with no reconnect, so nothing re-anchors at
all. And after release the alignment is immutable — the worklet consumes exactly
`sampleRate` samples/s at 1×, so no buffering can move a sample (finding 4).
The `AudioRateController` trim is the only other lever and is far too slow for a
step this size: ±0.4 % walks out 190 ms in ~48 s of visibly wrong lip sync.

**Fix.** `AudioSink` probes the video schedule's offset at a fixed timestamp on
every refresh — the schedule is affine in the timestamp, so `present(0)`
isolates exactly the part a playout change moves and is otherwise stable — and
re-anchors (flush + re-prime) when it moves by more than
`SCHEDULE_REANCHOR_MS` (100 ms). That threshold sits between the two
populations it must separate: `PlayoutController` slews at most 50 ms/s, so a
~500 ms stats tick legitimately moves the schedule ~25 ms, while the toggle this
exists for moves it 140–190 ms. A `SCHEDULE_REANCHOR_COOLDOWN_MS` (2 s) floor
keeps an oscillating baseline from turning the fix into a stutter — one silence
is a fix, ten is the bug.

**Accepted cost:** re-anchoring drops the pending cushion, so toggling pacing
now costs a brief audio gap. That is the right trade against minutes of wrong
lip sync, and it only fires on a deliberate user action that already changes
playback — the same reasoning R19 used for "mode change = deliberate
reconnect". The re-anchor is visible as the overlay's **Recoveries** row.

Viewer-only, test-first (a material shift must re-anchor; a slew-sized sequence
must not), zero wire/relay/broadcaster changes.

**Measurement caveat for anyone re-running this:** the link degraded during the
session (`arrivalJitterMs` climbed 78 → 91 → 140 → 402 ms), so the later
absolute numbers are about the network, not the code. The finding above does not
depend on them — it rests on `alignmentHoldMs` staying pinned at 160 while
`capToRenderMs` moved 204 → 65.

### Field finding 12 (2026-07-24): `avSkewMs` over-reports on long/stressed sessions — RESOLVED 2026-07-26

A residual of [finding 9](#field-finding-9-2026-07-23-avskewms-measured-buffering-depth--estimator-lag-not-lip-sync). Finding 9's fix (measure at presentation, snap the mapping on a re-anchor) bounded the *steady-state* over-report, but did not eliminate it. A ~multi-hour Chrome/macOS field capture reported **`avSkewMs` = 18788 ms** while — per the owner watching the video — **audio was visibly near-correct**. So this is a **metric artifact, not real lip-sync error**; the audio path is fine. Corroborating: `audioPacketsReceived == audioPacketsDecoded` (zero loss), `resets` 0, buffer depth 195/150 ms and alignment hold 160 ms both healthy.

**Not yet root-caused, and deliberately left open.** An accelerated capture on 2026-07-24 (10 s sampling, fresh session) was **confounded by the test machine's performance trouble**: the worklet starved (underruns climbed ~50/s), and `avSkewMs` ramped 8 → 1986 ms in 30 s locked to the underrun storm — the playhead freezes during an underrun (`audioSink.ts` worklet, `playheadUs` only updates when a chunk is present) while video stays live, so the metric's video-minus-playhead subtraction grows. That reproduced *a* skew-inflation mechanism but **on a struggling machine, not a representative link**, so it does not cleanly isolate the multi-hour residual and is not a sound basis for a fix.

**A resync/flush "fix" was considered and rejected (owner, 2026-07-24):** the premise — that audio had really fallen behind live — is false here (audio was near-correct), so flushing audio to "catch up" would inject gaps to chase a phantom. Whatever the fix is, it belongs in how the metric is *taken* (as finding 9's did), not in the audio path.

**Root-caused 2026-07-26**, by driving the real module with synthetic playhead traces rather than waiting for another field capture. The recorded ramp is reproduced *exactly*: a playhead advancing at **0.934×** of wall time yields **1986 ms over 30 s**, the accelerated capture's figure to the digit. That is the whole shape of the bug — the reading was never a lip-sync offset at all, it was the audio **timeline** losing ground at `(1 − ratio)` per second, and one number was being asked to mean both things.

Four separable components, three of them the estimator's own error and one of them true:

1. **The mapping low-passed an exact measurement.** Each report was slewed toward the mapping's own prediction at 20 ms/s (with finding 9's snap above 250 ms bolted on). A worklet playhead that moves in jumps — the buffer skipping a hole, a re-prime resuming at live — outruns that cap, leaving a standing error for as long as the motion continues: **22.5 ms** in the sawtooth the regression test drives, invisible and unbounded in principle. The report *is* ground truth; it is now the anchor as it stands, and the snap disappears with the smoothing it was patching. Between reports the worklet drains at exactly 1×, so extrapolating one interval costs ~1 ms.
2. **Blind extrapolation ran to 1.5 s.** `PLAYHEAD_STALE_MS` bounded how long the mapping would keep extrapolating with no report, and at 1500 ms against a 4 Hz reporter the module could invent **up to 1.5 s of skew out of an assumption** — on a congested main thread, which is precisely the "stressed session" the report came from. Now 750 ms (three report intervals): past that the position is unknown, and unknown is a better answer than a guess with no error bar.
3. **Late-stamped reports over-reported by exactly the delay.** Driving the pre-[finding 13](#field-finding-13-2026-07-26-audio-settled-outputlatency-behind-the-picture-and-avskewms-read-zero-the-whole-time) stamping (`atEpochMs` = main-thread receipt) with a sustained delay gives a phantom skew equal to it — 50 ms ⇒ 50, 200 ⇒ 200, 500 ⇒ 500. Fixed by finding 13's `getOutputTimestamp()` anchoring, whose `performanceTime` belongs to the sample rather than to whenever the message was handled.
4. **The remainder is real, and finding 13 stopped it from becoming permanent.** A starving worklet genuinely falls behind, and the metric should say so. What made it *stick* was the re-prime keeping `nextExpectedUs`: the first chunk after a dry period read as a hole and bought a concealment, re-inserting the lost time as silence and pinning audio that much further back — for good, since alignment is a start-time decision. Finding 13's re-anchor removes that ratchet, so a storm now leaves a transient instead of a standing offset.

**The fix that ends the ambiguity is the fifth thing**: `avSkewMs` never had a companion saying whether the audio timeline was keeping up, which is why three findings have argued over one number. `getPlayheadAdvanceRatio()` reports how far the playhead moved against the wall clock over the last ~1 s, and the viewer overlay carries it as **Playhead advance** (annotated `(starving)` below 0.99×). Read together the two are unambiguous: `1.000×` with a skew means a genuine lip-sync offset worth acting on; `0.934×` with a skew means starvation debt still being accrued, and the buffer-health rows are where to look. It is deliberately *reported*, never used to suppress the skew — when audio really has fallen behind, that reading is true, and hiding it would be the same mistake pointing the other way.

| Goal | Verified by |
|------|-------------|
| The mapping tracks the playhead instead of trailing it | `av-sync.test.ts` "anchors on each report instead of slewing toward it" — mutation-checked: restoring the slew reads 22.5 ms of standing error. The test measures **at** each report, because extrapolating across a jump not yet reported is the 4 Hz sampling limit and not the estimator's fault |
| No skew is invented from an unheard playhead | `av-sync.test.ts` "stops reporting once it has not heard from the playhead" (500 ms fine, 900 ms null) |
| The reading can be attributed | `av-sync.test.ts` "reports how fast the audio timeline is advancing" — 1.000× healthy, 0.934× under the captured storm, and the 1980 ms of skew that ratio predicts over 30 s; `StatsOverlay.test.tsx` pins the row and its `(starving)` annotation |
| Report-arrival jitter cannot become a bias | `av-sync.test.ts` "stays within the arrival jitter it is handed" — ±40 ms in, bounded by ±40 ms out, where smoothing traded that bounded noise for an unbounded one-directional error |

**One thing is not yet re-measured**: the original multi-hour 18788 ms capture predates all of this, and no long session has been captured since. The components above are each removed and regression-tested, so the arithmetic says a healthy long session should now read near zero with `Playhead advance` at ~1.000× — but the confirming capture is a **verification task, not a design question**, and the new row makes it conclusive either way in one glance.

### Field finding 13 (2026-07-26): audio settled ~`outputLatency` behind the picture, and `avSkewMs` read zero the whole time

A Firefox/macOS viewer in **resilient mode with paced playback + interpolation**
reported audio "a little bit behind" the video. The capture said the opposite:
`avSkewMs` averaged **+1.6 ms** across the samples where video was actually
being presented, `audioPacketsReceived == audioPacketsDecoded` (zero loss),
`overflowDrops` 0, buffer depth 250 / 180 ms. Every corroborating row healthy,
the sync metric clean, and the owner still hearing it late.

Three defects, all of which converge on one root: **the skew metric is the only
long-run determinant of where audio sits**, because alignment is a start-time
decision (finding 4) and the drift trim integrates away from it thereafter. So
the metric's reference point and the metric's freshness are not diagnostics —
they are the control loop.

**(a) The trim servoed to the wrong point.** `observeVideoPresented` compared
the presented video timestamp against the **worklet's playhead**, which is the
sample being *written* into the output buffer — the device plays it
`outputLatency` later. `audioSink.ts` had this right at the alignment release
(it hands the cushion over early by exactly that much) but `AudioRateController`
then drove the *measured* skew to zero, and zero at the write position means
audio is *heard* `outputLatency` late. Given minutes, the trim always wins:
simulating the real loop from perfect alignment settles at
`outputLatency − RATE_TRIM_DEADBAND_MS`.

| device `outputLatency` | audio lateness @ 1 min | @ 5 min | settled |
|---|---|---|---|
| ≤ 20 ms | 0 | 0 | **0** (inside the deadband) |
| 40 ms | 9 ms | 20 ms | **20 ms** |
| 120 ms | 26 ms | 84 ms | **100 ms** |
| 300 ms | 63 ms | 209 ms | **280 ms** |

Built-in speakers hide it; Bluetooth, HDMI or a soundbar do not. Fixed by
measuring at the listener: `AudioSink` converts the worklet report before it
reaches av-sync, preferring `getOutputTimestamp()` — whose `contextTime` /
`performanceTime` pair *is* the audible position and the moment it is audible,
so it needs no latency estimate and also absorbs however long the message sat
in the main thread's task queue — and falling back to subtracting
`outputLatency`. `PlayheadReport.playheadUs` is renamed `heardUs` across the
worker command, because the field's meaning is the fix. A pleasant consequence:
the trim now *repairs* an imperfect alignment release instead of creating the
error it was correcting for.

**(b) The trim ran open-loop whenever presentation stalled.** `lastSkewMs` is
written only where a frame is presented, but read on the stats tick and fed
straight to the trim. The capture's first 13 samples have `renderedFps: 0` and
`renderCadenceStdDevMs: null` — nothing painted for 6.5 s (a hidden tab, an
occluded window, worker rAF throttled) — with `avSkewMs` frozen at −61.5 and the
controller still consuming it every tick, integrating a ghost into real,
permanent delay that nothing was measuring. `getAvSkewMs(now)` now returns null
past `PRESENTATION_STALE_MS` (1 s), which the trim already handles by slewing
back to 1×.

**(c) The underrun re-prime threw away the alignment it could have rebuilt.**
`noteUnderrun` cleared `alignOnSchedule`, on the reasoning that the oldest
pending chunk's slot "is already past by definition — that is why we ran dry".
True for a live-edge schedule, where the hold is ~0 anyway; false in a paced
mode, where the hold is the whole playout offset and audio arriving after the
dry-out is due a few hundred ms in the **future**. The flag is gone: the
schedule is consulted on every priming pass, and when it really is past,
`maybeRelease` already falls through to the depth gate — so live-edge is
unchanged and paced re-primes realign instead of stranding audio
`hold − floor` ms out for the minutes the trim needs at ≤4 ms/s.

One deliberate consequence: because the schedule is now consulted on every
priming episode, the rebuild's depth gate is `min(targetMs,
SCHEDULED_START_CUSHION_MS)` rather than the whole target. In resilient mode
that target rides the **video** arrival-jitter estimate and seeds at 500 ms — a
cushion sized by the wrong medium (audio is one packet per datagram, with far
lower jitter than video's carriers), and one that is pure lip-sync error when
the schedule has already gone by, since holding longer cannot make a late
release earlier. Live-edge and Deep buffer are unaffected: the former's target
is already below the cap, the latter's schedule is seconds ahead and so never
takes this path.

The same re-prime also double-paid for the gap: it kept `nextExpectedUs`, so
the first chunk after the dry period read as a hole and bought a concealment —
synthesized silence for a hole **the worklet had already filled with silence by
running dry** — and that concealment chunk carries the stale pre-gap timestamp,
so it lands at the head of the rebuild and anchors the release against a slot
long past, discarding the realignment. The timeline is now forgotten too, as it
is after a flush.

**(d) The broadcaster labelled audio later than video.** Upstream of all of the
above, and invisible to every viewer-side metric because these timestamps *are*
the reference the two media are compared against. `capture.ts` stamps a
`VideoFrame` at MSTP arrival, **before** encode; `AudioTimestampAnchor` was
pinned in the encoder's **output** callback, so the anchor absorbed MSTP
delivery + Opus algorithmic delay + queueing + the one-shot `configure()` init
and wrote all of it into every audio timestamp for the session. `avSkewMs`
cannot see it — the viewer plays audio exactly where the labels say. The anchor
is now pinned in `pushAudioData` (capture arrival, the stage video uses) and
the output side calls a new `stamped()` that applies the mapping without a
clock of its own. The excluded delay became a measurement: `encodeLagMs` (+
`anchorReanchors`, since each re-pin steps the whole audio timeline) on the
broadcaster overlay.

**Observability, because none of this was visible.** New overlay rows: viewer
**Output latency** (`audioBuffer.outputLatencyMs`) and broadcaster **Encode
lag** (annotated with re-anchors). Both ride Copy diagnostics. A capture that
shows a clean `avSkewMs` on a 200 ms output device now says so out loud.

| Goal | Verified by |
|------|-------------|
| A stream aligned at the speaker is left there | `av-sync.test.ts` "leaves a stream aligned at the speaker exactly where it is" — 15 min of the real loop, \|error\| < 1 ms; pre-fix the same loop walks to `outputLatency − 20` |
| The sink reports the audible sample, not the written one | `audioSink.test.ts` "reports the sample at the listener…" (120 ms device ⇒ 880 ms of a 1.000 s playhead) and "prefers getOutputTimestamp, which also absorbs the report hop" |
| Output latency is discoverable | `audioSink.test.ts` fallback chain (`outputLatency` → `baseLatency` → 20 ms) + `StatsOverlay.test.tsx` "Output latency" row |
| A frozen skew is not fed to the trim | `av-sync.test.ts` "stops reporting a skew once video presentation stalls" |
| A paced re-prime realigns instead of floating free | `audio-buffer.test.ts` "re-anchors a dry underrun against a schedule whose slots are still ahead"; the live-edge case still covered by "re-primes on a dry underrun by depth, not by a schedule already past", unchanged |
| Audio timestamps exclude the encoder | `audio-lane.test.ts` "stamps packets from the input arrival, not the encoder output" (80 ms encode delay ⇒ stamp at arrival) and "keeps the input anchor for every later packet" |
| The excluded delay is measured, not lost | `audio-lane.test.ts` `encodeLagMs` assertion + `BroadcasterStatsOverlay.test.tsx` "Encode lag" rows |

**Still owner-verifiable only**: the *size* of (d) on real hardware. The fix is
sound regardless — the anchor now matches video's stage by construction — but
how much lip sync it was costing depends on the encoder's init timing, which is
exactly what the new `encodeLagMs` row is there to report. Read it on the
gaming PC alongside the viewer's **Output latency**; between them they account
for every constant offset this finding removed.

### Post-implementation review (2026-07-19)

A self-review against `CODE-REVIEW.md` immediately after N6, before any
manual verification. Both findings were invisible to the green test suite,
which is the point worth keeping:

1. **An audio-lane construction failure killed the whole broadcast.**
   `startAudioLane()` builds a real `MediaStreamTrackProcessor`, which throws
   synchronously on an ended track (or a scope whose MSTP rejects audio), and
   it runs inside `startMedia()` — whose throw path tears the session down and
   rejects `start()` with `BroadcastStartError{phase:'capture'}`. That is
   exactly what Decision 6 forbids and what N2's own criterion claims. The
   existing test covered encoder errors *via the callback*; construction is a
   different path, so it stayed green. Found by CODE-REVIEW.md checklist item
   3 ("walk the failure paths by hand"), fixed test-first. The viewer sink's
   `postMessage`-with-transfer got the same guard for the same reason.
2. **Audio stats inverted the diagnosis on lane death.** The viewer read the
   decode counters straight off the live lane, so nulling it on error left the
   overlay reporting *"State: Error, decoded 0, format —"* — which reads as
   "audio never worked" rather than "audio worked, then died", on the one
   screen someone would use to debug it. Counters are now folded into a
   retained snapshot at both death sites (CODE-REVIEW.md: "counters and stats
   survive their owner's deletion").

Also closed here: the two N4 criteria that had no test behind them (a scope
without `AudioDecoder` annotating `unsupported`; a consumer taking no audio
never building a lane), and verification-by-reading that carrier records
reach audio through `viewer-transport.ts`'s single
`onCarrierRecord → onDatagram` seam, so Decision 12's viewer half needs no
audio-specific code.

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| N1 | **Wire + relay** — Go `AudioFrame`/`AudioConfig` codecs (0x07/0x08, layouts above) + TS mirrors + golden vectors both sides; hub dispatch cases; `cachedAudioConfig` cache/prime/invalidate (**both** sites: `StartPublish` + `InvalidatePrimes`, Decision 4); strict-parse limits | Golden vectors byte-identical Go↔TS (new vectors in `wire_test.go` + `wire.test.ts`); hub tests: audio frame fans out verbatim to all subscribers, config cached + primed on subscribe + invalidated on new publisher session **and** on `InvalidatePrimes` (R17 edge upstream loss), malformed audio datagrams count bad and never panic (fuzz-style table like existing types); an edge-marked hub re-ingests + caches + fans audio through the same dispatch (cluster needs no other change — Decision 4); a reliable subscriber receives audio over the unreliable datagram path, **never** as carrier records (field finding 5, reversing Decision 12's carrier routing — video deltas still ride the carrier); audio seqs never perturb `ingressFramesLost`/`ingressChunksLost` or `framesRelayed`; `/statusz` + metrics unchanged except generic datagram counters; tier-1 `e2e` CI stays green (video-only pubsim = the no-audio path, now CI-asserted) | ✅ implemented 2026-07-19 |
| N2 | **Broadcaster main-thread path + toggle** — "Enable audio (experimental)" in `broadcastSettingsStore` (own LS key, default false) + advanced panel row with applies-on-next-start annotation; capture constraints per Decision 6; audio lane in `BroadcastPipeline` (MSTP → anchor → `AudioEncoder` wrapper → datagrams + 1 Hz config re-send); no-track graceful state; audio-lane-only error teardown; `BroadcastStats` audio fields | Toggle off ⇒ `getDisplayMedia` called with `audio: false` and zero audio code paths active (behavioral no-op vs. today, existing tests untouched); toggle on + no audio track ⇒ video-only + annotation, no error; unit tests (fake encoder/sender): anchor math produces monotone shared-clock timestamps, seq increments with wrap, config re-sent at 1 Hz, encoder error kills only the audio lane; encoded packets ≤ 1184 B asserted | ✅ implemented 2026-07-19 |
| N3 | **Broadcaster worker path** — audio clone transferred beside video in `provideCapture`; `capture` command + `BroadcastMediaSource` seam gain the audio track; worker handshake probes `AudioEncoder`; no-worker-audio ⇒ video-only annotation (placement never changes) | Worker path sends audio (integration test with fake worker scope, pattern of existing broadcast-worker-core tests); handshake-without-AudioEncoder falls back to video-only while video stays in the worker; teardown stops both clones; main-thread fallback path (Firefox) unaffected | ✅ implemented 2026-07-19 |
| N4 | **Viewer decode + playback + conditional UI** — reassembler demux cases; worker `AudioDecoder` + transferred `AudioData` event; main-thread `AudioWorklet` sink (ring buffer, gap/late/underrun/overflow policies + counters, **flush/re-anchor on restart/resync + reconnect**, Decision 8); `audioPresent` flag; mute/volume in fading controls + context menu (`gawk:muted`/`gawk:volume`); tap-to-unmute on suspended context; main-thread pipeline fallback decodes in place | Ring-buffer policies unit-tested pure (fake clock): gap ⇒ silence + counter, late ⇒ drop + counter, underrun ⇒ silence + counter, overflow ⇒ oldest dropped; restart signal ⇒ ring flushed + re-anchored (a post-restart timestamp jump never late-drops forever); demux tests route 0x07/0x08 and still count unknown types bad — incl. fed as carrier records through the record path (same demux, Decision 12); `audioPresent` false ⇒ zero audio UI rendered (video-only streams pixel-identical to today), true ⇒ controls appear reactively mid-view; mute/volume persist and act on the sink only; worker-without-`AudioDecoder` ⇒ video-only viewer, annotated | ✅ implemented 2026-07-19 |
| N5 | **A/V sync** — 4 Hz playhead report channel; `avSkewMs` + audio-buffer stats in `ViewerStats`; adaptive buffer target (quantile tracker + clamp/slew per Decision 10, **profile-carrying per Decision 12** — resilient mode adopts [150, 2000] ms / seed 500 via the live-getter pattern); ~~audio-master `displayTargetMs` source in paced modes with slew-limited mapping + arrival-baseline fallback~~ **superseded 2026-07-20 (field finding 4): video-master — audio aligned at start to the video presentation schedule, drift absorbed by a sub-audible rate trim; av-sync exports no video-side lever** | Unit tests (fake clocks): skew computation correct on synthetic clocks incl. wrap; buffer target converges/clamps/slews like `PlayoutController` (same test shape) **and widens/re-seeds on the resilient flag** (mirror of the playout profile test); audio-master targets under the resilient profile sit at resilient depth, never dragging video below the video playout envelope (the Decision 12 conflict, regression-tested); ~~paced-mode target source switches audio↔arrival without a step > slew bound~~; interpolation tests still pass (unaffected by construction); **no mode delays video for audio — asserted at the module surface: av-sync exports no video-side lever, and the reorder buffer paces on the arrival baseline alone**; audio's alignment hold, its depth-floor and never-fires fallbacks, the underrun re-prime, and the rate trim's deadband/clamp/slew/give-up are unit-tested, as is the worklet resampler (instantiated against stubbed worklet globals — ramp continuity across chunk edges); manual measurement: median \|avSkewMs\| ≤ 60 ms, p95 ≤ 120 ms on the reference LAN, both browsers, **in the default adaptive mode first**, plus a ≥ 30 min drift run | ✅ implemented 2026-07-19; A/V sync reworked to video-master 2026-07-20 (manual measurement pending) |
| N6 | **Stats/overlay + docs + verify** — Audio sections on both overlays (broadcaster: codec/bitrate/encoded/s/sent/s; viewer: decoded/s, buffer ms, gaps/late/underruns, skew, muted) gated on audio presence; Copy-diagnostics includes audio; README gotchas + ROADMAP/CLAUDE status sync; manual verification pass below | Overlay sections render only when audio active; diagnostics JSON round-trips audio fields; docs synced; the full manual verification plan executed and findings recorded in this doc (incl. the graduation question: keep experimental or default-on) | 🔧 overlays + docs done 2026-07-19; graduation question answered 2026-07-23 (toggle removed — see below); manual verification pass pending |

Ordering: N1 → N2 → N4 form the minimal audible path (N2's main-thread
pipeline + N4 can be browser-verified before N3); N3 rides once N2's lane
exists; N5 needs N4's sink; N6 last. Nothing here blocks or is blocked by
R14 (which would add audio later by reusing the same wire messages — noted
as a follow-up there, not scoped here).

### Graduation (2026-07-23): the toggle is removed

User decision, once audio was verified working and reliable on real
hardware — this is the amendment the intro's rollout decision reserved, and
the answer to N6's open graduation question. **System audio is no longer a
setting.** "Enable audio (experimental)" is gone from the advanced panel,
`audioEnabled`/`gawk.audioEnabled` are gone from `broadcastSettingsStore`
(a stale persisted value is inert), and `BroadcasterScreen` passes one
`BROADCASTER_CAPTURE_CONFIG = { ...DEFAULT_CAPTURE_CONFIG, audio: true }` to
both its reclaim and mint call sites.

**Removed, not flipped to default-on**: a checkbox nobody unticks is a
support surface with nothing behind it, and everything below it already
treats "there is no audio" as a first-class state (video-only grants, the
`audioState` annotations, the viewer's audio-UI-only-when-received rule).
Unchanged by design: the frozen `#/debug/*` surfaces still pass plain
`DEFAULT_CAPTURE_CONFIG` (audio absent) and stay byte-identical; the native
broadcaster (R14) still sends no audio; the viewer is untouched.

**The one thing the toggle was still load-bearing for** is field finding 1's
error message: "turn off Enable audio and start again" was the only escape
on a machine that cannot start a system-audio source, because the video-only
retry needs its own transient activation and the picker has usually spent
it. With no toggle that advice is unimplementable, so `capture.ts` now
**remembers a refusal for the rest of the page session**: the first start
may still die on `NotReadableError`, and the next one asks for video only
and broadcasts. Deliberately *not* persisted — finding 1's device-state
class is transient (a woken output endpoint, a tab share instead of a
screen), so a reload earns a fresh audio attempt. The message now says
"start the broadcast again to continue without audio", which is what the
code does.

| Goal | Verified by |
|------|-------------|
| No audio control exists in the broadcaster UI | `EncoderSettingsPanel` renders acceleration/bitrate/codec only; the whole `gawk-app` suite stays green with the store field deleted (52 files / 732 tests) |
| The production broadcaster always requests system audio | `BROADCASTER_CAPTURE_CONFIG` is the single config object passed to both `createBroadcastSession` call sites |
| A browser that cannot start an audio source still gets to broadcast | `capture.test.ts`: a refusal yields a video-only grant flagged `audioUnavailable`, and the **next** start asks `audio: false` in a single call — both new cases fail without the session memory (mutation-checked) |
| `#/debug/*` behaviour unchanged | those surfaces still pass `DEFAULT_CAPTURE_CONFIG`, whose `audio` stays absent |
| The R20 browser-broadcaster CI step survives always-on audio | tier-1 `node run.mjs --browser-broadcast` run locally: broadcaster LIVE, `audioState: "active"`, 274 Opus packets sent, viewer 60 fps, zero relay ingress loss. Headless tab capture *grants* audio, so that step now exercises the encode lane too — it is no longer only the video-only path |

## Verification plan (manual, after N6)

All on the real deployment (homelab relay), Chrome broadcaster on a
Windows machine sharing a screen with game audio. **Expectations updated
2026-07-20 for video-master (field finding 4)** — the pre-inversion plan
predicted "video leads audio by the buffer depth", which is no longer the
shape to look for.

Where system audio can't start at all (Linux/macOS screen shares; a Windows
box whose default output endpoint won't open for loopback — field finding
1), run steps 2–5 with a **shared tab** and "share tab audio": that
exercises the entire path and is unblocked everywhere.

1. **Toggle off** (default): capture prompt shows no audio checkbox
   pressure; stream behaves byte-identically to today; viewer shows no
   audio UI. Flip the toggle mid-stream: nothing changes until the next
   broadcast start (annotation visible).
2. **Toggle on, happy path**: Chrome + Firefox viewers hear game audio
   **in the default mode (adaptive pacing + interpolation — the mainline
   path since the R12 defaults flip)**; mute/volume work and persist;
   overlay Audio sections populate on both surfaces; `avSkewMs` within
   targets (median ≤ 60 ms, p95 ≤ 120 ms). New rows to read: **Sync
   master** must say *Video (audio aligned)* — *Video (audio free-running)*
   means the schedule never reached the sink and sync is approximate; and
   **`alignmentHoldMs`** should be a few hundred ms (near zero means audio
   arrived *late* relative to video, the one case holding cannot fix).
   **`underruns` should be ~zero** — the alignment hold is now the sink's
   depth, far deeper than the old 60 ms cushion.
3. **Playout-mode sweep (R12 interplay)**: from the default, right-click
   down through Smooth playback (fixed) to live-edge (`off`), recording
   skew in each. Under video-master the expectation is the **same in every
   mode — skew ≈ 0** — because audio now aligns to the video presentation
   schedule in *all* modes, live-edge included (the schedule is
   `arrivalBaseline + playoutOffset`, and live-edge simply has offset 0).
   Video timing itself must be identical to a video-only broadcast in every
   mode: audio costs video nothing now, and any change there is a
   regression. Live-edge skew may sit slightly positive, since video
   presents on actual arrival while the schedule uses the windowed *min* —
   the rate trim should walk that out over ~a minute.
4. **Drift, the long run** (new, field finding 4): leave a viewer running
   **≥ 30 min** and watch `avSkewMs`. It should hover near zero, not walk
   steadily in one direction — a monotone drift means the ±0.4% trim isn't
   keeping up with the clock/soundcard offset and the bound wants raising.
   Listen for pitch artifacts while the trim is active: there should be
   none.
5. **Loss/stress**: throttle a viewer (existing playbook technique) —
   expect gap-concealment counters rising, silence blips, no growing delay,
   no video regression; recovery is immediate when throttling ends.
   `gapsConcealed` vs `underruns` separates the two causes: the former is
   packet loss (no FEC in v1 — Decision 11), the latter a cushion too thin.
   An underrun re-primes on the depth floor rather than the schedule, so
   expect skew to step once there and be walked back by the trim.
6. **Resilient mode (R19, Decision 12 as amended by field finding 5)**: on a
   lossy link (netem, docs/24 X1 technique), toggle Resilient mode — **audio
   stays on the unreliable datagram path** (only video deltas ride the
   carrier), so audio behaves like the default-mode run (single concealed
   gaps on loss, never GOP-clumped breaks) while video gains resilience; skew
   targets still met at resilient depth (~500 ms) because audio aligns to the
   deep video playhead, not the carrier.
7. **Graceful states**: uncheck "share audio" in the picker → video-only +
   broadcaster annotation; Firefox broadcaster → same; broadcaster restart
   mid-view with audio newly enabled → viewer's audio controls appear
   reactively; with audio disabled → controls disappear. Broadcaster
   auto-resume (R17) mid-audio → seamless, no sink reset artifacts.
8. **Autoplay edge + iPhone**: fresh profile / strict autoplay settings →
   the tap-to-unmute affordance appears and works. On an iPhone viewer
   (expected to *require* the tap): audio plays, controls stay reachable in
   pseudo-fullscreen (Decision 9's iOS note).
9. Record findings + the experimental-graduation verdict in this doc.

## Non-goals

- **Microphone/voice mixing** — this is game audio; voice is a different
  feature (processing on, different consent story).
- **Multiple audio tracks / per-viewer audio ladder** — one Opus stream for
  everyone, like video.
- **Audio in the R14 native broadcaster** — the wire messages are
  deliberately engine-agnostic; R14 as shipped runs a **GStreamer**
  subprocess (docs/19 — the ffmpeg wording that stood here was stale:
  ffmpeg/`pipewiregrab` was rejected 2026-07-15, not in mainline), so the
  future lane there is a GStreamer audio chain (`pipewiresrc`/`pulsesrc` →
  `opusenc`) emitting the same 0x07/0x08 messages from the engine; noted
  as an R14 follow-up, not scoped here.
- **Opus in-band FEC / decoder PLC** — no WebCodecs surface to exploit it
  (Decision 8); revisit only on evidence from the gap counters.
- **Audio-only listening mode**, **bitrate setting in the panel** (v1 is a
  constant; a setting can come with graduation), **DTX**.

## Rejected

- **Opus over reliable streams / raw PCM / MediaRecorder+MSE** — see the
  direction survey table; all three lose to datagram Opus on either
  philosophy (HOL stalls), cost-for-nothing (PCM), or latency (MSE).
- **Decode audio on the viewer main thread** — puts codec work back on the
  thread R8/S6 cleared, saves nothing (the worker→main crossing exists
  either way; transferring decoded `AudioData` is zero-copy).
- **Audio as a hard master clock in live-edge mode** (full lip-sync v1) —
  would delay video by the audio buffer depth for all viewers, violating
  the live-edge philosophy; the paced modes are exactly the place for
  audio-master timing, and that's where it went. *(2026-07-19: the R12
  defaults flip made the paced modes the production default rather than an
  opt-in, so audio-master timing is now the mainline path — the argument
  stands unchanged: live-edge mode, wherever selected, never delays
  video.)* **(2026-07-20, field finding 4: this rejection is resolved by
  inverting it, and the "full lip sync vs. live edge" trade it assumed
  turns out to be false. Full lip sync in every mode — live-edge
  included — is had by delaying **audio** to meet video, which costs the
  live-edge philosophy nothing precisely because video is never
  rescheduled. What we actually rejected, and still reject, is delaying
  *video* for audio's sake.)**
- **Feeding audio seqs into the relay ingress-loss window** — corrupts the
  video loss signal (single-sequence-space assumption); audio loss is
  observable client-side where it's actually concealed.
- **Repurposing an existing toggle or defaulting audio on** — experimental
  features ship default-off with their own toggle, and viewer UI appears
  only when the feature is live in the stream (user decisions 2026-07-15).
