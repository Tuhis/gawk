# gawk-broadcast troubleshooting

Field-tested failure modes, debug taps, and the sharp edges of the
capture/encode path. For how the pipeline is built, see
[`internals.md`](internals.md); for the design decisions, docs/19, 28 and
39 in the repository-level [`docs/`](../../docs/README.md).

## Debug taps

- **`GAWK_DUMP_TS=<path>` tees the raw MPEG-TS to disk** while
  broadcasting. Play it with `mpv`/`ffplay` — it is exactly what the
  encoder produced, so it splits "the capture is black at the source" from
  "the viewer can't decode it" in one step.
- **`GAWK_DUMP_H264=<path>` tees the demuxed Annex-B elementary stream** —
  every access unit exactly as the MPEG-TS demuxer reconstructed it,
  before any drop policy (also playable with `mpv`/`ffplay`). Against the
  TS dump it isolates the demuxer: identical quality convicts the encoder,
  damage only here convicts AU reconstruction, damage only on the viewer
  convicts drops/wire/decode.
- **`-v` passes the GStreamer child's stderr through**, and the child
  inherits `GST_DEBUG` — e.g. `GST_DEBUG=pipewire*:5` for capture-format
  problems.

## Capture

- **Don't gate on Wayland.** The portal works on X11 GNOME sessions too.
  The app gates on the portal call succeeding, never on the session type.
- **`pipewiresrc` dying in preroll with `stream error: unhandled format`
  is a capture problem, not an encoder problem.** The compositor's chosen
  screencast format sometimes can't be mapped onto the downstream caps
  (DMA-BUF modifier/DRM-caps skew between `gst-plugin-pipewire` and the
  encoder's converter, or 10-bit HDR desktop formats). The live start
  walks a three-rung capture ladder (zero-copy capped → zero-copy →
  system-memory, the last pinning bare `video/x-raw` at the source) for
  each encoder before advancing, and an all-`pipewiresrc` failure reports
  as a capture error, never "no hardware encoder".
- **A capture death *after* the probe window rebuilds the pipeline; it
  does not end the broadcast.** Window shares renegotiate their screencast
  format whenever the window resizes, changes output or hands its surface
  over. `Source.restartCapture` re-runs the cascade on the portal grant
  already in memory — no second picker, same relay session, same frameId
  space. Rebuilds are rate-limited (60 per 30 s, so only sustained
  thrashing gives up) and counted (`Stats.CaptureRestarts`). The rebuilt
  pipeline's first frame is a keyframe carrying `EncoderRestarted`, which
  re-derives the DecoderConfig: a rebuild may land on a different rung or
  encoder, and the viewer's decoder has only that codec string to go on.

## Encoding and timing

- **The encoder caps must carry `framerate=<fps>/1` even though the
  stream is VFR.** The portal's caps say `0/1` (damage-driven capture) and
  `vah264enc` silently budgets rate control for 30 fps when it sees that —
  at 60 fps that halves the effective bitrate and motion turns to mush.
  With `videorate drop-only=true` upstream the pin is signalling, not CFR:
  no frame is ever synthesized. Related: a bare `video/x-raw` capsfilter
  between a GPU converter and a GPU encoder means *system memory* — pin
  the memory feature (`video/x-raw(memory:VAMemory)`) or pay a download +
  re-upload per frame.
- **The keyframe cadence stretches when capture runs under the nominal
  fps.** `key-int-max`/`gop-size` count *frames*; at 40 fps real rate a
  30-frame GOP is 750 ms, not 500. gst-launch cannot inject
  force-key-unit events, so the cadence is measured (stats line
  `keyframes every ~…`, GUI "Keyframe interval") rather than silently
  assumed.
- **Frame timestamps are clock-anchored PES PTS, never arrival stamps.**
  Arrival stamping (post-encode/mux/pipe) clumps timestamps, and the
  viewer trusts timestamps for pacing — clumped stamps inflate its
  adaptive playout offset and schedule decode bursts that trip its
  discard-until-keyframe backpressure: decode fps intermittently craters
  on native streams while browser streams decode fine. `ptsAnchor` maps
  pipewiresrc's capture-time PTS onto the engine clock (min observed
  arrival−pts; re-anchors on PTS wrap/restart).
- **One `ptsAnchor` maps both media, and that is the A/V sync design.**
  Audio and video are muxed by one `mpegtsmux` from one pipeline running
  time, so one anchor maps both with one affine function and the relative
  skew is zero by construction. Giving audio its own anchor looks tidier
  and silently reintroduces a constant lip-sync bias the viewer cannot
  measure or remove. `TestOneAnchorStampsBothMedia` exists to stop that
  refactor.
- **Audio's declared framerate keeps audio smooth on a still screen.**
  `mpegtsmux` is a `GstAggregator`: it aggregates against a deadline
  derived from the *declared caps framerate*, not from what actually
  arrives. Because `BuildPipeline` pins `framerate=<fps>/1` on the encoder
  caps, audio keeps flowing at 20 ms while damage-driven video delivers
  nothing. Measured: video absent for 4.6 s, zero audio gaps over 100 ms.
  Declare `1/5` instead and audio bursts in five-second clumps (docs/28
  findings 6 and 8).

## Wire and demux

- **This is the Annex-B publisher.** The engine emits raw Annex-B with
  **empty DecoderConfig extradata** and never builds an avcC record; the
  viewer's `isAnnexB` start-code sniff routes it into the branch that
  ignores extradata. The browser broadcaster always sends AVCC, so **this
  path is the only thing exercising the viewer's Annex-B branch** — if it
  ever regresses, native broadcasts break and browser ones don't.
- **`h264parse config-interval=-1` is load-bearing, not cosmetic.** Empty
  extradata means a late joiner primed with the relay's cached keyframe
  can only decode if SPS/PPS travel *inside* the keyframe AU. Without it,
  late joiners see nothing while everyone already watching is fine — the
  worst kind of bug.
- **The Opus-in-MPEG-TS control header starts `0x7F`, not `0xFF`.** The
  mapping spec's 11-bit prefix holds `0x3FF` — *ten* ones — so an 11-bit
  field containing it is one zero bit followed by ten ones, and the first
  byte is `0x7F`. A demuxer syncing on `0xFF` finds nothing, forever.
  Also: one PES may carry **several** Opus packets (GStreamer writes one,
  ffmpeg batches five), and only the PES has a timestamp, so the rest are
  derived by adding what each packet's TOC says it lasts.
- **A dropped delta drops the rest of its GOP.** The pump's channel-full
  drops happen before frameIds are assigned, so the wire stays contiguous
  and the viewer's freeze-on-gap cannot see them — sending the GOP
  remainder would decode as reference-broken garbage (the first real
  viewer session did exactly that). `Source.offer` gates deltas until the
  next keyframe, and a keyframe arriving into a full queue flushes the
  stale backlog rather than being dropped. Under sustained backpressure
  the stream degrades to a clean keyframe cadence, never to artifacts.
- **Demuxed AUs must be cloned before they outlive the demux callback.**
  `mpegts.AU.Data` aliases the demuxer's internal buffer and is rewritten
  by the next AU; a frame handed to the channel un-cloned gets scrambled
  while it waits for the sender. In the field this produced clean dumps
  (both taps read the bytes while still valid) with a black viewer: the
  SPS parse ran on recycled bytes, no DecoderConfig was ever derived, and
  an unconfigured `VideoDecoder` swallows chunks silently. `-race`
  catches the aliasing outright; the pump clones at the callback boundary.
- **`pipewiregrab` is not in mainline FFmpeg** — an unmerged patchset
  carried downstream by Jami. Mainline ffmpeg has no PipeWire input at
  all. Don't re-propose it without checking it actually merged.

## Desktop integration

- **Notifications must be critical urgency to matter.** KDE's portal
  inhibits normal notifications *while screen casting*, so the act of
  broadcasting suppresses them. Failures use critical urgency; going live
  doesn't.
- **Viewer count**: the relay pushes the live "N watching" count
  (`TypeViewerCount` 0x0B) once a second; the GUI shows it on the stats
  card and rings a critical-urgency notification on the first 0→1 viewer
  transition, once per broadcast. Nothing arrives until the first push, so
  the count reads unavailable rather than `0` before then.
- **Building the GUI needs the Gio headers** (see the README's Build
  section); `go test ./internal/...` does not.
