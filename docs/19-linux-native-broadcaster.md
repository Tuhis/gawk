# R14 — Native Linux broadcaster (`gawk-broadcast`)

**Status**: design only, not started (chunks V1–V7, plus **V8 direct Vulkan
Video encode**, gated on V2). Doc-only pickup — each chunk lands later as its
own task.

## Goal

A standalone native broadcaster for Linux with **hardware encode**, bypassing
the browser entirely: a Gio **GUI app** for normal use, plus a CLI over the
same engine for headless/debug use.

```
gawk-broadcast-gui                     # the app you actually use
gawk-broadcast -url … -resolution 1080p -fps 30   # same engine, headless
→ Broadcast code: K7M2QP   join: https://gawk.example/#/view/K7M2QP
```

It speaks the existing publisher protocol byte-for-byte. Zero server changes,
zero wire changes, zero viewer changes. The browser broadcaster stays exactly
as it is and remains the path for Windows/macOS and for anyone who doesn't
want to install anything.

## Why: the browser path cannot do hardware encode on Linux

This is not a tuning problem, it is a platform gap, and it is worth recording
precisely so nobody re-litigates it with a flag hunt:

- **WebCodecs `VideoEncoder` hardware encode ships on Windows, macOS and
  Android — not Linux.** Linux gets hardware *decode* only. Chromium's own
  VA-API doc is explicit about the whole surface: *"VA-API on Linux is not
  supported, but it can be enabled using the flags below, and it might work on
  certain configurations -- there's no guarantees."* The relevant flag is now
  `--enable-features=AcceleratedVideoEncoder` (the older `VaapiVideoEncoder`
  naming is gone).
- **On NVIDIA it is structurally impossible, not merely unsupported.**
  Chromium's Linux encode path is VA-API only (no NVENC encoder exists for
  Linux; the other backends are V4L2, Media Foundation, VideoToolbox), and
  `nvidia-vaapi-driver` is NVDEC-backed and **decode-only** — its author's
  position is that VA-API and NVENC are too different to bridge. So NVIDIA +
  Linux + any browser = software x264/OpenH264, permanently, at whatever cost
  in game fps that implies.
- **On Intel/AMD it is a "maybe"** worth exactly one five-minute test (the
  flag above, then check `chrome://gpu` → Video Acceleration Information) —
  but WebCodecs encode specifically is still documented as unsupported on
  Linux, so the expected outcome is that it doesn't help *us* even when
  VA-API encode lights up for WebRTC.
- **Firefox is worse, not better.** Its `VideoEncoder` support is narrower,
  and `docs/11` already catalogues its AVCC extradata and path-MTU bugs. That
  path means owning those bugs forever.

Going native also sheds three problems we already know about, for free: the
process-level backgrounding caveat recorded in R11, the `getDisplayMedia`
picker, and R4's field finding that observed low fps was 4K-capture-source
limited.

## Why this is much cheaper than "write a second broadcaster" sounds

Most of it already exists, in Go:

- **`gawk-server/internal/wire` is the canonical frozen implementation with
  golden vectors.** The engine *reuses* it rather than becoming a third
  parallel implementation (after Go + `wire.ts`). Protocol drift risk is zero
  by construction — this is the single biggest reason to write this in Go
  rather than Rust or C.
- **`webtransport-go` is already a dependency** and its `Dialer`/`Session`
  expose precisely the publisher's needs: `SendDatagram` (deltas, TimeSync),
  `OpenUniStream` (keyframes), `AcceptUniStream` (the announce), and
  `ReceiveDatagram` (TimeSync pongs).
- **The viewer already auto-detects Annex-B vs AVCC** (`viewer.ts:352-361`:
  Annex-B start code sniffed on the frame data itself, `description` only
  attached when the payload isn't Annex-B *and* extradata starts with `0x01`).
  So the engine emits raw Annex-B with **empty extradata** and never
  constructs an `AVCDecoderConfigurationRecord`. That removes the nastiest
  interop risk in the whole idea before it exists — and note the
  `normalizeAvccExtradata` scrubber in `viewer.ts` becomes dead weight on this
  path, not a dependency.
- **A native client trusts the homelab CA directly**, so the
  `serverCertificateHashes` 14-day certificate dance doesn't apply.

The publisher protocol surface is five message types.

## What this does and does not buy (be honest)

- **Does**: hardware encode on Linux at all (the browser cannot); removes
  capture→encode from the renderer process; lets the desktop's own portal
  picker replace `getDisplayMedia`; a broadcaster that survives having no
  browser open.
- **Does not**: help Windows/macOS broadcasters (already fine), improve the
  viewer, or change end-to-end latency by itself — the win is *available
  encode capacity and game fps on the broadcasting machine*, not a shorter
  glass-to-glass path. The g2g budget is unchanged.
- **Costs**: a second broadcaster implementation to keep protocol-current.
  Decision 1 bounds that cost; Decision 11 makes it a maintenance rule rather
  than a hope.

## Shape: one engine, two shells

The GUI requirement is not a layer on top of the CLI — it determines the
engine's shape. A `main` with flags and package globals would have to be
dismantled to grow a window, so the engine is a **package** from V1 and the
CLI is its first shell, not its home:

```
internal/broadcast          engine: Session{Start,Stop,SetLadder} + Callbacks
  ├─ cmd/gawk-broadcast     CLI shell    — headless, harness, debug
  └─ cmd/gawk-broadcast-gui GUI shell    — Gio window + notifications
```

`Session` deliberately mirrors the TypeScript `BroadcastSessionLike`
(`start`/`stop`/`setLadder`) and `BroadcastCallbacks`
(`onBroadcastId`/`onStats`/`onError`/`onEnded`) from `transport/broadcaster.ts`.
Same idea, same names, other language: the two broadcasters stay legible to
each other, which is what makes a second implementation survivable.

The CLI is **not** scaffolding to be discarded. It proves the engine
end-to-end before any window exists (V4 gates the whole protocol and the
latency bias), and it stays as the headless path — the direct analogue of the
frozen `#/debug/broadcast` page next to the production UI.

## Locked decisions

1. **Lives at `gawk-server/`, same Go module.** Go's `internal/` rule is
   directory-based: a separate top-level module (`gawk-broadcaster/`) at
   `github.com/Tuhis/gawk/gawk-broadcaster` could **not** import
   `github.com/Tuhis/gawk/gawk-server/internal/wire` — only the tree rooted at
   `gawk-server/` may. Reusing `wire` unchanged is the whole point
   (Decision 11), so the engine (`internal/broadcast`) and both shells
   (`cmd/gawk-broadcast`, `cmd/gawk-broadcast-gui`) live in that tree,
   alongside the existing `cmd/gawk-devcert` and `cmd/gawk-echo` binaries. If
   it ever needs to ship independently, the escape hatch is to promote `wire`
   to a public `gawk-server/wire` package (or a shared `gawk-wire` module) —
   deliberately deferred, since it churns the frozen package's import path
   everywhere for no present benefit.
2. **No container image, no Helm chart, no CI deploy.** These are binaries you
   run on your own gaming PC — `go build ./cmd/...`. Explicitly outside the
   "chart version == appVersion == image tag" model, which exists for
   cluster-side components. Attaching prebuilt binaries to the
   `gawk-server-vX.Y.Z` release is a possible later convenience, not part of
   R14.
3. **The media stack is delegated to an ffmpeg subprocess for V1–V7; direct
   encode is V8 (Decision 18).** The honest reasons are **crash isolation** —
   we are heading into flaky NVIDIA-on-Wayland driver territory, and when
   ffmpeg segfaults there a dying child process is a notification, whereas an
   in-process libav segfault takes the GUI down with it — and **version
   tolerance** (any ffmpeg carrying the right features, versus `go-astiav`'s
   hard pin to n8.0, which in practice means vendoring ffmpeg).
   **Correction, recorded rather than quietly edited**: this decision
   originally rested on "needs no cgo toolchain", which is **void** —
   Decision 12 (Gio) already requires gcc, pkg-config and the
   Wayland/xkb/EGL/GLES/Vulkan headers. The two decisions were made
   independently and the conflict went unnoticed for a day; the cgo tax is
   paid regardless, so it can no longer buy anything here.
   **Also note**: ffmpeg's `pipewiregrab` does its own portal handshake and
   offers no way to pass one in, so the subprocess forecloses the
   "own-the-portal" option in Decision 19. **Rejected**: `go-astiav` (the pin,
   plus it forfeits crash isolation — reconsider only if Decision 6's
   timestamp bias fails its gate); GStreamer + `go-gst` (heavier, and reports
   exist of `pipewiresrc` capture stopping seconds after an app goes
   fullscreen — our exact use case, so it would need disproving first).
4. **Vendor-agnostic encode via a cascade probed per user, at every startup.**
   Ordered preference **`h264_vulkan`** → `h264_nvenc` → `h264_vaapi` →
   `libx264`, each **verified by a real short trial encode**, not by
   `ffmpeg -encoders` (which happily lists encoders that fail at runtime on
   the actual device). `h264_vulkan` leads because Vulkan Video is the target
   API (Decision 17) and this is how it gets proven on real hardware for the
   price of one line. This is the native-side cousin of **R13's probe matrix**
   (`media/probe.ts`), one layer down — and it inherits R13's caveat, which is
   the reason it trusts a trial encode over `ffmpeg -encoders`: R13 found
   `isConfigSupported`'s hardware answer is *advisory only*, and runtime
   `configure()` wins. That is the project's standing `getSettings()` rule in
   another costume: *trust the thing actually in hand, not the metadata that
   describes it*. The chosen encoder is reported at startup, in stats, and in
   the GUI.
   **This is a per-user runtime decision, not a design-time one.** R1 made
   gawk multi-broadcaster: several friends broadcast from their own machines,
   on GPUs we do not know and cannot survey. So there is no such thing as
   "the" encoder for this project — the cascade must resolve independently on
   each broadcaster's hardware, every run, and **the fallbacks are permanent
   infrastructure, not scaffolding on the way to Vulkan** (Decision 17). A
   broadcaster whose GPU has no Vulkan Video encode must still get hardware
   encode via NVENC or VAAPI, and one with neither must still broadcast on
   `libx264`. Carrying every backend *is* what makes this design
   vendor-neutral today — not one universal API, but the whole set, for free.
5. **Wayland via `pipewiregrab`; the desktop portal is the picker.**
   `pipewiregrab` is an ffmpeg ≥7.1 libavfilter source filter that does the
   XDG ScreenCast portal handshake itself and supports dmabuf sharing —
   `-f lavfi -i pipewiregrab`. The user gets their own desktop's share picker,
   which is a strictly nicer story than `getDisplayMedia`, and it means
   **the GUI needs no source picker of its own**. **X11 is not a target**
   (kmsgrab and NvFBC are noted in V7 as escape hatches; `x11grab` does a CPU
   readback per frame and is a non-starter at 4K).
6. **Frame timestamps are stamped on arrival from the pipe, with a measured
   bias gate.** Raw Annex-B over a pipe carries no PTS. Stamping when an
   access unit is fully read biases R5's absolute capture→render latency
   *low* by (capture + encode) latency — a roughly constant few ms against a
   ~200-400 ms budget, but a silent under-report is exactly the kind of lie
   the stats overlay must not tell. So: **V4 measures the bias, and if it
   exceeds 15 ms or proves non-constant, the fix is to mux `-f mpegts` and
   parse PES PTS** (which also yields AU framing free via
   `payload_unit_start_indicator`, retiring Decision 7's AUD trick). The gate
   is an acceptance criterion, not a footnote.
7. **AU framing via inserted access-unit delimiters.** A pipe destroys
   libavcodec's packet boundaries, so the engine must re-find them:
   `-bsf:v h264_metadata=aud=insert`, then split at each AUD (NAL type 9).
   Keyframe classification is NAL type 5 (IDR) present in the AU → reliable
   uni stream; otherwise → datagrams. SPS/PPS ride in-band before each IDR
   (the raw `h264` muxer sets no global header), which is exactly what the R8
   embedded-config design wants: self-sufficient keyframes.
8. **The codec string is parsed from the SPS, never assumed.** `avc1.` +
   hex(profile_idc, constraint_flags, level_idc) read out of the first SPS
   NAL, because the encoder may pick a different level than requested. ~20
   lines. On the Annex-B path the viewer uses this string as-is (its
   extradata-derived correction only runs for AVCC), so it is the *only*
   thing telling the decoder what it's getting — worth getting right.
9. **No ladder, no auto-fallback in v1.** Resolution/fps/bitrate are fixed for
   a session; scaling and fps gating happen in the ffmpeg filter graph (in
   hardware), not in a port of `preprocess.ts`. R4's `FallbackController` is
   deliberately **not** ported: its trigger is `encodeQueueSize` growth, and
   R4's own hardware finding (2026-07-13) was that HW encoders drain frames
   without that signal ever firing — porting it would carry the complexity and
   reproduce the under-fire. Defaults: **1080p60**, **500 ms GOP** (item 11's
   `keyframeIntervalMs`). Note the fps default tracks **R13**, not item 11:
   R13 consciously revised the fixed-30 fan-out cap to an 'auto' framerate
   resolving framerate-first (60 where a rung probes hardware, else 30). This
   engine is hardware-encode by construction — that is its entire reason to
   exist — so it lands on the 60 side of R13's rule, and 30 stays one flag
   away. `SetLadder` exists on the engine surface for symmetry but restarts
   ffmpeg; the GUI disables it while live.
10. **No auto-reconnect; reclaim is one click.** This matches the browser
    broadcaster exactly — `BroadcasterScreen` doesn't auto-reconnect either
    (only the viewer does, via `ViewerSession`). The GUI remembers the last
    code and offers **Resume broadcast**, which dials `/publish/{id}`; on
    failure it applies the existing rule — **fall back to minting only when
    `phase == 'connect'`** — so the Go engine's start error carries the same
    `phase` distinction (`connect` vs `capture`) for the same reason: a
    capture-phase failure had a live publisher session and must not silently
    move to a different broadcast ID.
11. **The wire package is reused, never mirrored, and the engine is bound to
    the protocol by the same golden vectors.** No hand-copied constants, no
    re-derived header layouts. Any wire change breaks the engine's compilation
    or its tests, which is the point: this is the mechanism that keeps a second
    broadcaster from silently rotting.

### GUI decisions

12. **Gio (`gioui.org`), locked.** Native Wayland (no XWayland, no webview),
    pure-Go application code, immediate mode. Build needs the usual Linux
    dev headers (`gcc pkg-config libwayland-dev libx11-dev libx11-xcb-dev
    libxkbcommon-x11-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev
    libxcursor-dev libvulkan-dev`) — document them in the README, they are the
    honest price. **Rejected**: *Fyne* (runs through XWayland by default;
    `-tags wayland` is mutually exclusive with X11 — harmless here since no
    media touches the toolkit, but Gio is the more interesting and more
    native choice for a project whose stated motive is learning); *Wails v3*
    (wants WebKitGTK 6.0, `-tags gtk3` for older distros, and WebKitGTK's
    DMA-BUF renderer has documented crashes on **Wayland + NVIDIA** — our
    exact target combo, worked around by disabling DMA-BUF; also puts a
    browser engine back inside the binary we are building *because* the
    browser can't do the job, and the R6 design-system reuse it promises is
    thinner than it looks: `BroadcasterScreen` is built around
    `BroadcastPipeline` + Zustand, so only tokens and a few primitives would
    carry over); *GTK4/gotk4* (large cgo API surface for a window with six
    controls).
13. **The window is the app; closing it ends the broadcast.** No tray, no
    background presence, no hidden state — if the window is gone, nothing is
    publishing. This is the simple, predictable behavior, and it is the one
    decision a tray would later overturn (see Deferred below), so it is worth
    naming rather than leaving implied.
14. **No preview.** You are looking at your own screen, and the portal picker
    already showed you what you're sharing. The browser broadcaster has a
    preview only because a tab isn't your screen. What people actually need is
    *"am I live and are frames moving"*, which a sent-fps readout and a
    heartbeat indicator answer at ~zero cost. Revisit only if V7's
    verification shows people genuinely can't tell.
15. **Notifications via `gioui.org/x/notify`** (D-Bus; X11/Wayland
    indifferent) on: went live (with the code), first viewer joined, error,
    ended. With no tray (Decision 13), these are the *only* thing that reaches
    you once you're fullscreen in a game — an ffmpeg death you don't learn
    about for an hour is the failure mode they exist to prevent. gio-x is
    explicitly experimental with an unstable API, so wrap it behind a
    one-method interface: a swap to raw `godbus` must be a contained change,
    not a refactor.
16. **Settings persist to `~/.config/gawk/broadcast.json`** (mode 0600): relay
    URL, publish secret, rung, encoder override, last broadcast code. The GUI
    writes it; CLI flags override it. A pre-shared secret in a local file is
    consistent with how it already travels (a query param, per R2 — the
    WebTransport JS API can't set headers).
17. **Vulkan Video (`VK_KHR_video_encode_h264`) is the target encode API,
    reached in two stages.** It is the only encode API that is genuinely
    vendor-neutral: RADV (AMD), ANV (Intel) and NVIDIA's proprietary driver
    all implement it — NVIDIA was among the first — with no
    `nvidia-vaapi-driver`-shaped asterisk. Choosing it is the same instinct
    that picked WebTransport + WebCodecs over a mature WebRTC path:
    standards-track, newer, lower-level, more interesting.
    **Direct VAAPI is rejected outright, and the reason matters**: Chromium's
    Linux encode backend is VAAPI-only, and that is *exactly why* it cannot
    hardware-encode on NVIDIA. Building gawk on direct VAAPI would spend weeks
    reproducing the precise limitation R14 exists to escape. VAAPI is
    vendor-neutral for Intel and AMD and structurally excludes the third
    vendor — it is *less* portable than the delegated cascade we already have,
    not more.
    - **Stage 1 (V2)**: `h264_vulkan` goes first in the delegated cascade.
      This proves Vulkan Video encode on real hardware in minutes rather than
      weeks, for the price of one line, and yields a **known-good reference
      bitstream**.
    - **Stage 2 (V8)**: direct Vulkan Video encode in Go, **gated on Stage 1
      succeeding on the development machine**, with ffmpeg's output as the
      differential oracle.
    The staging is not timidity, it is what makes the work tractable: without
    a reference bitstream, "my encoder emits garbage" is near-undebuggable,
    and with one, ffmpeg's `vulkan_encode_h264.c` becomes an executable spec
    to port against. If Stage 1 fails, Stage 2 was never possible either —
    and that is a ten-minute finding instead of a week-long one.
    **Vulkan Video being the target API does not make it the only path.**
    Per Decision 4 this is a multi-broadcaster project on unsurveyed
    hardware: a friend's GPU may have no Vulkan Video encoder, an older
    driver, or a broken one. Direct Vulkan is therefore an **additional
    top-of-cascade candidate for machines that support it**, never a
    replacement for the delegated backends. V8 shipping does not retire V2.
18. **Stats parity where it's honest, `n/a` where it isn't.** The engine
    reports what it can see: encoder fps, sent fps, datagrams, keyframe
    streams sent/failed, bytes, TimeSync RTT, chosen encoder. It **cannot**
    see pre-gate capture fps — ffmpeg owns that stage — so R9's funnel is
    reported with that stage marked unavailable rather than fabricated from
    the requested rate. Copy diagnostics JSON, matching R9's
    remote-troubleshooting story.
19. **Owning the portal handshake is a named future option, not v1.** Doing
    the D-Bus `org.freedesktop.portal.ScreenCast` dance ourselves
    (`CreateSession` → `SelectSources` → `Start`) is ~200–300 lines of pure Go
    over `godbus`, and it buys `restore_token` + `persist_mode`: **no
    re-picking your screen every session**, plus cursor-mode and monitor
    selection. It is the one genuinely high-value part of the "do it all
    ourselves" fantasy (the rest — SPA POD negotiation, dmabuf import — is a
    slog). It is deferred because it **does not compose with ffmpeg**:
    `pipewiregrab` owns its handshake and takes no session in. GStreamer's
    `pipewiresrc` accepts `fd=` and `path=<node-id>`, so taking this option
    means moving the capture half to go-gst. Revisit if re-picking the screen
    every session becomes the thing that annoys you most.

## Architecture

```
gaming PC (Linux/Wayland)                                    relay
─────────────────────────                                    ─────
  cmd/gawk-broadcast-gui (Gio)      cmd/gawk-broadcast (CLI)
    window · notify                   flags · stderr stats
            └──────────┬───────────────────────┘
                       ▼
              internal/broadcast — Session{Start,Stop,SetLadder} + Callbacks
                       │
    ffmpeg (subprocess)│
      pipewiregrab ──▶ portal picker (desktop's own; no picker in our GUI)
         │ dmabuf
      scale/fps + h264_nvenc | h264_vaapi | libx264   ← probed by trial encode
         │ Annex-B + AUDs, in-band SPS/PPS
         ▼ stdout pipe
      AU splitter ──▶ IDR? ──yes──▶ wire.AppendStreamFrameHeader
         │              │            + embedded DecoderConfig + payload
         │              │            └─▶ Session.OpenUniStream() ─────────▶ CONNECT
         │              └──no───▶ chunk ≤1180B                              /publish
         │                        wire.AppendVideoChunk                       [?secret=]
         │                        └─▶ Session.SendDatagram() ─────────────▶
      SPS parser ──▶ "avc1.42E02A" ──▶ wire.AppendDecoderConfig
      TimeSync ping/pong (2 s) ──▶ offset ──▶ wire.AppendClockMapping (5 s)
      AcceptUniStream ──▶ wire.ParseBroadcastAnnounce ──▶ code ──▶ shells
```

Reused verbatim from `internal/wire`: `AppendVideoChunk`,
`AppendDecoderConfig`, `AppendStreamFrameHeader`, `AppendTimeSync`,
`ParseTimeSync`, `AppendClockMapping`, `ParseBroadcastAnnounce`, and the
constants (`MaxDatagramSize` 1200, `MaxChunkPayload` 1180, `MaxChunkCount`
3000, `StreamFrameHeaderSize` 24, `TimeSyncSize` 18, `ClockMappingSize` 10).

## The capture path, end to end

The shape to hold in your head: **the picture stays on the GPU the whole way,
and our Go code never touches a pixel.** By the time anything reaches our
process it is already compressed H.264 — the app is a byte pump, and that is
why it can be simple.

1. **You click Start.** The app dials `/publish` *first*, before any capture.
   Same rule as the browser (Decision 10 / connect-before-picker): if the
   relay refuses, no share dialog should ever have appeared.
2. **The app spawns ffmpeg** as a child process with a pipe on its stdout.
3. **ffmpeg asks your desktop for a screen.** `pipewiregrab` talks to the XDG
   ScreenCast portal over D-Bus, and **your** desktop — KDE, GNOME — shows
   **its own** share dialog. We don't draw it, we don't theme it, we never see
   it. That's why the GUI has no source picker (Decision 5).
4. **You pick a screen; the portal returns a PipeWire stream.** Frames arrive
   as **dmabufs** — handles to GPU memory, not pixels. Nothing has been copied
   anywhere yet.
5. **ffmpeg scales and rate-limits on the GPU** (4K→1080p, 60→30 fps).
6. **ffmpeg encodes on the GPU's dedicated encode block** (NVENC / VAAPI) —
   not the CPU, and not the shader cores either, so it isn't taking from the
   game's frame budget. This is the step the browser cannot do on Linux, and
   the entire reason R14 exists.
7. **Compressed bytes come down the pipe.** *Now* the data is in CPU memory —
   but it's ~1% of the size, because it's already H.264.
8. **The app re-finds frame boundaries.** A pipe is a featureless byte stream:
   ffmpeg knew where each frame ended, and the pipe threw that away. So we ask
   it to insert AUD markers and we split on them (Decision 7). One blob out =
   one frame.
9. **The app stamps arrival time** from the same monotonic clock TimeSync uses
   (Decision 6 — and the reason that clock must be shared is that
   `ClockMapping` otherwise relates two unrelated timelines).
10. **The app reads the NAL types** in the blob. Contains an IDR (type 5)?
    Keyframe. Otherwise, delta. On the first one it also parses the SPS into
    `avc1.42E02A` (Decision 8) — the only thing telling the viewer's decoder
    what it's about to get.
11. **The road forks — this is R8's design, unchanged**: a **keyframe** goes
    out whole on one **reliable uni stream** (too big and too important to
    lose to one dropped packet), while a **delta** is sliced into ≤1180-byte
    **datagrams** (fast and lossy — lose one and you lose a frame, not a GOP).
12. **The relay fans out** to viewers exactly as it does for the browser
    broadcaster, because the bytes are identical.

Alongside all this, two small side channels: a TimeSync ping every 2 s (which
yields the relay-clock offset) and a ClockMapping publish every 5 s (which
turns that offset into the viewer's absolute capture→render latency).

**The one place the picture might touch the CPU** is step 6 on NVIDIA:
`pipewiregrab` hands out VAAPI/dmabuf frames and NVENC wants CUDA frames. If
that import doesn't work on the proprietary driver, ffmpeg has to round-trip
`hwdownload` → `hwupload_cuda` through system memory. Survivable at 1080p30,
but it's the risk V2 exists to settle.

## Chunks

### V1 — engine: session surface + transport

`internal/broadcast/`: the `Session` type (`Start`/`Stop`/`SetLadder`) +
`Callbacks` struct + `StartError{Phase}` (Decision 10), all injectable-clock
for tests. Dial `/publish` or `/publish/{id}` via `webtransport.Dialer`
(`TLSClientConfig` with system roots; `-insecure` for the dev cert); read the
first incoming uni stream to EOF → `ParseBroadcastAnnounce` → `OnBroadcastID`.

TimeSync client mirroring `time-sync.ts` exactly: ping every
`TIME_SYNC_INTERVAL_MS` (2 s — the relay rate-caps at 5/s per session, a
constant, so this must not become a knob), keep the **lowest-RTT sample of
8**, `offsetUs = serverTimeUs - (t0Us + rttUs/2)`. Frame timestamps and the
TimeSync `t0` must read the **same monotonic clock** — `time.Since(start)` in
µs, the direct analogue of the relay's own `relayNowUs()` and of the
broadcaster's `performance.now()` — or `ClockMapping` publishes a mapping
between two unrelated timelines and the viewer's absolute latency is nonsense.
Publish `AppendClockMapping(offsetUs)` every `CLOCK_MAPPING_INTERVAL_MS` (5 s),
nothing before the first pong.

| Acceptance criterion | Verified by |
|---|---|
| `Session` surface mirrors `BroadcastSessionLike`/`BroadcastCallbacks`; no package globals, no `os.Exit`, no direct stdio — a shell could be anything | review + the fact that V4 and V5 both consume it unmodified |
| Dials `/publish`, reports the announced code; `-id` dials `/publish/{id}` and reclaims | integration test against an in-process `transport.Server` |
| `StartError.Phase` distinguishes `connect` from `capture`; a capture-phase failure tears the session down (no zombie publisher) | integration test |
| Secret sent as query param; wrong/missing secret fails with a clear message | integration test |
| TimeSync lowest-RTT-of-8 selection matches `time-sync.ts` on identical sample sequences | unit test with a fake clock + shared fixture |
| ClockMapping sent only after the first pong, then every 5 s | unit test with a fake clock |
| Frame-timestamp clock and TimeSync `t0` clock are the same source | unit test asserting a single injected `now()` feeds both |

### V2 — engine: capture + encode

`internal/broadcast/ffmpeg.go`: arg vectors per encoder; startup cascade
(`h264_nvenc` → `h264_vaapi` → `libx264`) where each candidate is accepted
only after a real trial encode produces output; supervise the child (stderr
relayed at debug level, non-zero exit → clean shutdown carrying the last
stderr lines, never a silent hang).

Reference shape (VA-API; exact args are V2's job):

```
ffmpeg -f lavfi -i pipewiregrab \
  -vf 'hwmap=derive_device=vaapi,scale_vaapi=w=1920:h=1080:format=nv12,fps=30' \
  -c:v h264_vaapi -rc_mode CBR -b:v 8M -g 15 -bf 0 \
  -bsf:v h264_metadata=aud=insert \
  -f h264 -flush_packets 1 pipe:1
```

**Known risk, to be settled here**: NVIDIA + Wayland is the awkward corner —
`pipewiregrab` yields dmabuf/VAAPI frames while NVENC wants CUDA frames, and
cross-device import is unreliable on the proprietary driver. Expected
resolutions in order: dmabuf→CUDA import if it works; else `hwdownload` +
`hwupload_cuda` (a CPU roundtrip — acceptable at 1080p30, measure it); else
`h264_vaapi` if the machine turns out to be AMD/Intel after all. If all three
disappoint, V7's escape hatch applies.

| Acceptance criterion | Verified by |
|---|---|
| Cascade picks the first candidate that *actually encodes*; a listed-but-broken encoder is rejected and the next tried | unit test over an injected trial-encode runner |
| Chosen encoder + rung logged once and surfaced on `Callbacks` | unit test + manual |
| ffmpeg death is detected and surfaces as `OnError` with its last stderr, never hangs | unit test with a fake failing child |
| Encoder override forces one candidate and skips the cascade | unit test |
| At the chosen rung, ffmpeg reports no dropped frames and the encoder is confirmed hardware (`nvidia-smi` / `intel_gpu_top` / `radeontop` shows encode-engine activity) | manual, on the gaming PC |
| **The local ffmpeg actually has `pipewiregrab`** — it needs `--enable-libpipewire` at *build* time and distro builds vary, so "ffmpeg is already installed" guarantees nothing; a missing filter must produce a sentence a friend can act on, not a stack trace | manual: `ffmpeg -h filter=pipewiregrab`; a clear diagnostic if absent |
| **Stage-1 Vulkan gate (Decision 17)**: `h264_vulkan` is probed first and the result recorded — does this GPU encode via Vulkan Video at all? | manual: `vulkaninfo \| grep -i "video_encode"` + the trial encode |
| A `h264_vulkan` clip is captured and archived as the **reference bitstream** for V8's differential oracle | manual: file committed to a fixtures location or noted in the V8 task |
| **The cascade degrades cleanly on a machine with no Vulkan Video and no NVENC/VAAPI** — a broadcaster on unknown hardware still ends up on `libx264` rather than a failure (Decision 4: the fallbacks are permanent, and other people's GPUs are not surveyable) | unit test over the injected trial-encode runner, forcing each candidate to fail in turn |

### V3 — engine: bitstream

`annexb.go`: streaming AU splitter (split at AUD, NAL type 9; tolerate 3- and
4-byte start codes; bounded buffer rejecting an AU larger than
`wire.MaxKeyframeBytes` rather than growing without limit). `sps.go`: parse
profile_idc/constraint_flags/level_idc → `avc1.XXXXXX`. `send.go`: IDR → one
`StreamFrame` uni stream with embedded `DecoderConfig`; delta → chunk to
`MaxChunkPayload` and `SendDatagram`, handling `*quic.DatagramTooLargeError`
by lowering the chunk size to its `MaxDatagramPayloadSize` and re-chunking
(the Go-side analogue of `docs/11`'s Firefox path-MTU fix — do not assume 1200
is reachable). Frame IDs from 0, wrapping at uint32 like `nextFrameId` — the
relay's R9 ingress-loss window depends on it.

| Acceptance criterion | Verified by |
|---|---|
| Splitter emits exactly one AU per encoded frame across a recorded fixture, including start codes split across reads | unit test over a captured Annex-B fixture, byte-by-byte feed at adversarial read sizes |
| An AU exceeding `MaxKeyframeBytes` errors, never allocates unboundedly | unit test |
| IDR detection matches the fixture's known keyframe positions | unit test |
| SPS parse yields the codec string ffmpeg reports for the same fixture | unit test |
| Delta chunking round-trips against the existing reassembly logic | golden vectors from `wire_test.go` + a chunking unit test |
| `DatagramTooLargeError` lowers chunk size and re-chunks rather than dropping the frame | unit test with a fake session |
| Frame IDs start at 0 and wrap at uint32 | unit test |

### V4 — CLI shell + end-to-end + the timestamp-bias gate (Decision 6)

`cmd/gawk-broadcast/`: flags (`-url`, `-id`, `-secret`, `-insecure`,
`-resolution`, `-fps`, `-bitrate`, `-encoder`) + `GAWK_*` envs over the V1–V3
engine; periodic stderr stats line. Then **measure the bias**: compare the
viewer overlay's `Latency (capture→render)` against a physical reference (the
`docs/13` bottleneck-playbook method — a clock on the captured screen,
photographed alongside the viewer) at 1080p30.

| Acceptance criterion | Verified by |
|---|---|
| A Chrome viewer joins by code and renders; overlay reads `Renderer: WebGL`, `Transport: Worker` | manual |
| Overlay shows live `Live-edge drift`, a plausible `Latency (capture→render)`, populated `RTT (time-sync)` | manual |
| **Measured latency under-reports the physical reference by < 15 ms, and the gap is stable across a 60 s window** | manual, photographed reference — **if this fails, Decision 6's `-f mpegts` PTS path is required before R14 is done** |
| Keyframes arrive as streams, deltas as datagrams; `/statusz` + `/metrics` show the broadcast with ingress loss ≈ 0 on LAN | manual |
| Relay restart → reclaim by `-id` works; viewer recovers without a terminal 4000 | manual |
| Game fps at 1080p30 broadcast is materially better than the software-encode browser baseline on the same machine | manual — the whole point of R14 |

### V5 — GUI shell: window

`cmd/gawk-broadcast-gui/`: Gio window over the same engine. Settings (relay
URL, secret, resolution, fps, encoder override) loaded from and saved to
`~/.config/gawk/broadcast.json` (Decision 16); big Start/Stop; the broadcast
code with **Copy link**; **Resume broadcast** using the remembered code
(Decision 10's reclaim→mint rule); viewer count; live state and errors in
plain language. No source picker (Decision 5), no preview (Decision 14), no
tray (Decision 13 — closing the window stops the broadcast).

| Acceptance criterion | Verified by |
|---|---|
| Start → portal picker appears → code shown → viewer can join | manual |
| Engine consumed unmodified from V1 (no engine changes needed to add a GUI — the Decision-that-shaped-V1 paying off) | review of the diff: `internal/broadcast` untouched |
| Settings round-trip the config file; file is 0600; flags still override in the CLI | unit test + manual |
| Resume dials `/publish/{id}`; a `connect`-phase failure offers minting, a `capture`-phase failure does not | unit test over a fake engine |
| Errors surface as sentences, not Go error strings (an ffmpeg/portal failure must say what to do) | manual |
| Ladder controls disabled while live (Decision 9) | manual |
| Closing the window stops the broadcast and leaves no publisher on the relay | manual + `/statusz` |

### V6 — GUI: stats panel, copy diagnostics, notifications

Stats panel over the engine's `OnStats` (encoder, fps, bitrate, datagrams,
keyframe streams sent/failed, TimeSync RTT, viewer count), with capture-fps
marked unavailable rather than fabricated (Decision 17), plus **Copy
diagnostics** JSON matching R9's shape closely enough to be pasted next to a
viewer's. Notifications via `gioui.org/x/notify` behind a one-method interface
(Decision 15) — with no tray these are the only signal that reaches you
mid-game.

| Acceptance criterion | Verified by |
|---|---|
| Panel reflects a live session; numbers agree with `/statusz` for the same broadcast | manual |
| Pre-gate capture fps reads `n/a`, never a fabricated number | review + manual |
| Copy diagnostics produces JSON pasteable beside a viewer's Copy diagnostics | manual |
| Notifications fire on live / first viewer / error / ended, and are swappable off gio-x without touching call sites | manual + review |
| An ffmpeg death while fullscreen in a game reaches the user as a notification | manual — the failure mode Decision 15 exists for |

### V7 — docs + escape hatches

README (quickstart, the Gio build dependencies from Decision 12, the Linux
broadcaster section, and the **Annex-B/AVCC gotcha** — a native publisher
exercises the viewer's Annex-B branch, which the browser never does),
CLAUDE.md build-order entry + docs list, ROADMAP R14 status. Record the escape
hatches evaluated but not built, so a future reader doesn't rediscover them:
**kmsgrab** (X11 + Wayland, zero-copy dmabuf → VAAPI, needs `CAP_SYS_ADMIN`);
**NvFBC** (NVIDIA/X11, fastest, needs the well-known driver patch on GeForce);
**gpu-screen-recorder** as a capture+encode front end — it solves exactly this
problem (NvFBC on X11, KMS elsewhere, NVENC/VAAPI/AMF) better than ffmpeg
does, but piping its output to stdout is *not* documented (it targets files and
RTMP), so adopting it means first establishing whether a pipe is possible at
all.

| Acceptance criterion | Verified by |
|---|---|
| README gotcha list names the Annex-B publisher path | review |
| Gio build dependencies documented (they are the honest price of Decision 12) | review |
| CLAUDE.md + ROADMAP reflect R14 status | review |
| Escape hatches recorded with their trade-offs | review |
| All gates green (`go test ./...`, `go vet`, lint; frontend untouched) | CI |

### V8 — direct Vulkan Video encode (Stage 2, gated on V2)

**Entry gate**: V2 established that `h264_vulkan` encodes on the development
machine and archived a reference bitstream. If it didn't, this chunk does not
start — direct Vulkan would fail for the same reason, and the delegated
cascade's `h264_nvenc`/`h264_vaapi` fallback is the answer instead.

**This chunk adds a candidate; it does not replace the stack.** Other
broadcasters run GPUs we can't survey (Decision 4), so direct Vulkan sits at
the top of the same runtime cascade and every delegated backend stays behind
it, permanently. V8 shipping does not retire V2.

Replace the encode half only: `pipewiregrab` capture stays with ffmpeg (or
moves per Decision 19), and `send.go` + the session are untouched — the V1–V3
seam is what makes this a contained swap rather than a rewrite. Work:
generate the `VK_KHR_video_encode_queue` / `VK_KHR_video_encode_h264` bindings
from `vk.xml`; create the video session + session parameters (SPS/PPS built by
us); dmabuf → `VkImage` import via `VK_EXT_external_memory_dma_buf` +
`VK_EXT_image_drm_format_modifier`; DPB and reference-list management; rate
control; drain coded buffers to Annex-B.

**Known risks, stated plainly because they set the schedule, not the typing**:

- **Go's Vulkan bindings are unmaintained and predate Vulkan Video** — the
  encode extensions are simply not in them, so binding generation from the
  registry is step zero, not a given.
- **No Go Vulkan Video encoder is known to exist.** Zero prior art: the
  references are all C (ffmpeg's `vulkan_encode_h264.c`, GStreamer's, NVIDIA's
  samples). This is the condition under which AI assistance is *least*
  effective, which is exactly why the reference bitstream from V2 is the
  entry gate — with an oracle this is a port, without one it is a search.
- **The driver floor is recent and moving**: Mesa 26.0 only just landed a
  large RADV video-encode correctness batch (DPB sizes, alignments, reference
  management). Expect to be testing against a target that is still settling,
  and expect failures to surface as `VK_ERROR_DEVICE_LOST` or silent garbage
  rather than helpful errors.
- **Capture is unaffected** — this chunk buys the encode API, nothing else.

| Acceptance criterion | Verified by |
|---|---|
| Direct-Vulkan output decodes to the **same frames** as V2's archived `h264_vulkan` reference for identical input | differential test against the reference bitstream (the oracle that makes this tractable) |
| Chosen encoder reports `vulkan-direct` in stats; the delegated cascade remains available as fallback at runtime | manual + unit test |
| A Chrome viewer renders the direct-Vulkan stream with no corruption over a 10-minute heavy-motion session | manual |
| GPU encode-engine activity confirms hardware use (not a software fallback inside the driver) | manual |
| Game fps is no worse than the V4 delegated baseline at the same rung | manual — if it is, the whole exercise lost its point |

## Verification plan (manual)

On the Linux gaming PC, Wayland session: launch the GUI → Start → desktop
portal picker appears → code shown → join from a Chrome viewer on another
machine. Then, in order: hardware encode confirmed on the GPU's **own** encode
counter (not ffmpeg's claim); game fps against the browser-broadcaster
baseline at the same rung; the V4 latency-bias gate; keyframe streams vs delta
datagrams in `/statusz`; Resume across a relay restart; closing the window
mid-broadcast leaves no publisher behind; and a heavy-motion scene for
corruption/freeze behavior (the 500 ms GOP and the viewer's freeze-on-gap
policy should behave exactly as with the browser broadcaster — if they don't,
the AU splitter is the first suspect). Repeat the portal + notification path
on both KDE and GNOME.

## Deferred (wanted, not now)

Cut from scope 2026-07-15 to keep the first version small. Both are additive —
neither changes the engine, and the research is recorded so it doesn't need
redoing:

- **Tray icon.** Would overturn Decision 13 (window-closed currently means
  not-publishing) and give a background presence for the three hours you're in
  a game. `fyne.io/systray` is the pick when it comes back: D-Bus
  StatusNotifierItem, and the Fyne fork exists precisely to drop the GTK
  dependency, so it doesn't drag Fyne in. Known integration risk: Gio's
  `app.Main()` must own the main goroutine and blocks, and `systray.Run` also
  blocks — on Linux the SNI backend is pure D-Bus, so a goroutine should
  serve, but that needs proving.
- **Global start/stop hotkey.** Best-effort at best: the XDG GlobalShortcuts
  portal is implemented on KDE and Hyprland, **still blocked on GNOME**, and
  **absent on wlroots** compositors. So it could never have been load-bearing,
  which is a large part of why it's easy to drop.

## Rejected

- **Browser flags on Linux** — the evidence above; a five-minute test on
  Intel/AMD is fine, but it is not a path.
- **CLI only** — a broadcaster you drive by remembering flag names is not one
  you use before a gaming session. The CLI stays as the engine's harness and
  headless path, not as the product.
- **Fyne / Wails v3 / GTK4** — Decision 12.
- **Direct VAAPI** — Decision 17. Worth restating because it is the seductive
  wrong answer: VAAPI *looks* like the vendor-neutral Linux standard, and it
  is the exact API whose NVIDIA gap put us here. Chromium is VAAPI-only on
  Linux; that is why Chromium cannot encode on NVIDIA. Vulkan Video is the
  API that actually spans all three vendors.
- **Rust (`wtransport`) or C** — would make a third wire implementation and
  forfeit Decision 11's compile-time coupling. The learning goal is served
  better by the QUIC/Vulkan/Gio work than by a protocol re-port.
- **A local sidecar that hands encoded chunks to the browser page** (native
  capture+encode → localhost WebSocket → page does WebTransport) — all the
  native complexity, plus a browser, plus a hop, to keep a UI we don't need on
  this path.
- **WebRTC/MoQ** — unchanged from CLAUDE.md.
