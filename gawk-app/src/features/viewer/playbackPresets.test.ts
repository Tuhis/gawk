import { describe, expect, it } from 'vitest';
import {
  ADVANCED_DEFAULTS,
  PRESETS,
  advancedChanges,
  effectivePlayout,
  notApplicable,
  presetConfig,
  presetLabel,
  resolvePreset,
  type PlaybackConfig,
} from './playbackPresets';

// Today's shipping default: live-edge delivery, R12 adaptive pacing +
// interpolation, fleet-max parity, auto striping. R32 must not change it.
const DEFAULT_CONFIG: PlaybackConfig = {
  delivery: 'live',
  playout: 'adaptive',
  ...ADVANCED_DEFAULTS,
};

describe('playbackPresets — resolution', () => {
  // UX2.1
  it('resolves today’s default state to Balanced', () => {
    expect(resolvePreset(DEFAULT_CONFIG)).toBe('balanced');
    expect(presetLabel('balanced')).toBe('Balanced');
  });

  it('round-trips every preset through presetConfig → resolvePreset', () => {
    for (const preset of PRESETS) {
      expect(resolvePreset(presetConfig(preset.id))).toBe(preset.id);
    }
  });

  it('gives every preset a distinct delivery/playout pair', () => {
    const pairs = PRESETS.map((p) => `${p.delivery}/${p.playout}`);
    expect(new Set(pairs).size).toBe(PRESETS.length);
  });

  // UX2.2 — the whole basis of "Custom appears only once you change something".
  it('returns null when any single advanced field is off its default', () => {
    expect(resolvePreset({ ...DEFAULT_CONFIG, parity: 0 })).toBeNull();
    expect(resolvePreset({ ...DEFAULT_CONFIG, striping: 'off' })).toBeNull();
    expect(resolvePreset({ ...DEFAULT_CONFIG, interpolation: false })).toBeNull();
  });

  // UX2.3 — a state a real R19-era viewer can already be in. Snapping it to a
  // nearest preset would relabel it as something it is not.
  it('returns null for a legacy off-preset combination rather than snapping', () => {
    expect(resolvePreset({ ...DEFAULT_CONFIG, delivery: 'resilient', playout: 'off' })).toBeNull();
  });

  // UX2.4
  it('always returns null for the dev-only fixed pacing diagnostic', () => {
    expect(resolvePreset({ ...DEFAULT_CONFIG, playout: 'fixed' })).toBeNull();
    expect(
      resolvePreset({ ...DEFAULT_CONFIG, delivery: 'resilient', playout: 'fixed' }),
    ).toBeNull();
  });

  it('labels an unresolved configuration Custom', () => {
    expect(presetLabel(null)).toBe('Custom');
  });

  // Decision 2: a preset is a complete configuration, so applying one always
  // puts the advanced fields back to their defaults.
  it('presetConfig always carries the advanced defaults', () => {
    for (const preset of PRESETS) {
      const config = presetConfig(preset.id);
      expect(config.parity).toBe(ADVANCED_DEFAULTS.parity);
      expect(config.striping).toBe(ADVANCED_DEFAULTS.striping);
      expect(config.interpolation).toBe(ADVANCED_DEFAULTS.interpolation);
      expect(advancedChanges(config)).toBe(0);
    }
  });
});

describe('playbackPresets — advancedChanges', () => {
  // UX2.5 — all eight advanced combinations, and the two preset-owned fields
  // proven not to count.
  it('counts exactly the deviating advanced fields', () => {
    const cases: [Partial<PlaybackConfig>, number][] = [
      [{}, 0],
      [{ parity: 0 }, 1],
      [{ striping: 'on' }, 1],
      [{ interpolation: false }, 1],
      [{ parity: 1, striping: 'off' }, 2],
      [{ parity: 1, interpolation: false }, 2],
      [{ striping: 'off', interpolation: false }, 2],
      [{ parity: 0, striping: 'on', interpolation: false }, 3],
    ];
    for (const [patch, expected] of cases) {
      expect(advancedChanges({ ...DEFAULT_CONFIG, ...patch })).toBe(expected);
    }
  });

  it('never counts delivery or playout', () => {
    expect(advancedChanges({ ...DEFAULT_CONFIG, delivery: 'deep' })).toBe(0);
    expect(advancedChanges({ ...DEFAULT_CONFIG, playout: 'off' })).toBe(0);
    expect(advancedChanges({ ...DEFAULT_CONFIG, delivery: 'resilient', playout: 'fixed' })).toBe(0);
  });
});

describe('playbackPresets — notApplicable', () => {
  // UX2.6 — all six (field, delivery) pairs.
  it('grays parity and striping under the carrier delivery modes only', () => {
    for (const field of ['parity', 'striping'] as const) {
      expect(notApplicable(field, { ...DEFAULT_CONFIG, delivery: 'live' })).toBeNull();
      for (const delivery of ['resilient', 'deep'] as const) {
        const reason = notApplicable(field, { ...DEFAULT_CONFIG, delivery });
        expect(reason).toBeTruthy();
        expect(reason).toMatch(/not used in this mode/i);
      }
    }
  });

  it('gives parity and striping different reasons — they are not interchangeable', () => {
    const carrier = { ...DEFAULT_CONFIG, delivery: 'resilient' as const };
    expect(notApplicable('parity', carrier)).not.toBe(notApplicable('striping', carrier));
  });

  // UX2.7 — the LIFECYCLE-2 regression guard (docs/24 finding 16). A resilient
  // viewer whose *stored* pacing is 'off' still has interpolation running,
  // because playout.ts resolves carrier delivery to adaptive — so the control
  // must stay live, or the most GPU-expensive viewer feature has no off switch
  // on exactly the phones resilient mode exists for.
  it('gates interpolation on the effective pacing mode, not the stored one', () => {
    expect(
      notApplicable('interpolation', { ...DEFAULT_CONFIG, delivery: 'resilient', playout: 'off' }),
    ).toBeNull();
    expect(
      notApplicable('interpolation', { ...DEFAULT_CONFIG, delivery: 'deep', playout: 'off' }),
    ).toBeNull();
    // Live-edge with pacing off is the one case where it genuinely cannot run.
    expect(
      notApplicable('interpolation', { ...DEFAULT_CONFIG, delivery: 'live', playout: 'off' }),
    ).toBeTruthy();
    expect(notApplicable('interpolation', DEFAULT_CONFIG)).toBeNull();
  });

  it('effectivePlayout mirrors playout.ts’s resolution rule', () => {
    expect(effectivePlayout({ ...DEFAULT_CONFIG, delivery: 'resilient', playout: 'off' })).toBe(
      'adaptive',
    );
    expect(effectivePlayout({ ...DEFAULT_CONFIG, delivery: 'deep', playout: 'fixed' })).toBe(
      'adaptive',
    );
    expect(effectivePlayout({ ...DEFAULT_CONFIG, delivery: 'live', playout: 'off' })).toBe('off');
  });
});
