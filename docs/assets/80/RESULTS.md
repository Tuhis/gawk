# R79 MV0 — browser voice pipeline measurements

Measured 2026-10-26 for docs/80 §9 MV0 (a) and (b). Part (c), raw Opus
through a real murmur, is measured separately. The gumble-fork prototype is
not built: §11.1 C1 replaced gumble with a first-party client.

**What this covers.** Playwright drives three browser engines, headless, on
one Linux host (x86-64, NixOS). Chromium and Firefox run on the host and
WebKit runs in the Playwright 1.62.1 Ubuntu 24.04 image. All three share one
PulseAudio null sink. The page under test is the self-contained harness in
`harness/`. It imports nothing from `gawk-app`, so it measures what each
browser does, not what our wrappers do.

**What this does not cover.** There is no real microphone, speaker, network
or Mumble in these runs. Everything labelled *estimate* below is an estimate.
Safari, iOS and Android were not measured. Playwright's WebKit is a WPE
build with a GStreamer media backend. It is **not Safari**, and its WebCodecs
and WebAudio paths are not Safari's.

## Pipeline under test

```
source ─► capture ─► AudioEncoder (opus, 48 kHz mono, 40 kbit/s, frameDuration 10/20 ms)
       ─► loopback (in-page, or a WebTransport datagram to gawk-server /echo and back)
       ─► AudioDecoder ─► jitter-buffer stand-in ─► AudioWorklet output
```

- **source**: `gum` is `getUserMedia` on the browser's fake device, with
  echo cancellation, noise suppression and auto gain requested. `synth` is a
  looped 20 ms 1 kHz tone burst every 257.3 ms, generated in the same
  AudioContext. Every engine gets the same deterministic onsets, and the
  period is not a multiple of 10 or 20 ms, so the onsets fall at every phase
  of a packet.
- **capture**: `mstp` is `MediaStreamTrackProcessor` on Window, the shipping
  `audio-lane.ts` path. `worklet` is the D21 prototype: an AudioWorklet
  copies 128-frame render quanta into 480-frame (10 ms) blocks, posts them
  to the main thread, and the main thread wraps each block in
  `new AudioData({format: 'f32-planar', …})`.
- **jitter-buffer stand-in**: a FIFO in the output worklet that starts
  playing at 20 ms queued and goes back to prefill on an underrun. With no
  network there is no real jitter. Its depth is reported but says nothing
  about a production buffer.
- Each configuration ran twice for 6 s: about 1,200 packets per 10 ms
  configuration and 600 per 20 ms one. The first 5 packets of each run are
  dropped as start-up. Encode latency is wall clock from the `encode()` call
  for the input block that completed a packet to that packet's output
  callback, matched by sample count. Decode latency is `decode()` to output,
  matched in order.

## Results

<!-- BEGIN generated: node docs/assets/80/harness/report.mjs (results/tables.md) -->

### Per browser

| | chromium | firefox | webkit |
|---|---|---|---|
| Engine build (Playwright, headless) | Chromium 151.0.7922.34 | Firefox 153.0 | WebKit (WPE, **not Safari**) 26.5 |
| `isConfigSupported` opus 48k mono 40k, frameDuration 10 ms | yes | yes | yes |
| `isConfigSupported` frameDuration 20 ms | yes | yes | yes |
| Actual packet duration (Opus TOC), asked 10 ms | 10 ms | 10 ms | 10 ms |
| Actual packet duration (Opus TOC), asked 20 ms | 20 ms | 20 ms | 20 ms |
| Encode latency ms, median / p95 (10 ms) | 0.50 / 1.50 | 0.40 / 0.86 | 1.00 / 9.00 |
| Decode latency ms, median / p95 (10 ms) | 0.30 / 1.00 | 0.86 / 1.98 | 1.00 / 7.00 |
| Packet bytes, median / p95 (10 ms, 40 kbit/s) | 40 / 56 | 82 / 101 | 39 / 51 |
| Encode latency ms, median / p95 (20 ms) | 0.60 / 1.50 | 0.48 / 1.14 | 2.00 / 12.00 |
| Decode latency ms, median / p95 (20 ms) | 0.30 / 1.00 | 0.78 / 1.88 | 1.00 / 7.00 |
| Packet bytes, median / p95 (20 ms, 40 kbit/s) | 86 / 113 | 159 / 204 | 80 / 114 |
| MediaStreamTrackProcessor on Window | yes | no | no |
| Worklet capture → `AudioData` → `AudioEncoder` works | yes (7382 packets) | yes (7510 packets) | yes (7396 packets) |
| Worklet 10 ms block arrival interval p95 ms (main thread) | 13.1 | 19.7 | 17.0 |
| Opus lookahead seen by the app (offline probe), samples / ms | 310 / 6.5 | 310 / 6.5 | 310 / 6.5 |
| …with `opus.application: 'lowdelay'` | 121 / 2.5 | 310 / 6.5 | 310 / 6.5 |
| `opus.application` kept by `isConfigSupported` (`voip` / `lowdelay`) | kept / kept | dropped / dropped | dropped / dropped |
| Fake mic track settings with EC/NS/AGC requested | EC true, NS true, AGC true, 48000 Hz | EC false, NS false, AGC false, ? Hz | EC true, NS ?, AGC ?, 44100 Hz |
| WebTransport (datagram loopback to relay `/echo`) | yes | yes | no |
| AudioContext `baseLatency` / `outputLatency` s (null sink, not a device) | 0.0116 / 0.0510 | 0.0000 / 0.0479 | 0.0027 / 0.0000 |

Latencies are wall-clock (`performance.now()`) on the main thread. Firefox runs with
`privacy.reduceTimerPrecision=false`; WebKit clamps `performance.now()` to 1 ms, so its
encode/decode numbers are quantised to 0 or 1 ms.

### Capture-node → playout-ready transit (worklet capture, synthetic source)

Per decoded packet: AudioContext frame at which it reached the output worklet minus the
frame at which its first sample entered the capture worklet. This is the smallest fixed
playout delay that would play that packet on time, before any network: it contains the
packet duration, the Opus lookahead, the worklet→main→worklet hops, encode and decode.
Device input and output latency are **not** in it (no real device headless).

| Browser | Frame | Opus application | Transport | median ms | p95 ms | p99 ms | max ms | packets |
|---|---|---|---|---|---|---|---|---|
| chromium | 10 ms | default | inpage | 22.5 | 37.2 | 39.2 | 39.8 | 1234 |
| chromium | 20 ms | default | inpage | 31.8 | 39.8 | 49.2 | 49.2 | 612 |
| chromium | 10 ms | lowdelay | inpage | 18.5 | 33.2 | 35.2 | 35.8 | 1234 |
| chromium | 20 ms | lowdelay | inpage | 27.8 | 42.5 | 45.2 | 45.2 | 611 |
| chromium | 10 ms | voip | inpage | 22.5 | 35.2 | 39.8 | 59.2 | 1233 |
| chromium | 20 ms | voip | inpage | 31.8 | 39.8 | 49.2 | 49.2 | 612 |
| chromium | 10 ms | default | wt | 22.5 | 37.2 | 39.2 | 39.8 | 1235 |
| chromium | 20 ms | default | wt | 31.8 | 46.5 | 49.2 | 49.2 | 612 |
| firefox | 10 ms | default | inpage | 25.2 | 33.8 | 35.2 | 41.8 | 1236 |
| firefox | 20 ms | default | inpage | 35.8 | 43.8 | 46.5 | 54.5 | 645 |
| firefox | 10 ms | lowdelay | inpage | 25.8 | 33.8 | 35.2 | 37.2 | 1233 |
| firefox | 20 ms | lowdelay | inpage | 34.5 | 43.8 | 45.2 | 49.2 | 624 |
| firefox | 10 ms | voip | inpage | 25.8 | 34.5 | 40.5 | 53.2 | 1263 |
| firefox | 20 ms | voip | inpage | 34.5 | 43.8 | 45.2 | 46.5 | 654 |
| firefox | 10 ms | default | wt | 25.8 | 34.5 | 37.2 | 53.2 | 1274 |
| firefox | 20 ms | default | wt | 39.8 | 74.5 | 106.5 | 166.5 | 609 |
| webkit | 10 ms | default | inpage | 25.8 | 44.5 | 56.5 | 71.8 | 1230 |
| webkit | 20 ms | default | inpage | 43.8 | 65.2 | 90.5 | 126.5 | 611 |
| webkit | 10 ms | lowdelay | inpage | 24.5 | 32.5 | 37.8 | 51.8 | 1228 |
| webkit | 20 ms | lowdelay | inpage | 33.2 | 38.5 | 45.2 | 51.8 | 607 |
| webkit | 10 ms | voip | inpage | 27.8 | 46.5 | 58.5 | 73.8 | 1235 |
| webkit | 20 ms | voip | inpage | 35.8 | 47.8 | 59.8 | 107.8 | 608 |

### Every configuration

| Browser | Capture | Source | Frame | App | Transport | Runs ok | Packet dur | Enc ms p50/p95 | Dec ms p50/p95 | Bytes p50/p95 | Buffer depth p50 ms | Underruns | Onset in-graph p50 ms | Error |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| chromium | mstp | gum | 10 ms | default | inpage | 2/2 | 10 ms | 0.50 / 0.80 | 0.20 / 0.40 | 37 / 53 | 20.2 | 4 | 12.2 |  |
| chromium | mstp | gum | 20 ms | default | inpage | 2/2 | 20 ms | 0.70 / 1.40 | 0.30 / 0.60 | 61 / 96 | 22.7 | 4 | 14.6 |  |
| chromium | mstp | synth | 10 ms | default | inpage | 2/2 | 10 ms | 0.30 / 0.90 | 0.20 / 0.50 | 48 / 57 | 23.3 | 4 | 39.8 |  |
| chromium | mstp | synth | 20 ms | default | inpage | 2/2 | 20 ms | 0.40 / 0.80 | 0.20 / 0.40 | 100 / 110 | 28.0 | 4 | 49.2 |  |
| chromium | worklet | gum | 10 ms | default | inpage | 2/2 | 10 ms | 0.60 / 3.50 | 0.40 / 3.20 | 39 / 57 | 24.0 | 4 | 41.9 |  |
| chromium | worklet | gum | 20 ms | default | inpage | 2/2 | 20 ms | 0.60 / 3.80 | 0.40 / 2.10 | 101 / 130 | 37.5 | 1 | 58.7 |  |
| chromium | worklet | synth | 10 ms | default | inpage | 2/2 | 10 ms | 0.40 / 1.30 | 0.30 / 0.90 | 49 / 57 | 23.3 | 4 | 39.8 |  |
| chromium | worklet | synth | 20 ms | default | inpage | 2/2 | 20 ms | 0.60 / 1.20 | 0.30 / 0.70 | 101 / 110 | 28.0 | 2 | 49.2 |  |
| chromium | worklet | synth | 10 ms | lowdelay | inpage | 2/2 | 10 ms | 0.30 / 0.50 | 0.20 / 0.40 | 51 / 58 | 24.0 | 4 | 35.8 |  |
| chromium | worklet | synth | 20 ms | lowdelay | inpage | 2/2 | 20 ms | 0.40 / 0.70 | 0.20 / 0.40 | 101 / 112 | 28.2 | 0 | 45.3 |  |
| chromium | worklet | synth | 10 ms | voip | inpage | 2/2 | 10 ms | 0.50 / 1.60 | 0.30 / 1.40 | 24 / 60 | 23.3 | 6 | 39.8 |  |
| chromium | worklet | synth | 20 ms | voip | inpage | 2/2 | 20 ms | 0.50 / 1.30 | 0.20 / 0.40 | 45 / 138 | 28.0 | 2 | 49.2 |  |
| chromium | worklet | synth | 10 ms | default | wt | 2/2 | 10 ms | 0.40 / 0.70 | 0.30 / 0.50 | 51 / 57 | 23.3 | 4 | 39.8 |  |
| chromium | worklet | synth | 20 ms | default | wt | 2/2 | 20 ms | 0.40 / 0.80 | 0.20 / 0.40 | 101 / 110 | 28.0 | 2 | 49.2 |  |
| firefox | mstp | gum | 10 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| firefox | mstp | gum | 20 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| firefox | mstp | synth | 10 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| firefox | mstp | synth | 20 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| firefox | worklet | gum | 10 ms | default | inpage | 2/2 | 10 ms | 0.38 / 1.04 | 0.92 / 2.64 | 83 / 88 | 18.2 | 1 | – |  |
| firefox | worklet | gum | 20 ms | default | inpage | 2/2 | 20 ms | 0.42 / 1.06 | 0.80 / 1.78 | 158 / 169 | 22.7 | 3 | – |  |
| firefox | worklet | synth | 10 ms | default | inpage | 2/2 | 10 ms | 0.42 / 0.80 | 0.80 / 1.24 | 76 / 102 | 18.2 | 1 | 37.3 |  |
| firefox | worklet | synth | 20 ms | default | inpage | 2/2 | 20 ms | 0.56 / 1.14 | 0.78 / 1.98 | 176 / 205 | 32.2 | 0 | 56.0 |  |
| firefox | worklet | synth | 10 ms | lowdelay | inpage | 2/2 | 10 ms | 0.52 / 1.20 | 0.96 / 2.40 | 76 / 102 | 18.2 | 0 | 37.3 |  |
| firefox | worklet | synth | 20 ms | lowdelay | inpage | 2/2 | 20 ms | 0.76 / 2.04 | 1.06 / 3.18 | 173 / 205 | 32.2 | 0 | 56.0 |  |
| firefox | worklet | synth | 10 ms | voip | inpage | 2/2 | 10 ms | 0.56 / 1.64 | 0.98 / 4.08 | 77 / 102 | 25.5 | 3 | 45.2 |  |
| firefox | worklet | synth | 20 ms | voip | inpage | 2/2 | 20 ms | 0.60 / 1.18 | 0.78 / 2.06 | 177 / 205 | 32.2 | 0 | 56.0 |  |
| firefox | worklet | synth | 10 ms | default | wt | 2/2 | 10 ms | 0.82 / 2.94 | 1.46 / 4.44 | 80 / 102 | 39.5 | 0 | 58.7 |  |
| firefox | worklet | synth | 20 ms | default | wt | 2/2 | 20 ms | 0.82 / 11.04 | 1.20 / 16.82 | 199 / 205 | 32.2 | 11 | 56.0 |  |
| webkit | mstp | gum | 10 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| webkit | mstp | gum | 20 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| webkit | mstp | synth | 10 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| webkit | mstp | synth | 20 ms | default | inpage | 0/2 | – | – | – | – | – | – | – | no MediaStreamTrackProcessor on Window |
| webkit | worklet | gum | 10 ms | default | inpage | 2/2 | 10 ms | 1.00 / 12.00 | 1.00 / 9.00 | 43 / 60 | 48.0 | 10 | – |  |
| webkit | worklet | gum | 20 ms | default | inpage | 2/2 | 20 ms | 1.00 / 9.00 | 1.00 / 8.00 | 102 / 186 | 57.3 | 3 | – |  |
| webkit | worklet | synth | 10 ms | default | inpage | 2/2 | 10 ms | 1.00 / 7.00 | 1.00 / 5.00 | 26 / 44 | 41.3 | 6 | 66.4 |  |
| webkit | worklet | synth | 20 ms | default | inpage | 2/2 | 20 ms | 2.00 / 12.00 | 1.00 / 7.00 | 48 / 81 | 54.7 | 5 | 85.1 |  |
| webkit | worklet | synth | 10 ms | lowdelay | inpage | 2/2 | 10 ms | 1.00 / 3.00 | 1.00 / 2.00 | 26 / 44 | 27.3 | 3 | 42.4 |  |
| webkit | worklet | synth | 20 ms | lowdelay | inpage | 2/2 | 20 ms | 1.00 / 2.00 | 0.00 / 1.00 | 48 / 83 | 21.3 | 3 | 43.8 |  |
| webkit | worklet | synth | 10 ms | voip | inpage | 2/2 | 10 ms | 1.00 / 6.00 | 1.00 / 5.00 | 26 / 44 | 45.3 | 4 | 70.4 |  |
| webkit | worklet | synth | 20 ms | voip | inpage | 2/2 | 20 ms | 1.00 / 5.00 | 1.00 / 4.00 | 48 / 81 | 45.3 | 10 | 71.8 |  |
| webkit | worklet | synth | 10 ms | default | wt | 0/2 | – | – | – | – | – | – | – | no WebTransport |
| webkit | worklet | synth | 20 ms | default | wt | 0/2 | – | – | – | – | – | – | – | no WebTransport |


<!-- END generated -->

### Phones and Safari

| Browser | 10 ms accepted? | Actual packet duration | Encode p50/p95 | Decode p50/p95 | Packet bytes | Worklet capture works? |
|---|---|---|---|---|---|---|
| Safari, macOS | not measured: needs the owner's devices (MV8) | ← | ← | ← | ← | ← |
| Safari, iOS | not measured: needs the owner's devices (MV8) | ← | ← | ← | ← | ← |
| Chrome, Android | not measured: needs the owner's devices (MV8) | ← | ← | ← | ← | ← |

Safari is the engine D21 plans to run through the worker MSTP pump. None of
the WebKit numbers above carry over to it.

### Not measurable headless

| Quantity | Why not | Recorded instead |
|---|---|---|
| Microphone input latency (device + OS + browser capture) | Fake capture devices have no acoustic path | nothing; *estimate* below |
| Real `AudioContext.outputLatency` | The output is a PulseAudio null sink. The ~48–51 ms that Chromium and Firefox report is the null sink's buffer, not a speaker | the reported value, labelled as a null sink |
| Echo-cancellation processing delay | Chromium's fake device does report EC/NS/AGC on, but no echo reaches it. Firefox's fake device reports EC off | track settings in each `*-gum-*.json` |
| Echo cancellation of WebAudio playback (MV0 b) | Needs speakers and a real microphone in one room | **owner-pending**, procedure below. The headless `aec-smoke.json` only shows that the code path runs |
| Network, bridge and murmur hops; Mumble client playout | Out of this harness | MV0 (c) and MV8 |

## Findings (measured)

1. **10 ms frames work on all three engines.** `isConfigSupported` accepts
   `opus.frameDuration: 10000`, and the bitstream really carries 10 ms. All
   packets after start-up have a 10 ms Opus TOC, and the same holds for
   20 ms.
2. **Firefox can capture.** The worklet → `AudioData` → `AudioEncoder` path
   works on Firefox 153, with fake-device `getUserMedia` and with the
   synthetic source. It also works on Chromium and on Playwright WebKit. The
   docs/80 §7 risk that Firefox might be listen-only did not happen. Neither
   Firefox nor Playwright WebKit has `MediaStreamTrackProcessor` on Window.
   `AudioData` is not defined inside `AudioWorkletGlobalScope` on any of the
   three engines, so the main-thread (or worker) hop to build the `AudioData`
   is required, not a choice.
3. **Encode and decode cost under ~1 ms at the median** on Chromium and
   Firefox, with p95 under 2 ms in-page. WebKit's timer is clamped to 1 ms.
   Its p95 is 7–12 ms, and its transit tails are longer (next point), so the
   WPE/GStreamer backend is slower or burstier. This says nothing about
   Safari.
4. **Capture-node → playout-ready transit**, browser-side only (the
   capture-to-playout table above):

   | | 10 ms frames p50 / p95 | 20 ms frames p50 / p95 |
   |---|---|---|
   | Chromium | 22.5 / 37.2 ms | 31.8 / 39.8 ms |
   | Firefox | 25.2 / 33.8 ms | 35.8 / 43.8 ms |
   | WebKit (not Safari) | 25.8 / 44.5 ms | 43.8 / 65.2 ms |

   The lower bound is the frame duration plus the Opus lookahead
   (10 + 6.5 = 16.5 ms). The rest comes from waiting for render-callback
   bursts and from the two main-thread hops. Measured headless with nothing
   else on the page: a page running the React app adds main-thread jank to
   both hops. 10 ms frames save about 9–11 ms at the median on Chromium and
   Firefox, and about 18 ms on WebKit.
5. **Opus lookahead is 312 samples (6.5 ms) on every engine.** The offline
   alignment probe (`alignment.json`) puts the decoded onset 310–313 samples
   after the input onset. `opus.application: 'lowdelay'` cuts it to 120
   samples (2.5 ms) on **Chromium only**. Firefox and WebKit drop the
   `application` member: `isConfigSupported` does not echo it back, and the
   lookahead stays at 312. Lowdelay is CELT-only, so its speech quality at
   40 kbit/s is untested here.
6. **The decoders differ on pre-skip.** Chromium and Firefox trim the
   312-sample pre-skip from the first output, given the encoder's
   `description`. WebKit does not, so its decoded stream lags its input by a
   further 6.5 ms. A shared jitter buffer must not assume either behaviour.
   Chromium also stamps its decoded `AudioData` 6.5 ms earlier than the
   packet it came from, so match decoded output to packets by order or by
   sample count, not by timestamp.
7. **Chromium's `AudioEncoder.flush()` pads and emits an extra packet each
   time it is called.** The probe got 80 packets from 40 blocks with a flush
   per block. Flush once, at end of transmission (where the extra packet
   carries the lookahead tail), and never per frame.
8. **Firefox does not hold 40 kbit/s.** Its mean packet is 73 B at 10 ms and
   143 B at 20 ms, about 57–58 kbit/s. Chromium averages 35–36 kbit/s and
   WebKit 29–32 kbit/s on the same input. The cause was not investigated.
   Every packet is still far below the 1,200-byte datagram limit, but the
   bridge and its caps should not assume the configured bitrate.
9. **The WebTransport datagram loopback to gawk-server `/echo` on 127.0.0.1
   costs little.** The round trip (send to back in the reader) is 0.9 ms
   median and 1.5 ms p95 on Chromium, and 1.8 ms median and 4.9 ms p95 on
   Firefox. Firefox's 20 ms WT run had a long tail (transit p99 106 ms, 11
   underruns) that the in-page runs do not show. Playwright WebKit has no
   `WebTransport` at all, which says nothing about Safari.
10. **Worklet blocks reach the main thread on a 10 ms median cadence, with
    p95 intervals of 13 ms (Chromium), 20 ms (Firefox) and 17 ms (WebKit).**
    Waiting for the 128-frame quantum boundary costs 0.7–1.3 ms at the
    median, because 480 is not a multiple of 128.

## What this says about the ≤100 ms mouth-to-ear target

These budgets are for a gawk speaker heard by a native Mumble listener and
for gawk → gawk. Only the first row is measured.

| Stage | 10 ms frames | Source |
|---|---|---|
| Browser capture node → encoded → decoded → playout-ready, both browser ends | 22–26 ms p50, 34–45 ms p95 | **measured** (Chromium, Firefox, WebKit-not-Safari) |
| Mic device + OS + browser capture, incl. EC/NS/AGC processing | 10–20 ms | *estimate* (typical WebRTC APM + device buffer figures, not measured here) |
| Network browser → bridge → murmur → listener (homelab LAN/WAN) | 5–30 ms | *estimate*; MV0 (c) and MV8 measure the bridge/murmur part |
| Jitter buffer on a real network | 20–40 ms | *estimate*. The shipping viewer audio buffer floor is 40 ms (`DEFAULT_AUDIO_PROFILE.minMs`), and a voice buffer could run lower on a LAN |
| Output device latency (speaker/headset) | 10–40 ms | *estimate*. The headless `outputLatency` is a null sink, not a device |
| **Total** | **~70–155 ms** | measured part plus estimates |

Read honestly: the part this spike measured costs a quarter to a third of a
100 ms budget, and the rest depends on things only real devices and the real
deployment show. ≤100 ms is plausible for a wired desktop on the LAN with a
low-latency output device and a small jitter buffer. It is not shown, and it
is unlikely on phones or Bluetooth headsets. **I'd keep ≤100 ms as the
target for wired desktop, record phones and Bluetooth separately, and decide
at MV8 sign-off**, once MV0 (c)'s murmur numbers and a real-device run of
this harness are in.

## Frame size per browser

| Browser | Choice | Why |
|---|---|---|
| Chromium (desktop; Android unmeasured) | **10 ms** | Accepted and real. About 9 ms less transit than 20 ms, at under 1 ms extra encode cost. `lowdelay` saves another 4 ms, but its quality is untested. Leave it off unless MV8 listening says it is fine. |
| Firefox | **10 ms** | Same as Chromium, through the worklet capture path. `application` is ignored, so the lookahead stays at 6.5 ms. |
| Safari (macOS, iOS) | **10 ms, provisionally** | Playwright WebKit accepts it and emits real 10 ms packets, but that is not Safari. Confirm at MV8. |

Cost of 10 ms over 20 ms (*estimate*): twice the packet rate, 100/s instead
of 50/s. With roughly 50–60 B of IP/UDP/QUIC/datagram and wire overhead per
packet, one talker's uplink is about 75–80 kbit/s at 10 ms against 55–60 at
20 ms (Chromium payload sizes). Mumble's own client offers 10 ms frames, so
the frame size causes no interop problem.

## Echo cancellation with WebAudio playback (MV0 b): owner-pending

This could not be measured headless. The page can measure it on a real
machine. The procedure, per browser (Chrome, Firefox, Safari on the Mac, and
the phones once the page is served over https):

1. From the repo root, run
   `python3 -m http.server 8080 --bind 127.0.0.1 -d docs/assets/80/harness`
   and open `http://127.0.0.1:8080/voice-pipeline.html`. Localhost counts as
   a secure context, so `getUserMedia` works.
2. **No headset**: use the laptop speakers at normal talking volume, with the
   built-in microphone. Click **echoCancellation: true** and allow the
   microphone. It plays 100 ms chirps every 500 ms through an AudioWorklet
   (the voice playback path, not a WebRTC sink) for 10 s, recording the mic
   at the same time. Copy the JSON.
3. Click **echoCancellation: false** and copy that JSON as well.
4. Compare `echoOverFloorDb`, the mic level inside the chirps against
   between them. If EC removes WebAudio playback, the `true` run sits near
   0–3 dB while the `false` run is well above 10 dB. If both are high, that
   browser's EC does not cover WebAudio output, and users without a headset
   will be heard twice (OD17).
5. Repeat with a headset: both runs should then be near 0 dB.
6. Optional, by ear: run gawk voice against a second device and listen for
   your own voice coming back.

Put the JSONs in `results/aec-<browser>-<device>-<ec>.json`.

## Reproduce

From the repo root:

```sh
(cd e2e && npm ci && npx playwright-core install chromium firefox)
docker pull mcr.microsoft.com/playwright:v1.62.1-noble          # WebKit runs in here
# a pulseaudio binary on PATH, or PULSEAUDIO=/path/to/pulseaudio
# (NixOS: PULSEAUDIO=$(nix build nixpkgs#pulseaudio --no-link --print-out-paths)/bin/pulseaudio)
bash docs/assets/80/harness/run-all.sh                            # SECONDS_PER_RUN=6 REPS=2 by default
node docs/assets/80/harness/report.mjs                            # regenerates results/tables.md
```

`run-all.sh` builds `gawk-server` (`CGO_ENABLED=0`, for the `/echo` runs),
starts a private PulseAudio with a null sink, runs Chromium and Firefox on
the host and WebKit in the container, then writes the report. To run a
single browser, use `node docs/assets/80/harness/run.mjs --browsers chromium
[--only worklet-synth-10ms] [--relay-bin PATH]`. Firefox needs
`PULSE_SERVER` pointing at a running PulseAudio; without an output device its
`AudioContext` never leaves `suspended`.

## Files

- `harness/voice-pipeline.html` and `harness/voice-pipeline.js`: the page
  (pipeline run, offline alignment probe, AEC probe).
- `harness/run.mjs`: the Playwright driver. `harness/run-all.sh`: the
  full measurement. `harness/report.mjs`: the tables.
- `results/<browser>/<capture>-<source>-<frame>-<transport>-<application>-rep<N>.json`:
  one run each, with summary fields and per-packet raw arrays (`raw.legend`
  explains them).
- `results/<browser>/alignment.json`, `aec-smoke.json`, `meta.json`, and
  `results/tables.md` (generated).
