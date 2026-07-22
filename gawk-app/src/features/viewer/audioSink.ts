// R15 (docs/20 Decisions 7-9): the viewer's main-thread audio sink — an
// AudioContext + AudioWorklet ring buffer fed with planar PCM from the
// pipeline's decode lane, with a GainNode for volume and mute-as-output-pause.
//
// This is the first deliberate decoded-media crossing in the project (R8/R10
// keep video frames inside the worker): AudioContext cannot exist in a
// dedicated worker, so the PCM has to reach this thread. Only plain
// ArrayBuffers cross — see audio-decode.ts.
//
// The worklet processor ships as a source string turned into a Blob URL. It
// has to be a separate script by construction (addModule takes a URL), and a
// Blob keeps it in this module instead of adding a build entry that would
// serve raw TypeScript the browser can't parse.

import { log } from '../../lib/logger';
import { AudioJitterBuffer, type AudioBufferProfile, type AudioChunk } from '../../transport/audio-buffer';

// The worklet: a dumb FIFO. Policy (gaps, late, overflow) is decided by
// AudioJitterBuffer before anything reaches here; the worklet owns only the
// drain and the underrun signal, and reports both back on its port.
export const PROCESSOR_SOURCE = `
class GawkAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.queue = [];
    // Fractional: the drift trim advances it by \`rate\` per output frame.
    this.offset = 0;
    this.playedFrames = 0;
    this.underruns = 0;
    this.lastReportAt = 0;
    this.playheadUs = null;
    this.rate = 1;
    this.port.onmessage = (e) => {
      const msg = e.data;
      if (msg.type === 'chunk') {
        this.queue.push(msg);
      } else if (msg.type === 'rate') {
        // Video-master drift correction (docs/20 Decision 10 revised): a
        // sub-audible resample, never a step or a skip. The controller
        // upstream owns the slew; this just honors it.
        this.rate = msg.rate;
      } else if (msg.type === 'flush') {
        this.queue.length = 0;
        this.offset = 0;
        this.playheadUs = null;
        this.rate = 1;
      }
    };
  }

  // Linearly interpolated read at the fractional position, so a rate that is
  // not exactly 1 doesn't quantize to sample drops. At |rate − 1| ≤ 0.4% the
  // interpolation error is far below the noise floor of the content.
  //
  // The partner sample is taken from the NEXT chunk at a chunk edge. Clamping
  // to the last sample instead looks harmless but holds the output flat for
  // one sample every 20 ms — a periodic seam at the packet rate, which is
  // audible as a buzz precisely because it is periodic.
  sampleAt(head, channelIndex, pos) {
    const src = head.channels[Math.min(channelIndex, head.channels.length - 1)];
    if (!src) return 0;
    const i0 = Math.floor(pos);
    const a = src[i0] ?? 0;
    const frac = pos - i0;
    if (frac === 0) return a;
    let b;
    if (i0 + 1 < head.frameCount) {
      b = src[i0 + 1];
    } else {
      const next = this.queue[1];
      if (!next) return a;
      const nextSrc = next.channels[Math.min(channelIndex, next.channels.length - 1)];
      b = nextSrc ? nextSrc[0] : a;
    }
    return a * (1 - frac) + b * frac;
  }

  process(_inputs, outputs) {
    const output = outputs[0];
    if (!output || output.length === 0) return true;
    const quantum = output[0].length;
    const rate = this.rate;

    // Sample-at-a-time because the read position is fractional. The chunked
    // bulk copy this replaced could only advance in whole samples, which is
    // exactly the granularity a drift trim has to work below.
    for (let i = 0; i < quantum; i++) {
      const head = this.queue[0];
      if (!head) {
        // Underrun: silence for the rest of the quantum (drops over stalls).
        for (let c = 0; c < output.length; c++) output[c].fill(0, i);
        this.underruns++;
        break;
      }
      for (let c = 0; c < output.length; c++) {
        output[c][i] = this.sampleAt(head, c, this.offset);
      }
      if (head.timestampUs != null) {
        this.playheadUs = head.timestampUs + (this.offset / head.sampleRate) * 1e6;
      }
      this.offset += rate;
      this.playedFrames += rate;
      // The remainder carries across the boundary — dropping it would make
      // every chunk edge a sub-sample skip.
      while (this.queue.length > 0 && this.offset >= this.queue[0].frameCount) {
        this.offset -= this.queue[0].frameCount;
        this.queue.shift();
      }
    }

    // ~4 Hz playhead/drain report (docs/20 Decision 10 measures skew from it).
    if (currentTime - this.lastReportAt >= 0.25) {
      this.lastReportAt = currentTime;
      this.port.postMessage({
        type: 'playhead',
        playheadUs: this.playheadUs,
        playedFrames: this.playedFrames,
        underruns: this.underruns,
        contextTime: currentTime,
      });
      this.playedFrames = 0;
      this.underruns = 0;
    }
    return true;
  }
}
registerProcessor('gawk-audio', GawkAudioProcessor);
`;

export interface AudioSinkStats {
  contextState: AudioContextState | null;
  playheadUs: number | null;
  contextTime: number | null;
}

export interface AudioSinkCallbacks {
  // The worklet's ~4 Hz report — N5 turns this into avSkewMs.
  onPlayhead?: (info: { playheadUs: number | null; contextTime: number }) => void;
}

export function audioSinkSupported(): boolean {
  return typeof AudioContext !== 'undefined' && typeof AudioWorkletNode !== 'undefined';
}

// Field finding 7 (docs/20): the worklet posts a playhead report every ~250 ms
// as long as its AudioContext is running — even while underrunning. A gap this
// long means the context was suspended (Safari does this at will) or the
// worklet died: the buffer's depth estimate is frozen above the overflow
// ceiling and every chunk is being dropped with no way back. Four missed
// reports is unambiguous; a backgrounded tab with audio keeps reporting, so
// this does not false-fire there.
const STALL_RECOVERY_MS = 1000;

// How long after the last decoded chunk a worklet underrun still describes
// audio health. Past it there is simply no audio to play, and a dry quantum is
// the correct outcome rather than a defect worth counting (BUGS.md).
const AUDIO_EXPECTED_MS = 1000;

export class AudioSink {
  private ctx: AudioContext | null = null;
  private node: AudioWorkletNode | null = null;
  private gain: GainNode | null = null;
  private buffer: AudioJitterBuffer;
  private cb: AudioSinkCallbacks;
  private starting: Promise<void> | null = null;
  private disposed = false;
  private sampleRate: number | null = null;
  private volume = 1;
  private muted = false;
  private lastPlayheadUs: number | null = null;
  private lastContextTime: number | null = null;
  // The sink's own clock (injectable for tests), shared with the buffer.
  private now: () => number;
  // Local ms of the last playhead report; the stall watchdog measures against
  // it. Null until the worklet reports for the first time, so boot latency is
  // never mistaken for a stall.
  private lastPlayheadAtMs: number | null = null;
  private lastPushAtMs: number | null = null;
  private lastStallRecoverAtMs = -Infinity;

  // Where the video pipeline says a frame with this timestamp will be
  // presented, in this context's performance.now() ms. Set by the owner from
  // the pipeline's stats; null until the video baseline exists.
  private videoPresentationMs: ((timestampUs: number) => number | null) | null = null;

  constructor(
    callbacks: AudioSinkCallbacks = {},
    profile?: AudioBufferProfile | (() => AudioBufferProfile),
    opts: { now?: () => number } = {},
  ) {
    this.cb = callbacks;
    const now = opts.now ?? (() => performance.now());
    this.now = now;
    this.buffer = new AudioJitterBuffer((chunk) => this.forward(chunk), profile, {
      now,
      // Field finding 7: hold the alignment cushion until the worklet node
      // exists, so it is never released into a null node and lost.
      sinkReady: () => this.node !== null,
      // Video-master alignment (docs/20 field finding 4): hand a chunk to the
      // worklet early by exactly the latency the device adds, so it is *heard*
      // when the matching video frame is presented.
      schedule: () => {
        const present = this.videoPresentationMs;
        if (!present) return null;
        const lead = this.outputLatencyMs();
        return (timestampUs: number) => {
          const at = present(timestampUs);
          return at === null ? null : at - lead;
        };
      },
    });
  }

  // The device's own contribution, measured where the browser reports it.
  // outputLatency is the honest number (it includes the OS mixer); baseLatency
  // is the fallback, and 20 ms a last resort for scopes with neither.
  private outputLatencyMs(): number {
    const ctx = this.ctx as (AudioContext & { outputLatency?: number }) | null;
    const seconds = ctx?.outputLatency || ctx?.baseLatency || 0.02;
    return seconds * 1000;
  }

  // The video schedule, refreshed by the owner as the pipeline's baseline
  // settles. Only consulted while audio is still waiting to start.
  setVideoSchedule(present: ((timestampUs: number) => number | null) | null): void {
    this.videoPresentationMs = present;
    this.buffer.tick();
  }

  // Drift trim (av-sync AudioRateController): a sub-audible playback rate.
  setRate(rate: number): void {
    if (Math.abs(rate - this.lastRate) < 1e-5) return;
    this.lastRate = rate;
    try {
      this.node?.port.postMessage({ type: 'rate', rate });
    } catch {
      // Same rule as forward(): audio must never break video.
    }
  }

  private lastRate = 1;

  // Creates the context. Must be called from a user gesture (the viewer's
  // join click) — a context created outside one starts 'suspended' and the
  // screen shows tap-to-unmute (docs/20 Decision 9).
  async start(sampleRate: number): Promise<void> {
    if (this.disposed) return;
    if (this.starting) return this.starting;
    this.sampleRate = sampleRate;
    this.starting = this.build(sampleRate).catch((e) => {
      log.warn('Audio sink start failed; the stream plays video-only:', e);
      this.teardownNodes();
      throw e;
    });
    return this.starting;
  }

  private async build(sampleRate: number): Promise<void> {
    const ctx = new AudioContext({ sampleRate, latencyHint: 'interactive' });
    this.ctx = ctx;
    const url = URL.createObjectURL(new Blob([PROCESSOR_SOURCE], { type: 'application/javascript' }));
    try {
      await ctx.audioWorklet.addModule(url);
    } finally {
      URL.revokeObjectURL(url);
    }
    if (this.disposed) {
      void ctx.close();
      return;
    }
    const node = new AudioWorkletNode(ctx, 'gawk-audio', { outputChannelCount: [2] });
    node.port.onmessage = (e: MessageEvent) => {
      const msg = e.data as {
        type: string;
        playheadUs: number | null;
        playedFrames: number;
        underruns: number;
        contextTime: number;
      };
      if (msg.type !== 'playhead') return;
      // A live report means the worklet is draining: the stall watchdog resets.
      this.lastPlayheadAtMs = this.now();
      this.lastPlayheadUs = msg.playheadUs;
      this.lastContextTime = msg.contextTime;
      if (this.sampleRate) this.buffer.notePlayed((msg.playedFrames / this.sampleRate) * 1000);
      if (msg.underruns > 0) {
        // Underrunning with nothing to play is silence working, not a defect:
        // once the stream dies the worklet reports a dry quantum ~375×/s
        // forever, which buried the counter's real signal under six figures of
        // noise exactly when it was being read to diagnose a freeze (BUGS.md,
        // 2026-07-22). Report zero rather than skipping the call: the re-prime
        // side effect must still run, so audio that resumes rebuilds its
        // cushion instead of restarting at ~0 ms depth (field finding 6).
        this.buffer.noteUnderrun(this.audioExpected() ? msg.underruns : 0);
      }
      this.cb.onPlayhead?.({ playheadUs: msg.playheadUs, contextTime: msg.contextTime });
    };
    const gain = ctx.createGain();
    gain.gain.value = this.muted ? 0 : this.volume;
    node.connect(gain);
    gain.connect(ctx.destination);
    this.node = node;
    this.gain = gain;
  }

  // Feeds one decoded chunk. Safe before start() resolves — the jitter buffer
  // holds policy, and forward() drops silently until the node exists (those
  // first packets are ~one worklet-boot behind live anyway).
  push(chunk: AudioChunk): void {
    if (this.disposed) return;
    this.lastPushAtMs = this.now();
    this.maybeRecoverFromStall();
    this.buffer.push(chunk);
  }

  // Whether audio is still arriving, i.e. whether a dry worklet means anything.
  // One window of packets (50/s) is far more slack than any jitter this
  // buffer tolerates, so a true starvation still counts while a dead stream
  // stops counting almost immediately.
  private audioExpected(): boolean {
    return this.lastPushAtMs !== null && this.now() - this.lastPushAtMs <= AUDIO_EXPECTED_MS;
  }

  // Field finding 7: if the worklet has stopped reporting while audio is still
  // arriving, its context was suspended (or it died) and the jitter buffer is
  // now dropping every chunk with no recovery. Wake the context and flush both
  // sides so audio resumes at the live edge the instant the worklet runs again,
  // instead of never. Only meaningful once the worklet has reported at least
  // once — before that, the gap is boot latency, not a stall.
  private maybeRecoverFromStall(): void {
    if (!this.node || this.lastPlayheadAtMs === null) return;
    const now = this.now();
    if (now - this.lastPlayheadAtMs <= STALL_RECOVERY_MS) return;
    // Cooldown: one recovery per interval, not one per 50/s packet.
    if (now - this.lastStallRecoverAtMs < STALL_RECOVERY_MS) return;
    this.lastStallRecoverAtMs = now;
    log.warn('Audio worklet stopped reporting; resuming context and re-anchoring.');
    if (this.ctx?.state === 'suspended') void this.resume();
    this.flush();
    // Measure the next stall from here rather than re-firing every packet until
    // a fresh report lands.
    this.lastPlayheadAtMs = now;
  }

  // Returns whether the chunk reached the worklet — the buffer counts depth
  // only on delivery (field finding 7). A null node (still booting) or a
  // throwing/closed port both mean "not delivered".
  private forward(chunk: AudioChunk): boolean {
    const node = this.node;
    if (!node) return false;
    const transfer: ArrayBuffer[] = [];
    for (const c of chunk.channels) transfer.push(c.buffer as ArrayBuffer);
    try {
      node.port.postMessage(
        {
          type: 'chunk',
          channels: chunk.channels,
          frameCount: chunk.frameCount,
          sampleRate: chunk.sampleRate,
          timestampUs: chunk.timestampUs,
        },
        transfer,
      );
      return true;
    } catch (e) {
      // A detached buffer or closed port must not propagate: this runs on the
      // viewer's message path, and audio is never allowed to break video.
      // Dropping the packet is the correct live-edge outcome anyway.
      log.warn('Audio chunk could not reach the worklet; dropping it:', e);
      return false;
    }
  }

  // Broadcaster restart / viewer reconnect: drop everything and re-anchor
  // (docs/20 Decision 8). Without it every packet on the new timeline reads
  // as late forever.
  flush(): void {
    this.buffer.flush();
    this.lastRate = 1;
    try {
      this.node?.port.postMessage({ type: 'flush' });
    } catch (e) {
      // Same rule as forward(): a closed port must not break video. The buffer
      // is already re-anchored; the worklet clears on its next live message.
      log.warn('Audio worklet flush could not be delivered:', e);
    }
  }

  setVolume(volume: number): void {
    this.volume = Math.max(0, Math.min(1, volume));
    if (this.gain && !this.muted) this.gain.gain.value = this.volume;
  }

  setMuted(muted: boolean): void {
    this.muted = muted;
    // Output pause, not a pipeline pause: stats keep flowing, unmute is
    // instant (docs/20 Decision 9).
    if (this.gain) this.gain.gain.value = muted ? 0 : this.volume;
  }

  // Autoplay-policy recovery: the tap-to-unmute affordance calls this from a
  // real gesture.
  async resume(): Promise<void> {
    try {
      await this.ctx?.resume();
    } catch (e) {
      log.warn('AudioContext resume rejected:', e);
    }
  }

  get contextState(): AudioContextState | null {
    return this.ctx?.state ?? null;
  }

  // True when the browser is holding playback for a gesture.
  get needsGesture(): boolean {
    return this.ctx?.state === 'suspended';
  }

  updateTarget(jitterMs: number | null, nowMs: number): void {
    this.buffer.updateTarget(jitterMs, nowMs);
  }

  getStats() {
    return {
      ...this.buffer.getStats(),
      contextState: this.contextState,
      playheadUs: this.lastPlayheadUs,
      contextTime: this.lastContextTime,
    };
  }

  private teardownNodes(): void {
    try {
      this.node?.disconnect();
      this.gain?.disconnect();
    } catch {
      // best effort
    }
    this.node = null;
    this.gain = null;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.teardownNodes();
    const ctx = this.ctx;
    this.ctx = null;
    void ctx?.close().catch(() => {});
  }
}
