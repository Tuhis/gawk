import { StatsPanel, type StatsRow, type StatsSection } from '../../ui/StatsPanel';
import { formatHotkey } from '../../lib/useHotkey';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { fmt, fmtBits, fmtInt } from '../../lib/format';
import type { BroadcastStats } from '../../transport/broadcaster';
import type { EncoderConfigured } from '../../media/encoder';
import { OVERLAY_UNSUPPORTED } from './captureGuidance';

interface Props {
  stats: BroadcastStats | null;
  encoderInfo: EncoderConfigured | null;
  // Sent video bitrate derived from the sample window (bits/s).
  bitrateBps: number | null;
  // R24 (docs/30 CG4): whether this browser can do audio at all. When false, the
  // Audio State row reads the honest "Not supported here" instead of the raw
  // audioState ('no-track' → the misleading "No audio shared"). Optional so the
  // frozen #/debug overlay and older call sites are unaffected.
  audioSupported?: boolean;
  onClose: () => void;
  onCopy: () => void;
  copied: boolean;
}

// The broadcaster stats overlay (R9 M7, docs/13): the counterpart of the
// viewer's — the encode funnel (capture → post-gate → encoded → sent) plus
// this leg's connection health, so "is it my machine or my uplink" is
// answerable without leaving the stream.
export function BroadcasterStatsOverlay({ stats, encoderInfo, bitrateBps, audioSupported = true, onClose, onCopy, copied }: Props) {
  const conn = stats?.connection ?? null;
  const sections: StatsSection[] = [
    {
      title: 'Video',
      rows: [
        ['Codec', encoderInfo?.codec ?? '—'],
        ['Encoding', encoderInfo ? `${encoderInfo.width}×${encoderInfo.height} @ ${fmt(encoderInfo.framerate, 0)}` : '—'],
        // Runtime truth (docs/18 Decision 13): what configure() actually
        // landed on, not what the probe matrix predicted.
        ['Encode mode', encoderInfo?.acceleration ?? '—'],
        // R13: the probe-resolved auto targets ('—' on explicit selections).
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
        // R18: the relay's live audience push (fleet-global in cluster mode).
        ['Watching', stats?.viewerCount == null ? '—' : String(stats.viewerCount)],
        ['Encoded', String(stats?.encodedFrames ?? '—')],
        ['Keyframes', String(stats?.keyframes ?? '—')],
        ['Datagrams sent', String(stats?.datagramsSent ?? '—')],
        ['Keyframe streams', String(stats?.keyframeStreamsSent ?? '—')],
        ['Keyframe fails', String(stats?.keyframeStreamsFailed ?? '—')],
        ['Video bitrate (sent)', fmtBits(bitrateBps)],
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
    // R15 (docs/20 N6): the Audio section — present only when the toggle
    // asked for audio, so a video-only broadcast's overlay is unchanged.
    // 'no-track'/'unsupported' are states worth seeing: they explain a silent
    // stream without leaving the broadcast.
    ...(stats && stats.audioState !== 'off'
      ? [
          {
            title: 'Audio',
            rows: [
              [
                'State',
                // R24 (docs/30 CG4): a browser that can't do audio reads the
                // honest "Not supported here" whatever the raw state (Firefox
                // lands on 'no-track'/'unavailable', which would otherwise read
                // "No audio shared" — as if a checkbox would fix it).
                !audioSupported
                  ? OVERLAY_UNSUPPORTED
                  : stats.audioState === 'active'
                    ? 'Active'
                    : stats.audioState === 'no-track'
                      ? 'No audio shared'
                      : stats.audioState === 'unavailable'
                        ? 'Not capturable here'
                        : stats.audioState === 'unsupported'
                          ? 'Unsupported here'
                          : 'Error',
              ],
              [
                'Format',
                stats.audioCodec
                  ? `${stats.audioCodec} · ${stats.audioSampleRate ?? '—'} Hz · ${stats.audioChannels ?? '—'}ch · ${fmtBits(stats.audioBitrateBps)}`
                  : '—',
              ],
              ['Encoded/s', fmt(stats.audioEncodedPerSec)],
              ['Sent/s', fmt(stats.audioSentPerSec)],
              ['Packets sent', String(stats.audioPacketsSent)],
              ['Configs sent', String(stats.audioConfigsSent)],
              // docs/20 field finding 13: the encoder's own delay, which used
              // to be written into every audio timestamp and is now measured
              // instead. It costs no lip sync as long as it stays out of the
              // stamps — this row is what says whether it did. Re-anchors are
              // beside it because each one steps the audio timeline.
              [
                'Encode lag',
                stats.audioEncodeLagMs == null
                  ? '—'
                  : `${fmt(stats.audioEncodeLagMs)} ms${
                      stats.audioAnchorReanchors > 0
                        ? ` · ${stats.audioAnchorReanchors} re-anchor${stats.audioAnchorReanchors === 1 ? '' : 's'}`
                        : ''
                    }`,
              ],
            ] as StatsRow[],
          },
        ]
      : []),
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
