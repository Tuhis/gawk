// R29 finding 2 (docs/34): the viewer's incoming-datagram buffer knob.
//
// The bug this guards is not "datagrams were lost" but "they were lost in a
// way parity structurally cannot repair": the WebTransport receive queue drops
// from the HEAD when it overflows, so a frame's burst loses its earliest data
// chunks while the parity symbols written last always survive. Measured on a
// live Firefox session: 10.5 % of delta chunks gone, 0.05 % of parity.
//
// Every case below is about the reporting being honest, because the whole
// point of the gate is telling "set and honored" from "set and ignored" — a
// silent no-op is exactly how this stayed invisible for a release.

import { describe, expect, it } from 'vitest';

import {
  INCOMING_DATAGRAM_BUFFER,
  applyIncomingDatagramBuffer,
  type DatagramBufferStats,
} from './datagram-buffer';

// A stand-in for WebTransportDatagramDuplexStream that stores the attribute on
// its PROTOTYPE, the way a real browser does — `in` has to see through the
// chain, and a plain object literal would let a broken check pass.
function datagramsWith(props: Record<string, number>): object {
  const proto = Object.create(null) as Record<string, number>;
  for (const [k, v] of Object.entries(props)) proto[k] = v;
  return Object.create(proto) as object;
}

describe('applyIncomingDatagramBuffer', () => {
  it('raises the spec-named attribute and reports it applied', () => {
    const dg = datagramsWith({ incomingMaxBufferedDatagrams: 8 });
    const stats = applyIncomingDatagramBuffer(dg, 256);
    expect(stats).toEqual<DatagramBufferStats>({
      property: 'incomingMaxBufferedDatagrams',
      requested: 256,
      effective: 256,
      applied: true,
    });
    expect((dg as { incomingMaxBufferedDatagrams: number }).incomingMaxBufferedDatagrams).toBe(256);
  });

  // Firefox 154 — the browser the finding was measured on — ships only the
  // pre-rename name, and Chromium is removing it. Neither name alone covers
  // the fleet, so the fallback is the feature, not a nicety.
  it('falls back to the legacy attribute where only that exists', () => {
    const dg = datagramsWith({ incomingHighWaterMark: 1 });
    const stats = applyIncomingDatagramBuffer(dg, 256);
    expect(stats.property).toBe('incomingHighWaterMark');
    expect(stats.applied).toBe(true);
    expect((dg as { incomingHighWaterMark: number }).incomingHighWaterMark).toBe(256);
  });

  // They are aliases mid-rename: setting one is enough, and setting both would
  // report a `property` that doesn't say which one the browser actually reads.
  it('prefers the spec name when a browser exposes both', () => {
    const dg = datagramsWith({ incomingMaxBufferedDatagrams: 8, incomingHighWaterMark: 8 });
    const stats = applyIncomingDatagramBuffer(dg, 256);
    expect(stats.property).toBe('incomingMaxBufferedDatagrams');
    expect((dg as { incomingHighWaterMark: number }).incomingHighWaterMark).toBe(8);
  });

  it('reports unsupported without throwing where the browser exposes neither', () => {
    const stats = applyIncomingDatagramBuffer(datagramsWith({ maxDatagramSize: 1200 }), 256);
    expect(stats).toEqual<DatagramBufferStats>({
      property: null,
      requested: 256,
      effective: null,
      applied: false,
    });
  });

  it('tolerates a missing datagrams object entirely', () => {
    expect(applyIncomingDatagramBuffer(undefined, 256).property).toBeNull();
    expect(applyIncomingDatagramBuffer(null, 256).applied).toBe(false);
  });

  // The default is implementation-defined, so a browser may already buffer
  // more than we ask for. Clamping it DOWN would be this change causing the
  // very loss it exists to stop.
  it('never shrinks a default deeper than the request', () => {
    const dg = datagramsWith({ incomingMaxBufferedDatagrams: 1024 });
    const stats = applyIncomingDatagramBuffer(dg, 256);
    expect((dg as { incomingMaxBufferedDatagrams: number }).incomingMaxBufferedDatagrams).toBe(1024);
    expect(stats.effective).toBe(1024);
    expect(stats.applied).toBe(true);
  });

  // A UA is free to accept the assignment and ignore it. That reads identical
  // to success from the call site, and is precisely what the gate must expose
  // rather than paper over.
  it('reports not-applied when the browser accepts the assignment and ignores it', () => {
    const proto = {} as Record<string, unknown>;
    Object.defineProperty(proto, 'incomingMaxBufferedDatagrams', {
      get: () => 4,
      set: () => {},
      configurable: true,
    });
    const stats = applyIncomingDatagramBuffer(Object.create(proto) as object, 256);
    expect(stats.property).toBe('incomingMaxBufferedDatagrams');
    expect(stats.effective).toBe(4);
    expect(stats.applied).toBe(false);
  });

  it('survives a read-only attribute rather than failing the connect', () => {
    const proto = {} as Record<string, unknown>;
    Object.defineProperty(proto, 'incomingMaxBufferedDatagrams', {
      get: () => 4,
      set: () => {
        throw new TypeError('read only');
      },
      configurable: true,
    });
    const stats = applyIncomingDatagramBuffer(Object.create(proto) as object, 256);
    expect(stats.applied).toBe(false);
    expect(stats.effective).toBe(4);
  });

  // A burst is ~9-11 datagrams (a delta frame's chunks plus its two parity
  // symbols) and several frames can be in flight, so the default has to be a
  // large multiple of one burst to be worth setting at all.
  it('defaults to a depth of many frames worth of burst', () => {
    expect(INCOMING_DATAGRAM_BUFFER).toBeGreaterThanOrEqual(128);
    expect(applyIncomingDatagramBuffer(datagramsWith({ incomingMaxBufferedDatagrams: 1 })).effective).toBe(
      INCOMING_DATAGRAM_BUFFER,
    );
  });
});
