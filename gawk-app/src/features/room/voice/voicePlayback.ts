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
// (or after an end flag) with a seq that jumped by the pause, which the
// jitter buffer would read as a hole to fill with silence. So the sink is
// re-anchored at the start of every spurt — the cost is the buffer's priming
// cushion, once per spurt, which is exactly what a jitter buffer is for.
//
// Speaking indicators (speaking.ts) are fed from the same frames.

import { log } from '../../../lib/logger';
import type { AudioBufferProfile, AudioChunk } from '../../../transport/audio-buffer';
import type { VoiceFrameHeader } from '../../../transport/wire';
import { AudioSink, type AudioOutput } from '../../viewer/audioSink';
import { SpeakingTracker } from './speaking';
import { VOICE_SAMPLE_RATE, VOICE_SEQ_UNIT_US } from './voiceCapture';

export const SPEAKER_IDLE_RELEASE_MS = 10_000;
export const MAX_VOICE_SPEAKERS = 8;
// A pause longer than this starts a new spurt even without an end flag (the
// end-flagged terminator is a datagram too, and can be lost).
export const SPURT_GAP_MS = 200;
// Voice is not lip-synced to anything: the shallow live-edge envelope.
export const VOICE_AUDIO_PROFILE: AudioBufferProfile = { minMs: 40, maxMs: 150, seedMs: 60 };
const SWEEP_INTERVAL_MS = 1000;

export interface SpeakerDecoder {
  decode(payload: Uint8Array, timestampUs: number): void;
  close(): void;
}

export interface SpeakerSink {
  push(chunk: AudioChunk): void;
  flush(): void;
  setVolume(volume: number): void;
  dispose(): void;
}

export interface VoicePlaybackDeps {
  createDecoder: (onChunk: (chunk: AudioChunk) => void, onError: (e: Error) => void) => SpeakerDecoder;
  createSink: () => SpeakerSink | null;
  now: () => number;
}

export interface VoicePlaybackCallbacks {
  // The speaking set changed (Mumble session IDs, sorted).
  onSpeaking: (sessions: number[]) => void;
}

export interface VoicePlaybackLike {
  handleFrame(header: VoiceFrameHeader, payload: Uint8Array): void;
  setSpeakerVolume(session: number, volume: number): void;
  dispose(): void;
}

interface Speaker {
  session: number;
  decoder: SpeakerDecoder | null;
  sink: SpeakerSink | null;
  lastFrameAtMs: number;
  // The spurt ended (end flag): the next frame re-anchors.
  ended: boolean;
}

export function voicePlaybackSupported(): boolean {
  return (
    typeof WebTransport === 'function' &&
    typeof AudioDecoder === 'function' &&
    typeof AudioContext !== 'undefined' &&
    typeof AudioWorkletNode !== 'undefined'
  );
}

// The real decoder: Opus, 48 kHz mono (what Mumble sends), planar PCM out.
export function createOpusDecoder(onChunk: (chunk: AudioChunk) => void, onError: (e: Error) => void): SpeakerDecoder {
  const decoder = new AudioDecoder({
    output: (data) => {
      try {
        const channels: Float32Array[] = [];
        for (let i = 0; i < data.numberOfChannels; i++) {
          const plane = new Float32Array(data.numberOfFrames);
          data.copyTo(plane, { planeIndex: i, format: 'f32-planar' });
          channels.push(plane);
        }
        onChunk({ timestampUs: data.timestamp, sampleRate: data.sampleRate, channels, frameCount: data.numberOfFrames });
      } catch (e) {
        log.warn('voice copyTo failed; dropping a frame:', e);
      } finally {
        data.close();
      }
    },
    error: (e) => onError(e instanceof Error ? e : new Error(String(e))),
  });
  decoder.configure({ codec: 'opus', sampleRate: VOICE_SAMPLE_RATE, numberOfChannels: 1 });
  return {
    decode: (payload, timestampUs) =>
      decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: timestampUs, data: payload })),
    close: () => {
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
      const sink = new AudioSink({}, VOICE_AUDIO_PROFILE, { output: out });
      void sink.start(VOICE_SAMPLE_RATE).catch(() => {
        // logged by the sink; this speaker is silent, the rest play
      });
      return sink;
    },
    now: () => performance.now(),
  };
}

export class VoicePlayback implements VoicePlaybackLike {
  private readonly deps: VoicePlaybackDeps;
  private readonly cb: VoicePlaybackCallbacks;
  private readonly volumeFor: (session: number) => number;
  private readonly speakers = new Map<number, Speaker>();
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
    if (header.end && payload.length === 0) {
      // A bare terminator: the spurt is over; nothing to decode.
      if (sp) {
        sp.ended = true;
        sp.lastFrameAtMs = now;
      }
      return;
    }
    if (!sp) {
      sp = this.addSpeaker(session, now);
    } else if (sp.ended || now - sp.lastFrameAtMs > SPURT_GAP_MS) {
      sp.sink?.flush();
    }
    sp.ended = header.end;
    sp.lastFrameAtMs = now;
    if (!sp.decoder) return;
    try {
      sp.decoder.decode(payload, header.seq * VOICE_SEQ_UNIT_US);
    } catch (e) {
      log.warn(`voice decode failed for speaker ${session}; releasing it:`, e);
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
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    if (this.sweepTimer !== null) clearInterval(this.sweepTimer);
    this.sweepTimer = null;
    if (this.speakingTimer !== null) clearTimeout(this.speakingTimer);
    this.speakingTimer = null;
    for (const sp of [...this.speakers.values()]) this.release(sp);
    this.speaking.clear();
  }

  private addSpeaker(session: number, now: number): Speaker {
    if (this.speakers.size >= MAX_VOICE_SPEAKERS) {
      let oldest: Speaker | null = null;
      for (const s of this.speakers.values()) if (!oldest || s.lastFrameAtMs < oldest.lastFrameAtMs) oldest = s;
      if (oldest) this.release(oldest);
    }
    const sp: Speaker = { session, decoder: null, sink: null, lastFrameAtMs: now, ended: false };
    this.speakers.set(session, sp);
    try {
      sp.sink = this.deps.createSink();
      sp.sink?.setVolume(this.volumeFor(session));
      const sink = sp.sink;
      sp.decoder = this.deps.createDecoder(
        (chunk) => sink?.push(chunk),
        (e) => {
          log.warn(`voice decoder for speaker ${session} failed:`, e);
          if (this.speakers.get(session) === sp) this.release(sp);
        },
      );
    } catch (e) {
      log.warn(`voice playback for speaker ${session} could not start:`, e);
      this.closeParts(sp);
    }
    return sp;
  }

  private release(sp: Speaker): void {
    if (this.speakers.get(sp.session) === sp) this.speakers.delete(sp.session);
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
