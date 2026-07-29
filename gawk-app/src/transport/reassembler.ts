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

import { MAX_PARITY_SYMBOLS, parseParityChunk, recoverChunks } from './parity';
import { frameIdAhead, parseAudioConfig, parseAudioFrame, parseClockMapping, parseDecoderConfig, parseVideoChunk, parseViewerCount, peekType, TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME, TYPE_CLOCK_MAPPING, TYPE_DECODER_CONFIG, TYPE_PARITY_CHUNK, TYPE_VIDEO_CHUNK, TYPE_VIEWER_COUNT, WIRE_VERSION, type AudioConfigMessage, type DecoderConfigMessage } from './wire';

const MAX_ASSEMBLIES = 8;

export interface AssembledFrame {
  frameId: number;
  keyframe: boolean;
  timestampUs: bigint;
  data: Uint8Array; // contiguous copy, safe to retain
}

// R15 (docs/20 Decision 7): one demuxed audio packet — exactly one Opus
// packet. The payload aliases the input datagram (same non-reuse contract as
// push()).
export interface AudioPacket {
  seq: number;
  timestampUs: bigint;
  payload: Uint8Array;
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
  // R15 (docs/20 Decision 7): the audio lane's demux points. Audio has no
  // chunking/reassembly — a packet datagram IS the packet; the config is
  // deduplicated by byte equality like the video config (the broadcaster
  // re-sends it at 1 Hz by design).
  onAudioFrame?: (packet: AudioPacket) => void;
  onAudioConfig?: (config: AudioConfigMessage) => void;
  // R30 (docs/35 §5.5): one finalized frame's arrival accounting — how many
  // chunks the frame-global header promised and how many ACTUALLY arrived
  // (parity-recovered chunks are repairs, not deliveries). Fired at every
  // finalization: completion (including late-dropped ones — the network
  // delivered them) and eviction. This is the in-client port of the
  // datagram-loss-profile instrument's per-frame arithmetic, and the stripe
  // detector's only input.
  onFrameAccounting?: (expectedChunks: number, arrivedChunks: number) => void;
}

export interface ReassemblerStats {
  datagramsReceived: number;
  badDatagrams: number;
  duplicateChunks: number;
  duplicateConfigs: number;
  framesCompleted: number;
  framesDroppedIncomplete: number;
  framesDroppedLate: number;
  // R15 (docs/20): audio packets demuxed here. Loss/gaps are the sink's
  // story (it conceals them) — this is purely "what arrived".
  audioPacketsReceived: number;
  audioBytesReceived: number;
  // R29 forward parity (docs/34 §7.1). parityChunksReceived is arrival;
  // framesRecoveredByParity is the headline "is it working" signal — a frame
  // that would have been dropped incomplete and instead decoded.
  // parityRecoveryFailures counts frames where parity was present but there
  // were more erasures than symbols: routine on a bad link, not a fault.
  parityChunksReceived: number;
  framesRecoveredByParity: number;
  parityRecoveryFailures: number;
  // R30 (docs/35 §12 finding 2): data chunks arriving for a frame at or
  // behind the emit watermark, dropped WITHOUT creating an assembly. Rare
  // pre-R30 (one connection delivers a frame nearly atomically); routine
  // under striping, where eager parity recovery races the slowest leg and
  // the raced share then arrives behind the watermark. Before this guard,
  // each such share built a phantom assembly that died as
  // framesDroppedIncomplete — ~130/window on a clean loopback.
  staleChunks: number;
  // R29 finding 3 (docs/34): frames given up on that HELD parity which could
  // not cover their erasures. parityRecoveryFailures cannot see these — the
  // solve is never attempted — so it read 0 across a live session that
  // repaired nothing, making "parity worked" and "parity was never tried"
  // the same number. Counted at eviction, once per frame actually lost.
  parityInsufficient: number;
}

interface Assembly {
  keyframe: boolean;
  timestampUs: bigint;
  chunkCount: number;
  payloads: (Uint8Array | null)[];
  received: number;
  // Real chunk arrivals only. `received` doubles as the completeness cursor
  // and is bumped to chunkCount by a parity recovery; this one never is —
  // the stripe detector needs delivery truth, not repair truth (R30).
  arrived: number;
  bytes: number;
  // R29: parity symbols held for this frame, indexed by parityIndex, and the
  // total frame length their headers carry (the only thing that says how long
  // the short final chunk is). Both stay null/0 until a parity chunk arrives,
  // so a frame on a fleet with parity off allocates nothing extra.
  parity: (Uint8Array | null)[] | null;
  parityHeld: number;
  frameBytes: number;
}

// How far (in emitted frames) a RECOVERED frame's arrival accounting is
// deferred, waiting for the chunks the recovery raced. Half a GOP at the
// 500 ms cadence — far beyond any leg skew, far short of the detector's
// 30 s window caring.
const RECOVERED_ACCOUNTING_WINDOW = 16;

export class Reassembler {
  private cb: ReassemblerCallbacks;
  private assemblies = new Map<number, Assembly>(); // insertion order = arrival order
  private lastConfigBytes: Uint8Array | null = null;
  private lastAudioConfigBytes: Uint8Array | null = null;
  private lastEmittedFrameId: number | null = null;
  // R30 (docs/35 §12 finding 2): accounting for parity-RECOVERED frames,
  // held open so stragglers the recovery raced can be credited as the
  // deliveries they are. Reporting arrived < expected at recovery time
  // called a raced stripe leg a lossy link — which is exactly the false
  // signal the burst-threshold-loss rule would then fire on. Keyed by
  // frameId; rolled by watermark distance at each emit.
  private recoveredLedger = new Map<number, { expected: number; arrived: number }>();

  private stats: ReassemblerStats = {
    datagramsReceived: 0,
    badDatagrams: 0,
    duplicateChunks: 0,
    duplicateConfigs: 0,
    framesCompleted: 0,
    framesDroppedIncomplete: 0,
    framesDroppedLate: 0,
    audioPacketsReceived: 0,
    audioBytesReceived: 0,
    parityChunksReceived: 0,
    framesRecoveredByParity: 0,
    parityRecoveryFailures: 0,
    parityInsufficient: 0,
    staleChunks: 0,
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
      case TYPE_PARITY_CHUNK:
        this.pushParity(dgram);
        break;
      case TYPE_CLOCK_MAPPING:
        this.pushClockMapping(dgram);
        break;
      case TYPE_VIEWER_COUNT:
        this.pushViewerCount(dgram);
        break;
      case TYPE_AUDIO_FRAME:
        this.pushAudioFrame(dgram);
        break;
      case TYPE_AUDIO_CONFIG:
        this.pushAudioConfig(dgram);
        break;
      default:
        this.stats.badDatagrams++;
    }
  }

  private pushAudioFrame(dgram: Uint8Array): void {
    let header, payload: Uint8Array;
    try {
      ({ header, payload } = parseAudioFrame(dgram));
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    this.stats.audioPacketsReceived++;
    this.stats.audioBytesReceived += dgram.byteLength;
    this.cb.onAudioFrame?.({ seq: header.seq, timestampUs: header.timestampUs, payload });
  }

  private pushAudioConfig(dgram: Uint8Array): void {
    let config: AudioConfigMessage;
    try {
      config = parseAudioConfig(dgram);
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    // The broadcaster re-sends this at 1 Hz (docs/20 Decision 5) — dedup by
    // byte equality so the sink reconfigures only on a real change.
    if (this.lastAudioConfigBytes !== null && bytesEqual(this.lastAudioConfigBytes, dgram)) {
      this.stats.duplicateConfigs++;
      return;
    }
    this.lastAudioConfigBytes = dgram;
    this.cb.onAudioConfig?.(config);
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
      // R30 (docs/35 §12 finding 2): a chunk for an already-emitted frame
      // must not build a phantom assembly — the delta-chunk mirror of the
      // guard pushParity has had since R29. Keyframes bypass (a datagram
      // keyframe resets the watermark by design); frames with a LIVE
      // assembly behind the watermark still fill and late-drop as before.
      if (
        !header.keyframe &&
        this.lastEmittedFrameId !== null &&
        !frameIdAhead(header.frameId, this.lastEmittedFrameId)
      ) {
        this.stats.staleChunks++;
        // If the frame completed by recovery, this straggler is the
        // delivery the solve raced — credit it before accounting reports.
        const held = this.recoveredLedger.get(header.frameId);
        if (held && held.arrived < held.expected) held.arrived++;
        return;
      }
      this.evictIfFull();
      assembly = {
        keyframe: header.keyframe,
        timestampUs: header.timestampUs,
        chunkCount: header.chunkCount,
        payloads: new Array<Uint8Array | null>(header.chunkCount).fill(null),
        received: 0,
        arrived: 0,
        bytes: 0,
        parity: null,
        parityHeld: 0,
        frameBytes: 0,
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
    assembly.arrived++;
    assembly.bytes += payload.length;

    if (assembly.received === assembly.chunkCount) {
      this.assemblies.delete(header.frameId);
      this.completeFrame(header.frameId, assembly, false);
      return;
    }
    this.tryRecover(header.frameId, assembly);
  }

  // R29 (docs/34): a parity symbol for some frame. Held against the assembly
  // until either the frame completes on its own (parity discarded) or enough
  // chunks are in to solve for the missing ones.
  //
  // Parity for an unknown frame creates the assembly: on a lossy link the
  // producer's parity can outrun the chunk that would have created it, and
  // dropping it would waste exactly the symbol the frame needs.
  private pushParity(dgram: Uint8Array): void {
    let header, payload: Uint8Array;
    try {
      ({ header, payload } = parseParityChunk(dgram));
    } catch {
      this.stats.badDatagrams++;
      return;
    }
    this.stats.parityChunksReceived++;

    let assembly = this.assemblies.get(header.frameId);
    // Parity for a frame already emitted is redundant, and on a CLEAN link
    // that is the normal case — the producer sends parity after the data
    // chunks, so every frame completes before its symbols land. Creating an
    // assembly here would leave one that can never complete, which later
    // evicts as framesDroppedIncomplete: phantom drops on a lossless link,
    // inflating the counter R29's whole diagnosis rests on.
    //
    // Serial comparison (wrap-aware), the same rule pushDelta uses. Keyed on
    // the emitted watermark rather than on "have I seen this frame", because
    // parity legitimately outruns its own data chunks under reorder.
    if (
      !assembly &&
      this.lastEmittedFrameId !== null &&
      !frameIdAhead(header.frameId, this.lastEmittedFrameId)
    ) {
      return;
    }
    if (!assembly) {
      this.evictIfFull();
      assembly = {
        // Parity is delta-only by construction (keyframes ride reliable
        // streams), so a parity-created assembly is never a keyframe. The
        // timestamp is filled in by the first real chunk — a parity header
        // deliberately does not carry one (docs/34 §4.2).
        keyframe: false,
        timestampUs: 0n,
        chunkCount: header.chunkCount,
        payloads: new Array<Uint8Array | null>(header.chunkCount).fill(null),
        received: 0,
        arrived: 0,
        bytes: 0,
        parity: null,
        parityHeld: 0,
        frameBytes: 0,
      };
      this.assemblies.set(header.frameId, assembly);
    }
    if (header.chunkCount !== assembly.chunkCount) {
      this.stats.badDatagrams++;
      return;
    }
    if (!assembly.parity) assembly.parity = new Array<Uint8Array | null>(MAX_PARITY_SYMBOLS).fill(null);
    if (header.parityIndex >= assembly.parity.length) {
      this.stats.badDatagrams++;
      return;
    }
    if (assembly.parity[header.parityIndex] !== null) {
      this.stats.duplicateChunks++;
      return;
    }
    assembly.parity[header.parityIndex] = payload;
    assembly.parityHeld++;
    assembly.frameBytes = header.frameBytes;
    this.tryRecover(header.frameId, assembly);
  }

  // Attempts recovery as soon as the erasures are within the symbols held.
  // Eager by design: waiting would add latency to exactly the frames this
  // feature exists to save.
  private tryRecover(frameId: number, assembly: Assembly): void {
    if (!assembly.parity || assembly.parityHeld === 0) return;
    const missing = assembly.chunkCount - assembly.received;
    if (missing <= 0 || missing > assembly.parityHeld) return;
    // A frame whose only arrivals are parity has no timestamp yet; without it
    // the decoder cannot be fed, so wait for a real chunk.
    if (assembly.received === 0) return;
    try {
      recoverChunks(assembly.payloads, assembly.parity, assembly.frameBytes);
    } catch {
      // Routine on a bad link (more erasures than symbols, or a header that
      // cannot describe the block) — never fatal, and the frame stays held so
      // a straggler chunk can still complete it.
      this.stats.parityRecoveryFailures++;
      return;
    }
    let bytes = 0;
    for (const p of assembly.payloads) {
      if (!p) return; // recovery did not fill everything; leave it held
      bytes += p.length;
    }
    assembly.received = assembly.chunkCount;
    assembly.bytes = bytes;
    this.assemblies.delete(frameId);
    this.stats.framesRecoveredByParity++;
    this.completeFrame(frameId, assembly, true);
  }

  private completeFrame(frameId: number, assembly: Assembly, recovered: boolean): void {
    if (recovered) {
      // Deferred (docs/35 §12 finding 2): the chunks this recovery raced may
      // still be in flight on a slower stripe leg; report only once the
      // watermark has moved far enough that a straggler is genuine loss.
      this.recoveredLedger.set(frameId, {
        expected: assembly.chunkCount,
        arrived: assembly.arrived,
      });
    } else {
      this.cb.onFrameAccounting?.(assembly.chunkCount, assembly.arrived);
    }
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
    this.rollRecoveredLedger(frameId);
    this.cb.onFrame({
      frameId,
      keyframe: assembly.keyframe,
      timestampUs: assembly.timestampUs,
      data,
    });
  }

  // Reports (and drops) deferred recovered-frame accounting once the emit
  // watermark is RECOVERED_ACCOUNTING_WINDOW frames past it — stragglers
  // after that are genuine loss, not leg skew. Serial arithmetic throughout;
  // a broadcaster restart's backwards jump strands at most a window of old
  // entries, which the size cap below retires with their then-current tally.
  private rollRecoveredLedger(watermark: number): void {
    for (const [id, held] of this.recoveredLedger) {
      const distance = (watermark - id) >>> 0;
      if (frameIdAhead(watermark, id) && distance >= RECOVERED_ACCOUNTING_WINDOW) {
        this.recoveredLedger.delete(id);
        this.cb.onFrameAccounting?.(held.expected, held.arrived);
      }
    }
    while (this.recoveredLedger.size > 4 * RECOVERED_ACCOUNTING_WINDOW) {
      const oldest = this.recoveredLedger.keys().next();
      if (oldest.done) break;
      const held = this.recoveredLedger.get(oldest.value)!;
      this.recoveredLedger.delete(oldest.value);
      this.cb.onFrameAccounting?.(held.expected, held.arrived);
    }
  }

  private evictIfFull(): void {
    if (this.assemblies.size < MAX_ASSEMBLIES) return;
    const oldest = this.assemblies.keys().next();
    if (!oldest.done) {
      // R29 finding 3: eviction is where a frame is actually given up on, so
      // it is the only place the parity shortfall can be attributed to one
      // frame exactly once. Two shapes count, because both mean "parity was
      // present and could not save it": more erasures than symbols held, and
      // the n<=k case where every data chunk died so there is no timestamp to
      // decode the reconstruction with (docs/34 §4.2).
      const assembly = this.assemblies.get(oldest.value);
      if (assembly && assembly.parityHeld > 0) {
        const missing = assembly.chunkCount - assembly.received;
        if (missing > assembly.parityHeld || assembly.received === 0) {
          this.stats.parityInsufficient++;
        }
      }
      if (assembly) this.cb.onFrameAccounting?.(assembly.chunkCount, assembly.arrived);
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
