// @vitest-environment jsdom
//
// R13 codec-pin annotations (docs/18 Decision 9 extended to the codec list):
// each codec option is badged from its own single-codec matrix — hardware
// unmarked, ' · software' badge, ' · unsupported' disabled — and the
// annotation answers "what would pinning this codec get at the current
// resolution/fps selections".

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';

import { EncoderSettingsPanel } from './EncoderSettingsPanel';
import type { ResolutionRung } from '../../media/ladder';
import type { ProbeAcceleration, SupportMatrix } from '../../media/probe';
import { DEFAULT_CODEC_PREFERENCES } from '../../media/types';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

function fakeMatrix(acc: (rung: ResolutionRung, fps: number) => ProbeAcceleration): SupportMatrix {
  return {
    source: { width: 3840, height: 2160 },
    hwPreference: 'auto',
    entries: new Map(),
    get: (rung, fps) => ({ acceleration: acc(rung, fps), codec: null }),
  };
}

// vp8 unsupported everywhere; VP9 software; H.264 hardware — except at the
// 480 rung, where even H.264 is software (exercises selection-awareness).
function makeCodecMatrices(): Map<string, SupportMatrix> {
  const matrices = new Map<string, SupportMatrix>();
  for (const codec of DEFAULT_CODEC_PREFERENCES) {
    if (codec === 'vp8') {
      matrices.set(codec, fakeMatrix(() => 'unsupported'));
    } else if (codec.startsWith('vp09')) {
      matrices.set(codec, fakeMatrix(() => 'software'));
    } else {
      matrices.set(codec, fakeMatrix((rung) => (rung === 480 ? 'software' : 'hardware')));
    }
  }
  return matrices;
}

function codecOptions(): HTMLOptionElement[] {
  const select = screen.getByLabelText('Codec') as HTMLSelectElement;
  return Array.from(select.options);
}

function option(text: string): HTMLOptionElement {
  const found = codecOptions().find((o) => o.text === text);
  if (!found) {
    throw new Error(`option "${text}" not found in: ${codecOptions().map((o) => o.text).join(', ')}`);
  }
  return found;
}

beforeEach(() => {
  localStorage.clear();
  const s = useBroadcastSettingsStore.getState();
  s.setResolutionSelection('auto');
  s.setFramerateSelection('auto');
  s.setHwPreference('auto');
  s.setCodecOverride(null);
});

afterEach(cleanup);

describe('EncoderSettingsPanel codec annotations', () => {
  it('renders plain options without matrices (probe unavailable)', () => {
    render(<EncoderSettingsPanel codecMatrices={null} />);
    expect(option('VP8').disabled).toBe(false);
    expect(option('H.264 · avc1.4D4034')).toBeDefined();
  });

  it('badges software codecs and disables unsupported ones — never removes', () => {
    render(<EncoderSettingsPanel codecMatrices={makeCodecMatrices()} />);
    expect(option('H.264 · avc1.4D4034').disabled).toBe(false); // hardware — unmarked
    expect(option('VP9 · vp09.00.40.08 · software').disabled).toBe(false); // selectable
    expect(option('VP8 · unsupported').disabled).toBe(true);
    // The full preference list is still present (annotated, not filtered out).
    expect(codecOptions().length).toBe(DEFAULT_CODEC_PREFERENCES.length + 1); // + 'auto'
  });

  it('annotations follow the current resolution selection', () => {
    useBroadcastSettingsStore.getState().setResolutionSelection(480);
    render(<EncoderSettingsPanel codecMatrices={makeCodecMatrices()} />);
    // At the 480 rung even H.264 is software in this fake.
    expect(option('H.264 · avc1.4D4034 · software')).toBeDefined();
  });
});
