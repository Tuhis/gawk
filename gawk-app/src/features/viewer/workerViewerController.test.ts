// @vitest-environment jsdom
//
// R22 MF2 acceptance (docs/27, carrying R16 Decision 1 forward): non-gated
// devices' worker messages are BYTE-IDENTICAL — the init message carries no
// presentationMux key at all, and 'arm' is never sent unless requested. The
// controller is the one place every worker-bound message passes through, so
// this is the seam that proves it.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkerViewerController } from './workerViewerController';

interface Posted {
  msg: unknown;
  transfer: Transferable[];
}

const workers: FakeWorker[] = [];
class FakeWorker {
  posted: Posted[] = [];
  onmessage: ((e: { data: unknown }) => void) | null = null;
  terminated = false;

  constructor() {
    workers.push(this);
  }

  postMessage(msg: unknown, transfer: Transferable[] = []): void {
    this.posted.push({ msg, transfer });
  }

  terminate(): void {
    this.terminated = true;
  }

  boot(supported = true): void {
    this.onmessage?.({ data: { type: 'boot', supported } });
  }
}

function makeCanvas(): HTMLCanvasElement {
  const canvas = document.createElement('canvas');
  (canvas as unknown as { transferControlToOffscreen: () => unknown }).transferControlToOffscreen =
    vi.fn(() => ({ fake: 'offscreen' }));
  return canvas;
}

const START = {
  serverUrl: 'https://relay.test:4433',
  broadcastId: 'AB2CD3',
  connectOpts: {},
};

beforeEach(() => {
  workers.length = 0;
  vi.stubGlobal('Worker', FakeWorker);
});
afterEach(() => vi.unstubAllGlobals());

describe('WorkerViewerController message shapes (R22 MF2)', () => {
  it('non-gated: the init message carries NO presentationMux key (byte-identical)', () => {
    const controller = new WorkerViewerController(makeCanvas(), {
      onEvent: () => {},
      onUnsupported: () => {},
    });
    controller.start(START);
    workers[0].boot();

    const init = workers[0].posted.find(
      (p) => (p.msg as { type?: string }).type === 'init',
    )!;
    expect(init).toBeDefined();
    // Key-for-key: the exact pre-R22 shape, not merely a falsy flag.
    expect(Object.keys(init.msg as object).sort()).toEqual(['canvas', 'type']);
  });

  it('non-gated: armPresentation() never reaches the worker', () => {
    const controller = new WorkerViewerController(makeCanvas(), {
      onEvent: () => {},
      onUnsupported: () => {},
    });
    controller.start(START);
    workers[0].boot();
    controller.armPresentation();
    const types = workers[0].posted.map((p) => (p.msg as { type: string }).type);
    expect(types).toEqual(['init', 'start']);
  });

  it('gated: init carries presentationMux: true and arm follows once', () => {
    const controller = new WorkerViewerController(
      makeCanvas(),
      { onEvent: () => {}, onUnsupported: () => {} },
      { presentationMux: true },
    );
    controller.start(START);
    workers[0].boot();
    controller.armPresentation();
    controller.armPresentation(); // idempotent — one arm, ever

    const types = workers[0].posted.map((p) => (p.msg as { type: string }).type);
    expect(types).toEqual(['init', 'start', 'arm']);
    const init = workers[0].posted[0].msg as { presentationMux?: boolean };
    expect(init.presentationMux).toBe(true);
  });
});
