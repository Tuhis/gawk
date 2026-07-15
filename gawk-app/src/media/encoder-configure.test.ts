// Encoder.configure() acceleration hinting. When every prefer-hardware
// variant is rejected and a hint-free variant wins (Firefox: WebCodecs
// VideoEncoder is software-only, so this is the *normal* outcome there),
// an explicit log line must say hardware encoding is unavailable — so
// "why is this stream software-encoded?" is answerable from one console
// line instead of a debugging session.
//
// R12 (docs/17 L2): the acceleration tri-state filters the variant cascade
// — 'hardware' refuses to configure software, 'software' probes only
// prefer-software variants.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { log } from '../lib/logger';
import { Encoder } from './encoder';
import { DEFAULT_CAPTURE_CONFIG, type CaptureConfig } from './types';

const probed: VideoEncoderConfig[] = [];

type Respond = (config: VideoEncoderConfig) => { supported: boolean; config?: VideoEncoderConfig };

function stubVideoEncoderRespond(respond: Respond): void {
  probed.length = 0;
  vi.stubGlobal(
    'VideoEncoder',
    class {
      static isConfigSupported = async (config: VideoEncoderConfig) => {
        probed.push(config);
        return respond(config);
      };
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

function stubVideoEncoder(supported: (config: VideoEncoderConfig) => boolean): void {
  stubVideoEncoderRespond((config) => ({ supported: supported(config), config }));
}

const config: CaptureConfig = {
  ...DEFAULT_CAPTURE_CONFIG,
  codecPreferences: ['avc1.4D4034'],
};

function makeEncoder(overrides: Partial<CaptureConfig> = {}): Encoder {
  return new Encoder({ ...config, ...overrides }, { onEncoded: () => {}, onError: () => {} });
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

describe('Encoder.configure acceleration tri-state (R12)', () => {
  it('hardware mode refuses when every prefer-hardware variant is rejected', async () => {
    stubVideoEncoder((c) => c.hardwareAcceleration !== 'prefer-hardware');
    await expect(makeEncoder({ hwPreference: 'hardware' }).configure()).rejects.toThrow(
      /Hardware encoding required/,
    );
    // It never even probed a non-HW variant — refusal, not fallback.
    for (const c of probed) expect(c.hardwareAcceleration).toBe('prefer-hardware');
  });

  it('hardware mode refuses a supported answer the browser resolved to software', async () => {
    stubVideoEncoderRespond((c) => ({
      supported: true,
      config: { ...c, hardwareAcceleration: 'prefer-software' as HardwareAcceleration },
    }));
    await expect(makeEncoder({ hwPreference: 'hardware' }).configure()).rejects.toThrow(
      /Hardware encoding required/,
    );
  });

  it('hardware mode configures normally when hardware resolves', async () => {
    stubVideoEncoder(() => true);
    const configured = await makeEncoder({ hwPreference: 'hardware' }).configure();
    expect(configured.acceleration).toBe('hardware');
    expect(configured.variantLabel).toBe('prefer-hw + realtime');
  });

  it('software mode probes only prefer-software variants', async () => {
    stubVideoEncoder((c) => c.hardwareAcceleration === 'prefer-software');
    const configured = await makeEncoder({ hwPreference: 'software' }).configure();
    expect(configured.acceleration).toBe('software');
    expect(configured.variantLabel).toBe('prefer-sw + realtime');
    for (const c of probed) expect(c.hardwareAcceleration).toBe('prefer-software');
    // No hardware-unavailable hint: nothing was rejected, software was chosen.
    expect(hintLogged()).toBe(false);
  });

  it('auto mode (absent hwPreference) keeps the historical cascade order', async () => {
    stubVideoEncoder(() => false);
    await expect(makeEncoder().configure()).rejects.toThrow(/No codec \/ config combination/);
    const labels = probed.map((c) =>
      `${c.hardwareAcceleration ?? 'none'}/${c.latencyMode ?? 'none'}`,
    );
    expect(labels).toEqual([
      'prefer-hardware/realtime',
      'prefer-hardware/none',
      'none/realtime',
      'none/none',
    ]);
  });
});
