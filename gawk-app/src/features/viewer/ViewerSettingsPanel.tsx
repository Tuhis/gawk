import { useId, useState } from 'react';
import styles from './viewer.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { Button } from '../../ui/Button';
import {
  PARITY_OPTIONS,
  PRESETS,
  RECONNECT_NOTE,
  STRIPE_OPTIONS,
  advancedChanges,
  notApplicable,
  resolvePreset,
  type ParityChoice,
  type PlaybackConfig,
  type PresetId,
} from './playbackPresets';
import type { StripeMode } from '../../transport/stripe';

interface Props {
  config: PlaybackConfig;
  /**
   * Whether the pipeline can interpolate: `true`/`false` once it has reported,
   * `null` while nothing is known yet. The three states stay apart because
   * "not yet" and "never" need different words — collapsing them would tell a
   * healthy viewer, for the first second of every session, that its browser
   * cannot do something it can.
   */
  interpolationAvailable: boolean | null;
  onPreset: (id: PresetId) => void;
  onParity: (value: ParityChoice) => void;
  onStriping: (value: StripeMode) => void;
  onInterpolation: (value: boolean) => void;
  onResetAdvanced: () => void;
  onClose: () => void;
}

// R32 UX3 (docs/37 §6.3). The viewer's settings surface, deliberately the same
// shape as the broadcaster's (scrim + right-side GlassPanel + uppercase group
// titles) so the product has one settings idiom rather than two.
//
// Rendered by ViewerScreen *inside* the viewer root, never portalled to
// document.body: in CSS pseudo-fullscreen — the shipping iPhone tier since
// docs/21 U4 — the fullscreen element IS that root, so anything outside it is
// invisible. Same for desktop element fullscreen. (docs/37 decision 5.)
export function ViewerSettingsPanel({
  config,
  interpolationAvailable,
  onPreset,
  onParity,
  onStriping,
  onInterpolation,
  onResetAdvanced,
  onClose,
}: Props) {
  // Collapsed by default: the whole point of R32 is that an average viewer
  // never opens this. Controlled rather than a native <details> so it matches
  // the broadcaster's animated reveal.
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const groupName = useId();
  const current = resolvePreset(config);
  const changed = advancedChanges(config);

  const parityReason = notApplicable('parity', config);
  const stripingReason = notApplicable('striping', config);
  // Two independent reasons an interpolation toggle can be inert, and the
  // more specific one wins: "your pacing mode has no mid-frames to make" is
  // actionable, "this pipeline can't interpolate" is not.
  const interpolationReason =
    notApplicable('interpolation', config) ??
    (interpolationAvailable === null
      ? 'Available once the stream is running.'
      : interpolationAvailable
        ? null
        : 'Not available — this viewer pipeline can’t interpolate.');

  return (
    <>
      <div className={styles.settingsScrim} onClick={onClose} />
      <GlassPanel className={styles.settings}>
        <div className={styles.settingsHead}>
          <span>Playback</span>
          <Button variant="ghost" onClick={onClose}>
            Done
          </Button>
        </div>

        <section className={styles.settingsGroup}>
          <h3 className={styles.settingsGroupTitle}>Playback</h3>
          {PRESETS.map((preset) => (
            <label key={preset.id} className={styles.presetRow}>
              <input
                type="radio"
                name={groupName}
                checked={current === preset.id}
                onChange={() => onPreset(preset.id)}
              />
              <span className={styles.presetText}>
                <span className={styles.presetLabel}>{preset.label}</span>
                <span className={styles.presetSub}>
                  {preset.sub}
                  {/* Only the controls that actually re-dial say so, or the
                      disclosure means nothing (docs/37 decision 7). A carrier
                      preset is reached by renegotiating the subscription; the
                      pacing-only step between Lowest latency and Balanced is
                      not. */}
                  {preset.delivery !== config.delivery && (
                    <span className={styles.reconnectNote}> {RECONNECT_NOTE}</span>
                  )}
                </span>
              </span>
            </label>
          ))}
          {current === null && (
            <p className={styles.customNote}>
              Your settings don’t match a preset. Pick one above to go back to a standard
              configuration.
            </p>
          )}
        </section>

        <section className={styles.settingsGroup}>
          <button
            type="button"
            className={styles.disclosure}
            aria-expanded={advancedOpen}
            onClick={() => setAdvancedOpen((o) => !o)}
          >
            <span className={styles.settingsGroupTitle}>Advanced</span>
            <span className={styles.disclosureMeta}>
              {changed > 0 && <span className={styles.changedBadge}>· {changed} changed</span>}
              <span aria-hidden="true">{advancedOpen ? '⌃' : '⌄'}</span>
            </span>
          </button>

          {advancedOpen && (
            <div className={styles.advanced}>
              <AdvancedSelect
                label="Loss protection"
                value={String(config.parity)}
                reason={parityReason}
                note={parityReason ? null : `${optionSub(PARITY_OPTIONS, config.parity)} ${RECONNECT_NOTE}`}
                options={PARITY_OPTIONS.map((o) => ({ value: String(o.value), label: o.label }))}
                onChange={(v) => onParity(v === 'auto' ? 'auto' : (Number(v) as 0 | 1))}
              />
              <AdvancedSelect
                label="Striping"
                value={config.striping}
                reason={stripingReason}
                note={stripingReason ? null : optionSub(STRIPE_OPTIONS, config.striping)}
                options={STRIPE_OPTIONS.map((o) => ({ value: o.value, label: o.label }))}
                onChange={(v) => onStriping(v as StripeMode)}
              />

              <label
                className={[styles.advancedRow, interpolationReason ? styles.rowDisabled : '']
                  .filter(Boolean)
                  .join(' ')}
              >
                {/* A checkbox row groups its control with its label; the
                    select rows push label and control apart. Two layouts, so
                    the shared `advancedHead` justify-between doesn't strand a
                    checkbox on the far side of the panel from its own text. */}
                <span className={styles.advancedCheck}>
                  <input
                    type="checkbox"
                    checked={config.interpolation}
                    disabled={interpolationReason != null}
                    onChange={(e) => onInterpolation(e.target.checked)}
                  />
                  <span className={styles.advancedLabel}>Frame interpolation (experimental)</span>
                </span>
                <span className={styles.advancedSub}>
                  {interpolationReason ?? 'Synthesizes in-between frames for smoother motion.'}
                </span>
              </label>

              <div className={styles.advancedActions}>
                <Button variant="ghost" disabled={changed === 0} onClick={onResetAdvanced}>
                  Reset advanced
                </Button>
              </div>
            </div>
          )}
        </section>

        {/* R23 (docs/29): terms reachable from the viewer's settings, matching
            the broadcaster panel's footer. A new tab, so reading them never
            tears down the live stream. */}
        <div className={styles.settingsFoot}>
          <a
            href={`${window.location.origin}${window.location.pathname}#/terms`}
            target="_blank"
            rel="noopener"
          >
            Terms of use
          </a>
        </div>
      </GlassPanel>
    </>
  );
}

function optionSub<T>(options: readonly { value: T; sub: string }[], value: T): string {
  return options.find((o) => o.value === value)?.sub ?? '';
}

// A labelled select with its cost (or its not-applicable reason) below. One
// component for both advanced pickers so the disabled presentation cannot
// drift between them.
function AdvancedSelect({
  label,
  value,
  options,
  reason,
  note,
  onChange,
}: {
  label: string;
  value: string;
  options: readonly { value: string; label: string }[];
  reason: string | null;
  note: string | null;
  onChange: (value: string) => void;
}) {
  return (
    <label
      className={[styles.advancedRow, reason ? styles.rowDisabled : ''].filter(Boolean).join(' ')}
    >
      <span className={styles.advancedHead}>
        <span className={styles.advancedLabel}>{label}</span>
        <select
          className={styles.advancedSelect}
          value={value}
          disabled={reason != null}
          onChange={(e) => onChange(e.target.value)}
        >
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      </span>
      <span className={styles.advancedSub}>{reason ?? note}</span>
    </label>
  );
}
