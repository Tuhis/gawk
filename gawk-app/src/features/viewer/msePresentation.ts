// R22 (docs/27 Decisions 2/5/6): the main-thread half of iPhone native
// fullscreen. The viewer worker muxes the encoded stream into fMP4 segments
// (fmp4-muxer.ts); this module owns what must live on the main thread — the
// ManagedMediaSource, its SourceBuffer, and the hidden presentation <video>
// the fullscreen tap targets. Constructed only on gated (element-fullscreen-
// less) devices after the capability probe passes, so every other platform is
// byte-identical.
//
// ManagedMediaSource (MMS) is the MSE variant iPhone actually ships (iOS
// 17.1+; plain MediaSource may be undefined there). The system paces appends
// via `streaming` + startstreaming/endstreaming and evicts old buffered
// ranges itself; where only classic MediaSource exists (desktop Chrome — the
// CI proof, docs/27 Decision 10) the presenter prunes manually.

import { log } from '../../lib/logger';
import { opusMime, type Fmp4Track } from '../../transport/fmp4-muxer';

// ---------------------------------------------------------------------------
// Capability probe (MF2)

export interface MseProbeResult {
  supported: boolean;
  mime: string | null;
  // Human-readable verdict for the Feature Gates row / Copy diagnostics.
  reason: string;
}

// Minimal structural types: lib.dom has MediaSource but not ManagedMediaSource,
// and tests fake both.
export interface SourceBufferLike {
  updating: boolean;
  appendBuffer(data: BufferSource): void;
  remove(start: number, end: number): void;
  changeType?(mime: string): void;
  addEventListener(type: string, cb: () => void): void;
  removeEventListener(type: string, cb: () => void): void;
}

export interface MediaSourceLike {
  readyState: string;
  // NaN until set; MSE otherwise keeps raising it to the newest appended end
  // timestamp (see declareLive).
  duration: number;
  streaming?: boolean; // ManagedMediaSource only
  addSourceBuffer(mime: string): SourceBufferLike;
  addEventListener(type: string, cb: () => void): void;
  removeEventListener(type: string, cb: () => void): void;
}

export type MediaSourceCtor = (new () => MediaSourceLike) & {
  isTypeSupported(mime: string): boolean;
};

// ManagedMediaSource where it exists (iPhone), classic MediaSource otherwise
// (desktop test harnesses; on iPhone it may be absent — that's the point).
export function getMediaSourceCtor(): MediaSourceCtor | null {
  const g = globalThis as {
    ManagedMediaSource?: MediaSourceCtor;
    MediaSource?: MediaSourceCtor;
  };
  return g.ManagedMediaSource ?? g.MediaSource ?? null;
}

// The capability verdict for a negotiated codec (docs/27 MF2). H.264-only in
// v1 (Decision 11): VP8/VP9-in-fMP4 support on iOS is unreliable and the
// muxing differs, so a VP broadcast probes false and the button falls back to
// CSS pseudo-fullscreen. The trial addSourceBuffer half of the probe is the
// arm itself — MSE needs an open, element-attached MediaSource before
// addSourceBuffer is legal, and the arm happens at `watching`, well before
// any fullscreen tap; its failure degrades the gate the same way.
export function probeMsePresentation(codec: string): MseProbeResult {
  if (!/^avc1\./i.test(codec)) {
    return { supported: false, mime: null, reason: `codec ${codec} is not H.264` };
  }
  const Ctor = getMediaSourceCtor();
  if (!Ctor) {
    return { supported: false, mime: null, reason: 'no MediaSource/ManagedMediaSource' };
  }
  const mime = `video/mp4; codecs="${codec}"`;
  let ok = false;
  try {
    ok = Ctor.isTypeSupported(mime);
  } catch {
    ok = false;
  }
  if (!ok) return { supported: false, mime: null, reason: `unsupported: ${mime}` };
  return { supported: true, mime, reason: 'MSE available' };
}

// R22 audio (docs/27 finding 2): can this device take the R15 Opus lane as an
// MSE audio track? Strictly additive — a refusal keeps the native player
// video-only with audio still coming from the inline AudioWorklet, which is the
// pre-audio-muxing behavior. Opus-in-MP4 is a WebKit 17 feature ("one or two
// channel Opus audio in WebM and MPEG-4 containers"), but nothing in Apple's
// documentation promises it through ManagedMediaSource, and HLS's own mandated
// audio codec is AAC — so this is a runtime question by design, not an
// assumption. `channels` guards the same one-or-two-channel limit.
export function probeMseAudio(
  codec: string | null,
  channels: number | null,
): MseProbeResult {
  if (codec == null) return { supported: false, mime: null, reason: 'no audio' };
  if (!/^opus$/i.test(codec)) {
    return { supported: false, mime: null, reason: `codec ${codec} is not Opus` };
  }
  if (channels != null && (channels < 1 || channels > 2)) {
    return { supported: false, mime: null, reason: `${channels} channels unsupported in MP4` };
  }
  const Ctor = getMediaSourceCtor();
  if (!Ctor) return { supported: false, mime: null, reason: 'no MediaSource/ManagedMediaSource' };
  const mime = opusMime();
  let ok = false;
  try {
    ok = Ctor.isTypeSupported(mime);
  } catch {
    ok = false;
  }
  if (!ok) return { supported: false, mime: null, reason: `unsupported: ${mime}` };
  return { supported: true, mime, reason: 'Opus in MP4 available' };
}

// ---------------------------------------------------------------------------
// The presenter (MF3/MF4)

export type PresenterSegment =
  | { kind: 'init'; track: Fmp4Track; mime: string; data: Uint8Array }
  | { kind: 'media'; track: Fmp4Track; keyframe: boolean; data: Uint8Array };

export interface MsePresenterStats {
  attached: boolean;
  sourceOpen: boolean;
  // R22 audio: whether an audio SourceBuffer exists (i.e. audio segments are
  // actually being presented), and how many samples it has taken.
  audioTrack: boolean;
  audioSegmentsAppended: number;
  // Whether this MediaSource actually accepted duration = Infinity. False on an
  // open source means the native player will draw a finite timeline and treat
  // the buffered end as end-of-media (docs/27 finding 1).
  liveDuration: boolean;
  segmentsAppended: number;
  appendErrors: number;
  queued: number;
  failed: boolean;
}

// Bound on segments waiting for the SourceBuffer (one segment per frame ⇒
// ~2 s at 60 fps). Overflow drops from the FRONT and then discards up to the
// next keyframe-starting segment: media segments are a dependent stream, so
// resuming anywhere else would append deltas whose references were dropped.
export const MAX_QUEUED_SEGMENTS = 128;

// Manual pruning for classic MediaSource (MMS evicts on its own): once the
// buffered span exceeds the trigger, everything but the newest KEEP window is
// removed. Generous — the point is boundedness over a long session, not
// tightness.
export const PRUNE_TRIGGER_S = 30;
export const PRUNE_KEEP_S = 10;

// While the (fullscreen) video is playing, jumping this far behind the
// buffered end triggers a catch-up seek — MF4's "seek-to-live keeps native
// fullscreen near live". Paused (inline, armed) playback never seeks.
export const LIVE_CATCHUP_LAG_S = 2;
export const LIVE_EDGE_REJOIN_S = 0.1;

// Per-track appender state. Video and audio are independent MSE streams with
// independent queues, SourceBuffers and in-flight appends; what couples them is
// the element, whose `buffered` is the INTERSECTION of the two (so an audio hole
// stalls video — see the muxer's AUDIO_MAX_STRETCH_MS).
interface TrackState {
  readonly track: Fmp4Track;
  sb: SourceBufferLike | null;
  sbMime: string | null;
  // The newest init segment, cached so a re-attach (StrictMode remount, video
  // element swap) can re-prime a fresh MediaSource without a worker round-trip.
  cachedInit: { mime: string; data: Uint8Array } | null;
  queue: PresenterSegment[];
  // After any queue drop, media segments are discarded until a keyframe segment
  // restores a decodable start. (Always immediately satisfied on audio: every
  // Opus packet is a sync sample.)
  needKeyframe: boolean;
  appended: number;
}

function newTrackState(track: Fmp4Track): TrackState {
  return {
    track,
    sb: null,
    sbMime: null,
    cachedInit: null,
    queue: [],
    needKeyframe: false,
    appended: 0,
  };
}

export class MsePresenter {
  private ctor: MediaSourceCtor | null;
  private video: HTMLVideoElement | null = null;
  private ms: MediaSourceLike | null = null;
  private objectUrl: string | null = null;
  private sourceOpen = false;
  private failed = false;

  private readonly videoBuf = newTrackState('video');
  private readonly audioBuf = newTrackState('audio');
  private get tracks(): TrackState[] {
    return [this.videoBuf, this.audioBuf];
  }

  private segmentsAppended = 0;
  private appendErrors = 0;
  // Per-MediaSource: whether duration = Infinity stuck (see declareLive).
  private liveDuration = false;

  private readonly onSourceOpen = () => {
    this.sourceOpen = true;
    this.declareLive();
    this.pump();
  };
  private readonly onUpdateEnd = () => {
    this.maybeCatchUp();
    this.prune();
    this.pump();
  };
  private readonly onSbError = () => {
    // A SourceBuffer error is terminal for this MediaSource (readyState goes
    // 'closed' per spec on decode errors surfaced here). The gate degrades;
    // the inline canvas never depended on any of this.
    this.appendErrors++;
    this.failed = true;
    log.warn('MSE presentation: SourceBuffer error; native fullscreen degraded');
  };
  private readonly onStartStreaming = () => this.pump();

  constructor(ctor: MediaSourceCtor | null = getMediaSourceCtor()) {
    this.ctor = ctor;
  }

  getStats(): MsePresenterStats {
    return {
      attached: this.video !== null,
      sourceOpen: this.sourceOpen,
      liveDuration: this.liveDuration,
      audioTrack: this.audioBuf.sb !== null,
      audioSegmentsAppended: this.audioBuf.appended,
      segmentsAppended: this.segmentsAppended,
      appendErrors: this.appendErrors,
      queued: this.videoBuf.queue.length + this.audioBuf.queue.length,
      failed: this.failed,
    };
  }

  // Attach the hidden <video>: creates a fresh MediaSource and wires it up.
  // Media starts flowing once the cached/next init segment lands.
  attach(video: HTMLVideoElement): void {
    if (this.video === video) return;
    this.detach();
    if (!this.ctor) return;
    this.video = video;
    this.failed = false;
    let ms: MediaSourceLike;
    try {
      ms = new this.ctor();
    } catch (e) {
      this.failed = true;
      log.warn('MSE presentation: MediaSource construction failed:', e);
      return;
    }
    this.ms = ms;
    ms.addEventListener('sourceopen', this.onSourceOpen);
    // MMS pacing: appends only while the system says `streaming` (its
    // startstreaming kicks the pump; endstreaming just parks the queue).
    ms.addEventListener('startstreaming', this.onStartStreaming);
    // MMS + AirPlay conflict: WebKit requires remote playback disabled.
    try {
      (video as HTMLVideoElement & { disableRemotePlayback?: boolean }).disableRemotePlayback = true;
    } catch {
      // best-effort
    }
    // WebKit accepts a MediaSource as srcObject (the MMS-recommended wiring);
    // Chromium (the CI path) only via object URL.
    try {
      (video as unknown as { srcObject: unknown }).srcObject = ms;
    } catch {
      try {
        this.objectUrl = URL.createObjectURL(ms as unknown as MediaSource);
        video.src = this.objectUrl;
      } catch (e) {
        this.failed = true;
        log.warn('MSE presentation: attaching MediaSource failed:', e);
        return;
      }
    }
    // A fresh MediaSource has no history: re-prime it from the cached init,
    // then resume at the next keyframe (the queued deltas' references are in
    // the OLD SourceBuffer).
    for (const t of this.tracks) {
      if (!t.cachedInit) continue;
      t.queue = t.queue.filter((s) => s.kind !== 'init');
      // Audio primes lazily from the cache (see pushSegment): an audio
      // SourceBuffer with no samples would empty the element's buffered
      // intersection and stall the video it is supposed to accompany.
      if (t.track === 'video') t.queue.unshift({ kind: 'init', track: t.track, ...t.cachedInit });
      t.needKeyframe = true;
    }
  }

  // Detach from the current video (component re-render/remount). Keeps the
  // cached init and the queue so attach() can resume; drops the MediaSource,
  // which is bound to the element.
  detach(): void {
    const video = this.video;
    if (this.ms) {
      this.ms.removeEventListener('sourceopen', this.onSourceOpen);
      this.ms.removeEventListener('startstreaming', this.onStartStreaming);
    }
    for (const t of this.tracks) {
      if (t.sb) {
        t.sb.removeEventListener('updateend', this.onUpdateEnd);
        t.sb.removeEventListener('error', this.onSbError);
      }
      t.sb = null;
      t.sbMime = null;
    }
    this.ms = null;
    this.sourceOpen = false;
    // Per-MediaSource state: the next attach() builds a fresh one that has
    // never heard of an infinite duration.
    this.liveDuration = false;
    if (video) {
      try {
        (video as HTMLVideoElement & { srcObject: unknown }).srcObject = null;
      } catch {
        // never attached via srcObject
      }
      video.removeAttribute('src');
    }
    if (this.objectUrl) {
      URL.revokeObjectURL(this.objectUrl);
      this.objectUrl = null;
    }
    this.video = null;
  }

  // Feed one worker segment. Init segments are cached (re-attach priming) and
  // queued in-order; media segments queue behind them.
  pushSegment(seg: PresenterSegment): void {
    const t = seg.track === 'audio' ? this.audioBuf : this.videoBuf;
    if (seg.kind === 'init') {
      t.cachedInit = { mime: seg.mime, data: seg.data };
      // An audio track that exists but has no samples empties the element's
      // buffered intersection — so the audio init is only *cached* until a
      // sample is ready to follow it, and pump() primes the SourceBuffer with it
      // then. Once the SourceBuffer exists, a later init is a real parameter
      // change and must be appended in order.
      if (t.track === 'audio' && !t.sb) return;
    } else if (!t.cachedInit) {
      // Media before any init cannot ever decode; don't queue it.
      return;
    }
    t.queue.push(seg);
    while (t.queue.length > MAX_QUEUED_SEGMENTS) {
      // Dropping anything breaks the dependent stream: resume at a keyframe.
      // (A dropped init re-primes from cachedInit in pump() when needed.)
      t.queue.shift();
      t.needKeyframe = true;
    }
    this.pump();
  }

  dispose(): void {
    this.detach();
    for (const t of this.tracks) {
      t.cachedInit = null;
      t.queue = [];
    }
  }

  // A live presentation must say so: `duration = Infinity` (docs/27 finding 1).
  // Left unset, MSE's coded-frame processing raises duration to the newest
  // appended end timestamp, which costs two things on the native player —
  //   * the LIVE badge, replaced by a finite scrub bar over a growing duration;
  //   * the distinction between "the playhead reached the buffered end" and
  //     "the media resource ended". WebKit resolves that ambiguity by pausing
  //     and firing `ended` (where Chromium stalls and resumes on more data), so
  //     every underrun became a dead player needing a manual tap.
  // Infinity is never less than the highest end timestamp, so appends can't
  // lower it again and one write per MediaSource is enough. It must happen
  // exactly here: the setter throws unless readyState is 'open' with nothing
  // updating, which is true at sourceopen (no SourceBuffer exists yet) and
  // false during the append loop.
  private declareLive(): void {
    const ms = this.ms;
    if (!ms || this.liveDuration) return;
    try {
      ms.duration = Infinity;
      this.liveDuration = ms.duration === Infinity;
    } catch (e) {
      // Non-fatal and deliberately not `failed`: a finite duration degrades the
      // native player's UI and stall behavior, it doesn't stop it presenting.
      log.warn('MSE presentation: could not declare an infinite duration:', e);
    }
  }

  // Serialized appender: one appendBuffer in flight (updateend re-drives),
  // parked while MMS says streaming is off.
  private pump(): void {
    if (this.failed || !this.ms || !this.sourceOpen) return;
    // MMS pacing: `streaming` false parks the queue until startstreaming.
    if (this.ms.streaming === false) return;
    // Each track appends independently — a video append in flight must not hold
    // audio back (and vice versa), or the two buffers drift by a whole append
    // round-trip each frame.
    for (const t of this.tracks) this.pumpTrack(t);
  }

  private pumpTrack(t: TrackState): void {
    if (t.sb?.updating) return;

    while (t.queue.length > 0) {
      const seg = t.queue[0];
      if (seg.kind === 'media' && t.needKeyframe && !seg.keyframe) {
        t.queue.shift();
        continue;
      }
      if (seg.kind === 'media' && !t.sb && t.cachedInit) {
        // No SourceBuffer yet — either the init fell off the queue (overflow) or
        // this is the audio track priming lazily. Either way the cached init
        // goes first, then this segment is reprocessed.
        t.queue.unshift({ kind: 'init', track: t.track, ...t.cachedInit });
        continue;
      }
      t.queue.shift();
      if (seg.kind === 'init') {
        if (!this.ensureSourceBuffer(t, seg.mime)) return;
      } else if (!t.sb) {
        // Media without a SourceBuffer and no cached init: nothing to do.
        continue;
      }
      if (seg.kind === 'media' && seg.keyframe) t.needKeyframe = false;
      try {
        t.sb!.appendBuffer(seg.data as BufferSource);
        t.appended++;
        this.segmentsAppended++;
      } catch (e) {
        // QuotaExceeded and friends: drop this segment, resync at the next
        // keyframe. The relay-side drops-over-stalls philosophy, locally.
        this.appendErrors++;
        t.needKeyframe = true;
        log.warn(`MSE presentation: ${t.track} appendBuffer failed:`, e);
        continue;
      }
      return; // one append in flight per track; updateend pumps again
    }
  }

  private ensureSourceBuffer(t: TrackState, mime: string): boolean {
    if (t.sb) {
      if (t.sbMime !== mime) {
        // Codec change mid-stream (R13 pin / broadcaster swap): changeType
        // where it exists; without it the surface degrades to pseudo.
        try {
          if (typeof t.sb.changeType !== 'function') throw new Error('changeType unavailable');
          t.sb.changeType(mime);
          t.sbMime = mime;
        } catch (e) {
          this.appendErrors++;
          if (t.track === 'video') this.failed = true;
          log.warn(`MSE presentation: ${t.track} codec change failed:`, e);
          return false;
        }
      }
      return true;
    }
    try {
      const sb = this.ms!.addSourceBuffer(mime);
      sb.addEventListener('updateend', this.onUpdateEnd);
      sb.addEventListener('error', this.onSbError);
      t.sb = sb;
      t.sbMime = mime;
      return true;
    } catch (e) {
      // The trial-addSourceBuffer half of the probe, failing for real: mark
      // failed so the gate reports pseudo (docs/27 MF2) — but only for video.
      // Audio is additive: a refused audio SourceBuffer (the plausible
      // Opus-in-MP4 outcome on iOS) leaves the video presentation intact and
      // drops the audio track for good.
      this.appendErrors++;
      if (t.track === 'video') this.failed = true;
      else this.dropAudioTrack();
      log.warn(`MSE presentation: ${t.track} addSourceBuffer failed:`, e);
      return false;
    }
  }

  // Give up on the audio track without disturbing video: no SourceBuffer is left
  // half-created, and nothing further is queued (pushSegment drops media with no
  // cachedInit). The inline AudioWorklet sink keeps playing the audio either way.
  private dropAudioTrack(): void {
    this.audioBuf.cachedInit = null;
    this.audioBuf.queue = [];
    this.audioBuf.needKeyframe = false;
  }

  // MF4: while the (fullscreen) video plays, a playhead that fell behind the
  // buffered end beyond the lag bound jumps back to the live edge. Paused
  // (armed, inline) video never seeks — Decision 5's loaded-but-paused state.
  private maybeCatchUp(): void {
    const video = this.video;
    if (!video || video.paused) return;
    try {
      const b = video.buffered;
      if (b.length === 0) return;
      const end = b.end(b.length - 1);
      if (end - video.currentTime > LIVE_CATCHUP_LAG_S) {
        video.currentTime = Math.max(b.start(b.length - 1), end - LIVE_EDGE_REJOIN_S);
      }
    } catch {
      // buffered/seek quirks must never break the append loop
    }
  }

  // Manual eviction for classic MediaSource; MMS manages its own buffer (its
  // remove() semantics are the same, so running this there is harmless but
  // usually a no-op given the trigger).
  private prune(): void {
    const video = this.video;
    if (!video) return;
    try {
      // buffered is the intersection of the tracks, which is the right trigger:
      // it is the playable span, and both tracks are pruned to the same window
      // so the intersection can't be shortened by an asymmetric eviction.
      const b = video.buffered;
      if (b.length === 0) return;
      const start = b.start(0);
      const end = b.end(b.length - 1);
      if (end - start <= PRUNE_TRIGGER_S) return;
      for (const t of this.tracks) {
        if (!t.sb || t.sb.updating) continue;
        t.sb.remove(start, end - PRUNE_KEEP_S);
      }
    } catch {
      // pruning is best-effort
    }
  }
}
