export interface DecodedFrame {
  frame: VideoFrame;
  captureTimestampUs: number;
  decodeStartMs: number;
  decodeEndMs: number;
}

export interface DecoderCallbacks {
  onDecoded: (decoded: DecodedFrame) => void;
  onError: (err: Error) => void;
}

export class Decoder {
  private decoder: VideoDecoder;
  private decodeStartMap = new Map<number, number>();

  constructor(callbacks: DecoderCallbacks) {
    this.decoder = new VideoDecoder({
      output: (frame) => {
        const start = this.decodeStartMap.get(frame.timestamp) ?? performance.now();
        this.decodeStartMap.delete(frame.timestamp);
        callbacks.onDecoded({
          frame,
          captureTimestampUs: frame.timestamp,
          decodeStartMs: start,
          decodeEndMs: performance.now(),
        });
      },
      error: (e) => callbacks.onError(e instanceof Error ? e : new Error(String(e))),
    });
  }

  async configure(cfg: VideoDecoderConfig): Promise<void> {
    const wantsHwHint: VideoDecoderConfig = {
      hardwareAcceleration: 'prefer-hardware',
      optimizeForLatency: true,
      ...cfg,
    };
    const support = await VideoDecoder.isConfigSupported(wantsHwHint);
    if (!support.supported) {
      throw new Error(`Decoder config unsupported: ${cfg.codec} @ ${cfg.codedWidth}x${cfg.codedHeight}`);
    }
    this.decoder.configure(support.config ?? wantsHwHint);
  }

  decode(chunk: EncodedVideoChunk): void {
    if (this.decoder.state !== 'configured') return;
    this.decodeStartMap.set(chunk.timestamp, performance.now());
    this.decoder.decode(chunk);
  }

  get queueSize(): number {
    return this.decoder.decodeQueueSize;
  }

  async close(): Promise<void> {
    try {
      if (this.decoder.state === 'configured') await this.decoder.flush();
    } catch {
      // flush can throw during shutdown races — ignore
    }
    if (this.decoder.state !== 'closed') this.decoder.close();
    this.decodeStartMap.clear();
  }
}
