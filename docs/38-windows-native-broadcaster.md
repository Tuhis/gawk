# R34 — Native Windows broadcaster (`gawk-broadcast-windows`)

**Status**: designed 2026-07-30 (owner decisions taken the same day, listed in
§2). WB0 (scaffold, CI, release wiring), WB1 (the wire mirror) and WB2
(transport + engine core; wtransport gates measured — see §11 — and the
real-relay integration suite green) implemented 2026-07-30. **WB3–WB8
implemented 2026-07-31** (capture, encode cascade, audio, Slint GUI,
telemetry reporter, packaging/docs): every CI-verifiable criterion is green
on the Windows runner; the criteria marked *manual* in §8 — G1/G2/G6–G9 and
the §10 V-register — remain open until the on-hardware pass on the gaming
PC.

A Windows counterpart to the Linux native broadcaster (R14, `gawk-broadcast`),
with two capture modes selectable at start — **share one application** (its
window plus that app's own audio, independent of everything else on the
desktop) or **share the whole desktop** (plus system audio) — hardware video
encode only, and zero relay/wire-format/viewer changes.

This document leans heavily on three earlier ones and does not restate them:

- `docs/19` — the R14 Linux design. Every decision here either inherits one of
  its decisions or names the divergence and why.
- `docs/28` — the R25 audio design. The Opus parameters, the audio-subordinate
  stance and the single-clock invariant are inherited verbatim.
- `ROADMAP.md` §R34 — the motivation and the "not a Go sibling" analysis that
  this doc turns into concrete decisions.

## 1. Why (recap) and what "done" means

R14 exists because the browser structurally cannot hardware-encode on Linux.
That is **not** true on Windows — WebCodecs hardware encode ships there. The
driver for R34 is **capture fidelity, not encode capability**:

- `getDisplayMedia` is whole-screen-preferred by design and exposes **no
  per-application audio at all** — Chrome's window capture cannot give you
  that window's audio in isolation, only whole-system loopback.
- Windows has had a per-process audio-loopback API since Windows 10 2004 that
  no browser surfaces. A native app is the only way to offer "just this game,
  video and audio, nothing else on my desktop".
- The browser's picker cannot capture a window that is occluded, and requires
  alt-tabbing out of a fullscreen game to drive it.

### Milestone acceptance criteria

Per the repo convention, these are pre-registered up front. "Manual" means on
real hardware (the gaming PC / a Windows 10 2004+ machine); CI structurally
cannot see a GPU, a hardware encoder, or an interactive desktop, so — exactly
as R14's doc warns — the manual criteria are the ones that decide whether R34
works.

| # | Goal | Verified by |
|---|---|---|
| G1 | Mode 1: sharing one app streams that window's video and **only** that app's audio (a second app playing audio on the same PC is inaudible to viewers) | manual, on the gaming PC |
| G2 | Mode 2: sharing a monitor streams the desktop and whole-system audio, surviving a default-output-device switch mid-broadcast (or failing audio-only) | manual |
| G3 | Hardware encode only: with no hardware H.264 encoder, the app refuses with the curated message and points at the browser broadcaster; it never software-encodes | unit (faked enumeration) + manual on a GPU-less VM |
| G4 | The relay and viewer are untouched: a stock viewer at `gawk.ioio.fi` plays the stream with zero diffs outside `gawk-broadcast-windows/` (plus docs and the relay origin-allowlist value) | integration vs. the real `gawk-server` binary + review |
| G5 | Wire parity: every golden vector is byte-identical with `gawk-server/wire`, `wire.ts`, and `gawk-broadcast/internal/wirecheck` | unit test |
| G6 | Glass-to-glass latency is sub-500 ms on the hardware path, measured with R14 V4's photographed-reference method | manual, photographed reference |
| G7 | A/V sync: viewer-reported median `|avSkewMs| ≤ 60 ms`, p95 `≤ 120 ms` over 60 s (R25's criteria, unchanged) | manual, viewer diagnostics |
| G8 | Zero-config first run: launch → pick a window → Start reaches the production fleet with no settings touched | manual |
| G9 | The broadcast survives a relay pod restart or rolling drain with an automatic resume (≤ a few seconds of frozen video, no viewer reconnect) | integration (kill/restart relay) + manual during a fleet rollout |
| G10 | The Windows CI job runs **only** when `gawk-broadcast-windows/**`, `gawk-server/wire/**` or its own workflow changes | CI, observed on a docs-only PR |

## 2. Owner decisions (taken 2026-07-30)

Recorded here so the rest of the doc can build on them without hedging:

| # | Decision | Choice |
|---|---|---|
| OD1 | Implementation language | **Rust** (decided by WebTransport client availability, per the ROADMAP analysis) |
| OD2 | Encode strategy | **Media Foundation only** — vendor-neutral; NVENC/AMF/QuickSync SDKs stay a documented escape hatch, not day-one scope |
| OD3 | GUI stack | **Slint** |
| OD4 | Media plumbing | **Direct Windows APIs** (WGC + WASAPI + MF), no GStreamer |
| OD5 | Settings surface | **Fixed rung + advanced overrides** (mirrors the Linux GUI's surface) |
| OD6 | Packaging | **Portable single EXE** — no installer, no winget, no auto-update |
| OD7 | Tray icon / global hotkeys | **Neither** (match R14 Decision 15; ROADMAP already rules re-litigating them out of scope) |
| OD8 | Microphone | **Out of scope** — captured audio only, like every other gawk broadcaster |
| OD9 | Windows floor | **Windows 10 2004 (build 19041)+**, with runtime feature checks for newer-than-floor APIs |
| OD10 | Telemetry (R28) | **In scope day one**, mirroring the Linux reporter |
| OD11 | CI | **Self-hosted Linux runners, cross-compiled to msvc with cargo-xwin** — superseded the original `windows-latest` choice once it was measured that Linux loses no test or lint signal (revised 2026-07-31; see D18) |
| OD12 | CLI / headless shell | **GUI only day one**; the engine crate stays shell-free so a CLI or pubsim-style publisher can be added without redesign |
| OD13 | Rejected-CONNECT status (gate 2a failed on stock wtransport, §11 F-1) | **Vendor + patch wtransport in-repo** (`vendor/`, `[patch.crates-io]`) rather than a public fork or living without the status — decided 2026-07-30 when the gate was measured |

## 3. Non-goals

- **Software encode fallback** — refusal is the correct behavior (R14
  Decision 4's stance, adapted: see D9 for why the refusal message differs).
- **Mic capture/mixing, tray icon, global hotkeys, CLI shell** — OD7/OD8/OD12.
- **HEVC or AV1** — the viewer's negotiated codec list is H.264 → VP9 → VP8;
  hardware H.264 MFTs are universal on the target GPUs. A codec change is a
  wire-compatible follow-up, not R34.
- **A ladder, auto-fallback, or mid-session rung changes** — R14 Decision 9
  holds; nothing here re-opens it.
- **Any change to relay, wire format, or viewer** — only who else speaks the
  protocol changes. The single production-side change permitted is one line in
  the relay's origin allowlist (D19).
- **macOS** — per the ROADMAP: it would need its own ScreenCaptureKit research
  to earn an item.
- **Installer, code signing, winget, auto-update** — OD6. A version-check
  toast is a possible later convenience, not R34.
- **Sharing code, GUI, build, or release pipeline with the Linux
  `gawk-broadcast`** — the toolchains no longer overlap enough to force it.

## 4. Decisions

### D1 — A new top-level component: `gawk-broadcast-windows/`, a Cargo workspace

Mirrors *why* R14 made `gawk-broadcast` a separate Go module (Decision 1:
isolate an unrelated toolchain from the relay's CI and release cadence), with
a starker toolchain split. Own directory, own `Cargo.toml` workspace, own CI
job, own release-please component (`release-type: rust`, tags
`gawk-broadcast-windows-vX.Y.Z` inside the one combined release PR — the
manifest-mode constraint from CLAUDE.md stands). Like the Linux broadcaster
(R14 Decision 2): **no container image, no Helm chart, no publish job, no CI
deploy** — this is a binary you run on your own gaming PC.

### D2 — Rust, and the WebTransport client is `wtransport`, with two pinned gates

The ROADMAP's analysis is adopted as-is: the QUIC/WebTransport client decides
the language, and only Rust has real options. Primary: the `wtransport` crate
(dedicated, actively maintained, tokio-based, datagram + uni-stream support).
Pre-registered fallback: `web-transport-quinn`.

Two engine-critical capabilities are **not** assumed — they are WB2 acceptance
criteria, verified before anything is built on them:

1. **A custom `Origin` header on the CONNECT request.** The relay's
   `-allowed-origins` check matches an absent Origin against nothing
   (`gawk-broadcast/internal/engine/relay.go:81-96` — a real field bug on
   Linux). If `wtransport` can't set headers, that alone forces the fallback
   library.
2. **Visibility of the rejection HTTP status and the session close code.**
   `StartError{Phase, Status}` parity needs the status of a refused CONNECT
   (401/403/404/409/429), and the resume supervisor needs to distinguish
   close codes 4000/4004 (terminal) from 4002/abrupt death (resumable).
   webtransport-go hides the close code and the Go engine ports a workaround
   (`resume.go:164-184`); if `wtransport` hides it too, port the same
   workaround rather than weakening the supervisor.

### D3 — Direct Windows APIs, in-process; the R14 subprocess boundary is deliberately not kept

R14's GStreamer-subprocess + MPEG-TS-pipe design bought two things: cgo
avoidance (moot in Rust) and crash isolation for flaky NVIDIA-on-Wayland
drivers. Here the capture/encode stack is the OS's own (WGC, WASAPI, MF) —
the same binaries every Windows game-capture app exercises — and the crash
surface a subprocess would isolate is much smaller. In exchange we delete an
entire layer: no child process supervision, no MPEG-TS mux/demux, no pipe
framing, no stderr-tail attribution, no PTS anchor (see D7). Encoded H.264
access units come straight off the MFT with their sample times.

Panic hygiene replaces process isolation: every media thread (capture pump,
audio pump, encoder drain) runs under `catch_unwind`; a panic surfaces as a
session error through the same path as any other failure, ends the broadcast
cleanly, and raises a critical notification — it does not take the GUI down
silently or leave a zombie capture running (the exact failure class R14's
`finish()` incident documents).

### D4 — The `wire` crate is the fourth mirror; golden vectors are restated, never shared

The wire protocol becomes a third independent *reimplementation* (Go relay ↔
TS frontend ↔ this). The discipline is exactly `gawk-broadcast/internal/
wirecheck`'s: every golden vector from `gawk-server/wire/wire_test.go`
(plus `parity_test.go`, `stripe_test.go`) is **restated as literal hex in the
Rust tests** — "an exported fixture the two sides share could be edited once
and stay green in both places, which would defeat the purpose."

Parsers are strict and zero-copy like the Go originals: exact lengths for
fixed-size messages, reserved bits rejected not masked, payload slices
borrowing the input.

Producer-side message matrix (what this app must speak):

| Sends | Receives (uni streams, dispatched **by type**, never by order) | Receives (datagrams) |
|---|---|---|
| 0x01 VideoChunk (deltas, 20 B header, ≤1180 B payload) | 0x03 BroadcastAnnounce | 0x05 TimeSync pong |
| 0x02 DecoderConfig — only ever **embedded** in 0x04 | 0x09 ResumeToken | 0x0B ViewerCount |
| 0x04 StreamFrame (keyframes, reliable uni stream) | 0x0F RelayCapabilities (absence ⇒ no parity, byte-identical to pre-R29) | |
| 0x05 TimeSync ping @ 2 s | 0x0D TelemetryHello | |
| 0x06 ClockMapping @ 5 s | | |
| 0x07 AudioFrame (one Opus packet per datagram, never chunked) | | |
| 0x08 AudioConfig @ 1 Hz piggybacked | | |
| 0x0E ParityChunk (deltas only, gated on `CapParityChunks`) | | |

The uni-stream dispatch-by-type rule is load-bearing: docs/22 finding 9
measured the resume token beating the announce in about half of real dials.
Announce reads are bounded (≤ 258 B, 10 s deadline) and detached from `start`
exactly as the Go engine's are. Unknown types are ignored so a newer relay
can't break the client.

When WB1 lands, CLAUDE.md's mirror rule ("mirrored in the TS and
`gawk-broadcast` checks") must be extended to name this fourth mirror.

### D5 — Engine semantics are a port of the Go engine, not a redesign

"Same idea, same names, other language" (R14's legibility rule, now spanning
three languages: `BroadcastSessionLike`/`Callbacks` in TS, `Session`/
`Callbacks` in Go, and the same shapes here). Anything the relay or viewer
can observe behaves identically. The parity table — each row is an existing,
field-tested behavior with its home noted, and each is a WB2 test:

| Behavior | Source of truth |
|---|---|
| Publish URL: `CONNECT /publish` (mint) / `/publish/{id}` (reclaim); `secret` and `resume` (hex) as **query params** — the relay only reads query params | `engine.PublishURL`, `relay.go:115-143` |
| Delta chunking starts at `MaxChunkPayload` (1180) and **only ever shrinks**, once, on a datagram-too-large error; the new size sticks | `send.go:28-61` |
| A mid-frame datagram failure drops the frame's remaining chunks and counts `framesDroppedAtSend` | `send.go:24-27` |
| Keyframes ride reliable uni streams, ≤ 1 in flight; a newer keyframe resets the older's stream (supersede code 1) and counts it | `send.go` keyframe path |
| The DecoderConfig is embedded in **every** StreamFrame and never sent as a standalone datagram — it is what the relay's priming caches | `send.go:71-79` |
| `frameID` starts at 0, wraps as serial arithmetic, and **survives resume** — continuous frameIDs are what tell the relay a reclaim is a resume, not a restart | `send.go:140-175` |
| Producer video queue: bounded (8 frames), never blocks capture; a dropped delta drops the rest of its GOP; a keyframe into a full queue flushes the stale backlog | `source.go:645-685` |
| Audio queue: bounded (~32 packets ≈ 640 ms), **drop-oldest** — deliberately the opposite of video | docs/28 Decision 9 |
| Parity symbols (R29): GF(256) P/Q over delta chunk **payloads**, ≤ `MaxParityDataChunks` (255) data chunks, trailing the data; a parity send failure is not a frame failure | `parity.go`, `send.go` |
| Auto-resume: 250 ms → 5 s doubling backoff, 5 min window; terminal close codes **4000/4004**; terminal HTTP statuses **401/403/404/409**; everything else (incl. status 0) retries | `resume.go` |
| A reclaim is never a mint: no announce ⇒ no code to reclaim ⇒ give up (R1's orphaned-viewers bug) | `resume.go:112-118` |
| On resume: TimeSync estimate discarded (different pod ⇒ different clock), ClockMapping re-armed, AudioConfig cadence reset so the next packet re-primes | `resume.go:246-257`, `send.go:154-175` |
| The persisted resume token is only sent when the ID being claimed matches the ID it was minted for | both Linux shells |
| TimeSync @ 2 s, ClockMapping @ 5 s, AudioConfig @ 1 Hz | `timesync.go`, docs/28 D9 |
| QUIC keepalive 10 s (the browser advertises ~30 s idle; keepalive is what holds sessions open) | `relay.go:161` |

One deliberate **improvement** over the Go engine: on resume, force an IDR
(`CODECAPI_AVEncVideoForceKeyFrame`) so the relay's invalidated keyframe cache
re-primes immediately instead of waiting out the GOP. The Linux engine
couldn't do this (gst-launch can't inject force-key-unit events — docs/19
deviation notes); we own the encoder, so we can.

### D6 — Capture is Windows.Graphics.Capture, both modes; the picker is ours, and that is forced, not a preference

- **Mode 1 (single app)**: `GraphicsCaptureItem` from an `HWND` via
  `IGraphicsCaptureItemInterop::CreateForWindow`, frames from a
  **free-threaded** `Direct3D11CaptureFramePool` (no DispatcherQueue needed),
  format `B8G8R8A8UIntNormalized`.
- **Mode 2 (whole desktop)**: same machinery via `CreateForMonitor`. DXGI
  Desktop Duplication is the pre-registered fallback if WGC monitor capture
  disappoints on hardware; it is not built speculatively.
- **The in-app picker is structurally required.** The system
  `GraphicsCapturePicker` returns an opaque `GraphicsCaptureItem` — **no HWND,
  no PID** — and process-loopback audio needs the PID. So the app enumerates
  alt-tab-eligible top-level windows itself (`EnumWindows` + cloaking/tool-
  window filters, title + icon) and monitors (name, resolution), and builds
  `GraphicsCaptureItem`s directly. This also gives re-pick and a better UX
  than the system dialog.
- **Cursor embedded** (`IsCursorCaptureEnabled = true`) — R14 invariant.
- **Capture border**: `GraphicsCaptureSession.IsBorderRequired = false` where
  the API exists (build 20348+ — effectively Windows 11 for consumers; the
  19041 floor does not have it). Runtime `ApiInformation` check; on plain
  Windows 10 the yellow border stays and that is documented, not fought.
- **VFR discipline**: WGC is damage-driven, like PipeWire. Frames pass
  through drop-only gating to the target fps — never synthesize CFR (R14
  Decision 13's invariant). Timestamps come from the frame's
  `SystemRelativeTime` (QPC-based).
- **Minimized windows**: WGC delivers no frames for minimized windows (they
  are not composited). The ROADMAP's "works even when minimized" claim is
  recorded here as **optimistic pending verification** (V-2 in §10); the
  designed behavior is honest: viewers see the last frame, the GUI shows a
  "window is minimized — restore it to resume" hint keyed off `IsIconic`.
  Occluded (covered but not minimized) capture does work and is the actual
  selling point.
- **HDR**: when the source is HDR, an 8-bit BGRA frame pool yields
  clamped/washed-out output. V1 requests BGRA8 and measures (V-5); the
  pre-registered fix, if needed, is a `R16G16B16A16Float` pool plus a D3D11
  VideoProcessor HDR→SDR tone-map pass — the same class of surprise as
  R14's 10-bit-HDR capture failure, handled the same way: a ladder rung, not
  an assumption.

### D7 — One clock: QPC end to end; the "single anchor" invariant becomes structural

R25's load-bearing sync decision (one `ptsAnchor`, never two) exists because
the Linux pipeline's PTS timeline was the child's, not ours. Here there is no
child: video frames carry QPC (`SystemRelativeTime`), WASAPI capture packets
carry QPC (`u64QPCPosition` from `GetBuffer`), and TimeSync's `t0` reads QPC.
Both media are stamped from **the same monotonic clock at the source**, so the
affine mapping is the identity and relative A/V skew is zero by construction —
the invariant R25 enforces with `TestOneAnchorStampsBothMedia` is enforced
here by there being exactly one clock function in the engine crate, plus a
unit test that a scripted capture pair (video 5 ms late, audio 1 ms late, as
in R25's mutation test) produces timestamps that preserve source spacing.

Caveat pinned as verification V-4: process-loopback streams may report zero
QPC positions (Microsoft's own sample hedges). Pre-registered fallback:
stamp audio on arrival minus the buffered duration, which reproduces the
Linux arrival-stamping model and its measured bias gate (R14 Decision 6:
> 15 ms or non-constant bias ⇒ fix the stamping, don't ship the skew).

### D8 — Audio: WASAPI process loopback (mode 1) / endpoint loopback (mode 2); R25's Opus contract verbatim; audio never fails a broadcast

**Mode 1** — `ActivateAudioInterfaceAsync` with
`AUDIOCLIENT_ACTIVATION_PARAMS{PROCESS_LOOPBACK, TargetProcessId,
PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE}`. This is the
Windows-only capability the whole milestone exists for. Two properties to
note: the capture format is **ours to specify** (the mix is rendered into the
requested format — 48 kHz / stereo / f32 directly, no resampler dependency),
and capture follows the *process*, not an endpoint, so a headphone/speaker
switch should be a non-event (verify: V-3b). The open question the ROADMAP
already flags — **does the process tree actually cover games that route audio
through launcher/anti-cheat helpers outside their own tree?** — is V-3a, the
single most important on-hardware verification in R34. The UX handles the
failure honestly: a live level meter in the audio line; if the captured app
stays silent for ~10 s while broadcasting, a hint appears — "No audio from
&lt;App&gt; yet. Some games play audio through a helper process this capture
can't see — switch to whole-system audio?" — with a one-click switch (the
one permitted mid-session audio change, because it is a new capture, not a
renegotiation: same Opus stream, same seq space, viewers notice nothing).

**Mode 2** — ordinary endpoint loopback (`AUDCLNT_STREAMFLAGS_LOOPBACK`) on
the default render device, at the mix format with
`AUTOCONVERTPCM | SRC_DEFAULT_QUALITY` requesting 48 kHz stereo. Default-
device changes are followed via `IMMNotificationClient` — tear down and
re-open on the new default, mirroring the "follows the default sink" property
that made `pipewire-monitor` the top Linux rung. A re-open gap is dropped
packets, which the wire model already treats as truth.

**Encode** — libopus via a vendored static build (fits the single-EXE story).
Every R25 parameter and its reason carries over unchanged: 48 kHz/2 ch
**forced** (channel-mapping-family-1 multistream Opus would break WebCodecs
with gawk's empty description), 128 kbps **constant — not a setting**, 20 ms
frames (one packet per datagram, no chunking; oversize ⇒ drop and count),
`dtx=false` (silence must not look like loss), `inband-fec=false`,
`OPUS_APPLICATION_RESTRICTED_LOWDELAY`. The first packet's TOC byte is
verified against the config (R25 Decision 10): disagreement logs loudly and
marks audio errored — never ship a lying config.

**Subordination** (R25 Decision 6): audio is probed before going live (a
short self-contained capture+encode trial, no picker, no GPU); any live audio
failure drops audio, notifies, and leaves video running. Audio-off on the
wire is byte-identical to a video-only broadcaster.

### D9 — Encode: Media Foundation hardware MFTs only, trial-gated, with R14's cascade discipline intact

**Vendor-neutral by construction**: the hardware H.264 encoder MFTs *are*
NVENC, AMF and QuickSync, exposed through one COM interface — one
integration, three vendors (OD2). Vendor SDKs remain a documented escape
hatch (the analogue of R14's V8 stage) if MF's knobs prove insufficient
against G6/G7 — a decision to be forced by measurement, not taken now.

- **Enumeration**: `MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
  MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER)`, H.264 output type.
  Software MFTs are never requested and never used.
- **Enumeration is not acceptance** — R14's core probing lesson
  (`gst-inspect` lists elements that fail at runtime) transfers verbatim: a
  listed MFT is accepted only after a **real trial encode** — ~30 synthetic
  NV12 D3D11 textures in, requirements on what comes out (table below). The
  trial never touches the capture session, mirroring "trials encode
  videotestsrc, never the portal".
- **Last-good encoder** is cached in the config and re-verified first at
  every startup; a live-pipeline failure advances the cascade to the next
  hardware MFT (per-user runtime decision, R14 Decision 4).
- **Zero-copy path**: one D3D11 device (created with
  `D3D11_CREATE_DEVICE_VIDEO_SUPPORT`, multithread-protected) shared by WGC,
  the D3D11 **VideoProcessor** doing BGRA→NV12 (the vendor-neutral GPU
  converter; some MFTs take RGB directly — probed, not assumed), and the MFT
  via `IMFDXGIDeviceManager`. Frames never round-trip through system memory.
- **Refusal**: with no candidate surviving trial, the app refuses to start.
  The message differs from Linux because the *reason* differs — on Linux the
  browser cannot hardware-encode and the app fills that hole; on Windows the
  browser hardware-encodes fine and what the app adds is capture fidelity:

  > No hardware H.264 encoder was found, so gawk-broadcast can't start —
  > it deliberately has no software encoder. The browser broadcaster at
  > &lt;app URL&gt; hardware-encodes fine on Windows; what you lose without
  > this app is per-application audio and background-window capture.

**Per-candidate acceptance criteria** (the R14 Decision 13 invariants table,
re-derived for MF):

| Invariant | Mechanism | Verified in trial by |
|---|---|---|
| No B-frames (decode order == presentation order) | `CODECAPI_AVEncMPVDefaultBPictureCount = 0` | output sample times strictly monotonic |
| Closed 500 ms GOP | `CODECAPI_AVEncMPVGOPSize = fps/2` (frames — wall-clock GOP stretches under damage-driven capture; **measured and displayed** like Linux, EMA α = 0.3, never assumed) | IDR spacing in trial bitstream |
| SPS/PPS in-band before **every** IDR | vendor MFTs differ (V-6); if absent, prepend the cached headers from `MF_MT_MPEG_SEQUENCE_HEADER` — load-bearing because extradata is empty (D10) | NAL scan of trial bitstream |
| ≤ 1 frame encoder-internal latency | `CODECAPI_AVLowLatencyMode = TRUE` + `MF_LOW_LATENCY` | input-count minus output-count at drain |
| Rate control: peak-constrained VBR, peak = user-facing bitrate, mean = 75 % | `eAVEncCommonRateControlMode_PeakConstrainedVBR`, `CommonMaxBitRate` = peak, `CommonMeanBitRate` = 75 % (Linux deviation 9 semantics, exactly) | properties accepted; bitrate sanity over trial |
| VFR pass-through | real QPC-derived sample times + durations on every `IMFSample`; nominal fps only in `MF_MT_FRAME_RATE` for rate-control budgeting (the Linux caps-carry-framerate lesson) | output timestamps equal input timestamps |
| Cursor embedded | WGC session property (capture-side) | — |
| On-demand IDR (for resume, D5) | `CODECAPI_AVEncVideoForceKeyFrame` | forced mid-trial, IDR observed |

### D10 — Annex-B with empty extradata; the codec string comes from the SPS, nothing else

MF encoder MFTs emit Annex-B byte streams — so this app joins the Linux
engine as an Annex-B publisher (the pair of them are the only things
exercising the viewer's `isAnnexB` branch; the browser always sends AVCC).
Consequences, inherited from `docs/19` Decision 8 and
`gawk-broadcast/internal/engine/sps.go`:

- The DecoderConfig's extradata is **empty, deliberately and permanently**;
  the viewer's start-code sniff routes decode accordingly and no avcC
  correction ever runs on this path.
- Therefore the `avc1.PPCCLL` codec string is **parsed from the first SPS
  NAL** — profile_idc, constraint flags, level_idc, read raw (the three bytes
  precede any legal emulation-prevention position) — never assumed from what
  we *asked* the encoder for, because the encoder may pick a different level.
  This string is the only thing telling the viewer's decoder what is coming.
- Keyframe classification = AU contains an IDR NAL (type 5), same as Linux.

### D11 — The rung: fixed 1080p60 / 500 ms GOP / 16 Mbps peak VBR, with the Linux GUI's advanced overrides

Defaults identical to `DefaultMediaConfig` on Linux: **1920×1080, 60 fps,
16 Mbps peak (≈75 % typical), 500 ms GOP**. No ladder, no auto-fallback, no
mid-session changes; settings apply to the next broadcast and the panel is
disabled while live (R14 Decision 9 + the Linux GUI's exact behavior).

Advanced overrides mirror the Linux GUI's surface, not R18's browser one:
resolution 2560×1440 / 1920×1080 / 1280×720 / 854×480, framerate
120 / 60 / 30 / 5, bitrate in Mbps (blank = 16, clamped [1, 100]). Scaling to
a non-native rung happens GPU-side in the VideoProcessor pass that already
exists for format conversion. GOP is not a setting anywhere in gawk.

### D12 — GUI: Slint, one window, the Linux app's information architecture plus the capture picker

**Slint** (OD3), under its **royalty-free desktop license** — the repo is
proprietary, so the GPLv3 edition is unusable; confirming the royalty-free
terms fit (attribution requirement included) is a WB0 review criterion, with
the paid license as the fallback if they don't.

The window layout keeps the Linux GUI's card architecture (it is field-tested
and R32 confirmed the settings-behind-disclosure pattern) with one structural
addition — the picker card — and Windows-native look and feel: Fluent-adjacent
spacing/typography, system light/dark theme followed, the gawk accent color
shared with `gawk-app` so the two broadcasters read as one product.

1. **Header** — state (Not broadcasting / Starting… / Live) + heartbeat dot
   (green live, amber while auto-resuming with "Reconnecting to the relay…
   (attempt N)"), encode line (`Media Foundation — NVIDIA H.264 Encoder MFT ·
   zero-copy · 1080p60`), audio line with live level meter, "N watching"
   (hidden until the relay's first 0x0B push).
2. **Capture picker** (idle only) — two tabs: **Share an app** (alt-tab
   window list, icon + title, refresh) / **Share a screen** (monitors with
   name and resolution). Selection is remembered per mode for the session;
   the picker re-appears on every start like the Linux portal does — no
   silent re-use of a stale grant.
3. **Controls** — `Start broadcast` (+ `Resume <CODE>` when a previous ID is
   persisted); a red `Stop broadcast` while starting/live.
4. **Code card** — the 6-char code in monospace ("read aloud and typed by
   hand"), the join link `https://gawk.ioio.fi/#/view/<CODE>` (from the
   resolved app URL), `Copy link`, transient "Link copied" confirmation.
5. **Live confidence thumbnail** (mode 1 only, live only) — a ~1 Hz
   downscaled sample of the outgoing frames. This is a **deliberate, narrow
   divergence from R14 Decision 16** ("no preview — you are looking at your
   own screen"): that rationale breaks exactly when capturing an occluded or
   background window, which is this mode's selling point — the broadcaster
   *cannot* see what viewers see. 1 Hz, reusing frames already in hand,
   downscaled in the existing VideoProcessor pass; not a player.
6. **Error card** — curated sentences (sentinel errors checked before generic
   `StartError` rendering — the Linux shadowing bug, ported as a rule), and
   `Start a new broadcast instead` offered **only** for connect-phase
   failures (R1's mint-fallback bug stays fixed in all shells).
7. **Settings** (collapsed) — Relay URL ("blank = the default relay"), App
   URL ("blank = the default"), Publish secret (masked, DPAPI-stored), Peak
   bitrate, Resolution, Framerate, Telemetry URL ("blank = default, `off` =
   send nothing"), plus the two always-visible computed captions from the
   Linux GUI: "Broadcasting to &lt;resolved relay&gt;" and "Diagnostics to
   &lt;resolved ingest | 'off — nothing is sent'&gt;" — a default that sends
   data must be visible without reading the README. Terms line + link,
   non-gating (TC5: running it is the acknowledgment).
8. **Stats** (Details disclosure) — the Linux rows, with one honest
   improvement: **Capture fps is real here** (we own capture; no more
   `n/a (the encoder child owns capture)`), plus capture mode, encoder,
   capture path (zero-copy / tone-mapped), codec, rung, encode/sent fps,
   keyframes (sent/failed/superseded), measured keyframe interval vs target,
   dropped at send, datagrams (+MB), RTT, audio state/format/packets, viewer
   count — and `Copy diagnostics` producing the same JSON shape as Linux
   (`kind: "gawk-broadcast-windows"`, nullable-pointer semantics for
   unknowns, browser-style lowerCamelCase counter names).

**Window-is-the-app** (R14 Decision 15): closing the window ends the
broadcast; no tray, no background presence. One guard the Linux app lacks:
closing while **live** asks "You're live — stop the broadcast and quit?"
(fullscreen-game alt-tab misclicks make accidental closes likelier on the
target machine than on a Linux desktop).

**Notifications** — Windows toasts (WinRT `ToastNotificationManager`),
with R14 Decision 17's urgency mapping: *normal* for went-live / ended /
first-viewer-latched; *critical* (`ToastScenario` urgent/reminder, so it can
surface over a game) for failed-to-start / broadcast error / ended
unexpectedly. The KDE-portal-inhibits-notifications trap has a direct Windows
analogue: **Focus Assist auto-enables during fullscreen games** and eats
normal toasts — whether urgent-scenario toasts punch through is V-8; the
in-window heartbeat/status is the fallback truth either way. No notification
for individual resume attempts (nagging trains users to ignore the ones that
matter).

**Idle cost**: Slint is retained-mode, so Gio's redraw-loop hazard doesn't
map directly — but the Linux GUI shipped a 20-30 % idle-CPU bug anyway, so
the criterion is kept: an idle window sits at ~0 % CPU with no scheduled
wakeups beyond the stats tick (WB6, manual + profiler).

### D13 — Defaults point at production; "blank means the default", resolved at use

Shipped defaults (constants in the engine crate, mirroring
`gawk-broadcast/internal/config`):

| Setting | Default |
|---|---|
| Relay URL | `https://api.gawk.ioio.fi:4433` |
| App URL | `https://gawk.ioio.fi` — **unlike Linux, this ships as a real default** (Linux leaves `-app-url` empty); join links must work out of the box (G8) |
| Telemetry URL | `https://gawk.ioio.fi/api/telemetry/v1/ingest` (`off` = send nothing) |
| Origin | `gawk-broadcast://windows` (D19) |
| Rung | 1920×1080 @ 60, 16 Mbps peak, 500 ms GOP |
| Audio | on (whole-system in mode 2, per-app in mode 1) |

Both Linux config rules carry over exactly:

- **Blank means "the default", resolved at use, never at save** — the config
  file never stores a resolved default, so a release that moves the fleet
  moves every user with it.
- **The telemetry pairing rule**: a blank telemetry URL reports to the
  reference collector only when the relay is also the default one (the
  session token is an HMAC minted by the relay you connected to). Comparison
  on parsed, normalized URLs, never raw strings.

### D14 — Config: `%APPDATA%\gawk\broadcast.json`, atomic, corrupt-tolerant; credentials under DPAPI

Same filename and key names as Linux where the fields are shared (`relayUrl`,
`appUrl`, `publishSecret`, `telemetryUrl`, `origin`, `lastBroadcastId`,
`lastResumeToken`, `lastGoodEncoder`, `disableAudio`, `width`, `height`,
`fps`, `bitrateBps`) plus `captureMode` — no collision is possible
cross-OS, and shared vocabulary keeps docs and diagnostics legible. Atomic
write (temp + rename), corrupt file ⇒ warn and keep defaults, exactly like
`config.go`.

Divergence: Linux protects the file with mode 0600; Windows has no equivalent
that means much beyond the default `%APPDATA%` ACLs, so the two credential
fields — `publishSecret` and `lastResumeToken` — are wrapped with **DPAPI**
(`CryptProtectData`, per-user scope), stored as `dpapi:<base64>`. Zero
prompts, idiomatic, and a copied config file leaks nothing on another machine.
Plaintext values found in those fields are accepted and re-encrypted on next
save (hand-editing stays possible).

### D15 — Telemetry day one, mirroring the Linux reporter

OD10. `TelemetryHello` (0x0D) handling identical to Linux: `Enabled == false`
⇒ collect nothing, indistinguishable from a pre-R28 relay. The reporter
batches the same stats rollup with the **browser's lowerCamelCase field
names** (the Linux engine's hard-won lesson: untagged names broke every
downstream reader), `kind: "gawk-broadcast-windows"`, resumes/resuming
counters included, posting to the resolved ingest URL under the pairing rule.

### D16 — Terms: TC5 posture

The window shows the terms link (`<app URL>/#/terms`); running the app is the
acknowledgment; no gate. Identical to the Linux broadcaster.

### D17 — Packaging: one portable EXE, unsigned, distributed as a CI artifact

`cargo build --release` with `+crt-static` (static MSVC CRT), Slint's
compiled renderer, vendored static libopus ⇒ **one .exe with no dependencies
beyond inbox Windows DLLs**. Acceptance: a fresh Windows 10 2004 VM runs it
with nothing installed (WB8, manual).

No installer, no signing (OD6): the SmartScreen "unknown publisher" speed
bump is documented in the INSTALL doc rather than paid away — this is a
known-operator distribution (friends of the operator), same trust model as
the Linux `gh run download` story, and distribution *is* that story: a
per-commit CI artifact `gawk-broadcast-windows-x86_64-<sha>` containing the
exe + `BUILD-INFO.txt` + a copy of the INSTALL doc, 30-day retention,
`if-no-files-found: error` — R14's artifact-as-verification pattern: CI
cannot run this code meaningfully, so a build a human can run on real
hardware **before merge** is how it gets tested at all.

### D18 — CI: self-hosted Linux, cross-compiled to msvc, integration-tested against the real relay binary

**Revised 2026-07-31.** This decision originally read "`windows-latest`,
strictly path-filtered", because Windows minutes are billed at 2x on a
private repo and the job ran ~34 minutes. It was replaced after the cheaper
option was actually measured rather than assumed. The original reasoning and
its path-filter consequence are kept below the revision, because the *shape*
of the job — lint, test, integrate against the real relay, upload an artifact
— did not change; only where it runs did.

What the measurement found:

- **Linux loses no test signal.** `cargo test --workspace` is 139 passed / 0
  failed / 4 ignored on Linux, identical to a Windows run. The only
  Windows-exclusive tests are the three WARP conversion tests, and F-6 had
  already established those self-skip on the hosted runner — so the paid
  runner was contributing nothing unique. They stay on the §10 register,
  where they already were in practice.
- **Linux loses no lint signal.** `cargo xwin clippy --all-targets --target
  x86_64-pc-windows-msvc` type-checks every `cfg(windows)` block. The job now
  runs a *second*, host clippy pass as well, which lints the `cfg(unix)` code
  no Windows runner ever saw.
- **The artifact survives.** cargo-xwin links a real msvc-ABI PE32+ binary, so
  R14's artifact-as-verification pattern is intact. Two honest deltas, both
  recorded in `BUILD-INFO.txt`: the code generator is clang, not MSVC, and the
  bundled libopus is compiled with an SSE4.1 floor (see below).
- **Coverage went up, not down.** The path filter now includes
  `gawk-server/wire/**`, closing the blind spot the original decision
  accepted; and the relay integration suite gains the `cfg(unix)`
  drain-restart test that self-skipped on Windows.

Three toolchain gates had to be cleared, each found by failure and each fixed
in the workflow (the comments there carry the detail): `llvm-lib` lives in the
`llvm` package and `ring` dies without it; Debian ships no `clang-cl` binary;
and **libopus does not compile under clang-cl** — its CMake adds
`-mssse3/-msse4.1` only for GNU-like compilers because MSVC needs no such
flag, so `silk/x86/*.c` fails on `_mm_shuffle_epi8` and friends. The flags
cannot be injected via `CFLAGS_<target>` (cargo-xwin overwrites it) so the
workflow shadows `/usr/bin/clang-cl` with a wrapper. The accepted cost: the
whole libopus C build assumes SSE4.1, so opus's runtime SIMD dispatch becomes
a floor rather than a choice. Penryn (2008) clears it.

The caching question was answered by measurement too, and mostly in the
negative: restore on these runners runs at ~6 MB/s and `_work` is RAM-backed,
so a 5.5 GB target dir (~15 min, would not fit), a 727 MB registry (~2 min)
and the 1.1 GB xwin SDK (~3 min, against 1m09s to fetch it from Microsoft)
all lose. Only the 11 MB `cargo-xwin` binary is cached — seconds restored
against ~63 s of compiling it.

That 5.5 GB is a constraint on the **build**, not just on caching — a
distinction the first CI run on real hardware made expensive to miss. `_work`
is a 4 Gi RAM-backed emptyDir, and this job wants three trees in it: an msvc
debug tree for the cross clippy, a host debug tree for the host clippy and
the tests, and an msvc release tree for the artifact. Left alone they
accumulate and the job dies with `No space left on device` partway through
the second. The fix is to stop accumulating: `CARGO_PROFILE_DEV_DEBUG=0`
(debug info dominates a debug tree and nothing in CI consumes it), plus a
reclaim step after each debug tree has served its purpose, since the pod is
fresh every run and there is no cache to spoil. Peak becomes one tree instead
of three. Raising the tmpfs was rejected as the first move: it is RAM against
an 11 Gi pod limit, and the job does not actually need the space
simultaneously.

The still-Windows-only residue, unchanged: everything on the §10 on-hardware
register, plus anything about msvc-vs-clang codegen of the shipped EXE.

---

The original reasoning, retained because the job's shape still follows it:

OD11's cost constraint is a design input, not a nicety:

- The workflow (own file, e.g. `.github/workflows/broadcast-windows.yml`)
  triggers **only** on `paths: [gawk-broadcast-windows/**,
  .github/workflows/broadcast-windows.yml]` for both push and PR. G10 makes
  "a docs-only PR does not start the job" an explicit acceptance criterion.
  Consequence accepted and documented: a `gawk-server/wire` change does *not*
  run the Windows mirror's tests until a `gawk-broadcast-windows/` commit
  follows; the golden-vector discipline (vectors restated in every mirror)
  is what makes that safe, and the wire-change checklist in CLAUDE.md gains
  the fourth mirror so the follow-up commit is never forgotten.
- Job steps: rustup (pinned via `rust-toolchain.toml`) → `cargo fmt --check`
  → `clippy -D warnings` → `cargo test` (wire vectors, chunking, resume state
  machine, SPS parser, config, engine seams — all headless) → `setup-go` +
  `go build ./cmd/gawk-server` from the repo checkout → **integration tests
  that dial the real relay binary on loopback** — R14's discipline, more
  load-bearing here than ever: *"a hand-written fake would only assert the
  engine against our belief about the relay, which is the belief most worth
  doubting in a second implementation of a protocol"* — and this is the
  third. (quic-go loopback on a Windows runner is a WB0 spike item, V-9.)
  → release build → artifact upload.
- release-please: component only (changelog + tag + Cargo.toml bump), **no
  publish job** — same as the Linux broadcaster, same reason.

### D19 — Origin: `gawk-broadcast://windows`

A native client must send an Origin or an allowlisting relay rejects it
(empty matches nothing — the Linux field bug). A distinct value rather than
reusing `gawk-broadcast://native` costs one relay values line and buys
client-class visibility in relay logs and telemetry forever. Overridable via
settings/env like Linux. **Deployment note**: the production relay's
`-allowed-origins` must gain this entry before first use — the one
production-side change in R34 (G4 carves it out explicitly).

## 5. Architecture

```
gawk-broadcast-windows/
  Cargo.toml            # workspace; release-please bumps the workspace version
  rust-toolchain.toml
  crates/
    wire/               # the fourth mirror: framing, budgets, close codes,
                        #   golden vectors, GF(256) parity math
    engine/             # session lifecycle, send policy, resume supervisor,
                        #   timesync/clockmapping, stats, telemetry batching.
                        #   No GUI, no COM/WinRT — talks to media through traits,
                        #   to the network through a RelaySession trait seam
    capture/            # WGC frame source, window/monitor enumeration + filters,
                        #   D3D11 device + VideoProcessor (convert/scale/thumbnail)
    encode/             # MFT enumeration, trial probes, cascade, encoder session,
                        #   SPS parse → avc1.PPCCLL, Annex-B utilities
    audio/              # WASAPI process/system loopback, Opus encode, probe
    app/                # the Slint GUI shell — the only binary
```

- `engine` mirrors the Go engine's seam philosophy: a narrow `RelaySession`
  trait (send datagram / open uni / accept uni / close / closed-status),
  because **the send policy is defined by what happens when sends fail**, and
  only a seam lets tests script failures. Media enters through
  `VideoSource`/`AudioSource` traits shaped like the Go `MediaSource`/
  `AudioSource` pair (optional audio keeps video-only a first-class shape,
  R25 Decision 8).
- Dataflow (one process, three pumps, no pipes):

```
WGC frame pool ──BGRA texture──▶ VideoProcessor ──NV12──▶ MF encoder MFT
     (QPC ts)                     (scale/convert)            (Annex-B AU)
                                       │ 1 Hz                     │
                                       ▼                          ▼
                                 GUI thumbnail          engine: classify IDR,
                                                        chunk / StreamFrame,
WASAPI loopback ──f32 48k──▶ libopus ──packets──▶ audio lane (seq, 1 Hz config)
     (QPC ts)                                                     │
                                                                  ▼
                                                     wtransport session ─▶ relay
```

## 6. UX flows (the product surface, end to end)

**First run (G8)**: launch exe → SmartScreen "run anyway" (documented) →
window opens on the picker, defaults already pointing at production → click a
window or monitor → `Start broadcast` → encode line + code + join link appear
→ `Copy link` → paste to friends. Zero fields touched.

**Refusal (G3)**: no hardware encoder ⇒ error card with D9's message; the
browser-broadcaster link uses the resolved app URL. Nothing else is
clickable-broken: the picker stays usable so the user can retry after a
driver install.

**Resume**: relay pod restarts or drains (4002) ⇒ heartbeat amber,
"Reconnecting to the relay… (attempt N)", auto-resume per D5, forced IDR on
success, heartbeat green. Superseded (4004) ⇒ broadcast ends with "Another
broadcaster took over this code" — terminal, by relay invariant. App restart
⇒ `Resume <CODE>` button (persisted ID + token), one click, same code, viewers
untouched.

**Mode-1 audio doubt**: level meter flat for ~10 s ⇒ the D8 hint with the
one-click whole-system switch.

**Stopping**: `Stop broadcast`, or close the window (with the are-you-live
confirmation). Either way the session closes cleanly and the relay's grace
period (not the app) decides how long viewers wait before 4000.

## 7. What deliberately does not exist (inherited prohibitions)

Restated because every one of these was already litigated:

- No software encode rung (R14 Decision 4; D9 message adapted).
- No preview player (R14 Decision 16) — the 1 Hz thumbnail (D12.5) is a
  bounded exception with its own rationale, not a re-opening.
- No viewer→server keyframe back-channel (docs/15 Decision 6) — resume IDR is
  producer-initiated, not viewer-requested.
- No mint fallback outside connect-phase failures (R1).
- No auto-resume through 4000/4004 (relay invariant: newest publisher wins
  only converges if the deposed client stays down).
- No second clock / second anchor (R25 Decision 5, now structural per D7).
- No standalone DecoderConfig datagrams; config rides every keyframe.
- No DTX, no FEC, no Opus bitrate knob (R25 Decision 3).

## 8. Chunks and acceptance criteria

Prefix **WB** (Windows Broadcaster — first unclaimed two-letter prefix that
reads right; `MF` is taken by R22 and would collide with "Media Foundation"
in prose anyway).

Ordering note: WB1+WB2 (wire + transport/engine) are deliberately first after
scaffolding — they are CI-verifiable against the real relay with no Windows
media APIs involved, so the riskiest *protocol* work gets green gates before
any on-hardware work starts. WB3–WB5 are hardware-facing and land behind
trials. WB6 assembles the product.

### WB0 — Component scaffold, CI, release wiring

| Acceptance criterion | Verified by |
|---|---|
| Workspace with the six crates builds green (`fmt`, `clippy -D warnings` for both the msvc target and the host, `cargo test`) on the self-hosted Linux runners (D18, revised) | CI |
| The workflow triggers only on `gawk-broadcast-windows/**`, `gawk-server/wire/**` + its own file; a docs-only PR does not start it (G10) | CI, observed |
| release-please: `gawk-broadcast-windows` component (rust type) added to the manifest config; combined-release-PR behavior intact; no publish job | review |
| Artifact `gawk-broadcast-windows-x86_64-<sha>` uploads with `BUILD-INFO.txt`, `if-no-files-found: error` | CI |
| Slint royalty-free desktop license confirmed compatible with the proprietary repo (attribution requirement handled); fallback decision recorded if not | review |
| Spike: quic-go `gawk-server` builds and serves WebTransport on Windows-runner loopback (V-9) | CI spike |

### WB1 — The wire mirror

| Acceptance criterion | Verified by |
|---|---|
| Every golden vector from `wire_test.go`, `parity_test.go`, `stripe_test.go` restated as literal hex and byte-identical (G5) | unit test |
| Strict parsing: exact fixed lengths, reserved bits rejected, alphabet-validated announce, bounded token/config lengths | unit test |
| Chunker: starts at 1180, zero-length frame ⇒ one chunk, shrink-once semantics, refuses non-shrinking change | unit test |
| GF(256) parity: P/Q symbols byte-identical to the Go vectors; ≤ 255 data chunks enforced | unit test |
| CLAUDE.md + `wire.go` doc comments updated to name the fourth mirror in the wire-change checklist | review |

### WB2 — Transport + engine core (session, send policy, resume)

| Acceptance criterion | Verified by |
|---|---|
| `wtransport` gate 1: custom Origin header sent and honored by an allowlisting relay; gate 2: rejection HTTP status and session close code visible (or the Go workaround ported) — fallback library decision forced here if either fails | integration vs. real relay + review |
| Mint and reclaim flows: announce + token + capabilities + hello read by type under scripted adverse stream order; announce bounds (258 B / 10 s) | unit + integration |
| Send policy parity table (D5) holds row by row: keyframe ≤1 in flight with supersede code 1, mid-frame drop + counter, embedded config, frameID continuity, GOP-drop/keyframe-flush queue, parity gating on 0x0F | unit vs. scripted `RelaySession` seam |
| TimeSync @ 2 s / ClockMapping @ 5 s / AudioConfig @ 1 Hz cadences; TimeSync reset on resume | unit, scripted clock |
| Resume supervisor: backoff 250 ms→5 s, 5 min window; terminal on 4000/4004 and HTTP 401/403/404/409; retries status 0; reclaim never mints | unit state-machine tests |
| Kill and restart the real relay mid-publish: engine resumes on the same code with continuous frameIDs; datagrams flow again (G9) | integration |

### WB3 — Capture

| Acceptance criterion | Verified by |
|---|---|
| Window enumeration lists exactly the alt-tab set (cloaked/tool windows filtered), with title + icon; monitors with name + resolution | unit (filter logic) + manual |
| WGC frames arrive with QPC timestamps; drop-only fps gating (no CFR synthesis); cursor embedded | unit (gating logic) + manual |
| VideoProcessor BGRA→NV12 (+scale) correct on a synthetic texture via WARP | unit (WARP device) |
| Occluded-window capture keeps delivering; minimized behavior measured and the `IsIconic` hint shown (V-2) | manual |
| HDR source through a BGRA8 pool assessed; tone-map rung built only if needed (V-5) | manual, on the gaming PC |
| Border-removal applied where the API exists; clean degrade on build 19041 | manual on both builds |

### WB4 — Encode

| Acceptance criterion | Verified by |
|---|---|
| Hardware-only MFT enumeration; refusal path with D9's message when the (faked) list is empty (G3) | unit |
| Trial-encode gate enforces every row of the D9 invariants table on at least one real vendor MFT | manual, recorded bitstream analysis |
| SPS → `avc1.PPCCLL` parser matches the Go `sps.go` vectors | unit (ported vectors) |
| SPS/PPS-before-every-IDR guaranteed: prepend path exercised against a fixture missing in-band headers | unit |
| Last-good encoder cached, re-verified first; live failure advances the cascade | unit (scripted trials) + manual |
| Forced IDR on demand works (resume re-prime) | manual + integration once WB2+WB4 meet |

### WB5 — Audio

| Acceptance criterion | Verified by |
|---|---|
| Process loopback: target app audible, concurrent other app inaudible (G1); the launcher/anti-cheat tree question answered for ≥2 real games (V-3a) | manual, on the gaming PC |
| System loopback survives a default-device switch or fails audio-only (G2) | manual |
| 48 kHz/stereo delivered on both paths (direct-format request, or AUTOCONVERT/fallback recorded) | manual + unit for chosen fallback |
| Opus packets: TOC says stereo + 20 ms; config-vs-TOC verification errors loudly on mismatch; one packet per datagram, oversize dropped + counted | unit (decode produced packets) |
| Audio subordination: probe-fail and live-fail both leave video running, wire byte-identical to video-only | unit (fake source) + manual |
| Single-clock invariant: scripted A/V pair preserves source spacing (D7); QPC-position availability on process loopback answered (V-4) | unit + manual |

### WB6 — GUI

| Acceptance criterion | Verified by |
|---|---|
| All D12 cards present; states Idle/Starting/Live + amber resuming; picker → start → code → copy-link flow (G8) | manual |
| Curated error sentences pre-empt generic rendering (sentinels first); mint offer only on connect-phase failures | unit (message mapping) + manual |
| Settings persist/read the D14 file; blank-means-default captions live-computed; panel disabled while live | unit (config) + manual |
| Close-while-live confirmation; window close ends the broadcast and the capture session (no zombie encode — the `finish()` incident class) | manual |
| Toasts fire with the D12 urgency mapping; Focus Assist behavior measured (V-8) | manual |
| 1 Hz thumbnail in mode 1; idle window ~0 % CPU | manual + profiler |
| Diagnostics JSON: `kind`, nullable-pointer semantics, browser field names | unit |

### WB7 — Telemetry

| Acceptance criterion | Verified by |
|---|---|
| TelemetryHello parsed (golden vector); disabled flag ⇒ nothing collected | unit |
| Batch field names byte-equal to the browser's lowerCamelCase set | unit vs. recorded browser batch |
| Pairing rule + `off` semantics match the Go `config` tests (ported) | unit |
| A real session's batches land in the production telemetry service and render in the dashboard | manual |

### WB8 — Packaging, docs, and the on-hardware acceptance pass

| Acceptance criterion | Verified by |
|---|---|
| Single unsigned exe runs on a fresh Windows 10 2004 VM with nothing installed | manual |
| INSTALL doc (SmartScreen note, Windows-floor table, "when it doesn't work" — modeled on the Linux INSTALL.md's tester-with-no-context framing) ships inside the artifact | review |
| README + ROADMAP status + CLAUDE.md repo-layout note updated; new gotchas synced to README's list | review |
| G1, G2, G6 (photographed reference), G7 (viewer avSkewMs), G9 (fleet rollout) all pass on the gaming PC against production | manual, on the gaming PC |
| Relay `-allowed-origins` gains `gawk-broadcast://windows` in the production values | review + manual |

## 9. Risks

- **The wtransport gates (D2).** Highest structural risk; deliberately forced
  in WB2 before anything depends on the library. Both gates have named
  fallbacks (library swap; ported close-code workaround).
- **Process-loopback tree coverage (V-3a).** If big games' audio routinely
  escapes the process tree, mode 1's audio story degrades to "sometimes" —
  the D8 hint + one-click system-audio switch is the designed landing, and
  the finding would be recorded in the doc, not papered over.
- **MF knob sufficiency.** If some vendor's MFT can't hit the latency/GOP
  invariants, that vendor's users fall to the next MFT or to refusal — and
  the vendor-SDK escape hatch (D9) gets its evidence. The trial gate makes
  this a measured outcome, not a field surprise.
- **A third protocol implementation drifting.** Mitigated the only way that
  has worked so far: restated golden vectors, integration against the real
  relay binary, and the CLAUDE.md wire-change checklist naming this mirror.
- ~~**Path-filtered CI blind spot**~~ — **closed 2026-07-31** by D18's
  revision: free runners let `gawk-server/wire/**` become a trigger, so a wire
  change now exercises this mirror in the same PR. The restated golden vectors
  remain the belt to that braces.

## 10. On-hardware verification register

The questions only a real machine can answer, numbered so chunks and findings
can cite them: **V-1** wtransport Origin + status/close-code visibility (WB2);
**V-2** minimized-window frame delivery vs. the ROADMAP's optimistic claim
(WB3); **V-3a** process-loopback vs. launcher/anti-cheat process trees, ≥2
real games (WB5); **V-3b** process-loopback across a default-device switch
(WB5); **V-4** QPC positions on process-loopback capture packets (WB5);
**V-5** HDR washout through a BGRA8 pool (WB3); **V-6** per-vendor SPS/PPS
repetition on IDRs (WB4); **V-7** per-vendor invariant-table conformance
(WB4); **V-8** urgent toasts vs. Focus Assist during a fullscreen game (WB6);
**V-9** quic-go relay loopback on a GitHub Windows runner (WB0).

Findings land in this doc's deviations section as they arrive, R14-style:
design revised in place with the field evidence named.

## 11. Deviations and field findings

Recorded as they land, R14-style. WB2's integration tests against the real
`gawk-server` binary answered V-1 (2026-07-30) and found four interop
defects in stock `wtransport` 0.7.1 — every one invisible to unit tests and
each the exact class of thing D18's "test against the real relay" rule
exists for. The library is now **vendored with five small patches**
(`gawk-broadcast-windows/vendor/*/GAWK-PATCH.md`, every site marked
`GAWK PATCH`), all upstreaming candidates:

- **F-1 (gate 2a, pre-registered)**: the HTTP status of a rejected CONNECT
  is parsed and discarded (`SessionRejected` carries no data, upstream
  master included). Patched to carry it; the integration test pins
  401/403/404 arriving from the relay's real handlers. The pre-registered
  fallback library (`web-transport-quinn`) does not expose it either, so
  the owner chose vendor+patch (OD13, decided 2026-07-30).
- **F-2**: `Headers` is a HashMap, so QPACK-encoding emitted fields in
  arbitrary order — but RFC 9114 §4.3.1 requires pseudo-headers first, and
  quic-go enforces it with an H3_MESSAGE_ERROR stream reset. Consequence:
  stock wtransport could not send ANY additional request header (the D19
  Origin header included) to a quic-go server without a coin-flip protocol
  violation. Patched: pseudo-headers encode first.
- **F-3**: unknown H3 frame types violation-closed the connection; quic-go
  sends GOAWAY on the control stream during shutdown. Patched to ignore
  unknown frames per RFC 9114 §9.
- **F-4 (gate 2b)**: session close codes were lost twice over. (a)
  Capsules are a byte stream over the CONNECT stream's DATA frames and
  webtransport-go splits one capsule across two frames; stock wtransport
  parsed each DATA frame as exactly one whole capsule and silently skipped
  the rest — so 4000/4002/4004 never surfaced. Patched: the reader
  reassembles across frames. (b) Even a parsed capsule was then shadowed:
  the driver locally closes the QUIC connection after recording it, and
  `Connection::closed()` only consulted quinn (`LocallyClosed`). Patched:
  `closed()` prefers the driver's session-level close. The supersede
  integration test (4004 terminal, no resume-back) pins both.
- **F-5 (harness)**: the drain-restart test is `cfg(unix)` — SIGTERM has
  no analogue a console Go process handles on Windows, so the
  drain-while-Ready path joins the on-hardware register for the Windows
  side (§10); the Windows CI job still runs publish/reject/supersede.
- **F-6 (2026-07-31, WB3)**: the hosted `windows-latest` runner's WARP
  cannot create a D3D11 device with `D3D11_CREATE_DEVICE_VIDEO_SUPPORT`
  (`DXGI_ERROR_UNSUPPORTED` at creation). The WARP conversion tests
  self-skip loudly in that case — F-5's posture — and execute on any real
  Windows machine; the BGRA→NV12/scale/thumbnail pins therefore join the
  first on-hardware run rather than CI.
- **F-7 (2026-07-31, WB5)**: the vendored libopus (`opus` crate →
  `audiopus_sys`) ships a CMakeLists older than CMake 4's compatibility
  floor; every build needs `CMAKE_POLICY_VERSION_MINIMUM=3.5` in the
  environment (set in the CI workflow, documented in the component README).
- **F-8 (2026-07-31, field)**: the first external tester (desktop RTX 2070,
  browser hardware-encode working) got the D9 refusal — and the app could
  not say why: the cascade's per-candidate rejection trail went to
  `eprintln!`, which `windows_subsystem = "windows"` discards, so
  "MFTEnumEx enumerated nothing" and "NVENC was found but failed the trial
  gate" were indistinguishable in the field. Fixed by making the runtime
  record a file: every launch writes `%APPDATA%\gawk\debug.log` (rotated to
  `.old`, lifecycle-only volume) via the `log` facade — adapter identity
  (`D3D11CreateDevice(None, …)` takes the *default* adapter; on hybrid-GPU
  machines the encoder vendor's MFT may not match it, so the log names the
  adapter), MFT enumeration count + friendly names, per-candidate trial verdicts with
  the failing MF step named (`step()` wraps each session-setup call), and
  session lifecycle. Error cards append "Details were written to …". The
  2070 root cause itself is **open pending that machine's debug.log** —
  tracked in `BUGS.md`.
- **F-9 (2026-07-31, field, via F-8's log)**: the NVIDIA H.264 Encoder MFT
  (RTX 2070) rejects a hand-built NV12 input type with
  `MF_E_INVALIDMEDIATYPE` (0xC00D36B4) — major type + subtype + frame size
  + frame rate is not enough; the MFT compares against its own enumerated
  types, which carry attributes we cannot guess. AMD/Intel MFTs accepted
  the same hand-built type on the WB4 bring-up machine, so this is
  per-vendor V-7 territory, exactly what "enumeration is not acceptance"
  predicts. Fix: after `SetOutputType`, take the MFT's OWN
  `GetInputAvailableType` NV12 entry and complete it with our geometry;
  the hand-built fallback (for MFTs that don't enumerate) now carries
  `MF_MT_INTERLACE_MODE` and a square `MF_MT_PIXEL_ASPECT_RATIO` too. Same
  log also showed the NVIDIA MFT refusing
  `AVEncMPVDefaultBPictureCount = 0` with `E_INVALIDARG` *before* type
  negotiation — the best-effort knobs (B-count, low latency) are now
  re-applied after types are set; the trial gate still verifies the actual
  bitstream either way.
- **F-10 (2026-07-31, field, via F-8's log going silent)**: with F-9 fixed
  the same machine died with *no* log line after "screen capture started"
  territory — the process-loopback activation in `gawk-audio/wasapi.rs`
  built a `VT_BLOB` `PROPVARIANT` whose `pBlobData` points at a stack
  local, and the `windows` crate gives `PROPVARIANT` a `Drop` running
  `PropVariantClear`, which `CoTaskMemFree`s the blob pointer → heap
  corruption → process abort, no unwind, nothing logged. It never fired
  before because F-9 blocked every start earlier in the sequence; the WB5
  bring-up validated audio in system-loopback… also through this code
  path — the crash window is the *drop*, timing-sensitive. Fixes: the
  `PROPVARIANT` is now `ManuallyDrop` (the blob is borrowed, nothing to
  free); a global panic hook logs every panic with thread + location; the
  shell's phase-2 build runs under `catch_unwind` so a media-pipeline
  panic becomes a visible start failure instead of a UI stuck in
  "Starting…"; and the pipeline logs each bring-up seam (capture started,
  audio state, pipeline ready) so the next silent death is bracketed.
- **F-11 (2026-07-31, field)**: `SetIsBorderRequired(false)` alone does
  not remove the yellow capture border — the
  `GraphicsCaptureAccess::RequestAccessAsync(Borderless)` grant must come
  first (auto-granted without a prompt for unpackaged desktop apps), and
  the old `let _ =` swallowed the refusal. Both are attempted now, with
  outcomes logged; on Windows builds without `IsBorderRequired` (< 20348)
  the border stays and the log says so. The startup log also records the
  `UniversalApiContract` level as the OS fingerprint.

## 12. References

- `docs/19-linux-native-broadcaster.md` — R14: decisions 1–21, deviations,
  the incident write-up, the tray/hotkey deferral research.
- `docs/28-native-broadcaster-audio.md` — R25: Opus contract, subordination,
  the single-anchor invariant and its mutation test.
- `docs/34-live-edge-forward-parity.md` / `docs/35-connection-interleaving.md`
  — the parity and striping producer obligations (0x0E/0x0F/0x10).
- `gawk-server/wire/wire.go` — the wire source of truth; golden vectors in
  `wire_test.go`, `parity_test.go`, `stripe_test.go`.
- `gawk-broadcast/internal/engine/{send,resume,sps,relay}.go` — the semantics
  this engine ports.
- `ROADMAP.md` §R34 — the motivation and language analysis this doc resolves.
