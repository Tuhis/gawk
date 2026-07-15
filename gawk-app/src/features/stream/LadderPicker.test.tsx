// @vitest-environment jsdom
//
// R12 L4 (docs/17 Decision 9): picker options are annotated from the probe
// matrix — ' · software' badge, disabled 'unsupported' — never removed, and
// annotations recompute when the matrix changes (acceleration mode / codec
// pin re-probes).

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';

import { LadderPicker } from './LadderPicker';
import type { ResolutionRung } from '../../media/ladder';
import type { ProbeAcceleration, SupportMatrix } from '../../media/probe';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';

function fakeMatrix(
  acc: (rung: ResolutionRung, fps: number) => ProbeAcceleration,
): SupportMatrix {
  return {
    source: { width: 3840, height: 2160 },
    hwPreference: 'auto',
    entries: new Map(),
    get: (rung, fps) => ({ acceleration: acc(rung, fps), codec: 'avc1.4D4034' }),
  };
}

function optionLabels(selectLabel: string): string[] {
  const select = screen.getByLabelText(selectLabel) as HTMLSelectElement;
  return Array.from(select.options).map((o) => o.text);
}

function option(selectLabel: string, text: string): HTMLOptionElement {
  const select = screen.getByLabelText(selectLabel) as HTMLSelectElement;
  const found = Array.from(select.options).find((o) => o.text === text);
  if (!found) throw new Error(`option "${text}" not found in ${optionLabels(selectLabel).join(', ')}`);
  return found;
}

beforeEach(() => {
  localStorage.clear();
  const s = useBroadcastSettingsStore.getState();
  s.setResolutionSelection('auto');
  s.setFramerateSelection('auto');
});

afterEach(cleanup);

describe('LadderPicker without a matrix', () => {
  it('renders plain, enabled options (probe unavailable)', () => {
    render(<LadderPicker matrix={null} />);
    expect(optionLabels('Resolution')).toEqual(['auto', 'native', '1080p', '720p', '480p']);
    expect(optionLabels('Framerate')).toEqual(['auto', 'native', '60 fps', '30 fps', '5 fps']);
    expect(option('Resolution', 'native').disabled).toBe(false);
  });
});

describe('LadderPicker with a probe matrix', () => {
  // HW up to 1080p at 60/30; 5 fps unsupported everywhere.
  const matrix = fakeMatrix((rung, fps) => {
    if (fps === 5) return 'unsupported';
    return rung !== 'native' ? 'hardware' : 'software';
  });

  it('badges software combos and leaves hardware ones unmarked', () => {
    render(<LadderPicker matrix={matrix} />);
    expect(option('Resolution', 'native · software')).toBeDefined();
    expect(option('Resolution', '1080p')).toBeDefined(); // hardware — unmarked
    expect(option('Resolution', 'auto')).toBeDefined(); // auto never annotated
  });

  it('disables unsupported combos instead of hiding them', () => {
    render(<LadderPicker matrix={matrix} />);
    const fps5 = option('Framerate', '5 fps · unsupported');
    expect(fps5.disabled).toBe(true);
    // Software options stay selectable (explicit choices are honored).
    expect(option('Resolution', 'native · software').disabled).toBe(false);
  });

  it('fps options under auto resolution annotate with the best rung at that fps', () => {
    render(<LadderPicker matrix={matrix} />);
    // 60 fps: 1080p does HW — unmarked even though native is software.
    expect(option('Framerate', '60 fps')).toBeDefined();
  });

  it('annotations recompute when the matrix changes', () => {
    const { rerender } = render(<LadderPicker matrix={matrix} />);
    expect(option('Resolution', 'native · software')).toBeDefined();
    rerender(<LadderPicker matrix={fakeMatrix(() => 'hardware')} />);
    expect(option('Resolution', 'native')).toBeDefined();
    expect(optionLabels('Resolution')).not.toContain('native · software');
  });

  it('explicit resolution selection drives fps annotations', () => {
    useBroadcastSettingsStore.getState().setResolutionSelection('native');
    render(<LadderPicker matrix={matrix} />);
    // native is software at every fps here.
    expect(option('Framerate', '60 fps · software')).toBeDefined();
  });
});
