// @vitest-environment jsdom
//
// The MSE capability probe and the main-thread presenter. jsdom has no MSE, so the MediaSource/SourceBuffer are structural
// fakes injected through the presenter's ctor parameter — which is exactly
// how iPhone-vs-desktop divergence (ManagedMediaSource streaming pacing vs
// classic MediaSource) is simulated.

import { liveEdgePolicy } from '../../lib/liveEdge';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Fmp4Track } from '../../transport/fmp4-muxer';
import type { ViewerDeliveryMode } from '../../transport/resilient';
import {
  AUDIO_FIRST_SAMPLE_TIMEOUT_MS,
  MAX_QUEUED_SEGMENTS,
  MsePresenter,
  PRUNE_BEHIND_S,
  PRUNE_BUDGET_BYTES,
  PRUNE_CEILING_S,
  getMediaSourceCtor,
  probeMseAudio,
  probeMsePresentation,
  type MediaSourceCtor,
  type PresenterSegment,
  type SourceBufferLike,
} from './msePresentation';

// ---------------------------------------------------------------------------
// Fakes

// One segment per frame at 60 fps, the cadence MAX_QUEUED_SEGMENTS is sized for.
const FRAME_S = 1 / 60;

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

  // A timeline model, so a test can SEE a hole: media segment `tag` covers
  // [tag·FRAME_S, (tag+1)·FRAME_S) (see media()); remove() clears what starts in
  // its window. Init segments carry no time.
  get buffered(): TimeRanges {
    const spans = this.appended
      .filter((d) => d[0] === 2)
      .map((d) => (d[2] | (d[3] << 8)) * FRAME_S)
      .filter((t) => !this.removed.some(([a, b]) => t >= a && t < b))
      .sort((a, b) => a - b);
    const ranges: Array<[number, number]> = [];
    for (const t of spans) {
      const last = ranges[ranges.length - 1];
      if (last && Math.abs(last[1] - t) < 1e-9) last[1] = t + FRAME_S;
      else if (!last || t >= last[1]) ranges.push([t, t + FRAME_S]);
    }
    return {
      length: ranges.length,
      start: (i: number) => ranges[i][0],
      end: (i: number) => ranges[i][1],
    } as TimeRanges;
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

// jsdom's <video> reports readyState 0 and throws on `buffered`, so "does the
// element hold media" — which decides whether MMS parking is legal — has to
// be stated explicitly by any test that cares.
function setElementMedia(
  video: HTMLVideoElement,
  readyState: number,
  ranges: Array<[number, number]>,
): void {
  Object.defineProperty(video, 'readyState', { configurable: true, get: () => readyState });
  setBuffered(video, ranges);
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
const media = (
  keyframe: boolean,
  tag = 0,
  track: Fmp4Track = 'video',
  timeS = tag / 60,
): PresenterSegment => ({
  kind: 'media',
  track,
  keyframe,
  timeS,
  data: new Uint8Array([2, keyframe ? 1 : 0, tag & 0xff, (tag >> 8) & 0xff]),
});

// A playable element whose playhead the test drives: `buffered` is the given
// ranges array (mutated in place by the caller) and currentTime/paused are
// plain settable values.
function makePlayingVideo(ranges: Array<[number, number]>, playing = true) {
  const { video } = makeVideo();
  setElementMedia(video, 4, ranges);
  const el = { currentTime: 0, paused: !playing };
  Object.defineProperty(video, 'currentTime', {
    configurable: true,
    get: () => el.currentTime,
    set: (v: number) => {
      el.currentTime = v;
    },
  });
  Object.defineProperty(video, 'paused', { configurable: true, get: () => el.paused });
  return { video, el };
}

// Settle every in-flight append/remove on a source, as the browser would
// between frames.
function settle(ms: { buffers: FakeSourceBuffer[] }): void {
  for (let guard = 0; guard < 16; guard++) {
    const busy = ms.buffers.filter((b) => b.updating);
    if (busy.length === 0) return;
    for (const b of busy) b.finishUpdate();
  }
}

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

  // The Opus-in-MP4 question is answered at runtime, per device —
  // WebKit 17 added Opus in MP4, but nothing promises it through
  // ManagedMediaSource, so the verdict is probed and never assumed.
  it('probes the audio lane independently, and refuses what MP4 Opus cannot carry', () => {
    vi.stubGlobal('MediaSource', makeFakeMsCtor().ctor);
    expect(probeMseAudio('opus', 2)).toMatchObject({
      supported: true,
      codec: 'opus',
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
    expect(r.codec).toBeNull();
    // Both tiers named, so the row says what was actually tried.
    expect(r.reason).toContain('audio/mp4; codecs="opus"');
    expect(r.reason).toContain('mp4a.40.2');
  });

  // iOS 18.7's MMS refuses Opus in MP4 but takes AAC — the codec Apple's own
  // HLS mandates. That is the tier the iPhone lands
  // on, and it is the one that costs a transcode.
  it('falls back to the AAC tier when Opus in MP4 is refused', () => {
    const { ctor } = makeFakeMsCtor();
    (ctor as { isTypeSupported: (m: string) => boolean }).isTypeSupported = (m) =>
      !m.includes('opus');
    vi.stubGlobal('MediaSource', ctor);
    vi.stubGlobal('ManagedMediaSource', undefined);
    const r = probeMseAudio('opus', 2);
    expect(r).toMatchObject({ supported: true, codec: 'aac', mime: 'audio/mp4; codecs="mp4a.40.2"' });
    expect(r.reason).toContain('refused');
  });

  it('prefers Opus where it is supported — no transcode for nothing', () => {
    vi.stubGlobal('MediaSource', makeFakeMsCtor().ctor);
    expect(probeMseAudio('opus', 2)).toMatchObject({ codec: 'opus' });
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

  // Without an explicit infinite duration, MSE keeps raising
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

  // Video and audio are separate SourceBuffers.
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

    // The audio mime is declared up front, so BOTH buffers are
    // created with the first video init — an MSE implementation may refuse a
    // second buffer once the first init segment has been parsed.
    p.setExpectedAudioMime(AUDIO_MIME);
    p.pushSegment(init());
    p.pushSegment(media(true));
    // Audio config arrives (1 Hz), but no packet has been muxed yet.
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    expect(instances[0].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"', AUDIO_MIME]);
    // The buffer exists, but nothing has been appended to it yet: an audio track
    // holding an init and no samples still empties the buffered intersection.
    expect(p.getStats().audioSegmentsAppended).toBe(0);

    // The first audio sample primes the track: its init goes in first.
    p.pushSegment(media(true, 1, 'audio'));
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

  // The media error is the one fact that localizes a rejected init segment,
  // and it is only readable before the rebuild.
  it('records the element media error and MediaSource state at the failure', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    Object.defineProperty(video, 'error', {
      configurable: true,
      get: () => ({ code: 4, message: 'Media resource could not be decoded' }),
    });
    p.attach(video);
    instances[0].open();
    p.setExpectedAudioMime(AUDIO_MIME);
    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 1, 'audio'));

    expect(p.getStats().lastError).toBeNull();
    instances[0].buffers[1].fireError();

    const { lastError } = p.getStats();
    expect(lastError).toContain('audio');
    expect(lastError).toContain('code 4');
    expect(lastError).toContain('Media resource could not be decoded');
  });

  it('records a refused addSourceBuffer as the last error', () => {
    // The video buffer is created first; the audio one is the second, so it is
    // the one refused — the plausible shape on a runtime that accepts the mime
    // in isTypeSupported and still refuses a second buffer.
    const { ctor, instances } = makeFakeMsCtor({ addSourceBufferThrowsAfter: 1 });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.setExpectedAudioMime(AUDIO_MIME);
    p.pushSegment(init());
    p.pushSegment(media(true));

    expect(p.getStats().audioTrack).toBe(false);
    expect(p.getStats().lastError).toContain('addSourceBuffer');
  });

  // The element's buffered range is the INTERSECTION of the tracks, so a dead
  // audio buffer would freeze video. Audio is additive: it must be able to fail
  // without taking the presentation down.
  it('rebuilds video-only when the audio SourceBuffer errors', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    p.attach(video);
    instances[0].open();
    p.setExpectedAudioMime(AUDIO_MIME);

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 1, 'audio'));
    const audioSb = instances[0].buffers[1];
    expect(audioSb).toBeDefined();

    // The audio buffer rejects its content (a malformed transcode, say).
    audioSb.fireError();
    expect(p.getStats()).toMatchObject({ failed: false, audioTrack: false });
    // The WHY has to be captured here, at the error, or it is gone —
    // dropAudioTrack rebuilds the MediaSource and the element's error clears
    // with it.
    expect(p.getStats().lastError).toContain('audio');

    // A fresh MediaSource carrying video alone, re-primed from the cached init.
    expect(instances).toHaveLength(2);
    instances[1].open();
    p.pushSegment(media(true, 2));
    expect(instances[1].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
    expect(instances[1].buffers[0].appended.length).toBeGreaterThan(0);
    // Further audio is inert (the counter is cumulative, so it must not grow).
    const appendedBefore = p.getStats().audioSegmentsAppended;
    p.pushSegment(media(true, 3, 'audio'));
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    p.pushSegment(media(true, 4, 'audio'));
    expect(p.getStats().audioSegmentsAppended).toBe(appendedBefore);
    expect(instances[1].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
  });

  // A runtime with no AAC encoder (a Linux CI runner, say): the audio buffer
  // exists, nothing ever appends to it, and because `buffered` is the tracks'
  // INTERSECTION the video can be blocked too — worse than video-only.
  it('leaves a working presentation alone when audio produces nothing', () => {
    // The audio buffer never received even an init segment, so it contributes no
    // active track and this implementation plays video regardless. Rebuilding
    // would destroy a working presentation for nothing.
    vi.useFakeTimers();
    try {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const { video } = makeVideo();
      setBuffered(video, [[0, 5]]);
      Object.defineProperty(video, 'readyState', { configurable: true, get: () => 2 });
      p.attach(video);
      instances[0].open();
      p.setExpectedAudioMime(AUDIO_MIME);
      p.pushSegment(init());
      p.pushSegment(media(true));
      vi.advanceTimersByTime(AUDIO_FIRST_SAMPLE_TIMEOUT_MS + 10);
      expect(instances).toHaveLength(1); // untouched
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops audio when no sample ever arrives AND video is blocked on it', () => {
    vi.useFakeTimers();
    try {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const { video } = makeVideo();
      // Chromium's shape while it waits for every added buffer's init segment:
      // video appended, but no metadata and no buffered range.
      setBuffered(video, []);
      Object.defineProperty(video, 'readyState', { configurable: true, get: () => 0 });
      p.attach(video);
      instances[0].open();
      p.setExpectedAudioMime(AUDIO_MIME);

      p.pushSegment(init());
      p.pushSegment(media(true));
      expect(instances[0].buffers).toHaveLength(2); // the pair, created together
      const videoSb = instances[0].buffers[0];

      // Audio produces nothing (no encoder on this runtime). The watchdog must
      // fire on its own timer — the case it exists for is one where nothing is
      // appending, so an append-driven tick would never come.
      videoSb.finishUpdate();
      vi.advanceTimersByTime(AUDIO_FIRST_SAMPLE_TIMEOUT_MS - 1);
      expect(instances).toHaveLength(1);

      vi.advanceTimersByTime(2);
      // Rebuilt video-only, and audio is refused for good.
      expect(instances).toHaveLength(2);
      instances[1].open();
      p.pushSegment(media(true, 1));
      expect(instances[1].mimes).toEqual(['video/mp4; codecs="avc1.42C01E"']);
      p.pushSegment(init(AUDIO_MIME, 'audio'));
      p.pushSegment(media(true, 2, 'audio'));
      expect(p.getStats()).toMatchObject({ audioTrack: false, failed: false });
    } finally {
      vi.useRealTimers();
    }
  });

  it('prunes both tracks to the same window, so the intersection cannot shrink', () => {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    const ranges: Array<[number, number]> = [[0, 0]];
    const { video, el } = makePlayingVideo(ranges);
    p.attach(video);
    instances[0].open();

    p.pushSegment(init());
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    // Keyframes every second; the span alone crosses the ceiling.
    const fps = 10;
    for (let i = 0; i <= (PRUNE_CEILING_S + 5) * fps; i++) {
      const t = i / fps;
      p.pushSegment(media(i % fps === 0, i, 'video', t));
      p.pushSegment(media(true, i, 'audio', t));
      ranges[0] = [0, t + 1 / fps];
      el.currentTime = t;
      settle(instances[0]);
    }
    const [videoSb, audioSb] = instances[0].buffers;
    expect(videoSb.removed.length).toBeGreaterThan(0);
    expect(audioSb.removed).toEqual(videoSb.removed);
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
    // A fed element: it holds media, so the system's "stop sending" is about
    // pacing and the presenter must honor it.
    setElementMedia(video, 1, [[0, 5]]);
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

  // An MMS that has never been asked for anything opens with `streaming`
  // false and stays there: the system asks when the element needs more data,
  // and an element with no init segment needs nothing. Parking there is a deadlock, and its signature is a capture
  // with segmentsAppended 0 / appendErrors 0 / elementReadyState 0 and a
  // healthy open source — i.e. no visible failure anywhere.
  it('primes an element with no media even while MMS never asks for data', () => {
    const { ctor, instances } = makeFakeMsCtor({ managed: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setElementMedia(video, 0, []); // readyState 0, nothing buffered
    p.attach(video);
    instances[0].open();
    instances[0].streaming = false; // and startstreaming never fires

    p.pushSegment(init());
    p.pushSegment(media(true));
    p.pushSegment(media(false, 1));
    // The init went in on its own (one append in flight per track)…
    expect(instances[0].sb!.appended).toHaveLength(1);
    instances[0].sb!.finishUpdate();
    // …and the keyframe behind it, so the element can reach HAVE_METADATA and
    // tier 2 becomes reachable at all.
    expect(instances[0].sb!.appended).toHaveLength(2);

    // Once the element reports media, the pacing contract applies again: the
    // priming exemption is not a licence to ignore `streaming` forever, so the
    // delta behind the keyframe waits for the system to ask.
    setElementMedia(video, 1, [[0, 0.5]]);
    instances[0].sb!.finishUpdate();
    expect(instances[0].sb!.appended).toHaveLength(2);
    instances[0].setStreaming(true);
    expect(instances[0].sb!.appended).toHaveLength(3);
  });

  // A presenter holding segments and one never handed any both read appended
  // 0, errors 0. These three counters are what tells them apart remotely.
  it('reports what it received, what it is holding, and whether MMS is asking', () => {
    const { ctor, instances } = makeFakeMsCtor({ managed: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setElementMedia(video, 1, [[0, 5]]);
    p.attach(video);
    instances[0].open();
    instances[0].streaming = false;

    // Media arriving before any init is dropped on the floor — the shape a
    // sink that missed the muxer's one init segment produces forever.
    p.pushSegment(media(true));
    expect(p.getStats()).toMatchObject({ received: 1, droppedNoInit: 1, queued: 0 });

    p.pushSegment(init());
    p.pushSegment(media(true, 1));
    expect(p.getStats()).toMatchObject({
      received: 3,
      droppedNoInit: 1,
      queued: 2,
      streaming: false,
      segmentsAppended: 0,
      appendErrors: 0,
    });

    instances[0].setStreaming(true);
    expect(p.getStats().streaming).toBe(true);
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
    // Overflow discards the backlog up to the newest queued keyframe (60), so
    // what remains is that GOP onward plus the init it needs — and the drop is
    // counted (the on-device signature of an overflowed, parked appender).
    expect(p.getStats().queued).toBeLessThanOrEqual(MAX_QUEUED_SEGMENTS);
    expect(p.getStats().droppedOverflow).toBe(60);

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

  // docs/27 finding 3 item 5 / BUGS.md: MMS parks a fed, paused (armed) element,
  // the queue front-drops past its bound, and appends resume a whole park later
  // — a hole between the last appended frame and the resume keyframe, which
  // the native player stalls at. Chrome has no ManagedMediaSource, so this is
  // the only place `streaming === false` with a full queue is exercised.
  it('turns resume-after-park into a discontinuity instead of a hole in the timeline', () => {
    const { ctor, instances } = makeFakeMsCtor({ managed: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    // The element's buffered view is the (single) video SourceBuffer's.
    Object.defineProperty(video, 'readyState', { configurable: true, get: () => 4 });
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      get: () => instances[0].sb?.buffered ?? { length: 0, start: () => 0, end: () => 0 },
    });
    let currentTime = 0;
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => currentTime,
      set: (v: number) => {
        currentTime = v;
      },
    });
    Object.defineProperty(video, 'paused', { configurable: true, get: () => true }); // armed
    p.attach(video);
    instances[0].open();
    const drain = () => {
      let guard = 0;
      while (instances[0].sb?.updating && guard++ < 1000) instances[0].sb.finishUpdate();
    };
    const GOP = 30;
    let tag = 0;
    const next = () => {
      p.pushSegment(media(tag % GOP === 0, tag));
      tag++;
    };

    p.pushSegment(init());
    for (let i = 0; i < GOP; i++) {
      next();
      drain();
    }
    expect(video.buffered.length).toBe(1);

    // Park for longer than the queue holds.
    instances[0].streaming = false;
    for (let i = 0; i < MAX_QUEUED_SEGMENTS + 2 * GOP + 7; i++) next();
    expect(instances[0].sb!.appended.filter((d) => d[0] === 2)).toHaveLength(GOP);

    instances[0].setStreaming(true);
    drain();

    const b = video.buffered;
    expect(b.length).toBe(1); // no hole for the native player to stall at
    // The playhead was moved into what survives, not left on removed media.
    expect(currentTime).toBeGreaterThanOrEqual(b.start(0));
    expect(currentTime).toBeLessThanOrEqual(b.end(0));
    // …and the timeline runs up to the newest frame pushed.
    expect(b.end(0)).toBeCloseTo(tag * FRAME_S, 6);
    // Observable on-device: the queue shed segments, and the presenter healed.
    const stats = p.getStats();
    expect(stats.droppedOverflow).toBeGreaterThan(0);
    expect(stats.discontinuities).toBe(1);
    // The byte ledger forgets what the heal trimmed (PRUNE_BUDGET_BYTES counts
    // what is buffered, not what was once appended).
    const survived = instances[0].sb!.appended.filter(
      (d) => d[0] === 2 && (d[2] | (d[3] << 8)) * FRAME_S >= b.start(0) - 1e-9,
    );
    expect(stats.bufferedBytes).toBe(survived.reduce((n, d) => n + d.byteLength, 0));
  });

  it('re-anchors each track at its own newest range, so a trim never cuts a GOP', () => {
    const { ctor, instances } = makeFakeMsCtor({ managed: true });
    const p = new MsePresenter(ctor);
    const { video } = makeVideo();
    setElementMedia(video, 4, [[0, 1]]);
    p.setExpectedAudioMime(AUDIO_MIME);
    p.attach(video);
    instances[0].open();
    const drainAll = () => {
      let guard = 0;
      while (instances[0].buffers.some((b) => b.updating) && guard++ < 2000) {
        for (const b of instances[0].buffers) if (b.updating) b.finishUpdate();
      }
    };
    p.pushSegment(init());
    p.pushSegment(init(AUDIO_MIME, 'audio'));
    for (let i = 0; i < 10; i++) {
      p.pushSegment(media(i === 0, i));
      p.pushSegment(media(true, i, 'audio'));
      drainAll();
    }
    instances[0].streaming = false;
    // Video GOP of 30, audio every packet a sync sample.
    for (let i = 10; i < 10 + MAX_QUEUED_SEGMENTS + 40; i++) {
      p.pushSegment(media(i % 30 === 0, i));
      p.pushSegment(media(true, i, 'audio'));
    }
    instances[0].setStreaming(true);
    drainAll();
    const [vsb, asb] = instances[0].buffers;
    expect(vsb.buffered.length).toBe(1);
    expect(asb.buffered.length).toBe(1);
    // Video's trim stops exactly at its resume keyframe (a GOP boundary).
    const vStart = vsb.buffered.start(0) / FRAME_S;
    expect(Math.round(vStart) % 30).toBe(0);
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
    // …to the shared live target lag, not a hair inside the end: the cushion
    // this seek leaves is the whole jitter budget (docs/27 finding 3).
    expect(10 - currentTime).toBeCloseTo(liveEdgePolicy('live').targetLagS, 5);
  });

  // Appends arrive at 1× (after the paced release), so whatever a catch-up
  // seek leaves is the cushion for the rest of the session. It must be the
  // mode's target lag — never the old 0.1 s that sat below the measured
  // arrival jitter.
  function playingPresenter(mode?: ViewerDeliveryMode) {
    const { ctor, instances } = makeFakeMsCtor();
    const p = new MsePresenter(ctor);
    if (mode) p.setDeliveryMode(mode);
    const { video } = makeVideo();
    let end = 10;
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      get: () => ({ length: 1, start: () => 0, end: () => end }),
    });
    let currentTime = 0;
    Object.defineProperty(video, 'currentTime', {
      configurable: true,
      get: () => currentTime,
      set: (v: number) => {
        currentTime = v;
      },
    });
    Object.defineProperty(video, 'paused', { configurable: true, get: () => false });
    p.attach(video);
    instances[0].open();
    p.pushSegment(init());
    instances[0].sb!.finishUpdate();
    let n = 0;
    return {
      get lag() {
        return end - currentTime;
      },
      setPlayhead(t: number) {
        currentTime = t;
      },
      // One append that extends the buffered end — the burst a DVR catch-up
      // (or any arrival faster than 1×) produces.
      append(toEnd: number) {
        end = toEnd;
        p.pushSegment(media(true, ++n));
        instances[0].sb!.finishUpdate();
      },
    };
  }

  it('scales the catch-up landing by delivery mode', () => {
    for (const mode of ['live', 'resilient', 'deep'] as const) {
      const h = playingPresenter(mode);
      h.setPlayhead(1);
      h.append(20);
      expect(h.lag).toBeCloseTo(liveEdgePolicy(mode).targetLagS, 5);
    }
  });

  it('keeps the target cushion through a DVR catch-up burst', () => {
    const h = playingPresenter('deep');
    const { targetLagS, catchUpLagS } = liveEdgePolicy('deep');
    h.setPlayhead(10 - targetLagS); // landed at target
    // The burst lands seconds of media at once: one catch-up, which must keep
    // the target cushion rather than seeking to the very end…
    h.append(10 + catchUpLagS + 1);
    expect(h.lag).toBeCloseTo(targetLagS, 5);
    const landed = 10 + catchUpLagS + 1 - targetLagS;
    // …and burst appends that stay inside the trigger band only ADD cushion:
    // no seek throws it away.
    h.append(10 + catchUpLagS + 1 + (catchUpLagS - targetLagS) / 2);
    expect(h.lag).toBeGreaterThan(targetLagS);
    expect(h.lag).toBeLessThan(catchUpLagS);
    // (the playhead did not move)
    expect(h.lag).toBeCloseTo(10 + catchUpLagS + 1 + (catchUpLagS - targetLagS) / 2 - landed, 5);
  });

  it('does not catch up a resilient viewer inside its (wider) trigger', () => {
    const h = playingPresenter('resilient');
    h.setPlayhead(0);
    h.append(liveEdgePolicy('live').catchUpLagS + 0.2); // past live's trigger only
    expect(h.lag).toBeCloseTo(liveEdgePolicy('live').catchUpLagS + 0.2, 5);
  });

  describe('pruning', () => {
    // The capture behind BUGS.md: 3360×2100 at 60 fps. ~25 Mbps is a
    // conservative hardware-H.264 rate for 7 MP, i.e. ~52 KB per frame.
    const FPS = 60;
    const FRAME_BYTES = Math.round(25e6 / 8 / FPS);
    const GOP = 2 * FPS;
    const frame = (i: number): Extract<PresenterSegment, { kind: 'media' }> => ({
      kind: 'media',
      track: 'video',
      keyframe: i % GOP === 0,
      timeS: i / FPS,
      data: new Uint8Array(FRAME_BYTES),
    });

    function feed(
      p: MsePresenter,
      ms: { buffers: FakeSourceBuffer[] },
      ranges: Array<[number, number]>,
      el: { currentTime: number },
      seconds: number,
      opts: { followPlayhead?: boolean } = {},
    ): void {
      for (let i = 0; i < seconds * FPS; i++) {
        p.pushSegment(frame(i));
        const end = (i + 1) / FPS;
        // Our own removes move the buffered start, as the browser's would.
        const sb = ms.buffers[0];
        const cut = sb.removed.length > 0 ? sb.removed[sb.removed.length - 1][1] : 0;
        ranges[0] = [Math.max(ranges[0][0], cut), end];
        if (opts.followPlayhead !== false) el.currentTime = Math.max(0, end - 0.1);
        settle(ms);
      }
    }

    it('prunes 3360×2100-sized segments well before 20 s, by bytes', () => {
      const { ctor, instances } = makeFakeMsCtor({ managed: true });
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);

      feed(p, instances[0], ranges, el, 6);
      const sb = instances[0].buffers[0];
      // The old 30 s time trigger had not fired by 21 s; this must within 6 s.
      expect(sb.removed.length).toBeGreaterThan(0);

      feed(p, instances[0], ranges, el, 20);
      const stats = p.getStats();
      // Bounded by the budget plus at most one GOP kept behind the playhead.
      expect(stats.bufferedBytes).toBeLessThanOrEqual(PRUNE_BUDGET_BYTES + GOP * FRAME_BYTES);
      expect(stats.prunedBytes).toBeGreaterThan(0);
      expect(ranges[0][1] - ranges[0][0]).toBeLessThan(10);
    });

    it('cuts only at a keyframe at or before the playhead minus the margin', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);

      // A playhead that lags the edge: cuts must follow it, not the edge.
      for (let i = 0; i < 12 * FPS; i++) {
        p.pushSegment(frame(i));
        ranges[0] = [ranges[0][0], (i + 1) / FPS];
        el.currentTime = Math.max(0, (i + 1) / FPS - 1.5);
        const sb = instances[0].buffers[0];
        const before = sb.removed.length;
        settle(instances[0]);
        for (const [, cut] of sb.removed.slice(before)) {
          // On a GOP boundary — removing mid-GOP would take the playhead's
          // own GOP (its dependents up to the next keyframe) with it.
          expect((cut * FPS) % GOP).toBeCloseTo(0, 6);
          expect(cut).toBeLessThanOrEqual(el.currentTime - PRUNE_BEHIND_S);
          ranges[0] = [cut, ranges[0][1]];
        }
      }
      expect(instances[0].buffers[0].removed.length).toBeGreaterThan(0);
    });

    it('prunes a paused (armed) element relative to the live edge, not the stale playhead', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges, false);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);

      feed(p, instances[0], ranges, el, 12, { followPlayhead: false });
      expect(el.currentTime).toBe(0);
      const removed = instances[0].buffers[0].removed;
      expect(removed.length).toBeGreaterThan(0);
      const end = ranges[0][1];
      const lastCut = removed[removed.length - 1][1];
      // The entry seek lands near the edge; what it needs is kept.
      expect(lastCut).toBeLessThanOrEqual(end - PRUNE_BEHIND_S);
      expect(p.getStats().bufferedBytes).toBeLessThanOrEqual(PRUNE_BUDGET_BYTES + GOP * FRAME_BYTES);
    });

    it('keeps the Deep buffer entry seek cushion when pruning a paused element', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      p.setDeliveryMode('deep');
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges, false);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);

      feed(p, instances[0], ranges, el, 12, { followPlayhead: false });
      const removed = instances[0].buffers[0].removed;
      expect(removed.length).toBeGreaterThan(0);
      const end = ranges[0][1];
      const lastCut = removed[removed.length - 1][1];
      // The entry seek lands liveEdgePolicy('deep').targetLagS behind the edge.
      expect(lastCut).toBeLessThanOrEqual(end - liveEdgePolicy('deep').targetLagS - PRUNE_BEHIND_S);
    });

    it('still prunes small segments by the time ceiling', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);
      for (let i = 0; i < (PRUNE_CEILING_S + 5) * 10; i++) {
        p.pushSegment(media(i % 10 === 0, i, 'video', i / 10));
        ranges[0] = [0, (i + 1) / 10];
        el.currentTime = (i + 1) / 10 - 0.1;
        settle(instances[0]);
      }
      expect(p.getStats().bufferedBytes).toBeLessThan(PRUNE_BUDGET_BYTES);
      expect(instances[0].buffers[0].removed.length).toBeGreaterThan(0);
    });

    it('does not prune inside the only GOP', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);
      // One keyframe, then deltas far past the budget: nothing is removable.
      for (let i = 0; i < 6 * FPS; i++) {
        p.pushSegment({ ...frame(i), keyframe: i === 0 });
        ranges[0] = [0, (i + 1) / FPS];
        el.currentTime = (i + 1) / FPS - 0.1;
        settle(instances[0]);
      }
      expect(instances[0].buffers[0].removed).toEqual([]);
    });

    it('re-attaching starts the byte accounting over', () => {
      const { ctor, instances } = makeFakeMsCtor();
      const p = new MsePresenter(ctor);
      const ranges: Array<[number, number]> = [[0, 0]];
      const { video, el } = makePlayingVideo(ranges);
      p.attach(video);
      instances[0].open();
      p.pushSegment(init());
      settle(instances[0]);
      feed(p, instances[0], ranges, el, 1);
      expect(p.getStats().bufferedBytes).toBeGreaterThan(0);
      p.detach();
      expect(p.getStats().bufferedBytes).toBe(0);
    });
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
