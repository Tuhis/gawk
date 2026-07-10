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
    // Like the encoder (see media/encoder.ts): 'prefer-hardware' makes Chrome
    // return supported=false when it can't deliver HW decode — e.g. software
    // Chrome inside WSL2 with no GPU. So probe HW first, then fall back to no
    // hint (software) rather than throwing on the first miss.
    const base: VideoDecoderConfig = { optimizeForLatency: true, ...cfg };
    const variants: VideoDecoderConfig[] = [
      { hardwareAcceleration: 'prefer-hardware', ...base },
      base,
    ];
    for (const variant of variants) {
      const support = await VideoDecoder.isConfigSupported(variant);
      if (support.supported) {
        this.decoder.configure(support.config ?? variant);
        return;
      }
    }
    throw new Error(
      `Decoder config unsupported (tried HW + software): ${cfg.codec}` +
        (cfg.codedWidth ? ` @ ${cfg.codedWidth}x${cfg.codedHeight}` : ''),
    );
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
