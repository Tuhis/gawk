// Viewer-side depacketization: relay datagrams → decoder config messages +
// complete encoded frames. Pure bytes-in/callbacks-out (no WebCodecs, no
// network) so the whole drop/reorder policy is unit-testable in node.
//
// Policy (per the project principle: favor dropped frames over stalls):
// - A frame is emitted only when all of its chunks have arrived; nothing
//   ever waits for retransmits.
// - At most MAX_ASSEMBLIES frames assemble concurrently; starting one more
//   evicts the oldest in-progress assembly (it lost the race — a datagram
//   went missing).
// - Completed delta frames behind the last emitted frame are dropped (late
//   reorder). frameIds are uint32 and wrap, so "behind" is serial arithmetic
//   (wire.frameIdAhead), never `<=`.
// - Keyframes reset the ordering watermark — a keyframe doesn't reference
//   other frames, and the reset is what makes a broadcaster restart
//   (frameIds reset to 0) recover. Since R8 real keyframes arrive over
//   reliable streams and never pass through here, so the pipeline reports
//   them via noteStreamKeyframe(); the datagram-keyframe path below is kept
//   for robustness but no broadcaster sends it (R10 field finding, docs/14).
// - Duplicate DecoderConfig datagrams are deduplicated by byte equality;
//   the relay re-emits the config before every keyframe by design.

import { frameIdAhead, parseClockMapping, parseDecoderConfig, parseVideoChunk, parseViewerCount, peekType, TYPE_CLOCK_MAPPING, TYPE_DECODER_CONFIG, TYPE_VIDEO_CHUNK, TYPE_VIEWER_COUNT, WIRE_VERSION, type DecoderConfigMessage } from './wire';

const MAX_ASSEMBLIES = 8;

export interface AssembledFrame {
  frameId: number;
  keyframe: boolean;
  timestampUs: bigint;
  data: Uint8Array; // contiguous copy, safe to retain
}

export interface ReassemblerCallbacks {
  // Called only when the config bytes differ from the previous one.
  onConfig: (config: DecoderConfigMessage) => void;
  onFrame: (frame: AssembledFrame) => void;
  // R5 Q2: the broadcaster's clock mapping (relayClockUs = timestampUs +
  // offsetUs), relayed + cached by the relay. Last one wins.
  onClockMapping?: (offsetUs: bigint) => void;
  // R18 (docs/23 Decision 8): the relay's live "N watching" push (global
  // across the fleet in cluster mode). Last one wins, like the mapping.
  onSubscriberCount?: (count: number) => void;
}

export interface ReassemblerStats {
  datagramsReceived: number;
  badDatagrams: number;
  duplicateChunks: number;
  duplicateConfigs: number;
  framesCompleted: number;
  framesDroppedIncomplete: number;
  framesDroppedLate: number;
}

interface Assembly {
  keyframe: boolean;
  timestampUs: bigint;
  chunkCount: number;
  payloads: (Uint8Array | null)[];
  received: number;
  bytes: number;
}

export class Reassembler {
  private cb: ReassemblerCallbacks;
  private assemblies = new Map<number, Assembly>(); // insertion order = arrival order
  private lastConfigBytes: Uint8Array | null = null;
  private lastEmittedFrameId: number | null = null;

  private stats: ReassemblerStats = {
    datagramsReceived: 0,
    badDatagrams: 0,
    duplicateChunks: 0,
    duplicateConfigs: 0,
    framesCompleted: 0,
    framesDroppedIncomplete: 0,
    framesDroppedLate: 0,
  };

  constructor(callbacks: ReassemblerCallbacks) {
    this.cb = callbacks;
  }

  getStats(): ReassemblerStats {
    return { ...this.stats };
  }

  // Keyframes travel on reliable streams since R8 and never pass through the
  // datagram reassembler — so the pipeline reports them here to sync the
  // late-delta watermark. Without this, a broadcaster restart (frameIds reset
  // to 0) leaves the watermark at the old session's high frameId and every
  // new-session delta is dropped as "late" — keyframe-only 2 fps playback
  // (R10 field finding, docs/14).
  noteStreamKeyframe(frameId: number): void {
    // Unconditional: a backwards jump here is exactly the restart signal the
    // watermark must follow. Mid-session it's a no-op (keyframe ids track the
    // delta sequence), and a racing stale keyframe merely re-admits at most a
    // GOP of deltas that the reorder buffer drops as stale anyway.
    this.lastEmittedFrameId = frameId;
  }

  // Feeds one received datagram. The buffer must not be reused by the
  // caller afterwards (payload views are retained until frame completion).
  push(dgram: Uint8Array): void {
    this.stats.datagramsReceived++;
    let version: number;
    let msgType: number;
    try {
      ({ version, msgType } = peekType(dgram));
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    if (version !== WIRE_VERSION) {
      this.stats.badDatagrams++;
      return;
    }
    switch (msgType) {
      case TYPE_DECODER_CONFIG:
        this.pushConfig(dgram);
        break;
      case TYPE_VIDEO_CHUNK:
        this.pushChunk(dgram);
        break;
      case TYPE_CLOCK_MAPPING:
        this.pushClockMapping(dgram);
        break;
      case TYPE_VIEWER_COUNT:
        this.pushViewerCount(dgram);
        break;
      default:
        this.stats.badDatagrams++;
    }
  }

  private pushViewerCount(dgram: Uint8Array): void {
    let count: number;
    try {
      count = parseViewerCount(dgram);
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    this.cb.onSubscriberCount?.(count);
  }

  private pushClockMapping(dgram: Uint8Array): void {
    let offsetUs: bigint;
    try {
      offsetUs = parseClockMapping(dgram);
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    this.cb.onClockMapping?.(offsetUs);
  }

  private pushConfig(dgram: Uint8Array): void {
    let config: DecoderConfigMessage;
    try {
      config = parseDecoderConfig(dgram);
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    if (this.lastConfigBytes !== null && bytesEqual(this.lastConfigBytes, dgram)) {
      this.stats.duplicateConfigs++;
      return;
    }
    this.lastConfigBytes = dgram;
    this.cb.onConfig(config);
  }

  private pushChunk(dgram: Uint8Array): void {
    let header, payload: Uint8Array;
    try {
      ({ header, payload } = parseVideoChunk(dgram));
    } catch {
      this.stats.badDatagrams++;
      return;
    }

    let assembly = this.assemblies.get(header.frameId);
    if (!assembly) {
      this.evictIfFull();
      assembly = {
        keyframe: header.keyframe,
        timestampUs: header.timestampUs,
        chunkCount: header.chunkCount,
        payloads: new Array<Uint8Array | null>(header.chunkCount).fill(null),
        received: 0,
        bytes: 0,
      };
      this.assemblies.set(header.frameId, assembly);
    }
    if (header.chunkCount !== assembly.chunkCount) {
      // Chunks of one frame disagree on the count: corrupt.
      this.stats.badDatagrams++;
      return;
    }
    if (assembly.payloads[header.chunkIndex] !== null) {
      this.stats.duplicateChunks++;
      return;
    }
    assembly.payloads[header.chunkIndex] = payload;
    assembly.received++;
    assembly.bytes += payload.length;

    if (assembly.received === assembly.chunkCount) {
      this.assemblies.delete(header.frameId);
      this.completeFrame(header.frameId, assembly);
    }
  }

  private completeFrame(frameId: number, assembly: Assembly): void {
    // Late delta frames are useless (their reference frame was already
    // superseded); keyframes are self-contained and always emitted. "Late"
    // is serial (wrap-aware): a frameId just past the uint32 rollover is
    // AHEAD of the watermark, not 4 billion frames behind it.
    if (
      !assembly.keyframe &&
      this.lastEmittedFrameId !== null &&
      !frameIdAhead(frameId, this.lastEmittedFrameId)
    ) {
      this.stats.framesDroppedLate++;
      return;
    }
    const data = new Uint8Array(assembly.bytes);
    let offset = 0;
    for (const payload of assembly.payloads) {
      // All payloads are non-null once received === chunkCount.
      data.set(payload!, offset);
      offset += payload!.length;
    }
    this.lastEmittedFrameId = frameId;
    this.stats.framesCompleted++;
    this.cb.onFrame({
      frameId,
      keyframe: assembly.keyframe,
      timestampUs: assembly.timestampUs,
      data,
    });
  }

  private evictIfFull(): void {
    if (this.assemblies.size < MAX_ASSEMBLIES) return;
    const oldest = this.assemblies.keys().next();
    if (!oldest.done) {
      this.assemblies.delete(oldest.value);
      this.stats.framesDroppedIncomplete++;
    }
  }
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}
