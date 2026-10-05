# Viewer decode fixtures

Recorded streams for `tests/decode_fixtures.rs`: the decode layer's tests on
real VP8, VP9 and H.264 bitstreams (docs/67 §9, IO4). Committed bytes rather
than generated at test time, because the bytes differ between encoder
versions and CI shouldn't need an encoder. The Opus fixture isn't here: the
test reads the Go publisher simulator's
(`gawk-broadcast/internal/fixture/sample-audio.opus`) where it lives.

| File | Codec | Size | Frames | Keyframes |
|---|---|---|---|---|
| `vp8-320x240.ivf` | VP8 | 320×240 | 30 | 0, 15 |
| `vp9-320x240.ivf` | VP9 profile 0 | 320×240 | 30 | 0, 15 |
| `vp9-240x320.ivf` | VP9 profile 0 | 240×320 | 30 | 0, 15 |
| `h264-320x240.h264` | H.264 Constrained Baseline 1.3 (`avc1.42C00D`) | 320×240 | 30 | 0, 15 |

Made with ffmpeg 8.1.2 (Homebrew; libvpx 1.16.0, x264 r3222):

```sh
ffmpeg -f lavfi -i testsrc2=size=320x240:rate=30 -frames:v 30 -pix_fmt yuv420p \
  -c:v libvpx -deadline realtime -cpu-used 8 -lag-in-frames 0 -auto-alt-ref 0 \
  -g 15 -keyint_min 15 -b:v 200k -f ivf vp8-320x240.ivf

ffmpeg -f lavfi -i testsrc2=size=320x240:rate=30 -frames:v 30 -pix_fmt yuv420p \
  -c:v libvpx-vp9 -deadline realtime -cpu-used 8 -lag-in-frames 0 -auto-alt-ref 0 \
  -g 15 -keyint_min 15 -b:v 200k -f ivf vp9-320x240.ivf

ffmpeg -f lavfi -i testsrc2=size=240x320:rate=30 -frames:v 30 -pix_fmt yuv420p \
  -c:v libvpx-vp9 -deadline realtime -cpu-used 8 -lag-in-frames 0 -auto-alt-ref 0 \
  -g 15 -keyint_min 15 -b:v 200k -f ivf vp9-240x320.ivf

ffmpeg -f lavfi -i testsrc2=size=320x240:rate=30 -frames:v 30 -pix_fmt yuv420p \
  -c:v libx264 -profile:v baseline -preset veryfast -tune zerolatency -bf 0 \
  -g 15 -keyint_min 15 -sc_threshold 0 -b:v 200k \
  -x264-params aud=1:repeat-headers=1 -bsf:v h264_mp4toannexb \
  -f h264 h264-320x240.h264
```

Why they're shaped this way:

- **Realtime encodes, as broadcasters produce them.** `-deadline realtime`,
  `-lag-in-frames 0` and `-auto-alt-ref 0` (no hidden alt-ref frames) for
  libvpx, `-tune zerolatency -bf 0` for x264: one shown frame per compressed
  frame, in decode order, which is what WebCodecs and the native encoders
  send (no B-frames, docs/38 D9). The tests assert exactly one decoded frame
  per input frame, so a hidden frame would fail them.
- **A keyframe every 15 frames** (`-g 15 -keyint_min 15`, and
  `-sc_threshold 0` so x264 adds no scene-cut IDRs): two GOPs, so the tests
  cross a keyframe boundary, the D13 point where a delta chain resets.
- **A landscape and a portrait VP9 clip.** A rotation arrives as a new
  `DecoderConfig` at the swapped size (docs/67 D9), which resets the decoder
  (D13). The test decodes into the middle of the landscape GOP, rebuilds the
  decoder, and decodes the portrait clip, checking every frame reports the
  size it decoded at.
- **H.264 as the native broadcasters send it**: Annex-B, with an AUD and
  in-band SPS/PPS before every IDR (`aud=1:repeat-headers=1`; docs/38 D9,
  docs/54 D8). The AUDs are also how the test splits the raw stream into
  access units, since a raw `.h264` file has no container framing.
- **IVF** for VP8/VP9: the simplest container libvpx's own tools use, a
  32-byte header and a 12-byte header per frame, read by a few lines in the
  test.
- **Small**: 320×240 at 200 kbps for one second keeps each file under
  40 KB.
