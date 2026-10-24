# R76 — Broadcaster memory, measured (docs/77)

**Status**: designed and implemented 2026-10-24 (MM1–MM4, MM7–MM8), from an
owner report, the measurements in §2 and a telemetry audit (D12–D16). Owner decisions OD1–OD2 taken
2026-10-24 (§3). Open: MM5's hardware pass and MM6's baseline and
reduction work (§5). Chunk prefix `MM` (Memory Measured).

Touches `gawk-broadcast-desktop` (an engine `memory` module, the stats, the
telemetry reporter, every shell), therefore `gawk-ios` (same engine, by
path), `gawk-telemetry` (fields, the rollup, three playbook rules), and —
for the telemetry audit (D12–D16) — `gawk-app` (the session budget, the
viewer's telemetry session on a relay switch) and `gawk-server` (a PoP
leg's publisher kind and ingress leg).

---

## 1. Why

The owner watched the Windows broadcaster's memory in Task Manager rise from
about 250 MB to almost 750 MB after switching Delivery from *Lowest latency*
to *Reliable* in the middle of a multi-hour broadcast. Two things were wrong,
and telemetry could show neither:

- **The jump.** Nothing on the wire said how much memory the broadcaster
  held, so there was no way to tell a leak from a step, or to line the step
  up with the setting change.
- **The baseline.** 250 MB is too much for an app that runs beside the game
  it shares. The owner's goal is minimal resources, not hundreds of MB.

So this milestone makes memory a measured, reported quantity with a budget,
before anyone tries to reduce it (the measure-first rule docs/75 set).

## 2. What was measured (2026-10-24)

### M1 — The Reliable send path does not hold the memory

The engine's real `Sender` and `dial_publish` transport, driven at 20 Mbps,
60 fps and a 500 ms GOP against a local `gawk-server`, BBR, two minutes per
run, RSS read from `/proc/self/status` every 10 s (Linux; the Windows shell
uses the same engine). Impairments from `tc netem` in a private network
namespace.

| Path | Lowest latency | Reliable |
|---|---|---|
| Clean loopback | 7.4 MB, flat | 7.2 MB, flat (180 carriers) |
| 1 % loss, 30 ms RTT | 8.2 MB, flat | 7.7 MB, flat (4 carriers expired) |
| Capped at 12 Mbit/s, the stream does not fit | 9.4 MB, flat | 10.2 MB, flat (230 of 231 carriers expired, 14 440 records shed) |

The carrier lane's queue is shed at every keyframe (docs/75 D3) and QUIC's
buffers are capped (1 MiB of datagrams, a 10 MB stream send window). The one
known leak in this path, a quinn waiter per reset stream (`BUGS.md`), is
under 1 MiB an hour. **The jump is not in the send path**, and the shared
networking code accounts for about 10 MB of either figure. Where it is could
not be found from Linux; that is what the instrumentation below is for.

### M2 — Native samples stopped after about 90 minutes

A realistic native `Stats` sample serializes to **1 526 bytes** (1 712 with
this milestone's fields). At the fleet's 2 s cadence the reporter's 4 MiB
session budget (docs/33 §4.3, documented as "≈ a 5-hour session") lasted
**92 minutes** (82 with the new fields); after that it sent events only.
The budget is 16 MiB now (D12): about 5.4 hours. Memory creep is a question
asked over a whole broadcast, so memory still rides an event that outlives
the budget (D9).

### M3 — Where the broadcaster holds memory

From reading every shell (2026-10-24):

| Holder | Platforms | Bound |
|---|---|---|
| Capture frame pool | Windows WGC: 2 BGRA frames at the **source** size, GPU. macOS SCK: 5 NV12 IOSurfaces at the encode size. iOS SCK: default depth at native size. Linux pipewiresrc: element defaults, mostly DMA-BUF | fixed per stream |
| Conversion targets | Windows VideoProcessor: 1 + 4 ring NV12 textures at the encode size. iOS: two `CVPixelBufferPool`s | fixed per stream |
| Encoder internals | MFT, VideoToolbox, GStreamer: opaque | driver-owned |
| `FrameGate` (encoder → sender) | all, engine | 8 access units |
| Carrier lane queue | all, engine | **unbounded within a GOP** (shed at each keyframe) |
| Keyframe slot | all, engine | 1 in flight + 1 pending, ≤ 8 MiB each |
| QUIC send buffers | all, quinn | 1 MiB datagrams + 10 MB stream window, invisible to the app |
| Telemetry reporter | all, engine | 64 samples, 256 events, 8 batches |
| Thumbnail and preview pictures | desktop shells | ≤ 320 px wide RGBA, two of them |
| UI renderer, GPU driver, media frameworks | all | opaque |

The debug log writes to disk and holds nothing. Audio buffers are small and
fixed. One lead for MM6: the Windows shell allocates a new GPU render target
and a staging texture for the live thumbnail **every second**
(`d3d.rs` `thumbnail_rgba`).

## 3. Owner decisions

| # | Question | Decision |
|---|---|---|
| OD1 | What may the native broadcaster hold? | **100 MB while live** (2026-10-24), in the footprint the OS's own task manager shows (D1). |
| OD2 | How deep should the first breakdown go? | **Full per-component** (2026-10-24): process totals, the Rust heap, GPU memory, and byte gauges per subsystem (D2–D5). |

## 4. Decisions

### D1 — The budget: 100 MiB of footprint while live (OD1)

Measured as D2's footprint, in MiB, because Task Manager and Activity
Monitor label MiB as "MB": the verdict must match the number the person
broadcasting sees. "While live" because that is when the game is competing
for the same memory; the Ready page is not judged. The constant is
`memoryBudgetBytes` in `gawk-telemetry/internal/rules/playbook.go`.

### D2 — The footprint is the task manager's figure

`gawk_engine::memory::process()`, one probe per OS, each with no new
dependency (the engine keeps its no-COM rule):

| OS | Footprint (`memFootprintBytes`) | Also |
|---|---|---|
| Windows | Private working set (`K32GetProcessMemoryInfo`, `PROCESS_MEMORY_COUNTERS_EX2`; `PrivateUsage` before Windows 10 1809) — Task Manager's *Memory* column | `memCommitBytes`: committed private memory, Task Manager's *Commit size*. A leak that pages out leaves the working set and stays here. |
| macOS, iOS | `phys_footprint` (`task_info(TASK_VM_INFO)`) — Activity Monitor's *Memory*, and what iOS's jetsam limit is charged against | The kernel's own peak (`ledger_phys_footprint_peak`); iOS's `memLimitRemainingBytes` |
| Linux | `RssAnon + RssShmem` from `/proc/self/status` — PipeWire's memfd and DMA-BUF buffers are shmem | — |

`memResidentBytes` is every resident page everywhere (working set, RSS),
shared libraries included. `memFootprintPeakBytes` is the OS's peak where
it keeps one (Apple), else the highest any snapshot has seen.

### D3 — The Rust heap, counted

`CountingAlloc` wraps the system allocator with two relaxed atomics per
allocation; each binary opts in with `#[global_allocator]` (the three
desktop shells, and the iOS core under `cfg(target_os = "ios")`).
`memHeapBytes` and its peak are absent where it is not installed — the
tests, any future shell that has not opted in. Footprint minus heap is what
the process holds through libraries: the GPU driver, Media Foundation,
VideoToolbox, GStreamer, the UI renderer.

### D4 — Five components, each reporting where it allocates

`memory::Component` is a closed set; each is a typed field:

| Component (field) | Counts | Where |
|---|---|---|
| Capture (`memCaptureBytes`) | Captured frames between capture and the encoder, at pixel size, wherever they live — mostly GPU | Windows: WGC's pool + the converter's NV12 targets, set per frame. macOS: SCK's queue depth × NV12 at the fitted size, set at start |
| Encode (`memEncodeBytes`) | Access units in the engine's `FrameGate` | engine, every platform |
| Send (`memSendBytes`) | Carrier records queued for the uplink; the keyframe in flight and the one pending | engine, every platform |
| Preview (`memPreviewBytes`) | The live thumbnail and the Ready page's preview, as RGBA | the shared desktop UI |
| Telemetry (`memTelemetryBytes`) | Batches queued for posting | engine, every platform |

Queues account through `memory::Held`, a guard that adds its bytes when an
item is queued and takes them back when it drops — sent, shed or cleared,
whichever path drops it — so a queue's gauge cannot drift. A component is
absent until it reports and 0 once it has declared itself, so "empty" and
"not measured" stay apart. Not counted, and named as such: QUIC's own
buffers, the encoders' internals, the UI renderer, Linux and iOS capture
(§7).

### D5 — GPU memory: what the process is charged

Windows: DXGI's per-process `QueryVideoMemoryInfo`, `LOCAL` plus
`NON_LOCAL` (an integrated GPU has almost only the latter), summed over
every adapter — capture, the MFT's surfaces and the UI's renderer all count,
on whichever adapter they landed. Installed once at start by
`gawk_capture::d3d::install_gpu_memory_probe`. Apple: the kernel's graphics
ledger from the same `task_vm_info` call. On Apple's unified memory this is
**inside** the footprint; on a discrete Windows GPU it is **beside** it.
Linux: not measured (`memGpuAvailable` false).

### D6 — Sampling: once a second, where the stats are read

`Session::stats()` applies `memory::snapshot()`, so every shell's 1 Hz tick
and every telemetry sample carry it without shell plumbing; the desktop
shell also applies it when no session exists (diagnostics from the Ready
page). Reading costs microseconds. A spike between two reads is missed,
except in the Apple peak.

### D7 — Local surfaces: diagnostics and the log, not the main window

*Copy diagnostics* gains a `memory` object (null fields where unmeasured),
live or not. The once-a-minute `health:` log line ends with a memory clause
(`memory 212 MiB (peak 230), heap 38, gpu 410, capture 26.4, …`). No
main-flow UI: memory is the developers' concern, not the broadcaster's.

### D8 — Mid-stream changes are events

Changing Delivery while live records a `delivery-changed` event (detail
`reliable` or `lowest-latency`) beside the existing `restart` for a quality
change, so a step in memory lines up with its cause on the session's
timeline.

### D9 — Memory rides a minute event that outlives the byte budget

The reporter records a `memory` event once a minute while the process is
measured, ahead of its budget check, so it keeps flowing after samples stop
(M2). Detail, fixed order, measured parts only, at most about 120 of the
256 characters a detail may carry:

```
footprint=<bytes>,peak=<bytes>[,heap=<bytes>][,gpu=<bytes>][,commit=<bytes>]
```

About 7 KB an hour. Components stay in the samples: they explain a level,
and the level is what the event tracks.

### D10 — The rollup: series, and a whole-session summary

The eleven memory gauges join the broadcaster's rollup series (over the
samples' span). `Row.memory` (additive) summarizes the footprint over the
whole session, from the minute events when there are at least two, else
from the samples: points, span, median, peak, and — given at least
20 minutes — the medians of the 5-minute window after a 2-minute warm-up and
of the last 5 minutes, and their difference, `growthBytes`. Below 20
minutes, or with fewer than three points in a window, there is no growth
figure rather than a zero.

### D11 — Two playbook rules, both warnings

| Rule | Requires | Fires when |
|---|---|---|
| `memory-over-budget` | `client.memFootprintBytes` | footprint > `memoryBudgetBytes` (100 MiB) |
| `memory-growth` | `client.memFootprintGrowthBytes` | growth > `memoryGrowthWarnBytes` (32 MiB) |

Both are **warn at any size**: severity `bad` means broken from a viewer's
point of view, and memory costs the machine, not the stream. Evidence cites
the peak, the heap and the GPU figure where present. The live dashboard
reads the footprint from the last sample; the stored session from
`Row.memory`'s median (the events cover the whole session), and the growth
fact exists on the stored session only — a live window cannot see hours.
Browser broadcasters report no memory, so both rules read *unavailable*
there, as `relay-intake-drops` already does.

### D12 — The per-session telemetry budget is 16 MiB (owner, 2026-10-24)

`SESSION_BYTE_BUDGET` goes from 4 to 16 MiB in both clients — the engine's
reporter and `gawk-app`'s collector share the contract (docs/33 §4.3).
About 5.4 hours of native samples at 2 s. The service enforces no
per-session total (it caps one POST at 1 MiB, `schema.MaxBodyBytes`), so
nothing else changes; a session that reaches it still drops to events only
and says so.

### D13 — What the broadcaster was told is on the timeline

Beside `delivery-changed` (D8), the native shells record what the person
broadcasting saw: `uplink-warning` (`raised` / `cleared`, the desktop shell
and the iOS app) — whether that warning was right is R75's question, and
until now nothing recorded when it showed — `uplink-loss` (`raised` /
`cleared`, docs/57 D7's loss notice) and `network` when the link under the
broadcast changes (`wifi=…,vpn=…,awdl=…`: booleans, never a name or an
address).

### D14 — The stored row says which uplink ran (audit, 2026-10-24)

An audit of every field the native broadcaster sends against what the
service types, rolls up and turns into facts found three gaps:

- **The uplink's configuration never reached a permanent row.**
  `uplinkModeRequested` (the Delivery setting), `uplinkMode` and
  `uplinkCongestion` were typed since R73 but lived in the 14-day raw window
  only. They, and `shareMode`, join the broadcaster's rollup config, so
  history groups by them; and the first three become `text.*` facts on both
  surfaces, so a verdict can say which uplink it is about.
- **Today's spellings were never rolled up.** The rollup listed the legacy
  capitalized `SentFrames`, `FramesDroppedAtSend` and `KeyframeIntervalMs`
  that binaries sent before the engine carried JSON tags; the lowerCamelCase
  the engine sends now was not listed, so those — and the carrier lane's
  counters, quinn's datagram drops, parity, resumes and capture rebuilds —
  never reached a row. They are listed now.
- **Parity was untyped for broadcasters.** `parityLevel`,
  `parityChunksSent` and `parityBytesSent` were in the viewer table only, so
  every native sample counted three unknown fields.

### D15 — A telemetry session is one relay session (fixed 2026-10-24)

A web viewer that followed home's offer to a PoP (docs/76) kept home's
telemetry session: stopping a session drops its `ended` event, and a PoP
sends no telemetry hello (docs/76 §8), so nothing ended it, and the
collector went on posting the PoP connection's stats under home's token — a
home row describing a relay home never saw. The same happened on any
redial to a relay without telemetry. The viewer now ends the telemetry
session whenever it tears a relay session down; the next relay's hello, if
any, begins a new one.

### D16 — A PoP's uplink leg is its own leg (fixed 2026-10-24)

Home holds a PoP's uplink leg as the broadcast's publisher, so the ingress
window that measures a broadcaster's uplink everywhere else measured the
venue's internet line there, and fed the broadcaster-leg totals, metrics and
the `leg-a-broadcaster-uplink` verdict ("drop a rung on the broadcaster").
Now the hub marks that publisher `pop-leg` (`/statusz` `publisherKind`; no
PoP name, since `/statusz` is public), its ingress loss folds into a third
leg (`popLegIngress*` totals, `gawk_*_pop_leg_ingress_*` metrics) beside the
broadcaster and edge legs, and `gawk-telemetry` turns it into
`relay.publisherPopLeg`: `leg-a-broadcaster-uplink` and
`relay-intake-drops` stand aside, and **`pop-uplink-leg`** names the venue's
line instead.

## 5. Chunks and acceptance criteria

| Chunk | What | Acceptance | Verified by | Status |
|---|---|---|---|---|
| **MM1** | Engine: `memory` module — the per-OS probe (D2), `CountingAlloc` (D3), components and `Held` (D4), the GPU-probe seam (D5), `snapshot()` (D6); the `mem*` fields on `Stats` | The process is measured on every desktop OS; Linux's parse is pinned; a component is absent until it reports and a `Held` gives back on drop; the allocator balances alloc/realloc/dealloc; the stats key-set pin lists every new name and the optional ones are absent at default | `memory.rs` unit tests on the Linux runner and the macOS job; `stats.rs` key-set and absent-optional tests; the Windows and iOS code compiled (cargo-xwin; `rustc --target aarch64-apple-ios`) | ✅ 2026-10-24 |
| **MM2** | Engine queues and shells: `FrameGate`, carrier lane, keyframe slot and telemetry batches report (D4); Windows capture + GPU probe, macOS capture, desktop pictures; the counting allocator in every binary; diagnostics and the health line (D7); `delivery-changed` (D8) | Against the real `Sender` with a stalled carrier, the `send` gauge rises past the queued records and falls back when the next keyframe sheds them; the `encode` gauge follows push/pop/drop exactly; every shell builds; clippy clean | `crates/engine/tests/memory_gauges.rs` (its own binary: the gauges are statics); workspace build + clippy on Linux and cargo-xwin | ✅ 2026-10-24 |
| **MM3** | Reporter: the minute `memory` event (D9) | One at the first measured sample, then once a minute; still recorded after the byte budget truncated the samples; unmeasured parts left out; none for an unmeasured process | `telemetry.rs` `memory_rides_a_minute_event_that_outlives_the_byte_budget` | ✅ 2026-10-24 |
| **MM4** | `gawk-telemetry`: field types and catalogue, rollup series and `Row.memory` (D10), the two rules (D11) and their producers | The owner's report (250 MB for three hours, then 750 MB; samples cut off by the budget at 90 minutes) yields a 500 MiB growth from the events; warm-up is not growth; < 20 minutes has no growth figure; samples are the fallback; malformed details and viewers produce nothing. Each rule fires on its signature and not on a healthy session, both stay `warn`, evidence names heap and GPU; both producers emit exactly their share of `ProducibleFacts`; stored shapes are additive only | `rollup/memory_test.go`; `rules_test.go` (the playbook table, `TestMemoryFindingsWarnAndSayWhere`); `producible_test.go`, `readapi_test.go`, the live producer test; `storedshape` golden | ✅ 2026-10-24 |
| **MM5** | Hardware pass | On Windows, macOS and iOS hardware, `memFootprintBytes` is within 5 % of Task Manager's *Memory* / Activity Monitor's *Memory* / Xcode's memory gauge at the same moment, and `memGpuBytes` within 5 % of Task Manager's per-process GPU memory on Windows. The owner's report is reproduced or ruled out: a ≥ 2-hour Windows broadcast switched to Reliable at hour one, read from telemetry (`memory-growth`, the `delivery-changed` event, the components) | Recorded in §9 with the build, the machine and screenshots | not started |
| **MM7** | Telemetry coverage for diagnosis: the 16 MiB budget (D12), the timeline events (D13), the rollup and typing gaps (D14) | `SESSION_BYTE_BUDGET` is 16 MiB in both clients and the browser's exhaustion test still exhausts it; the stored row carries the uplink's configuration and today's counter spellings; parity is typed for broadcasters; both producers emit exactly their share of `ProducibleFacts` | `telemetry.test.ts`; `readapi_test.go` (the maximal broadcaster session) and the live producer test; `storedshape` golden | ✅ 2026-10-24 |
| **MM8** | PoP and fleet kept apart (D15, D16) | A viewer following an offer posts its final batch for home and nothing from the PoP; home's `/statusz` names a PoP leg's publisher `pop-leg` and the PoP's own hub does not; a PoP leg's ingress loss is in the pop-leg totals and metrics and in neither the broadcaster nor the edge family, and stays there after the leg hands back; on a PoP leg the broadcaster's rows stay quiet and `pop-uplink-leg` fires | `ViewerScreen.test.tsx` (`ends the home telemetry session when it follows an offer…`); `hub` `TestAPopLegsIngressIsItsOwnLeg`; `transport` `TestOutsideViewerGetsTheBroadcastByteIdenticalOnlyOnDemand`; `metrics` `TestLossAttributionByLeg`; `rules_test.go` (`pop-uplink-leg`, `TestAPopLegsLossIsNeverTheBroadcastersUplink`) | ✅ 2026-10-24 |
| **MM6** | Baseline and reduction | One week of fleet sessions per platform gives the median footprint, heap, GPU and components at each rung (§9); a reduction plan with one entry per holder above 10 MiB, ordered by size, each with its own acceptance; every native broadcaster at its default rung fits `memoryBudgetBytes` on the hardware MM5 used | §9's table; the plan as its own section or doc | not started |

## 6. Reading the numbers

- **Footprint far above heap**: the memory is a library's. On Windows look
  at `memGpuBytes` and `memCaptureBytes` first — the driver's own
  allocations for the capture pool, the converter and the MFT land in the
  process's private working set, and a 4K source makes WGC's pool alone
  66 MB of GPU memory.
- **Heap growing**: the app's own. The components say which queue; if none
  moves, it is a Rust allocation no gauge covers yet — add one.
- **Commit growing, footprint flat** (Windows): a leak being paged out.
- **A step at a `delivery-changed` or `restart`**: the setting costs memory;
  M1 says the Reliable send path itself does not.

## 7. Gaps and non-goals

- **Not counted**: QUIC's buffers (quinn exposes no size; the caps in M1
  bound them), encoder internals, the UI renderer.
- **Linux capture**: pipewiresrc's pool size is an element default and its
  buffers are DMA-BUF; no gauge until there is a reading worth trusting.
  Linux has no GPU figure either.
- **iOS capture**: SCK and `FrameConverter` live in Swift; the core reports
  the footprint, heap, jetsam headroom and the engine's queues only.
- **No memory UI for broadcasters**, and no automatic action (lowering the
  rung) on memory: this milestone measures.
- **Sessions served by a PoP** were left reporting nothing here: D15 and
  D16 keep PoP traffic out of home's data, and R77
  ([docs/78](78-pop-telemetry.md)) adds PoP data — home mints a PoP's tokens
  over its link, and the PoP's relay-side view reaches the service through
  home.
- **Not the browser broadcaster**: `performance.measureUserAgentSpecificMemory`
  needs cross-origin isolation and is Chromium-only.

## 8. Risks

| Risk | Answer |
|---|---|
| The counting allocator slows the hot path | Two relaxed atomics per allocation; the media path allocates per frame, not per byte. MM5 compares encode latency with and without it. |
| A component's estimate is wrong | Estimates are pixel arithmetic over known pool sizes, not readings; MM5 checks them against `memGpuBytes` deltas when a stream starts and stops. |
| Telemetry bytes | +12 % per sample (M2), about 7 KB an hour of events, and a long session may now store up to 16 MiB instead of 4 (D12). |
| DXGI adapters change at runtime (an eGPU) | The adapter list is read once at start; a new adapter's usage is missed until restart. |

## 9. Measurements

MM5 and MM6 record here.

## 10. References

- docs/33 §4.3 — the reporter's batching and byte budget.
- docs/75 D3, D8 — the carrier lane's shedding; uplink telemetry.
- `BUGS.md` — "every stream reset mid-`finish()` leaks a quinn waiter".
