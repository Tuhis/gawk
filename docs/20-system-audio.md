# R15 — System Audio: Opus over Datagrams (experimental)

Design doc for [ROADMAP R15](../ROADMAP.md#r15--system-audio) (designed
2026-07-15; **design refreshed 2026-07-19** against everything landed since —
R16 U4 verdict, R17 scale-out, R18 viewer count, R19 resilient mode, R20 CI,
and the R12 defaults flip — see [Design refresh](#design-refresh-2026-07-19);
**N1–N6 implemented 2026-07-19; hardware playback 2026-07-20/07-21 produced
six field findings, all fixed — including the Decision 10 inversion to
**video-master** A/V sync, the Decision 12 reversal taking audio back off
the R19 reliable carrier, and a live-edge audio buffer-depth floor;
hardware re-verification pending** — see
[Status](#status)). Adds the broadcaster's
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

**Implementation status (2026-07-19)**: N1–N6 implemented; automated gates
green in all three modules (`gofmt`/`go vet`/`go test -race`, `npm test` +
`lint` + `build`, `helm lint`). **Manual browser verification is pending** —
the plan below has not been executed, and audio has never played on real
hardware as designed. Three field findings so far: **1** (the audio toggle
failed the whole broadcast, before a single Opus packet was encoded) and,
once audio did play, **2** (video froze — two clocks driving one pipeline)
and **3** (the jitter buffer never buffered). All three are fixed; the
verification pass has **not** been re-run since. Deviations and decisions
taken during implementation:

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
carrier write stalled up to `KeyframeWriteTimeout` still delays the audio
datagrams queued behind it until the stall is cancelled — bounded and transient
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
| N6 | **Stats/overlay + docs + verify** — Audio sections on both overlays (broadcaster: codec/bitrate/encoded/s/sent/s; viewer: decoded/s, buffer ms, gaps/late/underruns, skew, muted) gated on audio presence; Copy-diagnostics includes audio; README gotchas + ROADMAP/CLAUDE status sync; manual verification pass below | Overlay sections render only when audio active; diagnostics JSON round-trips audio fields; docs synced; the full manual verification plan executed and findings recorded in this doc (incl. the graduation question: keep experimental or default-on) | 🔧 overlays + docs done 2026-07-19; manual verification pass pending |

Ordering: N1 → N2 → N4 form the minimal audible path (N2's main-thread
pipeline + N4 can be browser-verified before N3); N3 rides once N2's lane
exists; N5 needs N4's sink; N6 last. Nothing here blocks or is blocked by
R14 (which would add audio later by reusing the same wire messages — noted
as a follow-up there, not scoped here).

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
