# R35 — Single-app sharing (window + app audio) in the native Linux broadcaster

**Status**: designed 2026-08-01 (owner decisions AD1–AD4 taken the same day,
listed in §2). **Implemented 2026-08-01** — chunks AS1–AS6 landed; AS7 (the
on-hardware register, §6) is the outstanding half and is what decides the
milestone. §8 records what the build learned that the design could not have
known, including two places where reality amended a decision.

`gawk-broadcast` gains a **share one application** mode: the app's window as
video plus **only** that app's audio — a second app playing sound on the same
PC is inaudible to viewers. This is R34's mode 1 (docs/38 G1), on Linux.
Whole-desktop + system audio, today's behavior, remains the other mode and is
byte-identical to what ships now.

This document leans on four earlier ones and does not restate them:

- `docs/19` — the R14 design this feature lives inside: the portal handshake,
  the gst-launch child, the encoder cascade, the subprocess philosophy.
- `docs/28` — the R25 audio design. The Opus contract, audio subordination
  (Decision 6) and the one-child/one-muxer/one-PTS-timeline shape (Decision 4)
  are inherited verbatim and nothing here renegotiates them.
- `docs/38` — the Windows sibling. D8 (per-app audio UX, the silence escape
  hatch) and D11's geometry amendment (bounding box, never a stretch target)
  are ported where Linux can honor them; §4 D1 below records where it can't.
- `ROADMAP.md` §R35 — the feasibility findings (2026-08-01) this doc builds
  on: what the portal exposes, what it deliberately hides, and the prior art.

## 1. Why (recap) and what "done" means

The capture-fidelity driver is R34's, and it bites harder here: on Linux the
native app is the *only* hardware-encode path (R14), so its capture story
should not trail the Windows sibling's. Today a broadcaster who picks a
window in the portal dialog still ships their whole desktop's audio — every
notification ping and voice call goes out to viewers.

Two structural facts, established in the R35 feasibility pass, shape
everything below:

1. **Window video already flows.** `internal/portal` has requested
   `types = MONITOR | WINDOW` since R14; the desktop picker already offers
   windows and picking one broadcasts it. This milestone is not "add window
   capture" — it is *noticing* the choice (`source_type`), *sizing* it
   honestly (fit, never stretch), and *pairing* it with the right audio.
2. **The portal never says whose window it is.** Stream properties are
   `id`/`position`/`size`/`source_type`/`mapping_id` — no app identity, no
   PID, by privacy design. Windows' one-picker flow ("window ⇒ its process
   tree's audio") is structurally impossible on Linux. The design embraces a
   two-step flow instead of imitating the one-step badly.

### Milestone acceptance criteria

Pre-registered per the repo convention. "Manual" means on real hardware (the
gaming PC and at least one KDE + one GNOME session); CI cannot see a GPU, a
portal, or an interactive desktop — but unlike R14, most of the *audio* plane
is CI-verifiable, because a full PipeWire daemon runs headless on a runner
(AG4/AG5 below exploit that).

| # | Goal | Verified by |
|---|---|---|
| AG1 | Sharing one window streams that window's video and **only** the chosen app's audio: a second app playing sound on the same PC is inaudible to viewers | manual, on the gaming PC |
| AG2 | The whole-desktop path is unchanged: picking a monitor produces the same pipeline, wire bytes, and behavior as today (window-mode code adds zero diff to it) | unit (pipeline args byte-compare) + review |
| AG3 | A non-16:9 window is fitted inside the configured resolution, floored to even — never stretched; the GUI shows the fitted dimensions | unit (fit math) + manual |
| AG4 | App audio survives stream churn: the captured app closing and reopening its audio streams (in-game menu, engine restart, a second stream) re-links within one registry round-trip, with the gst pipeline untouched | integration vs. a real headless PipeWire daemon (synthetic emitters) |
| AG5 | Crash-safe cleanup: SIGKILL of the app, the helper, or the gst child leaves the sound server pristine — no orphan sink, no orphan links, app audio never re-routed (`pw-dump` asserted clean) | integration, kill -9 matrix |
| AG6 | Audio stays subordinate (R25 Decision 6): helper death, sink failure, or a vanished target degrades to the system-audio offer or audio-off with a notification — video never stops | unit + integration |
| AG7 | Zero wire/relay/viewer changes: a stock viewer plays an app-mode broadcast with no diffs outside `gawk-broadcast/` (plus docs) | review + integration vs. the real `gawk-server` |
| AG8 | A/V sync criteria hold across the virtual-sink hop: viewer-reported median `\|avSkewMs\| ≤ 60 ms`, p95 `≤ 120 ms` over 60 s (R25's numbers, unchanged) | manual, viewer diagnostics |
| AG9 | The portal picker still appears on every start (Decision 5 untouched); the audio step always asks, preselecting the last choice only when it is present and emitting | unit + manual |
| AG10 | Build/packaging honesty: the helper links only stock-distro libraries, `ldd`-checkable per INSTALL.md's existing recipe, and `BUILD-INFO.txt` records it | CI + review |

## 2. Owner decisions (taken 2026-08-01)

Recorded so the rest of the doc can build on them without hedging:

| # | Decision | Choice |
|---|---|---|
| AD1 | Mode entry | **Inferred from the picker** — no new pre-Start UI. Pick a monitor → today's behavior; pick a window → the whose-audio step appears (with "Whole system" as one of its options). The picker is the mode switch; no mode/picker disagreement state can exist. |
| AD2 | App identity for re-linking | **`application.process.binary`** (OBS's choice). Survives restarts and multi-process games; the stream-presence hint (D6) covers helpers with a different binary name. |
| AD3 | Audio-choice persistence | **Ask every start, preselect last** — the whose-audio step always appears in app mode; the last-used app is preselected when present and emitting. Decision 5's spirit (zero silent reuse) at one-click cost. |
| AD4 | PipeWire control plane | **A small native helper, day one** — a long-lived libpipewire client owning the sink and links, not `pw-cli`/`pw-link` subprocess driving. Robust event ordering from the start; the CLI route is recorded as rejected (§7). |

## 3. Non-goals

- **Any change to the relay, wire format, viewer, or browser broadcaster** —
  zero wire/server diffs (AG7), same stance as R34 G4.
- **A gawk-drawn window picker** — the desktop's portal picker stays the
  picker; Decision 5 (picker every start, no restore tokens) is not reopened.
- **Microphone capture** — OD8's project-wide stance holds.
- **Multiple windows / multiple apps' audio** — `multiple=false` stays; one
  window, one app's audio. The virtual-sink shape would extend to N apps
  later without redesign, but that is not this milestone.
- **A live audio level meter** (docs/38 D8 has one) — deferred, not needed:
  the Linux picker lists apps that are *actually emitting*, so the
  picked-the-wrong-process trap Windows meters for is mostly avoided by
  construction; the stream-presence hint (D6) covers the rest. The helper
  could add peak metering later without a protocol break.
- **Mid-session letterboxing on window aspect change** — the fit is taken at
  start (D2); a mid-session aspect change stretches within the fitted frame,
  recorded as a v1 limitation (the Windows VideoProcessor letterboxes; no
  GStreamer hardware converter rung offers `add-borders`). Games do not
  resize mid-session; desktop apps that do get an honest note, not a fight.

## 4. Decisions

### D1 — Mode is what the picker returned: `source_type` drives everything, and the whose-audio step is the only new UI

The portal's `Start` results carry a per-stream `source_type` (ScreenCast
interface v3+; verified against the spec 2026-08-01) and a `size`.
`ParseStartResults` currently keeps only the node id; it grows to return
`{NodeID, SourceType, Size}` — additive, no new portal calls, no new
permissions, no second dialog.

- `source_type == MONITOR` (or the property is absent — an old portal): the
  entire new machinery is bypassed. AG2 makes this a byte-compare guarantee,
  not an intention.
- `source_type == WINDOW`: the engine asks its shell for an audio target
  before building the pipeline (a new seam, `ChooseAudioTarget` — the GUI
  blocks on the whose-audio card, the CLI answers from `-audio-app`; see D5).

The flow per start: audio trial → portal picker (unchanged, every start) →
*if a window was picked*: whose-audio step → helper builds the sink + links →
gst child starts. The audio trial keeps running **before** the portal
(docs/19 Decision 10's ordering — learn "no audio on this machine" before
picking), and in app mode a second trial runs against the freshly created
sink monitor before going live: a monitor of an idle sink still clocks out
silence samples, and the trial checks samples, not loudness, so it passes on
a quiet sink — what it proves is that the capture path negotiates.

**Why not the Windows explicit-mode UI**: on Windows the app draws its own
picker, so the mode must be chosen before it. Here the desktop draws the
picker and already offers both source kinds in one dialog — a prior mode
switch would create exactly one new state (mode and picked source disagree)
and no new capability. AD1 closes that door.

### D2 — Geometry: the configured resolution is a bounding box; the fit input is the portal's reported size

Ported from docs/38 D11 (as amended 2026-07-31), adapted to this pipeline:

- The encode resolution is the **source aspect fitted inside the configured
  WxH**, one side pinned, the other shrunk, both floored to even. A shared
  pure-Go `fit` function (mirroring `gawk-capture`'s `fit` module semantics,
  including its golden cases) owns the math.
- The fit input is the stream's `size` from the portal `Start` results —
  available *before* the pipeline launches, so `encoderCaps` pins the fitted
  dimensions and no element ever letterboxes at start. When `size` is absent
  (optional in the spec), behavior falls back to today's exact-caps path and
  the GUI says so; per the standing trust-the-frame rule, the negotiated caps
  in the child's stats remain the runtime truth the fitted dims are checked
  against (a mismatch logs, it does not kill).
- **This applies to monitor sources too**: an ultrawide 21:9 desktop into the
  1080p box encodes 1920×822 instead of today's silent vertical stretch. The
  16:9-monitor common case is a no-op, which is what AG2's byte-compare
  demands — the fitted dims equal the configured dims there.
- Mid-session size changes ride the existing renegotiation: downstream caps
  stay fixed, the candidate's converter absorbs the scale. Same-aspect
  resizes are invisible; aspect *changes* stretch (non-goal above). If
  renegotiation instead kills the child — DMA-BUF renegotiation is the
  documented fragile spot (docs/19 gotchas) — the engine's existing live
  failure path restarts the cascade **reusing the portal session in memory**,
  same fitted dims, costing a brief freeze and no new picker. Which branch
  each compositor takes is V-2, not something this doc asserts.

### D3 — App audio: a helper-owned virtual sink, port links as a tee, and a gst pipeline that never learns any of it happened

The mechanism, proven in the field by OBS's `pipewire-audio-capture`
application mode and verified in the feasibility pass:

- The helper (D4) creates a **virtual sink** — media class
  `Audio/Sink/Internal` so pulse-facing app lists never show it — named
  uniquely per session (`gawk-app-capture-<pid>`).
- The sink's **channel layout matches the default sink's**, not stereo, and
  this is load-bearing: an app stream node's output ports already speak the
  layout it negotiated toward the default sink, and raw port-to-port links
  perform no channelmix — a stereo sink under a 5.1 game would silently drop
  the center channel (where the dialogue lives). The R25 stereo downmix
  stays exactly where it already is, in the gst branch's `audioconvert`,
  keeping the viewer's decoder configuration honest (docs/28 Decision 3).
  If the default sink's layout changes mid-session (a device switch), the
  helper recreates the sink and re-links — a dropped-packets gap, which the
  wire model already treats as truth; `audioconvert` absorbs the monitor's
  caps change without a pipeline restart.
- The target app's `Stream/Output/Audio` ports are **linked** into the sink:
  a tee, not a re-route. PipeWire output ports fan out, so the app keeps
  playing to the speakers and the sink receives a copy. Nothing about the
  user's audio routing is ever modified — the worst crash leaves a hidden
  idle sink, never silent speakers (and D4's ownership rule cleans even
  that).
- The gst child captures **one static node forever**: the audio branch's
  source becomes `pipewiresrc target-object=<sink serial>
  stream-properties=props,stream.capture.sink=true ! audio/x-raw` — the
  monitor of our sink, addressed by its `object.serial` (unique for the
  daemon's lifetime, immune to name races). Everything downstream of the
  source — `audioconvert` through `opusenc`, every R25 parameter — is
  byte-identical to today's branch. All dynamism (which app, streams dying
  and reappearing, re-links, sink recreation) lives in PipeWire link
  management, outside GStreamer: **no pipeline restarts, no A/V-sync model
  change**. One child, one muxer, one PTS timeline (docs/28 Decision 4)
  survives untouched, which is most of why this shape wins.
- Latency cost of the hop: one PipeWire quantum (~5–21 ms at defaults) ahead
  of the same capture point the monitor path uses today. AG8 keeps the R25
  sync criteria as the gate; the fixed component is absorbed by the viewer's
  adaptive controller like every other constant in this chain.

Engine plumbing: `MediaConfig` gains an `AudioTarget` (the sink serial, set
only in app mode), a new `AudioCandidate` (`app-sink-monitor`) consumes it,
and `AudioState`/stats/the R28 report grow the mode and the chosen app's
binary so diagnosis doesn't need ssh. The existing `-audio-device` override
keeps precedence semantics: naming a device pins the device candidate and
app mode's audio step is skipped with a visible note (the override is the
bigger hammer, and two audio masters would be worse than either).

### D4 — The control plane is a small native helper, spawned per app-mode broadcast, whose death *is* the cleanup

AD4. A new binary, `cmd/gawk-pw-helper`: a few hundred lines of Go + cgo
against `libpipewire-0.3`, doing exactly four things — watch the registry,
create the sink, maintain links for one target binary, report events. It is
**not** a media path: no samples flow through it; if it dies, audio degrades
(D6) and video never notices.

- **Why native, day one**: link maintenance is an event-ordering problem —
  streams appear, die, and reappear in bursts (game launch is exactly that),
  and correctness means acting on registry events in order with the
  round-trip guarantees the native API gives. Driving `pw-cli`/`pw-link`
  subprocesses means re-polling `pw-dump` snapshots and racing churn windows;
  the owner rejected shipping that as v1 (§7).
- **Why a subprocess and not in-process cgo**: crash isolation is this
  module's settled posture (docs/19 Decision 3 — the reason GStreamer is a
  child). A libpipewire loop wedging or crashing must not take the broadcast
  with it. The engine supervises the helper exactly like the gst child:
  spawn, watch, and on death apply D6.
- **Cleanup by construction**: the sink and every link are proxies bound to
  the helper's core connection — never linger-flagged — so the daemon
  destroys them when the connection closes, *whatever* the reason: clean
  exit, SIGKILL, OOM. AG5's kill matrix asserts this empirically rather than
  trusting the spec reading; if a PipeWire version is found where proxy
  teardown leaks, the helper adds explicit destroy-on-signal, but the design
  requirement is that **no** exit path can leave state behind.
- **Protocol**: newline-delimited JSON. stdin: `{"op":"watch"}`,
  `{"op":"capture","binary":…}`, `{"op":"release"}`. stdout events:
  `apps` (the emitting-app list: binary, `application.name`, stream count —
  the whose-audio card renders this), `sink` (serial — the engine feeds it to
  `BuildPipeline`), `links` (current link count to the target — D6's hint
  signal), `fatal`. stdin EOF means exit: the engine closing the pipe is the
  teardown call, no handshake to get wrong.
- **Build/packaging honesty**: the module is *already* cgo (Gio's window
  backend), so this adds `libpipewire-0.3-dev` to the existing CI header
  list, not a first compiled dependency. The helper links only
  `libpipewire-0.3` — a stock package on every distro INSTALL.md already
  names, and present by definition on any machine the portal works on.
  It ships next to the two existing binaries; `BUILD-INFO.txt` and the
  `ldd` recipe cover it (AG10). The GUI runs without it until the first
  app-mode start; a missing/unrunnable helper is a sentence ("app audio
  needs the gawk-pw-helper binary next to the app") and a system-audio
  fallback, never a crash.

### D5 — The whose-audio step: gawk's own list, honestly labeled, one click in the common case

The portal cannot say whose window was picked (§1), so after a window is
chosen the GUI shows one new card — **"Whose audio?"** — listing apps
currently emitting audio (from the helper's `apps` events: icon-less,
`application.name` + binary, live-updating), plus two fixed rows: **Whole
system** (today's monitor capture, unchanged) and **No audio**. AD3: the
card always appears in app mode; the last-used binary is preselected when
present and emitting, so the common restart-the-same-game case is a single
Enter.

- The card states its one honest caveat inline: *"Apps appear here when they
  play sound — start the game's audio first if the list looks empty."*
  Streams only exist once an app opens them; the list updating live (helper
  registry events) makes this a wait, not a dead end.
- Because the list is built from **emitting streams**, Windows' V-3a trap
  (picked the game process, audio lives in a helper process) mostly cannot
  happen — the user picks whoever is actually making sound. The residual
  case (the app goes silent later because a *different* binary took over) is
  D6's hint.
- CLI: `-audio-app <binary>` selects app audio headlessly; unset means
  system audio exactly as today. No interactive prompt in the CLI — it is
  the engine harness, not the product (docs/19).
- Mid-session, the card collapses to a status line (chosen app + link
  state) with one action: **Switch to whole-system audio** — the single
  permitted mid-session audio change, inherited from docs/38 D8 with the
  same justification: a re-link/re-target is a new capture, not a
  renegotiation — same Opus stream, same seq space, viewers notice nothing.
  In this design it is even smaller than on Windows: the helper re-links the
  sink from the app's ports to the default sink's monitor ports (or the
  engine swaps nothing at all in the gst child either way).

### D6 — Failure semantics: subordinate audio, a stream-presence hint, and no new ways to kill video

R25 Decision 6 is inherited whole; this decision only maps the new failure
modes onto it:

| Failure | Behavior |
|---|---|
| Helper missing / fails to start | Whose-audio step is replaced by a sentence + system-audio fallback offer; video proceeds |
| Helper dies mid-broadcast | Audio marked errored, notification, one-click "switch to system audio" (which needs no helper); video untouched |
| Target app's streams all vanish | Links drop to zero; after ~10 s a hint: "No audio from <App> right now — it may have stopped playing sound, or another process took over. Switch to whole-system audio?" — the docs/38 D8 escape hatch, driven by link count instead of a level meter |
| Target app reopens streams (churn) | Helper re-links on the registry event (AD2: matched by `application.process.binary`); the gap is dropped packets, which the wire model already treats as truth (AG4 gates the round-trip) |
| Default sink layout change | Helper recreates sink + re-links; `audioconvert` absorbs the monitor caps change (D3) |
| Sink monitor capture fails in gst | Existing audio-branch failure handling: the one clean audio-off re-run (docs/28 Decision 7's attribution rules apply; the new source element is a `pipewiresrc`, so the video-vs-audio attribution caveat there already covers it) |

Nothing in this table can fail the broadcast. The only new *video* code path
is D2's fit math, which is pure arithmetic under unit test.

### D7 — CI gets a real PipeWire: the audio plane is integration-tested headless

Unlike the portal/GPU plane (structurally invisible to CI — docs/19's
standing warning), the entire audio control plane runs on a runner: a full
PipeWire daemon + WirePlumber run headless with null devices. A new CI step
(inside the existing broadcast job, no new workflow) starts one and the
integration suite drives reality, not fakes:

- Synthetic emitters: `gst-launch-1.0 audiotestsrc ! pipewiresink` (or
  `pw-play`) with `application.process.binary` set via `PIPEWIRE_PROPS` —
  a fake "game" whose streams the tests start, kill, and restart.
- AG4: kill and restart the emitter; assert the helper re-links and samples
  flow again, gst pipeline untouched.
- AG5: `kill -9` each of {emitter, helper, gst child} in turn; assert
  `pw-dump` shows no gawk-named sink or links afterward.
- AG2: byte-compare `BuildPipeline` output for monitor mode against the
  pre-R35 golden args.
- The helper's protocol state machine additionally gets pure unit tests over
  an injected event seam — daemon-free, for the logic CI's matrix doesn't
  reach (event reordering, duplicate serials).

What CI still cannot prove — the compositor/portal/GPU half — is §6's
V-register, same honesty as R14: the on-hardware pass decides whether the
milestone works.

## 5. Chunks

Prefix **AS** (App Sharing — unclaimed; single letters are all spoken for
per convention). Ordered so the CI-provable core lands before any UI, and
the doc-gated on-hardware pass is last.

### AS1 — Portal metadata + fit geometry ✅

`ParseStartResults` returns `{NodeID, SourceType, Size}`; the `fit` function
with docs/38's golden cases; `encoderCaps` pins fitted dims; stats/GUI show
them; source_type in stats + R28 report.

| Acceptance | Verified by |
|---|---|
| Start results with/without `source_type`/`size` parse; absent ⇒ monitor-equivalent behavior | unit |
| Fit math matches the docs/38 D11 semantics (pin one side, shrink the other, floor to even), golden cases shared | unit |
| Monitor-mode pipeline args byte-identical to pre-R35 for 16:9 sources (AG2) | unit, golden compare |
| A 1000×700 window at the 1080p rung encodes 1542×1080 (even-floored fit), shown in stats | unit + manual later (V-1) |

### AS2 — The helper: scaffold, build, protocol ✅

`cmd/gawk-pw-helper` builds (cgo, `libpipewire-0.3-dev` in CI apt list);
registry watch + `apps` events + protocol state machine; packaging: ships in
the CI artifact, `BUILD-INFO.txt` + INSTALL.md `ldd` rows updated.

| Acceptance | Verified by |
|---|---|
| Helper builds in CI; links only stock libraries (AG10) | CI + `ldd` assertion |
| `apps` events list synthetic emitters with binary + name; list updates on emitter start/stop | integration vs. headless PipeWire (D7) |
| Protocol state machine survives event reordering, duplicate serials, stdin EOF ⇒ clean exit | unit over injected seam |
| stdin EOF / SIGKILL both leave `pw-dump` clean of gawk objects | integration |

### AS3 — Sink + link engine ✅

Sink creation (default-sink layout, `Audio/Sink/Internal`, per-session
name), `capture` op: link target binary's streams, follow churn, re-link on
default-layout change, `links` events, `release`.

| Acceptance | Verified by |
|---|---|
| Emitter audible on default sink **and** captured on gawk sink simultaneously (tee, not re-route) | integration: capture both monitors, assert samples on each |
| Emitter kill + restart re-links; second stream from same binary links too (AG4) | integration |
| Default sink layout change ⇒ sink recreated, links restored | integration (swap null-sink profiles) |
| kill -9 matrix leaves no sink/links (AG5) | integration |

### AS4 — Engine integration ✅

`AudioTarget` in `MediaConfig`; the `app-sink-monitor` candidate +
`BuildPipeline` target args; helper supervision; `ChooseAudioTarget` seam;
CLI `-audio-app`; D6's failure mapping incl. the mid-session switch;
`-audio-device` precedence note.

| Acceptance | Verified by |
|---|---|
| App-mode pipeline captures the sink monitor and produces the identical Opus contract (branch args downstream of the source byte-equal to today's) | unit |
| Every D6 row behaves as specified; no row stops video (AG6) | unit + integration |
| Mid-session switch to system audio: same seq space, no config re-emit, gap = dropped packets only | integration vs. real relay (AG7's harness) |
| `-audio-app` headless path works; unset ⇒ pre-R35 behavior | integration |

### AS5 — GUI: the whose-audio card ✅

The card (live list via helper events, Whole system / No audio rows,
preselect-last per AD3, the "apps appear when they play sound" caveat), the
live status line + one-click switch, error sentences for every D6 row.

| Acceptance | Verified by |
|---|---|
| Card appears only when `source_type == WINDOW`; monitor picks never see it (AD1) | unit over app state |
| Preselect only when last binary is present and emitting; otherwise no selection (AG9) | unit |
| Idle-window zero-frame invariant (docs/19 V9's layout test) still holds with the new card | existing layout test extended |
| Copy review: every failure is a sentence a friend can act on | review |

### AS6 — Docs + telemetry closeout ✅

INSTALL.md (helper row, package list), module README, README gotchas if any
new one landed, R28 report fields, ROADMAP status flip.

| Acceptance | Verified by |
|---|---|
| A fresh reader can install and use app mode from INSTALL.md alone | review |
| R28 report carries mode + chosen binary + link state | unit |

### AS7 — On-hardware verification (manual, decides the milestone) — outstanding

The §6 register on the gaming PC (KDE) + a GNOME session, closing AG1, AG3,
AG8, AG9's manual halves.

## 6. On-hardware verification register

Pre-registered, docs/38 §10 style — each is a claim this doc makes on paper
that only a desktop can prove:

| # | Claim to verify |
|---|---|
| V-1 | Fit: a deliberately odd-sized window encodes fitted-not-stretched; the GUI shows fitted dims (AG3) |
| V-2 | Window resize mid-broadcast: renegotiation either survives (same-aspect invisible; aspect change stretches) or the child dies and the cascade restart reuses the portal session without a picker — per compositor, KWin + Mutter |
| V-3 | Exclusive-fullscreen transition of the captured window: the docs/19 "capture stops on fullscreen" report, re-tested first-hand at last — window streams specifically |
| V-4 | Occluded and minimized window delivery, KWin vs. Mutter (expected: occluded fine on Wayland; minimized varies) — whichever fails gets an in-GUI hint, not a fight |
| V-5 | AG1 proper: second app's audio inaudible in a real viewer while the game's audio plays |
| V-6 | Game-audio churn in the wild: menu→game transitions, engine restarts — links follow without audible artifacts beyond the gap itself |
| V-7 | AG8: A/V sync numbers through the sink hop, viewer diagnostics over 60 s |
| V-8 | The silence hint fires on a genuinely-silent target and its one-click switch works mid-session |
| V-9 | Latency: glass-to-glass unchanged from the R14 baseline on the same rig (the video path did not change; this is a regression check, not a new measurement method) |

## 7. Rejected

- **`pw-cli`/`pw-link`/`pw-dump -m` as the control plane** — owner decision
  AD4 (2026-08-01). Rejected for v1 robustness, not distaste: link
  maintenance under game-launch churn is an event-ordering problem, and
  snapshot-and-shell-out tooling races it. Revisit only if the helper's
  build cost ever becomes real pain, with the churn integration suite (AS3)
  as the bar it would have to pass.
- **In-process libpipewire in the main binary** — forfeits the module's
  settled crash-isolation posture (docs/19 Decision 3) to save one small
  subprocess.
- **Direct single-node capture (`pipewiresrc target-object=<app stream>`),
  even as a rung** — misses multi-stream apps, dies with the first stream
  teardown, and would force gst pipeline restarts on churn — the exact
  dynamism the sink exists to absorb. One shape, not two.
- **`pactl move-sink-input` / re-routing the app into a capture sink** —
  mutates the user's audio routing; a crash strands the app on a hidden
  sink with silent speakers. The tee shape's whole point is that the worst
  crash leaves *nothing* re-routed.
- **A null-sink + loopback re-route** (the classic recipe) — same mutation
  problem plus a permanent extra latency stage the loopback adds.
- **Portal-based app audio** — there is nothing to reject: ScreenCast
  carries no audio and no audio portal exists (docs/28 Decision 1, spec
  re-verified 2026-08-01). Recorded so nobody goes looking.
- **Guessing the window→app correlation** (title vs. `application.name`
  heuristics) — a wrong guess silently streams the wrong app's audio, the
  worst failure this feature can have. The explicit step is one click and
  never lies.
- **An explicit pre-Start mode switch** (Windows D12's shape) — AD1; it
  adds a disagreement state and no capability on a desktop whose picker
  already offers both kinds.

## 8. What the build found (2026-08-01)

Written down because each of these cost a debugging cycle and two of them
amend a decision above. Where §8 and an earlier section disagree, §8 is what
shipped.

### F1 — `application.process.binary` is not in the registry's global properties

AD2's identity is reachable **only by binding the object**. Measured against
PipeWire 1.0.5: a `Stream/Output/Audio` node's registry globals carry
`client.id` but no application identity at all, and the owning Client's globals
stop at `application.name`. The binary appears only in the properties a *bound*
object reports through its info event.

Consequences, all of them load-bearing:

- The helper binds every Client and every audio-output Stream node, and merges
  the bound properties over the global's (`pwgraph.Merge`). A helper that
  trusted globals alone would render an application list it could not link.
- Identity resolves node → its client, not node alone. GStreamer's
  `pipewiresink` is one of the producers that sets nothing on the node, and CI's
  synthetic emitters are exactly that shape.
- **The helper must complete two registry round-trips before reporting
  `ready`.** The first delivers the globals and triggers the binds; the info
  events land after its `done`. A one-round-trip ready published an empty
  application list while a game was playing — which is precisely the "no apps"
  vs. "not looked yet" confusion EventReady exists to prevent.

### F2 — The capture sink is created once and never recreated (amends D3)

D3 says a default-sink layout change makes the helper "recreate the sink and
re-link". Implementation rejects the recreate half: the gst pipeline addresses
the sink by `object.serial`, so destroying and recreating it changes the target
out from under a running `pipewiresrc` — turning a headphone switch into a dead
audio branch and a full audio-off re-run. The design's own claim that
"`audioconvert` absorbs the monitor's caps change" holds for a caps change; it
does not survive the node disappearing.

What ships instead: the sink is created once per broadcast, with a layout
chosen from **the target application's own output ports** (falling back to the
widest real sink, then stereo), and a layout change mid-session is absorbed by
re-linking into the existing sink — channel-matched where names agree,
positional otherwise. This is a better fit for D3's actual goal than the
default-sink reading was, and F3 explains why.

### F3 — A surround machine is modelled by surround speakers, not a surround application

D3 wants the sink to match the default sink's layout *because* that is the
layout the application's ports negotiated. That turns out to be exactly right,
and more directly available than the design assumed: an application's stream
node is an **adapter**, and its output ports speak the layout it negotiated
toward the default sink. A 5.1 application playing to stereo speakers has
**stereo** output ports — its downmix already happened — so a stereo capture
sink drops nothing. Reading the application's own ports asks D3's question
directly, needs no `default.audio.sink` metadata binding, and is right even
when the application is not playing to the default sink at all.

The test that proves it therefore configures 5.1 *speakers*, not a 5.1
emitter — the first version of that test failed for the right reason and taught
this.

### F4 — The mid-session switch needed its own operation

D5's "the helper re-links the sink from the app's ports to the default sink's
monitor ports" is a second link plan, not a variation of the first, so the
protocol grew `capture-system` alongside `capture`. Internally it is the same
code path with a sentinel target, which is what keeps re-targeting, the link
diff and the events identical for both — only the plan differs. Verified
against a real daemon: the sink keeps its serial across the switch, so the gst
pipeline and the Opus sequence space are untouched.

### F5 — A capture request must always be answered with a link count

Zero is the most meaningful value the link count takes — it is what the silence
hint is built on — so the helper reports it on every capture request rather than
only on a change. Without that, capturing an application that had gone quiet
since the user picked it produced silence on the protocol as well as in the
stream, and the engine waited for news that was already true.

### F6 — What CI can now prove

D7's bet paid off: a full PipeWire daemon, WirePlumber and a null sink run
headless on a runner, and the whole audio control plane is integration-tested
against them (`internal/pwtest`) — the tee (by capturing *both* monitors and
requiring signal on each), stream churn, multi-stream applications, the surround
layout, re-targeting, the mid-session switch, and a `kill -9` matrix that
asserts `pw-dump` comes back clean. The engine's own seam is covered end to end
too, against the real helper.

The harness needed three things the design did not anticipate, all of them
environment rather than code:

- **WirePlumber requires a session bus** — it exits immediately without one.
- **An emitter started before WirePlumber has *adopted* a default sink** dies
  with "no target node available", which reads like a bug in the code under
  test rather than a race in the harness. Both the sink node and the default
  are waited for explicitly.
- **The stock WirePlumber configuration cannot run on a container runner.** It
  enables the ALSA, V4L2, libcamera and Bluetooth monitors, and the Bluetooth
  half loads the logind plugin; with no `/run/systemd` and no system bus that
  fails hard enough to take WirePlumber down ("failed to start systemd logind
  monitor: -2", then "disconnected from pipewire") — and with no session
  manager nothing routes, so an application's stream never negotiates ports and
  the entire graph is untestable. Found only in CI: a developer's machine has
  logind and never sees it. The harness therefore writes its own config
  directory enabling the session manager's *linking* half and nothing else.
  None of the dropped monitors could have contributed anything — every device
  in these tests is a null sink the harness creates itself.

Two guards keep this honest rather than merely green. The harness **skips**
(never fails) when the environment cannot host a sound server, since that says
nothing about the code under test; and CI **fails on that skip**, because a
silent skip would turn this section's coverage claim into something nothing
checks.

`-race` also caught a genuine defect in the harness itself: an `exec.Cmd`
captures a child's output on its own goroutine, so a `t.Cleanup` that reads the
buffer to print it races the copier. Every captured log is behind a mutex now.

### F7 — Two deviations from AG7's letter

AG7 asks for no diffs outside `gawk-broadcast/`. Two landed, both additive and
neither on the media path:

- `gawk-telemetry`'s schema types the two new broadcaster fields (`shareMode`,
  `audioApp`). Without it they arrive as untyped unknowns — stored but not
  queryable — and AS6's "the R28 report carries mode + chosen binary" would be
  true only in the weakest sense. "The wrong app's sound went out" is exactly
  the complaint these two answer.
- `.github/workflows/ci.yml` installs `libpipewire-0.3-dev` and the PipeWire
  runtime, and asserts the helper's linkage (AG10).

AG7's substance — zero wire, relay, viewer or browser-broadcaster changes, and a
stock viewer playing an app-mode broadcast — is intact.
