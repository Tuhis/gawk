// R15 N2 (docs/20): the audio lane's pure parts — anchor math, seq wrap,
// 1 Hz config re-send — and the core's error containment, driven with fake
// encoders and senders (no WebCodecs, no MSTP).

import { describe, expect, it, vi } from 'vitest';

import {
  AUDIO_ANCHOR_DRIFT_LIMIT_US,
  AUDIO_BITRATE_BPS,
  AUDIO_CONFIG_RESEND_MS,
  AudioLaneCore,
  AudioPacketizer,
  AudioTimestampAnchor,
  type AudioDataLike,
  type AudioEncoderFactory,
  type AudioEncoderInit,
  type EncodedAudioChunkLike,
} from './audio-lane';
import {
  MAX_DATAGRAM_SIZE,
  TYPE_AUDIO_CONFIG,
  TYPE_AUDIO_FRAME,
  parseAudioConfig,
  parseAudioFrame,
} from '../transport/wire';

describe('AudioTimestampAnchor', () => {
  it('anchors the first packet to the wall clock and keeps media spacing', () => {
    const anchor = new AudioTimestampAnchor();
    // Media clock starts at 0, wall clock at 5_000_000 µs.
    expect(anchor.stamp(0, 5_000_000)).toBe(5_000_000);
    // 20 ms later on both clocks: spacing comes from the media clock.
    expect(anchor.stamp(20_000, 5_020_500)).toBe(5_020_000);
    expect(anchor.stamp(40_000, 5_040_900)).toBe(5_040_000);
    expect(anchor.reanchors).toBe(0);
  });

  it('produces monotone shared-clock timestamps', () => {
    const anchor = new AudioTimestampAnchor();
    let prev = -1;
    for (let i = 0; i < 100; i++) {
      const ts = anchor.stamp(i * 20_000, 7_000_000 + i * 20_000 + (i % 7) * 100);
      expect(ts).toBeGreaterThan(prev);
      prev = ts;
    }
  });

  it('re-anchors when drift exceeds the bound', () => {
    const anchor = new AudioTimestampAnchor();
    anchor.stamp(0, 1_000_000);
    // Wall clock jumped far ahead of the anchored timeline (system sleep).
    const stamped = anchor.stamp(20_000, 1_020_000 + AUDIO_ANCHOR_DRIFT_LIMIT_US + 1);
    expect(stamped).toBe(1_020_000 + AUDIO_ANCHOR_DRIFT_LIMIT_US + 1);
    expect(anchor.reanchors).toBe(1);
    // The new anchor holds from here.
    expect(anchor.stamp(40_000, 1_040_001 + AUDIO_ANCHOR_DRIFT_LIMIT_US)).toBe(
      1_040_001 + AUDIO_ANCHOR_DRIFT_LIMIT_US,
    );
    expect(anchor.reanchors).toBe(1);
  });
});

describe('AudioPacketizer', () => {
  const config = {
    codec: 'opus',
    sampleRate: 48000,
    channels: 2,
    description: new Uint8Array(0),
  };

  it('increments seq with uint32 wrap', () => {
    const packetizer = new AudioPacketizer(0xffffffff);
    const [frame1] = packetizer.packetize(new Uint8Array([1]), 1000, 0);
    const [frame2] = packetizer.packetize(new Uint8Array([2]), 2000, 0);
    expect(parseAudioFrame(frame1).header.seq).toBe(0xffffffff);
    expect(parseAudioFrame(frame2).header.seq).toBe(0);
  });

  it('re-sends the config at 1 Hz, piggybacked on the packet flow', () => {
    const packetizer = new AudioPacketizer();
    packetizer.setConfig(config);

    // First packet carries the config.
    let out = packetizer.packetize(new Uint8Array([1]), 0, 0);
    expect(out.map((d) => d[1])).toEqual([TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME]);

    // Inside the cadence window: frame only.
    out = packetizer.packetize(new Uint8Array([2]), 20_000, AUDIO_CONFIG_RESEND_MS - 1);
    expect(out.map((d) => d[1])).toEqual([TYPE_AUDIO_FRAME]);

    // Cadence elapsed: config rides again.
    out = packetizer.packetize(new Uint8Array([3]), 40_000, AUDIO_CONFIG_RESEND_MS);
    expect(out.map((d) => d[1])).toEqual([TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME]);
    expect(packetizer.configsSent).toBe(2);
  });

  it('a config change resets the cadence so viewers hear about it promptly', () => {
    const packetizer = new AudioPacketizer();
    packetizer.setConfig(config);
    packetizer.packetize(new Uint8Array([1]), 0, 0);
    packetizer.setConfig({ ...config, sampleRate: 44100 });
    const out = packetizer.packetize(new Uint8Array([2]), 20_000, 1);
    expect(out.map((d) => d[1])).toEqual([TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME]);
    expect(parseAudioConfig(out[0]).sampleRate).toBe(44100);
  });

  it('produces datagrams within MAX_DATAGRAM_SIZE for max-size packets', () => {
    const packetizer = new AudioPacketizer();
    packetizer.setConfig(config);
    const out = packetizer.packetize(new Uint8Array(MAX_DATAGRAM_SIZE - 16).fill(7), 0, 0);
    for (const d of out) expect(d.length).toBeLessThanOrEqual(MAX_DATAGRAM_SIZE);
  });
});

// ── AudioLaneCore with fakes ──

function audioData(timestamp: number, sampleRate = 48000, channels = 2): AudioDataLike & { closed: boolean } {
  const d = {
    timestamp,
    sampleRate,
    numberOfChannels: channels,
    closed: false,
    close() {
      d.closed = true;
    },
  };
  return d;
}

function chunkOf(timestamp: number, bytes: number[]): EncodedAudioChunkLike {
  return {
    timestamp,
    byteLength: bytes.length,
    copyTo(dst: AllowSharedBufferSource) {
      const view =
        dst instanceof ArrayBuffer
          ? new Uint8Array(dst)
          : new Uint8Array(
              (dst as ArrayBufferView).buffer as ArrayBuffer,
              (dst as ArrayBufferView).byteOffset,
              (dst as ArrayBufferView).byteLength,
            );
      view.set(bytes);
    },
  };
}

interface FakeEncoder {
  init: AudioEncoderInit;
  encoded: number[];
  closed: boolean;
  emit(chunk: EncodedAudioChunkLike, description?: Uint8Array): void;
  emitError(err: Error): void;
}

function fakeEncoderFactory(): { factory: AudioEncoderFactory; encoders: FakeEncoder[] } {
  const encoders: FakeEncoder[] = [];
  const factory: AudioEncoderFactory = (init, callbacks) => {
    const fake: FakeEncoder = {
      init,
      encoded: [],
      closed: false,
      emit: (chunk, description) => callbacks.output(chunk, description),
      emitError: (err) => callbacks.error(err),
    };
    encoders.push(fake);
    return {
      encode: (data) => fake.encoded.push(data.timestamp),
      close: () => {
        fake.closed = true;
      },
    };
  };
  return { factory, encoders };
}

function collectingSender() {
  const sent: Uint8Array[][] = [];
  return {
    sent,
    send: (datagrams: Uint8Array<ArrayBuffer>[]) => {
      sent.push(datagrams);
      return Promise.resolve();
    },
  };
}

describe('AudioLaneCore', () => {
  it('configures the encoder from the first AudioData in hand, not metadata', () => {
    const { factory, encoders } = fakeEncoderFactory();
    const sender = collectingSender();
    const core = new AudioLaneCore({ send: sender.send, onError: () => {} }, factory, () => 0);

    core.pushAudioData(audioData(0, 44100, 1));
    expect(encoders).toHaveLength(1);
    expect(encoders[0].init).toEqual({
      codec: 'opus',
      sampleRate: 44100,
      numberOfChannels: 1,
      bitrate: AUDIO_BITRATE_BPS,
    });
    const stats = core.getStats();
    expect(stats.sampleRate).toBe(44100);
    expect(stats.channels).toBe(1);
  });

  it('closes every AudioData it is fed', () => {
    const { factory } = fakeEncoderFactory();
    const core = new AudioLaneCore(
      { send: () => Promise.resolve(), onError: () => {} },
      factory,
      () => 0,
    );
    const first = audioData(0);
    const second = audioData(20_000);
    core.pushAudioData(first);
    core.pushAudioData(second);
    expect(first.closed).toBe(true);
    expect(second.closed).toBe(true);
    core.stop();
    const late = audioData(40_000);
    core.pushAudioData(late);
    expect(late.closed).toBe(true);
  });

  it('sends config + frame datagrams for encoded packets and counts sends', async () => {
    const { factory, encoders } = fakeEncoderFactory();
    const sender = collectingSender();
    let nowMs = 0;
    const core = new AudioLaneCore({ send: sender.send, onError: () => {} }, factory, () => nowMs);

    core.pushAudioData(audioData(0));
    encoders[0].emit(chunkOf(0, [1, 2, 3]));
    nowMs = 20;
    encoders[0].emit(chunkOf(20_000, [4, 5]));
    await Promise.resolve();

    expect(sender.sent).toHaveLength(2);
    // First send: config + frame; second: frame only (cadence not elapsed).
    expect(sender.sent[0].map((d) => d[1])).toEqual([TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME]);
    expect(sender.sent[1].map((d) => d[1])).toEqual([TYPE_AUDIO_FRAME]);
    const frame = parseAudioFrame(sender.sent[0][1]);
    expect(frame.header.seq).toBe(0);
    expect(Array.from(frame.payload)).toEqual([1, 2, 3]);
    expect(parseAudioFrame(sender.sent[1][0]).header.seq).toBe(1);

    const stats = core.getStats();
    expect(stats.encodedPackets).toBe(2);
    expect(stats.packetsSent).toBe(2);
    expect(stats.configsSent).toBe(1);
    expect(stats.bytesSent).toBeGreaterThan(0);
  });

  it('an encoder error kills only the lane: onError once, encoder closed, later data ignored', () => {
    const { factory, encoders } = fakeEncoderFactory();
    const onError = vi.fn();
    const core = new AudioLaneCore({ send: () => Promise.resolve(), onError }, factory, () => 0);

    core.pushAudioData(audioData(0));
    encoders[0].emitError(new Error('encoder exploded'));
    expect(onError).toHaveBeenCalledTimes(1);
    expect(encoders[0].closed).toBe(true);

    // A second error (double callback) must not re-fire.
    encoders[0].emitError(new Error('again'));
    expect(onError).toHaveBeenCalledTimes(1);

    // Data after death is closed, never encoded (only the pre-error push
    // reached the encoder).
    const late = audioData(20_000);
    core.pushAudioData(late);
    expect(late.closed).toBe(true);
    expect(encoders[0].encoded).toEqual([0]);
  });

  it('send rejection is swallowed — the session owns its own death', async () => {
    const { factory, encoders } = fakeEncoderFactory();
    const onError = vi.fn();
    const core = new AudioLaneCore(
      { send: () => Promise.reject(new Error('no transport')), onError },
      factory,
      () => 0,
    );
    core.pushAudioData(audioData(0));
    encoders[0].emit(chunkOf(0, [1]));
    await Promise.resolve();
    await Promise.resolve();
    expect(onError).not.toHaveBeenCalled();
    expect(core.getStats().packetsSent).toBe(0);
    expect(core.getStats().encodedPackets).toBe(1);
  });

  it('a late description from the encoder updates the config datagram', async () => {
    const { factory, encoders } = fakeEncoderFactory();
    const sender = collectingSender();
    let nowMs = 0;
    const core = new AudioLaneCore({ send: sender.send, onError: () => {} }, factory, () => nowMs);

    core.pushAudioData(audioData(0));
    encoders[0].emit(chunkOf(0, [1]), new Uint8Array([0xaa, 0xbb]));
    await Promise.resolve();

    const cfg = parseAudioConfig(sender.sent[0][0]);
    expect(Array.from(cfg.description)).toEqual([0xaa, 0xbb]);
    expect(cfg.sampleRate).toBe(48000);
  });
});

// docs/20 field finding 13 (2026-07-26): audio timestamps must be anchored on
// the same pipeline stage video's are — capture arrival — or the encoder's own
// latency is written into the labels and the viewer plays audio that much
// behind its picture, permanently and invisibly. `capture.ts` stamps a
// VideoFrame with `performance.now()` at MSTP arrival, *before* encode; the
// audio anchor used to be established in the encoder's output callback, so it
// carried MSTP delivery + Opus algorithmic delay + queueing + the one-shot
// encoder init. Nothing downstream can see it: the timestamps ARE the sync
// reference, so `avSkewMs` reads a clean zero while lip sync is wrong.
describe('AudioLaneCore timestamp reference point', () => {
  it('stamps packets from the input arrival, not the encoder output', async () => {
    const { factory, encoders } = fakeEncoderFactory();
    const sender = collectingSender();
    let nowMs = 1000;
    const core = new AudioLaneCore({ send: sender.send, onError: () => {} }, factory, () => nowMs);

    // The AudioData for media time 0 arrives at t=1000 ms…
    core.pushAudioData(audioData(0));
    // …and the encoder emits it 80 ms later (init + algorithmic delay).
    nowMs = 1080;
    encoders[0].emit(chunkOf(0, [1]));
    await Promise.resolve();

    const frame = parseAudioFrame(sender.sent[0][1]);
    expect(Number(frame.header.timestampUs)).toBe(1_000_000);
    // The lag is now measured instead of baked in — the number to read when
    // lip sync is off and the viewer's own metrics look clean.
    expect(core.getStats().encodeLagMs).toBeCloseTo(80, 5);
  });

  it('keeps the input anchor for every later packet', async () => {
    const { factory, encoders } = fakeEncoderFactory();
    const sender = collectingSender();
    let nowMs = 1000;
    const core = new AudioLaneCore({ send: sender.send, onError: () => {} }, factory, () => nowMs);

    // Steady state: input every 20 ms, output 80 ms behind it.
    for (let i = 0; i < 4; i++) {
      nowMs = 1000 + i * 20;
      core.pushAudioData(audioData(i * 20_000));
    }
    for (let i = 0; i < 4; i++) {
      nowMs = 1080 + i * 20;
      encoders[0].emit(chunkOf(i * 20_000, [i]));
    }
    await Promise.resolve();

    const stamps = sender.sent.map((d) =>
      Number(parseAudioFrame(d[d.length - 1]!).header.timestampUs),
    );
    expect(stamps).toEqual([1_000_000, 1_020_000, 1_040_000, 1_060_000]);
    expect(core.getStats().anchorReanchors).toBe(0);
  });

  it('has no mapping until an input has been observed', () => {
    // The encoder's output callback must be able to ask "what wall time does
    // this media timestamp mean?" without supplying a clock reading of its
    // own — that reading is precisely the contamination. Null before the
    // first input keeps the fallback explicit instead of silently re-anchoring.
    const anchor = new AudioTimestampAnchor();
    expect(anchor.stamped(20_000)).toBeNull();
    anchor.stamp(0, 5_000_000);
    expect(anchor.stamped(20_000)).toBe(5_020_000);
    expect(anchor.reanchors).toBe(0);
  });
});
