// PCM → AudioData framing for the AudioWorklet capture path (R79, docs/80
// D21). Firefox has no MediaStreamTrackProcessor, so its microphone reaches
// the encoder through an AudioWorklet node that posts each render quantum's
// samples to the main thread; this module cuts that stream into 10 ms blocks
// and describes each as an AudioData. Pure, so the framing is unit-tested
// without an AudioContext.

// One block per Opus frame's worth of input: 10 ms.
export const VOICE_BLOCK_MS = 10;

export function blockFramesFor(sampleRate: number, ms = VOICE_BLOCK_MS): number {
  return Math.max(1, Math.round((sampleRate * ms) / 1000));
}

// Accumulates arbitrary-size chunks of mono PCM and hands back whole blocks of
// exactly `blockFrames` samples, in order, each with the timestamp of its
// first sample on a running sample clock (µs since the first sample). Each
// returned block owns its buffer (it is transferred into an AudioData).
export class PcmBlocker {
  private readonly blockFrames: number;
  private readonly sampleRate: number;
  private pending: Float32Array;
  private fill = 0;
  // Samples handed out in whole blocks so far: the clock the timestamps
  // count, so they never drift from the audio itself.
  private emitted = 0;

  constructor(sampleRate: number, blockFrames = blockFramesFor(sampleRate)) {
    if (!(sampleRate > 0) || !(blockFrames > 0)) throw new Error(`invalid framing ${sampleRate} Hz / ${blockFrames}`);
    this.sampleRate = sampleRate;
    this.blockFrames = blockFrames;
    this.pending = new Float32Array(blockFrames);
  }

  push(samples: Float32Array): { pcm: Float32Array; timestampUs: number }[] {
    const out: { pcm: Float32Array; timestampUs: number }[] = [];
    let off = 0;
    while (off < samples.length) {
      const take = Math.min(this.blockFrames - this.fill, samples.length - off);
      this.pending.set(samples.subarray(off, off + take), this.fill);
      this.fill += take;
      off += take;
      if (this.fill === this.blockFrames) {
        out.push({ pcm: this.pending, timestampUs: Math.round((this.emitted / this.sampleRate) * 1e6) });
        this.emitted += this.blockFrames;
        this.pending = new Float32Array(this.blockFrames);
        this.fill = 0;
      }
    }
    return out;
  }
}

// What `new AudioData(...)` takes for one block: f32-planar mono.
export function blockAudioDataInit(pcm: Float32Array, sampleRate: number, timestampUs: number): AudioDataInit {
  return {
    format: 'f32-planar',
    sampleRate,
    numberOfFrames: pcm.length,
    numberOfChannels: 1,
    timestamp: timestampUs,
    data: pcm as Float32Array<ArrayBuffer>,
  };
}

// Down-mixes one render quantum's channels to mono (the average). Mono input
// is copied as is: the worklet's buffers are reused by the engine.
export function downmixToMono(channels: Float32Array[]): Float32Array {
  const n = channels[0]?.length ?? 0;
  const out = new Float32Array(n);
  if (channels.length === 0) return out;
  if (channels.length === 1) {
    out.set(channels[0]);
    return out;
  }
  for (const ch of channels) for (let i = 0; i < n; i++) out[i] += ch[i] ?? 0;
  for (let i = 0; i < n; i++) out[i] /= channels.length;
  return out;
}

// The capture worklet: posts every render quantum's input, mono, to the main
// thread. Silent output (the node is pulled through a muted gain so the
// engine renders it; it must never be heard). A source string for the same
// reason as audioSink.ts's PROCESSOR_SOURCE: addModule takes a URL.
export const CAPTURE_PROCESSOR_NAME = 'gawk-voice-capture';
export const CAPTURE_PROCESSOR_SOURCE = `
class GawkVoiceCapture extends AudioWorkletProcessor {
  process(inputs) {
    const input = inputs[0];
    if (input && input.length > 0 && input[0].length > 0) {
      const n = input[0].length;
      const mono = new Float32Array(n);
      for (let c = 0; c < input.length; c++) {
        const ch = input[c];
        for (let i = 0; i < n; i++) mono[i] += ch[i];
      }
      if (input.length > 1) for (let i = 0; i < n; i++) mono[i] /= input.length;
      this.port.postMessage(mono, [mono.buffer]);
    }
    return true;
  }
}
registerProcessor('${CAPTURE_PROCESSOR_NAME}', GawkVoiceCapture);
`;
