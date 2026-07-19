// R15 N4 (docs/20 Decision 7) + post-implementation review finding 3: the
// audio sink runs on the viewer's message path, so nothing inside it may
// propagate — audio is never allowed to break video. A detached buffer or a
// closed worklet port must cost one dropped packet, which is the correct
// live-edge outcome anyway.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioSink } from './audioSink';
import type { AudioChunk } from '../../transport/audio-buffer';

const SAMPLE_RATE = 48000;
const FRAME_COUNT = SAMPLE_RATE / 50; // 20 ms

function chunk(timestampUs: number): AudioChunk {
  return {
    timestampUs,
    channels: [new Float32Array(FRAME_COUNT), new Float32Array(FRAME_COUNT)],
    sampleRate: SAMPLE_RATE,
    frameCount: FRAME_COUNT,
  };
}

// Stubs the minimum Web Audio surface the sink touches. `postMessage` is
// injectable so a test can make the worklet port throw.
function stubWebAudio(postMessage: (msg: unknown, transfer?: unknown) => void) {
  class FakeAudioWorkletNode {
    port = { postMessage, onmessage: null as unknown };
    connect = vi.fn();
    disconnect = vi.fn();
  }
  class FakeAudioContext {
    state = 'running';
    destination = {};
    audioWorklet = { addModule: vi.fn(() => Promise.resolve()) };
    createGain = vi.fn(() => ({ gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() }));
    resume = vi.fn(() => Promise.resolve());
    close = vi.fn(() => Promise.resolve());
  }
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeAudioWorkletNode);
  vi.stubGlobal('Blob', class {});
  vi.stubGlobal('URL', {
    createObjectURL: vi.fn(() => 'blob:stub'),
    revokeObjectURL: vi.fn(),
  });
}

beforeEach(() => {
  vi.stubGlobal('performance', { now: () => 0, timeOrigin: 0 });
});
afterEach(() => vi.unstubAllGlobals());

describe('AudioSink worklet delivery', () => {
  it('forwards chunks to the worklet with their channel buffers transferred', async () => {
    const posted: { msg: unknown; transfer: unknown }[] = [];
    stubWebAudio((msg, transfer) => posted.push({ msg, transfer }));
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);

    sink.push(chunk(0));
    expect(posted).toHaveLength(1);
    const msg = posted[0].msg as { type: string; frameCount: number };
    expect(msg.type).toBe('chunk');
    expect(msg.frameCount).toBe(FRAME_COUNT);
    // The channel buffers ride the transfer list — no structured clone of PCM.
    expect((posted[0].transfer as ArrayBuffer[]).length).toBe(2);
    sink.dispose();
  });

  // The finding: without a guard this throw escapes sink.push() → the viewer's
  // message handler. Audio must never break video.
  it('swallows a throwing worklet port instead of propagating', async () => {
    stubWebAudio(() => {
      throw new DOMException('an ArrayBuffer is detached', 'DataCloneError');
    });
    const sink = new AudioSink();
    await sink.start(SAMPLE_RATE);

    expect(() => sink.push(chunk(0))).not.toThrow();
    // And the sink stays usable — a later packet is attempted, not latched off.
    expect(() => sink.push(chunk(20_000))).not.toThrow();
    sink.dispose();
  });

  it('drops audio silently before the worklet exists rather than throwing', () => {
    stubWebAudio(() => {});
    const sink = new AudioSink();
    // No start() — the node is null; the first packets of a stream can land
    // during the worklet's async boot.
    expect(() => sink.push(chunk(0))).not.toThrow();
    sink.dispose();
  });
});
