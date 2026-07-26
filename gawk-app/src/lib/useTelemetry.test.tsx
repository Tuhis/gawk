// @vitest-environment jsdom
//
// R28 TM2: the browser-side lifecycle around the collector — the
// `visibilitychange → hidden` beacon flush, and unmount ending the session.
// These are the two paths that only exist in a document, so they get a jsdom
// test rather than a pure-unit one.

import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_TELEMETRY_URL } from '../config';
import { useTelemetryCollector } from './useTelemetry';
import type { TelemetryCollector } from './telemetry';
import type { TelemetryHelloMessage } from '../transport/wire';

const HELLO: TelemetryHelloMessage = {
  enabled: true,
  reportIntervalMs: 500,
  token: '00012345000102030405060708090a0ba1a2a3a4a5a6a7a8',
  broadcastKey: '1a2b3c4d5e6f',
};

let beacons: { url: string; body: string }[] = [];
let fetches: string[] = [];

function Probe({ onReady }: { onReady: (c: TelemetryCollector<{ fps: number }>) => void }) {
  const collector = useTelemetryCollector<{ fps: number }>('viewer');
  onReady(collector);
  return null;
}

function setVisibility(state: 'visible' | 'hidden'): void {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}

beforeEach(() => {
  beacons = [];
  fetches = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    fetches.push(String(init?.body ?? ''));
    void url;
    return { ok: true } as Response;
  });
  Object.defineProperty(navigator, 'sendBeacon', {
    configurable: true,
    value: (url: string, blob: Blob) => {
      // Record synchronously; the body read is async but the URL and the fact
      // of the call are what the beacon path is about.
      beacons.push({ url, body: '' });
      void blob;
      return true;
    },
  });
  setVisibility('visible');
});

afterEach(() => {
  // Unmount first: a lingering component keeps its visibilitychange listener,
  // and the next test's 'hidden' would flush the previous session.
  cleanup();
  vi.unstubAllGlobals();
});

describe('useTelemetryCollector', () => {
  it('flushes through sendBeacon when the page is hidden', () => {
    let collector!: TelemetryCollector<{ fps: number }>;
    render(<Probe onReady={(c) => (collector = c)} />);
    collector.begin(HELLO);
    collector.event('watching');

    setVisibility('hidden');

    expect(beacons).toHaveLength(1);
    expect(beacons[0].url).toBe(DEFAULT_TELEMETRY_URL);
    // The beacon path must not use fetch — the whole point is surviving a
    // document that is going away.
    expect(fetches).toHaveLength(0);
  });

  it('does not flush on a visibility change back to visible', () => {
    let collector!: TelemetryCollector<{ fps: number }>;
    render(<Probe onReady={(c) => (collector = c)} />);
    collector.begin(HELLO);
    collector.event('watching');

    setVisibility('visible');
    expect(beacons).toHaveLength(0);
  });

  it('sends nothing on hidden when no hello ever arrived', () => {
    render(<Probe onReady={() => {}} />);
    setVisibility('hidden');
    expect(beacons).toHaveLength(0);
    expect(fetches).toHaveLength(0);
  });

  it('ends the session on unmount and stops listening', () => {
    let collector!: TelemetryCollector<{ fps: number }>;
    const view = render(<Probe onReady={(c) => (collector = c)} />);
    collector.begin(HELLO);
    collector.event('watching');

    view.unmount();
    // Unmount ends the session: a final batch, over fetch (the document is
    // still here; it is the screen that went away).
    expect(fetches).toHaveLength(1);
    expect(JSON.parse(fetches[0]).final).toBe(true);
    expect(collector.active).toBe(false);

    // And the listener is gone — a later hidden must not resurrect anything.
    setVisibility('hidden');
    expect(beacons).toHaveLength(0);
  });
});
