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
  TELEMETRY_HELLO_SIZE,
  TYPE_RELAY_CAPABILITIES,
  TYPE_RELIABLE_CARRIER,
  TYPE_SESSION_CLOSING,
  SESSION_CLOSING_SIZE,
  parseSessionClosing,
  TYPE_STREAM_FRAME,
  TYPE_TELEMETRY_HELLO,
  TYPE_TELEMETRY_ENDPOINT,
  TELEMETRY_ENDPOINT_MAX_URL_LEN,
  parseTelemetryEndpoint,
  WIRE_VERSION,
  WireError,
  parseDecoderConfig,
  parseStreamFrameHeader,
  parseTelemetryHello,
  type DecoderConfigMessage,
  type TelemetryHelloMessage,
} from './wire';
import {
  LOCAL_RELAY_STREAM_MAX_BYTES,
  isLocalRelayType,
  parseLocalRelayEvent,
  type LocalRelayEvent,
} from './local-relay';
import {
  RELAY_CAPABILITIES_SIZE,
  parseRelayCapabilities,
  type RelayCapabilities,
} from './parity';

export interface ConnectOptions {
  // hex(SHA-256(cert DER)) as logged by gawk-server at startup
  // ("cert_hash_hex"). Required for the self-signed dev cert unless Chrome
  // was launched with the --ignore-certificate-errors-spki-list flag; leave
  // empty for a real (publicly trusted) certificate. May hold several
  // comma-separated hashes (the store's selectCertHashList form).
  certHashHex?: string;
  // R74: every pinned hash, e.g. a PoP's current + next across a rolling
  // rotation (docs/76 §6.1 item 3). Merged with certHashHex.
  certHashes?: string[];
  publishSecret?: string;
  // The hex resume token from a previous session of this broadcast
  // (wire 0x09). Required for every /publish/{id} claim; travels as the
  // `resume` query param (the WebTransport JS API can't set headers).
  resumeToken?: string;
  // Request reliable delta delivery at subscribe time (`?delivery=reliable`).
  // Viewer-only; ignored by the connect itself — ViewerPipeline appends the
  // query param when building the subscribe URL.
  deliveryMode?: 'reliable';
  // Opt DOWN from the fleet parity default. Absent means
  // "take whatever the fleet serves", which is the default and the only way to
  // get the maximum — a viewer cannot conjure symbols the producer never
  // emitted, so there is deliberately no way to ask for MORE.
  parityLevel?: 0 | 1;
  // R59 (docs/61 D4): this dial is the viewer session's automatic reconnect,
  // not a new viewer. ViewerSession sets it on every pipeline after the
  // first, and ViewerPipeline sends it as `?rejoin=1`.
  rejoin?: boolean;
  // R74 (docs/76 item 43.7): this client will follow a LocalRelayOffer, so
  // it says `lr=1` on the CONNECT and home may hold its demand briefly.
  followOffers?: boolean;
  // R74 (docs/76 item 34): a broadcaster's remembered party-wall Hide,
  // `wall=hidden` on /publish, so it never flashes onto the wall.
  wallHidden?: boolean;
}

// The WebTransportOptions every gawk session dials with. Shared with the
// room control session (room-session.ts), which carries no media
// but must present the same dev-cert hash as the media sessions do — a room
// that could not be joined on a local stack while its broadcasts could would
// be a confusing half-failure.
export function webTransportInit(opts: ConnectOptions = {}): WebTransportOptions {
  const init: WebTransportOptions = {
    // The whole design rides on datagrams — fail fast if the path can't do
    // them rather than silently falling back to nothing.
    requireUnreliable: true,
    congestionControl: 'low-latency',
  };
  const hashes: string[] = [];
  for (const h of [...(opts.certHashHex ?? '').split(','), ...(opts.certHashes ?? [])]) {
    const clean = h.trim().toLowerCase();
    if (clean !== '' && !hashes.includes(clean)) hashes.push(clean);
  }
  if (hashes.length > 0) {
    // The browser accepts the session if the presented certificate matches
    // ANY listed hash.
    init.serverCertificateHashes = hashes.map((h) => ({ algorithm: 'sha-256', value: hexToBytes(h) }));
  }
  return init;
}

// How long one WebTransport handshake may go unanswered before the dial
// abandons it and starts over on a fresh session, and how long it keeps
// starting over before giving up. The same budgets as the desktop engine's
// DIAL_ATTEMPT_TIMEOUT / DIAL_TIMEOUT (gawk-broadcast-desktop
// crates/engine/src/transport.rs), for the same failure: a QUIC connection
// whose first packets were lost recovers slowly or never, while a fresh one
// is answered at once. In the browser `ready` then never settles: a Firefox
// viewer opening a room link sat on "Connecting…" until a reload, and the
// relay logged nothing because no CONNECT ever reached it. A healthy dial
// takes a couple of round trips, so this only ever cuts short a stuck one.
export const DIAL_ATTEMPT_TIMEOUT_MS = 5_000;
export const DIAL_TIMEOUT_MS = 30_000;

// No attempt was answered within DIAL_TIMEOUT_MS. Distinct from a rejected
// `ready` (the relay, or the browser, refused the session), which is never
// retried here: a refusal is an answer.
export class DialTimeoutError extends Error {
  constructor(url: string) {
    super(`no answer from the relay in ${DIAL_TIMEOUT_MS / 1000} s: ${url}`);
    this.name = 'DialTimeoutError';
  }
}

function abortError(signal: AbortSignal): Error {
  return signal.reason instanceof Error ? signal.reason : new DOMException('dial aborted', 'AbortError');
}

// For a session the dial never hands back (abandoned or refused): nobody else
// holds it, so nobody else would observe its `closed` rejecting, and a refused
// handshake rejects `closed` along with `ready`.
function silenceClosed(wt: WebTransport): void {
  void (wt.closed as Promise<unknown> | undefined)?.catch(() => {});
}

function abandon(wt: WebTransport): void {
  silenceClosed(wt);
  try {
    wt.close();
  } catch {
    // a session that never opened may refuse close(); there is nothing to free
  }
}

// Opens a WebTransport session, retrying unanswered handshakes on a fresh
// session (see DIAL_ATTEMPT_TIMEOUT_MS). Resolves with the first session
// whose `ready` resolves; rejects with the error of a `ready` that rejects,
// with DialTimeoutError once the budget is spent, or with an AbortError when
// `signal` aborts. Every abandoned session is closed before the next dial,
// so a retried CONNECT that did reach the relay ends there rather than
// lingering. A retried CONNECT can still reach the relay twice (see
// docs/gotchas.md, "The native dial can send a CONNECT twice").
export async function dialWebTransport(
  url: string,
  init: WebTransportOptions,
  signal?: AbortSignal,
): Promise<WebTransport> {
  let left = DIAL_TIMEOUT_MS;
  for (let attempt = 1; ; attempt++) {
    if (signal?.aborted) throw abortError(signal);
    const wt = new WebTransport(url, init);
    const slice = Math.min(left, DIAL_ATTEMPT_TIMEOUT_MS);
    const outcome = await new Promise<'ready' | 'timeout' | 'aborted'>((resolve, reject) => {
      const onAbort = () => finish(() => resolve('aborted'));
      const timer = setTimeout(() => finish(() => resolve('timeout')), slice);
      const finish = (settle: () => void) => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', onAbort);
        settle();
      };
      signal?.addEventListener('abort', onAbort, { once: true });
      wt.ready.then(
        () => finish(() => resolve('ready')),
        (e: unknown) => {
          silenceClosed(wt);
          finish(() => reject(e instanceof Error ? e : new Error(String(e))));
        },
      );
    });
    if (outcome === 'ready') return wt;
    abandon(wt);
    if (outcome === 'aborted') throw abortError(signal!);
    left -= slice;
    if (left <= 0) throw new DialTimeoutError(url);
    log.info(`WebTransport dial attempt ${attempt} unanswered after ${slice} ms; retrying on a fresh session: ${url}`);
  }
}

export async function connectWebTransport(
  url: string,
  opts: ConnectOptions = {},
  signal?: AbortSignal,
): Promise<WebTransport> {
  const wt = await dialWebTransport(url, webTransportInit(opts), signal);
  if (wt.datagrams.maxDatagramSize < MAX_DATAGRAM_SIZE) {
    // Handled condition, not a fault: packetizeFrame sizes chunks to the
    // actual path limit (Firefox negotiates 1024), so nothing is
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

// One keyframe delivered over a reliable unidirectional stream.
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

// Per-session tallies of the reliable-carrier streams, owned by the transport
// and surfaced in ViewerStats. streamsAborted counts carriers ending in a
// reset — the relay CancelWrite-ing a stalled or superseded GOP tail;
// malformed counts framing violations (bad prologue, bad record length,
// truncated mid-record).
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
  // This session's telemetry identity, arriving on its own reliable uni
  // stream. Optional — an older relay, or one with telemetry off, sends none,
  // and the correct client behaviour is then to collect nothing rather than
  // to wait for a message that will not come.
  onTelemetryHello?: (hello: TelemetryHelloMessage) => void;
  // The relay-advertised telemetry ingest URL (0x12).
  onTelemetryEndpoint?: (url: string) => void;
  // The relay's capabilities. Also the striping version-skew gate: striping
  // engages only after CAP_STRIPED_DELIVERY is seen, so an older relay (which
  // never sends it) is never dialed for legs.
  onRelayCapabilities?: (caps: RelayCapabilities) => void;
  // The close code the relay is about to close this session with, stated
  // in-band because Chrome never reads a webtransport-go close code. The
  // consumer keeps it for when `closed` settles without one.
  onSessionClosing?: (code: number) => void;
  // R74: a PoP's LocalRelayInfo (0x18) or home's LocalRelayOffer (0x19).
  // Optional: without it both are read and dropped, never counted malformed.
  onLocalRelay?: (ev: LocalRelayEvent) => void;
  // One verbatim datagram record off a reliable carrier stream. The
  // transport feeds it into the same handler as a received datagram — the
  // whole point of the carrier design.
  onCarrierRecord: (record: Uint8Array<ArrayBuffer>) => void;
}

// Test seam (undefined in production). Samples the size of readServerStreams'
// in-flight stream set after every add and every settle, so a test can assert
// the working set never grows unbounded.
export interface ReadServerStreamsHooks {
  onInFlightChange?: (count: number) => void;
}

// Reads server-initiated unidirectional streams for the life of the session,
// dispatching each by its stream-kind bytes: a keyframe StreamFrame message
// (version‖0x04 — read to EOF, bounded by MAX_KEYFRAME_BYTES), a reliable
// carrier (version‖0x0A — a long-lived record loop), or a small one-shot
// control message (telemetry hello/endpoint, relay capabilities). Each stream
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
  // Track only the streams still being read. An hours-long mobile session
  // accepts thousands of short-lived streams (keyframe + carrier per GOP);
  // pruning each on settle keeps the working set proportional to open streams
  // (~2), not to session length.
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
    if (
      head0 !== WIRE_VERSION ||
      (head1 !== TYPE_STREAM_FRAME &&
        head1 !== TYPE_RELIABLE_CARRIER &&
        head1 !== TYPE_TELEMETRY_HELLO &&
        head1 !== TYPE_TELEMETRY_ENDPOINT &&
        head1 !== TYPE_RELAY_CAPABILITIES &&
        head1 !== TYPE_SESSION_CLOSING &&
        !isLocalRelayType(head1))
    ) {
      // Unknown stream kind or version: cancel without wedging the accept
      // loop. Counted as malformed so it is visible in stats.
      carrier.malformed++;
      log.warn(`unknown server stream kind 0x${head0.toString(16)} 0x${head1.toString(16)}; cancelling`);
      void reader.cancel().catch(() => {});
      return;
    }

    if (head1 === TYPE_TELEMETRY_HELLO) {
      // Fixed 35 bytes; read to EOF (it is a whole stream) and parse strictly.
      // A malformed hello costs telemetry for this session and nothing else —
      // it must never disturb the media path, so it is counted as malformed
      // and dropped like any other unreadable server stream.
      for (;;) {
        if (total > TELEMETRY_HELLO_SIZE) break;
        if (!(await readMore())) break;
      }
      try {
        cb.onTelemetryHello?.(parseTelemetryHello(concatChunks(chunks, total)));
      } catch (e) {
        carrier.malformed++;
        log.warn('telemetry hello unreadable; this session will not report:', e);
      }
      return;
    }

    if (head1 === TYPE_TELEMETRY_ENDPOINT) {
      // Variable length, bounded by the wire cap; read to EOF and parse
      // leniently — a malformed endpoint costs only the advertised
      // destination (reporting stays on the configured URL), never the media
      // path.
      for (;;) {
        if (total > 5 + TELEMETRY_ENDPOINT_MAX_URL_LEN + 64) break;
        if (!(await readMore())) break;
      }
      try {
        cb.onTelemetryEndpoint?.(parseTelemetryEndpoint(concatChunks(chunks, total)));
      } catch (e) {
        carrier.malformed++;
        log.warn('telemetry endpoint unreadable; reporting stays on the configured URL:', e);
      }
      return;
    }

    if (head1 === TYPE_RELAY_CAPABILITIES) {
      // Fixed 5 bytes; read to EOF and parse strictly. Best-effort like the
      // hello: an unreadable capabilities message costs striping (and the
      // parity-level display) for this session, never the media path.
      for (;;) {
        if (total > RELAY_CAPABILITIES_SIZE) break;
        if (!(await readMore())) break;
      }
      try {
        cb.onRelayCapabilities?.(parseRelayCapabilities(concatChunks(chunks, total)));
      } catch (e) {
        carrier.malformed++;
        log.warn('relay capabilities unreadable; striping stays unavailable:', e);
      }
      return;
    }

    if (head1 === TYPE_SESSION_CLOSING) {
      // Fixed 6 bytes; read to EOF and parse strictly. An unreadable
      // notice costs only the reason shown for the close, never media.
      for (;;) {
        if (total > SESSION_CLOSING_SIZE) break;
        if (!(await readMore())) break;
      }
      try {
        cb.onSessionClosing?.(parseSessionClosing(concatChunks(chunks, total)));
      } catch (e) {
        carrier.malformed++;
        log.warn('session closing notice unreadable:', e);
      }
      return;
    }

    if (isLocalRelayType(head1)) {
      // Variable length, small; read to EOF (bounded) and parse. An
      // unreadable one costs only the local relay hint, never media.
      for (;;) {
        if (total > LOCAL_RELAY_STREAM_MAX_BYTES) {
          void reader.cancel().catch(() => {});
          carrier.malformed++;
          return;
        }
        if (!(await readMore())) break;
      }
      try {
        const ev = parseLocalRelayEvent(concatChunks(chunks, total));
        cb.onLocalRelay?.(ev);
      } catch (e) {
        carrier.malformed++;
        log.warn('local relay message unreadable:', e);
      }
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

    // Reliable carrier: a record loop for the life of the stream. The
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

// Opens a writer on the session's outgoing datagram stream. The deprecated
// `datagrams.writable` attribute is preferred where it exists (Chromium,
// Firefox), keeping them on their proven path; WebKit implements only the
// current-spec `datagrams.createWritable()` and has no `writable` at all.
// Null when neither exists (test fakes).
export interface DatagramWritableSource {
  datagrams?: {
    writable?: WritableStream<BufferSource>;
    createWritable?: () => WritableStream<BufferSource>;
  };
}

export function openDatagramWriter(
  wt: WebTransport | DatagramWritableSource,
): WritableStreamDefaultWriter<BufferSource> | null {
  const { datagrams } = wt as DatagramWritableSource;
  if (datagrams?.writable) return datagrams.writable.getWriter();
  if (typeof datagrams?.createWritable === 'function') return datagrams.createWritable().getWriter();
  return null;
}

// What a send carries, which decides whether it may be dropped for age
// (docs/75 RU9). Deltas carry the GOP they belong to: the broadcaster
// numbers GOPs at each keyframe it produces. Keyframes never come through
// here — they ride their own streams (keyframe-sender.ts).
//  - 'delta': dropped once more than one GOP old, with the rest of its GOP.
//  - 'audio': dropped once more than one GOP old, alone (a lost audio
//    datagram is already survivable).
//  - 'control': never dropped for age — the audio config and the clock
//    mapping must arrive, and a late TimeSync ping is only a slow sample,
//    which its estimator (lowest RTT wins) ignores.
export type SendKind = 'delta' | 'audio' | 'control';

export interface SendTag {
  kind: SendKind;
  gop?: number;
}

// 'dropped' is a frame the sender refused as stale. It is not a failure:
// the session is fine and the frames behind it still go out. Callers count
// it apart and never as sent.
export type SendResult = 'sent' | 'dropped';

// The default age bound: one GOP (DEFAULT_CAPTURE_CONFIG.keyframeIntervalMs).
export const DEFAULT_SEND_MAX_AGE_MS = 500;

export interface DatagramSenderOptions {
  // The clock a frame's age is read from (performance.now by default).
  now?: () => number;
  // How old a delta or audio packet may be when its turn comes.
  maxAgeMs?: number;
  // Whether a GOP's deltas must give way to its keyframe: it waits behind a
  // late one, or did not go out at all (KeyframeSender.deltasBlocked).
  gopBlocked?: (gop: number) => boolean;
}

interface QueuedSend {
  datagrams: Uint8Array<ArrayBuffer>[];
  tag: SendTag;
  enqueuedAt: number;
  resolve: (r: SendResult) => void;
  reject: (e: Error) => void;
}

// Serializes datagram writes so frames leave in encode order (datagram
// delivery is unordered anyway, but bursting writes out of order would
// guarantee reordering instead of merely permitting it), and bounds how long
// a frame may wait for its turn (docs/75 RU9). The backlog of a link slower
// than the stream sits here — Chrome takes one datagram at a time — so this
// is where it is cut: whatever Chrome has already accepted it still sends.
export class DatagramSender {
  private writer: WritableStreamDefaultWriter<BufferSource>;
  private queue: QueuedSend[] = [];
  private pumping = false;
  private failed: Error | null = null;
  private now: () => number;
  private maxAgeMs: number;
  private gopBlocked: ((gop: number) => boolean) | null;
  // The GOP whose deltas are being dropped: once one delta of a GOP is, the
  // rest of that GOP follows it until the next keyframe's GOP.
  private droppingGop: number | null = null;

  constructor(wt: WebTransport, opts: DatagramSenderOptions = {}) {
    const writer = openDatagramWriter(wt);
    if (!writer) throw new Error('WebTransport session has no writable datagram stream');
    this.writer = writer;
    this.now = opts.now ?? (() => performance.now());
    this.maxAgeMs = opts.maxAgeMs ?? DEFAULT_SEND_MAX_AGE_MS;
    this.gopBlocked = opts.gopBlocked ?? null;
  }

  // Queues one frame's datagrams. Resolves 'sent' once they have all been
  // handed to the browser's datagram queue, or 'dropped' if the frame was
  // stale when its turn came; rejects (and latches the failure) only if the
  // session is gone.
  send(datagrams: Uint8Array<ArrayBuffer>[], tag: SendTag): Promise<SendResult> {
    if (this.failed) return Promise.reject(this.failed);
    return new Promise<SendResult>((resolve, reject) => {
      this.queue.push({ datagrams, tag, enqueuedAt: this.now(), resolve, reject });
      void this.pump();
    });
  }

  private async pump(): Promise<void> {
    if (this.pumping) return;
    this.pumping = true;
    try {
      for (let next = this.queue.shift(); next; next = this.queue.shift()) {
        if (this.failed) {
          next.reject(this.failed);
          continue;
        }
        // Decided once, when the frame's first datagram comes up, so a frame
        // is never half-sent.
        if (this.isStale(next)) {
          next.resolve('dropped');
          continue;
        }
        try {
          for (const d of next.datagrams) await this.writer.write(d);
          next.resolve('sent');
        } catch (e) {
          this.failed ??= e instanceof Error ? e : new Error(String(e));
          next.reject(this.failed);
        }
      }
    } finally {
      this.pumping = false;
    }
  }

  private isStale(q: QueuedSend): boolean {
    const { kind, gop } = q.tag;
    if (kind === 'control') return false;
    const old = this.now() - q.enqueuedAt > this.maxAgeMs;
    if (kind === 'audio' || gop === undefined) return old;
    if (gop === this.droppingGop) return true;
    if (old || this.gopBlocked?.(gop)) {
      this.droppingGop = gop;
      return true;
    }
    return false;
  }

  // Releases the writer. Nothing still queued is sent: those sends reject,
  // as any send on a dead session does (a resume builds a new sender).
  close(): void {
    this.failed ??= new Error('datagram sender closed');
    for (const q of this.queue.splice(0)) q.reject(this.failed);
    this.writer.releaseLock();
  }
}
