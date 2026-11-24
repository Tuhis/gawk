// The viewer worker shell's init handling. A worker has no `window`, so
// config.js's window.__GAWK_CONFIG__ never reaches it; the main thread sends
// the runtime config in 'init' and the shell must install it before the core
// exists, so the first start already sees the deployment's values. Runs in
// vitest's node environment: no `window`, like a real worker.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { getDvrBufferMs, getMaxDecoderQueueSize } from '../config';

const seenAtConstruction: { dvrBufferMs: number; maxDecoderQueueSize: number }[] = [];

vi.mock('./render-sink', () => ({ createRenderSink: () => ({}) }));
vi.mock('./viewer-worker-core', () => ({
  ViewerWorkerCore: class {
    constructor() {
      seenAtConstruction.push({
        dvrBufferMs: getDvrBufferMs(),
        maxDecoderQueueSize: getMaxDecoderQueueSize(),
      });
    }
    start(): void {}
    stop(): Promise<void> {
      return Promise.resolve();
    }
  },
}));

interface FakeScope {
  posted: unknown[];
  postMessage(msg: unknown): void;
  onmessage: ((e: { data: unknown }) => void) | null;
}

async function bootShell(): Promise<FakeScope> {
  const scope: FakeScope = {
    posted: [],
    postMessage(msg) {
      this.posted.push(msg);
    },
    onmessage: null,
  };
  vi.stubGlobal('self', scope);
  vi.resetModules();
  await import('./viewer.worker');
  return scope;
}

afterEach(async () => {
  vi.unstubAllGlobals();
  seenAtConstruction.length = 0;
  delete (globalThis as { __GAWK_CONFIG__?: unknown }).__GAWK_CONFIG__;
});

describe('viewer.worker init', () => {
  it('installs the runtime config from init before building the core', async () => {
    expect(typeof window).toBe('undefined');
    const scope = await bootShell();
    const config = await import('../config');
    // Before init the worker knows nothing but the defaults.
    expect(config.getDvrBufferMs()).toBe(config.DEFAULT_DVR_BUFFER_MS);

    scope.onmessage?.({
      data: { type: 'init', canvas: {}, config: { dvrBufferMs: 4000, maxDecoderQueueSize: 4 } },
    });

    expect(seenAtConstruction).toEqual([{ dvrBufferMs: 4000, maxDecoderQueueSize: 4 }]);
    expect(config.getDvrBufferMs()).toBe(4000);
  });

  it('keeps the defaults when init carries no config', async () => {
    const scope = await bootShell();
    const config = await import('../config');
    scope.onmessage?.({ data: { type: 'init', canvas: {} } });
    expect(config.getDvrBufferMs()).toBe(config.DEFAULT_DVR_BUFFER_MS);
    expect(config.getMaxDecoderQueueSize()).toBe(10);
  });
});
