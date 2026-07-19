// R13 (docs/18 L4): the advanced encoder controls — acceleration tri-state,
// bitrate override, codec pin. Store-backed like LadderPicker; onChange
// hands the full EncoderSettings snapshot to a live session. All three are
// applied via encoder recreate on the next frame — never a stream restart.

import styles from './stream.module.css';
import {
  BITRATE_OVERRIDE_MAX,
  BITRATE_OVERRIDE_MIN,
} from '../../media/ladder';
import type { HwPreference, SupportMatrix } from '../../media/probe';
import { DEFAULT_CODEC_PREFERENCES } from '../../media/types';
import {
  encoderSettingsFromStore,
  useBroadcastSettingsStore,
} from '../../state/broadcastSettingsStore';
import type { EncoderSettings } from '../../transport/broadcaster';
import { annotate, codecAcceleration } from './supportAnnotations';

interface Props {
  onChange?: (settings: EncoderSettings) => void;
  // R13 Decision 9 for the codec pin: per-codec probe matrices backing the
  // option annotations (see useCodecMatrices). null renders unannotated.
  codecMatrices?: Map<string, SupportMatrix> | null;
}

const HW_OPTIONS: Array<{ value: HwPreference; label: string }> = [
  { value: 'auto', label: 'auto (prefer hardware)' },
  { value: 'hardware', label: 'hardware only' },
  { value: 'software', label: 'software only' },
];

function codecLabel(codec: string): string {
  if (codec.startsWith('avc1')) return `H.264 · ${codec}`;
  if (codec.startsWith('vp09')) return `VP9 · ${codec}`;
  if (codec === 'vp8') return 'VP8';
  return codec;
}

export function EncoderSettingsPanel({ onChange, codecMatrices }: Props) {
  const hwPreference = useBroadcastSettingsStore((s) => s.hwPreference);
  const bitrateOverride = useBroadcastSettingsStore((s) => s.bitrateOverride);
  const codecOverride = useBroadcastSettingsStore((s) => s.codecOverride);
  const audioEnabled = useBroadcastSettingsStore((s) => s.audioEnabled);
  const setAudioEnabled = useBroadcastSettingsStore((s) => s.setAudioEnabled);
  // The codec annotations answer "what would pinning this codec get at the
  // *current* resolution/fps selections" — so they follow the pickers live.
  const resolutionSelection = useBroadcastSettingsStore((s) => s.resolutionSelection);
  const framerateSelection = useBroadcastSettingsStore((s) => s.framerateSelection);
  const setHwPreference = useBroadcastSettingsStore((s) => s.setHwPreference);
  const setBitrateOverride = useBroadcastSettingsStore((s) => s.setBitrateOverride);
  const setCodecOverride = useBroadcastSettingsStore((s) => s.setCodecOverride);

  const emit = () => onChange?.(encoderSettingsFromStore());

  // Stacked, full-width (like the dev settings) — three fields with full
  // codec strings overflow the side panel as a row.
  return (
    <div className={styles.stackedPicker}>
      <div className={styles.field}>
        <label htmlFor="hw-preference">Acceleration</label>
        <select
          id="hw-preference"
          value={hwPreference}
          onChange={(e) => {
            setHwPreference(e.target.value as HwPreference);
            emit();
          }}
        >
          {HW_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      </div>
      <div className={styles.field}>
        <label htmlFor="bitrate-override">Bitrate (Mbps, empty = auto)</label>
        <input
          id="bitrate-override"
          type="number"
          min={BITRATE_OVERRIDE_MIN / 1e6}
          max={BITRATE_OVERRIDE_MAX / 1e6}
          step={0.5}
          placeholder="auto"
          value={bitrateOverride === null ? '' : bitrateOverride / 1e6}
          onChange={(e) => {
            const v = e.target.value.trim();
            const mbps = Number(v);
            setBitrateOverride(v === '' || !Number.isFinite(mbps) || mbps <= 0 ? null : mbps * 1e6);
            emit();
          }}
        />
      </div>
      <div className={styles.field}>
        <label htmlFor="codec-override">Codec</label>
        <select
          id="codec-override"
          value={codecOverride ?? 'auto'}
          onChange={(e) => {
            setCodecOverride(e.target.value === 'auto' ? null : e.target.value);
            emit();
          }}
        >
          <option value="auto">auto (negotiate)</option>
          {DEFAULT_CODEC_PREFERENCES.map((codec) => {
            const { label, disabled } = annotate(
              codecLabel(codec),
              codecAcceleration(codecMatrices?.get(codec), resolutionSelection, framerateSelection),
            );
            return (
              <option key={codec} value={codec} disabled={disabled}>
                {label}
              </option>
            );
          })}
        </select>
      </div>
      {/* R15 (docs/20): not part of EncoderSettings — the toggle is read at
          broadcast start (getDisplayMedia can't add an audio track without
          re-prompting), so no onChange emit. */}
      <div className={styles.checkboxField}>
        <label htmlFor="audio-enabled">
          <input
            id="audio-enabled"
            type="checkbox"
            checked={audioEnabled}
            onChange={(e) => setAudioEnabled(e.target.checked)}
          />
          Enable audio (experimental)
        </label>
        {/* Not decoration: when the browser can't start a system-audio source
            it refuses the whole grant, video included (capture.ts retries
            video-only). Sharing a tab is the path that works everywhere. */}
        <span className={styles.fieldNote}>
          Applies when the broadcast starts. If the browser can't capture system audio, the
          broadcast continues video-only — sharing a tab with its audio is the reliable path.
        </span>
      </div>
    </div>
  );
}
