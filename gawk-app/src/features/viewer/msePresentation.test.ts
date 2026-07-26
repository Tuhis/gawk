// @vitest-environment jsdom
//
// R22 MF2/MF3/MF4 (docs/27): the MSE capability probe and the main-thread
// presenter. jsdom has no MSE, so the MediaSource/SourceBuffer are structural
// fakes injected through the presenter's ctor parameter — which is exactly
// how iPhone-vs-desktop divergence (ManagedMediaSource streaming pacing vs
// classic MediaSource) is simulated.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  MAX_QUEUED_SEGMENTS,
  MsePresenter,
  PRUNE_KEEP_S,
  PRUNE_TRIGGER_S,
  getMediaSourceCtor,
  probeMsePresentation,
  type MediaSourceCtor,
  type PresenterSegment,
  type SourceBufferLike,
} from './msePresentation';

// ---------------------------------------------------------------------------
// Fakes

class FakeSourceBuffer implements SourceBufferLike {
  updating = false;
  appended: Uint8Array[] = [];
  removed: Array<[number, number]> = [];
  changeTypes: string[] = [];
  appendThrows = false;
  private listeners = new Map<string, Set<() => void>>();

  appendBuffer(data: BufferSource): void {
    if (this.appendThrows) throw new DOMException('quota', 'QuotaExceededError');
    if (this.updating) throw new Error('appendBuffer while updating');
    this.updating = true;
    this.appended.push(new Uint8Array(data as Uint8Array));
  }

  remove(start: number, end: number): void {
    if (this.updating) throw new Error('remove while updating');
    this.updating = true;
    this.removed.push([start, end]);
  }

  changeType(mime: string): void {
    this.changeTypes.push(mime);
  }

  addEventListener(type: string, cb: () => void): void {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)!.add(cb);
  }

  removeEventListener(type: string, cb: () => void): void {
    this.listeners.get(type)?.delete(cb);
  }

  finishUpdate(): void {
    this.updating = false;
    for (const cb of this.listeners.get('updateend') ?? []) cb();
  }

  fireError(): void {
    for (const cb of this.listeners.get('error') ?? []) cb();
  }
}

interface FakeMsOptions {
  managed?: boolean; // expose `streaming`
  addSourceBufferThrows?: boolean;
  sourceBufferChangeType?: boolean;
}

function makeFakeMsCtor(opts: FakeMsOptions = {}) {
  const instances: FakeMediaSource[] = [];
  class FakeMediaSource {
    readyState = 'closed';
    streaming: boolean | undefined = opts.managed ? true : undefined;
    sb: FakeSourceBuffer | null = null;
    mimes: string[] = [];
    private listeners = new Map<string, Set<() => void>>();

    constructor() {
      instances.push(this);
    }

    static isTypeSupported(): boolean {
      return true;
    }

    addSourceBuffer(mime: string): SourceBufferLike {
      if (opts.addSourceBufferThrows) throw new DOMException('nope', 'NotSupportedError');
      this.mimes.push(mime);
      const sb = new FakeSourceBuffer();
      if (opts.sourceBufferChangeType === false) {
        (sb as { changeType?: unknown }).changeType = undefined;
      }
      this.sb = sb;
      return sb;
    }

    addEventListener(type: string, cb: () => void): void {
      if (!this.listeners.has(type)) this.listeners.set(type, new Set());
      this.listeners.get(type)!.add(cb);
    }

    removeEventListener(type: string, cb: () => void): void {
      this.listeners.get(type)?.delete(cb);
    }

    open(): void {
      this.readyState = 'open';
      for (const cb of this.listeners.get('sourceopen') ?? []) cb();
    }

    setStreaming(on: boolean): void {
      this.streaming = on;
      if (on) for (const cb of this.listeners.get('startstreaming') ?? []) cb();
    }
  }
  return { ctor: FakeMediaSource as unknown as MediaSourceCtor, instances };
}

// A <video> whose srcObject assignment is captured (WebKit accepts a
// MediaSource there; jsdom would otherwise reject the fake).
function makeVideo({ srcObjectThrows = false } = {}) {
  const video = document.createElement('video');
  const captured: unknown[] = [];
  Object.defineProperty(video, 'srcObject', {
    configurable: true,
    get: () => captured[captured.length - 1] ?? null,
    set: (v: unknown) => {
      if (srcObjectThrows && v !== null) throw new TypeError('srcObject rejects MediaSource');
      captured.push(v);
    },
  });
  return { video, captured };
}

function setBuffered(video: HTMLVideoElement, ranges: Array<[number, number]>): void {
  Object.defineProperty(video, 'buffered', {
    configurable: true,
    get: () => ({
      length: ranges.length,
      start: (i: number) => ranges[i][0],
      end: (i: number) => ranges[i][1],
    }),
  });
}

const init = (mime = 'video/mp4; codecs="avc1.42C01E"'): PresenterSegment => ({
  kind: 'init',
  mime,
  data: new Uint8Array([1, 1, 1]),
});
const media = (keyframe: boolean, tag = 0): PresenterSegment => ({
  kind: 'media',
  keyframe,
  data: new Uint8Array([2, keyframe ? 1 : 0, tag & 0xff, (tag >> 8) & 0xff]),
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Probe

describe('probeMsePresentation', () => {
  it('prefers ManagedMediaSource over MediaSource (the iPhone shape)', () => {
    const mms = makeFakeMsCtor({ managed: true }).ctor;
    const ms = makeFakeMsCtor().ctor;
    vi.stubGlobal('ManagedMediaSource', mms);
    vi.stubGlobal('MediaSource', ms);
    expect(getMediaSourceCtor()).toBe(mms);
  });

  it('probes true for H.264 where MSE exists', () => {
    vi.stubGlobal('MediaSource', makeFakeMsCtor().ctor);
    const r = probeMsePresentation('avc1.42E01F');
    expect(r.supported).toBe(true);
    expect(r.mime).toBe('video/mp4; codecs="avc1.42E01F"');
  });

  it('rejects VP codecs (H.264-only v1 — docs/27 Decision 11)', () => {
    vi.stubGlobal('MediaSource', makeFakeMsCtor().ctor);
    const r = probeMsePresentation('vp09.00.40.08');
    expect(r.supported).toBe(false);
    expect(r.reason).toContain('not H.264');
  });

  it('rejects when no MediaSource flavor exists', () => {
    vi.stubGlobal('ManagedMediaSource', undefined);
    vi.stubGlobal('MediaSource', undefined);
    const r = probeMsePresentation('avc1.42E01F');
    expect(r.supported).toBe(false);
    expect(r.reason).toContain('no MediaSource');
  });

  it('rejects when isTypeSupported says no', () => {
    const { ctor } = makeFakeMsCtor();
    (ctor as { isTypeSupported: (m: string) => boolean }).isTypeSupported = () => false;
    vi.stubGlobal('MediaSource', ctor);
    vi.stubGlobal('ManagedMediaSource', undefined);
    const r = probeMsePresentation('avc1.42E01F');
    expect(r.supported).toBe(false);
    expect(r.reason).toContain('unsupported');
  });
});

// ---------------------------------------------------------------------------
// Presenter

describe('MsePresenter', () => {
  it('attaches via srcObject, opens, appends init then media serially', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video, captured } = makeVideo();

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(media(false));
    p.attach(video);
    expect(captured[0]).toBe(instances[0]);

    // Nothing appends before sourceopen.
    expect(instances[0].sb).toBeNull();
    instances[0].open();

    const sb = instances[0].sb!;
    expect(instances[0].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
    expect(sb.appended).toHaveLength(1); // init in flight
    sb.finishUpdate();
    expect(sb.appended).toHaveLength(2); // keyframe
    sb.finishUpdate();
    expect(sb.appended).toHaveLength(3); // delta
    expect(p.getStats()).toMatchObject({ segmentsAppended: 3, appendErrors: 0, failed: false });
  });

  it('falls back to an object URL when srcObject rejects the MediaSource (Chromium)', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo({ srcObjectThrows: true });
    const createUrl = vi.fn(() => 'blob:fake');
    vi.stubGlobal('URL', { ...URL, createObjectURL: createUrl, revokeObjectURL: vi.fn() });

    p.attach(video);
    expect(createUrl).toHaveBeenCalledWith(instances[0]);
    expect(video.getAttribute('src')).toBe('blob:fake');
  });

  it('drops media arriving before any init segment', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();

    p.pushSegment(media(true));
    expect(p.getStats().queued).toBe(0);
    p.pushSegment(init());
    p.pushSegment(media(true));
    instances[0].sb!.finishUpdate();
    expect(instances[0].sb!.appended).toHaveLength(2);
  });

  it('parks the queue while ManagedMediaSource streaming is off, resumes on startstreaming', () => {
    const { ctor, instances } = makeFakeMsCtor({ managed: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();

    instances[0].streaming = false;
    p.pushSegment(init());
    p.pushSegment(media(true));
    expect(instances[0].sb).toBeNull(); // even the SourceBuffer waits

    instances[0].setStreaming(true);
    expect(instances[0].sb!.appended).toHaveLength(1);
    instances[0].sb!.finishUpdate();
    expect(instances[0].sb!.appended).toHaveLength(2);
  });

  it('bounds the queue and resumes only at a keyframe after dropping', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    // Note: NOT opened — everything queues.
    p.pushSegment(init());
    for (let i = 0; i < MAX_QUEUED_SEGMENTS + 20; i++) {
      p.pushSegment(media(i === 60, i)); // one keyframe in the middle
    }
    expect(p.getStats().queued).toBe(MAX_QUEUED_SEGMENTS);

    instances[0].open();
    const sb = instances[0].sb;
    // The init was dropped from the front… but it is cached, so attach()'s
    // re-prime is what a real overflow relies on; here the queue was dropped
    // below the keyframe, so all pre-keyframe deltas are discarded.
    // Drain everything.
    let guard = 0;
    while (sb && sb.updating && guard++ < 500) sb.finishUpdate();
    const appended = instances[0].sb?.appended ?? [];
    // Every appended media segment after a drop must start at the keyframe:
    // the first media segment appended is the keyframe-tagged one.
    const firstMedia = appended.find((d) => d[0] === 2);
    expect(firstMedia?.[1]).toBe(1);
  });

  it('re-primes a fresh MediaSource from the cached init on re-attach and waits for a keyframe', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const a = makeVideo();
    p.attach(a.video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.finishUpdate();
    p.pushSegment(media(true));
    instances[0].sb!.finishUpdate();

    // Remount: new video element, new MediaSource.
    const b = makeVideo();
    p.attach(b.video);
    expect(instances).toHaveLength(2);
    instances[1].open();
    p.pushSegment(media(false, 7)); // old-timeline delta — must NOT append
    p.pushSegment(media(true, 8));
    const sb2 = instances[1].sb!;
    // First append is the cached init.
    expect(sb2.appended[0]).toEqual(new Uint8Array([1, 1, 1]));
    sb2.finishUpdate();
    let guard = 0;
    while (sb2.updating && guard++ < 10) sb2.finishUpdate();
    const medias = sb2.appended.filter((d) => d[0] === 2);
    expect(medias).toHaveLength(1);
    expect(medias[0][1]).toBe(1); // the keyframe, not the stale delta
  });

  it('marks the surface failed when addSourceBuffer throws (trial-append semantics)', () => {
    const { ctor, instances } = makeFakeMsCtor({ addSourceBufferThrows: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    expect(p.getStats().failed).toBe(true);
    expect(p.getStats().appendErrors).toBe(1);
  });

  it('routes a codec change through changeType', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.pushSegment(init('video/mp4; codecs="avc1.42C01E"'));
    instances[0].sb!.finishUpdate();
    p.pushSegment(init('video/mp4; codecs="avc1.640028"'));
    instances[0].sb!.finishUpdate();
    expect(instances[0].sb!.changeTypes).toEqual(['video/mp4; codecs="avc1.640028"']);
    expect(p.getStats().failed).toBe(false);
  });

  it('degrades to failed when changeType is unavailable and the codec changes', () => {
    const { ctor, instances } = makeFakeMsCtor({ sourceBufferChangeType: false });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.pushSegment(init('video/mp4; codecs="avc1.42C01E"'));
    instances[0].sb!.finishUpdate();
    p.pushSegment(init('video/mp4; codecs="avc1.640028"'));
    expect(p.getStats().failed).toBe(true);
  });

  it('append failure drops the segment and resyncs at the next keyframe', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.finishUpdate();

    instances[0].sb!.appendThrows = true;
    p.pushSegment(media(true, 1));
    expect(p.getStats().appendErrors).toBe(1);
    instances[0].sb!.appendThrows = false;
    p.pushSegment(media(false, 2)); // depends on the dropped keyframe — skipped
    p.pushSegment(media(true, 3));
    let guard = 0;
    while (instances[0].sb!.updating && guard++ < 10) instances[0].sb!.finishUpdate();
    const medias = instances[0].sb!.appended.filter((d) => d[0] === 2);
    expect(medias.map((d) => d[2])).toEqual([3]);
    expect(p.getStats().failed).toBe(false);
  });

  it('catches the playhead up to the live edge only while playing', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setBuffered(video, [[0, 10]]);
    let currentTime = 1;
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => currentTime,
      set: (v: number) => {
        currentTime = v;
      },
    });
    let paused = true;
    Object.defineProperty(video, 'paused', { configurable: true, get: () => paused });

    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.finishUpdate(); // paused: no seek
    expect(currentTime).toBe(1);

    paused = false;
    p.pushSegment(media(true));
    instances[0].sb!.finishUpdate(); // playing, 9 s behind: catch up
    expect(currentTime).toBeCloseTo(9.9, 5);
  });

  it('prunes old buffered ranges past the trigger', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setBuffered(video, [[0, PRUNE_TRIGGER_S + 5]]);
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.finishUpdate();
    expect(instances[0].sb!.removed).toEqual([[0, PRUNE_TRIGGER_S + 5 - PRUNE_KEEP_S]]);
  });

  it('a SourceBuffer error event marks the surface failed', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.fireError();
    expect(p.getStats().failed).toBe(true);
  });

  it('dispose detaches, revokes the object URL and clears state', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo({ srcObjectThrows: true });
    const revoke = vi.fn();
    vi.stubGlobal('URL', { ...URL, createObjectURL: () => 'blob:fake', revokeObjectURL: revoke });
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    p.dispose();
    expect(revoke).toHaveBeenCalledWith('blob:fake');
    expect(p.getStats()).toMatchObject({ attached: false, queued: 0 });
  });

  it('does nothing without a MediaSource ctor (probe-fail belt-and-braces)', () => {
    const p = new MsePresenter(null);
    const { video } = makeVideo();
    p.attach(video);
    p.pushSegment(init());
    p.pushSegment(media(true));
    expect(p.getStats()).toMatchObject({ attached: false, segmentsAppended: 0 });
  });
});
