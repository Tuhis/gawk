# H.264 MPEG-TS test fixture

`sample.ts` is a real H.264 MPEG-TS stream, embedded as `fixture.TS` and
shared by the mpegts/engine/gst tests and by the `gawk-pubsim` synthetic
publisher (R20, docs/25). Regenerated with:

```sh
ffmpeg -f lavfi -i "testsrc2=size=320x240:rate=30:duration=2" \
  -c:v libx264 -preset ultrafast -tune zerolatency -profile:v baseline \
  -bf 0 -g 15 -keyint_min 15 -sc_threshold 0 \
  -x264-params "repeat-headers=1:annexb=1" \
  -f mpegts sample.ts
```

Properties the tests depend on, chosen to match the shipped encoder invariants
(docs/19 Decision 13):

| Property | Value | Why |
|---|---|---|
| Frames | 60 (2 s @ 30 fps) | enough GOPs to see cadence |
| GOP | 15 frames = **500 ms** | item 11's `keyframeIntervalMs` |
| B-frames | **none** (`-bf 0`) | decode order == presentation order is a protocol assumption |
| Parameter sets | SPS+PPS **before every IDR** (`repeat-headers=1`) | the DecoderConfig extradata is empty on this path, so only a self-sufficient keyframe can prime a late joiner |
| Framing | one PES per access unit | what the demuxer recovers structurally |

**A deliberate deviation from the design doc, recorded rather than hidden.**
docs/19 V3 specifies "a captured fixture from the real V2 pipeline" — i.e.
GStreamer with a hardware encoder. This fixture is generated with ffmpeg and
x264 instead, for two reasons:

1. It is what CI can reproduce. The gates run on a headless runner with no GPU,
   no PipeWire and no portal; a hardware-captured fixture could never be
   regenerated there, only committed and trusted.
2. What these tests exercise is the *container* and the *bitstream*, and those
   are identical whichever encoder produced them — TS packetization and H.264
   NAL syntax are not vendor-specific. The parts that genuinely differ per
   encoder (does *this* GPU honour `b-frames=0`, is the GOP really 500 ms, does
   `h264parse config-interval=-1` really repeat the headers) are properties of
   the live pipeline, and no committed fixture can verify them anyway.

So this fixture pins the demuxer and the parsers; the encoder invariants
themselves stay a V2 trial check and a manual verification on the gaming PC.
Using x264 here is **not** a software-encode rung sneaking back in (Decision 4's
refusal is about what the app *runs*, not what generates test data).

## `sample-audio.opus` — the Opus fixture (R25, docs/28 Decision 12)

`sample-audio.opus` is 101 Opus packets — 48 kHz stereo, 20 ms each (~2 s),
128 kbps — embedded as `fixture.Audio` and replayed by `gawk-pubsim -audio`.
It is what lets CI exercise the **real engine send path** (seq stamping, the
1 Hz config cadence, the wire encoding) rather than only the viewer.

Framing on disk is a `uint16` big-endian length prefix per packet, repeated —
deliberately the same shape as R19's carrier records, so the repo gains no new
framing concept for a test fixture. `fixture.SplitAudio` reads it.

Content is a **two-tone ping-pong**: 440 Hz and 880 Hz alternating every
250 ms, in opposite phase on the two channels, so a human running the harness
locally can hear at once whether audio is arriving, whether it is stereo, and
whether it is stuttering.

Regenerated in two steps — an encode, then a repackage using this repo's own
demuxer, which is why there is no Ogg page parser anywhere:

```sh
ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
       -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=0.25" \
       -filter_complex "[0:a][1:a][0:a][1:a][0:a][1:a][0:a][1:a]concat=n=8:v=0:a=1[l];\
[1:a][0:a][1:a][0:a][1:a][0:a][1:a][0:a]concat=n=8:v=0:a=1[r];\
[l][r]amerge=inputs=2,volume=0.3[a]" \
  -map "[a]" -c:a libopus -b:a 128k -ar 48000 -ac 2 -frame_duration 20 \
  -vbr off -application audio -f mpegts tone.ts

go run genaudio/main.go tone.ts sample-audio.opus
```

`genaudio` refuses anything that is not one stereo 20 ms frame per packet, so a
regenerated fixture cannot silently drift away from what Decision 3's caps
force and what the viewer is configured for.

The docs/28 recipe reaches the same bytes through GStreamer
(`opusenc ! multifilesink`, one packet per file). ffmpeg is used here for the
same reason it is used above: it is what a contributor's machine and CI
actually have. The **packets** are container-independent either way — which is
also why ffmpeg's habit of batching five access units into one PES does not
matter here (docs/28 NA1 finding 2): the demuxer loops over them.
