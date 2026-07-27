import type { SessionView } from '../api/types.ts';
import { coverageMs, WINDOW_MS, type SessionHistory } from '../lib/history.ts';
import { dur } from '../lib/format.ts';
import { TimelineChart, type SeriesSpec } from './TimelineChart.tsx';
import styles from './SessionTimeline.module.css';

// What each role's graphs are FOR, which is why the pairs differ:
//
//  * A viewer's question is "what is this person actually seeing?" — so the
//    delivery funnel (received -> decoded -> rendered) and then the experience
//    itself (end-to-end latency, and how long since a frame arrived).
//  * A broadcaster's is "is the send path keeping up with what it promised?" —
//    so the capture -> encode -> sent funnel against the configured target,
//    with the encoder queue as the pressure signal.

const VIEWER_FPS: SeriesSpec[] = [
  { key: 'receivedFps', label: 'received', color: 'var(--accent)' },
  { key: 'decoderFps', label: 'decoded', color: 'var(--ok)' },
  { key: 'renderedFps', label: 'rendered', color: 'var(--warn)' },
];

const VIEWER_EXPERIENCE: SeriesSpec[] = [
  { key: 'capToRenderMs', label: 'capture→render', color: 'var(--accent)' },
  { key: 'timeSinceLastFrameMs', label: 'since frame', color: 'var(--bad)' },
  { key: 'playoutOffsetMs', label: 'playout', color: 'var(--text-dimmer)', dashed: true },
];

const BROADCASTER_FPS: SeriesSpec[] = [
  { key: 'captureFps', label: 'capture', color: 'var(--accent)' },
  { key: 'encoderFps', label: 'encode', color: 'var(--ok)' },
  { key: 'sentFps', label: 'sent', color: 'var(--warn)' },
  // Dashed, because a target is a REFERENCE, not a measurement. D17's whole
  // point is that a rate is meaningless until you can see what it was asked for.
  { key: 'targetFps', label: 'target', color: 'var(--text-dimmer)', dashed: true },
];

const BROADCASTER_PRESSURE: SeriesSpec[] = [
  { key: 'encoderQueueDepth', label: 'encoder queue', color: 'var(--bad)' },
  { key: 'viewerCount', label: 'viewers', color: 'var(--text-dimmer)', dashed: true },
];

interface Props {
  session: SessionView;
  history: SessionHistory | undefined;
}

export function SessionTimeline({ session, history }: Props) {
  const points = history?.points ?? [];
  const covered = coverageMs(history);
  const isBroadcaster = session.role === 'broadcaster';

  return (
    <div className={styles.wrap}>
      {/*
        History is accumulated by this page from the polls it already makes —
        `/live` carries no series and the stored timeline only exists once a
        session has ENDED. So a fresh tab genuinely knows nothing yet, and says
        so rather than drawing a stub line as if it were the whole picture.
      */}
      <div className={styles.note}>
        {points.length < 2
          ? 'Collecting — the timeline starts when this page is opened.'
          : covered < WINDOW_MS
            ? `Showing ${dur(covered)} of ${dur(WINDOW_MS)} — history starts when the page is opened.`
            : `Last ${dur(WINDOW_MS)}.`}
      </div>
      <div className={styles.grid}>
        <TimelineChart
          title="Frame rate"
          unit="fps"
          points={points}
          series={isBroadcaster ? BROADCASTER_FPS : VIEWER_FPS}
          windowMs={WINDOW_MS}
          minTop={30}
        />
        <TimelineChart
          title={isBroadcaster ? 'Encoder pressure' : 'Experience'}
          unit={isBroadcaster ? 'frames / viewers' : 'ms'}
          points={points}
          series={isBroadcaster ? BROADCASTER_PRESSURE : VIEWER_EXPERIENCE}
          windowMs={WINDOW_MS}
          minTop={isBroadcaster ? 4 : 200}
        />
      </div>
    </div>
  );
}
