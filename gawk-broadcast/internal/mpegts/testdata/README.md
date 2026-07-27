# `opus-h264-na1.ts` — NA4's ground truth

263 KB / 1400 TS packets cut from a real `mpegtsmux` capture, produced by the
R25 NA1 spike (`gawk-broadcast/scripts/na1-audio-spike.sh`) on Debian 13
(trixie), GStreamer 1.26.2, `vah264enc`, 2026-07-27. The full 12 MB capture is
not committed; this is the first 1400 packets of it, which is where PAT, PMT and
the first PES of each stream live.

It is **the same pipeline shape `BuildPipeline` produces**, with `videotestsrc`
standing in for the portal (the spike cannot open a portal session), so
everything the demuxer keys on is real rather than synthesized:

| | |
|---|---|
| PMT PID | `0x0020` |
| video | `stream_type 0x1b`, PID `0x0041`, PES `0xE0`, 33.3 ms PTS steps |
| audio | `stream_type 0x06`, PID `0x0042`, PES `0xBD`, 20.0 ms PTS steps |
| audio descriptors | registration `"Opus"` (tag `0x05`) + DVB extension `0x80`, `channel_config=2` (tag `0x7f`) |
| Opus control header | `7F Ex` — **not** `FF Ex`; the 11-bit prefix holds `0x3FF`, which is ten ones, so the first byte is `0x7F` |
| access units per PES | exactly one (ffmpeg's muxer batches five — the format permits it, GStreamer's does not do it) |

What it actually contains, measured when NA4 was written (2026-07-27) rather
than assumed:

- **8 audio PES**, one access unit each, PTS stepping exactly 1800 ticks
  (20.0 ms), TOC `0xFC` throughout — CELT fullband, 20 ms, stereo, one frame.
- **Control headers of 4 or 5 bytes**, no trim flags anywhere. The size field
  is what varies: `0xFF` + a remainder for the usual ~300-byte packet, three
  bytes for the one 539-byte packet.
- **No trailing bytes** in any audio PES: the payload tiles exactly into
  control-header-plus-packet, which is why the demuxer treats a leftover as a
  parse failure rather than as padding.
- The **video** stream carries an HDMV registration descriptor. That is noise
  to us, but load-bearing noise: it is why audio is identified by its `"Opus"`
  registration descriptor and not by "this stream has a registration
  descriptor" — the latter would bind the audio PID to the picture.

**Correction (2026-07-27).** This file previously said the first audio PES
carries `start_trim_flag=1` with a 6-byte control header. It does not, and no
PES in this capture does. That claim belongs to docs/28 NA1 finding 1, whose
bytes (`7f f0 ff ff 58 01 38 fc`) came from the **ffmpeg** reference capture
used to validate the analyzer — a different implementation of the same mapping
spec — and it was carried across to this file by mistake. The parser handles
trim fields either way (`parseOpusControlHeader`, and a test pins each optional
field's effect on the header length), but the fixture is not what proves it.

Contents are a test pattern and whatever was playing on the machine. There is
nothing private in it, and it is committed bytes on purpose: regenerating it
needs a Linux box with PipeWire and a GPU, which CI does not have.
