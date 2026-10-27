# R80 — RNNoise noise suppression for room voice

**Status**: designed 2026-10-27 after the NS0 measurement spike (§12, raw
data and harness in [`assets/81/`](assets/81/RESULTS.md)). Owner decisions
2026-10-27 (§2). **NS1–NS4 implemented 2026-10-27** (§12.7: NS4 found the
44.1 kHz path sending more keyboard and replaced D6's linear resampler).
Open: NS5, the owner-device rows (Safari on macOS and iOS, Chrome on
Android, a real microphone).

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
stands between it and everyone else's headphones. NS0 measured what that
leaves on Chromium with today's constraints (§12.4): while nobody is
talking, a keyboard comes out **11–13 dB louder** than it went in, because
auto gain lifts it (over speech it adds ~3 dB) and suppression does not
reduce it at all, and today's level gate sends 55 % of it. (On the unprocessed signal, before any of that, the same
gate is open for 96 % of a minute of typing.) Mumble users have
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
| The wasm is **built from pinned source** in a container pinned by digest; CI rebuilds it and fails on any byte difference from the committed file | NS1 CI job (a deliberate one-byte change fails it) |
| With nobody talking, a minute of keyboard noise sends **≤ 1 % of frames at −36 dBFS and ≤ 2 % at −26 dBFS** with the default gate on the signal as recorded (NS0: 0.5 % and 1.6 %), and **≤ 3 % through Chromium's processing** with D11's constraints, the file at −36 dBFS (NS0: 2.2 %; the browser's processing is what adds the difference, §12.4) | NS3 test over the NS0 rules; NS4 Playwright with a fake-mic file (Chromium), against the ≤ 3 % row |
| Speech is not cut: ≥ 99 % of labelled speech frames sent and ≤ 2 % of onsets clipped by ≥ 50 ms, **pooled over the four noises** (clipped onsets ÷ onsets, every reading at 10 and 5 dB SNR), default gate (NS0: 99.5 % and 1.2 %, 3 of 256; per noise, keyboard 4.7 % — 3 of 64 — and 0 % for the other three, over only 32 distinct onsets) | NS3 test; NS5's real-microphone pass, which has to confirm the onset figure on conversation |
| The gate is driven by RNNoise's **speech probability**, with a user-set threshold whose default comes from §12 | NS3 unit tests; NS4 UI test |
| The user can switch **RNNoise · Browser · Off** and the threshold mid-call, and **hear themselves** as the others hear them, without leaving voice | NS4 component and Playwright tests |
| RNNoise never blocks voice: no SIMD, a failed download, a worklet error or a CPU overrun falls back to **Browser**, or to **Off** where the switch would need a prompt (D13; Off is noisier than today's Browser), and says which | NS2 unit tests for each fallback |
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
| D1 | **The RNNoise 0.2 release** (`rnnoise-0.2.tar.gz`, sha256 `90fce4b0…`, its bundled model `0b50c45`), pinned by hash. Not `main`'s newer models, not the commit Mumble pins. | §12.3, four models through the same gates. With the default gate, 0.2 sends 99.5 % of speech, clips 1.2 % of onsets pooled over the four noises (4.7 % on keyboard, 0 % on the rest) and sends 0.5–1.6 % of a keyboard with nobody talking; `main`'s default model clips 7.4 % and sends 8.3 % of the loud keyboard, its little model clips 10.9 % and sends none. Both of `main`'s models clip more onsets than 0.2 at every one of the 60 gate settings swept (pooled at 10 and 5 dB: `main` 2.3–10.2 %, little 3.9–15.2 %, 0.2 0.4–3.5 %). Part of that gap may be timing rather than the model: 0.2's probability runs a frame ahead of its audio and `main`'s does not (§12.2). That is how each behaves in production, so the comparison is fair, but NS0 did not separate the two. Mumble's pin (`d983458`, model `5e78411`) gates like 0.2 (2.0 %, 2.5 %) at twice the CPU. Its SI-SDR is within 0.6 dB of the best model in every mix (best in 8 of 16, Mumble's pin in the other 8), and it is, with `main`'s little model, the cheapest: ~0.29 ms per 10 ms frame in Chromium, 0.17 in Firefox, 0.34 in WebKit (§12.1; on Chromium the two are within run-to-run noise, each ahead in one of NS0's runs), 1.47 MB. What 0.2 does **not** do is remove babble (§6): 7.9 dB off a restaurant, where `main` takes 28.9 dB. That is the trade, and the gate is what this milestone is for. |
| D2 | **The build lives in `gawk-app/rnnoise/`**: `build.sh` (the NS0 `build-wasm.sh` cut down to one variant), the pins, Xiph's `COPYING`, and the built **`rnnoise.wasm`, committed**. A CI job (`rnnoise` in `ci.yml`) rebuilds it in `emscripten/emsdk:4.0.10`, pinned by digest (`@sha256:90b757eb…`), and fails on any byte difference. `build.sh` streams the sources into the container and the wasm out over stdin and stdout, not bind mounts, because a runner's docker daemon need not share its filesystem. | OD1. The icon set's shape (docs/53): the derivative is committed and drift-checked, so `npm run build`, the app's Dockerfile, the dev stack and vitest need no Emscripten. NS0's two full builds on one host were byte-identical for all nine variants (`assets/81/results/wasm-sha256.txt`); NS1 confirms it holds on the CI runner. The digest pin matters because a byte-compare gate is exactly where a re-pushed tag would surface, as an unexplained failure. |
| D3 | **One binary: `-O3 -flto -msimd128 -mavx`, `-sSTANDALONE_WASM --no-entry`, fixed 4 MB memory, no JS glue.** Exports `rnnoise_create`, `rnnoise_process_frame`, `rnnoise_destroy`, `malloc`, `free`, `memory`, `_initialize`; **imports nothing**. No scalar fallback build (0.2's scalar path does not compile anyway, §11). | Emscripten lowers `vec_avx.h`'s SSE/AVX intrinsics to 128-bit wasm SIMD; `-mavx2` buys nothing: within 10 % of `-mavx` on every engine, and slower in all but one cell (§12.1). Every engine gawk supports has wasm SIMD (all three in NS0; Safari since 16.4). A module with no imports instantiates synchronously inside an AudioWorklet with three lines of loader. 1.47 MB uncompressed, 1.21 MB gzipped (`results/wasm-sizes.txt`). |
| D4 | **Loaded on demand.** Vite emits `rnnoise.wasm` as a hashed asset (`?url`), served from `/assets/` with the existing immutable cache header; nginx's `gzip_types` gains `application/wasm` (its `mime.types` already has it). `WebAssembly.compileStreaming` starts when the user presses **Join voice**, in parallel with the microphone prompt. Listen-only never fetches it. | The download is paid once per release, by voice users only. Starting it with the permission prompt hides it behind a dialog that takes seconds anyway. |
| D5 | **Voice capture runs in the AudioWorklet in every mode, on every browser, and RNNoise runs there while it is on.** The existing `gawk-voice-capture` processor grows a denoise mode. The main thread compiles the module and passes the `WebAssembly.Module` in `processorOptions`; the worklet instantiates it in its constructor. The module usually lands after the node exists (D13 opens the microphone in parallel), so the capture then swaps in a new node carrying it, between the same source and sink; the encoder and the block clock carry on. **Never through the port**: Chromium ≤ 154 delivers a Module posted to a worklet port as a `messageerror` (151 and 154 measured; 156 delivers it), which left RNNoise "loading" for good in CI (§12.7); the worklet reports any `messageerror` as an error, so D13 falls back instead. Chromium leaves its MSTP pump (docs/80 D21) for voice altogether: one path, so a mode switch never changes the block rate under the encoder, which `VoiceCapture` configures once from the first block. | The audio thread is the one place that never janks with React. NS0: a compiled module reaches the worklet through `processorOptions` and the port on Chromium, Firefox and WebKit, and the worklet keeps real time (its render clock advances 0.9996–1.0004 s per second on every engine, §12.1). MV0 measured Chromium's worklet and MSTP paths at the same transit (23.3 ms), so leaving MSTP costs nothing. |
| D6 | **The worklet resamples to 48 kHz.** When the context does not run at 48 kHz, a polyphase windowed-sinc resampler (Blackman, 64 taps at or below 48 kHz, its history and phase carried across render quanta; 0.75 ms of delay from 44.1 kHz) turns its input into 48 kHz, in every mode, before RNNoise when it is on, and the 10 ms blocks it posts are 48 kHz. A rate whose ratio to 48 kHz would need more than 2048 phases falls back to linear interpolation. The encoder is configured from the first block, as today, so it now always runs at 48 kHz and no switch or fallback changes its rate. | RNNoise only works at 48 kHz, and an AudioContext need not run at the rate asked (docs/gotchas.md, macOS; headless Chromium runs at 44.1 kHz). **Designed as linear interpolation; NS4 replaced it** (§12.7): linear rolls the highs off and sent a −26 dBFS keyboard minute 2.08 % against 1.57 % at 48 kHz, and 3.5–4.1 % against 2.3 % through Chromium; the sinc resampler gives 1.63 %. NS0's check (§12.5) compared SI-SDR and speech decisions at probability 0.6 and found linear harmless; the default gate on keyboard is more sensitive than either. Feeding 44.1 kHz samples as if they were 48 kHz still mostly agrees on speech or not (96.8–98.2 % of frames, aligned in time), but moves the noise removed by up to 9 dB either way, and its blocks would reach the encoder at the wrong rate. A second, 48 kHz context would break docs/44's one context per room and still be refused on macOS. |
| D7 | **Framing and scale.** 480 samples (10 ms) per call, which is also the voice block (`VOICE_BLOCK_MS`), so a denoised block maps 1:1 onto an Opus frame. Samples are multiplied by 32768 in and divided out: RNNoise works on int16-scaled floats (Mumble's `AudioInput.cpp` feeds it shorts). The worklet posts `{pcm, prob}` per block, the PCM buffer transferred. | One block in, one block out: no added buffering beyond RNNoise's own delay. The scale is a silent failure if wrong (the model sees near-silence). |
| D8 | **RNNoise adds 20 ms**, and that is all it adds. NS0 measured 960 samples of output delay on every variant (RNNoise 0.2 added a frame of lookahead, xiph `84fe83b`). The probability returned with a call runs one frame (10 ms) ahead of the audio returned with it (§12.2). A gate on the probability alone, fed each output block with its own call's probability, therefore opens 10 ms early, never late; the default gate's floor (D9) reads the denoised block, which is the delayed audio, so it opens with the audio. | Owned, not hidden: the settings say it, and **Browser** remains a one-click lowest-latency choice, as the R73 lesson asks. |
| D9 | **The gate: a speech detector over RNNoise's probability** (OD4), Mumble's shape plus a level floor. A block counts as speech when its probability is ≥ **on**, or ≥ **off** while the gate is already open, **and** the denoised block is louder than the **floor**; the gate stays open **hold** after the last speech block. **Defaults: on 0.90, off 0.70, hold 300 ms, floor −50 dBFS** (§12.3). The user's slider sets **on** (0.50–0.99); **off** follows at on − 0.2; hold and floor are fixed. | §12.3: without the floor, 0.2's probability stayed open through a pause in one reading (`speech-a`: 88 % of its pause frames sent with keyboard, 80 % with street, at both SNRs; the other two readings sent none) and, with nobody talking, sends 2.7 % of the loud keyboard. The floor, on a signal RNNoise has already silenced, takes the pause to 0 % and the keyboard to 1.6 %, and costs nothing in speech. Across on = 0.6–0.95 the floor gate sends ≥ 99.4 % of speech; 0.9 is the highest on that keeps onset clipping at the 0.6–0.8 level (1.2 % pooled; on keyboard every floor gate with on 0.5–0.9 and a floor of −55 dBFS or higher clips the same three onsets, 4.7 %; 0.95 clips 2.0 %, 6.3 % on keyboard) while halving the keyboard sent at −26 dBFS against 0.6–0.8 (1.6 % vs 2.6–3.1 %). Mumble's gate has the same two thresholds and a hold (`fVADmax` 0.98, `fVADmin` 0.80, `iVoiceHold` 200 ms) over Speex's probability. One slider is what people tune; two are what they get wrong. |
| D10 | **The level gate stays** for **Browser** and **Off**, and becomes configurable too (OD5): its slider is in dBFS, default the current −40 dBFS (`VAD_THRESHOLD_RMS`). | Without RNNoise there is no probability; a fixed −40 dBFS cannot suit every microphone. |
| D11 | **Browser suppression is off while RNNoise is on**: the microphone opens with `noiseSuppression: false`, echo cancellation and auto gain on. **Browser** opens it with all three on (today's constraints); **Off** with suppression off and no RNNoise. RNNoise ↔ Off has the same constraints and is the worklet's alone. A switch to or from **Browser** **re-opens the microphone** inside the running voice session (a capture restart, not a rejoin): **stop the old track, then `getUserMedia`**. It re-opens only when `navigator.permissions.query({name: 'microphone'})` says `granted` (a throw, or any other answer, counts as not granted, as `features/pop/permission.ts` treats its own query), so it never prompts; otherwise the choice is saved, applies at the next join, and the Audio section says so. A re-open that still fails ends the capture as a lost microphone does today. **On WebKit no switch re-opens**: it has no suppression constraint to change (D12). | NS0 (§12.4, auto gain on as here): suppression before RNNoise left 16 dB more keyboard than RNNoise alone (−32.8 against −48.4 dBFS), and RNNoise then called 11.3 % of the keyboard frames speech (probability ≥ 0.9) against 1.5 %. Chromium 156 accepts `applyConstraints({noiseSuppression})` and changes neither `getSettings()` nor the audio (§12.6), so a re-open is the only switch. The old track goes **first** because Chromium gives a second track on an open microphone the first one's processing: asked for suppression on while a suppression-off track was open, it came back off, its audio identical to the first's. Stopped first, the re-open (14.5 ms) gets the new processing, settings and audio both (§12.6). "Open the new one, keep the old one if refused" cannot work there, and a prompt mid-call, on Firefox without *Remember this decision* or Safari set to *Ask*, would risk the microphone for a denoiser choice. **Auto gain stays on**: over a minute of keyboard through Chromium the default gate sends 2.2 % (4.1 spurts a minute) with it on and 3.1 % (6.2) with it off (§12.4), and a quiet microphone needs the gain to reach the encoder at a usable level. With it off RNNoise does leave the keyboard ~5 dB lower; NS5 compares the two by ear (§8). |
| D12 | **Safari can't turn its suppression off.** WebKit lists no `noiseSuppression` or `autoGainControl` constraint; its voice processing comes with echo cancellation. RNNoise runs after it there. NS5 checks on the owner's Mac and iPhone that the two together are not worse than either alone; if they are, Safari's default becomes **Browser**. | Measured on WPE WebKit, not Safari: the owner-device pass decides. |
| D13 | **Fallbacks, never a failure.** The SIMD check is synchronous and runs before the first `getUserMedia`, so without SIMD the microphone opens as **Browser**. Anything after the microphone is open (a failed fetch or compile, an instantiate or worklet error, the CPU watchdog, D14) moves the capture to **Browser** by D11's re-open when the permission is `granted`, and otherwise to **Off** (no RNNoise, no browser suppression, the level gate) on the open track, for the rest of the voice session. Off is noisier than today's Browser (its level gate sent 62.5 % of a keyboard lead against 55.3 %, §12.4): a fallback, not today's behaviour. The Audio section shows *RNNoise isn't available here: {reason}* and the mode it ended in. **While the module is still loading** (D4 opens the microphone in parallel), the capture runs as Off and the meter says RNNoise is loading; it switches to RNNoise in the worklet when the module arrives, with no re-open. The user's saved choice stays **RNNoise**, so the next session tries again. | RNNoise is an improvement, not a dependency: voice that fails to join because a denoiser didn't load would be a regression. |
| D14 | **A CPU watchdog in the worklet.** It sums `Date.now()` deltas around each RNNoise call (the only clock every worklet scope has; with millisecond ticks the sum is unbiased over many calls) and compares them with the audio processed. Above **40 % of real time over 2 s** it bypasses RNNoise and posts `slow`, which D13 turns into the fallback. | The worklet shares the room's context with every stream's and every speaker's playback; a denoiser that can't keep up would glitch all of it. 40 % is ~6× the worst cost NS0 measured for 0.2 in a worklet (0.69 ms per 10 ms frame, 6.9 %, one WebKit run at 48 kHz; its other WebKit runs 0.35–0.38 ms, ~10–11×), room for a slow phone, not for a broken one. NS5 records the real numbers on phones. |
| D15 | **The Audio section** (OD3) in the voice panel, collapsed by default: **Noise suppression** RNNoise · Browser · Off; the detector's **threshold slider** with a **live meter** (the current probability or level, the threshold mark, and a lamp when the gate is open); **Hear yourself**; **Reset to defaults**; and one line saying what the chosen mode costs (*RNNoise adds 20 ms*). Every control applies live, except a switch D11 defers: then the selector shows the saved choice with *applies when you next join* beside the running mode. Where the browser has no `noiseSuppression` constraint (`getSupportedConstraints()`, not the user agent: WebKit), Browser and Off are the same mode, so **Off** is not offered. On a phone the section is the same, full width. | A/B means switching while talking and hearing the result. The meter answers "why didn't it send?" without guesswork. |
| D16 | **Hear yourself plays your own packets back** through a local speaker in `voicePlayback.ts`: the same Opus decoder, jitter buffer and voice bus the others' audio uses, after the gate. Off by default and never saved; the toggle warns to use headphones. | You hear what the others hear: the denoising, the codec and the gate's cuts. A raw monitor of the microphone would hide the gate. |
| D17 | **Preferences** (per browser, `voicePrefs.ts`): `gawk:voice-noise` `rnnoise` \| `browser` \| `off` (default `rnnoise`); `gawk:voice-speech-threshold` 0.50..0.99 (default D9's **on**); `gawk:voice-level-threshold` dBFS, −70..−10 (default −40). Values out of range are clamped; **Reset to defaults** removes all three, so a later change of a default reaches the browser. | OD2, OD5. Same storage helpers and validation as the existing voice prefs. |
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

With **Browser** or **Off**, or after a fallback, the same worklet runs
without RNNoise: resampled to 48 kHz, then the level gate. The MSTP pump
that docs/80 D21 uses on Chromium no longer carries voice (D5).
(docs/80 D21 also names Safari's worker MSTP; `voiceCapture.ts` runs MSTP
on the main thread only, so Safari already takes the worklet.)

Files that change: `voiceCapture.ts` (mode, the module hand-off, the
re-open, the fallbacks), `voiceFraming.ts` (the processor source:
resampler, RNNoise framing, watchdog), a new `rnnoise.ts` (fetch, compile,
SIMD check), `talkGate.ts` (the speech detector), `voicePrefs.ts`,
`voiceController.ts` (passing the gate the probability, the noise status,
the meter, Hear yourself), `voicePlayback.ts` (the self speaker), a new
`VoiceAudio.tsx` (the Audio section, mounted in `VoicePanel.tsx`),
`deploy/nginx.conf`, `gawk-app/rnnoise/`, the `rnnoise` CI job and
`tools/licenses/gen-notices.py`. Tests run the processor source with the
real wasm in Node (`voiceTestKit.ts`, which also generates the synthetic
speech and noise); the browser check is `e2e/voice-rnnoise.mjs`.

---

## 5. Security and privacy

- No audio leaves the browser that did not before: the denoised PCM goes
  to the encoder, the probability to the gate and the meter. The worklet
  posts only to its own port. docs/80 D24 holds: nothing is written,
  logged or recorded; Hear yourself plays from memory and keeps nothing.
- The wasm is same-origin and content-hashed. It is built from pinned
  hashes in an image pinned by digest, and CI proves the committed bytes are what
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
  onsets in read speech, and the §1 criterion holds only pooled over the
  noises: on keyboard the default gate clips 4.7 %, 3 of 64 (two in one
  reading, once at each SNR, and one in another). NS5 checks it per noise
  on conversation.
- **A mode switch could prompt.** D11's re-open is a second
  `getUserMedia`, so it runs only when the permission is already granted.
  The cost: a user who did not remember the permission switches to or
  from Browser at the next join, and a fallback lands on Off rather than
  Browser (D13). NS2 tests both branches with a stubbed permission state;
  NS5 checks Firefox's and Safari's real ones.
- **Two tracks share processing.** On Chromium a second track on an open
  microphone gets the first one's processing (§12.6), so the re-open stops
  the old track first, which NS0 measured working on Chromium. Firefox and
  Safari are unmeasured on both counts (their fake devices have no
  processing); NS4 checks the new track's audio after a switch.
- **Chromium's processing adds keyboard spurts.** The same minute of
  keyboard sends 0.5 % with the default gate as recorded and 2.2 % through
  Chromium with D11's constraints (§12.4), hence §1's separate ≤ 3 % row.
  NS5's real microphones decide whether the default on needs to move.

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
  at every gate setting swept (7–11 % against 1.2 % at the default), and
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
- Whether auto gain should be off under RNNoise (D11, §12.4): off, the
  keyboard ends ~2 dB further below the speech, but the default gate sent
  more of a keyboard minute through Chromium (3.1 % against 2.2 %). NS0's
  data favours on; NS5 listens to both.
- Whether real Safari starts the room's AudioContext when it is first
  created after the microphone prompt (a room with no stream playing has
  none until the capture asks for it). Playwright's WebKit and Firefox both
  ran it (§12.7); NS5 checks Safari on macOS and iOS before anything is
  changed for it.

---

## 9. Chunks

Every letter is claimed; this milestone's prefix is **NS**.

| Chunk | What | Acceptance |
|---|---|---|
| **NS0** | **Measurement spike** (done 2026-10-27): builds, parity, delay, CPU, quality, gates, browser probes, Chromium's suppression against RNNoise. | `docs/assets/81/RESULTS.md` with raw data and a harness that reproduces it; §12 filled from it. |
| **NS1** ✅ | `gawk-app/rnnoise/`: `build.sh`, pins, `COPYING`, the committed `rnnoise.wasm`; the CI job (`emscripten/emsdk:4.0.10` container, pinned by digest, on the self-hosted runners, triggered by `gawk-app/rnnoise/**`); D19's notices entry; nginx `gzip_types`. | CI rebuild is byte-identical; a one-byte edit to the wasm fails the job; `tools/licenses` check passes and the notices list RNNoise; `curl` of the asset from the built image is served `application/wasm`, gzipped. |
| **NS2** ✅ | Denoiser: `rnnoise.ts` (SIMD check, fetch, compile), the worklet's denoise mode (D5–D7, D14), `VoiceCapture` choosing the worklet path while RNNoise is on, the D11 constraints and the in-session re-open, D13's fallbacks. | Vitest: resampler and framing (44.1 and 48 kHz in, 480-sample 48 kHz blocks out, timestamps on the 48 kHz clock, no sample lost or repeated across quanta); the scale; the processor source evaluated in a fake worklet scope with the real wasm (Node runs it) denoising a test signal; no SIMD opens the microphone as Browser before any prompt; while the module loads the capture runs as Off, then RNNoise without a re-open; RNNoise → Off and the watchdog fallback on a 44.1 kHz fake context keep the encoder running on 48 kHz blocks; listen-only fetches nothing; a re-open keeps the voice session and its seq clock, stops the old track before `getUserMedia`, and runs only with the permission `granted` (otherwise the choice waits for the next join); RNNoise ↔ Off and every WebKit switch never call `getUserMedia`; each later fallback (fetch error, compile error, worklet error, watchdog) ends in Browser with the permission granted and in Off without it, each with the reason. |
| **NS3** ✅ | Gate: the speech detector in `talkGate.ts` (D9), the level threshold (D10), D17's prefs, `voiceController.ts` passing probability and level. | Vitest: hysteresis, floor and hold, frame by frame; PTT unaffected (D18); prefs validation; the §1 rows for noise only and for speech, computed over NS0's corpus rule (onsets pooled as in §1) on a short committed synthetic fixture (no third-party audio in the repo). |
| **NS4** ✅ | UI: the Audio section (D15), the meter, Hear yourself (D16), reset, phone layout. | Component tests for every control, the fallback message, the deferred *applies when you next join* state and no **Off** without a `noiseSuppression` constraint; Playwright on Chromium with a fake-mic WAV (keyboard, then speech): no packets during the keyboard, packets during speech, the switch to Browser and back mid-call, judged on the new track's audio, not `getSettings()` (§12.6's method); Firefox and WebKit smoke (join, RNNoise on, packets flow); screens at 360/390/430 px and desktop. |
| **NS5** | Docs and the owner pass: docs/80 D21's pointer here, any new gotchas, the README's voice paragraph if it has one; on the reference deployment, Chrome, Firefox and Safari on desktop and Chrome on Android and Safari on iOS, each with a real microphone and a keyboard: RNNoise vs Browser by ear with Hear yourself, the watchdog's measured cost, the default threshold, and on Firefox and Safari a switch to Browser with and without a remembered permission (D11), recording what `permissions.query` returns after a grant without *Remember*. | The §1 table ticked on all five platforms, the phone CPU numbers and D12's verdict written into §12. |

---

## 10. Implementation notes

- **The probability's frame.** With 0.2, the probability a call returns
  lines up with the labels one frame later than the audio it returns
  (§12.2). Gate each output block with its own call's probability: the
  probability then leads its audio by 10 ms, which is the safe side. With
  the default floor the gate opens with the audio, not before it, because
  the floor reads the delayed block. `main`'s models line up with the output exactly; a
  model change must re-measure this.
- **The first 20 ms** out of a fresh RNNoise state are its lookahead
  (zeros). The worklet drops nothing; the gate is closed then anyway.
- **One state per capture.** A re-open (D11) or a fallback destroys the
  RNNoise state; a new capture starts a new one. The model weights live in
  the module's data segment, so a new instance costs 2–4 ms (NS0), not a
  download.
- **Seq clock and encoder.** Blocks are 10 ms at 48 kHz whatever the
  context rate and the mode (D5, D6), so the `VoiceFrame` seq arithmetic
  (10 ms units) does not change, and the encoder, configured once from the
  first block (`this.encoder ??=`), never sees a rate change.
- **The re-open is not a `start()`.** `VoiceCapture.start()`'s catch calls
  `release()`, which would tear down the session's capture on a failed
  re-open; a failed re-open goes through `fail()`, so its error reaches
  `onError` as a lost microphone's does. The old track's `ended` listener
  (it fails the capture) comes off before the deliberate `stop()`.

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
  track and changes nothing, neither `getSettings()` nor the audio (§12.6);
  re-open the microphone to switch.
- WebKit has no `noiseSuppression` or `autoGainControl` constraint at all.
- Chromium gives a second track on an already open microphone the first
  track's processing, whatever it asked for (`getSettings()` and audio
  both): stop the old track before opening one with other constraints.
- Chromium's auto gain lifts a keyboard with nobody talking by 11–13 dB;
  over speech it adds ~3 dB.

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
| **0.2, `-mavx`** (D1, D3) | **0.29** | **0.17** | **0.34** | 1.47 MB (1.21 MB gzip) |
| `main` default model, `-mavx` | 0.62 | 0.29 | 0.66 | 3.6 MB |
| `main` little model, `-mavx` | 0.32 | 0.18 | 0.35 | 1.6 MB |
| `main`, scalar | 1.57 | 1.61 | 2.07 | 3.6 MB |

Browser figures move by up to ~10 % between runs: NS0's first run had
the little model ahead of 0.2 on Chromium (0.28 against 0.31 ms). Mumble's
pin costs the same as `main` (0.62 ms in Node; same code). `-mavx2` is
within 10 % of `-mavx` everywhere and faster in one cell only (the little
model on Chromium, by 3 %). Inside the AudioWorklet 0.2 costs about the
same by the worklet's 1 ms clock: 0.21–0.38 ms per run across the
engines, and one WebKit run at 48 kHz at 0.69 ms.

The worklet keeps real time. Over 6 s, after a second of start-up, its
render clock (`currentFrame`) advanced 0.9996–1.0004 s per second of the
worklet's own clock on all three engines: the six variant × hand-off runs
at 44.1 kHz and one at 48 kHz (`v02-avx` via the port; D5's
`processorOptions` hand-off did not run at 48 kHz). It denoised every
frame: 91.73–91.98 frames/s against 91.875, and 99.98–100.03 against 100.
At ~3 % load that shows no backlog, not headroom; and headless engines
render to a fake sink, so glitches on a real device are NS5's. NS0's first run instead divided frames by main-thread time
around a port round trip, which read WebKit up to 10 % above real time and
one Chromium run 19 % below it; that measured the round trip, not the
worklet.

### 12.2 Delay

Every variant delays its output by exactly **960 samples (20 ms)**. The
probability lines up with the labels one frame after the input on 0.2 and
Mumble's pin, two frames (with the audio) on `main`'s models.

### 12.3 Quality and the gate

Speech at −26 dBFS active level, keyboard, restaurant, street and white
noise at 20/10/5/0 dB SNR; noise alone for a minute at −36 and −26 dBFS.

| Default gate (on 0.90, off 0.70, hold 300 ms, floor −50 dBFS) | Speech sent · onsets clipped, pooled over the four noises (10, 5 dB) | Keyboard alone, −36 / −26 dBFS | Restaurant alone, −26 | Babble attenuation |
|---|---|---|---|---|
| **0.2** | **99.5 % · 1.2 %** | **0.5 % / 1.6 %** | 95.9 % | 7.9 dB |
| Mumble's pin | 99.6 % · 2.0 % | 0.5 % / 2.5 % | 95.9 % | 9.3 dB |
| `main` default | 99.0 % · 7.4 % | 0.5 % / 8.3 % | 6.5 % | 28.9 dB |
| `main` little | 98.7 % · 10.9 % | 0.0 % / 0.0 % | 5.3 % | 28.9 dB |

For comparison on 0.2: **today's gate on the unprocessed signal sends
96 % of the keyboard minute**, and the same gate after RNNoise sends 0–1.5 %
of it but clips 3.5 % of onsets (4.7 % on keyboard, 7.8 % on restaurant;
p90 32 ms against the floor gate's 23 ms). Without the floor the
probability stayed open through one pause in one reading (`speech-a`: 88 %
of its pause frames sent with keyboard, 80 % with street; the other two
readings none); the floor takes that to 0 %. Per noise, the default gate
clips 4.7 % of keyboard onsets (3 of 64: two in `speech-a`, once at each
SNR and probably the same onset, one in `speech-b`, none in `speech-c`)
and none on the other three; every floor gate with on 0.5–0.9 and a floor
of −55 dBFS or higher clips the same three.

### 12.4 Chromium's suppression against RNNoise

The same fake microphone (5 s of keyboard at −38.2 dBFS, then speech
over keyboard at 10 dB), echo cancellation on and, except in the last two
rows, auto gain on, as `voiceCapture.ts` opens it today. The lead is the
keyboard-only start, before anyone speaks:

| | Keyboard lead, dBFS | Against the input | Level gate on the lead | RNNoise probability ≥ 0.9 on the lead |
|---|---|---|---|---|
| Chromium, suppression off | −27.0 | 11.3 dB louder | 62.5 % sent | |
| **Chromium, suppression on** (today) | **−25.7** | **12.5 dB louder** | 55.3 % sent | |
| Suppression off + RNNoise (D11) | −48.4 | 10.2 dB quieter | 40.3 % sent | 1.5 % |
| Suppression on + RNNoise | −32.8 | 5.4 dB louder | 54.0 % sent | 11.3 % |
| Suppression and auto gain off | −39.5 | 1.2 dB quieter | 55.3 % sent | |
| Suppression and auto gain off + RNNoise | −53.7 | 15.5 dB quieter | 7.8 % sent | 1.3 % |

With nobody talking, auto gain lifts the keyboard 11–13 dB; over the
speech that follows it adds ~3 dB (+2.8 and +3.0 dB against the input).
NS0 did not measure the keyboard's level once speech has set the gain.
Chromium's suppression does not reduce the keyboard at all (it ends 1.3 dB
louder than with suppression off). RNNoise after suppression off ends
23 dB below today; after suppression on, 16 dB above RNNoise alone, and it
then calls 7× as many keyboard frames speech. Clicks still cross the level
gate after RNNoise; the probability is what keeps them out. With auto gain
off too, RNNoise leaves the keyboard 5 dB lower and the speech 3 dB lower
(−1.8 against +1.2 dB), ~2 dB more separation, for about the same
probability.

The default gate (D9) over the 4 s lead, where one spurt is ~8 %, sends
11.8 % (two spurts) with auto gain on, 8.3 % (one) with it off and 33 %
(three) with suppression on first. One of those spurts is in the file
itself: a keyboard sound RNNoise calls speech, which opens the gate on the
unprocessed input too. Four seconds cannot test a per-minute criterion, so
NS0 also played a minute of keyboard alone (−36 dBFS) through Chromium
with D11's constraints:

| A minute of keyboard, default gate after RNNoise | Into RNNoise | Sent | Spurts a minute |
|---|---|---|---|
| The file itself, no browser | −36.0 dBFS | 0.5 % | 1.0 |
| Chromium, suppression off, auto gain on (D11) | −26.6 dBFS | 2.2 % | 4.1 |
| Chromium, suppression off, auto gain off | −36.9 dBFS | 3.1 % | 6.2 |

Chromium's processing, not auto gain, is what adds the spurts: with auto
gain off the keyboard is at the file's level and the gate opens more often
still. §1's Chromium row is set from this (≤ 3 %).

A waveform score is not given: Chromium's processing filters the capture
even with suppression off, so SI-SDR against the clean speech is ~0 dB in
every condition and measures the filter.

### 12.5 44.1 kHz

0.2 on a mix resampled 48 → 44.1 (ffmpeg) → 48 kHz (linear, D6): noise
attenuation and SI-SDR within 1 dB and 0.05 dB of the direct run, the
probability decision at 0.6 (a model check, not the default gate)
agreeing on ≥ 99.4 % of frames. Fed the 44.1 kHz
samples as if they were 48 kHz, each frame compared with the direct run's
frame at the same time (`round(k·48000/44100)`): 96.8–98.2 % agreement,
but the noise removed moves by up to 9 dB (keyboard 42.0 against 32.9 dB,
street 30.4 against 38.9), and the output runs 8.8 % slow. This script
sets levels with its own rule (samples above 0.01, `speech-a` only), so
its 10 dB SNR is not §12.3's: compare its rows with each other, not with
that table.

### 12.6 Browser behaviour

- wasm SIMD on all three engines.
- A compiled `WebAssembly.Module` reaches an AudioWorklet through
  `processorOptions` and through `port.postMessage` on all three.
- A 48 kHz and a 44.1 kHz AudioContext both accept
  `createMediaStreamSource` of the microphone on all three (Firefox 157
  included). Audio was seen flowing (an analyser peak over 500 ms) on
  Firefox and WebKit at both rates. On Chromium the fake device's beep is
  intermittent and a 500 ms window caught it once in NS0's two runs
  (44.1 kHz), so this probe leaves Chromium's 48 kHz path unverified.
- Chromium: `applyConstraints({noiseSuppression})` on an open track is
  accepted and changes nothing: `getSettings()` keeps the old value, and
  so does the audio. On keyboard alone (auto gain on), the level of 12–19 s
  after a switch at 10 s matched a run without a switch to 0.01 dB, off →
  on and on → off (`results/tables.md`).
- Chromium: a second track opened with suppression **on** while a
  suppression-off track is open comes back with the first one's
  processing: `getSettings()` says off and its audio equals the first
  track's to every digit. Each recording opens the fake device alone; this
  is the only run with two at once.
- Chromium: a track opened with suppression off, `stop()`ped after 8 s and
  replaced at once by `getUserMedia` with it on (14.5 ms) gets the new
  processing: `getSettings()` says on and its audio equals a fresh
  suppression-on run to every digit (the fake device restarts its file
  for a new capture). D11's stop-then-reopen works there.
- WebKit: no `noiseSuppression` or `autoGainControl` constraint.
  Firefox's fake device reports every processing flag false, so its
  constraint rows say nothing about a real microphone.
- Chromium exposes a `voiceIsolation` constraint; not tried here.

### 12.7 NS1–NS4: the production pipeline (2026-10-27)

The implementation measured against NS0, through the code that ships:
the worklet source (`voiceFraming.ts`) with the committed wasm, in Node
and in browsers.

**Build (NS1).** `rnnoise.wasm` is 1,470,633 bytes, sha256 `e84c2ae9…`;
two builds on one host were byte-identical, through bind mounts and
through `build.sh`'s stdin/stdout transport. It differs from NS0's
`v02-avx` (`20e9628e…`) only in its export list (no
`rnnoise_get_frame_size`). The app image serves it as `application/wasm`,
gzipped (CI checks both).

**The gate on the production worklet, offline (Node 24).** NS0's keyboard
minute, D9's default gate:

| | −36 dBFS | −26 dBFS |
|---|---|---|
| 48 kHz context, no resampling | 0.50 % | 1.57 % |
| 44.1 kHz (soxr from 48), linear interpolation back (D6 as designed) | 0.52 % | **2.08 %** |
| 44.1 kHz, the 64-tap windowed sinc (D6 as built) | 0.50 % | 1.63 % |
| 44.1 kHz, soxr back to 48 kHz, no worklet resampling | 0.50 % | 1.57 % |

The 48 kHz row is NS0's §12.3 to the digit, so the worklet framing, scale
and gate are the ones NS0 measured. Band-limiting to 44.1 kHz costs
nothing (last row); linear interpolation does.

**Chromium (headless, Playwright 1.64.1), `e2e/voice-rnnoise.mjs
--corpus`.** The room's AudioContext runs at 44.1 kHz. NS0's minute of
keyboard alone at −36 dBFS as the fake microphone, D11's constraints,
counting VoiceFrames with audio over whole loops of the file:

| | Sent |
|---|---|
| A 48 kHz context forced (no resampling) | 2.3 %, 2.3 % (two runs) |
| 44.1 kHz, linear interpolation | 3.5 %, 4.1 % (two runs, terminators included) |
| 44.1 kHz, windowed sinc, single minutes | 1.1, 1.7, 1.7, 1.7, 3.3, 3.4 % (six runs) |
| 44.1 kHz, windowed sinc, three minutes in one run | 1.1, 1.1, 1.1 % |

Within a run the minutes agree (the same few keyboard sounds open the
gate on every loop); between runs they differ by up to 3×, which is
Chromium's processing, not the gate. With one to six spurts a minute, one
minute is a noisy test of §1's ≤ 3 % row; the e2e judges the mean of three.
Two of the six single minutes were above 3 %: NS5's real microphones
decide whether the default **on** needs to move (§6).

**The e2e's synthetic microphone** (8 s of generated keyboard, then 6 s of
generated speech over it; no third-party audio). RNNoise sends 100 % of
the speech window and 15–20 % of the keyboard window (the synthetic
clicks are broadband, and after Chromium's processing RNNoise passes more
of them than of real typing); Browser, after a re-open with the first
track stopped, sends ~100 % of both. Back on RNNoise, after a second
re-open, the keyboard drops again. Screens at 360/390/430/1280 px with the
Audio section open show no horizontal overflow.

**The module's way into the worklet.** NS4's first CI run (Chrome 154)
sat at "RNNoise is loading…": the wasm downloaded in 9 ms and the worklet
ran, but a `WebAssembly.Module` posted to the worklet's port arrives as a
`messageerror` on Chrome 151 and 154, and as the module on 156 (NS0's
§12.6 port row was 156 only). `processorOptions` carries it on 151 and 156
alike, as D5 designed; the implementation had moved it to the port and is
back on `processorOptions`, with a node swap when the module lands late.
Chromium 151 and 156 then both pass the e2e.

**Firefox and WebKit** (Playwright 1.64.1's builds, in
`mcr.microsoft.com/playwright` on a PulseAudio null sink): RNNoise loads
and runs with no fallback, and 151–152 frames leave in 1.5 s of
push-to-talk on each. Without a sound server both leave the AudioContext
suspended and nothing is captured (docs/gotchas.md).

**The §1 rule on synthetic material (NS3, `speechGate.test.ts`).** Three
generated readings over keyboard, white, fan and hum noise at 10 and 5 dB:
99.9 % of speech sent, 0 of 120 onsets clipped by ≥ 50 ms; a generated
keyboard minute alone sends 0 % at −36 and −26 dBFS. Generated audio is
kinder than NS0's corpus, so this guards the rule and the pipeline in CI;
the corpus numbers above are the measurement.

