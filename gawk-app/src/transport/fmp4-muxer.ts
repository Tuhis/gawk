// R22 (docs/27 Decision 3/4): the fMP4 muxer behind iPhone native fullscreen.
// Consumes the reorder buffer's release stream — the same in-order,
// freeze-on-gap-applied encoded frames the VideoDecoder eats — and emits fMP4
// segments for a ManagedMediaSource-backed <video>, which is the only media
// source the iOS native fullscreen player is known to present (the R16
// MediaStream tee was black; docs/21 U4). Pure and DOM-free: runs in the
// viewer worker, unit-tests in node, and is CI-provable in a desktop Chrome
// MediaSource (docs/27 Decision 10).
//
// Wire-format reality it must absorb (docs/27 Decision 4):
//   - Browser broadcaster: AVCC samples (length-prefixed NALs) + an avcC in
//     DecoderConfig.extradata.
//   - Native broadcaster (docs/19): Annex-B samples with EMPTY extradata and
//     in-band SPS/PPS at every IDR — the avcC is synthesized from those, and
//     visual dimensions come from the SPS itself (docs/01: trust the
//     bitstream, not metadata).
//   - No B-frames is a protocol invariant (docs/19), so CTS == DTS everywhere
//     and the trun carries no composition offsets.
//
// Output timeline: input timestamps are the broadcaster's performance.now()
// microseconds — huge, and they jump on a broadcaster restart. The muxer maps
// them onto its own zero-based, strictly monotonic output timeline
// (baseMediaDecodeTime must never move backwards, or the SourceBuffer stacks
// new media behind old); a backwards or absurd forward jump re-anchors at
// last output + one frame interval, so a restart plays through as a seamless
// continuation.

import { log } from '../lib/logger';
import type { DecoderConfigMessage } from './wire';
import { normalizeAvccExtradata } from './avcc';

// The shape the reorder buffer releases (structurally ReleasedFrame from
// reorder-buffer.ts, minus frameId which the muxer has no use for).
export interface MuxInputFrame {
  keyframe: boolean;
  timestampUs: bigint;
  data: Uint8Array;
  config: DecoderConfigMessage | null;
}

// R22 audio (docs/27 finding 2): video and audio ride SEPARATE SourceBuffers on
// one MediaSource — the standard MSE shape, and the one that keeps the working
// video path untouched when audio is absent, unsupported, or dies mid-stream.
export type Fmp4Track = 'video' | 'audio';

export interface Fmp4InitSegment {
  kind: 'init';
  track: Fmp4Track;
  codec: string; // e.g. 'avc1.42C01E' / 'opus', derived from the bitstream/config
  mime: string; // video/mp4; codecs="..." | audio/mp4; codecs="opus"
  width: number; // 0 on the audio track
  height: number; // 0 on the audio track
  data: Uint8Array;
}

export interface Fmp4MediaSegment {
  kind: 'media';
  track: Fmp4Track;
  // Video: a sync sample. Audio samples are all sync samples, so the flag is
  // always true there — the presenter's resync-at-keyframe policy is a no-op
  // for audio, which is correct: any Opus packet is a decodable restart point.
  keyframe: boolean;
  data: Uint8Array;
}

export type Fmp4Segment = Fmp4InitSegment | Fmp4MediaSegment;

// The R15 audio lane's config, as the muxer needs it (docs/20 wire type 0x08).
// `description` carries the codec's out-of-band setup bytes where the format
// needs them: unused for Opus (dOps is built from the fields above), required
// for AAC — the AudioSpecificConfig that goes inside `esds`, taken verbatim from
// the encoder's own `decoderConfig.description` rather than synthesized.
export interface AudioMuxConfig {
  codec: string;
  sampleRate: number;
  channels: number;
  description?: Uint8Array;
}

// Which audio encapsulation the muxer is producing. Opus is the R15 lane
// muxed verbatim (no transcode); AAC is the iOS path — iOS refuses
// `audio/mp4; codecs="opus"` through ManagedMediaSource (docs/27 finding 4), so
// the decoded PCM is re-encoded to AAC, which Apple's own HLS mandates.
export type AudioMuxCodec = 'opus' | 'aac';

export function aacMime(codec: string): string {
  return `audio/mp4; codecs="${codec}"`;
}

// AAC-LC frames are 1024 samples, not Opus's 960 — the nominal fallback duration
// when a sample has no successor to measure against.
export const AAC_FRAME_SAMPLES = 1024;

export interface AudioMuxInput {
  timestampUs: bigint;
  data: Uint8Array; // exactly one Opus packet (docs/20: one packet per datagram)
}

// What the pipeline forks to the muxer: the encoded audio lane in wire order.
// Lives here (not in the worker core) so `viewer.ts` can name it without a
// type cycle through the core.
export type AudioTapEvent =
  | { kind: 'config'; config: AudioMuxConfig }
  | { kind: 'packet'; packet: AudioMuxInput };

export function h264Mime(codec: string): string {
  return `video/mp4; codecs="${codec}"`;
}

// Opus-in-MP4 (RFC 7845 §4 encapsulation, 'Opus' sample entry + dOps). Whether
// iOS accepts it is a runtime question — isTypeSupported decides, and a refusal
// simply leaves the native player video-only (probeMseAudio in
// features/viewer/msePresentation.ts).
export function opusMime(): string {
  return 'audio/mp4; codecs="opus"';
}

// One movie timescale for everything: microseconds, so wire timestamps map
// with no rounding. (32-bit mdhd timescale holds 1e6 fine.)
export const MOVIE_TIMESCALE = 1_000_000;

// Fallback per-sample duration before any inter-frame delta is observed, and
// the step used when re-anchoring across a restart. 30 fps — the fleet's
// default fan-out cadence (docs/08).
export const DEFAULT_FRAME_DURATION_US = 33_333;

// Opus is always 48 kHz out, and the audio track's timescale IS its sample rate
// so every duration is an exact sample count (docs/20: 20 ms frames = 960).
export const OPUS_FRAME_MS = 20;

// A live MSE audio track must be hole-free: HTMLMediaElement.buffered is the
// INTERSECTION of the SourceBuffers' ranges, so a hole in audio is a hole in
// playback — it stalls the video the native player is showing. A lost datagram
// (or two) is therefore absorbed by stretching the preceding sample's declared
// duration to cover the gap: the range stays continuous and the audio renderer
// pads the missing content. Beyond this bound the loss is a real outage, not
// jitter — the sample is declared honestly and the hole is counted.
export const AUDIO_MAX_STRETCH_MS = 1000;

// An input timestamp jumping backwards, or forward by more than this, is a
// timeline discontinuity (broadcaster restart / clock change), not a frame
// interval — re-anchor instead of encoding a multi-second freeze into the
// output timeline. Same 5 s judgement as CADENCE_MAX_INTERVAL_MS.
export const RESTART_JUMP_US = 5_000_000;

const EMPTY = new Uint8Array(0);

// The fixed one-sample fragment layout (see buildMediaSegment): moof is a
// constant 100 bytes, so the trun data_offset — from moof start to the first
// sample byte inside mdat — is a compile-time constant. Pinned by tests.
const MOOF_SIZE = 100;
const TRUN_DATA_OFFSET = MOOF_SIZE + 8;

// ---------------------------------------------------------------------------
// NAL plumbing

const NAL_SPS = 7;
const NAL_PPS = 8;
const NAL_AUD = 9;

// Split an Annex-B stream into NAL unit payloads (views, no copy). Handles
// both 3- and 4-byte start codes.
export function splitAnnexB(data: Uint8Array): Uint8Array[] {
  const nals: Uint8Array[] = [];
  let i = 0;
  let start = -1;
  const n = data.length;
  while (i + 2 < n) {
    if (data[i] === 0 && data[i + 1] === 0) {
      let scLen = 0;
      if (data[i + 2] === 1) scLen = 3;
      else if (i + 3 < n && data[i + 2] === 0 && data[i + 3] === 1) scLen = 4;
      if (scLen > 0) {
        if (start >= 0) nals.push(data.subarray(start, i));
        start = i + scLen;
        i = start;
        continue;
      }
    }
    i++;
  }
  if (start >= 0 && start < n) nals.push(data.subarray(start, n));
  // Trailing zero bytes after a NAL are padding, not payload.
  return nals.map(trimTrailingZeros).filter((nal) => nal.length > 0);
}

function trimTrailingZeros(nal: Uint8Array): Uint8Array {
  let end = nal.length;
  while (end > 0 && nal[end - 1] === 0) end--;
  return nal.subarray(0, end);
}

// Split an AVCC (length-prefixed) sample into NAL payloads. Throws on a
// malformed length so the caller can count a mux error instead of emitting a
// corrupt sample.
export function splitAvcc(data: Uint8Array, lengthSize: number): Uint8Array[] {
  const nals: Uint8Array[] = [];
  let i = 0;
  while (i < data.length) {
    if (i + lengthSize > data.length) throw new Error('truncated AVCC length prefix');
    let len = 0;
    for (let j = 0; j < lengthSize; j++) len = (len << 8) | data[i + j];
    i += lengthSize;
    if (len < 0 || i + len > data.length) throw new Error('AVCC NAL length out of bounds');
    nals.push(data.subarray(i, i + len));
    i += len;
  }
  return nals;
}

export function nalType(nal: Uint8Array): number {
  return nal.length > 0 ? nal[0] & 0x1f : 0;
}

export function startsWithStartCode(data: Uint8Array): boolean {
  return (
    data.length >= 4 &&
    data[0] === 0x00 &&
    data[1] === 0x00 &&
    (data[2] === 0x01 || (data[2] === 0x00 && data[3] === 0x01))
  );
}

// ---------------------------------------------------------------------------
// avcC handling

export interface AvccInfo {
  avcc: Uint8Array;
  lengthSize: number;
  sps: Uint8Array; // first SPS
}

// Parse an avcC record far enough for what the muxer needs: the NAL length
// size and the first SPS (dimensions + codec string).
export function parseAvcc(avcc: Uint8Array): AvccInfo {
  if (avcc.length < 7 || avcc[0] !== 0x01) throw new Error('not an avcC record');
  const lengthSize = (avcc[4] & 0x03) + 1;
  const numSps = avcc[5] & 0x1f;
  if (numSps < 1) throw new Error('avcC carries no SPS');
  const spsLen = (avcc[6] << 8) | avcc[7];
  if (8 + spsLen > avcc.length) throw new Error('avcC SPS out of bounds');
  return { avcc, lengthSize, sps: avcc.subarray(8, 8 + spsLen) };
}

// Synthesize an avcC from in-band SPS/PPS (the native broadcaster's empty-
// extradata Annex-B path). 4-byte NAL lengths, matching the samples we write.
export function buildAvcc(sps: Uint8Array, pps: Uint8Array): Uint8Array {
  const out = new Uint8Array(11 + sps.length + pps.length);
  out[0] = 0x01;
  out[1] = sps[1]; // profile_idc
  out[2] = sps[2]; // constraint flags
  out[3] = sps[3]; // level_idc
  out[4] = 0xff; // reserved + lengthSizeMinusOne = 3
  out[5] = 0xe1; // reserved + 1 SPS
  out[6] = (sps.length >> 8) & 0xff;
  out[7] = sps.length & 0xff;
  out.set(sps, 8);
  let o = 8 + sps.length;
  out[o++] = 0x01; // 1 PPS
  out[o++] = (pps.length >> 8) & 0xff;
  out[o++] = pps.length & 0xff;
  out.set(pps, o);
  return out;
}

// The RFC 6381 codec string, from the avcC's profile/compat/level bytes —
// the same derivation viewer.ts uses when extradata and the negotiated codec
// disagree (trust the bitstream).
export function codecFromAvcc(avcc: Uint8Array): string {
  const hex = (b: number) => b.toString(16).toUpperCase().padStart(2, '0');
  return `avc1.${hex(avcc[1])}${hex(avcc[2])}${hex(avcc[3])}`;
}

// ---------------------------------------------------------------------------
// SPS parsing (visual dimensions)

class BitReader {
  private data: Uint8Array;
  private bytePos = 0;
  private bitPos = 0;

  constructor(data: Uint8Array) {
    this.data = data;
  }

  u1(): number {
    if (this.bytePos >= this.data.length) throw new Error('SPS bit reader overrun');
    const bit = (this.data[this.bytePos] >> (7 - this.bitPos)) & 1;
    this.bitPos++;
    if (this.bitPos === 8) {
      this.bitPos = 0;
      this.bytePos++;
    }
    return bit;
  }

  u(n: number): number {
    let v = 0;
    for (let i = 0; i < n; i++) v = (v << 1) | this.u1();
    return v;
  }

  // Exp-Golomb unsigned. Values past 30 leading zeros would overflow the
  // 32-bit shift below (1 << 31 is negative in JS) — no field a real SPS
  // carries gets near that, so treat it as corruption, not data.
  ue(): number {
    let zeros = 0;
    while (this.u1() === 0) {
      zeros++;
      if (zeros > 30) throw new Error('invalid Exp-Golomb code');
    }
    return (1 << zeros) - 1 + this.u(zeros);
  }

  // Exp-Golomb signed.
  se(): number {
    const k = this.ue();
    return k % 2 === 0 ? -(k / 2) : (k + 1) / 2;
  }
}

// Strip emulation-prevention bytes (00 00 03 xx → 00 00 xx) to get the RBSP.
function ebspToRbsp(nal: Uint8Array): Uint8Array {
  const out = new Uint8Array(nal.length);
  let o = 0;
  for (let i = 0; i < nal.length; i++) {
    if (i >= 2 && nal[i] === 0x03 && nal[i - 1] === 0x00 && nal[i - 2] === 0x00) continue;
    out[o++] = nal[i];
  }
  return out.subarray(0, o);
}

function skipScalingList(r: BitReader, size: number): void {
  let lastScale = 8;
  let nextScale = 8;
  for (let i = 0; i < size; i++) {
    if (nextScale !== 0) nextScale = (lastScale + r.se() + 256) % 256;
    lastScale = nextScale === 0 ? lastScale : nextScale;
  }
}

export interface SpsInfo {
  profileIdc: number;
  levelIdc: number;
  width: number;
  height: number;
}

// Minimal H.264 SPS parse: everything up to the frame cropping, per
// ISO/IEC 14496-10 §7.3.2.1.1. Only the fields ahead of the dimensions are
// walked; VUI is not needed.
export function parseSps(spsNal: Uint8Array): SpsInfo {
  if (nalType(spsNal) !== NAL_SPS) throw new Error('not an SPS NAL');
  const r = new BitReader(ebspToRbsp(spsNal.subarray(1)));
  const profileIdc = r.u(8);
  r.u(8); // constraint flags + reserved
  const levelIdc = r.u(8);
  r.ue(); // seq_parameter_set_id

  let chromaFormatIdc = 1; // default 4:2:0 for non-high profiles
  if (
    [100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135].includes(profileIdc)
  ) {
    chromaFormatIdc = r.ue();
    if (chromaFormatIdc === 3) r.u1(); // separate_colour_plane_flag
    r.ue(); // bit_depth_luma_minus8
    r.ue(); // bit_depth_chroma_minus8
    r.u1(); // qpprime_y_zero_transform_bypass_flag
    if (r.u1() === 1) {
      // seq_scaling_matrix_present_flag
      const lists = chromaFormatIdc === 3 ? 12 : 8;
      for (let i = 0; i < lists; i++) {
        if (r.u1() === 1) skipScalingList(r, i < 6 ? 16 : 64);
      }
    }
  }

  r.ue(); // log2_max_frame_num_minus4
  const pocType = r.ue();
  if (pocType === 0) {
    r.ue(); // log2_max_pic_order_cnt_lsb_minus4
  } else if (pocType === 1) {
    r.u1(); // delta_pic_order_always_zero_flag
    r.se(); // offset_for_non_ref_pic
    r.se(); // offset_for_top_to_bottom_field
    const cycle = r.ue();
    for (let i = 0; i < cycle; i++) r.se();
  }
  r.ue(); // max_num_ref_frames
  r.u1(); // gaps_in_frame_num_value_allowed_flag

  const picWidthInMbs = r.ue() + 1;
  const picHeightInMapUnits = r.ue() + 1;
  const frameMbsOnly = r.u1();
  if (frameMbsOnly === 0) r.u1(); // mb_adaptive_frame_field_flag
  r.u1(); // direct_8x8_inference_flag

  let cropLeft = 0;
  let cropRight = 0;
  let cropTop = 0;
  let cropBottom = 0;
  if (r.u1() === 1) {
    cropLeft = r.ue();
    cropRight = r.ue();
    cropTop = r.ue();
    cropBottom = r.ue();
  }

  // Crop units per §7.4.2.1.1: chroma 4:2:0 halves both axes, 4:2:2 halves X
  // only, 4:4:4 and monochrome crop in luma samples.
  const subWidthC = chromaFormatIdc === 1 || chromaFormatIdc === 2 ? 2 : 1;
  const subHeightC = chromaFormatIdc === 1 ? 2 : 1;
  const cropUnitX = chromaFormatIdc === 0 ? 1 : subWidthC;
  const cropUnitY = (chromaFormatIdc === 0 ? 1 : subHeightC) * (2 - frameMbsOnly);

  const width = picWidthInMbs * 16 - cropUnitX * (cropLeft + cropRight);
  const height = (2 - frameMbsOnly) * picHeightInMapUnits * 16 - cropUnitY * (cropTop + cropBottom);
  return { profileIdc, levelIdc, width, height };
}

// ---------------------------------------------------------------------------
// Box writing

class BoxWriter {
  private buf = new Uint8Array(1024);
  private len = 0;
  private stack: number[] = [];

  private ensure(n: number): void {
    if (this.len + n <= this.buf.length) return;
    let cap = this.buf.length * 2;
    while (cap < this.len + n) cap *= 2;
    const next = new Uint8Array(cap);
    next.set(this.buf.subarray(0, this.len));
    this.buf = next;
  }

  u8(v: number): void {
    this.ensure(1);
    this.buf[this.len++] = v & 0xff;
  }

  u16(v: number): void {
    this.u8(v >> 8);
    this.u8(v);
  }

  u32(v: number): void {
    this.u16(v >>> 16);
    this.u16(v);
  }

  u64(v: number): void {
    // Timestamps in µs stay far under 2^53; split into 32-bit halves.
    this.u32(Math.floor(v / 0x100000000));
    this.u32(v >>> 0);
  }

  bytes(b: Uint8Array): void {
    this.ensure(b.length);
    this.buf.set(b, this.len);
    this.len += b.length;
  }

  ascii(s: string): void {
    this.ensure(s.length);
    for (let i = 0; i < s.length; i++) this.buf[this.len++] = s.charCodeAt(i);
  }

  zeros(n: number): void {
    this.ensure(n);
    this.len += n; // Uint8Array is zero-initialized (and never shrunk)
  }

  box(type: string, body: () => void): void {
    const sizeAt = this.len;
    this.u32(0); // backpatched
    this.ascii(type);
    this.stack.push(sizeAt);
    body();
    const start = this.stack.pop()!;
    const size = this.len - start;
    this.buf[start] = (size >>> 24) & 0xff;
    this.buf[start + 1] = (size >>> 16) & 0xff;
    this.buf[start + 2] = (size >>> 8) & 0xff;
    this.buf[start + 3] = size & 0xff;
  }

  fullBox(type: string, version: number, flags: number, body: () => void): void {
    this.box(type, () => {
      this.u8(version);
      this.u8((flags >>> 16) & 0xff);
      this.u8((flags >>> 8) & 0xff);
      this.u8(flags & 0xff);
      body();
    });
  }

  // Current write offset, and a single-byte backpatch — the ISO 14496-1
  // descriptor sizes inside `esds` are not box sizes, so `box()` can't do it.
  mark(): number {
    return this.len;
  }

  patchU8(at: number, v: number): void {
    this.buf[at] = v & 0xff;
  }

  take(): Uint8Array {
    return this.buf.slice(0, this.len);
  }
}

// The identity matrix in the 16.16 fixed-point layout mvhd/tkhd want.
function writeUnityMatrix(w: BoxWriter): void {
  w.u32(0x00010000);
  w.u32(0);
  w.u32(0);
  w.u32(0);
  w.u32(0x00010000);
  w.u32(0);
  w.u32(0);
  w.u32(0);
  w.u32(0x40000000);
}

export function buildInitSegment(avcc: Uint8Array, width: number, height: number): Uint8Array {
  const w = new BoxWriter();

  w.box('ftyp', () => {
    w.ascii('isom');
    w.u32(0x200);
    w.ascii('isom');
    w.ascii('iso5');
    w.ascii('avc1');
    w.ascii('mp41');
  });

  w.box('moov', () => {
    w.fullBox('mvhd', 0, 0, () => {
      w.u32(0); // creation_time
      w.u32(0); // modification_time
      w.u32(1000); // movie timescale (presentation-level; track has its own)
      w.u32(0); // duration: unknown/live
      w.u32(0x00010000); // rate 1.0
      w.u16(0x0100); // volume 1.0
      w.u16(0);
      w.u32(0);
      w.u32(0);
      writeUnityMatrix(w);
      w.zeros(24); // pre_defined
      w.u32(2); // next_track_ID
    });

    w.box('trak', () => {
      w.fullBox('tkhd', 0, 0x3, () => {
        // enabled + in-movie
        w.u32(0);
        w.u32(0);
        w.u32(1); // track_ID
        w.u32(0);
        w.u32(0); // duration
        w.u32(0);
        w.u32(0);
        w.u16(0); // layer
        w.u16(0); // alternate_group
        w.u16(0); // volume (video)
        w.u16(0);
        writeUnityMatrix(w);
        w.u32(width << 16); // 16.16 fixed
        w.u32(height << 16);
      });

      w.box('mdia', () => {
        w.fullBox('mdhd', 0, 0, () => {
          w.u32(0);
          w.u32(0);
          w.u32(MOVIE_TIMESCALE);
          w.u32(0); // duration
          w.u16(0x55c4); // language 'und'
          w.u16(0);
        });
        w.fullBox('hdlr', 0, 0, () => {
          w.u32(0);
          w.ascii('vide');
          w.zeros(12);
          w.ascii('gawk');
          w.u8(0);
        });
        w.box('minf', () => {
          w.fullBox('vmhd', 0, 1, () => {
            w.u16(0); // graphicsmode
            w.zeros(6); // opcolor
          });
          w.box('dinf', () => {
            w.fullBox('dref', 0, 0, () => {
              w.u32(1);
              w.fullBox('url ', 0, 1, () => {}); // self-contained
            });
          });
          w.box('stbl', () => {
            w.fullBox('stsd', 0, 0, () => {
              w.u32(1);
              w.box('avc1', () => {
                w.zeros(6); // reserved
                w.u16(1); // data_reference_index
                w.u16(0); // pre_defined
                w.u16(0); // reserved
                w.zeros(12); // pre_defined
                w.u16(width);
                w.u16(height);
                w.u32(0x00480000); // 72 dpi
                w.u32(0x00480000);
                w.u32(0);
                w.u16(1); // frame_count
                w.zeros(32); // compressorname
                w.u16(0x0018); // depth
                w.u16(0xffff); // pre_defined (-1)
                w.box('avcC', () => w.bytes(avcc));
              });
            });
            w.fullBox('stts', 0, 0, () => w.u32(0));
            w.fullBox('stsc', 0, 0, () => w.u32(0));
            w.fullBox('stsz', 0, 0, () => {
              w.u32(0);
              w.u32(0);
            });
            w.fullBox('stco', 0, 0, () => w.u32(0));
          });
        });
      });
    });

    w.box('mvex', () => {
      w.fullBox('trex', 0, 0, () => {
        w.u32(1); // track_ID
        w.u32(1); // default_sample_description_index
        w.u32(0);
        w.u32(0);
        w.u32(0);
      });
    });
  });

  return w.take();
}

// ISO 14496-1 descriptor header: tag + a variable-length size (7 bits/byte).
// Every descriptor here is far under 128 bytes, so one size byte is enough —
// asserted rather than assumed, because a silent truncation would produce an
// init segment that parses as garbage.
function descriptor(w: BoxWriter, tag: number, body: () => void): void {
  w.u8(tag);
  const sizeAt = w.mark();
  w.u8(0); // backpatched
  body();
  const size = w.mark() - sizeAt - 1;
  if (size > 0x7f) throw new Error(`descriptor ${tag} too large for a 1-byte size: ${size}`);
  w.patchU8(sizeAt, size);
}

// The AAC sample entry: `mp4a` + `esds` carrying the encoder's own
// AudioSpecificConfig. Apple's mandated HLS audio codec, and the iOS path for
// R22 audio (docs/27 finding 4).
function writeMp4aEntry(w: BoxWriter, cfg: AudioMuxConfig): void {
  const asc = cfg.description;
  if (!asc || asc.length === 0) throw new Error('AAC needs an AudioSpecificConfig description');
  // docs/27 finding 6: an ES_Descriptor (tag 0x03) or DecoderConfigDescriptor
  // (tag 0x04) is what Safari's AudioEncoder hands back as `description`;
  // audio-transcode.ts unwraps it to the ASC. If an un-normalized one gets here,
  // nesting it inside our own DecoderSpecificInfo yields an init segment WebKit
  // rejects with MEDIA_ERR_SRC_NOT_SUPPORTED — which closes the MediaSource
  // rather than failing visibly. An ASC's first byte carries the 5-bit
  // audioObjectType in its high bits, so it can never be 0x03/0x04 (AOT 0 is
  // "NULL" and never encoded): refuse those outright.
  if (asc[0] === 0x03 || asc[0] === 0x04) {
    throw new Error(`AAC description is a descriptor (tag 0x0${asc[0]}), not an AudioSpecificConfig`);
  }
  w.box('mp4a', () => {
    w.zeros(6); // reserved
    w.u16(1); // data_reference_index
    w.u32(0); // version + revision
    w.u32(0); // vendor
    w.u16(cfg.channels);
    w.u16(16); // samplesize
    w.u16(0); // pre_defined
    w.u16(0); // reserved
    w.u32(cfg.sampleRate * 0x10000); // 16.16 fixed
    w.fullBox('esds', 0, 0, () => {
      descriptor(w, 0x03, () => {
        // ES_Descriptor
        w.u16(1); // ES_ID
        w.u8(0); // no stream dependency / URL / OCR, priority 0
        descriptor(w, 0x04, () => {
          // DecoderConfigDescriptor
          w.u8(0x40); // objectTypeIndication: Audio ISO/IEC 14496-3
          w.u8(0x15); // streamType 0x05 (audio) << 2, upStream 0, reserved 1
          w.u8(0); // bufferSizeDB (24-bit)
          w.u16(0);
          w.u32(0); // maxBitrate — unknown for a live re-encode
          w.u32(0); // avgBitrate
          descriptor(w, 0x05, () => w.bytes(asc)); // DecoderSpecificInfo
        });
        descriptor(w, 0x06, () => w.u8(0x02)); // SLConfigDescriptor: MP4 default
      });
    });
  });
}

// The Opus sample entry: 'Opus' + dOps (RFC 7845 §5.1).
function writeOpusEntry(w: BoxWriter, cfg: AudioMuxConfig): void {
  w.box('Opus', () => {
    w.zeros(6); // reserved
    w.u16(1); // data_reference_index
    w.u32(0); // version + revision
    w.u32(0); // vendor
    w.u16(cfg.channels);
    w.u16(16); // samplesize
    w.u16(0); // pre_defined
    w.u16(0); // reserved
    // 16.16 fixed. Multiplication, not `<< 16`: 48000 << 16 overflows int32 in
    // JS (the bytes come out right either way, but the expression should not
    // read as a negative number).
    w.u32(cfg.sampleRate * 0x10000);
    // OpusSpecificBox — a plain box carrying its own version byte, NOT a fullBox.
    w.box('dOps', () => {
      w.u8(0); // Version
      w.u8(cfg.channels); // OutputChannelCount
      // PreSkip 0: WebCodecs exposes no encoder delay (docs/20 leaves the
      // AudioDecoder description empty for the same reason), so there is nothing
      // honest to declare. The cost is the encoder's ~6.5 ms ramp-up being
      // audible-in-principle at stream start — an order below the 60 ms A/V
      // skew target.
      w.u16(0);
      w.u32(cfg.sampleRate); // InputSampleRate (informational)
      w.u16(0); // OutputGain (Q7.8, 0 dB)
      w.u8(0); // ChannelMappingFamily: mono/stereo
    });
  });
}

// The audio counterpart of buildInitSegment: one audio track, timescale ==
// sample rate. Deliberately a sibling rather than a parameterization of the
// video builder — the video moov is pinned by golden vectors and must not move.
export function buildAudioInitSegment(cfg: AudioMuxConfig, codec: AudioMuxCodec): Uint8Array {
  const w = new BoxWriter();

  w.box('ftyp', () => {
    w.ascii('isom');
    w.u32(0x200);
    w.ascii('isom');
    w.ascii('iso5');
    w.ascii('mp41');
    if (codec === 'opus') w.ascii('opus');
  });

  w.box('moov', () => {
    w.fullBox('mvhd', 0, 0, () => {
      w.u32(0);
      w.u32(0);
      w.u32(1000);
      w.u32(0); // duration: unknown/live
      w.u32(0x00010000);
      w.u16(0x0100);
      w.u16(0);
      w.u32(0);
      w.u32(0);
      writeUnityMatrix(w);
      w.zeros(24);
      w.u32(2); // next_track_ID
    });

    w.box('trak', () => {
      w.fullBox('tkhd', 0, 0x3, () => {
        w.u32(0);
        w.u32(0);
        w.u32(1); // track_ID — its own file, so 1 again (not the video's 2)
        w.u32(0);
        w.u32(0); // duration
        w.u32(0);
        w.u32(0);
        w.u16(0); // layer
        w.u16(1); // alternate_group: audio
        w.u16(0x0100); // volume 1.0
        w.u16(0);
        writeUnityMatrix(w);
        w.u32(0); // width/height: none
        w.u32(0);
      });

      w.box('mdia', () => {
        w.fullBox('mdhd', 0, 0, () => {
          w.u32(0);
          w.u32(0);
          // Timescale == sample rate: durations are exact sample counts, so the
          // track can never accumulate rounding drift against the video.
          w.u32(cfg.sampleRate);
          w.u32(0); // duration
          w.u16(0x55c4); // 'und'
          w.u16(0);
        });
        w.fullBox('hdlr', 0, 0, () => {
          w.u32(0);
          w.ascii('soun');
          w.zeros(12);
          w.ascii('gawk');
          w.u8(0);
        });
        w.box('minf', () => {
          w.fullBox('smhd', 0, 0, () => {
            w.u16(0); // balance
            w.u16(0); // reserved
          });
          w.box('dinf', () => {
            w.fullBox('dref', 0, 0, () => {
              w.u32(1);
              w.fullBox('url ', 0, 1, () => {});
            });
          });
          w.box('stbl', () => {
            w.fullBox('stsd', 0, 0, () => {
              w.u32(1);
              // AudioSampleEntry: 28 bytes of common fields, then the
              // codec-specific setup box.
              if (codec === 'aac') writeMp4aEntry(w, cfg);
              else writeOpusEntry(w, cfg);
            });
            w.fullBox('stts', 0, 0, () => w.u32(0));
            w.fullBox('stsc', 0, 0, () => w.u32(0));
            w.fullBox('stsz', 0, 0, () => {
              w.u32(0);
              w.u32(0);
            });
            w.fullBox('stco', 0, 0, () => w.u32(0));
          });
        });
      });
    });

    w.box('mvex', () => {
      w.fullBox('trex', 0, 0, () => {
        w.u32(1); // track_ID
        w.u32(1); // default_sample_description_index
        w.u32(0);
        w.u32(0);
        w.u32(0);
      });
    });
  });

  return w.take();
}

// One-sample moof+mdat. The layout is fixed (one track, one sample, explicit
// duration/size/flags), which is what makes MOOF_SIZE/TRUN_DATA_OFFSET
// constants — pinned by tests so a box edit can't silently break the offset.
// `decodeTime`/`duration` are in the TRACK's timescale: microseconds for video
// (MOVIE_TIMESCALE), sample counts for audio.
export function buildMediaSegment(
  sample: Uint8Array,
  opts: { sequence: number; decodeTime: number; duration: number; keyframe: boolean },
): Uint8Array {
  const w = new BoxWriter();
  w.box('moof', () => {
    w.fullBox('mfhd', 0, 0, () => w.u32(opts.sequence));
    w.box('traf', () => {
      // default-base-is-moof: sample data offsets are relative to moof start.
      w.fullBox('tfhd', 0, 0x020000, () => w.u32(1));
      w.fullBox('tfdt', 1, 0, () => w.u64(opts.decodeTime));
      // data-offset + per-sample duration/size/flags; no composition offset —
      // CTS == DTS is the no-B-frames invariant on the wire.
      w.fullBox('trun', 0, 0x000701, () => {
        w.u32(1); // sample_count
        w.u32(TRUN_DATA_OFFSET);
        w.u32(opts.duration);
        w.u32(sample.length);
        // sample_flags: sync sample (depends on nothing) vs non-sync.
        w.u32(opts.keyframe ? 0x02000000 : 0x01010000);
      });
    });
  });
  w.box('mdat', () => w.bytes(sample));
  return w.take();
}

// ---------------------------------------------------------------------------
// The muxer

export interface Fmp4MuxerStats {
  initSegments: number;
  mediaSegments: number;
  // Frames skipped before the first keyframe made an init segment possible.
  skippedAwaitingInit: number;
  errors: number;
  // R22 audio. audioSkipped counts packets that could not be placed on the
  // output timeline (no video anchor yet, no config, or a non-Opus codec);
  // audioHoles counts gaps too long to absorb by stretching, i.e. the ones that
  // do reach the buffered ranges.
  audioInitSegments: number;
  audioSegments: number;
  audioSkipped: number;
  audioHoles: number;
}

export class Fmp4Muxer {
  private avcc: Uint8Array | null = null;
  private codec: string | null = null;
  // The wire format, decided at keyframes and sticky between them. A per-frame
  // sniff is NOT safe here: an AVCC 4-byte length prefix for a 256–511-byte
  // NAL is 00 00 01 xx — byte-identical to an Annex-B start code (the fixture's
  // frame 16 is exactly that). Keyframes are unambiguous — an avcC-bearing
  // config means AVCC, in-band SPS/PPS behind a real start code means Annex-B —
  // so the decision is made there and deltas inherit it.
  private format: 'annexb' | 'avcc' | null = null;

  private sequence = 0;
  private prevInputUs: number | null = null;
  private prevOutputUs = 0;
  private lastDurationUs = DEFAULT_FRAME_DURATION_US;
  // One frame of lookahead (docs/27 finding 3). A sample's declared duration
  // must be the interval to its SUCCESSOR, which is only knowable once the
  // successor arrives — so the newest frame is held here until then.
  private pendingVideo: { outputUs: number; keyframe: boolean; sample: Uint8Array } | null = null;

  // The video path's input→output shift, republished on every frame (including
  // each re-anchor). This is what keeps audio in sync: both media carry
  // timestamps on the same broadcaster performance.now() clock (docs/20's
  // load-bearing sync decision), so one shared offset puts both tracks on one
  // output timeline and relative A/V skew is zero by construction. Null until
  // the first video frame — audio cannot be placed before then.
  private outputOffsetUs: number | null = null;

  private audioConfig: AudioMuxConfig | null = null;
  private audioSequence = 0;
  // One packet of lookahead: a sample's duration is the interval to its
  // SUCCESSOR, which is only knowable once the successor arrives (see pushAudio).
  private pendingAudio: { dts: number; data: Uint8Array } | null = null;
  private audioNominalSamples = 0;

  private stats: Fmp4MuxerStats = {
    initSegments: 0,
    mediaSegments: 0,
    skippedAwaitingInit: 0,
    errors: 0,
    audioInitSegments: 0,
    audioSegments: 0,
    audioSkipped: 0,
    audioHoles: 0,
  };

  getStats(): Fmp4MuxerStats {
    return { ...this.stats };
  }

  // Feed one released frame; returns the segments it produced (an init
  // segment precedes the media segment on the first keyframe and whenever the
  // parameter sets change). Never throws: a malformed frame counts an error
  // and produces nothing — the inline pipeline must keep painting whatever
  // happens here.
  push(frame: MuxInputFrame): Fmp4Segment[] {
    try {
      return this.pushInner(frame);
    } catch (e) {
      this.stats.errors++;
      log.warn('fmp4 muxer: frame dropped:', e);
      return [];
    }
  }

  private pushInner(frame: MuxInputFrame): Fmp4Segment[] {
    const out: Fmp4Segment[] = [];
    // A re-init is held back with the frame that triggers it: the sample still
    // pending belongs to the OLD parameter sets, and appending the new init
    // segment first would have the SourceBuffer parse it with the new config.
    let pendingInit: Fmp4InitSegment | null = null;

    if (frame.keyframe) {
      const extradata = frame.config?.extradata;
      if (extradata && extradata.length > 0 && extradata[0] === 0x01) {
        // avcC in the config: the browser broadcaster's AVCC shape.
        this.format = 'avcc';
        const normalized = normalizeAvccExtradata(extradata);
        const parsed = parseAvcc(normalized);
        pendingInit = this.maybeReinit(normalized.slice(), parsed.sps);
      } else if (startsWithStartCode(frame.data)) {
        // A genuine start code at offset 0: the native broadcaster's Annex-B
        // shape, parameter sets in-band (an AVCC sample can only fake a start
        // code mid-prefix — see `format` above — never a real leading one
        // followed by an SPS/PPS pair).
        const nals = splitAnnexB(frame.data);
        const sps = nals.find((n) => nalType(n) === NAL_SPS);
        const pps = nals.find((n) => nalType(n) === NAL_PPS);
        if (sps && pps) {
          this.format = 'annexb';
          pendingInit = this.maybeReinit(buildAvcc(sps, pps), sps);
        }
        // No in-band parameter sets and no avcC config: this keyframe cannot
        // (re)init anything — if a config is already active it still muxes
        // below on the sticky format.
      }
    }

    if (!this.avcc || this.format === null) {
      // Nothing to init from yet (deltas before the first keyframe, or an
      // AVCC keyframe whose config was lost).
      this.stats.skippedAwaitingInit++;
      if (pendingInit) out.push(pendingInit);
      return out;
    }

    const nals =
      this.format === 'annexb'
        ? splitAnnexB(frame.data)
        : splitAvcc(frame.data, parseAvcc(this.avcc).lengthSize);

    const sample = buildSample(nals);
    if (sample.length === 0) throw new Error('frame contains no sample NALs');

    const inputUs = Number(frame.timestampUs);
    let outputUs: number;
    if (this.prevInputUs === null) {
      outputUs = 0;
    } else {
      const delta = inputUs - this.prevInputUs;
      if (delta > 0 && delta <= RESTART_JUMP_US) {
        outputUs = this.prevOutputUs + delta;
        this.lastDurationUs = delta;
      } else {
        // Restart / wrap / stall: keep the output timeline strictly monotonic
        // by continuing at one frame interval.
        outputUs = this.prevOutputUs + this.lastDurationUs;
      }
    }
    this.prevInputUs = inputUs;
    this.prevOutputUs = outputUs;
    this.outputOffsetUs = outputUs - inputUs;

    // The held frame's duration is now known: exactly the interval to this one,
    // so consecutive samples abut and the buffered range has no hole. A long
    // interval (a capture stall, or a reorder-gap resync that discarded a GOP)
    // becomes a long-held frame — which is what the inline canvas does too.
    const pending = this.pendingVideo;
    if (pending) out.push(this.emitVideo(pending, outputUs - pending.outputUs));
    if (pendingInit) out.push(pendingInit);
    this.pendingVideo = { outputUs, keyframe: frame.keyframe, sample };
    return out;
  }

  // End of input: emit the held frame with the last observed interval, since no
  // successor will ever fix its duration. The live pipeline never calls this —
  // a live stream has no end, and the held frame is one frame of latency in the
  // MSE path only. Idempotent.
  flush(): Fmp4Segment[] {
    const pending = this.pendingVideo;
    if (!pending) return [];
    this.pendingVideo = null;
    return [this.emitVideo(pending, this.lastDurationUs)];
  }

  private emitVideo(
    pending: { outputUs: number; keyframe: boolean; sample: Uint8Array },
    durationUs: number,
  ): Fmp4MediaSegment {
    this.sequence++;
    this.stats.mediaSegments++;
    return {
      kind: 'media',
      track: 'video',
      keyframe: pending.keyframe,
      data: buildMediaSegment(pending.sample, {
        sequence: this.sequence,
        decodeTime: pending.outputUs,
        duration: durationUs,
        keyframe: pending.keyframe,
      }),
    };
  }

  // R22 audio (docs/27 finding 2): the R15 audio config (wire 0x08). Returns an
  // init segment when the track parameters actually change — the broadcaster
  // re-sends the config at 1 Hz, and re-initing per repeat would reset the
  // SourceBuffer for nothing. Emitted eagerly (it carries no timestamps), but
  // the presenter deliberately holds it until the first audio sample: an audio
  // track with a SourceBuffer and no samples empties the element's buffered
  // intersection and would stall the video.
  setAudioConfig(cfg: AudioMuxConfig): Fmp4Segment[] {
    // Two encapsulations, no guessing: Opus (the R15 lane verbatim) or AAC (the
    // iOS transcode path). Anything else can't be muxed here.
    const isOpus = /^opus$/i.test(cfg.codec);
    const isAac = /^mp4a\./i.test(cfg.codec);
    if ((!isOpus && !isAac) || cfg.sampleRate <= 0 || cfg.channels < 1) {
      this.stats.audioSkipped++;
      return [];
    }
    const codec: AudioMuxCodec = isAac ? 'aac' : 'opus';
    const cur = this.audioConfig;
    if (
      cur &&
      cur.codec === cfg.codec &&
      cur.sampleRate === cfg.sampleRate &&
      cur.channels === cfg.channels &&
      bytesEqual(cur.description ?? EMPTY, cfg.description ?? EMPTY)
    ) {
      return [];
    }
    const next: AudioMuxConfig = {
      codec: isAac ? cfg.codec : 'opus',
      sampleRate: cfg.sampleRate,
      channels: cfg.channels,
      ...(cfg.description ? { description: cfg.description.slice() } : {}),
    };
    let data: Uint8Array;
    try {
      data = buildAudioInitSegment(next, codec);
    } catch (e) {
      // A missing AudioSpecificConfig is the only way here: refuse the track
      // rather than append an init segment no demuxer can read.
      this.stats.audioSkipped++;
      log.warn('fmp4 muxer: audio init segment rejected:', e);
      return [];
    }
    this.audioConfig = next;
    this.pendingAudio = null;
    this.audioNominalSamples = isAac
      ? AAC_FRAME_SAMPLES
      : Math.round((OPUS_FRAME_MS / 1000) * cfg.sampleRate);
    this.stats.audioInitSegments++;
    return [
      {
        kind: 'init',
        track: 'audio',
        codec: next.codec,
        mime: isAac ? aacMime(next.codec) : opusMime(),
        width: 0,
        height: 0,
        data,
      },
    ];
  }

  // Feed one Opus packet. Emits the PREVIOUS packet's segment: a sample's
  // duration must be the interval to the next sample, or the audio timeline
  // grows holes (on a slowdown) and overlaps (on a speed-up) exactly the way the
  // video timeline did — and because buffered is the intersection of both
  // tracks, an audio hole freezes the native player's video. One packet of
  // lookahead is 20 ms; the audio track already runs ahead of the paced video
  // release by more than that.
  pushAudio(pkt: AudioMuxInput): Fmp4Segment[] {
    const cfg = this.audioConfig;
    if (!cfg || this.outputOffsetUs === null) {
      this.stats.audioSkipped++;
      return [];
    }
    const rate = cfg.sampleRate;
    const outUs = Number(pkt.timestampUs) + this.outputOffsetUs;
    if (outUs < 0) {
      // Audio for a timeline the video hasn't reached (join order, or a restart
      // seam): unplaceable, and negative decode times are illegal.
      this.stats.audioSkipped++;
      return [];
    }
    const dts = Math.round((outUs * rate) / 1_000_000);

    const prev = this.pendingAudio;
    if (!prev) {
      this.pendingAudio = { dts, data: pkt.data };
      return [];
    }
    const gap = dts - prev.dts;
    if (gap <= 0) {
      // Duplicate or reordered packet — the datagram lane has no ordering
      // guarantee. Keep the sample already in hand; a rewritten past would make
      // baseMediaDecodeTime non-monotonic.
      this.stats.audioSkipped++;
      return [];
    }
    const maxStretch = Math.round((AUDIO_MAX_STRETCH_MS / 1000) * rate);
    let duration = gap;
    if (gap > maxStretch) {
      // A real outage, not jitter: declare the sample honestly and leave the
      // hole visible (stretching a second of silence would desync everything
      // after it).
      duration = this.audioNominalSamples;
      this.stats.audioHoles++;
    }

    this.audioSequence++;
    const seg: Fmp4Segment = {
      kind: 'media',
      track: 'audio',
      keyframe: true, // every Opus packet is a sync sample
      data: buildMediaSegment(prev.data, {
        sequence: this.audioSequence,
        decodeTime: prev.dts,
        duration,
        keyframe: true,
      }),
    };
    this.pendingAudio = { dts, data: pkt.data };
    this.stats.audioSegments++;
    return [seg];
  }

  // Emit a fresh init segment when the parameter sets actually changed
  // (docs/27 Decision 6: R4/R13 resolution steps, codec pins, broadcaster
  // restarts with a different config). Byte-compares the avcC — the SPS/PPS
  // are re-sent with every IDR on the Annex-B path, and re-initing per GOP
  // would reset the SourceBuffer's decoder pointlessly.
  private maybeReinit(avcc: Uint8Array, sps: Uint8Array): Fmp4InitSegment | null {
    if (this.avcc && bytesEqual(this.avcc, avcc)) return null;
    const info = parseSps(sps);
    this.avcc = avcc;
    this.codec = codecFromAvcc(avcc);
    this.stats.initSegments++;
    return {
      kind: 'init',
      track: 'video',
      codec: this.codec,
      mime: h264Mime(this.codec),
      width: info.width,
      height: info.height,
      data: buildInitSegment(avcc, info.width, info.height),
    };
  }
}

// Rebuild the frame's NALs as a 4-byte-length-prefixed AVCC sample, dropping
// what doesn't belong in one: AUD (framing noise), SPS/PPS (they live in the
// avcC — in-band parameter sets are an avc3 affordance and Safari is strict
// about avc1).
function buildSample(nals: Uint8Array[]): Uint8Array {
  let size = 0;
  for (const nal of nals) {
    if (isSampleNal(nal)) size += 4 + nal.length;
  }
  const out = new Uint8Array(size);
  let o = 0;
  for (const nal of nals) {
    if (!isSampleNal(nal)) continue;
    const len = nal.length;
    out[o++] = (len >>> 24) & 0xff;
    out[o++] = (len >>> 16) & 0xff;
    out[o++] = (len >>> 8) & 0xff;
    out[o++] = len & 0xff;
    out.set(nal, o);
    o += len;
  }
  return out;
}

function isSampleNal(nal: Uint8Array): boolean {
  const t = nalType(nal);
  return t !== NAL_AUD && t !== NAL_SPS && t !== NAL_PPS;
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}
