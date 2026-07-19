# R15 — System Audio: Opus over Datagrams (experimental)

Design doc for [ROADMAP R15](../ROADMAP.md#r15--system-audio) (designed
2026-07-15; **design refreshed 2026-07-19** against everything landed since —
R16 U4 verdict, R17 scale-out, R18 viewer count, R19 resilient mode, R20 CI,
and the R12 defaults flip — see [Design refresh](#design-refresh-2026-07-19);
**N1–N6 implemented 2026-07-19, automated gates green, manual browser
verification pending** — see [Status](#status)). Adds the broadcaster's
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
    where pacing already exists.** Good-enough, by construction, in three
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
      becomes the master clock.** The video `displayTargetMs` derives from
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
    this design)*. For a `?delivery=reliable` subscriber the drain writes
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
    - Viewer side: carrier records feed the existing datagram path
      (`readServerStreams` → same demux), so N4's reassembler cases serve
      both delivery modes unchanged. No audio-specific carrier code.
    - **The audio jitter-buffer target becomes profile-carrying, like
      `PlayoutController`** (same live-getter-on-the-resilient-flag pattern,
      `transport/playout.ts` / `resilient.ts`): default clamp [40, 150] ms
      as designed; resilient mode adopts the video playout envelope
      (clamp **[150, 2000] ms, seed 500**) so the audio playhead sits at
      resilient depth. Without this, Decision 10's audio-master pacing
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
  This was the only genuine conflict found.
- **Defaults flip (R12, docs/17 Decision 8 superseded)** (Decision 10):
  adaptive pacing + interpolation are the production defaults, so
  audio-master pacing is the mainline path; verification re-ordered
  accordingly.
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
hardware. Deviations and decisions taken during implementation:

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
| N1 | **Wire + relay** — Go `AudioFrame`/`AudioConfig` codecs (0x07/0x08, layouts above) + TS mirrors + golden vectors both sides; hub dispatch cases; `cachedAudioConfig` cache/prime/invalidate (**both** sites: `StartPublish` + `InvalidatePrimes`, Decision 4); strict-parse limits | Golden vectors byte-identical Go↔TS (new vectors in `wire_test.go` + `wire.test.ts`); hub tests: audio frame fans out verbatim to all subscribers, config cached + primed on subscribe + invalidated on new publisher session **and** on `InvalidatePrimes` (R17 edge upstream loss), malformed audio datagrams count bad and never panic (fuzz-style table like existing types); an edge-marked hub re-ingests + caches + fans audio through the same dispatch (cluster needs no other change — Decision 4); a reliable subscriber receives audio as carrier records with `SendDatagram` never called (Decision 12); audio seqs never perturb `ingressFramesLost`/`ingressChunksLost` or `framesRelayed`; `/statusz` + metrics unchanged except generic datagram counters; tier-1 `e2e` CI stays green (video-only pubsim = the no-audio path, now CI-asserted) | ✅ implemented 2026-07-19 |
| N2 | **Broadcaster main-thread path + toggle** — "Enable audio (experimental)" in `broadcastSettingsStore` (own LS key, default false) + advanced panel row with applies-on-next-start annotation; capture constraints per Decision 6; audio lane in `BroadcastPipeline` (MSTP → anchor → `AudioEncoder` wrapper → datagrams + 1 Hz config re-send); no-track graceful state; audio-lane-only error teardown; `BroadcastStats` audio fields | Toggle off ⇒ `getDisplayMedia` called with `audio: false` and zero audio code paths active (behavioral no-op vs. today, existing tests untouched); toggle on + no audio track ⇒ video-only + annotation, no error; unit tests (fake encoder/sender): anchor math produces monotone shared-clock timestamps, seq increments with wrap, config re-sent at 1 Hz, encoder error kills only the audio lane; encoded packets ≤ 1184 B asserted | ✅ implemented 2026-07-19 |
| N3 | **Broadcaster worker path** — audio clone transferred beside video in `provideCapture`; `capture` command + `BroadcastMediaSource` seam gain the audio track; worker handshake probes `AudioEncoder`; no-worker-audio ⇒ video-only annotation (placement never changes) | Worker path sends audio (integration test with fake worker scope, pattern of existing broadcast-worker-core tests); handshake-without-AudioEncoder falls back to video-only while video stays in the worker; teardown stops both clones; main-thread fallback path (Firefox) unaffected | ✅ implemented 2026-07-19 |
| N4 | **Viewer decode + playback + conditional UI** — reassembler demux cases; worker `AudioDecoder` + transferred `AudioData` event; main-thread `AudioWorklet` sink (ring buffer, gap/late/underrun/overflow policies + counters, **flush/re-anchor on restart/resync + reconnect**, Decision 8); `audioPresent` flag; mute/volume in fading controls + context menu (`gawk:muted`/`gawk:volume`); tap-to-unmute on suspended context; main-thread pipeline fallback decodes in place | Ring-buffer policies unit-tested pure (fake clock): gap ⇒ silence + counter, late ⇒ drop + counter, underrun ⇒ silence + counter, overflow ⇒ oldest dropped; restart signal ⇒ ring flushed + re-anchored (a post-restart timestamp jump never late-drops forever); demux tests route 0x07/0x08 and still count unknown types bad — incl. fed as carrier records through the record path (same demux, Decision 12); `audioPresent` false ⇒ zero audio UI rendered (video-only streams pixel-identical to today), true ⇒ controls appear reactively mid-view; mute/volume persist and act on the sink only; worker-without-`AudioDecoder` ⇒ video-only viewer, annotated | ✅ implemented 2026-07-19 |
| N5 | **A/V sync** — 4 Hz playhead report channel; `avSkewMs` + audio-buffer stats in `ViewerStats`; adaptive buffer target (quantile tracker + clamp/slew per Decision 10, **profile-carrying per Decision 12** — resilient mode adopts [150, 2000] ms / seed 500 via the live-getter pattern); audio-master `displayTargetMs` source in paced modes with slew-limited mapping + arrival-baseline fallback | Unit tests (fake clocks): skew computation correct on synthetic clocks incl. wrap; buffer target converges/clamps/slews like `PlayoutController` (same test shape) **and widens/re-seeds on the resilient flag** (mirror of the playout profile test); audio-master targets under the resilient profile sit at resilient depth, never dragging video below the video playout envelope (the Decision 12 conflict, regression-tested); paced-mode target source switches audio↔arrival without a step > slew bound; interpolation tests still pass with audio-derived targets (explicit criterion); live-edge mode never delays video (no video-target change when mode `off`); manual measurement: median \|avSkewMs\| ≤ 60 ms, p95 ≤ 120 ms on the reference LAN, both browsers, **in the default adaptive mode first** (the mainline path since the defaults flip) | ✅ implemented 2026-07-19 (manual measurement pending) |
| N6 | **Stats/overlay + docs + verify** — Audio sections on both overlays (broadcaster: codec/bitrate/encoded/s/sent/s; viewer: decoded/s, buffer ms, gaps/late/underruns, skew, muted) gated on audio presence; Copy-diagnostics includes audio; README gotchas + ROADMAP/CLAUDE status sync; manual verification pass below | Overlay sections render only when audio active; diagnostics JSON round-trips audio fields; docs synced; the full manual verification plan executed and findings recorded in this doc (incl. the graduation question: keep experimental or default-on) | 🔧 overlays + docs done 2026-07-19; manual verification pass pending |

Ordering: N1 → N2 → N4 form the minimal audible path (N2's main-thread
pipeline + N4 can be browser-verified before N3); N3 rides once N2's lane
exists; N5 needs N4's sink; N6 last. Nothing here blocks or is blocked by
R14 (which would add audio later by reusing the same wire messages — noted
as a follow-up there, not scoped here).

## Verification plan (manual, after N6)

All on the real deployment (homelab relay), Chrome broadcaster on the
Windows gaming PC sharing a screen with game audio:

1. **Toggle off** (default): capture prompt shows no audio checkbox
   pressure; stream behaves byte-identically to today; viewer shows no
   audio UI. Flip the toggle mid-stream: nothing changes until the next
   broadcast start (annotation visible).
2. **Toggle on, happy path**: Chrome + Firefox viewers hear game audio
   **in the default mode (adaptive pacing + interpolation — the mainline
   path since the R12 defaults flip)**; mute/volume work and persist;
   overlay Audio sections populate on both surfaces; `avSkewMs` within
   targets (median ≤ 60 ms, p95 ≤ 120 ms).
3. **Playout-mode sweep (R12 interplay, inverted since the defaults
   flip)**: from the default, right-click down through Smooth playback
   (fixed) to live-edge (`off`), recording skew in each — live-edge must
   never delay video (video leading audio by roughly the buffer depth is
   the expected shape); re-enable adaptive + interpolation — paced modes
   should *tighten* skew (record before/after), no regressions in the R12
   jitter metrics.
4. **Loss/stress**: throttle a viewer (existing playbook technique) —
   expect gap-concealment counters rising, silence blips, no growing delay,
   no video regression; recovery is immediate when throttling ends.
5. **Resilient mode (R19, Decision 12)**: on a lossy link (netem, docs/24
   X1 technique), toggle Resilient mode — audio arrives as carrier records
   (overlay Delivery-mode row), concealment gaps near zero vs. the
   datagram run, skew targets still met at resilient depth (~500 ms), a
   carrier drop surfaces as a silence blip, never growing delay.
6. **Graceful states**: uncheck "share audio" in the picker → video-only +
   broadcaster annotation; Firefox broadcaster → same; broadcaster restart
   mid-view with audio newly enabled → viewer's audio controls appear
   reactively; with audio disabled → controls disappear. Broadcaster
   auto-resume (R17) mid-audio → seamless, no sink reset artifacts.
7. **Autoplay edge + iPhone**: fresh profile / strict autoplay settings →
   the tap-to-unmute affordance appears and works. On an iPhone viewer
   (expected to *require* the tap): audio plays, controls stay reachable in
   pseudo-fullscreen (Decision 9's iOS note).
8. Record findings + the experimental-graduation verdict in this doc.

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
  video.)*
- **Feeding audio seqs into the relay ingress-loss window** — corrupts the
  video loss signal (single-sequence-space assumption); audio loss is
  observable client-side where it's actually concealed.
- **Repurposing an existing toggle or defaulting audio on** — experimental
  features ship default-off with their own toggle, and viewer UI appears
  only when the feature is live in the stream (user decisions 2026-07-15).
