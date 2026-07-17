# mpegts test fixture

`sample.ts` is a real H.264 MPEG-TS stream, regenerated with:

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
