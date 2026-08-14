// R37 (docs/40 §4.4): the server probe — validity + latency over the relay's
// existing WebTransport /echo route, plus the RelayIdentity the relay sends
// on a uni stream at session start.
//
// RTT is measured client-side by round-tripping small datagrams and taking
// the median of the successful samples. The session is held open until the
// identity stream has been read OR an identity deadline passes, whichever
// comes first — on a fast link the echoes complete before the relay's uni
// stream arrives, and closing on RTT alone would nondeterministically label
// a current relay as pre-R37 (F5).
//
// Failure is ONE combined state: browsers surface WebTransport failures
// opaquely, so "unreachable", "not a gawk relay", "invalid certificate" and
// "origin not allowed" are indistinguishable from JS. The UI copy names all
// causes once (docs/40 §4.4) rather than pretending to diagnose.

import { parseRelayIdentity, type RelayIdentityMessage } from '../../transport/wire';
import { hexToBytes } from '../../transport/connection';

export const PROBE_SAMPLES = 5;
export const PROBE_SAMPLE_SPACING_MS = 120;
export const PROBE_CONNECT_TIMEOUT_MS = 4000;
// F5: measured from session ready — long enough for one RTT plus relay
// scheduling, short enough that the panel never feels stuck.
export const PROBE_IDENTITY_DEADLINE_MS = 1500;

export interface ProbeSuccess {
  state: 'ok';
  rttMs: number;
  identity: RelayIdentityMessage | null;
}

export interface ProbeFailure {
  state: 'failed';
}

export type ProbeResult = ProbeSuccess | ProbeFailure;

// The WebTransport surface the probe needs — injectable so unit tests can
// drive echoes/identity deterministically (jsdom has no WebTransport).
export interface ProbeTransport {
  ready: Promise<void>;
  closed: Promise<unknown>;
  datagrams: {
    writable: WritableStream<Uint8Array>;
    readable: ReadableStream<Uint8Array>;
  };
  incomingUnidirectionalStreams: ReadableStream<ReadableStream<Uint8Array>>;
  close(): void;
}

export type ProbeTransportFactory = (url: string, certHashHex: string) => ProbeTransport;

function defaultTransportFactory(url: string, certHashHex: string): ProbeTransport {
  const init: WebTransportOptions = {
    requireUnreliable: true,
    congestionControl: 'low-latency',
  };
  const hash = certHashHex.trim();
  if (hash) {
    init.serverCertificateHashes = [{ algorithm: 'sha-256', value: hexToBytes(hash) }];
  }
  return new WebTransport(url, init) as unknown as ProbeTransport;
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('probe timeout')), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

// One echo session: connect, round-trip PROBE_SAMPLES datagrams, read the
// identity stream if one arrives inside the deadline, close, report.
export async function probeRelay(
  url: string,
  certHashHex: string,
  now: () => number = () => performance.now(),
  factory: ProbeTransportFactory = defaultTransportFactory,
): Promise<ProbeResult> {
  let wt: ProbeTransport;
  try {
    wt = factory(`${url}/echo`, certHashHex);
  } catch {
    return { state: 'failed' };
  }
  try {
    await withTimeout(wt.ready, PROBE_CONNECT_TIMEOUT_MS);
  } catch {
    try {
      wt.close();
    } catch {
      // A transport that failed to open has nothing to close.
    }
    return { state: 'failed' };
  }

  const readyAt = now();

  // Identity: first uni stream, read fully, parsed leniently — a malformed
  // message degrades to "no identity", never to a failed probe (F7).
  const identityPromise: Promise<RelayIdentityMessage | null> = (async () => {
    try {
      const streams = wt.incomingUnidirectionalStreams.getReader();
      const { value: stream, done } = await streams.read();
      streams.releaseLock();
      if (done || !stream) return null;
      const reader = stream.getReader();
      const parts: Uint8Array[] = [];
      let total = 0;
      for (;;) {
        const { value, done: streamDone } = await reader.read();
        if (streamDone) break;
        parts.push(value);
        total += value.length;
        if (total > 4096) return null; // no legitimate identity is this large
      }
      const buf = new Uint8Array(total);
      let off = 0;
      for (const p of parts) {
        buf.set(p, off);
        off += p.length;
      }
      return parseRelayIdentity(buf);
    } catch {
      return null;
    }
  })();

  // RTT: send stamped pings, read echoes, median of successes.
  const rtts: number[] = [];
  try {
    const writer = wt.datagrams.writable.getWriter();
    const reader = wt.datagrams.readable.getReader();
    const echoReads = (async () => {
      for (let i = 0; i < PROBE_SAMPLES; i++) {
        const { value, done } = await reader.read();
        if (done || !value || value.length < 10) return;
        const view = new DataView(value.buffer, value.byteOffset, value.byteLength);
        const sentAt = view.getFloat64(2);
        rtts.push(now() - sentAt);
      }
    })();
    for (let i = 0; i < PROBE_SAMPLES; i++) {
      const ping = new Uint8Array(10);
      const view = new DataView(ping.buffer);
      // Not a wire message: /echo reflects arbitrary bytes. 0x00 in the
      // version slot guarantees no parser anywhere mistakes it for one.
      ping[0] = 0x00;
      ping[1] = i;
      view.setFloat64(2, now());
      await writer.write(ping);
      if (i < PROBE_SAMPLES - 1) await sleep(PROBE_SAMPLE_SPACING_MS);
    }
    await Promise.race([echoReads, sleep(1000)]);
    writer.releaseLock();
  } catch {
    // Datagram plumbing died mid-probe: fall through — zero samples reads
    // as failure below.
  }

  // F5: hold for identity up to its deadline, measured from ready.
  const identity = await Promise.race([
    identityPromise,
    sleep(Math.max(0, PROBE_IDENTITY_DEADLINE_MS - (now() - readyAt))).then(() => null),
  ]);

  try {
    wt.close();
  } catch {
    // Already closed by the peer — the samples still stand.
  }

  if (rtts.length === 0) return { state: 'failed' };
  const sorted = [...rtts].sort((a, b) => a - b);
  return {
    state: 'ok',
    rttMs: sorted[Math.floor(sorted.length / 2)],
    identity,
  };
}

// F6: the display-name sanitizer. The name is attacker-influenced trust UI —
// strip control and bidi-control characters; rendering ALWAYS pairs the
// result with the normalized host (the caller's job), never replaces it.
export function sanitizeIdentityName(name: string): string {
  let out = '';
  for (const ch of name) {
    const code = ch.codePointAt(0)!;
    const isControl = code < 0x20 || code === 0x7f;
    const isBidi =
      code === 0x200e ||
      code === 0x200f ||
      (code >= 0x202a && code <= 0x202e) ||
      (code >= 0x2066 && code <= 0x2069);
    if (!isControl && !isBidi) out += ch;
  }
  return out.trim();
}
