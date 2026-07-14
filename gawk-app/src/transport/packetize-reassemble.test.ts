import { describe, expect, it } from 'vitest';

import { packetizeDecoderConfig, packetizeFrame } from './packetizer';
import { Reassembler, type AssembledFrame } from './reassembler';
import { MAX_CHUNK_PAYLOAD, encodeClockMapping, type DecoderConfigMessage } from './wire';

function patternBytes(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  for (let i = 0; i < length; i++) {
    out[i] = (i * 31 + seed) & 0xff;
  }
  return out;
}

function collector() {
  const frames: AssembledFrame[] = [];
  const configs: DecoderConfigMessage[] = [];
  const r = new Reassembler({
    onFrame: (f) => frames.push(f),
    onConfig: (c) => configs.push(c),
  });
  return { r, frames, configs };
}

describe('packetizeFrame', () => {
  it('splits data into MAX_CHUNK_PAYLOAD-sized datagrams', () => {
    const data = patternBytes(MAX_CHUNK_PAYLOAD * 2 + 100, 1);
    const dgrams = packetizeFrame({ frameId: 7, keyframe: true, timestampUs: 123n }, data);
    expect(dgrams.length).toBe(3);
  });

  it('produces one datagram for an empty frame', () => {
    const dgrams = packetizeFrame({ frameId: 0, keyframe: false, timestampUs: 0n }, new Uint8Array(0));
    expect(dgrams.length).toBe(1);
  });
});

describe('packetize → reassemble round trip', () => {
  it('reassembles a multi-chunk frame byte-for-byte', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 42);
    for (const d of packetizeFrame({ frameId: 3, keyframe: true, timestampUs: 999n }, data)) {
      r.push(d);
    }
    expect(frames).toHaveLength(1);
    expect(frames[0]).toMatchObject({ frameId: 3, keyframe: true, timestampUs: 999n });
    expect(frames[0].data).toEqual(data);
  });

  it('reassembles despite chunk reordering and duplicates', () => {
    const { r, frames } = collector();
    const data = patternBytes(4000, 7);
    const dgrams = packetizeFrame({ frameId: 1, keyframe: false, timestampUs: 5n }, data);
    r.push(dgrams[2]);
    r.push(dgrams[0]);
    r.push(dgrams[0]); // duplicate
    r.push(dgrams[3]);
    r.push(dgrams[1]);
    expect(frames).toHaveLength(1);
    expect(frames[0].data).toEqual(data);
    expect(r.getStats().duplicateChunks).toBe(1);
  });

  it('never emits a frame with a missing chunk', () => {
    const { r, frames } = collector();
    const dgrams = packetizeFrame(
      { frameId: 1, keyframe: false, timestampUs: 0n },
      patternBytes(3000, 3),
    );
    dgrams.slice(1).forEach((d) => r.push(d)); // chunk 0 lost
    expect(frames).toHaveLength(0);
  });

  it('round-trips an empty frame', () => {
    const { r, frames } = collector();
    for (const d of packetizeFrame({ frameId: 9, keyframe: false, timestampUs: 1n }, new Uint8Array(0))) {
      r.push(d);
    }
    expect(frames).toHaveLength(1);
    expect(frames[0].data.length).toBe(0);
  });
});

describe('config handling', () => {
  it('emits a config once and deduplicates re-emissions', () => {
    const { r, configs } = collector();
    const cfg = packetizeDecoderConfig('avc1.42E02A', new Uint8Array([1, 2, 3]));
    r.push(cfg);
    r.push(cfg.slice()); // relay re-emission, same bytes
    expect(configs).toHaveLength(1);
    expect(configs[0].codec).toBe('avc1.42E02A');
    expect(r.getStats().duplicateConfigs).toBe(1);
  });

  it('emits again when the config actually changes', () => {
    const { r, configs } = collector();
    r.push(packetizeDecoderConfig('avc1.42E02A', new Uint8Array([1])));
    r.push(packetizeDecoderConfig('vp8'));
    expect(configs.map((c) => c.codec)).toEqual(['avc1.42E02A', 'vp8']);
  });

  it('accepts ArrayBuffer descriptions (WebCodecs hands those out too)', () => {
    const { r, configs } = collector();
    r.push(packetizeDecoderConfig('avc1.42E02A', new Uint8Array([9, 8]).buffer));
    expect(Array.from(configs[0].extradata)).toEqual([9, 8]);
  });
});

describe('ordering policy', () => {
  function pushFrame(r: Reassembler, frameId: number, keyframe: boolean) {
    for (const d of packetizeFrame({ frameId, keyframe, timestampUs: BigInt(frameId) }, patternBytes(100, frameId))) {
      r.push(d);
    }
  }

  it('drops a late-completing delta frame', () => {
    const { r, frames } = collector();
    pushFrame(r, 2, false);
    pushFrame(r, 1, false); // completes after a newer frame was emitted
    expect(frames.map((f) => f.frameId)).toEqual([2]);
    expect(r.getStats().framesDroppedLate).toBe(1);
  });

  it('always emits keyframes, allowing broadcaster-restart recovery', () => {
    const { r, frames } = collector();
    pushFrame(r, 100, true);
    pushFrame(r, 101, false);
    pushFrame(r, 0, true); // broadcaster restarted, frameIds reset
    pushFrame(r, 1, false); // new session's delta must flow
    expect(frames.map((f) => f.frameId)).toEqual([100, 101, 0, 1]);
  });

  it('recovers new-session deltas after restart via the stream-keyframe watermark reset', () => {
    // Since R8, real keyframes arrive over reliable streams and never pass
    // through the reassembler — the datagram-keyframe watermark reset (test
    // above) no longer fires in practice. After a broadcaster restart the
    // watermark must instead be reset via noteStreamKeyframe, or every
    // new-session delta is dropped as late (R10 field finding, docs/14).
    const { r, frames } = collector();
    pushFrame(r, 100_000, false); // old session's last delta; watermark = 100000
    r.noteStreamKeyframe(3); // restart: new session's keyframe (id 3) via stream
    pushFrame(r, 4, false); // new session's delta must flow
    pushFrame(r, 5, false);
    expect(frames.map((f) => f.frameId)).toEqual([100_000, 4, 5]);
    expect(r.getStats().framesDroppedLate).toBe(0);
  });

  it('accepts deltas across frameId rollover (uint32 wrap)', () => {
    const { r, frames } = collector();
    pushFrame(r, 0xffff_fffe, false);
    pushFrame(r, 0xffff_ffff, false);
    pushFrame(r, 0, false); // wire frameId wraps: 0 is the successor
    pushFrame(r, 1, false);
    expect(frames.map((f) => f.frameId)).toEqual([0xffff_fffe, 0xffff_ffff, 0, 1]);
    expect(r.getStats().framesDroppedLate).toBe(0);
  });

  it('evicts the oldest incomplete assembly under pressure', () => {
    const { r, frames } = collector();
    // Frame 0 misses a chunk, then 8 more incomplete assemblies start.
    const stuck = packetizeFrame({ frameId: 0, keyframe: false, timestampUs: 0n }, patternBytes(3000, 0));
    r.push(stuck[0]);
    for (let id = 1; id <= 8; id++) {
      const dgrams = packetizeFrame({ frameId: id, keyframe: false, timestampUs: 0n }, patternBytes(3000, id));
      r.push(dgrams[0]);
    }
    expect(r.getStats().framesDroppedIncomplete).toBe(1);
    // Frame 0's remaining chunks arrive too late — it was evicted, and the
    // leftover chunks just start a fresh (incomplete) assembly.
    stuck.slice(1).forEach((d) => r.push(d));
    expect(frames).toHaveLength(0);
  });
});

describe('clock mapping (R5 Q2)', () => {
  it('routes a ClockMapping datagram to the callback, last one wins', () => {
    const offsets: bigint[] = [];
    const r = new Reassembler({
      onConfig: () => {},
      onFrame: () => {},
      onClockMapping: (offsetUs) => offsets.push(offsetUs),
    });
    r.push(encodeClockMapping(1_500_000n));
    r.push(encodeClockMapping(-42n));
    expect(offsets).toEqual([1_500_000n, -42n]);
    expect(r.getStats().badDatagrams).toBe(0);
  });

  it('drops a malformed ClockMapping as a bad datagram', () => {
    const offsets: bigint[] = [];
    const r = new Reassembler({
      onConfig: () => {},
      onFrame: () => {},
      onClockMapping: (offsetUs) => offsets.push(offsetUs),
    });
    r.push(encodeClockMapping(7n).subarray(0, 5)); // truncated
    expect(offsets).toEqual([]);
    expect(r.getStats().badDatagrams).toBe(1);
  });

  it('tolerates a ClockMapping with no callback wired', () => {
    const r = new Reassembler({ onConfig: () => {}, onFrame: () => {} });
    r.push(encodeClockMapping(5n)); // must not throw or count bad
    expect(r.getStats().badDatagrams).toBe(0);
  });
});

describe('malformed input', () => {
  it('counts bad datagrams without emitting or throwing', () => {
    const { r, frames, configs } = collector();
    r.push(new Uint8Array(0));
    r.push(new Uint8Array([0x02, 0x01])); // wrong version
    r.push(new Uint8Array([0x01, 0x7f])); // unknown type
    r.push(new Uint8Array([0x01, 0x01, 0x00])); // truncated video chunk
    r.push(new Uint8Array([0x01, 0x02, 0x00, 0x05, 0x76])); // codecLen overrun
    expect(r.getStats().badDatagrams).toBe(5);
    expect(frames).toHaveLength(0);
    expect(configs).toHaveLength(0);
  });

  it('rejects a chunk whose count disagrees with its frame', () => {
    const { r, frames } = collector();
    const a = packetizeFrame({ frameId: 5, keyframe: false, timestampUs: 0n }, patternBytes(2000, 1));
    expect(a.length).toBe(2);
    r.push(a[0]);
    // Forge a chunk for frame 5 claiming chunkCount 3.
    const forged = packetizeFrame({ frameId: 5, keyframe: false, timestampUs: 0n }, patternBytes(3000, 2))[1];
    r.push(forged);
    expect(r.getStats().badDatagrams).toBe(1);
    r.push(a[1]);
    expect(frames).toHaveLength(1); // original frame still completes
  });
});
