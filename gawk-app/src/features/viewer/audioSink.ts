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
const PROCESSOR_SOURCE = `
class GawkAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.queue = [];
    this.offset = 0;
    this.playedFrames = 0;
    this.underruns = 0;
    this.lastReportAt = 0;
    this.playheadUs = null;
    this.port.onmessage = (e) => {
      const msg = e.data;
      if (msg.type === 'chunk') {
        this.queue.push(msg);
      } else if (msg.type === 'flush') {
        this.queue.length = 0;
        this.offset = 0;
        this.playheadUs = null;
      }
    };
  }

  process(_inputs, outputs) {
    const output = outputs[0];
    if (!output || output.length === 0) return true;
    const quantum = output[0].length;
    let written = 0;

    while (written < quantum) {
      const head = this.queue[0];
      if (!head) {
        // Underrun: silence for the rest of the quantum (drops over stalls).
        for (let c = 0; c < output.length; c++) output[c].fill(0, written);
        this.underruns++;
        written = quantum;
        break;
      }
      const available = head.frameCount - this.offset;
      const take = Math.min(available, quantum - written);
      for (let c = 0; c < output.length; c++) {
        const src = head.channels[Math.min(c, head.channels.length - 1)];
        if (src) {
          output[c].set(src.subarray(this.offset, this.offset + take), written);
        } else {
          output[c].fill(0, written, written + take);
        }
      }
      if (head.timestampUs != null) {
        this.playheadUs = head.timestampUs + (this.offset / head.sampleRate) * 1e6;
      }
      this.offset += take;
      written += take;
      this.playedFrames += take;
      if (this.offset >= head.frameCount) {
        this.queue.shift();
        this.offset = 0;
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

  constructor(callbacks: AudioSinkCallbacks = {}, profile?: AudioBufferProfile | (() => AudioBufferProfile)) {
    this.cb = callbacks;
    this.buffer = new AudioJitterBuffer((chunk) => this.forward(chunk), profile);
  }

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
      this.lastPlayheadUs = msg.playheadUs;
      this.lastContextTime = msg.contextTime;
      if (this.sampleRate) this.buffer.notePlayed((msg.playedFrames / this.sampleRate) * 1000);
      if (msg.underruns > 0) this.buffer.noteUnderrun(msg.underruns);
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
    this.buffer.push(chunk);
  }

  private forward(chunk: AudioChunk): void {
    const node = this.node;
    if (!node) return;
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
    } catch (e) {
      // A detached buffer or closed port must not propagate: this runs on the
      // viewer's message path, and audio is never allowed to break video.
      // Dropping the packet is the correct live-edge outcome anyway.
      log.warn('Audio chunk could not reach the worklet; dropping it:', e);
    }
  }

  // Broadcaster restart / viewer reconnect: drop everything and re-anchor
  // (docs/20 Decision 8). Without it every packet on the new timeline reads
  // as late forever.
  flush(): void {
    this.buffer.flush();
    this.node?.port.postMessage({ type: 'flush' });
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
