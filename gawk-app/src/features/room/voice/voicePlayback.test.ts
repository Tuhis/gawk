// Per-speaker playback (docs/80 D22): a decoder + sink per speaker, created
// on the first frame, released after 10 s of silence, bounded; spurts
// re-anchor the sink; speaking indicators (D16) from the same frames.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AudioChunk } from '../../../transport/audio-buffer';
import {
  MAX_VOICE_SPEAKERS,
  SPEAKER_IDLE_RELEASE_MS,
  SPURT_GAP_MS,
  ArrivalJitter,
  applyFade,
  type FrameMeta,
  JITTER_HALF_LIFE_MS,
  VoicePlayback,
  createOpusDecoder,
  realPlaybackDeps,
  voicePlaybackSupported,
  type SpeakerDecoder,
  type SpeakerSink,
} from './voicePlayback';
import { SPEAKING_HOLD_MS } from './speaking';

interface FakeDecoder extends SpeakerDecoder {
  decoded: { payload: Uint8Array; ts: number; meta: FrameMeta }[];
  closed: boolean;
  restarts: number;
  // A restart's boundary callback, held until the test releases it (the old
  // codec still draining) when `holdRestarts` is set.
  heldBoundaries: (() => void)[];
  emit: (chunk: AudioChunk, meta?: FrameMeta) => void;
  fail: (e: Error) => void;
}
interface FakeSink extends SpeakerSink {
  pushed: AudioChunk[];
  // Not part of SpeakerSink: here only to prove nothing flushes a speaker.
  flush(): void;
  flushes: number;
  reanchors: number;
  volume: number;
  disposed: boolean;
  targets: { jitterMs: number | null; nowMs: number; afterReanchors: number }[];
}

let clock = 0;
let holdRestarts = false;
let decoders: FakeDecoder[];
let sinks: FakeSink[];

function makePlayback(volumeFor: (s: number) => number = () => 1, onSpeaking = vi.fn()) {
  const p = new VoicePlayback(
    {
      createDecoder: (onChunk, onError) => {
        const d: FakeDecoder = {
          decoded: [],
          closed: false,
          restarts: 0,
          heldBoundaries: [],
          decode(payload, ts, meta) {
            this.decoded.push({ payload, ts, meta });
          },
          restart(onBoundary) {
            this.restarts++;
            if (holdRestarts) this.heldBoundaries.push(onBoundary);
            else onBoundary();
          },
          close() {
            this.closed = true;
          },
          emit: (chunk, meta = { first: false, last: false }) => onChunk(chunk, meta),
          fail: onError,
        };
        decoders.push(d);
        return d;
      },
      createSink: () => {
        const s: FakeSink = {
          pushed: [],
          flushes: 0,
          reanchors: 0,
          volume: -1,
          disposed: false,
          targets: [],
          push(c) {
            this.pushed.push(c);
          },
          flush() {
            this.flushes++;
          },
          reanchor() {
            this.reanchors++;
          },
          setVolume(v) {
            this.volume = v;
          },
          updateTarget(jitterMs, nowMs) {
            this.targets.push({ jitterMs, nowMs, afterReanchors: this.reanchors });
          },
          dispose() {
            this.disposed = true;
          },
        };
        sinks.push(s);
        return s;
      },
      now: () => clock,
    },
    { onSpeaking },
    volumeFor,
  );
  return { p, onSpeaking };
}

const frame = (speaker: number, seq: number, end = false) => ({ end, target: 0, speaker, seq });
const opus = new Uint8Array([0xfc, 0xff]);

beforeEach(() => {
  vi.useFakeTimers();
  clock = 0;
  holdRestarts = false;
  decoders = [];
  sinks = [];
});
afterEach(() => vi.useRealTimers());

describe('VoicePlayback decoder lifecycle', () => {
  it('creates one decoder + sink per speaker on the first frame, at its persisted volume', () => {
    const { p } = makePlayback((s) => (s === 7 ? 0.3 : 1));
    p.handleFrame(frame(7, 100), opus);
    p.handleFrame(frame(7, 101), opus);
    p.handleFrame(frame(9, 5), opus);
    expect(decoders).toHaveLength(2);
    expect(sinks.map((s) => s.volume)).toEqual([0.3, 1]);
    // seq is in 10 ms units: the decoder's timestamps are µs.
    expect(decoders[0].decoded.map((d) => d.ts)).toEqual([1_000_000, 1_010_000]);
    // Decoded PCM goes to that speaker's sink.
    const chunk: AudioChunk = { timestampUs: 0, channels: [new Float32Array(480)], sampleRate: 48000, frameCount: 480 };
    decoders[0].emit(chunk);
    expect(sinks[0].pushed).toEqual([chunk]);
    expect(p.speakerCount).toBe(2);
    p.dispose();
  });

  it('releases a speaker after 10 s of silence, and a returning one gets a fresh decoder', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(7, 1), opus);
    clock = SPEAKER_IDLE_RELEASE_MS - 1;
    vi.advanceTimersByTime(SPEAKER_IDLE_RELEASE_MS - 1);
    expect(decoders[0].closed).toBe(false);
    clock = SPEAKER_IDLE_RELEASE_MS;
    vi.advanceTimersByTime(1000);
    expect(decoders[0].closed).toBe(true);
    expect(sinks[0].disposed).toBe(true);
    expect(p.speakerCount).toBe(0);
    p.handleFrame(frame(7, 2000), opus);
    expect(decoders).toHaveLength(2);
    p.dispose();
  });

  it('is bounded: a speaker past the cap evicts the least recently heard', () => {
    const { p } = makePlayback();
    for (let i = 0; i < MAX_VOICE_SPEAKERS; i++) {
      clock = i;
      p.handleFrame(frame(100 + i, 0), opus);
    }
    clock = 50;
    p.handleFrame(frame(100, 1), opus); // speaker 100 is fresh again
    clock = 60;
    p.handleFrame(frame(999, 0), opus);
    expect(p.speakerCount).toBe(MAX_VOICE_SPEAKERS);
    // 101 was the stalest.
    expect(decoders[1].closed).toBe(true);
    expect(decoders[0].closed).toBe(false);
    p.dispose();
  });

  it('re-anchors the sink at each new spurt: after an end flag or a long pause', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(7, 1), opus);
    clock = 10;
    p.handleFrame(frame(7, 2), opus);
    expect(sinks[0].reanchors).toBe(0);
    // A bare terminator decodes nothing.
    p.handleFrame(frame(7, 3, true), new Uint8Array(0));
    expect(decoders[0].decoded).toHaveLength(2);
    clock = 20;
    p.handleFrame(frame(7, 50), opus);
    expect(sinks[0].reanchors).toBe(1);
    clock = 20 + SPURT_GAP_MS + 1;
    p.handleFrame(frame(7, 90), opus);
    expect(sinks[0].reanchors).toBe(2);
    // A last frame that carries the end flag with audio still decodes.
    p.handleFrame(frame(7, 91, true), opus);
    expect(decoders[0].decoded).toHaveLength(5);
    p.dispose();
  });

  // A Mumble client whose voice activation flaps ends its transmission and
  // starts the next within tens of ms (docs/80 §12.7). Flushing there threw
  // away the end of the last word, still queued in the worklet, and played
  // silence until the cushion refilled.
  it('never flushes the sink at a spurt boundary: the end of the last spurt still plays', () => {
    const { p } = makePlayback();
    for (let k = 0; k < 20; k++) {
      clock = k * 10;
      p.handleFrame(frame(7, k), opus);
    }
    p.handleFrame(frame(7, 20, true), new Uint8Array(0));
    clock += 40;
    p.handleFrame(frame(7, 0), opus);
    clock += SPURT_GAP_MS + 1;
    p.handleFrame(frame(7, 1), opus);
    expect(sinks[0].reanchors).toBe(2);
    expect(sinks[0].flushes).toBe(0);
    p.dispose();
  });

  it('a decoder error or a throwing decode releases that speaker only', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(1, 0), opus);
    p.handleFrame(frame(2, 0), opus);
    decoders[0].fail(new Error('bad opus'));
    expect(sinks[0].disposed).toBe(true);
    expect(p.speakerCount).toBe(1);
    decoders[1].decode = () => {
      throw new Error('closed');
    };
    p.handleFrame(frame(2, 1), opus);
    expect(p.speakerCount).toBe(0);
    p.dispose();
  });

  it('per-person volume reaches the speaker\'s sink; dispose releases every speaker', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(1, 0), opus);
    p.handleFrame(frame(2, 0), opus);
    p.setSpeakerVolume(2, 0.25);
    p.setSpeakerVolume(77, 0.5); // unknown: nothing
    expect(sinks[1].volume).toBe(0.25);
    p.dispose();
    p.dispose();
    expect(decoders.every((d) => d.closed)).toBe(true);
    expect(sinks.every((s) => s.disposed)).toBe(true);
    p.handleFrame(frame(3, 0), opus);
    expect(decoders).toHaveLength(2);
  });

  it('a speaker whose decoder cannot be built stays silent without breaking the rest', () => {
    const p = new VoicePlayback(
      {
        createDecoder: () => {
          throw new Error('no AudioDecoder');
        },
        createSink: () => null,
        now: () => clock,
      },
      { onSpeaking: () => {} },
      () => 1,
    );
    expect(() => p.handleFrame(frame(1, 0), opus)).not.toThrow();
    expect(() => p.handleFrame(frame(1, 1), opus)).not.toThrow();
    p.dispose();
  });
});

describe('VoicePlayback speaking indicators', () => {
  it('speaking from the first frame, held 250 ms after the last, cleared at once on the end flag', () => {
    const { p, onSpeaking } = makePlayback();
    p.handleFrame(frame(7, 0), opus);
    expect(onSpeaking).toHaveBeenLastCalledWith([7]);
    clock = 100;
    p.handleFrame(frame(7, 10), opus);
    clock = 100 + SPEAKING_HOLD_MS - 1;
    vi.advanceTimersByTime(SPEAKING_HOLD_MS - 1);
    expect(onSpeaking).toHaveBeenCalledTimes(1);
    clock = 100 + SPEAKING_HOLD_MS;
    vi.advanceTimersByTime(1);
    expect(onSpeaking).toHaveBeenLastCalledWith([]);

    clock = 1000;
    p.handleFrame(frame(9, 0), opus);
    expect(onSpeaking).toHaveBeenLastCalledWith([9]);
    p.handleFrame(frame(9, 1, true), new Uint8Array(0));
    expect(onSpeaking).toHaveBeenLastCalledWith([]);
    p.dispose();
  });
});

describe('voicePlaybackSupported', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('needs WebTransport, AudioDecoder and Web Audio', () => {
    vi.stubGlobal('WebTransport', undefined);
    expect(voicePlaybackSupported()).toBe(false);
    vi.stubGlobal('WebTransport', class {});
    vi.stubGlobal('AudioDecoder', class {});
    vi.stubGlobal('AudioContext', class {});
    vi.stubGlobal('AudioWorkletNode', class {});
    expect(voicePlaybackSupported()).toBe(true);
    vi.stubGlobal('AudioDecoder', undefined);
    expect(voicePlaybackSupported()).toBe(false);
  });

  it('the real deps build no sink without an output', () => {
    expect(realPlaybackDeps(() => null).createSink()).toBeNull();
  });
});

describe('createOpusDecoder', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('configures Opus 48 kHz mono, stamps chunks by seq, and copies planar PCM out', () => {
    const made: { config: unknown; decoded: { timestamp: number; data: Uint8Array }[]; closed: number; output: (d: unknown) => void; error: (e: unknown) => void; state: string }[] = [];
    vi.stubGlobal(
      'AudioDecoder',
      class {
        state = 'configured';
        rec: (typeof made)[number];
        constructor(init: { output: (d: unknown) => void; error: (e: unknown) => void }) {
          this.rec = { config: null, decoded: [], closed: 0, output: init.output, error: init.error, state: 'configured' };
          made.push(this.rec);
        }
        configure(c: unknown) {
          this.rec.config = c;
        }
        decode(chunk: { timestamp: number; data: Uint8Array }) {
          this.rec.decoded.push(chunk);
        }
        close() {
          this.rec.closed++;
          this.state = 'closed';
        }
      },
    );
    vi.stubGlobal(
      'EncodedAudioChunk',
      class {
        timestamp: number;
        data: Uint8Array;
        constructor(init: { timestamp: number; data: Uint8Array }) {
          this.timestamp = init.timestamp;
          this.data = init.data;
        }
      },
    );
    const chunks: AudioChunk[] = [];
    const errors: Error[] = [];
    const d = createOpusDecoder((c) => chunks.push(c), (e) => errors.push(e));
    expect(made[0].config).toEqual({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1 });
    d.decode(new Uint8Array([1]), 20_000, { first: true, last: false });
    expect(made[0].decoded[0].timestamp).toBe(20_000);
    let closed = 0;
    made[0].output({
      numberOfChannels: 1,
      numberOfFrames: 4,
      sampleRate: 48000,
      timestamp: 20_000,
      copyTo: (dst: Float32Array) => dst.fill(0.25),
      close: () => closed++,
    });
    expect(chunks[0]).toMatchObject({ timestampUs: 20_000, sampleRate: 48000, frameCount: 4 });
    expect(Array.from(chunks[0].channels[0])).toEqual([0.25, 0.25, 0.25, 0.25]);
    made[0].output({
      numberOfChannels: 1,
      numberOfFrames: 4,
      sampleRate: 48000,
      timestamp: 0,
      copyTo: () => {
        throw new Error('bad format');
      },
      close: () => closed++,
    });
    expect(chunks).toHaveLength(1);
    expect(closed).toBe(2);
    made[0].error('boom');
    expect(errors[0].message).toBe('boom');
    d.close();
    d.close();
    expect(made[0].closed).toBe(1);
  });
});

describe('VoicePlayback, Hear yourself (docs/81 D16)', () => {
  it('plays your own packets through a speaker of its own: never in the speaking set or the cap', () => {
    const { p, onSpeaking } = makePlayback();
    p.handleSelfFrame({ end: true, seq: 0 }, new Uint8Array(0)); // a bare terminator builds nothing
    expect(decoders).toHaveLength(0);
    p.handleSelfFrame({ end: false, seq: 3 }, opus);
    p.handleSelfFrame({ end: false, seq: 4 }, opus);
    expect(decoders).toHaveLength(1);
    expect(decoders[0].decoded.map((d) => d.ts)).toEqual([30_000, 40_000]);
    expect(sinks[0].volume).toBe(1);
    expect(p.speakerCount).toBe(0);
    expect(onSpeaking).not.toHaveBeenCalled();
    // A new spurt re-anchors, as anyone's does.
    p.handleSelfFrame({ end: true, seq: 5 }, new Uint8Array(0));
    p.handleSelfFrame({ end: false, seq: 50 }, opus);
    expect(sinks[0].reanchors).toBe(1);
    // Others fill the cap without evicting it.
    for (let s = 1; s <= MAX_VOICE_SPEAKERS + 1; s++) p.handleFrame(frame(s, 0), opus);
    expect(decoders[0].closed).toBe(false);
    p.stopSelf();
    expect(decoders[0].closed).toBe(true);
    expect(sinks[0].disposed).toBe(true);
    p.stopSelf();
    p.dispose();
  });

  it('is released when idle, on a decoder error, and on dispose; nothing after dispose', () => {
    const { p } = makePlayback();
    p.handleSelfFrame({ end: false, seq: 0 }, opus);
    clock = SPEAKER_IDLE_RELEASE_MS;
    p.sweep();
    expect(decoders[0].closed).toBe(true);
    p.handleSelfFrame({ end: false, seq: 1 }, opus);
    decoders[1].fail(new Error('bad'));
    expect(sinks[1].disposed).toBe(true);
    p.handleSelfFrame({ end: false, seq: 2 }, opus);
    p.dispose();
    expect(decoders[2].closed).toBe(true);
    p.handleSelfFrame({ end: false, seq: 3 }, opus);
    expect(decoders).toHaveLength(3);
  });
});

// A native Mumble client on Linux (Flatpak, PipeWire) was measured on the
// production server sending its 10 ms packets in clumps: median
// inter-arrival 0.2 ms, p99 83 ms, up to 126 ms late against its own seq
// clock. The fixed 60 ms cushion ran dry on every long gap: a pop every ~2 s
// for that speaker only, while web speakers (≤ 15 ms late) were clean.
describe('ArrivalJitter', () => {
  it('reads a steady 10 ms cadence as no jitter', () => {
    const j = new ArrivalJitter();
    for (let k = 0; k < 100; k++) j.frame(1000 + k, k * 10, k === 0);
    expect(j.jitterMs(990)).toBe(0);
  });

  it('measures how late a clumped sender runs against its own seq clock', () => {
    const j = new ArrivalJitter();
    // Eight packets at once every 80 ms: the last of each clump is on time,
    // the first is 70 ms late.
    for (let k = 0; k < 200; k++) j.frame(k, Math.floor(k / 8) * 80 + 70, k === 0);
    expect(j.jitterMs(1600 + 70)).toBe(70);
  });

  it('is relative to each spurt: a seq jump across a pause is not lateness', () => {
    const j = new ArrivalJitter();
    for (let k = 0; k < 50; k++) j.frame(k, k * 10, k === 0);
    // A spurt starting 3 s later at a seq that jumped (or reset) arbitrarily.
    for (let k = 0; k < 50; k++) j.frame(9000 + k, 3500 + k * 10, k === 0);
    expect(j.jitterMs(4000)).toBe(0);
  });

  it('holds the peak past its window, decaying', () => {
    const k = new ArrivalJitter();
    k.frame(0, 0, true);
    k.frame(1, 100, false);
    expect(k.jitterMs(100)).toBe(90);
    // Past the window the measurement is gone but the peak is held, decaying
    // by half each JITTER_HALF_LIFE_MS: a speaker's bursts come back with
    // their next sentence, and Mumble remembers a user's the same way
    // (AudioOutputSpeech's per-user fAverageAvailable).
    expect(k.jitterMs(100 + JITTER_HALF_LIFE_MS)).toBeCloseTo(45, 5);
  });
});

describe('VoicePlayback cushion per speaker', () => {
  it("sizes each speaker's sink by that speaker's measured arrival jitter", () => {
    const { p } = makePlayback();
    for (let k = 0; k < 200; k++) {
      clock = Math.floor(k / 8) * 80 + 70;
      p.handleFrame(frame(7, k), opus);
      clock = k * 10;
      p.handleFrame(frame(8, k), opus);
    }
    const last = (s: FakeSink) => s.targets[s.targets.length - 1]?.jitterMs;
    expect(last(sinks[0])).toBe(70);
    expect(last(sinks[1])).toBe(0);
    p.dispose();
  });

  it('carries the learned cushion into the next spurt, after the re-anchor', () => {
    const { p } = makePlayback();
    for (let k = 0; k < 80; k++) {
      clock = Math.floor(k / 8) * 80 + 70;
      p.handleFrame(frame(7, k), opus);
    }
    p.handleFrame(frame(7, 80, true), new Uint8Array(0));
    clock += 2000;
    p.handleFrame(frame(7, 400), opus);
    const t = sinks[0].targets[sinks[0].targets.length - 1];
    // The spurt's cushion reaches the sink at the re-anchor itself, before
    // the spurt's first chunk can be released against a stale target.
    expect(t.afterReanchors).toBe(1);
    expect(t.jitterMs).toBe(70);
    p.dispose();
  });

  it('remembers a speaker across the idle release: the next sentence starts on their cushion', () => {
    const { p } = makePlayback();
    for (let k = 0; k < 80; k++) {
      clock = Math.floor(k / 8) * 80 + 70;
      p.handleFrame(frame(7, k), opus);
    }
    clock += SPEAKER_IDLE_RELEASE_MS;
    p.sweep();
    expect(sinks[0].disposed).toBe(true);
    p.handleFrame(frame(7, 5000), opus);
    const first = sinks[1].targets[0];
    expect(first.jitterMs).toBeGreaterThan(50);
    p.dispose();
  });
});

// Mumble's client resets its Opus encoder at the start of every transmission
// (AudioInput: bResetEncoder → OPUS_RESET_STATE) and its listeners decode
// each transmission on a fresh AudioOutputSpeech, fading the first frame in
// and the last one out. Decoding a reset encoder's first packets on a
// decoder that kept the last transmission's state garbles them: 10 ms CELT
// at 72 kbit/s measured −0.9 dB SNR over each transmission's first 20 ms
// against 22.6 dB on a fresh decoder (docs/80 §12.8).
describe('VoicePlayback transmissions, as Mumble plays them', () => {
  it('restarts the decoder at every new transmission, never the first', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(7, 0), opus);
    p.handleFrame(frame(7, 1, true), opus);
    expect(decoders[0].restarts).toBe(0);
    clock = 40;
    p.handleFrame(frame(7, 0), opus);
    expect(decoders[0].restarts).toBe(1);
    clock = 40 + SPURT_GAP_MS + 1;
    p.handleFrame(frame(7, 1), opus);
    expect(decoders[0].restarts).toBe(2);
    p.dispose();
  });

  it('re-anchors the sink only once the last transmission is out of the decoder', () => {
    holdRestarts = true;
    const { p } = makePlayback();
    p.handleFrame(frame(7, 900), opus);
    p.handleFrame(frame(7, 901, true), opus);
    clock = 30;
    p.handleFrame(frame(7, 0), opus);
    // The old codec is still draining: its last chunks would land after a
    // re-anchor made now, carrying the old timeline's timestamps.
    expect(sinks[0].reanchors).toBe(0);
    decoders[0].heldBoundaries.shift()!();
    expect(sinks[0].reanchors).toBe(1);
    p.dispose();
  });

  it("marks a transmission's first frame and its end-flagged last frame", () => {
    const { p } = makePlayback();
    p.handleFrame(frame(7, 0), opus);
    p.handleFrame(frame(7, 1), opus);
    p.handleFrame(frame(7, 2, true), opus);
    expect(decoders[0].decoded.map((d) => d.meta)).toEqual([
      { first: true, last: false },
      { first: false, last: false },
      { first: false, last: true },
    ]);
    clock = 30;
    p.handleFrame(frame(7, 0), opus);
    expect(decoders[0].decoded[3].meta).toEqual({ first: true, last: false });
    p.dispose();
  });

  it('fades those frames as Mumble does: sin(i·π/2N) in, its mirror out', () => {
    const { p } = makePlayback();
    p.handleFrame(frame(7, 0), opus);
    const pcm = () => ({ timestampUs: 0, sampleRate: 48000, frameCount: 4, channels: [new Float32Array(4).fill(1)] });
    decoders[0].emit(pcm(), { first: true, last: false });
    decoders[0].emit(pcm(), { first: false, last: false });
    decoders[0].emit(pcm(), { first: false, last: true });
    const w = (i: number) => Math.sin((i * Math.PI) / 8);
    expect(Array.from(sinks[0].pushed[0].channels[0])).toEqual([0, 1, 2, 3].map((i) => Math.fround(w(i))));
    expect(Array.from(sinks[0].pushed[1].channels[0])).toEqual([1, 1, 1, 1]);
    expect(Array.from(sinks[0].pushed[2].channels[0])).toEqual([3, 2, 1, 0].map((i) => Math.fround(w(i))));
    p.dispose();
  });

  it('applyFade shapes every channel of a chunk', () => {
    const c = { timestampUs: 0, sampleRate: 48000, frameCount: 2, channels: [new Float32Array([1, 1]), new Float32Array([2, 2])] };
    applyFade(c, 'in');
    expect(Array.from(c.channels[1])).toEqual([0, Math.fround(2 * Math.sin(Math.PI / 4))]);
  });
});

describe('createOpusDecoder restart', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('drains the old codec, runs the boundary, then decodes the next transmission on a fresh one', async () => {
    const made: { decoded: number[]; closed: number; flushes: number; release: (() => void)[]; output: (d: unknown) => void }[] = [];
    vi.stubGlobal(
      'AudioDecoder',
      class {
        state = 'configured';
        rec: (typeof made)[number];
        constructor(init: { output: (d: unknown) => void }) {
          this.rec = { decoded: [], closed: 0, flushes: 0, release: [], output: init.output };
          made.push(this.rec);
        }
        configure() {}
        decode(chunk: { timestamp: number }) {
          this.rec.decoded.push(chunk.timestamp);
        }
        flush() {
          this.rec.flushes++;
          return new Promise<void>((r) => this.rec.release.push(r));
        }
        close() {
          this.rec.closed++;
          this.state = 'closed';
        }
      },
    );
    vi.stubGlobal(
      'EncodedAudioChunk',
      class {
        timestamp: number;
        constructor(init: { timestamp: number }) {
          this.timestamp = init.timestamp;
        }
      },
    );
    const metas: FrameMeta[] = [];
    const order: string[] = [];
    const d = createOpusDecoder(
      (_c, meta) => metas.push(meta),
      () => {},
    );
    const out = (rec: (typeof made)[number]) =>
      rec.output({ numberOfChannels: 1, numberOfFrames: 1, sampleRate: 48000, timestamp: 0, copyTo: () => {}, close: () => {} });
    d.decode(new Uint8Array([1]), 10_000, { first: true, last: false });
    d.decode(new Uint8Array([1]), 20_000, { first: false, last: true });
    d.restart(() => order.push('boundary'));
    // Frames of the next transmission wait for the old codec to drain.
    d.decode(new Uint8Array([1]), 0, { first: true, last: false });
    expect(made).toHaveLength(1);
    expect(made[0].decoded).toEqual([10_000, 20_000]);
    out(made[0]);
    out(made[0]);
    made[0].release.shift()!();
    await Promise.resolve();
    await Promise.resolve();
    expect(order).toEqual(['boundary']);
    expect(made[0].closed).toBe(1);
    expect(made).toHaveLength(2);
    expect(made[1].decoded).toEqual([0]);
    out(made[1]);
    expect(metas).toEqual([
      { first: true, last: false },
      { first: false, last: true },
      { first: true, last: false },
    ]);
    d.close();
    expect(made[1].closed).toBe(1);
  });
});
