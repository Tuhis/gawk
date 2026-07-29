// R32 (docs/37): the viewer's playback presets — the model, the copy, and the
// not-applicable rules, in one pure module so all of it is unit-testable
// without React and no surface can inline a second copy of a label
// (CODE-REVIEW "one definition, one home"; the docs/30 decision-8 precedent).
//
// The load-bearing observation (docs/37 §3): the viewer's four tuning controls
// are not four independent choices, and two of them are not even on the same
// axis.
//
//   delivery mode (R19/R21) ─┐
//   paced playback (R12)    ─┴─ latency: live → ~0.5 s → seconds behind
//
//   loss protection (R29)   ─┐
//   striping (R30)          ─┴─ robustness: costs data / connections,
//                               and **zero latency**
//
// So a preset governs delivery + pacing. Turning parity or striping off does
// not make anything faster — it makes it cheaper and more fragile — which is
// why an early sketch that folded them into a "Lowest latency" preset was
// rejected. They are Advanced, they sit at their defaults, and they are
// exactly what "Custom" tracks.

import type { PlayoutMode } from '../../transport/playout';
import type { ViewerDeliveryMode } from '../../transport/resilient';
import type { StripeMode } from '../../transport/stripe';

// R29's opt-DOWN from the fleet parity default. 'auto' means "take what the
// fleet serves", which is the ceiling — a viewer cannot ask for more parity
// than the producer emitted.
export type ParityChoice = 'auto' | 1 | 0;

export type PresetId = 'lowest' | 'balanced' | 'smoother' | 'stable';

/** Everything a preset governs, plus everything it leaves at a default. */
export interface PlaybackConfig {
  delivery: ViewerDeliveryMode;
  playout: PlayoutMode;
  parity: ParityChoice;
  striping: StripeMode;
  interpolation: boolean;
}

/** The advanced fields, shared by every preset (docs/37 decision 2). */
export const ADVANCED_DEFAULTS = {
  parity: 'auto',
  striping: 'auto',
  interpolation: true,
} as const satisfies Pick<PlaybackConfig, 'parity' | 'striping' | 'interpolation'>;

export interface Preset {
  id: PresetId;
  label: string;
  /** The outcome, in the viewer's terms — the cost IS the choice. */
  sub: string;
  delivery: ViewerDeliveryMode;
  playout: PlayoutMode;
}

// One axis, ordered by increasing delay. `balanced` is defined to be today's
// shipping default (R12's adaptive pacing + interpolation, live-edge
// delivery), so installing R32 and touching nothing changes no behaviour.
export const PRESETS: readonly Preset[] = [
  {
    id: 'lowest',
    label: 'Lowest latency',
    sub: 'least delay — can judder',
    delivery: 'live',
    playout: 'off',
  },
  {
    id: 'balanced',
    label: 'Balanced',
    sub: 'smooth, ~0.2 s behind — default',
    delivery: 'live',
    playout: 'adaptive',
  },
  {
    id: 'smoother',
    label: 'Smoother',
    sub: 'for mobile networks, ~0.5 s behind',
    delivery: 'resilient',
    playout: 'adaptive',
  },
  {
    id: 'stable',
    label: 'Most stable',
    sub: 'rides out dropouts, seconds behind',
    delivery: 'deep',
    playout: 'adaptive',
  },
] as const;

export const DEFAULT_PRESET: PresetId = 'balanced';

/** Shown on the pill and as a checked, inert row when nothing else matches. */
export const CUSTOM_LABEL = 'Custom';

// Changing delivery or parity re-dials: both are in useViewerConnection's
// session-effect dependency array, because delivery is negotiated at subscribe
// time. Pacing, striping and interpolation cross into the live pipeline
// instead and never reconnect. Disclosed on exactly those controls — an
// annotation on everything would say nothing (docs/37 decision 7).
export const RECONNECT_NOTE = '· switching reconnects';

/** The complete configuration a preset applies. */
export function presetConfig(id: PresetId): PlaybackConfig {
  const preset = PRESETS.find((p) => p.id === id) ?? PRESETS[1]!;
  return {
    delivery: preset.delivery,
    playout: preset.playout,
    ...ADVANCED_DEFAULTS,
  };
}

/**
 * The preset this configuration *is*, or null for Custom.
 *
 * Deliberately an exact match on all five fields rather than a nearest-preset
 * snap: a legacy R19-era viewer (resilient delivery, pacing stored off) and a
 * dev build's 'fixed' diagnostic both land on Custom, which is honest, where
 * snapping would silently relabel a state as something it is not.
 */
export function resolvePreset(config: PlaybackConfig): PresetId | null {
  const advancedDefault =
    config.parity === ADVANCED_DEFAULTS.parity &&
    config.striping === ADVANCED_DEFAULTS.striping &&
    config.interpolation === ADVANCED_DEFAULTS.interpolation;
  if (!advancedDefault) return null;
  return (
    PRESETS.find((p) => p.delivery === config.delivery && p.playout === config.playout)?.id ?? null
  );
}

/** The label for the pill: a preset's name, or Custom. */
export function presetLabel(id: PresetId | null): string {
  return PRESETS.find((p) => p.id === id)?.label ?? CUSTOM_LABEL;
}

/**
 * How many advanced fields differ from their defaults. Drives the disclosure's
 * "· N changed" marker and whether Reset advanced does anything. Counts only
 * advanced fields — delivery and playout are the preset's own business.
 */
export function advancedChanges(config: PlaybackConfig): number {
  let n = 0;
  if (config.parity !== ADVANCED_DEFAULTS.parity) n++;
  if (config.striping !== ADVANCED_DEFAULTS.striping) n++;
  if (config.interpolation !== ADVANCED_DEFAULTS.interpolation) n++;
  return n;
}

export type AdvancedField = 'parity' | 'striping' | 'interpolation';

/**
 * Whether an advanced control applies right now, and if not, why — one
 * function, so a surface cannot gray something out without saying why
 * (docs/37 decision 4). `null` means applicable.
 *
 * Both delivery rules restate what the pipeline already does: R29 serves
 * parity only to live-edge subscribers and R30 stripes only datagram
 * delivery, because the carrier modes recover loss by retransmission. Before
 * R32 these controls were *removed* from the menu in those modes, so the menu
 * changed length with the delivery mode and a viewer who had seen "Loss
 * protection" once could not find it again (docs/37 §1.2).
 */
export function notApplicable(field: AdvancedField, config: PlaybackConfig): string | null {
  if (field === 'interpolation') {
    // The *effective* pacing mode, never the stored one: R19 delivery implies
    // adaptive pacing inside playout.ts, so a resilient viewer whose stored
    // mode is 'off' does have interpolation running and must keep the control
    // that turns it off. That is review finding LIFECYCLE-2 (docs/24 finding
    // 16) and it must not regress through this move.
    return effectivePlayout(config) === 'adaptive'
      ? null
      : 'Needs paced playback — available on Balanced and smoother.';
  }
  if (config.delivery === 'live') return null;
  return field === 'parity'
    ? 'Not used in this mode — it already recovers lost packets by resending them.'
    : 'Not used in this mode — reliable delivery already handles bursts.';
}

/**
 * The pacing mode actually in force. Mirrors playout.ts's resolution rule:
 * any carrier delivery mode implies adaptive regardless of the stored value.
 */
export function effectivePlayout(config: PlaybackConfig): PlayoutMode {
  return config.delivery !== 'live' ? 'adaptive' : config.playout;
}

// ── Advanced control vocabularies ────────────────────────────────────────────
// The options and their costs, beside the model they belong to.

export const PARITY_OPTIONS: readonly { value: ParityChoice; label: string; sub: string }[] = [
  { value: 'auto', label: 'Full', sub: 'repairs lost packets, ~22% more data' },
  { value: 1, label: 'Light', sub: '~11% more data' },
  { value: 0, label: 'Off', sub: 'least data, freezes on loss' },
];

export const STRIPE_OPTIONS: readonly { value: StripeMode; label: string; sub: string }[] = [
  { value: 'auto', label: 'Auto', sub: 'splits bursts when loss is detected' },
  { value: 'on', label: 'On', sub: 'extra connections, loss or not' },
  { value: 'off', label: 'Off', sub: 'one connection' },
];
