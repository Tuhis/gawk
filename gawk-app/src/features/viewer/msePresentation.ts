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

// ---------------------------------------------------------------------------
// The presenter (MF3/MF4)

export type PresenterSegment =
  | { kind: 'init'; mime: string; data: Uint8Array }
  | { kind: 'media'; keyframe: boolean; data: Uint8Array };

export interface MsePresenterStats {
  attached: boolean;
  sourceOpen: boolean;
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

export class MsePresenter {
  private ctor: MediaSourceCtor | null;
  private video: HTMLVideoElement | null = null;
  private ms: MediaSourceLike | null = null;
  private sb: SourceBufferLike | null = null;
  private objectUrl: string | null = null;
  private sourceOpen = false;
  private failed = false;

  // The newest init segment, cached so a re-attach (StrictMode remount, video
  // element swap) can re-prime a fresh MediaSource without a worker round-trip.
  private cachedInit: { mime: string; data: Uint8Array } | null = null;
  private sbMime: string | null = null;
  private queue: PresenterSegment[] = [];
  // After any queue drop, media segments are discarded until a keyframe
  // segment restores a decodable start.
  private needKeyframe = false;

  private segmentsAppended = 0;
  private appendErrors = 0;

  private readonly onSourceOpen = () => {
    this.sourceOpen = true;
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
      segmentsAppended: this.segmentsAppended,
      appendErrors: this.appendErrors,
      queued: this.queue.length,
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
    if (this.cachedInit) {
      this.queue = this.queue.filter((s) => s.kind !== 'init');
      this.queue.unshift({ kind: 'init', ...this.cachedInit });
      this.needKeyframe = true;
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
    if (this.sb) {
      this.sb.removeEventListener('updateend', this.onUpdateEnd);
      this.sb.removeEventListener('error', this.onSbError);
    }
    this.sb = null;
    this.sbMime = null;
    this.ms = null;
    this.sourceOpen = false;
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
    if (seg.kind === 'init') {
      this.cachedInit = { mime: seg.mime, data: seg.data };
    } else if (!this.cachedInit) {
      // Media before any init cannot ever decode; don't queue it.
      return;
    }
    this.queue.push(seg);
    while (this.queue.length > MAX_QUEUED_SEGMENTS) {
      // Dropping anything breaks the dependent stream: resume at a keyframe.
      // (A dropped init re-primes from cachedInit in pump() when needed.)
      this.queue.shift();
      this.needKeyframe = true;
    }
    this.pump();
  }

  dispose(): void {
    this.detach();
    this.cachedInit = null;
    this.queue = [];
  }

  // Serialized appender: one appendBuffer in flight (updateend re-drives),
  // parked while MMS says streaming is off.
  private pump(): void {
    if (this.failed || !this.ms || !this.sourceOpen) return;
    // MMS pacing: `streaming` false parks the queue until startstreaming.
    if (this.ms.streaming === false) return;
    if (this.sb?.updating) return;

    while (this.queue.length > 0) {
      const seg = this.queue[0];
      if (seg.kind === 'media' && this.needKeyframe && !seg.keyframe) {
        this.queue.shift();
        continue;
      }
      if (seg.kind === 'media' && !this.sb && this.cachedInit) {
        // The init segment fell off the queue (overflow) before it could
        // append — re-prime from the cache, then reprocess this segment.
        this.queue.unshift({ kind: 'init', ...this.cachedInit });
        continue;
      }
      this.queue.shift();
      if (seg.kind === 'init') {
        if (!this.ensureSourceBuffer(seg.mime)) return;
      } else if (!this.sb) {
        // Media without a SourceBuffer and no cached init: nothing to do.
        continue;
      }
      if (seg.kind === 'media' && seg.keyframe) this.needKeyframe = false;
      try {
        this.sb!.appendBuffer(seg.data as BufferSource);
        this.segmentsAppended++;
      } catch (e) {
        // QuotaExceeded and friends: drop this segment, resync at the next
        // keyframe. The relay-side drops-over-stalls philosophy, locally.
        this.appendErrors++;
        this.needKeyframe = true;
        log.warn('MSE presentation: appendBuffer failed:', e);
        continue;
      }
      return; // one append in flight; updateend pumps again
    }
  }

  private ensureSourceBuffer(mime: string): boolean {
    if (this.sb) {
      if (this.sbMime !== mime) {
        // Codec change mid-stream (R13 pin / broadcaster swap): changeType
        // where it exists; without it the surface degrades to pseudo.
        try {
          if (typeof this.sb.changeType !== 'function') throw new Error('changeType unavailable');
          this.sb.changeType(mime);
          this.sbMime = mime;
        } catch (e) {
          this.appendErrors++;
          this.failed = true;
          log.warn('MSE presentation: codec change failed:', e);
          return false;
        }
      }
      return true;
    }
    try {
      const sb = this.ms!.addSourceBuffer(mime);
      sb.addEventListener('updateend', this.onUpdateEnd);
      sb.addEventListener('error', this.onSbError);
      this.sb = sb;
      this.sbMime = mime;
      return true;
    } catch (e) {
      // The trial-addSourceBuffer half of the probe, failing for real: mark
      // failed so the gate reports pseudo (docs/27 MF2).
      this.appendErrors++;
      this.failed = true;
      log.warn('MSE presentation: addSourceBuffer failed:', e);
      return false;
    }
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
    const sb = this.sb;
    if (!video || !sb || sb.updating) return;
    try {
      const b = video.buffered;
      if (b.length === 0) return;
      const start = b.start(0);
      const end = b.end(b.length - 1);
      if (end - start > PRUNE_TRIGGER_S) {
        sb.remove(start, end - PRUNE_KEEP_S);
      }
    } catch {
      // pruning is best-effort
    }
  }
}
