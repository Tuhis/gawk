// @vitest-environment jsdom

// A page that goes away must close its WebTransport sessions itself. In
// production a reloaded room tab's old room and voice sessions stayed open
// until the tab closed: the relay kept the old participant, so its Mumble
// user stayed beside the reloaded one ("alice [web]" twice).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { dialWebTransport } from './connection';
import { pageExitHooks } from './page-exit';

class FakeWT {
  static instances: FakeWT[] = [];
  ready = Promise.resolve();
  closeCalls = 0;
  private resolveClosed!: () => void;
  closed = new Promise<void>((r) => (this.resolveClosed = r));
  constructor() {
    FakeWT.instances.push(this);
  }
  close(): void {
    this.closeCalls++;
    this.resolveClosed();
  }
  // The peer ends it: no local close() is called.
  endRemotely(): void {
    this.resolveClosed();
  }
}

function pageEvent(type: 'pagehide' | 'pageshow', persisted: boolean): Event {
  const e = new Event(type);
  Object.defineProperty(e, 'persisted', { value: persisted });
  return e;
}

const flush = () => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  FakeWT.instances = [];
  vi.stubGlobal('WebTransport', FakeWT);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('page exit', () => {
  it('closes every open session on pagehide', async () => {
    const a = (await dialWebTransport('https://relay.test/room/x', {})) as unknown as FakeWT;
    const b = (await dialWebTransport('https://bridge.test/voice', {})) as unknown as FakeWT;
    window.dispatchEvent(pageEvent('pagehide', false));
    expect(a.closeCalls).toBe(1);
    expect(b.closeCalls).toBe(1);
  });

  it('does not touch a session that already ended', async () => {
    const a = (await dialWebTransport('https://relay.test/room/x', {})) as unknown as FakeWT;
    a.endRemotely();
    await flush();
    window.dispatchEvent(pageEvent('pagehide', false));
    expect(a.closeCalls).toBe(0);
  });

  it('a page restored from the back/forward cache reloads, its sessions being closed', async () => {
    const reload = vi.spyOn(pageExitHooks, 'reload').mockImplementation(() => {});
    await dialWebTransport('https://relay.test/room/x', {});
    window.dispatchEvent(pageEvent('pagehide', true));
    window.dispatchEvent(pageEvent('pageshow', false));
    expect(reload).not.toHaveBeenCalled();
    window.dispatchEvent(pageEvent('pageshow', true));
    expect(reload).toHaveBeenCalledTimes(1);
  });
});
