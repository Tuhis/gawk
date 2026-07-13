import { describe, expect, it } from 'vitest';

import { FpsGate } from './preprocess';

const US = 1_000_000;

// Feed `count` frames at `fps` starting at `startUs`, return accepted timestamps.
function run(gate: FpsGate, fps: number, count: number, startUs = 0): number[] {
  const accepted: number[] = [];
  for (let i = 0; i < count; i++) {
    const ts = Math.round(startUs + (i * US) / fps);
    if (gate.accept(ts)) accepted.push(ts);
  }
  return accepted;
}

describe('FpsGate', () => {
  it('passes everything through with no target (native rung)', () => {
    const gate = new FpsGate();
    expect(run(gate, 60, 120)).toHaveLength(120);
    expect(gate.droppedCount).toBe(0);
  });

  it('halves 60 fps input to 30 fps', () => {
    const gate = new FpsGate();
    gate.setTargetFps(30);
    const accepted = run(gate, 60, 120); // 2 seconds
    // 2s at 30 fps = 60 frames (± the anchor frame).
    expect(accepted.length).toBeGreaterThanOrEqual(59);
    expect(accepted.length).toBeLessThanOrEqual(61);
    expect(gate.droppedCount).toBe(120 - accepted.length);
  });

  it('decimates 60 fps input to 5 fps', () => {
    const gate = new FpsGate();
    gate.setTargetFps(5);
    const accepted = run(gate, 60, 600); // 10 seconds
    expect(accepted.length).toBeGreaterThanOrEqual(49);
    expect(accepted.length).toBeLessThanOrEqual(51);
  });

  it('holds the target cadence under input jitter', () => {
    const gate = new FpsGate();
    gate.setTargetFps(30);
    let accepted = 0;
    // 60 fps with ±4ms jitter, deterministic pattern.
    for (let i = 0; i < 600; i++) {
      const jitter = (i % 3 - 1) * 4000;
      if (gate.accept(Math.round((i * US) / 60) + jitter)) accepted++;
    }
    // 10 seconds at 30 fps, generous tolerance for jitter effects.
    expect(accepted).toBeGreaterThanOrEqual(280);
    expect(accepted).toBeLessThanOrEqual(320);
  });

  it('accepts everything when input is already at or below the target', () => {
    const gate = new FpsGate();
    gate.setTargetFps(60);
    expect(run(gate, 30, 60)).toHaveLength(60);
    expect(gate.droppedCount).toBe(0);
  });

  it('re-anchors after a capture stall instead of bursting to catch up', () => {
    const gate = new FpsGate();
    gate.setTargetFps(30);
    run(gate, 60, 60); // 1s of steady input
    // 5-second stall, then steady 60 fps resumes.
    const resumed = run(gate, 60, 60, 6 * US);
    // The first resumed frame is accepted immediately (re-anchor)…
    expect(resumed[0]).toBe(6 * US);
    // …and the resumed second still gates to ~30 fps, not a catch-up burst.
    expect(resumed.length).toBeGreaterThanOrEqual(29);
    expect(resumed.length).toBeLessThanOrEqual(31);
  });

  it('re-anchors when the target changes mid-stream', () => {
    const gate = new FpsGate();
    gate.setTargetFps(5);
    run(gate, 60, 60); // 1s at 5 fps cadence
    gate.setTargetFps(30);
    const after = run(gate, 60, 60, 1 * US);
    expect(after.length).toBeGreaterThanOrEqual(29);
    expect(after.length).toBeLessThanOrEqual(31);
  });

  it('clearing the target restores passthrough', () => {
    const gate = new FpsGate();
    gate.setTargetFps(5);
    run(gate, 60, 60);
    gate.setTargetFps(null);
    expect(run(gate, 60, 60, 1 * US)).toHaveLength(60);
  });
});
