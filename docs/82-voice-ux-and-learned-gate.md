# R81 — Room voice UX, and a talk gate that learns the speaker's level

**Status**: designed and **VX0–VX3 implemented 2026-10-28**, after the VX0
measurement spike (§12; harness, raw data and tables in
[`assets/82/`](assets/82/results/tables.md)). Owner decision 2026-10-28
(§2). Open: VX4, the owner's pass with real microphones on the reference
deployment.

**Relationship to earlier work**: R79 ([docs/80](80-mumble-bridge.md)) gave
rooms voice through a Mumble bridge, its controls a **Voice section inside
the people panel** and a small in-call dock. R80
([docs/81](81-noise-suppression.md)) put RNNoise before the encoder, a
speech-probability gate with a **fixed −50 dBFS level floor** (D9), auto gain
on (D11), and an **Audio** section. This milestone changes the web app only:
nothing on the relay, the bridge or the wire. It supersedes docs/81 D11's
auto gain for the RNNoise and Off modes (D5) and answers docs/81 §8's open
questions on the floor and on auto gain.

---

## 1. Why, and what "done" means

Two problems, one reported, one measured.

**Voice was hard to find.** A room with voice said so nowhere: the Voice
section was the third section of a panel behind an icon that shows people,
and the dock appeared only once you had joined. New users did not find it.

**Quiet hums got through.** The owner heard hums and murmurs reach the
channel with RNNoise on. VX0 (§12) found why: RNNoise's probability says
*voice*, not *this speaker* — a person humming, a chant, a TV are all voice
to it — and the only thing that could tell them from the speaker, loudness,
was undone by Chromium's auto gain. In a pause it lifts a hum 15 dB below
the speaker by ~20 dB, **to the speaker's own level** (§12.2). No floor,
fixed or learned, separates them after that. With auto gain off, a floor
that follows the speaker's own level does.

### Milestone acceptance criteria

| Goal | Verified by |
|---|---|
| A room with voice says so before anyone joins: a **Voice** button in the header with the number in voice, an **invite chip** where the dock will be (when someone is in voice), or a **one-time hint** (when nobody is) | VX2 component tests; `e2e/voice-panel.mjs` (Chromium, fake relay) |
| Voice is its own panel: a flyout, **pinnable** into a column the stage reflows beside; one right-hand panel at a time; on a phone, People and Voice are tabs of one sheet | VX2 tests; `e2e/voice-panel.mjs` screenshots at 360/390/430/1280 px, no horizontal overflow |
| In a call: mute, deafen, push-to-talk and the **Microphone popover** (talk mode, threshold, noise suppression, Hear yourself) reachable from the dock without opening the panel | VX2 tests; `e2e/voice-panel.mjs` |
| Through Chromium's processing, a person **humming 15 dB below the speaker** is sent **≤ 50 %** of the time (today: 93 %), one **20 dB below ≤ 10 %**; a background voice 15 dB below **≤ 25 %** (today 90 %), 20 dB below **≤ 5 %** (today 90 %) | VX0 §12.3 (measured: 39.9 %, 5.8 %, 20.4 %, 0.0 %) |
| The speaker is not cut: through Chromium **≥ 98.5 %** of speech sent and **≤ 3 %** of onsets clipped by ≥ 50 ms over VX0's nine captures (fixed −50 with auto gain on: 99.5 %, 0 of 88) | VX0 §12.3 (measured: 98.5 %, 2 of 88) |
| docs/81 §1's keyboard row still holds: **≤ 3 %** of a keyboard minute at −36 dBFS through Chromium with nobody talking | VX0 §12.3 (2.0 % alone, 0.0 % after the speaker spoke); `e2e/voice-rnnoise.mjs --corpus` (1.6 %, three minutes) |
| A speaker who gets quieter is followed: after a **10 dB** drop **≥ 90 %** of the next minute's speech is sent | VX0 §12.4 (measured 93 %) |
| Per-person volume goes to **200 %**, behind a limiter on the voice bus; streams are untouched | VX3 tests |

---

## 2. Owner decisions (2026-10-28)

| # | Question | Decision |
|---|---|---|
| OD1 | Voice as its own panel? | **Yes**, optionally pinned (the canvas, §3). |
| OD2 | Per-person volume above 100 %? | **Yes**, the "option 1" of the gain discussion: up to 200 % with a limiter. A microphone boost and an auto-gain switch were not taken. |
| OD3 | Use the level as well as RNNoise's probability, on the same slider? | **Yes**: the floor follows a learned level, the one slider moves both. |
| OD4 | VX0 showed the floor is defeated by auto gain. Which way? | **Auto gain off under RNNoise, plus our own gain** after the gate, from the learned level (the other options were auto gain off with no gain, and keeping auto gain and parking the gate). |
| OD5 | Should the voice notice keep "Mumble can record it"? | **No**: not relevant to users and it confused them. The notice says only that the channel is Mumble's and shared (docs/80 §6's risk stands; the UI no longer spells it out). |

---

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | **Voice is its own panel.** A header **Voice** button (headphones, "Voice", a green count; "In voice · N" while in voice; icon and count only on a phone) opens a 320 px **flyout** under the header. **Pin beside the stage** turns it into a 320 px column the stage reflows beside (saved per browser). People and Voice are **one right-hand panel at a time**; on a phone they are **tabs of the one bottom sheet**. Closing the panel never leaves voice. `VoiceSection` leaves `RoomPanel`. | OD1. A flyout covers only what it needs; pinning is for people who keep it open; two side panels at once would leave no stage. |
| D2 | **Finding voice.** Someone in voice and you not: an **invite chip** at the dock's spot (avatars, "N in voice", Join voice, Listen only, × for this visit). Nobody in voice: a **one-time hint** under the Voice button ("This room has voice chat. Talk with the room, or listen only." · Got it), retired for good by Got it or by opening the panel. Never both. | The reported problem. The chip is the strongest prompt and needs no new data: the roster reaches people outside voice (docs/80 OD28). |
| D3 | **First join asks how to talk**, once (saved): the talk-mode switch with one line on each mode, the live threshold control (voice activity) or the key (push to talk), and Done. After that only the switch remains. | Voice activity versus push to talk is the one choice that decides how a call goes; the threshold is easiest to judge while speaking. |
| D4 | **The Audio section and the Microphone popover.** The threshold slider is laid over the live meter's stretch (speech 0.50–0.99 on 0..1, level −70..−10 on −70..0 dBFS), so its thumb is the line the bar must pass; a plain help line under it. **Push to talk hides the threshold** (`talkGate.ts` ignores it there) and says so. The dock's mic is a split button whose caret opens a 300 px **Microphone** popover: talk mode, threshold, noise suppression, Hear yourself, a link to the panel. | Adjusting the threshold mid-game must not need the panel; one control is easier than a slider and a meter. |
| D5 | **Auto gain only in Browser mode.** `voiceAudioConstraints`: RNNoise and Off open the microphone with `autoGainControl: false`; Browser keeps all three. RNNoise ↔ Off keep one set of constraints, so docs/81 D11's switching (worklet-only between them, a re-open to or from Browser) is unchanged. | VX0 §12.2: with auto gain on, a hum in a pause ends at the speaker's level and every floor fails (§12.3). OD4. Supersedes docs/81 D11 for these two modes. |
| D6 | **Our own gain, after the gate.** Sent blocks are multiplied by `clamp(−20 dBFS − learned level, 0, +20 dB)`, ramped across each block and clipped to ±1. Before this session has a level it uses the last one stored (`gawk:voice-speech-level`, whole dB); it keeps the last level through a long silence. The gate always judges the block as captured. Reset to defaults forgets the level. | Auto gain was there so a quiet microphone reaches listeners at a usable level (docs/81 D11). Driven only by confident speech, this gain never lifts a hum in a pause. Stored, a quiet microphone is not quiet for the first seconds of the next call; the floor never uses the stored value (a stale one could cut the speaker). |
| D7 | **The learned level** (`SpeechLevelTracker`): the **0.85 quantile** of the denoised level of blocks with probability **≥ 0.95** and level ≥ −50 dBFS, over a 1 dB histogram whose weight **halves every 15 s**, known once **300 blocks** of evidence are in hand (~3.5 s of speech). It learns from every confident block, **sent or not**, in voice activity and while the push-to-talk key is held; never while blocked. | Learning only from sent blocks would let the floor ratchet itself up. A high quantile ignores quieter voice-like sound under the speaker. The half-life is VX0 §12.4's trade: 5 min kept a hum out longest but cut a speaker who got 14 dB quieter for a minute (28 % sent); 15 s sends 74 % of that minute and still holds a 15 dB hum to 40 %. |
| D8 | **The floor**: learned level − margin, clamped to **[−50, −30] dBFS**; docs/81 D9's −50 until a level is learned. The margin comes from the one slider: **20 dB at 0.50, 12 at the default 0.90, 10 at 0.99**, linear between. No hysteresis on the floor. | OD3. VX0 §12.3: 12 dB is where a hum 15 dB down is mostly kept out and onsets clipped stay 2 of 88 through Chromium; 10 dB clips 9 of 88. Hysteresis (6 dB) sent more hum for no speech gained. The lenient end is the way out for a speaker the floor cuts. |
| D9 | **The meter shows the floor**: `VoiceMeter.floorDb` (null without RNNoise); a block under it is grey ("too quiet to count") whatever its probability, green past the line is sent. | The learned floor is otherwise invisible; grey says why a word was not sent. |
| D10 | **Per-person volume 0–200 %**: `VolumeControl` takes a `max` (the roster passes 2, a tick at 100 %, amber above it, the value resets to 100 %); `AudioSink` takes a per-sink maximum (streams stay at 1); saved volumes are clamped to 0..2. A `DynamicsCompressorNode` (threshold −3 dB, knee 0, ratio 20, attack 3 ms, release 100 ms) sits between the voice bus and the speakers; streams bypass it. | OD2. A native Mumble user's level is whatever their client sends; the limiter keeps a boosted voice, or several at once, from clipping. |

---

## 4. What changes where

- `gawk-app/src/features/room/voice/talkGate.ts`: `SpeechLevelTracker`,
  `marginForThreshold`, the learned floor in `SpeechDetector` (D7, D8),
  `TalkGate.floorDb()`, `learnedLevelDb()`, `gainDb()`, `forgetLevel()` (D6).
  The tracker ports `docs/assets/82/harness/gates.mjs` `LevelTracker`; keep
  the two in step (VX0 checked they decide identically on two Chromium
  captures, 18,602 blocks, 0 differences).
- `voiceCapture.ts`: D5's constraints; `gainDb` in the capture callbacks,
  applied to sent blocks (D6).
- `voiceController.ts`: the meter's `floorDb`, the stored level in and out,
  `forgetSpeechLevel()`; `voicePrefs.ts` the `gawk:voice-speech-level` key;
  `useRoomVoice.ts` wires them.
- `VoicePanel.tsx` (rewritten), `VoiceAudio.tsx`, `voiceCopy.ts`,
  `RoomPanel.tsx`, `RoomScreen.tsx`, `room.module.css`, `voice.module.css`,
  `BroadcasterScreen.tsx` (its room header gets the Voice button): D1–D4, D9.
- `ui/VolumeControl.tsx`, `features/viewer/audioSink.ts`,
  `voice/voicePlayback.ts`, `roomAudio.ts`: D10.
- `e2e/voice-panel.mjs`, `voice-rnnoise.mjs`, `voice.mjs`, `voice-fake.mjs`:
  the new panel and controls.

## 5. Risks and limits

- **A hum that outlasts the half-life while you are silent creeps back.**
  With nothing else confident, its blocks become the learned level within
  about a minute and the floor drops under it (to what docs/81 shipped).
  The longer half-life that prevents this cuts a speaker who gets quieter
  (D7); being cut is the worse failure.
- **A speaker who drops 14 dB or more is partly cut for tens of seconds**
  (§12.4: 74 % of the minute after a 14 dB drop, 48 % after 18 dB). The meter
  greys those words (D9); the slider's lenient end widens the margin to 20 dB.
- **Onsets**: VX0's offline mix clips 14 of 256 onsets against 3 of 256 with
  the fixed floor (on unprocessed corpus audio); through Chromium 2 of 88
  against 1 of 88 (auto gain off) and 0 of 88 (auto gain on). VX4's real
  microphones decide whether that is heard.
- **Loud humming or singing along** is louder than the floor; no level rule
  helps. Push to talk does.
- **Our gain is not auto gain.** It follows confident speech slowly and is
  capped at +20 dB; a microphone further than that from −20 dBFS stays quiet,
  and nothing lifts a speaker mid-sentence the way auto gain does.
- **Safari** has no `autoGainControl` constraint (docs/81 D12): its own
  processing still runs, D5 changes nothing there, D6's gain adds to it.

## 6. Out of scope

A microphone picker; an auto-gain switch in the UI; a microphone boost before
the gate; per-device stored levels; a speaking ring on stream tiles (needs a
voice-row → participant → broadcast link); hotkeys for mute and deafen.

---

## 9. Chunks

Every letter is claimed; this milestone's prefix is **VX**.

| Chunk | What | Acceptance |
|---|---|---|
| **VX0** ✅ | **Measurement spike**: `docs/assets/82/harness/` (corpus extension, Chromium captures with auto gain on and off, gates, report). | `run-all.sh` reproduces `results/vx0.json` and `tables.md`; §12 filled from them; the §1 rows measured. |
| **VX1** ✅ | The learned gate: D5–D9 in `talkGate.ts`, `voiceCapture.ts`, `voiceController.ts`, `voicePrefs.ts`, `useRoomVoice.ts`. | Vitest first: tracker (minimum evidence, confident-only, eligibility, quantile under a quieter hum, half-life, seed), margin curve, floor clamps, the learned floor rejecting a quiet confident block, a speaker 22 dB quieter followed within 30 s, PTT learning only while held, blocked learns nothing, the gain from a stored level and from the session's, kept through silence, capped, forgotten on reset; constraints per mode; the gain applied after the gate and clipped; the meter's floor; the stored level saved on leave. The TS gate decides identically to the harness on VX0's captures. |
| **VX2** ✅ | The UI: D1–D4, D9. | Component tests (button count and live state, chip and hint rules, flyout/pinned/sheet, one side panel, close keeps the call, first-join card, PTT hides the threshold, popover open/close/Escape, phone tabs and thumb bar); `e2e/voice-panel.mjs` and `e2e/voice-rnnoise.mjs` pass in Chromium with screenshots at four widths, no overflow. |
| **VX3** ✅ | Per-person volume and the limiter: D10. | Tests: clamp 0..2, per-sink maximum (streams 1), limiter wiring and settings, streams bypass it, the boost amber and the reset. |
| **VX4** | **Owner pass** on the reference deployment: Chrome, Firefox and Safari on desktop, Chrome on Android, Safari on iOS, real microphones; a real hum and a TV behind the speaker; leaning back mid-call; a quiet microphone through D6's gain; the limiter with two boosted people; `e2e/voice-rnnoise.mjs` on Firefox and WebKit (needs a sound server); `e2e/voice.mjs` against the compose stack. | §1's rows confirmed by ear and by Hear yourself on all five; findings written into §12. |

---

## 12. Results (VX0, 2026-10-28)

Harness: `docs/assets/82/harness/` (`run-all.sh`). Corpus: NS0's six files
(docs/81 §12) plus four hums from Wikimedia Commons, pinned by sha256 and not
committed (`fetch-extra.sh`): a woman humming (CC0), a murmured chant (CC0),
mains hum (CC BY-SA 3.0), an air conditioner (CC BY 4.0). Speaker at −26 dBFS
active level, as NS0. RNNoise is the wasm gawk-app ships. Chromium is
Playwright's (1.64.1 build), fake microphone from a WAV, docs/81 D11's
constraints with auto gain on and off. All numbers: `results/tables.md`.

### 12.1 Scenarios

*hum*: the speaker 40 s, the hum alone 30 s, the speaker 20 s, the hum 10,
15 and 20 dB below the speaker. *bg*: a second reading under the speaker
throughout, 10/15/20 dB below, with background-only stretches. *mix*: NS0's
matrix (three readings × keyboard, restaurant, street, white at 10 and 5 dB).
*level*: the speaker at −40/−26/−16 dBFS over keyboard. *drop*: two minutes
at −26, then a minute 6/10/14/18 dB softer. *kb*: the keyboard minute alone
and after 30 s of the speaker.

### 12.2 Where the speaker and the hum sit after RNNoise

Confident blocks (probability ≥ 0.95), denoised level, dBFS (p50):

| | speaker | humming woman, 15 dB below | background voice, 15 dB below |
|---|---|---|---|
| Offline (no processing) | −27.5 | −41.2 | −42.5 |
| Chromium, auto gain **on** | −22.4 | **−21.3** | −28.9 |
| Chromium, auto gain off | −27.7 | −41.2 | −42.7 |

Auto gain lifts the hum ~20 dB in the pause, level with the speaker. The
chant and the air conditioner produce few confident blocks; mains hum none
(RNNoise removes stationary hum; it never was the problem).

### 12.3 The gates (on 0.90, hold 300 ms)

Share of interferer-only frames sent through Chromium, and the speaker over
the nine captures:

| Gate | Auto gain | humming −15 | humming −20 | voice −15 | voice −20 | speaker sent · onsets clipped | keyboard minute |
|---|---|---|---|---|---|---|---|
| fixed −50 (docs/81) | on | 93.1 % | 93.1 % | 90.2 % | 90.0 % | 99.5 % · 0/88 | 2.6 % |
| fixed −40 | on | 92.4 % | 91.4 % | 88.6 % | 87.0 % | 99.1 % · 2/88 | |
| learned, margin 12, half-life 15 s | on | 88.9 % | 85.5 % | 77.6 % | 65.3 % | 98.4 % · 2/88 | |
| fixed −50 | off | 91.3 % | 89.5 % | 89.7 % | 83.7 % | 99.3 % · 1/88 | 2.0 % |
| fixed −40 | off | 61.6 % | 8.3 % | 71.2 % | 2.1 % | 96.8 % · 8/88 | |
| **learned, margin 12, half-life 15 s** (shipped) | **off** | **39.9 %** | **5.8 %** | **20.4 %** | **0.0 %** | **98.5 % · 2/88** | **2.0 % (0.0 % after speech)** |
| learned, margin 12, half-life 5 min | off | 19.8 % | 0.0 % | 12.0 % | 0.0 % | 98.6 % · 2/88 | |

A fixed floor cannot serve both a quiet and a loud speaker: with auto gain
off, fixed −40 sends 79 % of a −40 dBFS speaker's speech and clips 7 of 9
onsets, where the learned floor sends 97.9 % (the fixed −50's figure).
Offline, over NS0's mix, the learned floor clips 14 of 256 onsets against
3 of 256 (§5).

### 12.4 Following a speaker who gets quieter

Speech sent in the minute after the drop, offline, margin 12:

| Half-life | −6 dB | −10 dB | −14 dB | −18 dB |
|---|---|---|---|---|
| fixed −50 | 100 % | 100 % | 100 % | 100 % |
| 5 min | 96 % | 80 % | 28 % | 2 % |
| 60 s | 97 % | 85 % | 38 % | 6 % |
| 30 s | 97 % | 90 % | 52 % | 10 % |
| **15 s** | **98 %** | **93 %** | **74 %** | **48 %** |

A lower quantile with a smaller margin (0.7 and 10 dB, 0.5 and 7 dB) follows
faster still but keeps less of the hum and the background voice out
(`tables.md`); 0.85 at 15 s is the balance D7 takes.

### 12.5 In the app

`e2e/voice-rnnoise.mjs` (Chromium, fake relay, the production capture path
with D5–D8): RNNoise sent 9.5 % of the synthetic keyboard and 99.4 % of the
speech, Browser 99.7 % and 100 %; with `--corpus`, three minutes of the real
keyboard sent 1.1, 2.1 and 1.6 % (mean 1.6 %, docs/81 §1 ≤ 3 %).
`e2e/voice-panel.mjs` passed with the new panel at 360, 390, 430 and 1280 px.
Firefox and WebKit were not run (they need a sound server here).
