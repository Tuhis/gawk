// @vitest-environment jsdom
// The probe against a scripted transport: jsdom has no WebTransport, and
// these behaviours (median over lossy samples,
// identity-late, degraded identity) need deterministic control anyway.
import { describe, expect, it } from 'vitest';

import { probeRelay, sanitizeIdentityName, type ProbeTransport } from './probe';
import { encodeRelayIdentity } from '../../transport/wire';

// A scripted echo peer: echoes a configurable subset of datagrams after a
// per-ping delay, and optionally delivers an identity payload after
// identityDelayMs.
function fakeTransport(opts: {
  echoDelayMs?: number;
  dropPings?: number[];
  identity?: Uint8Array;
  identityDelayMs?: number;
  // Uni streams the relay opens before the identity one (R74: a PoP also
  // sends 0x18 LocalRelayInfo on /echo, in no promised order).
  before?: Uint8Array[];
  failReady?: boolean;
  // WebKit shape: no `datagrams.writable`, only `createWritable()`.
  webkit?: boolean;
}): ProbeTransport {
  const echoDelay = opts.echoDelayMs ?? 5;
  const drop = new Set(opts.dropPings ?? []);
  let echoController: ReadableStreamDefaultController<Uint8Array>;
  const echoReadable = new ReadableStream<Uint8Array>({
    start(c) {
      echoController = c;
    },
  });
  let pingIndex = 0;
  const writable = new WritableStream<BufferSource>({
    write(chunk) {
      const i = pingIndex++;
      if (drop.has(i)) return;
      const copy = new Uint8Array(chunk as ArrayBuffer);
      setTimeout(() => {
        try {
          echoController.enqueue(copy);
        } catch {
          // closed
        }
      }, echoDelay);
    },
  });

  const uniStreams = new ReadableStream<ReadableStream<Uint8Array>>({
    start(c) {
      for (const payload of opts.before ?? []) {
        c.enqueue(
          new ReadableStream<Uint8Array>({
            start(sc) {
              sc.enqueue(payload);
              sc.close();
            },
          }),
        );
      }
      if (opts.identity) {
        const payload = opts.identity;
        setTimeout(() => {
          try {
            c.enqueue(
              new ReadableStream<Uint8Array>({
                start(sc) {
                  sc.enqueue(payload);
                  sc.close();
                },
              }),
            );
          } catch {
            // closed
          }
        }, opts.identityDelayMs ?? 0);
      }
    },
  });

  return {
    ready: opts.failReady ? Promise.reject(new Error('refused')) : Promise.resolve(),
    closed: new Promise(() => {}),
    datagrams: opts.webkit
      ? { createWritable: () => writable, readable: echoReadable }
      : { writable, readable: echoReadable },
    incomingUnidirectionalStreams: uniStreams,
    close() {
      try {
        echoController.close();
      } catch {
        // already closed
      }
    },
  };
}

describe('probeRelay', () => {
  it('reports median RTT and identity on a clean session', async () => {
    const identity = encodeRelayIdentity({ serverVersion: '9.9.9', name: 'Homelab' });
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ identity, echoDelayMs: 10 }),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.rttMs).toBeGreaterThan(0);
    // notices 0: this relay says nothing beyond its name (docs/71 §9.3).
    expect(result.identity).toEqual({ serverVersion: '9.9.9', name: 'Homelab', notices: 0 });
  });

  // WebKit has no `datagrams.writable`, only `createWritable()`; a probe that
  // requires `writable` reports every relay as failed on Safari.
  it('measures RTT over createWritable() on WebKit', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ webkit: true, echoDelayMs: 10 }),
    );
    expect(result.state).toBe('ok');
  });

  it('computes the median over lossy samples', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ dropPings: [1, 3] }),
    );
    expect(result.state).toBe('ok');
  });

  // Echoes finish long before the identity stream arrives; the wait window
  // must still capture it.
  it('captures identity arriving after the echoes complete', async () => {
    const identity = encodeRelayIdentity({ serverVersion: '9.9.9', name: 'Late' });
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ identity, identityDelayMs: 900, echoDelayMs: 1 }),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.identity?.name).toBe('Late');
  });

  it('reports ok without identity from a pre-R37 relay', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({}),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.identity).toBe(null);
  });

  // A malformed identity degrades to "no identity", never a failed probe.
  it('degrades a malformed identity to null', async () => {
    const junk = new Uint8Array([0x01, 0x11, 0xff, 0x00]);
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ identity: junk }),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.identity).toBe(null);
  });

  it('reports the combined failure state when the connect fails', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ failReady: true }),
    );
    expect(result).toEqual({ state: 'failed' });
  });

  it('reports failure when every echo is lost', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ dropPings: [0, 1, 2, 3, 4] }),
    );
    expect(result).toEqual({ state: 'failed' });
  }, 10000);
});

// Control and bidi-control characters are stripped; the host is always
// rendered alongside by the caller, so this only cleans the string.
describe('sanitizeIdentityName', () => {
  it('strips control and bidi characters', () => {
    expect(sanitizeIdentityName('Off‮icial relay⁦ ✓')).toBe('Official relay ✓');
    expect(sanitizeIdentityName('‏‎plain')).toBe('plain');
  });

  it('keeps ordinary unicode', () => {
    expect(sanitizeIdentityName('Süd-Homelab 🎥')).toBe('Süd-Homelab 🎥');
  });
});

// R74: a relay may open other uni streams on /echo before its identity, so
// the probe picks the RelayIdentity out by its type byte, not by order.
describe('probeRelay stream order (R74)', () => {
  it('finds the identity behind other server streams', async () => {
    const identity = encodeRelayIdentity({ serverVersion: '9.9.9', name: 'PoP' });
    // A 0x18-typed message (LocalRelayInfo vector B) and an unknown kind.
    const localRelayInfo = Uint8Array.from([1, 0x18, 0, 0, 0, 0, 0]);
    const unknown = Uint8Array.from([1, 0x7f, 0]);
    const result = await probeRelay('https://192.168.1.20:4433', '', undefined, () =>
      fakeTransport({ identity, before: [localRelayInfo, unknown], echoDelayMs: 5 }),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.identity).toMatchObject({ name: 'PoP' });
  });

  it('degrades to no identity when none of the streams is one', async () => {
    const result = await probeRelay('https://192.168.1.20:4433', '', undefined, () =>
      fakeTransport({ before: [Uint8Array.from([1, 0x18, 0, 0, 0, 0, 0])], echoDelayMs: 5 }),
    );
    expect(result.state).toBe('ok');
    if (result.state !== 'ok') return;
    expect(result.identity).toBe(null);
  });

  it('hands the whole hash list to the transport', async () => {
    const seen: string[] = [];
    const h = `${'11'.repeat(32)},${'22'.repeat(32)}`;
    await probeRelay('https://192.168.1.20:4433', h, undefined, (url, hashes) => {
      seen.push(url, hashes);
      return fakeTransport({ echoDelayMs: 1 });
    });
    expect(seen).toEqual(['https://192.168.1.20:4433/echo', h]);
  });
});
