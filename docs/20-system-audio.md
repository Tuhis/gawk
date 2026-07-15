# R15 — System Audio: Opus over Datagrams (experimental)

Design doc for [ROADMAP R15](../ROADMAP.md#r15--system-audio) (designed
2026-07-15, not started). Adds the broadcaster's **system audio** to the
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
   are taken), mirrored byte-identically in Go (`internal/wire`) and TS
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

4. **Relay: two dispatch cases, one new cache slot, zero new mechanisms.**
   In `HandleDatagram`: `TypeAudioFrame` → strict-parse header + verbatim
   `relayDatagram` fan-out (payload opaque, like everything else);
   `TypeAudioConfig` → strict-parse + copy into a new `cachedAudioConfig` +
   fan out. The cache is join-primed in `Subscribe` and invalidated in
   `StartPublish`, byte-for-byte the `cachedClockMapping` lifecycle. Audio
   rides the existing per-subscriber queues, bandwidth cap, and drop paths
   untouched — they are media-agnostic.
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
      precisely the seam R12 built.
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
  Subscribe: prime cachedAudioConfig (after ClockMapping, before keyframe)

Viewer:
  transport worker (unchanged, opaque datagrams)
  → viewer worker: reassembler demux 0x07/0x08
      → AudioDecoder → AudioData ──transfer──▶ main thread
  main thread: AudioWorklet ring buffer (adaptive 40–150 ms target)
      gaps→silence, late→drop, GainNode volume, mute = output pause
      ── 4 Hz playhead report ──▶ viewer worker (skew metric; master
                                   clock for paced video modes)
```

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| N1 | **Wire + relay** — Go `AudioFrame`/`AudioConfig` codecs (0x07/0x08, layouts above) + TS mirrors + golden vectors both sides; hub dispatch cases; `cachedAudioConfig` cache/prime/invalidate; strict-parse limits | Golden vectors byte-identical Go↔TS (new vectors in `wire_test.go` + `wire.test.ts`); hub tests: audio frame fans out verbatim to all subscribers, config cached + primed on subscribe + invalidated on new publisher session, malformed audio datagrams count bad and never panic (fuzz-style table like existing types); audio seqs never perturb `ingressFramesLost`/`ingressChunksLost` or `framesRelayed`; `/statusz` + metrics unchanged except generic datagram counters | 📋 not started |
| N2 | **Broadcaster main-thread path + toggle** — "Enable audio (experimental)" in `broadcastSettingsStore` (own LS key, default false) + advanced panel row with applies-on-next-start annotation; capture constraints per Decision 6; audio lane in `BroadcastPipeline` (MSTP → anchor → `AudioEncoder` wrapper → datagrams + 1 Hz config re-send); no-track graceful state; audio-lane-only error teardown; `BroadcastStats` audio fields | Toggle off ⇒ `getDisplayMedia` called with `audio: false` and zero audio code paths active (behavioral no-op vs. today, existing tests untouched); toggle on + no audio track ⇒ video-only + annotation, no error; unit tests (fake encoder/sender): anchor math produces monotone shared-clock timestamps, seq increments with wrap, config re-sent at 1 Hz, encoder error kills only the audio lane; encoded packets ≤ 1184 B asserted | 📋 not started |
| N3 | **Broadcaster worker path** — audio clone transferred beside video in `provideCapture`; `capture` command + `BroadcastMediaSource` seam gain the audio track; worker handshake probes `AudioEncoder`; no-worker-audio ⇒ video-only annotation (placement never changes) | Worker path sends audio (integration test with fake worker scope, pattern of existing broadcast-worker-core tests); handshake-without-AudioEncoder falls back to video-only while video stays in the worker; teardown stops both clones; main-thread fallback path (Firefox) unaffected | 📋 not started |
| N4 | **Viewer decode + playback + conditional UI** — reassembler demux cases; worker `AudioDecoder` + transferred `AudioData` event; main-thread `AudioWorklet` sink (ring buffer, gap/late/underrun/overflow policies + counters); `audioPresent` flag; mute/volume in fading controls + context menu (`gawk:muted`/`gawk:volume`); tap-to-unmute on suspended context; main-thread pipeline fallback decodes in place | Ring-buffer policies unit-tested pure (fake clock): gap ⇒ silence + counter, late ⇒ drop + counter, underrun ⇒ silence + counter, overflow ⇒ oldest dropped; demux tests route 0x07/0x08 and still count unknown types bad; `audioPresent` false ⇒ zero audio UI rendered (video-only streams pixel-identical to today), true ⇒ controls appear reactively mid-view; mute/volume persist and act on the sink only; worker-without-`AudioDecoder` ⇒ video-only viewer, annotated | 📋 not started |
| N5 | **A/V sync** — 4 Hz playhead report channel; `avSkewMs` + audio-buffer stats in `ViewerStats`; adaptive buffer target (quantile tracker + clamp/slew per Decision 10); audio-master `displayTargetMs` source in paced modes with slew-limited mapping + arrival-baseline fallback | Unit tests (fake clocks): skew computation correct on synthetic clocks incl. wrap; buffer target converges/clamps/slews like `PlayoutController` (same test shape); paced-mode target source switches audio↔arrival without a step > slew bound; interpolation tests still pass with audio-derived targets (explicit criterion); live-edge mode never delays video (no video-target change when mode `off`); manual measurement: median \|avSkewMs\| ≤ 60 ms, p95 ≤ 120 ms on the reference LAN, both browsers | 📋 not started |
| N6 | **Stats/overlay + docs + verify** — Audio sections on both overlays (broadcaster: codec/bitrate/encoded/s/sent/s; viewer: decoded/s, buffer ms, gaps/late/underruns, skew, muted) gated on audio presence; Copy-diagnostics includes audio; README gotchas + ROADMAP/CLAUDE status sync; manual verification pass below | Overlay sections render only when audio active; diagnostics JSON round-trips audio fields; docs synced; the full manual verification plan executed and findings recorded in this doc (incl. the graduation question: keep experimental or default-on) | 📋 not started |

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
2. **Toggle on, happy path**: Chrome + Firefox viewers hear game audio;
   mute/volume work and persist; overlay Audio sections populate on both
   surfaces; `avSkewMs` within targets (median ≤ 60 ms, p95 ≤ 120 ms) in
   live-edge mode.
3. **R12 interplay**: enable Smooth playback, then Paced (adaptive), then
   interpolation — audio stays in sync (paced modes should *tighten* skew:
   record before/after), no regressions in the R12 jitter metrics.
4. **Loss/stress**: throttle a viewer (existing playbook technique) —
   expect gap-concealment counters rising, silence blips, no growing delay,
   no video regression; recovery is immediate when throttling ends.
5. **Graceful states**: uncheck "share audio" in the picker → video-only +
   broadcaster annotation; Firefox broadcaster → same; broadcaster restart
   mid-view with audio newly enabled → viewer's audio controls appear
   reactively; with audio disabled → controls disappear.
6. **Autoplay edge**: fresh profile / strict autoplay settings → the
   tap-to-unmute affordance appears and works.
7. Record findings + the experimental-graduation verdict in this doc.

## Non-goals

- **Microphone/voice mixing** — this is game audio; voice is a different
  feature (processing on, different consent story).
- **Multiple audio tracks / per-viewer audio ladder** — one Opus stream for
  everyone, like video.
- **Audio in the R14 native broadcaster** — the wire messages are
  deliberately engine-agnostic and R14's ffmpeg path can produce Opus
  later; noted as an R14 follow-up, not scoped here.
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
  the live-edge default; the paced modes are exactly the opt-in place for
  audio-master timing, and that's where it went.
- **Feeding audio seqs into the relay ingress-loss window** — corrupts the
  video loss signal (single-sequence-space assumption); audio loss is
  observable client-side where it's actually concealed.
- **Repurposing an existing toggle or defaulting audio on** — experimental
  features ship default-off with their own toggle, and viewer UI appears
  only when the feature is live in the stream (user decisions 2026-07-15).
