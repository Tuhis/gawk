import { describe, expect, it } from 'vitest';
import { DiagnosticsBuffer } from './diagnostics';

interface S {
  frames: number;
  connection: { bytesReceived: number | null } | null;
}

function makeBuffer(capacity = 20) {
  let t = 0;
  const buf = new DiagnosticsBuffer<S>(capacity, () => t);
  return { buf, advance: (ms: number) => (t += ms) };
}

describe('DiagnosticsBuffer', () => {
  it('keeps only the newest N samples', () => {
    const { buf, advance } = makeBuffer(3);
    for (let i = 1; i <= 5; i++) {
      buf.push({ frames: i, connection: null });
      advance(500);
    }
    expect(buf.latest()?.frames).toBe(5);
    const json = JSON.parse(buf.build({}));
    expect(json.samples).toHaveLength(3);
    expect(json.samples[0].stats.frames).toBe(3);
  });

  it('derives a per-second rate from cumulative counters across the window', () => {
    const { buf, advance } = makeBuffer();
    buf.push({ frames: 0, connection: { bytesReceived: 1000 } });
    advance(500);
    buf.push({ frames: 0, connection: { bytesReceived: 2000 } });
    advance(500);
    buf.push({ frames: 0, connection: { bytesReceived: 3000 } });
    // 2000 bytes over 1 s.
    expect(buf.rate((s) => s.connection?.bytesReceived)).toBeCloseTo(2000, 5);
  });

  it('rate is null when the counter goes backwards (pipeline restart)', () => {
    const { buf, advance } = makeBuffer();
    buf.push({ frames: 0, connection: { bytesReceived: 9000 } });
    advance(500);
    buf.push({ frames: 0, connection: { bytesReceived: 100 } }); // reset
    expect(buf.rate((s) => s.connection?.bytesReceived)).toBeNull();
  });

  it('rate is null with fewer than two samples or null counters', () => {
    const { buf, advance } = makeBuffer();
    expect(buf.rate((s) => s.frames)).toBeNull();
    buf.push({ frames: 1, connection: null });
    expect(buf.rate((s) => s.frames)).toBeNull();
    advance(500);
    buf.push({ frames: 2, connection: null });
    expect(buf.rate((s) => s.connection?.bytesReceived)).toBeNull();
    expect(buf.rate((s) => s.frames)).not.toBeNull();
  });

  it('builds well-formed JSON with context, environment and rebased timestamps', () => {
    const { buf, advance } = makeBuffer();
    advance(1000); // samples don't start at t=0
    buf.push({ frames: 1, connection: null });
    advance(500);
    buf.push({ frames: 2, connection: null });

    const json = JSON.parse(buf.build({ surface: 'viewer', broadcastId: 'AB2CD3' }));
    expect(json.surface).toBe('viewer');
    expect(json.broadcastId).toBe('AB2CD3');
    expect(typeof json.capturedAt).toBe('string');
    expect(json.samples).toHaveLength(2);
    expect(json.samples[0].tMs).toBe(0); // rebased to the first sample
    expect(json.samples[1].tMs).toBe(500);
    expect(json.samples[1].stats.frames).toBe(2);
  });
});
