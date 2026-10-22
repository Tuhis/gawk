// R74 (docs/76 §6.12): the two session-start messages about local relays.
// LocalRelayInfo (0x18) comes from a PoP and says what it is: its name,
// whether it is linked to a home fleet, the home app origin and the party
// wall. LocalRelayOffer (0x19) comes from home and suggests a PoP on the
// client's own network. Each rides its own server-opened uni stream right
// after the upgrade on /echo, /publish, /subscribe and /room, so every
// session reader dispatches them here, and what to do about them is the
// main thread's business (features/pop).

import { log } from '../lib/logger';
import {
  TYPE_LOCAL_RELAY_INFO,
  TYPE_LOCAL_RELAY_OFFER,
  WIRE_VERSION,
  WireError,
  parseLocalRelayInfo,
  parseLocalRelayOffer,
  type LocalRelayInfoMessage,
  type LocalRelayOfferMessage,
} from './wire';

// Plain data on purpose: the event crosses from the viewer workers to the
// main thread by structured clone.
export type LocalRelayEvent =
  | { kind: 'info'; info: LocalRelayInfoMessage }
  | { kind: 'offer'; offer: LocalRelayOfferMessage };

// Far past either message's largest legal size (an offer of four candidates
// with 256-byte URLs and two hashes each is under 1.5 KB); a stream longer
// than this is cancelled, not buffered.
export const LOCAL_RELAY_STREAM_MAX_BYTES = 4096;

export function isLocalRelayType(type: number): boolean {
  return type === TYPE_LOCAL_RELAY_INFO || type === TYPE_LOCAL_RELAY_OFFER;
}

// Throws WireError for anything that is not a well-formed 0x18 or 0x19.
export function parseLocalRelayEvent(msg: Uint8Array): LocalRelayEvent {
  if (msg.length < 2 || msg[0] !== WIRE_VERSION || !isLocalRelayType(msg[1])) {
    throw new WireError('not a local relay message');
  }
  return msg[1] === TYPE_LOCAL_RELAY_INFO
    ? { kind: 'info', info: parseLocalRelayInfo(msg) }
    : { kind: 'offer', offer: parseLocalRelayOffer(msg) };
}

// A whole uni stream, or null once it passes `limit` bytes (then cancelled).
export async function readWholeStream(
  stream: ReadableStream<Uint8Array>,
  limit: number,
): Promise<Uint8Array<ArrayBuffer> | null> {
  const reader = stream.getReader();
  const parts: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      if (!value || value.length === 0) continue;
      parts.push(value);
      total += value.length;
      if (total > limit) {
        void reader.cancel().catch(() => {});
        return null;
      }
    }
  } finally {
    reader.releaseLock();
  }
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

// For a session that reads no media (the room control session): every
// server uni stream is read, bounded, and only 0x18/0x19 are dispatched. The
// rest are dropped, as a room session always ignored them. Returns when the
// session ends or `signal` aborts.
export async function readLocalRelayStreams(
  wt: WebTransport,
  onEvent: (ev: LocalRelayEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  let reader: ReadableStreamDefaultReader<ReadableStream<Uint8Array>>;
  try {
    reader = wt.incomingUnidirectionalStreams.getReader();
  } catch {
    return; // a test double without uni streams
  }
  const onAbort = () => void reader.cancel().catch(() => {});
  signal?.addEventListener('abort', onAbort, { once: true });
  const tasks: Promise<void>[] = [];
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done || !value) break;
      tasks.push(
        readWholeStream(value, LOCAL_RELAY_STREAM_MAX_BYTES).then(
          (buf) => {
            if (buf === null || buf.length < 2 || !isLocalRelayType(buf[1])) return;
            try {
              onEvent(parseLocalRelayEvent(buf));
            } catch (e) {
              log.warn('local relay message unreadable:', e);
            }
          },
          () => {},
        ),
      );
    }
  } catch {
    // the session ended
  } finally {
    signal?.removeEventListener('abort', onAbort);
    reader.releaseLock();
    await Promise.allSettled(tasks);
  }
}
