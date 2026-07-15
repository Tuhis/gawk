// WebTransport session plumbing shared by the broadcaster and viewer
// pipelines: connect (with dev-cert hash support), a datagram read loop,
// and a write chain that serializes datagram sends.

import { log } from '../lib/logger';
import {
  MAX_DATAGRAM_SIZE,
  MAX_KEYFRAME_BYTES,
  STREAM_FRAME_HEADER_SIZE,
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
}

// Reads server-initiated unidirectional streams for the life of the session,
// each carrying exactly one keyframe StreamFrame message. Each stream is read
// to EOF (bounded by MAX_KEYFRAME_BYTES) and processed on its own task so a
// stalled or superseded stream never blocks the next. Returns normally on
// session end or abort; connection errors reject.
export async function readKeyframeStreams(
  wt: WebTransport,
  onKeyframe: (kf: KeyframeStreamFrame) => void,
  signal?: AbortSignal,
): Promise<void> {
  const reader = wt.incomingUnidirectionalStreams.getReader();
  const onAbort = () => void reader.cancel().catch(() => {});
  signal?.addEventListener('abort', onAbort, { once: true });
  const tasks: Promise<void>[] = [];
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value) tasks.push(readOneKeyframe(value, onKeyframe).catch((e) => log.warn('keyframe stream read failed:', e)));
    }
  } catch (e) {
    if (signal?.aborted) return;
    throw e instanceof Error ? e : new Error(String(e));
  } finally {
    signal?.removeEventListener('abort', onAbort);
    reader.releaseLock();
    await Promise.allSettled(tasks);
  }
}

async function readOneKeyframe(
  stream: ReadableStream<Uint8Array>,
  onKeyframe: (kf: KeyframeStreamFrame) => void,
): Promise<void> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      if (!value) continue;
      total += value.length;
      if (total > MAX_KEYFRAME_BYTES) {
        void reader.cancel().catch(() => {});
        log.warn(`keyframe stream exceeds ${MAX_KEYFRAME_BYTES} bytes; dropping`);
        return;
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const data = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    data.set(c, offset);
    offset += c.length;
  }

  const header = parseStreamFrameHeader(data);
  const body = data.subarray(STREAM_FRAME_HEADER_SIZE);
  const configBytes = body.subarray(0, header.configLen);
  const payload = body.subarray(header.configLen, header.configLen + header.payloadLen);
  const config = header.configLen > 0 ? parseDecoderConfig(configBytes) : null;
  onKeyframe({ frameId: header.frameId, timestampUs: header.timestampUs, config, data: payload });
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
