// Encoder.configure() acceleration hinting. When every prefer-hardware
// variant is rejected and a hint-free variant wins (Firefox: WebCodecs
// VideoEncoder is software-only, so this is the *normal* outcome there),
// an explicit log line must say hardware encoding is unavailable — so
// "why is this stream software-encoded?" is answerable from one console
// line instead of a debugging session.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { log } from '../lib/logger';
import { Encoder } from './encoder';
import { DEFAULT_CAPTURE_CONFIG, type CaptureConfig } from './types';

function stubVideoEncoder(supported: (config: VideoEncoderConfig) => boolean): void {
  vi.stubGlobal(
    'VideoEncoder',
    class {
      static isConfigSupported = async (config: VideoEncoderConfig) => ({
        supported: supported(config),
        config,
      });
      state = 'unconfigured';
      constructor(_init: VideoEncoderInit) {}
      configure(_config: VideoEncoderConfig): void {
        this.state = 'configured';
      }
      close(): void {
        this.state = 'closed';
      }
    },
  );
}

const config: CaptureConfig = {
  ...DEFAULT_CAPTURE_CONFIG,
  codecPreferences: ['avc1.4D4034'],
};

function makeEncoder(): Encoder {
  return new Encoder(config, { onEncoded: () => {}, onError: () => {} });
}

const infoMessages: string[] = [];

function hintLogged(): boolean {
  return infoMessages.some((m) => m.includes('Hardware encoding unavailable'));
}

beforeEach(() => {
  infoMessages.length = 0;
  vi.spyOn(log, 'info').mockImplementation((...args: unknown[]) => {
    infoMessages.push(String(args[0]));
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('Encoder.configure hardware-unavailable hint', () => {
  it('logs the hint when every prefer-hardware variant is rejected', async () => {
    stubVideoEncoder((c) => c.hardwareAcceleration !== 'prefer-hardware');
    const configured = await makeEncoder().configure();
    expect(configured.variantLabel).toBe('realtime');
    expect(hintLogged()).toBe(true);
  });

  it('does not log the hint when a prefer-hardware variant is accepted', async () => {
    stubVideoEncoder(() => true);
    const configured = await makeEncoder().configure();
    expect(configured.variantLabel).toBe('prefer-hw + realtime');
    expect(hintLogged()).toBe(false);
  });
});
