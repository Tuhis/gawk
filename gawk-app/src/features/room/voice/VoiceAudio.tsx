// The Audio section of the voice panel (R80, docs/81 D15, OD3; R81 docs/82):
// noise suppression RNNoise · Browser · Off, the detector's threshold as one
// control over its live meter, Hear yourself and Reset to defaults. Collapsed
// by default; every control applies live (a switch that would need a
// permission prompt waits for the next join, and says so). The threshold and
// noise controls are also the dock's Microphone popover (VoicePanel.tsx).

import { useEffect, useId, useState, type CSSProperties } from 'react';
import styles from './voice.module.css';
import roomStyles from '../room.module.css';
import { useVoiceStore } from './voiceStore';
import type { VoiceActions } from './useRoomVoice';
import type { VoiceMeter } from './voiceController';
import {
  NOISE_MODE_LABEL,
  PTT_NO_THRESHOLD,
  SPEECH_THRESHOLD_HELP,
  THRESHOLD_HELP,
  noiseCostText,
  noiseStatusText,
} from './voiceCopy';
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

type MeterKind = 'speech' | 'level';

// Where a value sits on the meter, 0..1: probability as is, a level on
// -70..0 dBFS.
function meterPosition(kind: MeterKind, v: number): number {
  const x = kind === 'speech' ? v : (v - MIN_LEVEL_THRESHOLD_DB) / (0 - MIN_LEVEL_THRESHOLD_DB);
  return Math.max(0, Math.min(1, x));
}

// The speech detector decides while RNNoise runs; the level gate otherwise.
function useMeterKind(): MeterKind {
  const noise = useVoiceStore((s) => s.noise);
  const noiseMode = useVoiceStore((s) => s.noiseMode);
  const running = noise?.running ?? (noiseMode === 'off' && noise?.offAvailable === false ? 'browser' : noiseMode);
  return running === 'rnnoise' && !noise?.loading ? 'speech' : 'level';
}

// The gate's level floor for this block (docs/82 D7: learned from the
// voice): a block under it counts as silence however high its probability,
// so the bar greys out.
function floorOf(m: VoiceMeter | null): number | null {
  return m?.floorDb ?? null;
}

// The threshold as one control (R81): the bar is the live meter, the slider's
// thumb laid over it is the threshold line, and the lamp says whether the
// block went out. Green past the line is sent; grey is a block too quiet to
// count. The meter polls only while `live` (the section open, the popover up).
export function ThresholdControl({ actions, live }: { actions: VoiceActions; live: boolean }) {
  const s = useVoiceStore();
  const kind = useMeterKind();
  const id = useId();
  const [polled, setPolled] = useState<VoiceMeter | null>(null);
  useEffect(() => {
    if (!live) return;
    const t = setInterval(() => setPolled(actions.meter()), METER_INTERVAL_MS);
    return () => clearInterval(t);
  }, [live, actions]);
  // A closed section shows an empty meter, never a stale reading.
  const m = live ? polled : null;

  const speech = kind === 'speech';
  const value = speech ? s.speechThreshold : s.levelThresholdDb;
  const min = speech ? MIN_SPEECH_THRESHOLD : MIN_LEVEL_THRESHOLD_DB;
  const max = speech ? MAX_SPEECH_THRESHOLD : MAX_LEVEL_THRESHOLD_DB;
  // The slider spans exactly its range's stretch of the meter, so its thumb
  // sits where the threshold falls on the bar.
  const left = meterPosition(kind, min);
  const width = meterPosition(kind, max) - left;
  const reading = speech ? (m?.prob ?? 0) : (m?.levelDb ?? -100);
  const floor = floorOf(m);
  const quiet = m !== null && floor !== null && m.levelDb < floor;
  const tone = quiet ? 'quiet' : m?.open ? 'open' : 'idle';

  return (
    <div className={styles.threshold}>
      <label className={styles.audioLabel} htmlFor={id}>
        {speech ? 'Speech threshold' : 'Level threshold'}
        <span className={styles.audioValue}>{speech ? value.toFixed(2) : `${Math.round(value)} dBFS`}</span>
      </label>
      <div className={styles.meterRow}>
        <div className={styles.track}>
          <div
            className={styles.meter}
            role="meter"
            aria-label={speech ? 'Speech probability' : 'Microphone level'}
            aria-valuemin={speech ? 0 : MIN_LEVEL_THRESHOLD_DB}
            aria-valuemax={speech ? 1 : 0}
            aria-valuenow={speech ? Math.round(reading * 100) / 100 : Math.round(reading)}
            data-tone={tone}
            data-testid="voice-meter"
          >
            <span className={styles.meterFill} style={{ width: `${meterPosition(kind, reading) * 100}%` }} />
          </div>
          <input
            id={id}
            className={styles.thresholdInput}
            type="range"
            min={min}
            max={max}
            step={speech ? 0.01 : 1}
            value={value}
            style={{ left: `${left * 100}%`, width: `${width * 100}%` } as CSSProperties}
            onChange={(e) => {
              const v = Number(e.currentTarget.value);
              if (speech) actions.setSpeechThreshold(v);
              else actions.setLevelThresholdDb(v);
            }}
          />
        </div>
        <span
          className={styles.meterLamp}
          data-open={m?.open ? 'true' : 'false'}
          title={m?.open ? 'Sending' : 'Not sending'}
          data-testid="voice-meter-lamp"
        />
      </div>
      <p className={styles.audioNote}>{speech ? SPEECH_THRESHOLD_HELP : THRESHOLD_HELP}</p>
    </div>
  );
}

// Noise suppression: the choice, what it costs and what is actually running.
export function NoiseControl({ actions }: { actions: VoiceActions }) {
  const s = useVoiceStore();
  const id = useId();
  const noise = s.noise;
  const offAvailable = noise?.offAvailable ?? true;
  const modes: NoiseMode[] = offAvailable ? ['rnnoise', 'browser', 'off'] : ['rnnoise', 'browser'];
  // Off is Browser where the browser has no suppression to turn off.
  const chosen: NoiseMode = !offAvailable && s.noiseMode === 'off' ? 'browser' : s.noiseMode;
  const running = noise?.running ?? chosen;
  const status = noiseStatusText(noise);
  return (
    <>
      <span className={styles.audioLabel} id={id}>
        Noise suppression
      </span>
      <div className={[roomStyles.segment, styles.fill].join(' ')} role="radiogroup" aria-labelledby={id}>
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
    </>
  );
}

export function HearSelf({ actions }: { actions: VoiceActions }) {
  const hearSelf = useVoiceStore((s) => s.hearSelf);
  return (
    <>
      <label className={styles.audioCheck}>
        <input type="checkbox" checked={hearSelf} onChange={(e) => actions.setHearSelf(e.currentTarget.checked)} />
        Hear yourself
      </label>
      {hearSelf && <p className={styles.audioNote}>Use headphones: through speakers, the microphone hears you twice.</p>}
    </>
  );
}

export function AudioSettings({ actions }: { actions: VoiceActions }) {
  const ptt = useVoiceStore((s) => s.talkMode === 'ptt');
  const [open, setOpen] = useState(false);
  return (
    // Uncontrolled: the element owns its open state and onToggle only mirrors
    // it for the meter, so a re-render can never fold a section just opened.
    <details className={styles.audio} onToggle={(e) => setOpen(e.currentTarget.open)} data-testid="voice-audio">
      <summary className={styles.audioSummary}>Audio</summary>
      <div className={styles.audioBody}>
        <NoiseControl actions={actions} />
        {ptt ? <p className={styles.audioNote}>{PTT_NO_THRESHOLD}</p> : <ThresholdControl actions={actions} live={open} />}
        <HearSelf actions={actions} />
        <button type="button" className={`${styles.linkBtn} ${styles.audioReset}`} onClick={actions.resetAudio}>
          Reset to defaults
        </button>
      </div>
    </details>
  );
}
