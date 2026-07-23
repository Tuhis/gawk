# R25 — Native broadcaster audio (gawk-broadcast)

**Status**: designed 2026-07-23, not started. Flips the docs/20 non-goal
"**Audio in the R14 native broadcaster**", which already named the shape this
doc fills in: *"the future lane there is a GStreamer audio chain
(`pipewiresrc`/`pulsesrc` → `opusenc`) emitting the same 0x07/0x08 messages
from the engine; noted as an R14 follow-up, not scoped here."*

Chunks are **NA1–NA8**. Every single-letter chunk prefix A–Z is claimed, so
this milestone uses a two-letter prefix like R21 (`DV`) and R22 (`MF`).

**Blast radius**: `gawk-broadcast` only. Zero relay changes, zero wire
changes, zero viewer changes, zero `gawk-app` changes. That is not an
aspiration — it is checked below, message by message.

## Goal

A Linux broadcaster running `gawk-broadcast` publishes **system audio**
alongside their screen, and a viewer hears it in sync, with no viewer-side or
relay-side change of any kind. Audio is on by default and **never** costs a
broadcast: a machine with no usable audio source publishes video exactly as it
does today, and says so.

Pre-registered success criteria (the same numbers R15 set for the browser, so
the two broadcasters are held to one standard):

- A viewer of a native broadcast reports median `|avSkewMs| ≤ 60 ms` and
  p95 `≤ 120 ms` over a 60 s window.

  **Caveat on the instrument**: `avSkewMs` only became a lip-sync measurement
  in docs/20 **field finding 9** (2026-07-23) — before it, it conflated the
  video presentation hold and the audio buffer depth with true skew, and read
  over 2000 ms on a session that was visually and aurally fine. The fix
  (sample at *presentation*, snap the mapping on re-anchor) is implemented but
  shares findings 1–9's **pending hardware re-verification**. So a bad reading
  during NA8 indicts two candidates, and the honest sequencing is to run R25's
  manual pass **after or alongside** R15's re-verification — not to debug a
  native audio lane with an instrument whose own field pass has not happened.
- Audio survives the broadcaster switching their default output device
  (headphones ↔ speakers) mid-broadcast — or, if it cannot, it fails
  *audio-only* and the video keeps going.
- With audio unavailable, every byte on the wire is what today's build sends.

## Background: what already exists

Most of this feature shipped with R15 — for the browser. The parts that are
engine-agnostic are already in place and are **not** re-litigated here:

| Layer | State | Where |
|---|---|---|
| Wire 0x07 `AudioFrame` / 0x08 `AudioConfig` | Done, in the **shared Go** package | `gawk-server/wire/wire.go` |
| Golden vectors for both, mirrored into the native module | Done (docs/20 deviation 6, ahead of need) | `gawk-broadcast/internal/wirecheck` |
| Relay dispatch, config cache, join-prime, invalidate | Done | `hub.go` `TypeAudioFrame`/`TypeAudioConfig` |
| Audio excluded from the video ingress-loss window and `framesRelayed` | Done | R15 N3 |
| Viewer decode → jitter buffer → AudioWorklet sink, A/V sync, overlay rows | Done, plus nine field findings' worth of hardening | docs/20 findings 1–9 |

So the native broadcaster is the **only** missing producer. docs/20 deviation
6 anticipated exactly this: the vectors were mirrored into `wirecheck` while
the native broadcaster still sent no audio, precisely so that this milestone
would be a producer change and nothing else.

What is genuinely new here is Linux capture (the portal carries no audio),
GStreamer-side Opus encode, and getting encoded packets across the child
process boundary onto **the same clock as video**.

## Decisions

### 1. Audio is captured outside the portal, from the default sink's monitor

The XDG ScreenCast portal grants a video node. It carries no audio, and there
is no audio equivalent to hand out. So audio does not come from the grant — it
comes from PipeWire directly, capturing the **monitor of the default sink**
(what is coming out of the speakers). No portal permission is involved, which
is also why OBS's PipeWire audio capture works the way it does.

Three consequences worth stating plainly, because they differ from the browser:

- The share picker stays **video-only**. A broadcaster picks a screen; they do
  not pick audio, and nothing new appears in the dialog.
- Audio is **whole-system output**, never per-window. The browser's
  tab-audio-vs-system-audio distinction (docs/20 field finding 1) has no
  analogue here.
- PipeWire is **guaranteed present**: the portal handshake this app already
  performs requires it. So the primary capture candidate can assume PipeWire
  rather than probing for a sound server.

### 2. The audio source is a probed cascade, `pipewiresrc` first

Same instinct as the encoder cascade (docs/19 Decision 4): a list of
candidates, each accepted only by a **real trial**, last-good cached and
re-verified rather than trusted.

| # | Element | Why it is in this position |
|---|---|---|
| 1 | `pipewiresrc` with `stream.capture.sink=true` | WirePlumber routes it to the **default sink's monitor** and *follows* that default — a headphone/speaker switch re-routes the stream instead of erroring it. That is the single most valuable property in the list (see Decision 6's mid-session hole). |
| 2 | `pulsesrc device=@DEFAULT_MONITOR@` | The pipewire-pulse compatibility path, for stacks where candidate 1 does not resolve. Binds to the monitor at start; does not follow a default change. |
| 3 | `pulsesrc device=<explicit>` | `-audio-device` / config. The escape hatch for a machine whose "system audio" is a specific device the broadcaster names. |

**An audio trial is cheap and unobtrusive in a way the video trial is not**: it
opens no picker, needs no permission, and touches no GPU. So it runs
*before* the portal handshake, alongside `EnsureBinary` — the same ordering
rule that keeps a machine without GStreamer from being asked to share its
screen first (docs/19 Decision 10). A broadcaster learns "no audio on this
machine" before they pick a window, not after.

Trial pipeline, mirroring `BuildTrialPipeline`'s shape:

```
<candidate> ! audioconvert ! audioresample
  ! audio/x-raw,rate=48000,channels=2
  ! opusenc ! fakesink num-buffers=25
```

25 buffers ≈ 500 ms of audio: enough to prove the source produces samples and
`opusenc` accepts them, short enough not to be felt at startup. Bounded by a
context timeout like `realTrial`, because a source that opens and then never
produces is exactly as broken as one that fails to open, and must not hang
startup.

### 3. Encode stays in GStreamer: Opus, 48 kHz stereo, 20 ms, DTX off

The encode does not move into Go. A pure-Go Opus encoder does not exist, and
the cgo bindings that do would put libopus headers on the build path of
`internal/engine` — which is imported by `cmd/gawk-pubsim`, whose entire reason
for living in this module is that it builds **without** the GUI's cgo
dependencies (docs/25 Decision 3). Adding cgo to the engine would break the CI
harness to save a subprocess we already run.

The audio branch:

```
<audiosrc> ! queue ! audioconvert ! audioresample
  ! audio/x-raw,rate=48000,channels=2
  ! opusenc bitrate=128000 frame-size=20 dtx=false inband-fec=false \
            audio-type=restricted-lowdelay
  ! queue ! mux.
```

Every property is load-bearing; none is decoration:

- **`rate=48000,channels=2` forced.** A 5.1 or 7.1 output sink has a 6- or
  8-channel monitor, and `opusenc` would answer that with **channel-mapping
  family 1** (multistream Opus). WebCodecs cannot decode multistream Opus
  without an `OpusHead` description — and docs/20 sends an **empty**
  description by design. Downmixing in `audioconvert` is what keeps the
  viewer's decoder configuration honest. `audioresample` covers a 44.1 kHz
  monitor.
- **`dtx=false`.** docs/20 Decision 1 makes a constant packet rate
  load-bearing: the viewer's gap detection and buffer clock assume 50 packets
  per second, and DTX would silently make silence look like loss.
- **`frame-size=20`.** 20 ms ≈ 320 B at 128 kbps — the "one Opus packet per
  datagram, no chunking, no reassembly" property the whole R15 wire design
  rests on.
- **`inband-fec=false`.** The viewer has no FEC hook (docs/20 Decision 8:
  WebCodecs exposes none), so in-band FEC would spend bitrate on redundancy
  nothing can read.
- **`audio-type=restricted-lowdelay`.** Drops libopus's ~6.5 ms encoder
  lookahead. This project spends design budget on single-digit milliseconds
  elsewhere; at 128 kbps stereo the quality cost on game audio is not audible.
  It is the one property here that is a *judgement* rather than a constraint —
  flagged in NA8's verification so it gets listened to rather than assumed.
- **`queue` on both sides of the encoder.** Decouples the audio source's
  thread from the muxer's, so audio scheduling jitter does not reach the video
  path. Non-leaky and small: the drop policy lives in Go (Decision 9), where it
  can be counted.

Bitrate is a constant, not a setting, exactly as in the browser (docs/20:
"Bitrate is a named constant, not a setting, in v1").

### 4. One child, one pipe: audio is muxed into the existing MPEG-TS

The audio branch feeds **the `mpegtsmux` already in the pipeline**, and Go
recovers Opus packets from the same stdout it already demuxes.

Verified before choosing this (2026-07-23):

- `mpegtsmux` accepts `audio/x-opus` on its sink pads.
- It is a **`GstAggregator`** subclass (`mpegtsmux` → `GstBaseTsMux` →
  `GstAggregator`), so in a live pipeline it aggregates against a clock
  deadline. This matters more here than anywhere: our video is
  **damage-driven** and stops entirely on a static screen, and a
  collect-pads-style muxer that waited for a video buffer before releasing
  audio would hold audio hostage to whether anything is moving on screen.
- Opus is muxed as PES `stream_id` **0xBD** (private stream 1), PMT
  `stream_type` **0x06** (private data) with a `"Opus"` **registration
  descriptor** plus a DVB extension descriptor (0x80) carrying the channel
  configuration.

Why muxing rather than a second pipe, when a second pipe is less code: **the
shared clock** (Decision 5). Everything else about audio in this project is
downstream of it.

The cost is that `internal/mpegts` — a package whose doc comment says it
"should not grow into" a general demuxer — has to learn one more shape. That
comment's actual rule is *"the scope here is exactly the pipeline we build in
internal/gst"*, and this **is** the pipeline we build. The scope stays exactly
that: one program, one video stream, one audio stream, sections that fit one
packet. NA4 keeps that sentence true by extending it, not by deleting it.

**Pre-registered fallback**, decided by NA1's spike and not by taste: if
`mpegtsmux`'s Opus support turns out to be absent, broken, or to add latency in
this pipeline, audio moves to its **own pipe out of the same child** —
`rtpopuspay timestamp-offset=0 ! rtpstreampay ! fdsink fd=4`, giving a 2-byte
length prefix (RFC 4571 framing) and a 12-byte RTP header, both fully specified
and both trivial to parse. The price is Decision 5: two timebases instead of
one, and a constant A/V bias of tens of milliseconds. Take it only on evidence.

### 5. One PTS anchor for both media — the load-bearing sync decision

R15's entire A/V design rests on one sentence from docs/20: audio and video
timestamps are stamped **on the same clock**. In the browser that is
`performance.now()` for both. Here it is `ptsAnchor` — and the decision is that
audio and video share **one anchor instance per child**, not one each.

`ptsAnchor` maps the child's PES PTS timeline onto the engine clock by keeping
`min(arrival − pts)` (see `internal/gst/pts.go`). Because both media are muxed
by the same `mpegtsmux` from the same pipeline running time, they share one PTS
timeline; one anchor therefore maps both with **one affine function**, and the
relative A/V skew it introduces is exactly zero, by construction, for the same
reason the browser's is.

Two consequences to keep in mind while implementing:

- The shared minimum will be dominated by whichever medium has the lower
  end-to-end delay (probably audio: no GPU encode, no 64 kB pipe backlog). That
  shifts **both** stamps by one constant, which is what "one affine function"
  means — the pair stays internally consistent, and the R5 absolute
  capture→render latency reading stays honest to the earlier-arriving medium.
  This is a feature of the shared anchor, not a bug to correct.
- Giving audio its own anchor would look tidier and would silently reintroduce
  a `(video path latency − audio path latency)` bias — a constant lip-sync
  error the viewer cannot detect or remove. **Don't.** A test pins the
  single-anchor property so a later refactor cannot quietly split it.

The re-anchor gap check (`ptsReanchorGapUs`, 5 s) is unaffected: the muxer emits
in timestamp order, so interleaved audio and video PTS stay monotonic to well
within that tolerance.

### 6. Audio is subordinate: it never fails a broadcast

Copied verbatim in spirit from `AudioLaneCore`'s contract (docs/20): *"an audio
failure tears down the lane only, never the video pipeline."* Native has to
work harder for it, because both media come out of one process.

| When | What happens |
|---|---|
| No candidate passes the pre-flight trial | Start video-only. `AudioState = "unavailable"`, logged once, surfaced in stats and the GUI. Not an error, not a notification. |
| The live pipeline dies inside its probe window **and** stderr names an audio element | Drop audio and retry **the same rung** immediately. |
| The whole cascade fails with audio on | Re-run the cascade once with audio off before declaring failure. A machine that cannot encode video must not be told the problem is its sound card. |
| An audio element dies **mid-session** | Session-fatal in v1 — documented, not hidden. See below. |

**The mid-session hole is real and stated rather than papered over.** One
child means one failure domain: today a `pulsesrc` whose device disappears
takes the broadcast with it. Three things make that acceptable for v1: the
primary candidate (Decision 2) *follows* the default sink, which removes the
common trigger (switching output device) rather than merely surviving it; the
engine already treats capture death as session-fatal and reports it; and the
Decision 4 fallback — audio in its own pipe — is also the upgrade path to real
isolation if this bites. A **two-child** design (a second `gst-launch` for
audio, independently restartable) is the eventual answer if it does, and it is
deliberately *not* v1: it buys isolation at the cost of the shared clock, and
the shared clock is the thing R15 was built around.

### 7. Audio is an outer cascade dimension, never a per-rung one

The existing cascade is 3 encoders × 3 capture modes, each with a 3 s live
probe window. Making audio a third dimension would make it 18 attempts and a
worst case near a minute of startup — a broadcaster staring at nothing while
the app quietly tries combinations.

So audio is not part of the cascade product. It is: run the cascade **with**
audio; on the first live failure whose stderr implicates an audio element, drop
audio for the rest of this start; if everything fails, one clean re-run without
audio. Worst case grows by one cascade pass, not by a factor of two, and only
on a machine that is already failing.

Attribution reuses machinery that exists: `child.stderrTail()` already retains
the last 20 stderr lines specifically so the cascade can tell *capture*
failures from *encoder* failures (`allFailuresInsidePipeWireSrc`). Audio is a
third bucket for the same evidence, matched on the audio element names the
pipeline builder itself chose — never on free-text guessing.

### 8. The engine grows an *optional* `AudioSource` interface

```go
type AudioPacket struct {
    Data        []byte // one Opus packet
    TimestampUs uint64 // engine clock, shared anchor (Decision 5)
}

type AudioFormat struct {
    Codec      string // "opus"
    SampleRate int    // 48000
    Channels   int    // 2
    BitrateBps int    // 128000
    Source     string // the winning cascade element, for stats
}

// Implemented by media sources that carry audio. The engine type-asserts;
// a source that does not implement it is video-only, which stays a
// first-class shape rather than a degraded one.
type AudioSource interface {
    Audio() <-chan AudioPacket
    AudioFormat() (AudioFormat, bool)
}
```

Widening `MediaSource` itself was the obvious alternative and is rejected: it
would force every implementation — the test fakes, and `internal/pubsim` — to
grow audio methods they have nothing to say about. The optional interface keeps
**video-only as a first-class shape**, which is not merely tidy: R20 tier-1
asserts the no-audio path continuously (docs/20's CI note), and that assertion
is only meaningful while a video-only source is a real thing rather than an
audio source returning nil.

### 9. The sender mirrors `AudioPacketizer`, and drops audio like audio

`sender.sendAudio` is the Go mirror of `gawk-app/src/media/audio-lane.ts`'s
`AudioPacketizer`, deliberately so that the two broadcasters stay legible to
each other (docs/19's standing rule):

- Its **own uint32 seq space**, independent of video frameIDs, advanced with
  the same wrap-aware successor rule.
- **One datagram per packet. Never chunked.** A 320 B packet has no chunking
  story, and the wire has no reassembly for one. A packet that somehow exceeds
  `wire.MaxAudioPayload` is dropped and counted, not split.
- **`AudioConfig` piggybacked at 1 Hz** on the packet flow — no separate timer,
  because 50 packets/s is already a scheduler (docs/20 Decision 5). Audio has
  no keyframe to anchor re-emits to; repetition is the lossy-tolerance story.
- A send failure **drops that packet and counts it**, and touches no video
  counter. Audio never triggers the video frame-drop path, never affects
  `FramesDroppedAtSend`, and never shrinks the video chunk budget.

Queue policy between the demuxer and the sender: a small bounded channel
(~32 packets ≈ 640 ms) that **evicts the oldest** on overflow. That is the
opposite of the video path's drop-newest, and for the reason docs/24 finding 14
established: for an in-order, live-edge stream, shedding the *backlog* keeps the
consumer near live, while shedding the newcomer strands it as far behind as the
queue is deep. Audio has no GOP, so there is nothing to poison — dropping one
packet costs one 20 ms concealment on the viewer, which its buffer already
knows how to do.

### 10. Trust the bitstream: verify the config against the Opus TOC

`ensureConfig` already refuses to assume video's codec string and parses it from
the SPS (docs/19 Decision 8). Audio gets the same treatment in the form
available to it: the first Opus packet's **TOC byte** states the stereo flag
and the frame duration, so the engine checks the config it is about to
advertise (48 kHz / 2 ch / 20 ms) against what the encoder actually produced.

A disagreement means the caps filter did not do what we told it to. The
response is to **log loudly and mark audio errored**, not to ship a config that
lies about the stream — a viewer configured for stereo that receives mono
produces a confusing bug report three layers away from its cause. The check is
~10 lines and pays for itself the first time a distro's `opusenc` defaults
differ.

### 11. Audio is on by default; `-audio=false` turns it off; no mid-session toggle

The browser lane shipped experimental and default-off, then graduated by
removing its toggle once it worked (docs/20 "Graduation", user decision
2026-07-23). The native lane starts where the browser one ended up: **on by
default**, because a broadcaster who wanted silence would not have installed a
screen-sharing app, and because Decision 6 makes "on" safe on machines where it
cannot work.

`-audio=false` (and a `DisableAudio` config key, so the zero value means on)
exists for the broadcaster who genuinely wants silence — not as a workaround
for breakage, which Decision 6 handles without user involvement.

**No mid-session toggle**, matching R15's one R13 exception: audio is decided at
start. Here the reason is not `getDisplayMedia`, it is that changing the
element graph means restarting the child, which means restarting capture — and
docs/18's "no settings change ever restarts the stream" cuts the other way, so
the answer is not to offer it.

### 12. `gawk-pubsim` gets a committed Opus fixture, behind a default-off flag

docs/20 left this as a possible later stretch ("an audio-capable fixture is out
of scope"). It is in scope here, because R25 is the change that makes it
cheap: once `internal/pubsim` implements Decision 8's `AudioSource`, the CI
harness exercises the **real engine send path** — seq stamping, the 1 Hz config
cadence, the wire encoding — and not merely the viewer.

Design (full acceptance criteria in NA7):

- The fixture is **committed bytes**, never generated at run time. No libopus
  in CI, no cgo, deterministic across runs — the same rule
  `internal/fixture/sample.ts` already follows.
- Framing on disk is `uint16` big-endian length prefix + packet, repeated: the
  same shape as R19's carrier records, so there is no new parser concept in the
  repo. Generation recipe, documented in the package and reproducible offline:
  `opusenc`'s GStreamer element emits **exactly one Opus packet per buffer**, so
  `... ! opusenc ... ! multifilesink location=%05d.raw` writes one packet per
  file and the prefixing step is a `cat` loop — no Ogg page parser anywhere.
- Content is a short (~2 s) loop of a distinctive two-tone pattern, so a human
  running the harness locally can hear whether it is right, and a future check
  could correlate it.
- **The flag defaults to off.** docs/25's tier-1 currently asserts the
  no-audio path stays intact; that assertion must keep running. Audio is a
  *second* viewer pass, following the docs/25 finding 16 precedent (the
  resilient-mode pass repeats the viewer run with one flag seeded).
- Timestamps come from the engine `Clock` at emit time, paced at 20 ms, from
  the same clock instance the video AUs use — pubsim must not weaken the
  Decision 5 invariant just because its packets are canned, exactly as its
  doc comment already says about video stamps.

## Chunks

| Chunk | Work | Acceptance criteria |
|---|---|---|
| **NA1** | **Spike on the gaming PC.** Which source candidate resolves; does `mpegtsmux` carry Opus in this pipeline; what does its PES payload actually look like. | A `GAWK_DUMP_TS` capture containing both streams; `gst-launch … ! tsdemux ! opusparse ! fakesink -v` round-trips it; the PES payload's control-header layout is written down as bytes. **Decision 4 is confirmed or the fallback is taken — recorded in this doc either way.** |
| **NA2** | Audio source cascade + injectable trial + last-good caching, mirroring `SelectEncoder`/`TrialFunc`. | Table-driven tests over candidate args and cascade order with a fake trial; a failing candidate advances, an explicit `-audio-device` pins exactly one; no hardware needed. Trial runs before the portal handshake (test asserts the ordering). |
| **NA3** | `BuildPipeline` grows the audio branch; `BuildTrialPipeline` gains its audio form. | Arg-shape tests in `pipeline_test.go`'s idiom, including one pinning each Decision 3 property with its reason. **Audio off produces byte-identical args to today** — asserted, not assumed. |
| **NA4** | `internal/mpegts` learns Opus: audio PID from the PMT (`stream_type 0x06` + `"Opus"` registration descriptor), PES `0xBD`, control-header strip, packets out via a second callback. | Test-first against committed TS bytes from NA1. Malformed/truncated headers drop the packet without disturbing video. A video-only stream produces zero audio callbacks and identical video output (regression pin). |
| **NA5** | Engine lane: `AudioPacket`/`AudioFormat`/`AudioSource`, the shared anchor wiring, `sender.sendAudio` (seq, 1 Hz config, no chunking, drop-oldest), TOC check, `Stats` fields. | Fakes-based unit tests for seq/config cadence and drop accounting; a test pins **one anchor for both media** (Decision 5); `relay_integration_test.go` extended to assert audio datagrams reach a subscriber with the config ahead of them. |
| **NA6** | Shells: `-audio=false`, `-audio-device`, CLI stats line, diagnostics JSON, GUI audio status row, config keys (`DisableAudio`, `AudioDevice`, `LastGoodAudioSource`), INSTALL package names per candidate. | Existing shell tests extended; diagnostics JSON carries the audio block; GUI renders "unavailable" without an audio source present. |
| **NA7** | **Audio-capable `gawk-pubsim`** (Decision 12) + tier-1 CI pass. | Committed fixture + framing split with tests; `-audio` flag default **off**; a second tier-1 viewer pass asserts, flow-shaped: `audioPacketsReceived > 0`, `audioPacketsDecoded ≈ received`, audio reported present, and finding-8's `overflowDrops`/`gapsSkipped` near zero. **Kill criterion**: if headless Chrome cannot drive an `AudioWorklet`, assert the worker-side decode counters only, record the sink rows as unasserted, and **say so in the harness output** (docs/25's no-silent-caps rule) — degrade the assertion, don't drop the chunk. |
| **NA8** | Verification pass: automated gates in both Go modules, then the on-hardware A/V run. | `gofmt`/`go vet`/`go test -race` green in `gawk-server` and `gawk-broadcast`; on the gaming PC, the three goal criteria at the top of this doc, plus a listening check on `audio-type=restricted-lowdelay` (Decision 3). Findings recorded here in docs/20's numbered-findings style. |

NA1 gates NA3 and NA4 (it decides the framing). NA2 and NA5 are independent of
it and can proceed in parallel. NA7 depends on NA5 only — the fixture path never
touches GStreamer.

## Verification plan

**Automated** (every chunk, per CODE-REVIEW.md's test-first rule — the failing
test lands before the fix, always):

- Both Go modules' gates. The `wirecheck` audio vectors already exist and must
  stay green untouched — if this milestone needs to change a wire vector,
  something has gone wrong upstream of it.
- The relay integration test grows an audio assertion (NA5): a subscriber
  receives `AudioConfig` before the first `AudioFrame`, and frames arrive with
  monotonic seq.
- Tier-1 CI grows one audio viewer pass (NA7) while keeping the existing
  no-audio pass.

**Manual, on the gaming PC** (not CI-reachable — no PipeWire, no sound device,
no GPU on the runners). Sequenced after R15's pending hardware
re-verification, per the goal section's caveat on `avSkewMs`:

1. Start a broadcast, join from a browser: audio plays, `avSkewMs` inside the
   goal criteria over 60 s.
2. Switch the default output device mid-broadcast. Expected: audio follows
   (candidate 1). Recorded either way — this is the Decision 6 hole's field
   test.
3. Start with the sound server stopped: video publishes, `AudioState =
   "unavailable"`, no error dialog, no notification.
4. `-audio=false`: no audio elements in the pipeline, wire bytes unchanged from
   today's build.
5. A listening pass on `restricted-lowdelay` versus `generic` at 128 kbps on
   real game audio (Decision 3's one judgement call).

## Risks and open questions

Three facts this design leans on that were **not** verified at design time.
They are listed here rather than assumed, and NA1 answers all three in one
sitting:

1. **Does GStreamer write the Opus-in-TS control header the way the mapping
   spec describes?** `tsmuxstream.c` was read and confirms the stream ID,
   stream type and descriptors (Decision 4); the payload-prefix code lives in
   `gst_base_ts_mux_prepare_opus`, which was not read. If the layout differs,
   NA4 changes; nothing else does.
2. **Does `pipewiresrc` forward `stream.capture.sink=true` into the stream
   properties WirePlumber routes on?** If not, candidate 1 degrades to
   candidate 2 and Decision 6's mid-session hole gets wider — worth knowing
   before NA2 is written, not after.
3. **Does pipewire-pulse resolve `@DEFAULT_MONITOR@`?** Only relevant if 2
   fails.

Two risks that are design-level rather than factual:

- **`GstAggregator` latency with an idle video pad.** Decision 4's reasoning
  says live aggregation times out against the clock rather than waiting for
  damage-driven video. If that is wrong in practice, audio latency will track
  screen stillness — a symptom that would be bizarre to debug without this
  paragraph. NA1 should watch audio delivery cadence on a **static** screen
  specifically.
- **One process, one failure domain** (Decision 6). Stated, bounded, with the
  upgrade path named.

## Non-goals

- **Microphone / voice mixing.** Game audio only, same as R15.
- **Per-broadcast audio bitrate or codec settings.** A constant in v1, like the
  browser's.
- **Mid-session audio toggling** (Decision 11).
- **Audio in the encoder ladder or R4-style adaptive fallback.** Audio is
  128 kbps of a 16 Mbps stream; adapting it would save nothing and add a
  control loop.
- **Windows/macOS native broadcasters.** `gawk-broadcast` is Linux-only by
  construction (docs/19); the browser covers the rest.
- **Making the relay or viewer aware that a broadcast is native.** They already
  cannot tell, and that is the property this milestone is built to preserve.

## Rejected

- **Encoding Opus in Go (cgo bindings to libopus).** Puts C headers on
  `gawk-pubsim`'s build path and breaks the CI harness's reason for existing
  (Decision 3).
- **A second GStreamer child for audio.** Better isolation, worse sync: two
  pipeline clocks means two anchors means a constant lip-sync bias with no
  mechanism to measure or remove it (Decision 5). Named as the upgrade path if
  Decision 6's hole proves painful in the field.
- **Widening `MediaSource` with audio methods.** Forces audio semantics onto
  the fakes and onto `pubsim`, and erodes video-only as a first-class shape —
  which is the shape R20 tier-1 continuously asserts (Decision 8).
- **A per-rung audio dimension in the cascade.** 18 attempts and a
  minute-long worst case, to handle a failure mode that one outer retry covers
  (Decision 7).
- **Raw PCM over the pipe with encode in Go.** Same cgo problem as above, plus
  ~1.5 Mbps through a pipe to save an element.
- **Capturing audio through the portal.** There is nothing there to capture
  (Decision 1). Do not go looking for a portal flag; the ScreenCast interface
  has no audio.
- **Generating the CI fixture at run time.** Non-deterministic, and needs an
  encoder in the runner. Committed bytes, like `sample.ts` (Decision 12).
