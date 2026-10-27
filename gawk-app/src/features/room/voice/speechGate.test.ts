// R80 NS3 (docs/81 §1, D9): the default gate over RNNoise, judged by NS0's
// rules (docs/assets/81/harness/offline.mjs) on synthetic speech and noise
// run through the real worklet and the real wasm: what it sends with nobody
// talking, and how much speech it sends and how many onsets it clips with
// someone talking over four noises at 10 and 5 dB SNR, pooled as §1 pools
// them. The material is synthetic (voiceTestKit.ts), so these are a
// regression gate on the rule and the pipeline, not NS0's numbers again.
import { describe, expect, it } from 'vitest';
import { SpeechDetector, blockRms } from './talkGate';
import {
  gateMetrics,
  rnnoiseModule,
  runWorklet,
  scaleTo,
  shift,
  speechLabels,
  syntheticFan,
  syntheticHum,
  syntheticKeyboard,
  syntheticSpeech,
  syntheticWhite,
  type GateMetrics,
} from './voiceTestKit';

// RNNoise's 20 ms, in frames: labels move into the output's time base.
const LAG_FRAMES = 2;

function defaultGate(x: Float32Array): Uint8Array {
  const w = runWorklet(48000, true);
  w.send({ t: 'module', module: rnnoiseModule() });
  w.feed(x);
  const d = new SpeechDetector();
  return Uint8Array.from(w.blocks(), (b, f) => (d.update(b.prob, blockRms(b.pcm), f * 10) ? 1 : 0));
}

const NOISES = {
  keyboard: syntheticKeyboard,
  white: syntheticWhite,
  fan: syntheticFan,
  hum: syntheticHum,
};

function pool(ms: GateMetrics[]): GateMetrics {
  return ms.reduce((a, m) => ({
    speechFrames: a.speechFrames + m.speechFrames,
    speechSent: a.speechSent + m.speechSent,
    noiseFrames: a.noiseFrames + m.noiseFrames,
    noiseSent: a.noiseSent + m.noiseSent,
    onsets: a.onsets + m.onsets,
    onsetsClipped50ms: a.onsetsClipped50ms + m.onsetsClipped50ms,
  }));
}

describe('the default gate over RNNoise, by NS0’s rules (docs/81 §1)', () => {
  it.each([
    [-36, 0.01],
    [-26, 0.02],
  ])('a minute of keyboard with nobody talking at %i dBFS sends at most %f of its frames', (dbfs, limit) => {
    const kb = scaleTo(syntheticKeyboard(60, 11), dbfs);
    const gate = defaultGate(kb);
    const m = gateMetrics(gate, new Uint8Array(gate.length));
    expect(m.noiseFrames).toBeGreaterThan(5900);
    expect(m.noiseSent / m.noiseFrames).toBeLessThanOrEqual(limit);
  }, 30_000);

  it('speech over four noises at 10 and 5 dB: >= 99 % of speech sent, <= 2 % of onsets clipped, pooled', () => {
    const readings = [21, 22, 23].map((seed) => syntheticSpeech(12, seed));
    const all: GateMetrics[] = [];
    for (const make of Object.values(NOISES)) {
      for (const snr of [10, 5]) {
        for (const [r, clean] of readings.entries()) {
          const labels = speechLabels(clean);
          const speech = scaleTo(clean, -26, labels);
          const noise = scaleTo(make(12, 100 + r), -26 - snr);
          const mix = speech.map((v, i) => v + noise[i]);
          const m = gateMetrics(defaultGate(mix), shift(labels, LAG_FRAMES));
          all.push(m);
        }
      }
    }
    const p = pool(all);
    expect(p.onsets).toBeGreaterThan(100);
    expect(p.speechSent / p.speechFrames).toBeGreaterThanOrEqual(0.99);
    expect(p.onsetsClipped50ms / p.onsets).toBeLessThanOrEqual(0.02);
  }, 60_000);
});
