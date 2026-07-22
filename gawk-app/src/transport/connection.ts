// WebTransport session plumbing shared by the broadcaster and viewer
// pipelines: connect (with dev-cert hash support), a datagram read loop,
// and a write chain that serializes datagram sends.

import { log } from '../lib/logger';
import {
  CARRIER_PROLOGUE_SIZE,
  CarrierRecordParser,
  MAX_DATAGRAM_SIZE,
  MAX_KEYFRAME_BYTES,
  STREAM_FRAME_HEADER_SIZE,
  TYPE_RELIABLE_CARRIER,
  TYPE_STREAM_FRAME,
  WIRE_VERSION,
  WireError,
  parseDecoderConfig,
  parseStreamFrameHeader,
  type DecoderConfigMessage,
} from './wire';

export interface ConnectOptions {
  // hex(SHA-256(cert DER)) as logged by gawk-server at startup
  // ("cert_hash_hex"). Required for the self-signed dev cert unless Chrome
  // was launched with the --ignore-certificate-errors-spki-list flag; leave
  // empty for a real (publicly trusted) certificate.
  certHashHex?: string;
  publishSecret?: string;
  // R17 W2: the hex resume token from a previous session of this broadcast
  // (wire 0x09). Required for every /publish/{id} claim; travels as the
  // `resume` query param (the WebTransport JS API can't set headers).
  resumeToken?: string;
  // R19 (docs/24 Decision 6): request reliable delta delivery at subscribe
  // time (`?delivery=reliable`). Viewer-only; ignored by the connect itself —
  // ViewerPipeline appends the query param when building the subscribe URL.
  deliveryMode?: 'reliable';
}

export async function connectWebTransport(url: string, opts: ConnectOptions = {}): Promise<WebTransport> {
  const init: WebTransportOptions = {
    // The whole design rides on datagrams — fail fast if the path can't do
    // them rather than silently falling back to nothing.
    requireUnreliable: true,
    congestionControl: 'low-latency',
  };
  const hash = opts.certHashHex?.trim();
  if (hash) {
    init.serverCertificateHashes = [{ algorithm: 'sha-256', value: hexToBytes(hash) }];
  }
  const wt = new WebTransport(url, init);
  await wt.ready;
  if (wt.datagrams.maxDatagramSize < MAX_DATAGRAM_SIZE) {
    // Handled condition, not a fault: packetizeFrame sizes chunks to the
    // actual path limit (docs/11 — Firefox negotiates 1024), so nothing is
    // dropped; smaller datagrams just mean more chunks per frame.
    log.info(
      `Path maxDatagramSize ${wt.datagrams.maxDatagramSize} < wire MAX_DATAGRAM_SIZE ${MAX_DATAGRAM_SIZE}; video chunks will be sized to the path limit (more datagrams per frame)`,
    );
  }
  log.info(`WebTransport connected: ${url} (maxDatagramSize ${wt.datagrams.maxDatagramSize})`);
  return wt;
}

export function bytesToHex(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += b.toString(16).padStart(2, '0');
  return out;
}

export function hexToBytes(hex: string): Uint8Array<ArrayBuffer> {
  const clean = hex.trim().toLowerCase();
  if (clean.length === 0 || clean.length % 2 !== 0 || /[^0-9a-f]/.test(clean)) {
    throw new Error(`Invalid hex string: "${hex}"`);
  }
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

// Reads datagrams until the stream ends (session closed) or abort() is
// called. Returns normally on either; connection errors reject.
export async function readDatagrams(
  wt: WebTransport,
  onDatagram: (dgram: Uint8Array) => void,
  signal?: AbortSignal,
): Promise<void> {
  const reader = wt.datagrams.readable.getReader();
  const onAbort = () => void reader.cancel().catch(() => {});
  signal?.addEventListener('abort', onAbort, { once: true });
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value) onDatagram(value);
    }
  } catch (e) {
    if (signal?.aborted) return;
    throw e instanceof Error ? e : new Error(String(e));
  } finally {
    signal?.removeEventListener('abort', onAbort);
    reader.releaseLock();
  }
}

// One keyframe delivered over a reliable unidirectional stream (R8).
export interface KeyframeStreamFrame {
  frameId: number;
  timestampUs: bigint;
  config: DecoderConfigMessage | null; // embedded config, if any
  data: Uint8Array; // encoded keyframe payload (safe to retain: a fresh copy)
  // Whole StreamFrame message as read off the wire (header + config +
  // payload) — mirrors the broadcaster's bytesSent accounting, so the
  // viewer's self-counted received bitrate matches the sent one.
  streamBytes: number;
}

// Per-session tallies of the R19 reliable-carrier streams, owned by the
// transport and surfaced in ViewerStats (docs/24 Decision 10). streamsAborted
// counts carriers ending in a reset — the relay CancelWrite-ing a stalled or
// superseded GOP tail; malformed counts framing violations (bad prologue,
// bad record length, truncated mid-record).
export interface CarrierCounters {
  streamsOpened: number;
  recordsReceived: number;
  streamsAborted: number;
  malformed: number;
}

export function newCarrierCounters(): CarrierCounters {
  return { streamsOpened: 0, recordsReceived: 0, streamsAborted: 0, malformed: 0 };
}

export interface ServerStreamCallbacks {
  onKeyframe: (kf: KeyframeStreamFrame) => void;
  // R19: one verbatim datagram record off a reliable carrier stream. The
  // transport feeds it into the same handler as a received datagram — the
  // whole point of the carrier design (docs/24 Decision 2).
  onCarrierRecord: (record: Uint8Array<ArrayBuffer>) => void;
}

// Test seam (undefined in production). readServerStreams tracks each in-flight
// stream task in a set it prunes on settle, so the working set stays
// proportional to open streams (~2), not to session length. This hook samples
// that set's size after every add and every settle so a test can assert the
// working set never grows unbounded (INGEST-1) — the bug being that the old
// array retained one settled promise per stream for the life of the session.
export interface ReadServerStreamsHooks {
  onInFlightChange?: (count: number) => void;
}

// Reads server-initiated unidirectional streams for the life of the session,
// dispatching each by its stream-kind bytes: a keyframe StreamFrame message
// (version‖0x04 — read to EOF, bounded by MAX_KEYFRAME_BYTES) or, since R19,
// a reliable carrier (version‖0x0A — a long-lived record loop). Each stream
// is processed on its own task so a stalled or superseded stream never blocks
// the next. Returns normally on session end or abort; connection errors
// reject.
export async function readServerStreams(
  wt: WebTransport,
  cb: ServerStreamCallbacks,
  carrier: CarrierCounters,
  signal?: AbortSignal,
  hooks?: ReadServerStreamsHooks,
): Promise<void> {
  const reader = wt.incomingUnidirectionalStreams.getReader();
  const onAbort = () => void reader.cancel().catch(() => {});
  signal?.addEventListener('abort', onAbort, { once: true });
  // Track only the streams still being read. A hours-long mobile session
  // accepts thousands of short-lived streams (keyframe + carrier per GOP);
  // pruning each on settle keeps the working set proportional to open streams
  // (~2), not to session length — the old append-only array was a slow leak
  // (INGEST-1: ~1 MB/hr, monotonic).
  const inFlight = new Set<Promise<void>>();
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value) {
        const task = readOneServerStream(value, cb, carrier).catch((e) =>
          log.warn('server stream read failed:', e),
        );
        inFlight.add(task);
        hooks?.onInFlightChange?.(inFlight.size);
        void task.finally(() => {
          inFlight.delete(task);
          hooks?.onInFlightChange?.(inFlight.size);
        });
      }
    }
  } catch (e) {
    if (signal?.aborted) return;
    throw e instanceof Error ? e : new Error(String(e));
  } finally {
    signal?.removeEventListener('abort', onAbort);
    reader.releaseLock();
    // Snapshot is taken synchronously; the .finally pruners deleting entries
    // during the await don't disturb the settled set Promise.allSettled built.
    await Promise.allSettled(inFlight);
  }
}

async function readOneServerStream(
  stream: ReadableStream<Uint8Array>,
  cb: ServerStreamCallbacks,
  carrier: CarrierCounters,
): Promise<void> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  const readMore = async (): Promise<boolean> => {
    const { value, done } = await reader.read();
    if (done) return false;
    if (value && value.length > 0) {
      chunks.push(value);
      total += value.length;
    }
    return true;
  };

  try {
    // Accumulate the two stream-kind bytes (they may span reads).
    while (total < CARRIER_PROLOGUE_SIZE) {
      if (!(await readMore())) return; // ended before declaring a kind
    }
    const head0 = chunks[0][0];
    const head1 = chunks[0].length > 1 ? chunks[0][1] : chunks[1][0];
    if (head0 !== WIRE_VERSION || (head1 !== TYPE_STREAM_FRAME && head1 !== TYPE_RELIABLE_CARRIER)) {
      // Unknown stream kind or version: cancel without wedging the accept
      // loop. Counted as malformed so it is visible in stats.
      carrier.malformed++;
      log.warn(`unknown server stream kind 0x${head0.toString(16)} 0x${head1.toString(16)}; cancelling`);
      void reader.cancel().catch(() => {});
      return;
    }

    if (head1 === TYPE_STREAM_FRAME) {
      // Keyframe: read to EOF, bounded, then parse the whole message.
      for (;;) {
        if (total > MAX_KEYFRAME_BYTES) {
          void reader.cancel().catch(() => {});
          log.warn(`keyframe stream exceeds ${MAX_KEYFRAME_BYTES} bytes; dropping`);
          return;
        }
        if (!(await readMore())) break;
      }
      emitKeyframe(concatChunks(chunks, total), cb.onKeyframe);
      return;
    }

    // Reliable carrier (R19): a record loop for the life of the stream. The
    // relay closes it at a rotation (clean EOF at a record boundary) or
    // resets it to shed a stalled GOP tail.
    carrier.streamsOpened++;
    const parser = new CarrierRecordParser();
    const deliver = (records: Uint8Array<ArrayBuffer>[]): void => {
      for (const record of records) {
        carrier.recordsReceived++;
        cb.onCarrierRecord(record);
      }
    };
    try {
      deliver(parser.push(concatChunks(chunks, total).subarray(CARRIER_PROLOGUE_SIZE)));
      for (;;) {
        const { value, done } = await reader.read();
        if (done) {
          // A clean FIN mid-record is a framing violation — the relay only
          // ever closes at record boundaries.
          if (parser.hasPartial()) carrier.malformed++;
          return;
        }
        if (value && value.length > 0) deliver(parser.push(value));
      }
    } catch (e) {
      if (e instanceof WireError) {
        // Corrupt length prefix: there is no resynchronizing a
        // length-prefixed stream — abandon it; the next rotation recovers.
        carrier.malformed++;
        log.warn('carrier stream framing error:', e);
        void reader.cancel().catch(() => {});
        return;
      }
      // A reset (relay CancelWrite of a stalled/superseded GOP): expected
      // under backpressure. The reorder buffer resyncs at the next keyframe.
      carrier.streamsAborted++;
      return;
    }
  } finally {
    reader.releaseLock();
  }
}

function concatChunks(chunks: Uint8Array[], total: number): Uint8Array<ArrayBuffer> {
  const data = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    data.set(c, offset);
    offset += c.length;
  }
  return data;
}

function emitKeyframe(data: Uint8Array, onKeyframe: (kf: KeyframeStreamFrame) => void): void {
  const header = parseStreamFrameHeader(data);
  const body = data.subarray(STREAM_FRAME_HEADER_SIZE);
  const configBytes = body.subarray(0, header.configLen);
  const payload = body.subarray(header.configLen, header.configLen + header.payloadLen);
  const config = header.configLen > 0 ? parseDecoderConfig(configBytes) : null;
  onKeyframe({
    frameId: header.frameId,
    timestampUs: header.timestampUs,
    config,
    data: payload,
    streamBytes: data.length,
  });
}

// Serializes datagram writes so frames leave in encode order (datagram
// delivery is unordered anyway, but bursting writes out of order would
// guarantee reordering instead of merely permitting it).
export class DatagramSender {
  private writer: WritableStreamDefaultWriter<BufferSource>;
  private chain: Promise<void> = Promise.resolve();
  private failed: Error | null = null;

  constructor(wt: WebTransport) {
    this.writer = wt.datagrams.writable.getWriter();
  }

  // Queues datagrams for sending. Returns a promise that settles when they
  // have been handed to the browser's datagram queue; rejects (and latches
  // the failure) if the session is gone.
  send(datagrams: Uint8Array<ArrayBuffer>[]): Promise<void> {
    if (this.failed) return Promise.reject(this.failed);
    this.chain = this.chain.then(async () => {
      for (const d of datagrams) {
        await this.writer.write(d);
      }
    });
    this.chain = this.chain.catch((e) => {
      this.failed = e instanceof Error ? e : new Error(String(e));
      throw this.failed;
    });
    return this.chain;
  }

  close(): void {
    this.writer.releaseLock();
  }
}
