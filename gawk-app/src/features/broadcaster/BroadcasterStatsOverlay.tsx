import { StatsPanel, type StatsSection } from '../../ui/StatsPanel';
import { formatHotkey } from '../../lib/useHotkey';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { fmt, fmtBits, fmtInt } from '../../lib/format';
import type { BroadcastStats } from '../../transport/broadcaster';
import type { EncoderConfigured } from '../../media/encoder';

interface Props {
  stats: BroadcastStats | null;
  encoderInfo: EncoderConfigured | null;
  // Sent bitrate derived from the sample window (bits/s).
  bitrateBps: number | null;
  onClose: () => void;
  onCopy: () => void;
  copied: boolean;
}

// The broadcaster stats overlay (R9 M7, docs/13): the counterpart of the
// viewer's — the encode funnel (capture → post-gate → encoded → sent) plus
// this leg's connection health, so "is it my machine or my uplink" is
// answerable without leaving the stream.
export function BroadcasterStatsOverlay({ stats, encoderInfo, bitrateBps, onClose, onCopy, copied }: Props) {
  const conn = stats?.connection ?? null;
  const sections: StatsSection[] = [
    {
      title: 'Video',
      rows: [
        ['Codec', encoderInfo?.codec ?? '—'],
        ['Encoding', encoderInfo ? `${encoderInfo.width}×${encoderInfo.height} @ ${fmt(encoderInfo.framerate, 0)}` : '—'],
        // Runtime truth (docs/17 Decision 13): what configure() actually
        // landed on, not what the probe matrix predicted.
        ['Encode mode', encoderInfo?.acceleration ?? '—'],
        // R12: the probe-resolved auto targets ('—' on explicit selections).
        ['Auto ceiling', stats?.autoCeiling == null ? '—' : stats.autoCeiling === 'native' ? 'native' : `${stats.autoCeiling}p`],
        ['Auto fps', stats?.autoFps == null ? '—' : String(stats.autoFps)],
        ['Pipeline', stats?.pipelineContext === 'worker' ? 'Worker' : stats?.pipelineContext === 'main-thread' ? 'Main thread' : '—'],
        ['Capture fps', fmt(stats?.captureFps ?? NaN)],
        ['Encoder fps', fmt(stats?.encoderFps ?? NaN)],
        ['Sent fps', fmt(stats?.sentFps ?? NaN)],
        ['Gate dropped', String(stats?.fpsGateDropped ?? '—')],
        ['Dropped (encoder)', String(stats?.droppedFrames ?? '—')],
        ['Encoder queue', String(stats?.encoderQueueDepth ?? '—')],
        ['Encode latency', `${fmt(stats?.lastEncodeLatencyMs ?? NaN)} ms`],
      ],
    },
    {
      title: 'Delivery',
      rows: [
        ['Encoded', String(stats?.encodedFrames ?? '—')],
        ['Keyframes', String(stats?.keyframes ?? '—')],
        ['Datagrams sent', String(stats?.datagramsSent ?? '—')],
        ['Keyframe streams', String(stats?.keyframeStreamsSent ?? '—')],
        ['Keyframe fails', String(stats?.keyframeStreamsFailed ?? '—')],
        ['Bitrate (sent)', fmtBits(bitrateBps)],
      ],
    },
    {
      title: 'Network',
      rows: [
        ['RTT', conn?.rttMs == null ? '—' : `${fmt(conn.rttMs)} ms`],
        // R5 Q2: from our own TimeSync ping — independent of getStats(), so it
        // works even though no browser ships getStats() today (docs/13 D7).
        ['RTT (time-sync)', stats?.timeSyncRttMs == null ? '—' : `${fmt(stats.timeSyncRttMs)} ms`],
        ['RTT variation', conn?.rttVarMs == null ? '—' : `${fmt(conn.rttVarMs)} ms`],
        ['Packets lost', fmtInt(conn?.packetsLost)],
        ['Send rate (est.)', fmtBits(conn?.estimatedSendRateBps)],
        ['At capacity', conn?.atSendCapacity == null ? '—' : conn.atSendCapacity ? 'yes' : 'no'],
        ['Dgrams expired (out)', fmtInt(conn?.datagramsExpiredOutgoing)],
        ['Dgrams lost (out)', fmtInt(conn?.datagramsLostOutgoing)],
      ],
    },
  ];

  return (
    <StatsPanel
      ariaLabel="Broadcast stats"
      sections={sections}
      footer={`${formatHotkey(STATS_HOTKEY)} to toggle`}
      onClose={onClose}
      onCopy={onCopy}
      copied={copied}
    />
  );
}
