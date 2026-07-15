// R12 (docs/17 L4): the advanced encoder controls — acceleration tri-state,
// bitrate override, codec pin. Store-backed like LadderPicker; onChange
// hands the full EncoderSettings snapshot to a live session. All three are
// applied via encoder recreate on the next frame — never a stream restart.

import styles from './stream.module.css';
import {
  BITRATE_OVERRIDE_MAX,
  BITRATE_OVERRIDE_MIN,
} from '../../media/ladder';
import type { HwPreference } from '../../media/probe';
import { DEFAULT_CODEC_PREFERENCES } from '../../media/types';
import {
  encoderSettingsFromStore,
  useBroadcastSettingsStore,
} from '../../state/broadcastSettingsStore';
import type { EncoderSettings } from '../../transport/broadcaster';

interface Props {
  onChange?: (settings: EncoderSettings) => void;
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

export function EncoderSettingsPanel({ onChange }: Props) {
  const hwPreference = useBroadcastSettingsStore((s) => s.hwPreference);
  const bitrateOverride = useBroadcastSettingsStore((s) => s.bitrateOverride);
  const codecOverride = useBroadcastSettingsStore((s) => s.codecOverride);
  const setHwPreference = useBroadcastSettingsStore((s) => s.setHwPreference);
  const setBitrateOverride = useBroadcastSettingsStore((s) => s.setBitrateOverride);
  const setCodecOverride = useBroadcastSettingsStore((s) => s.setCodecOverride);

  const emit = () => onChange?.(encoderSettingsFromStore());

  return (
    <div className={styles.ladderPicker}>
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
          {DEFAULT_CODEC_PREFERENCES.map((codec) => (
            <option key={codec} value={codec}>
              {codecLabel(codec)}
            </option>
          ))}
        </select>
      </div>
    </div>
  );
}
