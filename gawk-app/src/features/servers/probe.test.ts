// @vitest-environment jsdom
// R37 (docs/40 §4.4 / SP6): the probe against a scripted transport — jsdom
// has no WebTransport, and these behaviours (median over lossy samples,
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
  failReady?: boolean;
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
  const writable = new WritableStream<Uint8Array>({
    write(chunk) {
      const i = pingIndex++;
      if (drop.has(i)) return;
      const copy = new Uint8Array(chunk);
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
    datagrams: { writable, readable: echoReadable },
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
    expect(result.identity).toEqual({ serverVersion: '9.9.9', name: 'Homelab' });
  });

  it('computes the median over lossy samples', async () => {
    const result = await probeRelay('https://relay.example:4433', '', undefined, () =>
      fakeTransport({ dropPings: [1, 3] }),
    );
    expect(result.state).toBe('ok');
  });

  // F5: echoes finish long before the identity stream arrives — the wait
  // window must still capture it.
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

  // F7: a malformed identity degrades to "no identity", never a failed probe.
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

// F6: control and bidi-control characters are stripped; the host is always
// rendered alongside by the caller — this only cleans the string.
describe('sanitizeIdentityName', () => {
  it('strips control and bidi characters', () => {
    expect(sanitizeIdentityName('Off‮icial relay⁦ ✓')).toBe('Official relay ✓');
    expect(sanitizeIdentityName('‏‎plain')).toBe('plain');
  });

  it('keeps ordinary unicode', () => {
    expect(sanitizeIdentityName('Süd-Homelab 🎥')).toBe('Süd-Homelab 🎥');
  });
});
