// The main-thread half of iPhone native fullscreen. The viewer worker muxes
// the encoded stream into fMP4 segments (fmp4-muxer.ts); this module owns what
// must live on the main thread — the ManagedMediaSource, its SourceBuffer, and
// the hidden presentation <video> the fullscreen tap targets. Constructed only
// on gated (element-fullscreen-less) devices after the capability probe
// passes, so no other platform runs any of it.
//
// ManagedMediaSource (MMS) is the MSE variant iPhone actually ships (iOS
// 17.1+; plain MediaSource may be undefined there). The system paces appends
// via `streaming` + startstreaming/endstreaming and is meant to evict old
// buffered ranges itself — but on iPhone it was observed holding 21 s of 7 MP
// media, so the presenter prunes to a byte budget on every MediaSource.

import { liveEdgePolicy, liveEdgeSeekTarget } from '../../lib/liveEdge';
import { log } from '../../lib/logger';
import { aacMime, opusMime, type AudioMuxCodec, type Fmp4Track } from '../../transport/fmp4-muxer';
import { AAC_CODEC } from '../../transport/audio-transcode';
import type { ViewerDeliveryMode } from '../../transport/resilient';

// ---------------------------------------------------------------------------
// Capability probe

export interface MseProbeResult {
  supported: boolean;
  mime: string | null;
  // Human-readable verdict for the Feature Gates row / Copy diagnostics.
  reason: string;
}

// The audio verdict also says WHICH encapsulation won, since that decides
// whether the worker transcodes.
export interface MseAudioProbe extends MseProbeResult {
  codec: AudioMuxCodec | null;
}

// Minimal structural types: lib.dom has MediaSource but not ManagedMediaSource,
// and tests fake both.
export interface SourceBufferLike {
  updating: boolean;
  appendBuffer(data: BufferSource): void;
  remove(start: number, end: number): void;
  // This buffer's own ranges (the element's are the tracks' intersection).
  // Optional only so minimal fakes can omit it; every real SourceBuffer has it.
  readonly buffered?: TimeRanges;
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

// The capability verdict for a negotiated codec. H.264 only: VP8/VP9-in-fMP4
// support on iOS is unreliable and the muxing differs, so a VP broadcast probes false and the button falls back to
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

// How (if at all) this device can take the audio lane as an MSE audio track.
// Two tiers, in cost order — Opus muxed verbatim, else AAC re-encoded from the
// decoded PCM. Strictly additive: with neither, the native player stays
// video-only and audio keeps coming from the inline AudioWorklet.
//
// Why two tiers rather than one: Opus-in-MP4 is a WebKit 17 feature ("one or two
// channel Opus audio in WebM and MPEG-4 containers") and Chrome accepts it, but
// iOS 18.7's ManagedMediaSource answers `isTypeSupported('audio/mp4;
// codecs="opus"')` with **false** — while AAC is the codec Apple's own HLS
// mandates. `channels` guards MP4 Opus's one-or-two-channel limit; it does not
// constrain the AAC tier.
export function probeMseAudio(codec: string | null, channels: number | null): MseAudioProbe {
  if (codec == null) return { supported: false, codec: null, mime: null, reason: 'no audio' };
  if (!/^opus$/i.test(codec)) {
    return { supported: false, codec: null, mime: null, reason: `codec ${codec} is not Opus` };
  }
  if (channels != null && (channels < 1 || channels > 2)) {
    return {
      supported: false,
      codec: null,
      mime: null,
      reason: `${channels} channels unsupported in MP4`,
    };
  }
  const Ctor = getMediaSourceCtor();
  if (!Ctor) {
    return { supported: false, codec: null, mime: null, reason: 'no MediaSource/ManagedMediaSource' };
  }
  const supports = (mime: string): boolean => {
    try {
      return Ctor.isTypeSupported(mime);
    } catch {
      return false;
    }
  };
  // Opus first: it needs no transcode, and where the runtime takes it (Chrome)
  // the muxed track is the audio lane verbatim.
  const opus = opusMime();
  if (supports(opus)) {
    return { supported: true, codec: 'opus', mime: opus, reason: 'Opus in MP4 available' };
  }
  // iOS 18.7 / Safari 26.5.2 refuses Opus in MP4 through ManagedMediaSource,
  // so fall back to AAC, the codec Apple's own HLS mandates, by re-encoding
  // the decoded PCM in the worker.
  const aac = aacMime(AAC_CODEC);
  if (supports(aac)) {
    return {
      supported: true,
      codec: 'aac',
      mime: aac,
      reason: `${opus} refused → transcoding to ${AAC_CODEC}`,
    };
  }
  return { supported: false, codec: null, mime: null, reason: `unsupported: ${opus} and ${aac}` };
}

function errText(e: unknown): string {
  return e instanceof Error ? `${e.name}: ${e.message}` : String(e);
}

// ---------------------------------------------------------------------------
// The presenter

export type PresenterSegment =
  | { kind: 'init'; track: Fmp4Track; mime: string; data: Uint8Array }
  // `timeS`: the segment's start on the SourceBuffer timeline (its tfdt in
  // seconds), which is what lets pruning cut exactly at a keyframe.
  | { kind: 'media'; track: Fmp4Track; keyframe: boolean; timeS: number; data: Uint8Array };

export interface MsePresenterStats {
  attached: boolean;
  sourceOpen: boolean;
  // ManagedMediaSource's `streaming` — whether the system is currently asking
  // for data. Null on classic MediaSource (no such attribute) or before a
  // source exists. The one field that separates "the presenter got nothing to
  // append" from "the presenter is holding segments the system won't take".
  streaming: boolean | null;
  // Segments handed to pushSegment, and media segments dropped there because
  // their track had no cached init segment. The muxer's counters live in the
  // worker, so without these a broken worker→main→sink hop and a parked
  // appender produce identical stats (appended 0, errors 0).
  received: number;
  droppedNoInit: number;
  // Segments shed because a track's queue overflowed MAX_QUEUED_SEGMENTS — the
  // MMS-parking signature (docs/27 finding 3 item 5): nonzero is what tells a
  // parked-and-overflowed appender apart from every other cause of a short
  // `segmentsAppended`. `discontinuities` counts the resumes after such a drop
  // that found a hole and healed it (seek into the newest range, trim the rest).
  droppedOverflow: number;
  discontinuities: number;
  // Whether an audio SourceBuffer exists (i.e. audio segments are
  // actually being presented), and how many samples it has taken.
  audioTrack: boolean;
  audioSegmentsAppended: number;
  // Whether this MediaSource actually accepted duration = Infinity. False on an
  // open source means the native player will draw a finite timeline and treat
  // the buffered end as end-of-media.
  liveDuration: boolean;
  segmentsAppended: number;
  appendErrors: number;
  // WHAT the most recent append failure actually was. The only place WebKit
  // gives the reason is `HTMLMediaElement.error` at the moment it fires, and
  // the audio drop's rebuild clears it immediately after, so it is captured at
  // every site that increments appendErrors; null until one does.
  lastError: string | null;
  queued: number;
  failed: boolean;
  // Video bytes this MediaSource currently holds by the presenter's own
  // accounting (appended minus pruned), and video bytes pruned this session.
  bufferedBytes: number;
  prunedBytes: number;
}

// Bound on segments waiting for the SourceBuffer (one segment per frame ⇒
// ~2 s at 60 fps). Overflow discards the whole stale backlog up to the NEWEST
// queued keyframe (media segments are a dependent stream, so resuming anywhere
// else would append deltas whose references were dropped) and marks the track
// discontinuous: what it appends next cannot abut what it appended last, so the
// resume is healed as a discontinuity (see maybeHeal) rather than left as a
// hole in the buffered timeline. On iPhone this is the steady state of a long
// arming period: MMS parks a fed, paused element (docs/27 Decision 5) while the
// live stream keeps arriving. A single GOP longer than the bound (a WHIP/OBS
// GOP can reach ~2.2 s, docs/83 D7) is dropped whole, so the bound is hard.
export const MAX_QUEUED_SEGMENTS = 128;

// Pruning, on every MediaSource: MMS is supposed to evict on its own, but an
// iPhone was observed holding 21 s of 3360×2100 media (docs/27 "Pruning"),
// so it gets the same bound as classic MediaSource.
//
// The bound is BYTES, because memory is what it protects and a time window
// costs 10× more at 7 MP than at 720p. The presenter counts the video bytes it
// appends against each segment's timestamp; once they exceed the budget — or
// the buffered span exceeds the time ceiling, which keeps a near-static,
// low-bitrate stream bounded too — it removes everything before the newest
// keyframe at or before `playhead − PRUNE_BEHIND_S` (a paused element: below).
// Audio is cut at the same time so the tracks' intersection stays intact; its
// bytes are negligible (~16 KB/s) and not counted.
//
// Why so little history: nothing seeks behind the kept range. Native
// fullscreen seeks toward live (useFullscreen's entry seek, maybeCatchUp, the
// stall watchdog), and its few backward steps stay within the delivery mode's
// target lag of the end (liveEdge.ts). What IS needed behind the playhead is
// its own GOP: MSE's remove() also drops every frame depending on a removed one
// up to the next keyframe, so a cut inside the playhead's GOP would take the
// frames ahead of it too. Hence keyframe-aligned cuts, plus one second of
// margin to absorb a stale currentTime. A paused element is the armed one,
// whose stale playhead the entry seek replaces with a landing the mode's
// target lag behind the live edge, so it is pruned to the live edge minus that
// lag minus the margin: the GOP the entry seek lands in is always kept.
//
// 8 MiB is ~2.5 s at 7 MP / 25 Mbps and ~10 s at 1080p / 6 Mbps: past any
// reasonable GOP. At 7 MP it is less than Deep buffer's 3 s catch-up trigger,
// which is harmless: a playing element is cut relative to currentTime, never
// the edge, so the budget only ever removes history (and a keyframe-aligned
// cut can't touch the live edge's working set anyway — the budget is a soft
// bound over a single long GOP).
export const PRUNE_BUDGET_BYTES = 8 * 1024 * 1024;
export const PRUNE_CEILING_S = 30;
export const PRUNE_BEHIND_S = 1;
// Well under a frame (16.7 ms at 60 fps): absorbs `buffered` rounding only.
const LEDGER_TIME_EPSILON_S = 0.001;

// How long the audio SourceBuffer may exist without receiving a single sample
// before audio is given up on. Creating it up front is what makes the pair
// addable at all, but it has a cost: until audio appends, the element's
// buffered range — the INTERSECTION of the tracks — is empty, so video
// cannot play either. An audio path that never produces (no AAC encoder on this
// runtime, a transcoder error) must therefore be detected and dropped, or the
// presentation is worse than video-only: it is dead. Generous enough for a
// transcoder to warm up, short enough that nobody watches a blank fullscreen.
export const AUDIO_FIRST_SAMPLE_TIMEOUT_MS = 3000;

// HTMLMediaElement.HAVE_METADATA — the readiness `webkitEnterFullscreen`
// requires (useFullscreen tier 2 checks the same bar).
const HAVE_METADATA = 1;

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
  // The bound 'error' listener for this track's current SourceBuffer, kept so it
  // can be removed on detach.
  onError: (() => void) | null;
  // Whether an init segment has been appended to the CURRENT SourceBuffer. The
  // buffer existing is not the same thing: it is created up front (before any
  // bytes) so both tracks can be added before the first append, and a media
  // segment appended to a buffer that never saw its init is a decode error.
  initAppended: boolean;
  // The presenter's prune target already removed from THIS SourceBuffer; a
  // track busy when the target moved catches up at its next idle moment.
  prunedTo: number;
  // Set when the queue overflowed: the next keyframe this track appends opens a
  // new buffered range after a hole. Becomes `healPending` once that append is
  // issued, and the heal runs at its updateend (when `buffered` includes it).
  gap: boolean;
  healPending: boolean;
  // Where a heal decided this track's stale ranges end; removed before the next
  // append (one SourceBuffer operation in flight at a time).
  trimBefore: number | null;
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
    initAppended: false,
    onError: null,
    prunedTo: 0,
    gap: false,
    healPending: false,
    trimBefore: null,
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
  // The audio SourceBuffer's mime, known from the negotiated tier before any
  // audio bytes exist. It has to be known EARLY: an MSE implementation may
  // refuse addSourceBuffer once the first init segment has been parsed (Chromium
  // throws QuotaExceededError, "reached the limit of SourceBuffer objects"), so
  // both buffers are created together with the video one — never lazily when the
  // first audio sample shows up, which is always too late.
  private expectedAudioMime: string | null = null;
  // Sticky: once audio has been given up on, later audio segments are ignored for
  // this presentation's life. The worker muxer has no idea the presenter gave up
  // and keeps producing them, so without this a later init would resurrect the
  // track that just failed.
  private audioDropped = false;
  // A real timer, not a per-append tick: appends are what would normally drive
  // this, but the pathological case is precisely one where nothing is appending.
  private audioTimer: ReturnType<typeof setTimeout> | null = null;
  private get tracks(): TrackState[] {
    return [this.videoBuf, this.audioBuf];
  }

  private segmentsAppended = 0;
  private appendErrors = 0;
  private segmentsReceived = 0;
  private droppedNoInit = 0;
  private droppedOverflow = 0;
  private discontinuities = 0;
  // Set by an overflow, cleared by the heal that counts it: one resume is one
  // discontinuity however many tracks' updateends re-run maybeHeal.
  private healArmed = false;
  private lastError: string | null = null;
  // Per-MediaSource: whether duration = Infinity stuck (see declareLive).
  private liveDuration = false;
  // Per-MediaSource byte ledger of appended video media segments, oldest
  // first, and the time everything before which has been (or is being)
  // removed. See PRUNE_BUDGET_BYTES.
  private ledger: Array<{ timeS: number; bytes: number; keyframe: boolean }> = [];
  private ledgerBytes = 0;
  private pruneTo = 0;
  private prunedBytes = 0;
  // Sizes the catch-up seek's cushion (liveEdge.ts). Set by the screen from
  // its own state — the transport flag lives in the worker when one runs.
  private deliveryMode: ViewerDeliveryMode = 'live';

  private readonly onSourceOpen = () => {
    this.sourceOpen = true;
    this.declareLive();
    this.pump();
  };
  private readonly onUpdateEnd = () => {
    this.maybeHeal();
    this.maybeCatchUp();
    this.prune();
    this.pump();
  };
  // Compose the failure detail while it is still readable: `video.error` is the
  // only place WebKit says WHY (a rejected init segment reads
  // MEDIA_ERR_SRC_NOT_SUPPORTED, code 4), and `readyState: 'closed'` is how it
  // shows that a bad append took the whole MediaSource down rather than one
  // buffer.
  private recordError(track: Fmp4Track, what: string): void {
    this.appendErrors++;
    const media = this.video?.error;
    const detail = media ? `; media error code ${media.code}${media.message ? `: ${media.message}` : ''}` : '';
    const state = this.ms ? `; source ${this.ms.readyState}` : '';
    this.lastError = `${track}: ${what}${detail}${state}`;
  }

  // Per-track, because the two tracks are not equally load-bearing: a video
  // SourceBuffer error degrades the gate to pseudo-fullscreen, but an AUDIO one
  // must not take the video presentation down with it — audio is additive, and
  // on the AAC path it is the least-proven part of the pipeline.
  private readonly onTrackError = (t: TrackState) => () => {
    this.recordError(t.track, 'SourceBuffer error event');
    if (t.track === 'video') {
      this.failed = true;
      log.warn('MSE presentation: video SourceBuffer error; native fullscreen degraded');
      return;
    }
    log.warn('MSE presentation: audio SourceBuffer error; presenting video only');
    // Rebuild video-only: a broken audio buffer cannot simply be abandoned,
    // because `buffered` is the INTERSECTION of the tracks — a frozen audio range
    // would freeze the video presentation with it.
    this.dropAudioTrack(true);
  };
  private readonly onStartStreaming = () => this.pump();

  constructor(ctor: MediaSourceCtor | null = getMediaSourceCtor()) {
    this.ctor = ctor;
  }

  getStats(): MsePresenterStats {
    return {
      attached: this.video !== null,
      sourceOpen: this.sourceOpen,
      streaming: this.ms?.streaming ?? null,
      received: this.segmentsReceived,
      droppedNoInit: this.droppedNoInit,
      droppedOverflow: this.droppedOverflow,
      discontinuities: this.discontinuities,
      liveDuration: this.liveDuration,
      audioTrack: this.audioBuf.sb !== null,
      audioSegmentsAppended: this.audioBuf.appended,
      segmentsAppended: this.segmentsAppended,
      appendErrors: this.appendErrors,
      lastError: this.lastError,
      queued: this.videoBuf.queue.length + this.audioBuf.queue.length,
      failed: this.failed,
      bufferedBytes: this.ledgerBytes,
      prunedBytes: this.prunedBytes,
    };
  }

  // The viewer's delivery mode: how much live-edge cushion (and catch-up
  // slack) the playing presentation keeps. Read on every catch-up check, so a
  // change applies from the next append.
  setDeliveryMode(mode: ViewerDeliveryMode): void {
    this.deliveryMode = mode;
  }

  // Declare which audio mime this presentation will carry, as soon as the
  // tier is negotiated. Called before any audio
  // bytes exist — `addSourceBuffer` needs only the mime, while the init segment
  // needs the codec's setup bytes, and those arrive later on the AAC path.
  //
  // If the video SourceBuffer already exists, the window for adding another has
  // closed, so the whole MediaSource is rebuilt: video re-primes from its cached
  // init at the next keyframe (a ≤1-GOP hiccup, invisible while the armed element
  // is paused) and both buffers are created up front on the fresh source.
  setExpectedAudioMime(mime: string | null): void {
    if (this.audioDropped || this.expectedAudioMime === mime) return;
    this.expectedAudioMime = mime;
    if (!mime || this.audioBuf.sb) return;
    if (this.videoBuf.sb && this.video) {
      const video = this.video;
      this.detach();
      this.attach(video);
      return;
    }
    // No video SourceBuffer yet: the pair is created together in
    // ensureSourceBuffer.
    this.pump();
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
        if (t.onError) t.sb.removeEventListener('error', t.onError);
      }
      t.onError = null;
      t.sb = null;
      t.sbMime = null;
      t.initAppended = false;
      t.prunedTo = 0;
      // A fresh MediaSource has no old range for a hole to open after.
      t.gap = false;
      t.healPending = false;
      t.trimBefore = null;
    }
    this.ms = null;
    // A fresh MediaSource starts empty.
    this.ledger = [];
    this.ledgerBytes = 0;
    this.pruneTo = 0;
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
    this.segmentsReceived++;
    if (seg.track === 'audio' && this.audioDropped) return;
    const t = seg.track === 'audio' ? this.audioBuf : this.videoBuf;
    if (seg.kind === 'init') {
      t.cachedInit = { mime: seg.mime, data: seg.data };
      // The FIRST audio init is only *cached*, never queued: an audio track whose
      // buffer holds an init and no samples still contributes an empty range to
      // the element's buffered intersection, so pump() primes from the cache when
      // the first sample is ready to follow it. A later init is a real parameter
      // change and must be appended in order.
      if (t.track === 'audio' && !t.initAppended) return;
    } else if (!t.cachedInit) {
      // Media before any init cannot ever decode; don't queue it. Counted,
      // because the muxer emits its init exactly once per session: a sink that
      // was not registered for that one segment drops every segment after it,
      // for good, and this counter is the only trace that would leave.
      this.droppedNoInit++;
      return;
    }
    t.queue.push(seg);
    if (t.queue.length > MAX_QUEUED_SEGMENTS) this.discardBacklog(t);
    this.pump();
  }

  // Queue overflow (see MAX_QUEUED_SEGMENTS): keep only what starts at the newest
  // queued keyframe — everything older is a backlog the playhead will be moved
  // past anyway — and mark the track discontinuous so the resume is healed.
  private discardBacklog(t: TrackState): void {
    let cut = t.queue.length;
    for (let i = t.queue.length - 1; i >= 0; i--) {
      const s = t.queue[i];
      if (s.kind === 'media' && s.keyframe) {
        cut = i;
        break;
      }
    }
    // Nothing but inits ahead of the newest keyframe (it heads the queue, or
    // sits behind a re-queued init): the GOP it starts is itself longer than
    // the bound (a WHIP/OBS GOP can reach ~2.2 s, docs/83 D7). Drop it too and
    // resume at the next keyframe, or the queue would grow with the GOP.
    if (!t.queue.slice(0, cut).some((s) => s.kind === 'media')) cut = t.queue.length;
    const dropped = t.queue.slice(0, cut);
    const kept = t.queue.slice(cut);
    // The init governing the kept head may be among the dropped (a parameter
    // change inside the backlog): put the newest dropped one back in front.
    let lastInit: PresenterSegment | null = null;
    for (const s of dropped) if (s.kind === 'init') lastInit = s;
    if (lastInit) kept.unshift(lastInit);
    t.queue = kept;
    this.droppedOverflow += dropped.filter((s) => s.kind === 'media').length;
    t.needKeyframe = true;
    t.gap = true;
    this.healArmed = true;
  }

  dispose(): void {
    this.clearAudioWatchdog();
    this.detach();
    for (const t of this.tracks) {
      t.cachedInit = null;
      t.queue = [];
    }
  }

  // A live presentation must say so: `duration = Infinity`.
  // Left unset, MSE's coded-frame processing raises duration to the newest
  // appended end timestamp, which costs two things on the native player —
  //   * the LIVE badge, replaced by a finite scrub bar over a growing duration;
  //   * the distinction between "the playhead reached the buffered end" and
  //     "the media resource ended". WebKit resolves that ambiguity by pausing
  //     and firing `ended` (where Chromium stalls and resumes on more data), so
  //     every underrun leaves a dead player needing a manual tap.
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
  // parked while MMS says streaming is off — but never before the element has
  // media (see canPark).
  private pump(): void {
    if (this.failed || !this.ms || !this.sourceOpen) return;
    // MMS pacing: `streaming` false parks the queue until startstreaming. A
    // pending heal trim still runs: it removes, never appends, and it is what
    // takes the element back to one buffered range.
    if (this.ms.streaming === false && this.canPark()) {
      for (const t of this.tracks) this.trim(t);
      return;
    }
    // Each track appends independently — a video append in flight must not hold
    // audio back (and vice versa), or the two buffers drift by a whole append
    // round-trip each frame.
    for (const t of this.tracks) this.pumpTrack(t);
  }

  private pumpTrack(t: TrackState): void {
    if (t.sb?.updating) return;
    if (this.trim(t)) return;

    while (t.queue.length > 0) {
      const seg = t.queue[0];
      if (seg.kind === 'media' && t.needKeyframe && !seg.keyframe) {
        t.queue.shift();
        continue;
      }
      if (seg.kind === 'media' && !t.initAppended && t.cachedInit) {
        // This buffer has never taken an init segment: either it was created up
        // front for the track pair, or its init fell off the queue (overflow), or
        // a re-attach built a fresh MediaSource. Prime from the cache, then
        // reprocess this segment.
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
      if (seg.kind === 'media' && seg.keyframe) {
        t.needKeyframe = false;
        if (t.gap) {
          t.gap = false;
          t.healPending = true;
        }
      }
      try {
        t.sb!.appendBuffer(seg.data as BufferSource);
        if (seg.kind === 'init') t.initAppended = true;
        else if (t.track === 'video') {
          this.ledger.push({ timeS: seg.timeS, bytes: seg.data.byteLength, keyframe: seg.keyframe });
          this.ledgerBytes += seg.data.byteLength;
        }
        t.appended++;
        this.segmentsAppended++;
      } catch (e) {
        // QuotaExceeded and friends: drop this segment, resync at the next
        // keyframe. The relay-side drops-over-stalls philosophy, locally.
        this.recordError(t.track, `appendBuffer threw ${errText(e)}`);
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
        // Codec change mid-stream (codec pin / broadcaster swap): changeType
        // where it exists; without it the surface degrades to pseudo.
        try {
          if (typeof t.sb.changeType !== 'function') throw new Error('changeType unavailable');
          t.sb.changeType(mime);
          t.sbMime = mime;
        } catch (e) {
          this.recordError(t.track, `changeType to ${mime} failed: ${errText(e)}`);
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
      t.onError = this.onTrackError(t);
      sb.addEventListener('error', t.onError);
      t.sb = sb;
      t.sbMime = mime;
      // The audio buffer must exist before this video init segment is appended
      // (see setExpectedAudioMime). Its failure is non-fatal: video carries on.
      if (t.track === 'video' && this.expectedAudioMime && !this.audioBuf.sb) {
        this.ensureSourceBuffer(this.audioBuf, this.expectedAudioMime);
      }
      if (t.track === 'audio') this.armAudioWatchdog();
      return true;
    } catch (e) {
      // The trial-addSourceBuffer half of the probe, failing for real: mark
      // failed so the gate reports pseudo — but only for video.
      // Audio is additive: a refused audio SourceBuffer (the plausible
      // Opus-in-MP4 outcome on iOS) leaves the video presentation intact and
      // drops the audio track for good.
      this.recordError(t.track, `addSourceBuffer(${mime}) failed: ${errText(e)}`);
      if (t.track === 'video') this.failed = true;
      else this.dropAudioTrack();
      log.warn(`MSE presentation: ${t.track} addSourceBuffer failed:`, e);
      return false;
    }
  }

  // Give up on the audio track without disturbing video: nothing further is
  // queued (pushSegment drops audio once dropped), and the inline AudioWorklet
  // sink keeps playing the audio either way. `rebuild` is for the case where an
  // audio SourceBuffer already exists and has data: since the element's
  // `buffered` is the intersection of the tracks, leaving a dead audio range
  // attached would freeze video, so the MediaSource is rebuilt video-only (video
  // re-primes from its cached init at the next keyframe). Where no audio buffer
  // was ever created — a refused addSourceBuffer — clearing state is enough.
  private dropAudioTrack(rebuild = false): void {
    this.audioDropped = true;
    this.expectedAudioMime = null;
    this.clearAudioWatchdog();
    this.audioBuf.cachedInit = null;
    this.audioBuf.queue = [];
    this.audioBuf.needKeyframe = false;
    const video = this.video;
    if (rebuild && video) {
      this.detach();
      this.attach(video);
    }
  }

  // An audio buffer that never receives a sample holds the whole presentation
  // hostage (see AUDIO_FIRST_SAMPLE_TIMEOUT_MS): `buffered` is the intersection of
  // the tracks, so video cannot play past an empty audio range.
  private armAudioWatchdog(): void {
    this.clearAudioWatchdog();
    this.audioTimer = setTimeout(() => {
      this.audioTimer = null;
      if (this.audioBuf.appended > 0) return;
      // Only act on the OBSERVABLE symptom, never on the proxy. An audio buffer
      // that never received even its init segment contributes no active track, so
      // some implementations play video regardless — and rebuilding there would
      // destroy a working presentation for nothing. Chromium's demuxer, by
      // contrast, waits for every added buffer's init segment before it reports
      // metadata at all, and that is what being held hostage looks like: video
      // appended, yet the element has no buffered range and no metadata.
      const held = this.videoBuf.appended > 0 && !this.hasMedia();
      if (!held) return;
      log.warn('MSE presentation: audio never produced and video is blocked on it;'
        + ' presenting video only');
      this.dropAudioTrack(true);
    }, AUDIO_FIRST_SAMPLE_TIMEOUT_MS);
  }

  // Whether MMS's `streaming === false` may park the queue.
  // Parking is right for a fed element — it is how MMS saves power and bounds
  // its buffer — but it is a DEADLOCK before the element has media: the system
  // asks for data when the element needs more, and an element with no init
  // segment has nothing to need. Left unguarded, an MMS that opens with
  // `streaming` already false never takes a single byte, so the element sits at
  // readyState 0 forever and `useFullscreen` tier 2 (which requires
  // HAVE_METADATA) falls to CSS pseudo-fullscreen for the whole session — with
  // zero appends and zero errors to show for it.
  //
  // So priming appends (the init segment, its SourceBuffer, and the first
  // keyframes behind it) always go through; parking resumes once the element
  // reports playable media. Deliberately the OPPOSITE default to hasMedia() on
  // an unreadable `buffered`: there an unknown must not trigger a rebuild, here
  // it must not park the priming that breaks the deadlock — a few appends the
  // system did not ask for cost buffer, never the whole surface.
  private canPark(): boolean {
    return this.elementMediaState() === true;
  }

  // Whether the element has anything to play. `readyState >= HAVE_METADATA` is the
  // spec-level "the demuxer initialized"; the buffered check covers the same thing
  // for implementations that report metadata earlier.
  private hasMedia(): boolean {
    // Unreadable (null) counts as "has media": never rebuild on a diagnostics
    // failure. canPark() reads the same measurement the other way round.
    return this.elementMediaState() !== false;
  }

  // One reading of "does the element hold playable media", three-valued so the
  // two callers can disagree about what an unreadable answer means: true/false
  // as measured, null when the element cannot be asked (no element at all is a
  // definite false — there is nothing holding media).
  private elementMediaState(): boolean | null {
    const video = this.video;
    if (!video) return false;
    try {
      return video.readyState >= HAVE_METADATA && video.buffered.length > 0;
    } catch {
      return null;
    }
  }

  private clearAudioWatchdog(): void {
    if (this.audioTimer !== null) clearTimeout(this.audioTimer);
    this.audioTimer = null;
  }

  // Issue a heal's pending remove; true if one is now in flight.
  private trim(t: TrackState): boolean {
    if (t.trimBefore === null || !t.sb || t.sb.updating) return false;
    const end = t.trimBefore;
    t.trimBefore = null;
    try {
      t.sb.remove(0, end);
      return true;
    } catch (e) {
      // Best-effort: MMS evicts eventually, and the playhead is already past it.
      log.warn(`MSE presentation: ${t.track} discontinuity trim failed:`, e);
      return false;
    }
  }

  // Resume-after-overflow is a DISCONTINUITY (BUGS.md, docs/27 finding 3 item 5):
  // the segments shed while parked were never appended, so the first keyframe
  // after them opens a new range and the old one ends in a hole the native
  // player stalls at. Once that keyframe is in `buffered`, move the playhead to
  // the live edge of the newest playable range and remove each track's older
  // ranges — per track, at that track's own newest range start, because a video
  // trim that ended mid-GOP would take the rest of the GOP with it (MSE's remove
  // extends to the next random access point). One seek per resume; an armed,
  // paused element is otherwise never seeked (Decision 5).
  private maybeHeal(): void {
    let due = false;
    for (const t of this.tracks) {
      if (t.healPending && t.sb && !t.sb.updating) {
        t.healPending = false;
        due = true;
      }
    }
    const video = this.video;
    if (!due || !video) return;
    try {
      let healed = false;
      for (const t of this.tracks) {
        const b = t.sb?.buffered;
        if (!b || b.length < 2) continue;
        t.trimBefore = b.start(b.length - 1);
        if (t.track === 'video') this.retireLedgerBefore(t.trimBefore);
        healed = true;
      }
      const eb = video.buffered;
      if (eb.length > 0) {
        const start = eb.start(eb.length - 1);
        if (eb.length > 1 || video.currentTime < start) {
          // The same landing as the entry seek: the mode's target lag behind
          // the end (liveEdge.ts), which the paused prune keep range covers.
          const lag = liveEdgePolicy(this.deliveryMode).targetLagS;
          video.currentTime = Math.max(start, eb.end(eb.length - 1) - lag);
          healed = true;
        }
      }
      if (healed && this.healArmed) {
        this.healArmed = false;
        this.discontinuities++;
      }
    } catch {
      // buffered/seek quirks must never break the append loop
    }
  }

  // While the (fullscreen) video plays, a playhead that fell behind the
  // buffered end past the mode's catch-up trigger jumps back to the mode's
  // target lag (liveEdge.ts, shared with useFullscreen's entry seek). Paused
  // (armed, inline) video never seeks: it stays loaded-but-paused until the
  // in-gesture play.
  //
  // It lands AT the target lag, never nearer the end: appends arrive at 1×,
  // so the cushion this seek leaves is the session's whole jitter budget. That
  // matters most during a DVR catch-up burst — the one time real cushion
  // arrives for free — which used to be thrown away by a seek to `end − 0.1`
  // (docs/27 finding 3). Inside the trigger band nothing seeks, so a burst
  // that stays under it only adds cushion.
  private maybeCatchUp(): void {
    const video = this.video;
    if (!video || video.paused) return;
    try {
      const { targetLagS, catchUpLagS } = liveEdgePolicy(this.deliveryMode);
      const to = liveEdgeSeekTarget(video.buffered, video.currentTime, targetLagS, catchUpLagS);
      if (to !== null) video.currentTime = to;
    } catch {
      // buffered/seek quirks must never break the append loop
    }
  }

  // Byte-budgeted eviction (see PRUNE_BUDGET_BYTES). Runs on every updateend:
  // it moves the shared cut target when the budget or the ceiling is crossed,
  // then applies the target to each idle SourceBuffer that has not taken it.
  private prune(): void {
    const video = this.video;
    if (!video) return;
    try {
      const b = video.buffered;
      if (b.length > 0) {
        const end = b.end(b.length - 1);
        const span = end - b.start(0);
        if (this.ledgerBytes > PRUNE_BUDGET_BYTES || span > PRUNE_CEILING_S) {
          // A paused element is the armed one, and the entry seek replaces its
          // playhead with the live edge — so that is what history is kept for.
          // The entry seek then lands the mode's target lag behind that edge
          // (liveEdge.ts, up to 1.5 s in Deep buffer), so keep that much too.
          const behind = video.paused
            ? liveEdgePolicy(this.deliveryMode).targetLagS + PRUNE_BEHIND_S
            : PRUNE_BEHIND_S;
          const playhead = video.paused ? end : Math.min(video.currentTime, end);
          this.advancePruneTarget(playhead - behind);
        }
      }
    } catch {
      // buffered quirks: pruning is best-effort
    }
    for (const t of this.tracks) {
      if (!t.sb || t.sb.updating || t.prunedTo >= this.pruneTo) continue;
      try {
        t.sb.remove(0, this.pruneTo);
      } catch {
        // best-effort; a refused remove is not retried at this target
      }
      t.prunedTo = this.pruneTo;
    }
  }

  // A heal's trim removes the stale video ranges outside the prune target; the
  // byte ledger counts what is buffered, so it forgets them too. `timeS` is the
  // tfdt while `buffered.start()` is rounded to the UA's timescale, hence the
  // tolerance: the resume keyframe's own entry must survive.
  private retireLedgerBefore(timeS: number): void {
    let n = 0;
    while (n < this.ledger.length && this.ledger[n].timeS < timeS - LEDGER_TIME_EPSILON_S) n++;
    for (const e of this.ledger.splice(0, n)) {
      this.ledgerBytes -= e.bytes;
      this.prunedBytes += e.bytes;
    }
  }

  // Move the cut to the newest keyframe at or before `limit`, retiring the
  // ledger entries before it. Never moves backwards, and never inside a GOP.
  private advancePruneTarget(limit: number): void {
    let cut = -1;
    for (let i = this.ledger.length - 1; i >= 0; i--) {
      const e = this.ledger[i];
      if (e.keyframe && e.timeS <= limit) {
        cut = i;
        break;
      }
    }
    if (cut <= 0) return; // nothing whole behind the playhead's GOP
    const target = this.ledger[cut].timeS;
    if (target <= this.pruneTo) return;
    for (const e of this.ledger.splice(0, cut)) {
      this.ledgerBytes -= e.bytes;
      this.prunedBytes += e.bytes;
    }
    this.pruneTo = target;
  }
}
