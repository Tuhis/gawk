// The Audio section of the voice panel (R80, docs/81 D15, OD3): noise
// suppression RNNoise · Browser · Off, the detector's threshold with a live
// meter, Hear yourself and Reset to defaults. Collapsed by default; every
// control applies live (a switch that would need a permission prompt waits
// for the next join, and says so).

import { useEffect, useState } from 'react';
import styles from './voice.module.css';
import roomStyles from '../room.module.css';
import { useVoiceStore } from './voiceStore';
import type { VoiceActions } from './useRoomVoice';
import type { VoiceMeter } from './voiceController';
import { NOISE_MODE_LABEL, noiseCostText, noiseStatusText } from './voiceCopy';
import {
  MAX_LEVEL_THRESHOLD_DB,
  MAX_SPEECH_THRESHOLD,
  MIN_LEVEL_THRESHOLD_DB,
  MIN_SPEECH_THRESHOLD,
  type NoiseMode,
} from './voicePrefs';

// How often the meter reads the last block: smooth enough to watch, cheap
// enough to leave open.
export const METER_INTERVAL_MS = 50;

// Where a value sits on the meter, 0..1.
function meterPosition(kind: 'speech' | 'level', v: number): number {
  const x = kind === 'speech' ? v : (v - MIN_LEVEL_THRESHOLD_DB) / (0 - MIN_LEVEL_THRESHOLD_DB);
  return Math.max(0, Math.min(1, x));
}

function Meter({ kind, threshold, read }: { kind: 'speech' | 'level'; threshold: number; read: () => VoiceMeter | null }) {
  const [m, setM] = useState<VoiceMeter | null>(read);
  useEffect(() => {
    const t = setInterval(() => setM(read()), METER_INTERVAL_MS);
    return () => clearInterval(t);
  }, [read]);
  const value = kind === 'speech' ? (m?.prob ?? 0) : (m?.levelDb ?? -100);
  return (
    <div className={styles.meterRow}>
      <div
        className={styles.meter}
        role="meter"
        aria-label={kind === 'speech' ? 'Speech probability' : 'Microphone level'}
        aria-valuemin={kind === 'speech' ? 0 : MIN_LEVEL_THRESHOLD_DB}
        aria-valuemax={kind === 'speech' ? 1 : 0}
        aria-valuenow={kind === 'speech' ? Math.round(value * 100) / 100 : Math.round(value)}
        data-testid="voice-meter"
      >
        <span className={styles.meterFill} style={{ width: `${meterPosition(kind, value) * 100}%` }} />
        <span className={styles.meterMark} style={{ left: `${meterPosition(kind, threshold) * 100}%` }} />
      </div>
      <span
        className={styles.meterLamp}
        data-open={m?.open ? 'true' : 'false'}
        title={m?.open ? 'Sending' : 'Not sending'}
        data-testid="voice-meter-lamp"
      />
    </div>
  );
}

export function AudioSettings({ actions }: { actions: VoiceActions }) {
  const s = useVoiceStore();
  const [open, setOpen] = useState(false);
  const noise = s.noise;
  const offAvailable = noise?.offAvailable ?? true;
  const modes: NoiseMode[] = offAvailable ? ['rnnoise', 'browser', 'off'] : ['rnnoise', 'browser'];
  // Off is Browser where the browser has no suppression to turn off.
  const chosen: NoiseMode = !offAvailable && s.noiseMode === 'off' ? 'browser' : s.noiseMode;
  const running = noise?.running ?? chosen;
  // The speech detector decides while RNNoise runs; the level gate otherwise.
  const kind: 'speech' | 'level' = running === 'rnnoise' && !noise?.loading ? 'speech' : 'level';
  const status = noiseStatusText(noise);

  return (
    <details className={styles.audio} open={open} onToggle={(e) => setOpen(e.currentTarget.open)} data-testid="voice-audio">
      <summary className={styles.audioSummary}>Audio</summary>
      <div className={styles.audioBody}>
        <span className={styles.audioLabel} id="voice-noise-label">
          Noise suppression
        </span>
        <div className={roomStyles.segment} role="radiogroup" aria-labelledby="voice-noise-label">
          {modes.map((m) => (
            <button
              key={m}
              type="button"
              role="radio"
              aria-checked={chosen === m}
              className={roomStyles.segmentBtn}
              onClick={() => actions.setNoiseMode(m)}
            >
              {NOISE_MODE_LABEL[m]}
            </button>
          ))}
        </div>
        <p className={styles.audioNote}>{noiseCostText(running)}</p>
        {status && (
          <p className={styles.status} role="status" data-tone={noise?.fallback ? 'warn' : undefined}>
            {status}
          </p>
        )}

        <label className={styles.audioLabel} htmlFor="voice-threshold">
          {kind === 'speech' ? 'Speech threshold' : 'Level threshold'}
          <span className={styles.audioValue}>
            {kind === 'speech' ? s.speechThreshold.toFixed(2) : `${Math.round(s.levelThresholdDb)} dBFS`}
          </span>
        </label>
        <input
          id="voice-threshold"
          className={styles.audioSlider}
          type="range"
          min={kind === 'speech' ? MIN_SPEECH_THRESHOLD : MIN_LEVEL_THRESHOLD_DB}
          max={kind === 'speech' ? MAX_SPEECH_THRESHOLD : MAX_LEVEL_THRESHOLD_DB}
          step={kind === 'speech' ? 0.01 : 1}
          value={kind === 'speech' ? s.speechThreshold : s.levelThresholdDb}
          onChange={(e) => {
            const v = Number(e.currentTarget.value);
            if (kind === 'speech') actions.setSpeechThreshold(v);
            else actions.setLevelThresholdDb(v);
          }}
        />
        {open && (
          <Meter kind={kind} threshold={kind === 'speech' ? s.speechThreshold : s.levelThresholdDb} read={actions.meter} />
        )}

        <label className={styles.audioCheck}>
          <input type="checkbox" checked={s.hearSelf} onChange={(e) => actions.setHearSelf(e.currentTarget.checked)} />
          Hear yourself
        </label>
        {s.hearSelf && <p className={styles.audioNote}>Use headphones: through speakers, the microphone hears you twice.</p>}

        <button type="button" className={`${styles.linkBtn} ${styles.audioReset}`} onClick={actions.resetAudio}>
          Reset to defaults
        </button>
      </div>
    </details>
  );
}
