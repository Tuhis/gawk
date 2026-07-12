// WebTransport session plumbing shared by the broadcaster and viewer
// pipelines: connect (with dev-cert hash support), a datagram read loop,
// and a write chain that serializes datagram sends.

import { log } from '../lib/logger';
import { MAX_DATAGRAM_SIZE } from './wire';

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
    // Wire chunks are sized to MAX_DATAGRAM_SIZE; a smaller path MTU would
    // make the browser drop our biggest datagrams. Loud warning over hard
    // failure: most frames' final chunk is smaller and still flows.
    log.warn(
      `Path maxDatagramSize ${wt.datagrams.maxDatagramSize} < wire MAX_DATAGRAM_SIZE ${MAX_DATAGRAM_SIZE}; full-size chunks will be dropped`,
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
