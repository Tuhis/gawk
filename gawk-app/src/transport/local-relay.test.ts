// R74 (docs/76 §6.12): every session kind receives LocalRelayInfo (0x18) from
// a PoP and LocalRelayOffer (0x19) from home, each on its own server uni
// stream. Before R74 the viewer's allowlist cancelled both and counted them
// malformed; these pin that they reach their own callback instead, and never
// the media path.

import { describe, expect, it } from 'vitest';

import { newCarrierCounters, readServerStreams } from './connection';
import { readLocalRelayStreams, parseLocalRelayEvent, type LocalRelayEvent } from './local-relay';
import {
  WireError,
  encodeLocalRelayInfo,
  encodeLocalRelayOffer,
  encodeSessionClosing,
  type LocalRelayInfoMessage,
  type LocalRelayOfferMessage,
} from './wire';

const INFO: LocalRelayInfoMessage = {
  linked: true,
  onWall: true,
  name: 'Assembly 2026',
  homeOrigin: 'https://gawk.ioio.fi',
  wallCode: 'assembly-2026',
};

const OFFER: LocalRelayOfferMessage = {
  handle: '6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab',
  name: 'Assembly 2026',
  validity: 3600,
  candidates: [
    { url: 'https://p-7k2m9x.pop.gawk.ioio.fi:4433', hashes: [] },
    { url: 'https://192.168.1.20:4433', hashes: [new Uint8Array(32).fill(1), new Uint8Array(32).fill(2)] },
  ],
};

function wtWithStreams(streams: Uint8Array[][]): WebTransport {
  const incoming = new ReadableStream<ReadableStream<Uint8Array>>({
    start(controller) {
      for (const chunks of streams) {
        controller.enqueue(
          new ReadableStream<Uint8Array>({
            start(inner) {
              for (const c of chunks) inner.enqueue(c);
              inner.close();
            },
          }),
        );
      }
      controller.close();
    },
  });
  return { incomingUnidirectionalStreams: incoming } as unknown as WebTransport;
}

const noMedia = {
  onKeyframe: () => {
    throw new Error('not a keyframe');
  },
  onCarrierRecord: () => {
    throw new Error('not a carrier record');
  },
};

describe('parseLocalRelayEvent', () => {
  it('tells the two messages apart by their type byte', () => {
    expect(parseLocalRelayEvent(encodeLocalRelayInfo(INFO))).toEqual({ kind: 'info', info: INFO });
    const ev = parseLocalRelayEvent(encodeLocalRelayOffer(OFFER));
    expect(ev.kind).toBe('offer');
    expect(ev.kind === 'offer' && ev.offer.candidates[1].hashes).toHaveLength(2);
  });

  it('rejects any other message', () => {
    expect(() => parseLocalRelayEvent(encodeSessionClosing(4000))).toThrow(WireError);
  });
});

describe('readServerStreams — LocalRelayInfo / LocalRelayOffer (R74)', () => {
  it('dispatches both to onLocalRelay, never to the media path or the malformed count', async () => {
    const offer = encodeLocalRelayOffer(OFFER);
    // The offer split across reads, as QUIC may deliver it.
    const wt = wtWithStreams([[encodeLocalRelayInfo(INFO)], [offer.subarray(0, 1), offer.subarray(1, 40), offer.subarray(40)]]);
    const seen: LocalRelayEvent[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(wt, { ...noMedia, onLocalRelay: (ev) => seen.push(ev) }, counters);
    expect(seen.map((e) => e.kind).sort()).toEqual(['info', 'offer']);
    expect(seen.find((e) => e.kind === 'info')).toEqual({ kind: 'info', info: INFO });
    expect(counters.malformed).toBe(0);
  });

  it('reads them quietly when the consumer does not want them', async () => {
    const wt = wtWithStreams([[encodeLocalRelayInfo(INFO)], [encodeLocalRelayOffer(OFFER)]]);
    const counters = newCarrierCounters();
    await readServerStreams(wt, noMedia, counters);
    expect(counters.malformed).toBe(0);
  });

  it('counts an unreadable one as malformed and keeps going', async () => {
    const bad = encodeLocalRelayOffer(OFFER);
    bad[2] = 1; // the strict flags byte
    const wt = wtWithStreams([[bad], [encodeLocalRelayInfo(INFO)]]);
    const seen: LocalRelayEvent[] = [];
    const counters = newCarrierCounters();
    await readServerStreams(wt, { ...noMedia, onLocalRelay: (ev) => seen.push(ev) }, counters);
    expect(seen).toEqual([{ kind: 'info', info: INFO }]);
    expect(counters.malformed).toBe(1);
  });
});

// The room control session reads no media streams: before R74 it never read
// its uni streams at all.
describe('readLocalRelayStreams', () => {
  it('dispatches 0x18 and 0x19 and skips every other kind', async () => {
    const wt = wtWithStreams([
      [encodeSessionClosing(4002)],
      [encodeLocalRelayInfo(INFO)],
      [encodeLocalRelayOffer(OFFER)],
    ]);
    const seen: LocalRelayEvent[] = [];
    await readLocalRelayStreams(wt, (ev) => seen.push(ev));
    expect(seen.map((e) => e.kind).sort()).toEqual(['info', 'offer']);
  });
});
