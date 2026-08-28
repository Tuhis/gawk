import { EncoderSettingsPanel, useBroadcastSettingsStore } from 'gawk-app';

// The advanced encoder controls (R13): acceleration tri-state, bitrate
// override, codec pin. All three apply via encoder recreate on the next frame
// — never a stream restart.
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
  maxWidth: '680px',
};

const mk = (acceleration: string) =>
  ({
    source: { width: 2560, height: 1440 },
    hwPreference: 'auto',
    entries: new Map(),
    get: () => ({ acceleration, codec: 'avc1.42E02A' }),
  }) as never;

// Per-codec matrices backing the codec-pin annotations: H.264 baseline and
// high profile are hardware here, VP9 and VP8 are software only.
const codecMatrices = new Map<string, never>([
  ['avc1.42E02A', mk('hardware')],
  ['avc1.640028', mk('hardware')],
  ['avc1.42E01F', mk('hardware')],
  ['vp09.00.40.08', mk('software')],
  ['vp09.00.31.08', mk('software')],
  ['vp8', mk('software')],
]);

function withSettings(patch: Record<string, unknown>) {
  useBroadcastSettingsStore.setState(patch as never);
}

/** Defaults: acceleration auto, no bitrate override, no codec pin. */
export const Defaults = () => {
  withSettings({ hwPreference: 'auto', bitrateOverride: null, codecOverride: null });
  return <div style={canvas}><EncoderSettingsPanel codecMatrices={codecMatrices as never} /></div>;
};

/** Every advanced control moved off its default — hardware-only, pinned codec, bitrate override. */
export const FullyOverridden = () => {
  withSettings({ hwPreference: 'hardware', bitrateOverride: 12_000_000, codecOverride: 'avc1.640028' });
  return <div style={canvas}><EncoderSettingsPanel codecMatrices={codecMatrices as never} /></div>;
};

/** Software-only, with no probe matrices — the options render unannotated. */
export const SoftwareOnlyUnannotated = () => {
  withSettings({ hwPreference: 'software', bitrateOverride: null, codecOverride: null });
  return <div style={canvas}><EncoderSettingsPanel codecMatrices={null} /></div>;
};
