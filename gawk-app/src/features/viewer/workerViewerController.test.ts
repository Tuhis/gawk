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
  onerror: ((e: Event) => void) | null = null;
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

  // What a module worker whose script 404s or throws while evaluating does:
  // an error event on the Worker object, and never a 'boot'.
  fail(): void {
    this.onerror?.(new Event('error'));
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

// A worker that never boots must not strand the viewer on "Connecting…": the
// caller's only way off the worker path is onUnsupported, and the canvas is
// still untransferred (so usable by the main-thread pipeline) until a
// successful boot.
describe('WorkerViewerController boot failure', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  function setup() {
    const canvas = makeCanvas();
    const onUnsupported = vi.fn();
    const controller = new WorkerViewerController(canvas, { onEvent: () => {}, onUnsupported });
    const transfer = (canvas as unknown as { transferControlToOffscreen: ReturnType<typeof vi.fn> })
      .transferControlToOffscreen;
    return { controller, onUnsupported, transfer };
  }

  it('falls back when the worker script fails to load', () => {
    const { controller, onUnsupported, transfer } = setup();
    controller.start(START);
    workers[0].fail();

    expect(onUnsupported).toHaveBeenCalledTimes(1);
    expect(workers[0].terminated).toBe(true);
    expect(transfer).not.toHaveBeenCalled();
    expect(workers[0].posted).toEqual([]);
  });

  it('falls back when the worker never boots within the timeout', () => {
    const { controller, onUnsupported, transfer } = setup();
    controller.start(START);

    vi.advanceTimersByTime(1999);
    expect(onUnsupported).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(onUnsupported).toHaveBeenCalledTimes(1);
    expect(workers[0].terminated).toBe(true);
    expect(transfer).not.toHaveBeenCalled();
  });

  it('ignores a boot that arrives after the fallback, leaving the canvas untransferred', () => {
    const { controller, onUnsupported, transfer } = setup();
    controller.start(START);
    vi.advanceTimersByTime(2000);
    expect(onUnsupported).toHaveBeenCalledTimes(1);

    workers[0].boot(true);
    controller.start(START);

    expect(onUnsupported).toHaveBeenCalledTimes(1);
    expect(transfer).not.toHaveBeenCalled();
    expect(workers[0].posted).toEqual([]);
  });

  it('routes a throwing Worker constructor to the fallback instead of throwing', () => {
    vi.stubGlobal(
      'Worker',
      class {
        constructor() {
          throw new DOMException('Refused by worker-src', 'SecurityError');
        }
      },
    );
    const onUnsupported = vi.fn();
    let controller: WorkerViewerController | undefined;
    expect(() => {
      controller = new WorkerViewerController(makeCanvas(), { onEvent: () => {}, onUnsupported });
    }).not.toThrow();
    expect(onUnsupported).toHaveBeenCalledTimes(1);

    // The caller still drives it until its effect cleanup runs.
    expect(() => {
      controller!.setViewerDeliveryMode('live');
      controller!.start(START);
      controller!.setPlayoutMode('off');
      controller!.stop();
      controller!.dispose();
    }).not.toThrow();
    vi.advanceTimersByTime(2000);
    expect(onUnsupported).toHaveBeenCalledTimes(1);
  });

  it('dispose() cancels the boot timeout', () => {
    const { controller, onUnsupported } = setup();
    controller.dispose();
    vi.advanceTimersByTime(2000);
    expect(onUnsupported).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('a booted worker is never torn down by the timeout or a later worker error', () => {
    const { controller, onUnsupported, transfer } = setup();
    controller.start(START);
    workers[0].boot(true);
    expect(transfer).toHaveBeenCalledTimes(1);

    vi.advanceTimersByTime(2000);
    workers[0].fail();

    expect(onUnsupported).not.toHaveBeenCalled();
    expect(workers[0].terminated).toBe(false);
  });
});
