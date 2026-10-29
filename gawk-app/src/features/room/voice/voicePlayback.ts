// Room voice playback (R79, docs/80 D22, OD26): the browser mixes. One Opus
// AudioDecoder and one jitter buffer + worklet sink (the viewer's AudioSink,
// on the room's shared AudioContext through the voice bus) per speaker
// session, created on that speaker's first frame and released after
// SPEAKER_IDLE_RELEASE_MS of silence. Decoder count is bounded
// (MAX_VOICE_SPEAKERS): a new speaker past the cap evicts whoever was heard
// least recently. Usually two or three people talk at once, so the cap is
// slack, not a limit anyone meets.
//
// A talk spurt is its own timeline: a speaker's frames resume after a pause
// (or after an end flag) with a seq that jumped by the pause, or restarted
// at 0 (Mumble resets it per transmission), which the jitter buffer would
// read as a hole to fill with silence or as late. So the sink's timeline is
// re-anchored at the start of every spurt — re-anchored, never flushed: what
// the worklet still holds is the end of the last spurt. A Mumble client
// whose voice activation flaps ends and restarts its transmission inside a
// sentence, often 0–50 ms apart; flushing there cut the end of each word
// and then played silence until the cushion refilled (docs/80 §12.7).
//
// A transmission starts on a fresh decoder, as Mumble plays it. Mumble's
// client resets its Opus encoder whenever a transmission starts (AudioInput:
// bResetEncoder → OPUS_RESET_STATE), and our capture does the same; its
// listeners decode each transmission on a fresh AudioOutputSpeech. A reset
// encoder's first packets decoded on the last transmission's state come out
// garbled: −0.9 dB SNR over the first 20 ms against 22.6 dB fresh (docs/80
// §12.8). The old codec drains first, so the end of the last word still
// plays, and only then is the sink re-anchored: a re-anchor at the new
// transmission's first *packet* raced the old codec's last outputs, which
// then carried the old timeline into the new one. Mumble also fades a
// transmission's first frame in and its last (end-flagged) frame out; so do
// we, with its sin(i·π/2N) window.
//
// The cushion is per speaker, sized by how late that speaker's packets run
// against their own seq clock (ArrivalJitter). A web speaker's 10 ms packets
// arrive within ~15 ms of it and keep the 40–60 ms floor; a native Linux
// client measured on the production server sent its packets in clumps up to
// 126 ms late, which ran a fixed 60 ms cushion dry — a pop every ~2 s, for
// that speaker only.
//
// Speaking indicators (speaking.ts) are fed from the same frames.
//
// Hear yourself (R80, docs/81 D16) is one more speaker: your own packets,
// after the gate, through the same decoder, jitter buffer and voice bus, so
// you hear what the others hear. It is never in the speaking set or the cap,
// and nothing of it is kept.

import { log } from '../../../lib/logger';
import type { AudioBufferProfile, AudioChunk } from '../../../transport/audio-buffer';
import type { VoiceFrameHeader } from '../../../transport/wire';
import { AudioSink, type AudioOutput } from '../../viewer/audioSink';
import { SpeakingTracker } from './speaking';
import { VOICE_SAMPLE_RATE, VOICE_SEQ_UNIT_US } from './voiceCapture';
import { MAX_SPEAKER_VOLUME } from './voicePrefs';

export const SPEAKER_IDLE_RELEASE_MS = 10_000;
export const MAX_VOICE_SPEAKERS = 8;
// A pause longer than this starts a new spurt even without an end flag (the
// end-flagged terminator is a datagram too, and can be lost).
export const SPURT_GAP_MS = 200;
// Voice is not lip-synced to anything: the shallow live-edge envelope.
export const VOICE_AUDIO_PROFILE: AudioBufferProfile = { minMs: 40, maxMs: 150, seedMs: 60 };
const SWEEP_INTERVAL_MS = 1000;
// How often a speaker's jitter reaches their sink between re-anchors: the
// buffer slews toward it anyway, so 100 frames/s of updates buy nothing.
const TARGET_UPDATE_INTERVAL_MS = 250;

// Where a frame sits in its transmission, for Mumble's fades.
export interface FrameMeta {
  first: boolean;
  last: boolean;
}

export interface SpeakerDecoder {
  decode(payload: Uint8Array, timestampUs: number, meta: FrameMeta): void;
  // A new transmission: everything decoded so far comes out, then
  // onBoundary runs, then the next frames decode on a fresh codec.
  restart(onBoundary: () => void): void;
  close(): void;
}

export interface SpeakerSink {
  push(chunk: AudioChunk): void;
  // A new spurt: forget the timeline, keep the queue (AudioSink.reanchor).
  reanchor(): void;
  setVolume(volume: number): void;
  // The adaptive cushion (AudioJitterBuffer.updateTarget): jitter in ms.
  updateTarget(jitterMs: number | null, nowMs: number): void;
  dispose(): void;
}

export interface VoicePlaybackDeps {
  createDecoder: (onChunk: (chunk: AudioChunk, meta: FrameMeta) => void, onError: (e: Error) => void) => SpeakerDecoder;
  createSink: () => SpeakerSink | null;
  now: () => number;
}

export interface VoicePlaybackCallbacks {
  // The speaking set changed (Mumble session IDs, sorted).
  onSpeaking: (sessions: number[]) => void;
}

export interface VoicePlaybackLike {
  handleFrame(header: VoiceFrameHeader, payload: Uint8Array): void;
  // One of your own packets (or a bare terminator), for Hear yourself.
  handleSelfFrame(header: Pick<VoiceFrameHeader, 'end' | 'seq'>, payload: Uint8Array): void;
  stopSelf(): void;
  setSpeakerVolume(session: number, volume: number): void;
  dispose(): void;
}

// The self speaker's key; Mumble sessions are unsigned.
const SELF = -1;

// How long a speaker's measured jitter takes to halve once it is no longer
// being measured.
export const JITTER_HALF_LIFE_MS = 30_000;
// Speakers whose jitter is remembered after their decoder is released.
const MAX_REMEMBERED_SPEAKERS = 64;

// How late a speaker's packets arrive against their own seq clock: per spurt
// (a seq jumps or resets across a pause), the spread of arrival − seq time
// over the last WINDOW_MS. Max, not a percentile: the latest packet is the
// one that runs the cushion dry.
//
// The peak is held past the window, halving every JITTER_HALF_LIFE_MS: a
// speaker's bursts come back with their next sentence, and a spurt that
// starts on the seed cushion pops before the window has learned anything.
// Mumble's client remembers a user the same way (AudioOutputSpeech's
// per-user fAverageAvailable).
export class ArrivalJitter {
  static readonly WINDOW_MS = 5000;
  private samples: { atMs: number; offsetMs: number; spurt: number }[] = [];
  private spurt = 0;
  private peakMs = 0;
  private peakAtMs = 0;

  frame(seq: number, nowMs: number, newSpurt: boolean): void {
    if (newSpurt) this.spurt++;
    this.samples.push({ atMs: nowMs, offsetMs: nowMs - (seq * VOICE_SEQ_UNIT_US) / 1000, spurt: this.spurt });
  }

  jitterMs(nowMs: number): number {
    const from = nowMs - ArrivalJitter.WINDOW_MS;
    let drop = 0;
    while (drop < this.samples.length && this.samples[drop].atMs < from) drop++;
    if (drop > 0) this.samples.splice(0, drop);
    let worst = 0;
    let spurt = -1;
    let min = 0;
    let max = 0;
    for (const s of this.samples) {
      if (s.spurt !== spurt) {
        worst = Math.max(worst, max - min);
        spurt = s.spurt;
        min = max = s.offsetMs;
      } else {
        min = Math.min(min, s.offsetMs);
        max = Math.max(max, s.offsetMs);
      }
    }
    const measured = Math.max(worst, max - min);
    const held = this.peakMs * 0.5 ** (Math.max(0, nowMs - this.peakAtMs) / JITTER_HALF_LIFE_MS);
    if (measured >= held) {
      this.peakMs = measured;
      this.peakAtMs = nowMs;
      return measured;
    }
    return held;
  }
}

interface Speaker {
  session: number;
  decoder: SpeakerDecoder | null;
  sink: SpeakerSink | null;
  lastFrameAtMs: number;
  // The spurt ended (end flag): the next frame re-anchors.
  ended: boolean;
  jitter: ArrivalJitter;
  lastTargetAtMs: number;
}

export function voicePlaybackSupported(): boolean {
  return (
    typeof WebTransport === 'function' &&
    typeof AudioDecoder === 'function' &&
    typeof AudioContext !== 'undefined' &&
    typeof AudioWorkletNode !== 'undefined'
  );
}

// Mumble's fade (AudioOutputSpeech fFadeIn / fFadeOut): sin(i·π/2N) across
// one frame, in place, on every channel.
export function applyFade(chunk: AudioChunk, kind: 'in' | 'out'): void {
  const n = chunk.frameCount;
  const mul = Math.PI / (2 * n);
  for (const ch of chunk.channels) {
    for (let i = 0; i < n; i++) ch[i] *= Math.sin((kind === 'in' ? i : n - 1 - i) * mul);
  }
}

// The real decoder: Opus, 48 kHz mono (what Mumble sends), planar PCM out.
// One AudioDecoder per transmission. A restart queues behind the frames
// already given: the old codec flushes (all its outputs out, in order), the
// boundary runs, and a fresh one takes the frames that arrived meanwhile.
export function createOpusDecoder(
  onChunk: (chunk: AudioChunk, meta: FrameMeta) => void,
  onError: (e: Error) => void,
): SpeakerDecoder {
  let closed = false;
  // Outputs come one per frame, in order, so each output takes the oldest meta.
  const metas: FrameMeta[] = [];
  const open = (): AudioDecoder => {
    const d = new AudioDecoder({
      output: (data) => {
        const meta = metas.shift() ?? { first: false, last: false };
        try {
          const channels: Float32Array[] = [];
          for (let i = 0; i < data.numberOfChannels; i++) {
            const plane = new Float32Array(data.numberOfFrames);
            data.copyTo(plane, { planeIndex: i, format: 'f32-planar' });
            channels.push(plane);
          }
          onChunk({ timestampUs: data.timestamp, sampleRate: data.sampleRate, channels, frameCount: data.numberOfFrames }, meta);
        } catch (e) {
          log.warn('voice copyTo failed; dropping a frame:', e);
        } finally {
          data.close();
        }
      },
      error: (e) => onError(e instanceof Error ? e : new Error(String(e))),
    });
    d.configure({ codec: 'opus', sampleRate: VOICE_SAMPLE_RATE, numberOfChannels: 1 });
    return d;
  };
  let decoder = open();
  // Frames and restarts waiting on a draining codec, in arrival order.
  type Step = { frame: [Uint8Array, number, FrameMeta] } | { boundary: () => void };
  const waiting: Step[] = [];
  let draining = false;
  const submit = (payload: Uint8Array, timestampUs: number, meta: FrameMeta) => {
    metas.push(meta);
    decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: timestampUs, data: payload }));
  };
  const pump = (): void => {
    while (!draining && !closed && waiting.length > 0) {
      const step = waiting.shift()!;
      if ('frame' in step) {
        submit(...step.frame);
        continue;
      }
      draining = true;
      const old = decoder;
      const done = () => {
        if (closed) return;
        if (old.state !== 'closed') old.close();
        metas.length = 0;
        step.boundary();
        decoder = open();
        draining = false;
        pump();
      };
      old.flush().then(done, (e: unknown) => {
        // A codec that failed or closed under the flush has nothing left to
        // give; the next transmission still gets a fresh one.
        if (!closed) log.warn('voice decoder flush failed; restarting anyway:', e);
        done();
      });
    }
  };
  return {
    decode: (payload, timestampUs, meta) => {
      if (draining || waiting.length > 0) {
        waiting.push({ frame: [payload, timestampUs, meta] });
        return;
      }
      submit(payload, timestampUs, meta);
    },
    restart: (onBoundary) => {
      waiting.push({ boundary: onBoundary });
      pump();
    },
    close: () => {
      closed = true;
      waiting.length = 0;
      if (decoder.state !== 'closed') decoder.close();
    },
  };
}

export function realPlaybackDeps(output: () => AudioOutput | null): VoicePlaybackDeps {
  return {
    createDecoder: createOpusDecoder,
    createSink: () => {
      const out = output();
      if (!out) return null;
      const sink = new AudioSink({}, VOICE_AUDIO_PROFILE, { output: out, maxVolume: MAX_SPEAKER_VOLUME });
      void sink.start(VOICE_SAMPLE_RATE).catch(() => {
        // logged by the sink; this speaker is silent, the rest play
      });
      return sink;
    },
    now: () => performance.now(),
  };
}

function newSpeaker(session: number, now: number, jitter: ArrivalJitter): Speaker {
  // lastTargetAtMs < 0: no frame yet, so the first one starts a spurt
  // without a re-anchor (the sink is fresh).
  return { session, decoder: null, sink: null, lastFrameAtMs: now, ended: false, jitter, lastTargetAtMs: -1 };
}

export class VoicePlayback implements VoicePlaybackLike {
  private readonly deps: VoicePlaybackDeps;
  private readonly cb: VoicePlaybackCallbacks;
  private readonly volumeFor: (session: number) => number;
  private readonly speakers = new Map<number, Speaker>();
  private self: Speaker | null = null;
  // Each speaker's jitter, kept past their release (docs/80 D22's 10 s idle).
  private readonly jitters = new Map<number, ArrivalJitter>();
  private readonly speaking = new SpeakingTracker();
  private speakingTimer: ReturnType<typeof setTimeout> | null = null;
  private sweepTimer: ReturnType<typeof setInterval> | null = null;
  private disposed = false;

  constructor(deps: VoicePlaybackDeps, cb: VoicePlaybackCallbacks, volumeFor: (session: number) => number) {
    this.deps = deps;
    this.cb = cb;
    this.volumeFor = volumeFor;
    this.sweepTimer = setInterval(() => this.sweep(), SWEEP_INTERVAL_MS);
  }

  get speakerCount(): number {
    return this.speakers.size;
  }

  handleFrame(header: VoiceFrameHeader, payload: Uint8Array): void {
    if (this.disposed) return;
    const now = this.deps.now();
    const session = header.speaker;
    if (this.speaking.frame(session, header.end, now)) this.emitSpeaking();
    this.armSpeakingTimer();

    let sp = this.speakers.get(session);
    if (!sp && !(header.end && payload.length === 0)) sp = this.addSpeaker(session, now);
    if (sp) this.feed(sp, header, payload, now);
  }

  handleSelfFrame(header: Pick<VoiceFrameHeader, 'end' | 'seq'>, payload: Uint8Array): void {
    if (this.disposed) return;
    const now = this.deps.now();
    if (!this.self && !(header.end && payload.length === 0)) {
      this.self = newSpeaker(SELF, now, this.jitterFor(SELF));
      this.startParts(this.self, 1);
    }
    if (this.self) this.feed(this.self, header, payload, now);
  }

  stopSelf(): void {
    const sp = this.self;
    this.self = null;
    if (sp) this.closeParts(sp);
  }

  private feed(sp: Speaker, header: Pick<VoiceFrameHeader, 'end' | 'seq'>, payload: Uint8Array, now: number): void {
    if (header.end && payload.length === 0) {
      // A bare terminator: the spurt is over; nothing to decode.
      sp.ended = true;
      sp.lastFrameAtMs = now;
      return;
    }
    const newSpurt = sp.ended || now - sp.lastFrameAtMs > SPURT_GAP_MS || sp.lastTargetAtMs < 0;
    sp.jitter.frame(header.seq, now, newSpurt);
    if (newSpurt && sp.lastTargetAtMs >= 0) {
      // Re-anchor at the boundary in the decoder's output, not here: the
      // last transmission's final chunks are still to come out of it.
      const sink = sp.sink;
      sp.decoder?.restart(() => sink?.reanchor());
    }
    // Before the spurt's first chunks: they are held against this speaker's
    // cushion, not a stale one.
    if (newSpurt || now - sp.lastTargetAtMs >= TARGET_UPDATE_INTERVAL_MS) {
      sp.sink?.updateTarget(sp.jitter.jitterMs(now), now);
      sp.lastTargetAtMs = now;
    }
    sp.ended = header.end;
    sp.lastFrameAtMs = now;
    if (!sp.decoder) return;
    try {
      sp.decoder.decode(payload, header.seq * VOICE_SEQ_UNIT_US, { first: newSpurt, last: header.end });
    } catch (e) {
      log.warn(`voice decode failed for speaker ${sp.session}; releasing it:`, e);
      this.release(sp);
    }
  }

  setSpeakerVolume(session: number, volume: number): void {
    this.speakers.get(session)?.sink?.setVolume(volume);
  }

  // Releases every speaker idle past SPEAKER_IDLE_RELEASE_MS. Run on an
  // interval; exposed for the tests.
  sweep(): void {
    const now = this.deps.now();
    for (const sp of [...this.speakers.values()]) {
      if (now - sp.lastFrameAtMs >= SPEAKER_IDLE_RELEASE_MS) this.release(sp);
    }
    if (this.self && now - this.self.lastFrameAtMs >= SPEAKER_IDLE_RELEASE_MS) this.stopSelf();
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    if (this.sweepTimer !== null) clearInterval(this.sweepTimer);
    this.sweepTimer = null;
    if (this.speakingTimer !== null) clearTimeout(this.speakingTimer);
    this.speakingTimer = null;
    for (const sp of [...this.speakers.values()]) this.release(sp);
    this.stopSelf();
    this.jitters.clear();
    this.speaking.clear();
  }

  private addSpeaker(session: number, now: number): Speaker {
    if (this.speakers.size >= MAX_VOICE_SPEAKERS) {
      let oldest: Speaker | null = null;
      for (const s of this.speakers.values()) if (!oldest || s.lastFrameAtMs < oldest.lastFrameAtMs) oldest = s;
      if (oldest) this.release(oldest);
    }
    const sp = newSpeaker(session, now, this.jitterFor(session));
    this.speakers.set(session, sp);
    this.startParts(sp, this.volumeFor(session));
    return sp;
  }

  private jitterFor(session: number): ArrivalJitter {
    let j = this.jitters.get(session);
    if (j) {
      // Most recently heard last, so the first key is the one to forget.
      this.jitters.delete(session);
    } else {
      j = new ArrivalJitter();
      if (this.jitters.size >= MAX_REMEMBERED_SPEAKERS) this.jitters.delete(this.jitters.keys().next().value!);
    }
    this.jitters.set(session, j);
    return j;
  }

  private startParts(sp: Speaker, volume: number): void {
    try {
      sp.sink = this.deps.createSink();
      sp.sink?.setVolume(volume);
      const sink = sp.sink;
      sp.decoder = this.deps.createDecoder(
        (chunk, meta) => {
          if (meta.first) applyFade(chunk, 'in');
          if (meta.last) applyFade(chunk, 'out');
          sink?.push(chunk);
        },
        (e) => {
          log.warn(`voice decoder for speaker ${sp.session} failed:`, e);
          this.release(sp);
        },
      );
    } catch (e) {
      log.warn(`voice playback for speaker ${sp.session} could not start:`, e);
      this.closeParts(sp);
    }
  }

  private release(sp: Speaker): void {
    if (sp === this.self) this.self = null;
    else if (this.speakers.get(sp.session) === sp) this.speakers.delete(sp.session);
    this.closeParts(sp);
  }

  private closeParts(sp: Speaker): void {
    try {
      sp.decoder?.close();
    } catch {
      // already closed
    }
    sp.decoder = null;
    try {
      sp.sink?.dispose();
    } catch {
      // already gone
    }
    sp.sink = null;
  }

  private armSpeakingTimer(): void {
    if (this.speakingTimer !== null) clearTimeout(this.speakingTimer);
    this.speakingTimer = null;
    const due = this.speaking.nextExpiryMs();
    if (due === null || this.disposed) return;
    this.speakingTimer = setTimeout(
      () => {
        this.speakingTimer = null;
        if (this.speaking.expire(this.deps.now())) this.emitSpeaking();
        this.armSpeakingTimer();
      },
      Math.max(0, due - this.deps.now()),
    );
  }

  private emitSpeaking(): void {
    this.cb.onSpeaking(this.speaking.speaking());
  }
}
