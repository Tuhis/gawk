// R22 MF1 (docs/27): the fMP4 muxer's automated proof, golden-vector style
// (the wire-test culture). The committed fixture is the same 320x240 @ 30 fps
// H.264 stream gawk-broadcast embeds (h264-fixture.ts documents provenance):
// Annex-B access units with in-band SPS/PPS at every IDR — the native
// broadcaster's wire shape. The AVCC (browser broadcaster) shape is derived
// from it in this file, which doubles as an independent check that both input
// formats produce byte-identical media segments. The final proof — the bytes
// actually playing in a Chrome MediaSource <video> — lives in the e2e harness
// (docs/27 Decision 10), not here: vitest has no media stack.

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_FRAME_DURATION_US,
  Fmp4Muxer,
  buildAvcc,
  codecFromAvcc,
  h264Mime,
  nalType,
  parseAvcc,
  parseSps,
  splitAnnexB,
  splitAvcc,
  type Fmp4InitSegment,
  type Fmp4MediaSegment,
  type Fmp4Segment,
  type MuxInputFrame,
} from './fmp4-muxer';
import {
  FIXTURE_FRAMES,
  FIXTURE_FRAME_INTERVAL_US,
  FIXTURE_HEIGHT,
  FIXTURE_WIDTH,
} from './h264-fixture';

// ---------------------------------------------------------------------------
// Helpers

const FRAME_US = Math.round(FIXTURE_FRAME_INTERVAL_US); // 33333

// The broadcaster clock is huge and arbitrary; the muxer must zero-base it.
const BASE_TS_US = 1_234_567_890n;

function annexBFrame(i: number, overrides: Partial<MuxInputFrame> = {}): MuxInputFrame {
  const f = FIXTURE_FRAMES[i];
  return {
    keyframe: f.keyframe,
    timestampUs: BASE_TS_US + BigInt(i * FRAME_US),
    data: f.data,
    // The native broadcaster's config: codec negotiated, extradata EMPTY —
    // parameter sets are in-band (docs/19).
    config: f.keyframe ? { codec: 'avc1.42E01F', extradata: new Uint8Array(0) } : null,
    ...overrides,
  };
}

// Derive the browser-broadcaster (AVCC) shape of a fixture frame: parameter
// sets move to the extradata, AUD/SPS/PPS leave the sample, NALs get 4-byte
// length prefixes. Deliberately re-implemented here rather than importing the
// muxer's own conversion — the test must not trust the code under test.
function toAvcc(data: Uint8Array): Uint8Array {
  const nals = independentSplit(data).filter((n) => ![7, 8, 9].includes(n[0] & 0x1f));
  let size = 0;
  for (const n of nals) size += 4 + n.length;
  const out = new Uint8Array(size);
  let o = 0;
  for (const n of nals) {
    out[o++] = (n.length >>> 24) & 0xff;
    out[o++] = (n.length >>> 16) & 0xff;
    out[o++] = (n.length >>> 8) & 0xff;
    out[o++] = n.length & 0xff;
    out.set(n, o);
    o += n.length;
  }
  return out;
}

function independentSplit(data: Uint8Array): Uint8Array[] {
  const nals: Uint8Array[] = [];
  let start = -1;
  for (let i = 0; i + 2 < data.length; i++) {
    const sc3 = data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 1;
    const sc4 =
      i + 3 < data.length &&
      data[i] === 0 &&
      data[i + 1] === 0 &&
      data[i + 2] === 0 &&
      data[i + 3] === 1;
    if (sc3 || sc4) {
      if (start >= 0) nals.push(trimZeros(data.subarray(start, i)));
      start = i + (sc3 ? 3 : 4);
      i = start - 1;
    }
  }
  if (start >= 0) nals.push(trimZeros(data.subarray(start)));
  return nals.filter((n) => n.length > 0);
}

function trimZeros(nal: Uint8Array): Uint8Array {
  let end = nal.length;
  while (end > 0 && nal[end - 1] === 0) end--;
  return nal.subarray(0, end);
}

function fixtureSpsPps(): { sps: Uint8Array; pps: Uint8Array } {
  const nals = independentSplit(FIXTURE_FRAMES[0].data);
  const sps = nals.find((n) => (n[0] & 0x1f) === 7)!;
  const pps = nals.find((n) => (n[0] & 0x1f) === 8)!;
  return { sps, pps };
}

function avccFrame(i: number): MuxInputFrame {
  const f = FIXTURE_FRAMES[i];
  const { sps, pps } = fixtureSpsPps();
  return {
    keyframe: f.keyframe,
    timestampUs: BASE_TS_US + BigInt(i * FRAME_US),
    data: toAvcc(f.data),
    config: f.keyframe ? { codec: 'avc1.42E01F', extradata: buildAvcc(sps, pps) } : null,
  };
}

// A minimal MP4 box walker for structural assertions.
interface Box {
  type: string;
  start: number;
  size: number;
  children: Box[];
  body: Uint8Array;
}

const CONTAINERS = new Set(['moov', 'trak', 'mdia', 'minf', 'dinf', 'stbl', 'mvex', 'moof', 'traf']);

function parseBoxes(data: Uint8Array, start = 0, end = data.length): Box[] {
  const boxes: Box[] = [];
  let i = start;
  while (i + 8 <= end) {
    const size = (data[i] << 24) | (data[i + 1] << 16) | (data[i + 2] << 8) | data[i + 3];
    const type = String.fromCharCode(data[i + 4], data[i + 5], data[i + 6], data[i + 7]);
    if (size < 8 || i + size > end) throw new Error(`bad box ${type} size ${size} at ${i}`);
    const body = data.subarray(i + 8, i + size);
    const children = CONTAINERS.has(type) ? parseBoxes(data, i + 8, i + size) : [];
    boxes.push({ type, start: i, size, children, body });
    i += size;
  }
  return boxes;
}

function find(boxes: Box[], path: string): Box {
  const [head, ...rest] = path.split('.');
  const box = boxes.find((b) => b.type === head);
  if (!box) throw new Error(`box ${head} not found among [${boxes.map((b) => b.type).join(', ')}]`);
  return rest.length === 0 ? box : find(box.children, rest.join('.'));
}

function u32At(b: Uint8Array, off: number): number {
  return ((b[off] << 24) | (b[off + 1] << 16) | (b[off + 2] << 8) | b[off + 3]) >>> 0;
}

function u64At(b: Uint8Array, off: number): number {
  return u32At(b, off) * 0x100000000 + u32At(b, off + 4);
}

// Self-contained synchronous SHA-256 (FIPS 180-4): the app tsconfig carries
// no node types (so no node:crypto) and crypto.subtle is async. Verifiable
// against `shasum -a 256` on the same bytes.
function sha256(data: Uint8Array): string {
  const K = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
    0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
    0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
    0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
    0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
    0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
    0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
    0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
    0xc67178f2,
  ];
  const H = [
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
    0x5be0cd19,
  ];
  const rotr = (x: number, n: number) => (x >>> n) | (x << (32 - n));
  const bitLen = data.length * 8;
  const padded = new Uint8Array((((data.length + 8) >> 6) + 1) << 6);
  padded.set(data);
  padded[data.length] = 0x80;
  new DataView(padded.buffer).setUint32(padded.length - 4, bitLen >>> 0);
  new DataView(padded.buffer).setUint32(padded.length - 8, Math.floor(bitLen / 0x100000000));
  const w = new Array<number>(64);
  const view = new DataView(padded.buffer);
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }
    let [a, b, c, d, e, f, g, h] = H;
    for (let i = 0; i < 64; i++) {
      const S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
      const ch = (e & f) ^ (~e & g);
      const t1 = (h + S1 + ch + K[i] + w[i]) | 0;
      const S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (S0 + maj) | 0;
      h = g;
      g = f;
      f = e;
      e = (d + t1) | 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) | 0;
    }
    H[0] = (H[0] + a) | 0;
    H[1] = (H[1] + b) | 0;
    H[2] = (H[2] + c) | 0;
    H[3] = (H[3] + d) | 0;
    H[4] = (H[4] + e) | 0;
    H[5] = (H[5] + f) | 0;
    H[6] = (H[6] + g) | 0;
    H[7] = (H[7] + h) | 0;
  }
  return H.map((x) => (x >>> 0).toString(16).padStart(8, '0')).join('');
}

function muxAll(frames: MuxInputFrame[]): { segments: Fmp4Segment[]; muxer: Fmp4Muxer } {
  const muxer = new Fmp4Muxer();
  const segments = frames.flatMap((f) => muxer.push(f));
  return { segments, muxer };
}

const inits = (segs: Fmp4Segment[]) => segs.filter((s): s is Fmp4InitSegment => s.kind === 'init');
const medias = (segs: Fmp4Segment[]) =>
  segs.filter((s): s is Fmp4MediaSegment => s.kind === 'media');

// ---------------------------------------------------------------------------
// SPS parsing

describe('parseSps', () => {
  it('reads the fixture dimensions from the bitstream (docs/01: trust the bitstream)', () => {
    const { sps } = fixtureSpsPps();
    const info = parseSps(sps);
    expect(info.width).toBe(FIXTURE_WIDTH);
    expect(info.height).toBe(FIXTURE_HEIGHT);
  });

  // ffmpeg/x264-generated SPS at awkward geometries: 1080p high profile
  // (chroma-format branch + vertical cropping, 1088 → 1080), 852-wide main
  // (horizontal cropping), 720p constrained baseline (no crop).
  const CASES: Array<[string, string, number, number, number]> = [
    ['1080p high', '67640028acd940780227e5c044000003000400000300c83c60c658', 100, 1920, 1080],
    ['852x480 main', '674d401eeca06c1ef3f808800000030080000019078b16cb', 77, 852, 480],
    ['720p baseline', '6742c01fd9005005bb011000000300100000030320f1832480', 66, 1280, 720],
  ];
  for (const [name, hex, profile, width, height] of CASES) {
    it(`parses ${name}`, () => {
      const sps = Uint8Array.from(hex.match(/../g)!.map((h) => parseInt(h, 16)));
      const info = parseSps(sps);
      expect(info.profileIdc).toBe(profile);
      expect(info.width).toBe(width);
      expect(info.height).toBe(height);
    });
  }

  it('rejects a non-SPS NAL', () => {
    expect(() => parseSps(new Uint8Array([0x68, 0xce, 0x3c, 0x80]))).toThrow(/not an SPS/);
  });

  it('treats an over-long Exp-Golomb run as corruption instead of overflowing', () => {
    // NAL type 7 + profile/flags/level, then all-zero bits: an unbounded
    // leading-zero run in seq_parameter_set_id (1 << 31 would go negative).
    const sps = new Uint8Array(16);
    sps[0] = 0x67;
    sps[1] = 66;
    sps[3] = 30;
    expect(() => parseSps(sps)).toThrow(/Exp-Golomb|overrun/);
  });
});

// ---------------------------------------------------------------------------
// NAL plumbing

describe('NAL splitting', () => {
  it('splits the fixture keyframe into its NALs (3- and 4-byte start codes)', () => {
    const nals = splitAnnexB(FIXTURE_FRAMES[0].data);
    expect(nals.map(nalType)).toEqual(independentSplit(FIXTURE_FRAMES[0].data).map((n) => n[0] & 0x1f));
    // AUD, SPS, PPS, SEI, then IDR slices.
    expect(nals.map(nalType)).toEqual([9, 7, 8, 6, 5, 5, 5]);
  });

  it('round-trips AVCC splitting', () => {
    const avccData = toAvcc(FIXTURE_FRAMES[0].data);
    const nals = splitAvcc(avccData, 4);
    expect(nals.map(nalType)).toEqual([6, 5, 5, 5]);
  });

  it('throws on a truncated AVCC length', () => {
    expect(() => splitAvcc(new Uint8Array([0, 0, 0, 10, 1, 2]), 4)).toThrow(/out of bounds/);
  });
});

describe('avcC handling', () => {
  it('synthesizes a well-formed avcC from in-band SPS/PPS', () => {
    const { sps, pps } = fixtureSpsPps();
    const avcc = buildAvcc(sps, pps);
    const parsed = parseAvcc(avcc);
    expect(parsed.lengthSize).toBe(4);
    expect(Array.from(parsed.sps)).toEqual(Array.from(sps));
    expect(avcc[1]).toBe(sps[1]);
    expect(avcc[3]).toBe(sps[3]);
  });

  it('derives the codec string from the avcC profile bytes', () => {
    const { sps, pps } = fixtureSpsPps();
    const codec = codecFromAvcc(buildAvcc(sps, pps));
    expect(codec).toMatch(/^avc1\.[0-9A-F]{6}$/);
    // Profile byte round-trips: e.g. SPS profile 0x42 → 'avc1.42....'.
    expect(codec.slice(5, 7)).toBe(sps[1].toString(16).toUpperCase().padStart(2, '0'));
  });
});

// ---------------------------------------------------------------------------
// The muxer: Annex-B (native broadcaster) path

describe('Fmp4Muxer on the Annex-B fixture', () => {
  it('emits one init segment, then one media segment per frame', () => {
    const { segments, muxer } = muxAll(FIXTURE_FRAMES.map((_, i) => annexBFrame(i)));
    expect(inits(segments)).toHaveLength(1);
    expect(medias(segments)).toHaveLength(FIXTURE_FRAMES.length);
    // The second GOP's keyframe (frame 15) re-sends identical SPS/PPS — that
    // must NOT re-init (a per-GOP re-init would reset the decoder every 500 ms).
    expect(muxer.getStats()).toMatchObject({
      initSegments: 1,
      mediaSegments: FIXTURE_FRAMES.length,
      errors: 0,
      skippedAwaitingInit: 0,
    });
  });

  it('derives dimensions and codec from the bitstream, not the negotiated config', () => {
    const { segments } = muxAll([annexBFrame(0)]);
    const init = inits(segments)[0];
    expect(init.width).toBe(FIXTURE_WIDTH);
    expect(init.height).toBe(FIXTURE_HEIGHT);
    // The negotiated codec said 42E01F; the SPS is authoritative.
    const { sps } = fixtureSpsPps();
    expect(init.codec.slice(5, 7)).toBe(sps[1].toString(16).toUpperCase().padStart(2, '0'));
    expect(init.mime).toBe(h264Mime(init.codec));
  });

  it('builds a structurally valid init segment whose avcC matches the in-band parameter sets', () => {
    const { segments } = muxAll([annexBFrame(0)]);
    const init = inits(segments)[0];
    const boxes = parseBoxes(init.data);
    expect(boxes.map((b) => b.type)).toEqual(['ftyp', 'moov']);
    const moov = find(boxes, 'moov');
    expect(moov.children.map((b) => b.type)).toEqual(['mvhd', 'trak', 'mvex']);
    const stbl = find(boxes, 'moov.trak.mdia.minf.stbl');
    expect(stbl.children.map((b) => b.type)).toEqual(['stsd', 'stts', 'stsc', 'stsz', 'stco']);

    // stsd body: fullbox(4) + entry_count(4) + avc1 sample entry.
    const stsd = find(boxes, 'moov.trak.mdia.minf.stbl.stsd');
    expect(u32At(stsd.body, 4)).toBe(1);
    const avc1 = parseBoxes(stsd.body, 8, stsd.body.length)[0];
    expect(avc1.type).toBe('avc1');
    // width/height at offsets 24/26 of the sample entry body.
    expect((avc1.body[24] << 8) | avc1.body[25]).toBe(FIXTURE_WIDTH);
    expect((avc1.body[26] << 8) | avc1.body[27]).toBe(FIXTURE_HEIGHT);
    // The avcC box trails the fixed 78-byte entry: assert its bytes are the
    // synthesized record.
    const avcC = parseBoxes(avc1.body, 78, avc1.body.length)[0];
    expect(avcC.type).toBe('avcC');
    const { sps, pps } = fixtureSpsPps();
    expect(Array.from(avcC.body)).toEqual(Array.from(buildAvcc(sps, pps)));
  });

  it('emits fixed-layout one-sample fragments: moof 100 B, data offset 108, CTS == DTS', () => {
    const { segments } = muxAll(FIXTURE_FRAMES.map((_, i) => annexBFrame(i)));
    for (const [i, seg] of medias(segments).entries()) {
      const boxes = parseBoxes(seg.data);
      expect(boxes.map((b) => b.type)).toEqual(['moof', 'mdat']);
      const moof = find(boxes, 'moof');
      expect(moof.size).toBe(100);

      const mfhd = find(boxes, 'moof.mfhd');
      expect(u32At(mfhd.body, 4)).toBe(i + 1); // sequence numbers count from 1

      const trun = find(boxes, 'moof.traf.trun');
      const flags = u32At(trun.body, 0) & 0xffffff;
      // data-offset + duration + size + flags, and crucially NO composition
      // offset bit (0x800): CTS == DTS is structural (no B-frames).
      expect(flags).toBe(0x701);
      expect(u32At(trun.body, 4)).toBe(1); // sample_count
      expect(u32At(trun.body, 8)).toBe(108); // data_offset: moof + mdat header
      const sampleSize = u32At(trun.body, 16);
      const mdat = find(boxes, 'mdat');
      expect(mdat.body.length).toBe(sampleSize);

      // Sample flags: sync for keyframes, non-sync otherwise.
      expect(u32At(trun.body, 20)).toBe(FIXTURE_FRAMES[i].keyframe ? 0x02000000 : 0x01010000);
    }
  });

  it('zero-bases the timeline and advances baseMediaDecodeTime by the wire deltas', () => {
    const { segments } = muxAll(FIXTURE_FRAMES.map((_, i) => annexBFrame(i)));
    const times = medias(segments).map((seg) => {
      const tfdt = find(parseBoxes(seg.data), 'moof.traf.tfdt');
      expect(tfdt.body[0]).toBe(1); // version 1 → 64-bit
      return u64At(tfdt.body, 4);
    });
    expect(times).toEqual(FIXTURE_FRAMES.map((_, i) => i * FRAME_US));
  });

  it('strips AUD/SPS/PPS from samples and length-prefixes the rest', () => {
    const { segments } = muxAll([annexBFrame(0)]);
    const seg = medias(segments)[0];
    const mdat = find(parseBoxes(seg.data), 'mdat');
    const sampleNals = splitAvcc(mdat.body, 4);
    expect(sampleNals.map(nalType)).toEqual([6, 5, 5, 5]); // SEI + IDR slices
    // Byte-for-byte the fixture's own NALs.
    const source = independentSplit(FIXTURE_FRAMES[0].data).filter(
      (n) => ![7, 8, 9].includes(n[0] & 0x1f),
    );
    expect(sampleNals.map((n) => Array.from(n))).toEqual(source.map((n) => Array.from(n)));
  });

  it('is byte-stable (golden): the fixture produces pinned init and first-fragment hashes', () => {
    const { segments } = muxAll(FIXTURE_FRAMES.map((_, i) => annexBFrame(i)));
    const init = inits(segments)[0];
    const media = medias(segments);
    // Pinned goldens — an intentional format change must update these
    // consciously (and re-run the e2e Chrome MediaSource proof).
    expect(sha256(init.data)).toBe(
      'bcc5281fac2ff5750f725ad3a4c49b3b6caa74398eb704addc6f4f8bfd6d39d1',
    );
    expect(sha256(media[0].data)).toBe(
      '35b4ddd92b8ba387777ca06881fcf01c98ff042de73f8e9250ac370834aa5154',
    );
    expect(sha256(concat(media.map((m) => m.data)))).toBe(
      '6381e275a8eb6c32dc5093f46915febbc441633c569fd904c0a0db4c93f132e0',
    );
  });
});

function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

// ---------------------------------------------------------------------------
// The muxer: AVCC (browser broadcaster) path

describe('Fmp4Muxer on the AVCC shape', () => {
  it('produces byte-identical media segments to the Annex-B path', () => {
    const annexB = muxAll(FIXTURE_FRAMES.map((_, i) => annexBFrame(i)));
    const avcc = muxAll(FIXTURE_FRAMES.map((_, i) => avccFrame(i)));
    const a = medias(annexB.segments).map((s) => sha256(s.data));
    const b = medias(avcc.segments).map((s) => sha256(s.data));
    expect(b).toEqual(a);
    // And the same init: the synthesized avcC equals the supplied extradata.
    expect(sha256(inits(avcc.segments)[0].data)).toBe(sha256(inits(annexB.segments)[0].data));
  });

  it('does not misread an AVCC length prefix as an Annex-B start code', () => {
    // Fixture frame 16's first NAL is 461 bytes, so its 4-byte AVCC length
    // prefix is 00 00 01 CD — byte-identical to an Annex-B start code. A
    // per-frame sniff mangles exactly this frame (found by the equivalence
    // test below); the format decision is sticky from the keyframe instead.
    const muxer = new Fmp4Muxer();
    muxer.push(avccFrame(0));
    const seg = medias(muxer.push(avccFrame(16)))[0];
    expect(avccFrame(16).data.subarray(0, 3)).toEqual(new Uint8Array([0, 0, 1]));
    const mdat = find(parseBoxes(seg.data), 'mdat');
    expect(splitAvcc(mdat.body, 4).map(nalType)).toEqual([1, 1, 1]);
  });

  it('skips AVCC frames until a config-bearing keyframe arrives', () => {
    const muxer = new Fmp4Muxer();
    // A keyframe whose config is null (e.g. datagram-borne keyframe with the
    // config lost) cannot init the AVCC path.
    const out = muxer.push({ ...avccFrame(0), config: null });
    expect(out).toEqual([]);
    expect(muxer.getStats().skippedAwaitingInit).toBe(1);
    // The next config-bearing keyframe recovers.
    const out2 = muxer.push(avccFrame(0));
    expect(inits(out2)).toHaveLength(1);
    expect(medias(out2)).toHaveLength(1);
  });
});

// ---------------------------------------------------------------------------
// Edge behavior

describe('Fmp4Muxer edge behavior', () => {
  it('skips Annex-B deltas before the first keyframe', () => {
    const muxer = new Fmp4Muxer();
    const out = muxer.push(annexBFrame(1)); // a delta
    expect(out).toEqual([]);
    expect(muxer.getStats().skippedAwaitingInit).toBe(1);
    expect(muxer.push(annexBFrame(0)).length).toBe(2); // init + media
  });

  it('keeps baseMediaDecodeTime monotonic across a broadcaster restart (backwards timestamps)', () => {
    const muxer = new Fmp4Muxer();
    const segs: Fmp4Segment[] = [];
    segs.push(...muxer.push(annexBFrame(0)));
    segs.push(...muxer.push(annexBFrame(1)));
    // Restart: the new session's clock starts near zero — far behind.
    segs.push(
      ...muxer.push(annexBFrame(15, { timestampUs: 500n, keyframe: true })),
      ...muxer.push(annexBFrame(16, { timestampUs: 500n + BigInt(FRAME_US) })),
    );
    const times = medias(segs).map((seg) => u64At(find(parseBoxes(seg.data), 'moof.traf.tfdt').body, 4));
    expect(times[0]).toBe(0);
    expect(times[1]).toBe(FRAME_US);
    // Re-anchored: continues at one (observed) frame interval, never backwards.
    expect(times[2]).toBe(times[1] + FRAME_US);
    // And the new timeline's own deltas resume normally.
    expect(times[3]).toBe(times[2] + FRAME_US);
  });

  it('re-anchors on an absurd forward jump instead of encoding a freeze', () => {
    const muxer = new Fmp4Muxer();
    const segs: Fmp4Segment[] = [];
    segs.push(...muxer.push(annexBFrame(0)));
    // 60 s forward — a stall/clock discontinuity, not a frame interval.
    segs.push(...muxer.push(annexBFrame(1, { timestampUs: BASE_TS_US + 60_000_000n })));
    const times = medias(segs).map((seg) => u64At(find(parseBoxes(seg.data), 'moof.traf.tfdt').body, 4));
    expect(times[1]).toBe(times[0] + DEFAULT_FRAME_DURATION_US);
  });

  it('emits a fresh init segment when the parameter sets change mid-stream', () => {
    const muxer = new Fmp4Muxer();
    const first = muxer.push(avccFrame(0));
    expect(inits(first)).toHaveLength(1);

    // Same avcC again (GOP 2 keyframe): no re-init.
    const again = muxer.push({ ...avccFrame(15), timestampUs: BASE_TS_US + BigInt(15 * FRAME_US) });
    expect(inits(again)).toHaveLength(0);

    // A changed level byte = a different config (R13 codec pin / R4 step).
    const { sps, pps } = fixtureSpsPps();
    const sps2 = sps.slice();
    sps2[3] = 0x28; // level 4.0
    const changed = muxer.push({
      ...avccFrame(15),
      timestampUs: BASE_TS_US + BigInt(16 * FRAME_US),
      config: { codec: 'avc1.42E01F', extradata: buildAvcc(sps2, pps) },
      data: toAvcc(FIXTURE_FRAMES[15].data),
    });
    expect(inits(changed)).toHaveLength(1);
    expect(inits(changed)[0].codec.endsWith('28')).toBe(true);
  });

  it('never throws on malformed input — it counts an error and keeps going', () => {
    const muxer = new Fmp4Muxer();
    muxer.push(annexBFrame(0));
    const out = muxer.push({
      keyframe: false,
      timestampUs: BASE_TS_US + BigInt(FRAME_US),
      // AVCC-looking garbage with an out-of-bounds length.
      data: new Uint8Array([0x00, 0x01, 0xff, 0xff, 0x00]),
      config: null,
    });
    expect(out).toEqual([]);
    expect(muxer.getStats().errors).toBe(1);
    // The stream recovers on the next good frame.
    expect(medias(muxer.push(annexBFrame(1)))).toHaveLength(1);
  });
});
