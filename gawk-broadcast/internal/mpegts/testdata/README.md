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

Two cases the fixture happens to contain, both of which a demuxer must survive:
the **first** audio PES carries `start_trim_flag=1` with a 2-byte trim field
(120 samples), so its control header is 6 bytes where every later one is 4; and
the video stream carries an HDMV registration descriptor, which is noise to us —
video is found by `stream_type 0x1b`, never by the descriptor.

Contents are a test pattern and whatever was playing on the machine. There is
nothing private in it, and it is committed bytes on purpose: regenerating it
needs a Linux box with PipeWire and a GPU, which CI does not have.
