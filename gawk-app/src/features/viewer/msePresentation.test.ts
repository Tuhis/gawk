// @vitest-environment jsdom
//
// R22 MF2/MF3/MF4 (docs/27): the MSE capability probe and the main-thread
// presenter. jsdom has no MSE, so the MediaSource/SourceBuffer are structural
// fakes injected through the presenter's ctor parameter — which is exactly
// how iPhone-vs-desktop divergence (ManagedMediaSource streaming pacing vs
// classic MediaSource) is simulated.

import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Fmp4Track } from '../../transport/fmp4-muxer';
import {
  MAX_QUEUED_SEGMENTS,
  MsePresenter,
  PRUNE_KEEP_S,
  PRUNE_TRIGGER_S,
  getMediaSourceCtor,
  probeMseAudio,
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
  // Throw only from the Nth addSourceBuffer onwards — an MMS that takes the
  // video mime and refuses the audio one (the plausible iOS Opus-in-MP4 shape).
  addSourceBufferThrowsAfter?: number;
  sourceBufferChangeType?: boolean;
  durationThrows?: boolean;
}

function makeFakeMsCtor(opts: FakeMsOptions = {}) {
  const instances: FakeMediaSource[] = [];
  class FakeMediaSource {
    readyState = 'closed';
    streaming: boolean | undefined = opts.managed ? true : undefined;
    // Every SourceBuffer this source handed out, in creation order (video then
    // audio); `sb` stays the first for the single-track tests.
    buffers: FakeSourceBuffer[] = [];
    sb: FakeSourceBuffer | null = null;
    mimes: string[] = [];
    durationValue = NaN;
    durationWrites: number[] = [];
    private listeners = new Map<string, Set<() => void>>();

    constructor() {
      instances.push(this);
    }

    // Spec-shaped duration: NaN until set, and the setter is only legal on an
    // open source with nothing updating (MSE "duration change algorithm").
    get duration(): number {
      return this.durationValue;
    }

    set duration(value: number) {
      this.durationWrites.push(value);
      if (opts.durationThrows) throw new DOMException('nope', 'InvalidStateError');
      if (this.readyState !== 'open') throw new DOMException('not open', 'InvalidStateError');
      if (this.buffers.some((b) => b.updating)) {
        throw new DOMException('updating', 'InvalidStateError');
      }
      this.durationValue = value;
    }

    static isTypeSupported(): boolean {
      return true;
    }

    addSourceBuffer(mime: string): SourceBufferLike {
      if (opts.addSourceBufferThrows) throw new DOMException('nope', 'NotSupportedError');
      if (
        opts.addSourceBufferThrowsAfter != null &&
        this.buffers.length >= opts.addSourceBufferThrowsAfter
      ) {
        throw new DOMException('nope', 'NotSupportedError');
      }
      this.mimes.push(mime);
      const sb = new FakeSourceBuffer();
      if (opts.sourceBufferChangeType === false) {
        (sb as { changeType?: unknown }).changeType = undefined;
      }
      this.buffers.push(sb);
      this.sb ??= sb;
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

const init = (
  mime = 'video/mp4; codecs="avc1.42C01E"',
  track: Fmp4Track = 'video',
): PresenterSegment => ({
  kind: 'init',
  track,
  mime,
  data: new Uint8Array([1, 1, 1]),
});
const media = (keyframe: boolean, tag = 0, track: Fmp4Track = 'video'): PresenterSegment => ({
  kind: 'media',
  track,
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

  // R22 audio: the Opus-in-MP4 question is answered at runtime, per device —
  // WebKit 17 added Opus in MP4, but nothing promises it through
  // ManagedMediaSource, so the verdict is probed and never assumed.
  it('probes the audio lane independently, and refuses what MP4 Opus cannot carry', () => {
    vi.stubGlobal('MediaSource', makeFakeMsCtor().ctor);
    expect(probeMseAudio('opus', 2)).toMatchObject({
      supported: true,
      mime: 'audio/mp4; codecs="opus"',
    });
    // No audio in the broadcast at all.
    expect(probeMseAudio(null, null).supported).toBe(false);
    // A codec the muxer has no encapsulation for.
    expect(probeMseAudio('mp4a.40.2', 2).reason).toContain('not Opus');
    // RFC 7845 channel-mapping family 0 (and WebKit's support) is 1–2 channels.
    expect(probeMseAudio('opus', 6).reason).toContain('6 channels');
  });

  it('reports an audio refusal from isTypeSupported without touching the video verdict', () => {
    const { ctor } = makeFakeMsCtor();
    (ctor as { isTypeSupported: (m: string) => boolean }).isTypeSupported = (m) =>
      !m.startsWith('audio/');
    vi.stubGlobal('MediaSource', ctor);
    vi.stubGlobal('ManagedMediaSource', undefined);
    expect(probeMsePresentation('avc1.42E01F').supported).toBe(true);
    const r = probeMseAudio('opus', 2);
    expect(r.supported).toBe(false);
    expect(r.reason).toBe('unsupported: audio/mp4; codecs="opus"');
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

  // docs/27 finding 1: without an explicit infinite duration, MSE keeps raising
  // duration to the newest appended end timestamp — so the native player draws
  // a finite scrub bar instead of the LIVE badge, and "the playhead reached the
  // buffered end" becomes indistinguishable from "the media ended" (WebKit
  // pauses and fires `ended` there).
  it('declares an infinite duration as soon as the source opens', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();

    p.attach(video);
    expect(instances[0].duration).toBeNaN(); // not before sourceopen: the setter would throw
    instances[0].open();

    expect(instances[0].duration).toBe(Infinity);
    expect(instances[0].durationWrites).toEqual([Infinity]);
    expect(p.getStats().liveDuration).toBe(true);
  });

  it('does not re-write the duration once it is infinite', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();

    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    p.pushSegment(media(true));
    instances[0].sb!.finishUpdate();
    instances[0].sb!.finishUpdate();

    // One write, ever — re-asserting per append would throw mid-update.
    expect(instances[0].durationWrites).toEqual([Infinity]);
  });

  // A fresh MediaSource per attach: the new one needs its own declaration.
  it('re-declares the infinite duration on a re-attach', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const first = makeVideo();
    const second = makeVideo();

    p.attach(first.video);
    instances[0].open();
    p.attach(second.video);
    instances[1].open();

    expect(instances[1].duration).toBe(Infinity);
    expect(p.getStats().liveDuration).toBe(true);
  });

  it('survives a refusing duration setter — appends still flow, the gate reports it', () => {
    const { ctor, instances } = makeFakeMsCtor({ durationThrows: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();

    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    p.pushSegment(media(true));

    expect(instances[0].sb!.appended).toHaveLength(1);
    expect(p.getStats()).toMatchObject({ liveDuration: false, failed: false });
  });

  // R22 audio (docs/27 finding 2): video and audio are separate SourceBuffers.
  // The element's `buffered` is their INTERSECTION, which drives the two rules
  // under test: an audio SourceBuffer is never created before a sample can
  // follow its init (an empty audio track empties the intersection and stalls
  // video), and a refused audio track must leave video untouched.
  const AUDIO_MIME = 'audio/mp4; codecs="opus"';

  it('holds the audio init until a sample can follow it, then adds a second SourceBuffer', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();

    p.pushSegment(init());
    p.pushSegment(media(true));
    // Audio config arrives (1 Hz), but no packet has been muxed yet.
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    expect(instances[0].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
    expect(p.getStats().audioTrack).toBe(false);

    // The first audio sample primes the track: its init goes in first.
    p.pushSegment(media(true, 1, 'audio'));
    expect(instances[0].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"', AUDIO_MIME]);
    const audioSb = instances[0].buffers[1];
    expect(audioSb.appended).toHaveLength(1); // the held init
    audioSb.finishUpdate();
    expect(audioSb.appended).toHaveLength(2); // then the sample
    expect(p.getStats()).toMatchObject({ audioTrack: true, audioSegmentsAppended: 2 });
  });

  it('appends the two tracks independently — neither waits on the other', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 1, 'audio'));
    const [videoSb, audioSb] = instances[0].buffers;

    // Video has an append in flight and a queued segment behind it; audio must
    // still be able to make progress (its own SourceBuffer, its own in-flight).
    p.pushSegment(media(false, 2));
    expect(videoSb.appended).toHaveLength(1);
    audioSb.finishUpdate();
    p.pushSegment(media(true, 3, 'audio'));
    expect(audioSb.appended).toHaveLength(2);
    expect(videoSb.appended).toHaveLength(1); // untouched by audio's progress
  });

  it('keeps video presenting when the audio SourceBuffer is refused (the iOS Opus risk)', () => {
    // Second addSourceBuffer throws: exactly the shape of an MMS that takes
    // avc1 but not Opus in MP4.
    const { ctor, instances } = makeFakeMsCtor({ addSourceBufferThrowsAfter: 1 });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 1, 'audio'));

    // Video is unharmed and the gate stays up; audio is dropped for good.
    expect(p.getStats()).toMatchObject({ failed: false, audioTrack: false });
    const videoSb = instances[0].buffers[0];
    videoSb.finishUpdate();
    p.pushSegment(media(false, 2));
    expect(videoSb.appended).toHaveLength(2);
    // Further audio never queues (no cached init to prime from), so nothing
    // accumulates behind the dropped track.
    videoSb.finishUpdate();
    p.pushSegment(media(true, 3, 'audio'));
    p.pushSegment(media(false, 4, 'audio'));
    expect(instances[0].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
    expect(p.getStats()).toMatchObject({ queued: 0, audioSegmentsAppended: 0 });
  });

  it('prunes both tracks to the same window, so the intersection cannot shrink', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setBuffered(video, [[0, PRUNE_TRIGGER_S + 5]]);
    p.attach(video);
    instances[0].open();

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 1, 'audio'));
    const [videoSb, audioSb] = instances[0].buffers;
    videoSb.finishUpdate();
    audioSb.finishUpdate();

    const window: [number, number] = [0, PRUNE_TRIGGER_S + 5 - PRUNE_KEEP_S];
    expect(videoSb.removed).toContainEqual(window);
    expect(audioSb.removed).toContainEqual(window);
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
