# R80 — RNNoise noise suppression for room voice

**Status**: designed 2026-10-27 after the NS0 measurement spike (§12, raw
data and harness in [`assets/81/`](assets/81/RESULTS.md)). Owner decisions
2026-10-27 (§2). Not started: NS1–NS5. Open before NS5: the owner-device
rows (Safari on macOS and iOS, Chrome on Android, a real microphone).

**Relationship to earlier work**: R79 (docs/80) gave rooms voice through a
Mumble bridge. Its capture (D21) opens the microphone with the browser's
echo cancellation, noise suppression and auto gain, and `talkGate.ts`
decides when to send from the block's RMS (≈ −40 dBFS, 300 ms hold). This
milestone puts RNNoise between the microphone and the encoder, the
denoiser Mumble's desktop client offers (`NoiseCancelRNN`, RNNoise 0.2;
Mumble's default is still Speex, and its gate reads Speex's probability,
not RNNoise's), and lets RNNoise's speech probability decide when to
transmit. Nothing on the relay, the
bridge or the wire changes.

---

## 1. Why, and what "done" means

A room's voice is people talking while they play. The noise that matters
is the keyboard and mouse under the microphone, a fan, a TV or someone
else in the room, and today the browser's own suppression is all that
stands between it and everyone else's headphones. NS0 measured how much
that is on Chromium: **2.5 dB** off a keyboard (§12.4). An energy gate
fed that signal is open for most of a minute of typing. Mumble users have
had RNNoise for years; a web user next to them should not be the noisy
one.

With this milestone a web participant's voice is denoised by RNNoise
before it is encoded, the gate opens on speech rather than on loudness,
and an **Audio** section in the voice panel lets the user compare RNNoise
with the browser's suppression and tune the gate while hearing exactly
what the others hear.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| RNNoise is **on by default** for every web voice participant on Chrome, Firefox and Safari, desktop and phone | NS2/NS3 unit tests; NS4 Playwright on Chromium, Firefox and WebKit; NS5 owner pass on all five platforms |
| The wasm is **built from pinned source** in a pinned container; CI rebuilds it and fails on any byte difference from the committed file | NS1 CI job (a deliberate one-byte change fails it) |
| With nobody talking, a minute of keyboard noise sends **≤ 1 % of frames at −36 dBFS and ≤ 2 % at −26 dBFS** with the default gate (NS0: 0.5 % and 1.6 %) | NS3 test over the NS0 rules; NS4 Playwright with a fake-mic file (Chromium) |
| Speech is not cut: ≥ 99 % of labelled speech frames sent and, **as a mean over the four noises**, ≤ 2 % of onsets clipped by ≥ 50 ms at 10 and 5 dB SNR, default gate (NS0: 99.5 % and a mean of 1.3 %; per noise, keyboard 5.2 % — 3 of 64 onsets — and 0 % for the other three, over only 32 distinct onsets) | NS3 test; NS5's real-microphone pass, which has to confirm the onset figure on conversation |
| The gate is driven by RNNoise's **speech probability**, with a user-set threshold whose default comes from §12 | NS3 unit tests; NS4 UI test |
| The user can switch **RNNoise · Browser · Off** and the threshold mid-call, and **hear themselves** as the others hear them, without leaving voice | NS4 component and Playwright tests |
| RNNoise never blocks voice: no SIMD, a failed download, a worklet error or a CPU overrun falls back to the browser's suppression and the level gate, and says so | NS2 unit tests for each fallback |
| Added mouth-to-ear latency is RNNoise's **20 ms** and nothing more (no extra buffering) | NS2 framing test; NS4 Playwright transit check as in docs/assets/80 |
| Listen-only never downloads the wasm or opens the microphone | NS2 test |
| No voice audio is written, logged or sent anywhere new (docs/80 D24) | review; NS2 test that the worklet posts only to its own port |

---

## 2. Owner decisions (2026-10-27)

| # | Question | Decision |
|---|---|---|
| OD1 | Package or source? | **Built from source**, not an npm package. |
| OD2 | Default | **On by default.** |
| OD3 | Settings | **A sensible advanced settings UI for easy A/B testing.** |
| OD4 | What opens the gate | **RNNoise's speech probability drives the gate.** |
| OD5 | Threshold | **Configurable, with a sensible default.** |

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **The RNNoise 0.2 release** (`rnnoise-0.2.tar.gz`, sha256 `90fce4b0…`, its bundled model `0b50c45`), pinned by hash. Not `main`'s newer models, not the commit Mumble pins. | §12.3, four models through the same gates. With the default gate, 0.2 sends 99.5 % of speech, clips a mean of 1.3 % of onsets over the four noises (5.2 % on keyboard, 0 % on the rest) and sends 0.5–1.6 % of a keyboard with nobody talking; `main`'s default model clips a mean of 8.1 % and sends 8.3 % of the loud keyboard, its little model clips 11.3 % and sends none. Both of `main`'s models clip more onsets than 0.2 at every one of the 60 gate settings swept. Mumble's pin (`d983458`, model `5e78411`) gates like 0.2 (2.1 %, 2.5 %) at twice the CPU. Its SI-SDR is within 0.6 dB of the best model in every mix (best in 8 of 16, Mumble's pin in the other 8), and it is the cheapest: ~0.31 ms per 10 ms frame in Chromium, 0.17 in Firefox, 0.33 in WebKit (§12.1), 1.47 MB. What 0.2 does **not** do is remove babble (§6): 7.9 dB off a restaurant, where `main` takes 28.9 dB. That is the trade, and the gate is what this milestone is for. |
| D2 | **The build lives in `gawk-app/rnnoise/`**: `build.sh` (the NS0 `build-wasm.sh` cut down to one variant), the pins, Xiph's `COPYING`, and the built **`rnnoise.wasm`, committed**. A CI job rebuilds it in `emscripten/emsdk:4.0.10` and fails on any byte difference. | OD1. The icon set's shape (docs/53): the derivative is committed and drift-checked, so `npm run build`, the app's Dockerfile, the dev stack and vitest need no Emscripten. NS0's two full builds on one host were byte-identical for all nine variants (`assets/81/results/wasm-sha256.txt`); NS1 confirms it holds on the CI runner. |
| D3 | **One binary: `-O3 -flto -msimd128 -mavx`, `-sSTANDALONE_WASM --no-entry`, fixed 4 MB memory, no JS glue.** Exports `rnnoise_create`, `rnnoise_process_frame`, `rnnoise_destroy`, `malloc`, `free`, `memory`, `_initialize`; **imports nothing**. No scalar fallback build (0.2's scalar path does not compile anyway, §11). | Emscripten lowers `vec_avx.h`'s SSE/AVX intrinsics to 128-bit wasm SIMD; `-mavx` and `-mavx2` are within 10 % of each other on every engine, `-mavx` ahead on Firefox and WebKit (§12.1). Every engine gawk supports has wasm SIMD (all three in NS0; Safari since 16.4). A module with no imports instantiates synchronously inside an AudioWorklet with three lines of loader. 1.47 MB uncompressed, 1.21 MB gzipped. |
| D4 | **Loaded on demand.** Vite emits `rnnoise.wasm` as a hashed asset (`?url`), served from `/assets/` with the existing immutable cache header; nginx's `gzip_types` gains `application/wasm` (its `mime.types` already has it). `WebAssembly.compileStreaming` starts when the user presses **Join voice**, in parallel with the microphone prompt. Listen-only never fetches it. | The download is paid once per release, by voice users only. Starting it with the permission prompt hides it behind a dialog that takes seconds anyway. |
| D5 | **RNNoise runs in the capture AudioWorklet, on every browser, while it is on.** The existing `gawk-voice-capture` processor grows a denoise mode. The main thread compiles the module and passes the `WebAssembly.Module` in `processorOptions`; the worklet instantiates it in its constructor. Chromium leaves its MSTP pump for the worklet while RNNoise is on and goes back to it when RNNoise is off. | The audio thread is the one place that never janks with React. NS0: a compiled module reaches the worklet through `processorOptions` and the port on Chromium, Firefox and WebKit, and the worklet keeps real time. MV0 measured Chromium's worklet and MSTP paths at the same transit (23.3 ms), so leaving MSTP costs nothing. |
| D6 | **The worklet resamples to 48 kHz.** When the context does not run at 48 kHz, a linear interpolator (state carried across render quanta) turns its input into 48 kHz before RNNoise, and the 10 ms blocks it posts are 48 kHz. The encoder is configured from the first block, as today, so it now always runs at 48 kHz. | RNNoise only works at 48 kHz, and an AudioContext need not run at the rate asked (docs/gotchas.md, macOS). NS0: linear resampling from 44.1 kHz changes nothing measurable (SI-SDR within 0.05 dB, probability decisions agree on ≥ 99.5 % of frames); feeding 44.1 kHz samples as if they were 48 kHz agrees on 61–63 % (§12.5). A second, 48 kHz context would break docs/44's one context per room and still be refused on macOS. |
| D7 | **Framing and scale.** 480 samples (10 ms) per call, which is also the voice block (`VOICE_BLOCK_MS`), so a denoised block maps 1:1 onto an Opus frame. Samples are multiplied by 32768 in and divided out: RNNoise works on int16-scaled floats (Mumble's `AudioInput.cpp` feeds it shorts). The worklet posts `{pcm, prob}` per block, the PCM buffer transferred. | One block in, one block out: no added buffering beyond RNNoise's own delay. The scale is a silent failure if wrong (the model sees near-silence). |
| D8 | **RNNoise adds 20 ms**, and that is all it adds. NS0 measured 960 samples of output delay on every variant (RNNoise 0.2 added a frame of lookahead, xiph `84fe83b`). The probability returned with a call runs one frame (10 ms) ahead of the audio returned with it (§12.2), so gating each output block with its own call's probability opens the gate 10 ms early, never late. | Owned, not hidden: the settings say it, and **Browser** remains a one-click lowest-latency choice, as the R73 lesson asks. |
| D9 | **The gate: a speech detector over RNNoise's probability** (OD4), Mumble's shape plus a level floor. A block counts as speech when its probability is ≥ **on**, or ≥ **off** while the gate is already open, **and** the denoised block is louder than the **floor**; the gate stays open **hold** after the last speech block. **Defaults: on 0.90, off 0.70, hold 300 ms, floor −50 dBFS** (§12.3). The user's slider sets **on** (0.50–0.99); **off** follows at on − 0.2; hold and floor are fixed. | §12.3: without the floor, 0.2's probability stays open through the pauses in noisy speech (27–29 % of pause frames sent on keyboard and street); the floor, on a signal RNNoise has already silenced, takes that to 0 % and costs nothing in speech. Across on = 0.6–0.95 the floor gate sends ≥ 99.4 % of speech; 0.9 is the highest on that keeps onset clipping at the 0.6–0.8 level (a mean of 1.3 %; on keyboard every on ≥ 0.6 clips the same onsets, 5.2 %) while halving the keyboard sent at −26 dBFS against 0.6–0.8 (1.6 % vs 2.6–3.1 %). Mumble's gate has the same two thresholds and a hold (`fVADmax` 0.98, `fVADmin` 0.80, `iVoiceHold` 200 ms) over Speex's probability. One slider is what people tune; two are what they get wrong. |
| D10 | **The level gate stays** for **Browser** and **Off**, and becomes configurable too (OD5): its slider is in dBFS, default the current −40 dBFS (`VAD_THRESHOLD_RMS`). | Without RNNoise there is no probability; a fixed −40 dBFS cannot suit every microphone. |
| D11 | **Browser suppression is off while RNNoise is on**: the microphone opens with `noiseSuppression: false`, echo cancellation and auto gain on. **Browser** opens it with all three on (today's constraints); **Off** with suppression off and no RNNoise. Switching between them **re-opens the microphone** inside the running voice session (a capture restart, not a rejoin). | NS0: suppression twice (Chromium's, then RNNoise) left 3.8 dB more keyboard than RNNoise alone, and the level gate sent 21.5 % of a keyboard-only stretch against 7.8 % (§12.4). Chromium 156 accepts `applyConstraints({noiseSuppression})` and changes nothing (`getSettings()` stays put), so a re-open is the only switch that works. |
| D12 | **Safari can't turn its suppression off.** WebKit lists no `noiseSuppression` or `autoGainControl` constraint; its voice processing comes with echo cancellation. RNNoise runs after it there. NS5 checks on the owner's Mac and iPhone that the two together are not worse than either alone; if they are, Safari's default becomes **Browser**. | Measured on WPE WebKit, not Safari: the owner-device pass decides. |
| D13 | **Fallbacks, never a failure.** No wasm SIMD, a failed fetch or compile, an instantiate or worklet error, or the CPU watchdog (D14) moves the capture to **Browser** behaviour (suppression on, level gate) for the rest of the voice session and shows *RNNoise isn't available here: {reason}* in the Audio section. The user's saved choice stays **RNNoise**, so the next session tries again. | RNNoise is an improvement, not a dependency: voice that fails to join because a denoiser didn't load would be a regression. |
| D14 | **A CPU watchdog in the worklet.** It sums `Date.now()` deltas around each RNNoise call (the only clock every worklet scope has; with millisecond ticks the sum is unbiased over many calls) and compares them with the audio processed. Above **40 % of real time over 2 s** it bypasses RNNoise and posts `slow`, which D13 turns into the fallback. | The worklet shares the room's context with every stream's and every speaker's playback; a denoiser that can't keep up would glitch all of it. 40 % is ~12× the cost NS0 measured on WebKit (3.3 %), room for a slow phone, not for a broken one. NS5 records the real numbers on phones. |
| D15 | **The Audio section** (OD3) in the voice panel, collapsed by default: **Noise suppression** RNNoise · Browser · Off; the detector's **threshold slider** with a **live meter** (the current probability or level, the threshold mark, and a lamp when the gate is open); **Hear yourself**; **Reset to defaults**; and one line saying what the chosen mode costs (*RNNoise adds 20 ms*). Every control applies live. On a phone the section is the same, full width. | A/B means switching while talking and hearing the result. The meter answers "why didn't it send?" without guesswork. |
| D16 | **Hear yourself plays your own packets back** through a local speaker in `voicePlayback.ts`: the same Opus decoder, jitter buffer and voice bus the others' audio uses, after the gate. Off by default and never saved; the toggle warns to use headphones. | You hear what the others hear: the denoising, the codec and the gate's cuts. A raw monitor of the microphone would hide the gate. |
| D17 | **Preferences** (per browser, `voicePrefs.ts`): `gawk:voice-noise` `rnnoise` \| `browser` \| `off` (default `rnnoise`); `gawk:voice-speech-threshold` 0..1 (default D9's **on**); `gawk:voice-level-threshold` dBFS (default −40). | OD2, OD5. Same storage helpers and validation as the existing voice prefs. |
| D18 | **Push-to-talk still denoises.** The key decides when to send; RNNoise still cleans what is sent, and the meter still shows the probability. | Typing while holding the key is the case PTT users have. |
| D19 | **Licence**: RNNoise and its model are BSD-3-Clause (Xiph's `COPYING`). `gen-notices.py` gains a hand-listed entry for the vendored wasm (as `NATIVE_LIBRARIES` does for libopus), so `gawk-app/THIRD-PARTY-NOTICES.md` names it; the app image already ships that file. | The notices are generated from lockfiles, and this is not in one. |

---

## 4. Architecture

```
getUserMedia (EC on, AGC on, NS off — Safari: its voice processing)
   │  room AudioContext (any rate)
   ▼
gawk-voice-capture worklet ───────────────────────────────────────────┐
   downmix → resample to 48 kHz (D6) → 480-sample frames                │
   → ×32768 → rnnoise_process_frame → ÷32768 (D7)   watchdog (D14)      │
   post {pcm 48 kHz, prob}  ── per 10 ms block, PCM transferred         │
   ▼                                                                    │
main thread: VoiceCapture.handleBlock                                   │
   gate: speech detector(prob, denoised level) | PTT (D9, D18)          │
   → AudioData (48 kHz) → AudioEncoder (Opus 10 ms) → VoiceSession ─────┘
                                     └─ Hear yourself (D16): own packets
                                        → VoicePlayback → voice bus
```

With **Browser** or **Off**, or after a fallback, capture is today's: the
MSTP pump on Chromium, the worklet without denoising elsewhere, and the
level gate.

Files that change: `voiceCapture.ts` (mode, the module hand-off, the
re-open), `voiceFraming.ts` (the processor source: resampler, RNNoise
framing, watchdog), a new `rnnoise.ts` (fetch, compile, SIMD check),
`talkGate.ts` (the speech detector), `voicePrefs.ts`, `voiceController.ts`
(passing the gate the probability, the restart on mode change),
`voicePlayback.ts` (the self speaker), `VoicePanel.tsx` (the Audio
section), `deploy/nginx.conf`, and `gawk-app/rnnoise/`.

---

## 5. Security and privacy

- No audio leaves the browser that did not before: the denoised PCM goes
  to the encoder, the probability to the gate and the meter. The worklet
  posts only to its own port. docs/80 D24 holds: nothing is written,
  logged or recorded; Hear yourself plays from memory and keeps nothing.
- The wasm is same-origin and content-hashed. It is built from pinned
  hashes in a pinned image, and CI proves the committed bytes are what
  those sources produce (D2); a hand-edited binary fails the build.
- The module imports nothing, so it can reach nothing but its own memory.
- The app has no Content-Security-Policy today; if one is added, it needs
  `'wasm-unsafe-eval'` in `script-src`.

---

## 6. Risks

- **Phones.** NS0 ran on one desktop CPU. A low-end Android phone may be
  several times slower; D14 turns that into a fallback rather than
  glitches, and NS5 measures it.
- **Safari's double processing** (D12) is unmeasured on Safari itself.
- **Read speech is kind to a gate.** The corpus is three Spoken
  Wikipedia readings: clean pauses, steady level, English. Conversation,
  laughter, quiet speakers and other languages may need a lower default.
  The slider exists for that, and NS5's real-microphone pass checks the
  default.
- **Babble passes.** A TV, a stream on speakers or someone else talking
  near the microphone is real speech; 0.2 calls it speech (the gate sends
  ~96 % of a restaurant with nobody at the mic talking) and takes only
  7.9 dB off it. `main`'s model would take 28.9 dB and gate it out, at the
  cost of clipping speech (D1). Push-to-talk is the answer there, as in
  Mumble. If NS5 finds babble is the common case, revisit D1 with
  `main` and a level-based gate; the harness has every number for it.
- **Few onsets.** The onset-clipping figure rests on 32 distinct speech
  onsets in read speech, and the §1 criterion holds only as a mean: on
  keyboard the default gate clips 5.2 %, which is 3 onsets. NS5 checks it
  per noise on conversation.

---

## 7. Out of scope

- The broadcaster's audio lane (game and system audio): RNNoise would
  eat the game. Voice only.
- Echo cancellation beyond the browser's (docs/80 OD17 stands).
- The native broadcasters and the iOS app: none of them is a voice client
  yet (docs/80 OD11).
- Training or tuning the model.
- Any server, bridge or wire change.

### Rejected

- **An npm package** (`@jitsi/rnnoise-wasm`, `@shiguredo/rnnoise-wasm`,
  …): OD1. Building it ourselves also decides the version, the model, the
  SIMD flags and the module's shape (D3), which a package decides for us.
- **`nnnoiseless`** (the Rust port) through wasm-bindgen: a second
  toolchain in the app build for a port of the code we can build directly.
- **Main-thread denoising** on the MSTP path: puts ~0.3 ms per 10 ms
  on the thread React renders on, for no latency gain (D5).
- **A dedicated 48 kHz AudioContext**: macOS ignores the requested rate,
  and docs/44 keeps one context per room (D6).
- **A scalar fallback build**: a second binary, ~2.6× slower (measured on
  `main`; 0.2's scalar path does not compile), for browsers without SIMD
  that gawk does not support anyway (D3).
- **`main`'s default or little model** and **Mumble's pinned commit**:
  measured against 0.2 in §12.3. `main`'s models clip more onsets than 0.2
  at every gate setting swept (8–11 % against 1.3 % at the default), and
  the default model sends 8.3 % of keyboard at −26 dBFS; Mumble's gates
  like 0.2 at twice the CPU.
- **Keeping the browser's suppression on under RNNoise** (except Safari,
  D12): measured worse (§12.4).

---

## 8. Open questions

- Whether **hold** and **floor** need sliders too. Mumble exposes two
  thresholds; this design exposes one and fixes the rest until NS5 says
  otherwise.
- Whether Safari's default should be **Browser** (D12, NS5).

---

## 9. Chunks

Every letter is claimed; this milestone's prefix is **NS**.

| Chunk | What | Acceptance |
|---|---|---|
| **NS0** | **Measurement spike** (done 2026-10-27): builds, parity, delay, CPU, quality, gates, browser probes, Chromium's suppression against RNNoise. | `docs/assets/81/RESULTS.md` with raw data and a harness that reproduces it; §12 filled from it. |
| **NS1** | `gawk-app/rnnoise/`: `build.sh`, pins, `COPYING`, the committed `rnnoise.wasm`; the CI job (`emscripten/emsdk:4.0.10` container on the self-hosted runners, triggered by `gawk-app/rnnoise/**`); D19's notices entry; nginx `gzip_types`. | CI rebuild is byte-identical; a one-byte edit to the wasm fails the job; `tools/licenses` check passes and the notices list RNNoise; `curl` of the asset from the built image is served `application/wasm`, gzipped. |
| **NS2** | Denoiser: `rnnoise.ts` (SIMD check, fetch, compile), the worklet's denoise mode (D5–D7, D14), `VoiceCapture` choosing the worklet path while RNNoise is on, the D11 constraints and the in-session re-open, D13's fallbacks. | Vitest: resampler and framing (44.1 and 48 kHz in, 480-sample 48 kHz blocks out, timestamps on the 48 kHz clock, no sample lost or repeated across quanta); the scale; the processor source evaluated in a fake worklet scope with the real wasm (Node runs it) denoising a test signal; each fallback (no SIMD, fetch error, compile error, worklet error, watchdog) ends in Browser behaviour with the reason; listen-only fetches nothing; a re-open keeps the voice session and its seq clock. |
| **NS3** | Gate: the speech detector in `talkGate.ts` (D9), the level threshold (D10), D17's prefs, `voiceController.ts` passing probability and level. | Vitest: hysteresis, floor and hold, frame by frame; PTT unaffected (D18); prefs validation; the §1 rows for noise only and for speech, computed over NS0's corpus rule on a short committed synthetic fixture (no third-party audio in the repo). |
| **NS4** | UI: the Audio section (D15), the meter, Hear yourself (D16), reset, phone layout. | Component tests for every control and the fallback message; Playwright on Chromium with a fake-mic WAV (keyboard, then speech): no packets during the keyboard, packets during speech, the switch to Browser and back mid-call; Firefox and WebKit smoke (join, RNNoise on, packets flow); screens at 360/390/430 px and desktop. |
| **NS5** | Docs and the owner pass: docs/80 D21's pointer here, any new gotchas, the README's voice paragraph if it has one; on the reference deployment, Chrome, Firefox and Safari on desktop and Chrome on Android and Safari on iOS, each with a real microphone and a keyboard: RNNoise vs Browser by ear with Hear yourself, the watchdog's measured cost, the default threshold. | The §1 table ticked on all five platforms, the phone CPU numbers and D12's verdict written into §12. |

---

## 10. Implementation notes

- **The probability's frame.** With 0.2, the probability a call returns
  lines up with the labels one frame later than the audio it returns
  (§12.2). Gate each output block with its own call's probability: the
  gate opens 10 ms before the speech it is for reaches the encoder, which
  is the safe side. `main`'s models line up with the output exactly; a
  model change must re-measure this.
- **The first 20 ms** out of a fresh RNNoise state are its lookahead
  (zeros). The worklet drops nothing; the gate is closed then anyway.
- **One state per capture.** A re-open (D11) or a fallback destroys the
  RNNoise state; a new capture starts a new one. The model weights live in
  the module's data segment, so a new instance costs 2–4 ms (NS0), not a
  download.
- **Seq clock.** Blocks are 10 ms at 48 kHz whatever the context rate, so
  the `VoiceFrame` seq arithmetic (10 ms units) does not change.

---

## 11. Gotchas found by NS0

Also in `docs/gotchas.md`:

- The RNNoise 0.2 release tarball does not compile its scalar or NEON path
  (`os_support.h` is missing; xiph/rnnoise#222, fixed on `main` by
  `372f7b4`, never released). x86 builds pass because they take the SSE
  path, which hides it until someone builds for wasm without SIMD or for
  ARM.
- RNNoise takes floats at **int16 scale** (±32768), not ±1. At ±1 it sees
  a near-silent signal and says so.
- RNNoise 0.2 and later delay their output by **20 ms** (two frames), not
  the 10 ms its frame size suggests.
- `main`'s model source is 78 MB of C because it carries float copies of
  the int8 weights; `-DDISABLE_DEBUG_FLOAT` drops them.
- Chromium 156 accepts `applyConstraints({noiseSuppression})` on an audio
  track and changes nothing; re-open the microphone to switch.
- WebKit has no `noiseSuppression` or `autoGainControl` constraint at all.

---

## 12. Results (2026-10-27)

Raw data, harness and generated tables:
[`assets/81/RESULTS.md`](assets/81/RESULTS.md). Headless Chromium 156,
Firefox 157 and Playwright's WebKit 27.2 (**not Safari**) on one x86-64
host; offline runs in Node 24 (V8 13.6) on the same host.

### 12.1 Builds and CPU

All nine builds compile to a module with **no imports** and start with one
synchronous `new WebAssembly.Instance`. Two full builds on the NS0 host
were byte-identical for all nine (`results/wasm-sha256.txt`). Against a native gcc build of the same (`main`) sources,
the wasm output agrees to 62–63 dB SNR and the probability to within 0.02.

| ms per 10 ms frame, main thread | Chromium 156 | Firefox 157 | WebKit 27.2 | wasm |
|---|---|---|---|---|
| **0.2, `-mavx`** (D1, D3) | **0.31** | **0.17** | **0.33** | 1.47 MB (1.21 MB gzip) |
| `main` default model, `-mavx` | 0.56 | 0.30 | 0.68 | 3.6 MB |
| `main` little model, `-mavx` | 0.28 | 0.18 | 0.35 | 1.6 MB |
| `main`, scalar | 1.49 | 1.56 | 2.04 | 3.6 MB |

Mumble's pin costs the same as `main` (0.62 ms in Node; same code).
`-mavx2` is within 10 % of `-mavx` everywhere: a little faster in Chromium, slower in Firefox and WebKit. Inside the AudioWorklet
0.2 costs about the same (0.32–0.46 ms on Chromium and WebKit by the
worklet's 1 ms clock), and the worklet keeps real time at 44.1 and
48 kHz on all three engines.

### 12.2 Delay

Every variant delays its output by exactly **960 samples (20 ms)**. The
probability lines up with the labels one frame after the input on 0.2 and
Mumble's pin, two frames (with the audio) on `main`'s models.

### 12.3 Quality and the gate

Speech at −26 dBFS active level, keyboard, restaurant, street and white
noise at 20/10/5/0 dB SNR; noise alone for a minute at −36 and −26 dBFS.

| Default gate (on 0.90, off 0.70, hold 300 ms, floor −50 dBFS) | Speech sent · onsets clipped, mean of the four noises (10, 5 dB) | Keyboard alone, −36 / −26 dBFS | Restaurant alone, −26 | Babble attenuation |
|---|---|---|---|---|
| **0.2** | **99.5 % · 1.3 %** | **0.5 % / 1.6 %** | 95.9 % | 7.9 dB |
| Mumble's pin | 99.6 % · 2.1 % | 0.5 % / 2.5 % | 95.9 % | 9.3 dB |
| `main` default | 99.0 % · 8.1 % | 0.5 % / 8.3 % | 6.5 % | 28.9 dB |
| `main` little | 98.7 % · 11.3 % | 0.0 % / 0.0 % | 5.3 % | 28.9 dB |

For comparison on 0.2: **today's gate on the unprocessed signal sends
96 % of the keyboard minute**, and the same gate after RNNoise sends 0–1.5 %
of it but clips a mean of 3.7 % of onsets (5.2 % on keyboard, 8.2 % on
restaurant) (p90 32 ms against the floor gate's
23 ms). The probability alone (no floor) sends 27–29 % of the pauses in
keyboard and street mixes; the floor takes that to 0 %. Per noise, the
default gate clips 5.2 % of keyboard onsets (3 of 64, one or two per
reading) and none on the other three; every gate with on ≥ 0.6 clips the
same keyboard onsets.

### 12.4 Chromium's suppression against RNNoise

The same fake microphone (keyboard, then speech over keyboard at 10 dB),
echo cancellation on:

| | Keyboard removed | Level gate on the keyboard alone |
|---|---|---|
| Chromium, suppression off | 1.2 dB | 55 % sent |
| **Chromium, suppression on** (today) | **2.5 dB** | 53 % sent |
| Suppression off + RNNoise (D11) | 15.5 dB | 7.8 % sent |
| Suppression on + RNNoise | 11.7 dB | 21.5 % sent |

A waveform score is not given: Chromium's processing filters the capture
even with suppression off, so SI-SDR against the clean speech is ~0 dB in
every condition and measures the filter.

### 12.5 44.1 kHz

0.2 on a mix resampled 48 → 44.1 (ffmpeg) → 48 kHz (linear, D6): noise
attenuation and SI-SDR within 1 dB and 0.05 dB of the direct run, the
probability decision agreeing on ≥ 99.5 % of frames. Fed the 44.1 kHz
samples as if they were 48 kHz: 61–63 % agreement.

### 12.6 Browser behaviour

- wasm SIMD on all three engines.
- A compiled `WebAssembly.Module` reaches an AudioWorklet through
  `processorOptions` and through `port.postMessage` on all three.
- A 48 kHz and a 44.1 kHz AudioContext both take
  `createMediaStreamSource` of the microphone on all three (Firefox 157
  included).
- Chromium: `applyConstraints({noiseSuppression: true})` on a track
  opened with it off is accepted and changes nothing.
- WebKit: no `noiseSuppression` or `autoGainControl` constraint.
  Firefox's fake device reports every processing flag false, so its
  constraint rows say nothing about a real microphone.
- Chromium exposes a `voiceIsolation` constraint; not tried here.
