import { StatsPanel, type StatsSection } from '../../ui/StatsPanel';
import { formatHotkey } from '../../lib/useHotkey';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { fmt, fmtBits, fmtInt, fmtOr } from '../../lib/format';
import type { ViewerStats } from '../../transport/viewer';

interface Props {
  stats: ViewerStats | null;
  codec: string | null;
  // Received bitrate derived from the sample window (bits/s); null until two
  // samples with connection byte counters exist.
  bitrateBps: number | null;
  onClose: () => void;
  onCopy: () => void;
  copied: boolean;
}

// The viewer stats overlay (docs/10 J4, extended by R9 M7) — "is it the
// stream or my machine". Sections follow the docs/13 funnel: video (decode),
// delivery (frames arriving/dropping), network (this leg's health).
export function StatsOverlay({ stats, codec, bitrateBps, onClose, onCopy, copied }: Props) {
  const conn = stats?.connection ?? null;
  const sections: StatsSection[] = [
    {
      title: 'Video',
      rows: [
        ['Codec', codec ?? '—'],
        ['Decode mode', stats?.isHardwareAccelerated === true ? 'Hardware' : stats?.isHardwareAccelerated === false ? 'Software' : '—'],
        ['Renderer', stats?.renderer === 'webgl' ? 'WebGL' : stats?.renderer === '2d' ? 'Canvas 2D' : '—'],
        ['Pipeline', stats?.pipelineContext === 'worker' ? 'Worker' : stats?.pipelineContext === 'main-thread' ? 'Main thread' : '—'],
        ['Received fps', fmt(stats?.receivedFps ?? NaN)],
        ['Decoder fps', fmt(stats?.decoderFps ?? NaN)],
        ['Rendered fps', fmtOr(stats?.renderedFps)],
        ['Decoder queue', String(stats?.decoderQueueDepth ?? '—')],
        ['Decode latency', `${fmt(stats?.lastDecodeLatencyMs ?? NaN)} ms`],
        ['Decoded', String(stats?.decodedFrames ?? '—')],
      ],
    },
    {
      title: 'Delivery',
      rows: [
        ['Completed', String(stats?.framesCompleted ?? '—')],
        ['Dropped (incomplete)', String(stats?.framesDroppedIncomplete ?? '—')],
        ['Dropped (late)', String(stats?.framesDroppedLate ?? '—')],
        ['Awaiting keyframe', String(stats?.framesDiscardedAwaitingKey ?? '—')],
        ['Keyframe streams', String(stats?.keyframeStreamsReceived ?? '—')],
        ['Gap resyncs', String(stats?.reorderGapResyncs ?? '—')],
        ['Reorder buffered', String(stats?.reorderBuffered ?? '—')],
        ['Last frame', stats?.timeSinceLastFrameMs == null ? '—' : `${fmtInt(stats.timeSinceLastFrameMs)} ms ago`],
        ['Keyframe age', stats?.lastKeyframeAgeMs == null ? '—' : `${fmtInt(stats.lastKeyframeAgeMs)} ms`],
        // R5 Q1: lag behind this session's best capture→decode delta. ~0 = at
        // live edge; sustained growth = falling behind (see docs/15).
        ['Live-edge drift', stats?.liveEdgeDriftMs == null ? '—' : `${fmtInt(stats.liveEdgeDriftMs)} ms`],
        // R5 Q2: absolute glass-to-glass via the relay clock; "—" until both
        // clock legs (broadcaster + this viewer) have synced.
        ['Latency (capture→render)', stats?.capToRenderMs == null ? '—' : `${fmtInt(stats.capToRenderMs)} ms`],
        // R5 Q3: the playout mode, from the pipeline's own context (ground
        // truth — a toggle that failed to cross the worker shows here).
        ['Playout', stats == null ? '—' : stats.playoutOffsetMs > 0 ? `smoothed (+${fmtInt(stats.playoutOffsetMs)} ms)` : 'live-edge'],
      ],
    },
    {
      title: 'Network',
      rows: [
        ['Transport', stats?.transport === 'worker' ? 'Worker' : stats?.transport === 'in-process' ? 'In-process' : '—'],
        ['RTT', conn?.rttMs == null ? '—' : `${fmt(conn.rttMs)} ms`],
        // R5 Q2: from our own TimeSync ping — independent of getStats(), so it
        // works even though no browser ships getStats() today (docs/13 D7).
        ['RTT (time-sync)', stats?.timeSyncRttMs == null ? '—' : `${fmt(stats.timeSyncRttMs)} ms`],
        ['RTT variation', conn?.rttVarMs == null ? '—' : `${fmt(conn.rttVarMs)} ms`],
        ['Packets lost', fmtInt(conn?.packetsLost)],
        ['Dgrams dropped (in)', fmtInt(conn?.datagramsDroppedIncoming)],
        ['Bitrate (recv)', fmtBits(bitrateBps)],
        ['Datagrams', String(stats?.datagramsReceived ?? '—')],
        ['Bad datagrams', String(stats?.badDatagrams ?? '—')],
      ],
    },
  ];

  return (
    <StatsPanel
      ariaLabel="Stream stats"
      sections={sections}
      footer={`${formatHotkey(STATS_HOTKEY)} or right-click to toggle`}
      onClose={onClose}
      onCopy={onCopy}
      copied={copied}
    />
  );
}
