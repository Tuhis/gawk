import { StatsPanel, type StatsRow, type StatsSection } from '../../ui/StatsPanel';
import { formatHotkey } from '../../lib/useHotkey';
import { STATS_HOTKEY } from '../../lib/hotkeys';
import { fmt, fmtBits, fmtInt, fmtOr } from '../../lib/format';
import type { FeatureGate, PresentationSurfaceStats } from '../../lib/featureGates';
import type { ViewerStats } from '../../transport/viewer';

interface Props {
  stats: ViewerStats | null;
  codec: string | null;
  // Received video bitrate derived from the sample window (bits/s) — from
  // the pipeline's self-counted videoBytesReceived (datagram payloads +
  // keyframe stream messages, no transport overhead); null until two
  // samples exist.
  bitrateBps: number | null;
  // R16 (docs/21 Decision 9): gate-controlled features by name + state. The
  // section renders only when the surface reports at least one gate — the
  // broadcaster overlay reports none and stays section-less.
  featureGates?: FeatureGate[];
  // R16 U4: tee + presentation-<video> diagnostics, passed on gated (iPhone)
  // devices only — details must be visible rows there, since the Feature
  // Gates tooltips need hover and iPhones have none. Absent ⇒ no section.
  presentationSurface?: PresentationSurfaceStats;
  onClose: () => void;
  onCopy: () => void;
  copied: boolean;
}

// The viewer stats overlay (docs/10 J4, extended by R9 M7) — "is it the
// stream or my machine". Sections follow the docs/13 funnel: video (decode),
// delivery (frames arriving/dropping), network (this leg's health).
export function StatsOverlay({ stats, codec, bitrateBps, featureGates, presentationSurface, onClose, onCopy, copied }: Props) {
  const conn = stats?.connection ?? null;
  const surface = presentationSurface ?? null;
  const sections: StatsSection[] = [
    {
      title: 'Video',
      rows: [
        ['Codec', codec ?? '—'],
        // The decoded frames' own dimensions @ the measured incoming frame
        // rate — what the stream actually carries, not what was requested.
        ['Resolution', stats?.frameWidth != null && stats.frameHeight != null ? `${stats.frameWidth}×${stats.frameHeight} @ ${fmtInt(stats.receivedFps)}` : '—'],
        ['Decode mode', stats?.isHardwareAccelerated === true ? 'Hardware' : stats?.isHardwareAccelerated === false ? 'Software' : '—'],
        ['Renderer', stats?.renderer === 'webgl' ? 'WebGL' : stats?.renderer === '2d' ? 'Canvas 2D' : '—'],
        ['Pipeline', stats?.pipelineContext === 'worker' ? 'Worker' : stats?.pipelineContext === 'main-thread' ? 'Main thread' : '—'],
        // R12 T2: where presentation happens — paced (adaptive mode, on rAF
        // or the degraded timer fallback) or immediate (live-edge/fixed).
        ['Presentation', stats?.presentation === 'paced-raf' ? 'Paced (rAF)' : stats?.presentation === 'paced-timer' ? 'Paced (timer)' : stats?.presentation === 'immediate' ? 'Immediate' : '—'],
        // R12 T4: the experimental interpolation state; "—" where the
        // pipeline can't offer it.
        ['Interpolation', stats?.interpolation === 'on' ? 'On (blend)' : stats?.interpolation === 'off' ? 'Off' : '—'],
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
        // R18: the relay's live audience push (fleet-global; includes this
        // viewer). "—" until the join-prime lands.
        ['Watching', stats?.viewerCount == null ? '—' : String(stats.viewerCount)],
        // R19 (docs/24 Decision 10): how deltas actually arrive — the
        // truthful mode row, incl. the Decision 8 degradation state.
        ['Delivery mode', stats == null ? '—' : stats.deliveryMode === 'reliable' ? 'reliable (resilient)' : stats.deliveryMode === 'reliable-requested' ? 'reliable requested / datagrams served' : 'datagrams (live-edge)'],
        ...(stats?.deliveryMode !== 'datagrams' && stats != null
          ? ([
              ['Carrier streams', stats.carrierStreams == null ? '—' : `${stats.carrierStreams} (${stats.carrierStreamsAborted ?? 0} aborted)`],
              ['Carrier records', stats.carrierRecords == null ? '—' : String(stats.carrierRecords)],
            ] as StatsRow[])
          : []),
        ['Completed', String(stats?.framesCompleted ?? '—')],
        ['Dropped (incomplete)', String(stats?.framesDroppedIncomplete ?? '—')],
        ['Dropped (late)', String(stats?.framesDroppedLate ?? '—')],
        ['Awaiting keyframe', String(stats?.framesDiscardedAwaitingKey ?? '—')],
        ['Keyframe streams', String(stats?.keyframeStreamsReceived ?? '—')],
        ['Gap resyncs', String(stats?.reorderGapResyncs ?? '—')],
        ['Reorder buffered', String(stats?.reorderBuffered ?? '—')],
        ['Last frame', stats?.timeSinceLastFrameMs == null ? '—' : `${fmtInt(stats.timeSinceLastFrameMs)} ms ago`],
        // Any inbound byte, media or not. Climbing past ~5 s with no media
        // means the session itself is dead, not that the broadcaster paused.
        ['Last inbound', stats?.timeSinceLastInboundMs == null ? '—' : `${fmtInt(stats.timeSinceLastInboundMs)} ms ago`],
        ['Keyframe age', stats?.lastKeyframeAgeMs == null ? '—' : `${fmtInt(stats.lastKeyframeAgeMs)} ms`],
        // R5 Q1: lag behind this session's best capture→decode delta. ~0 = at
        // live edge; sustained growth = falling behind (see docs/15).
        ['Live-edge drift', stats?.liveEdgeDriftMs == null ? '—' : `${fmtInt(stats.liveEdgeDriftMs)} ms`],
        // R5 Q2: absolute glass-to-glass via the relay clock; "—" until both
        // clock legs (broadcaster + this viewer) have synced.
        ['Latency (capture→render)', stats?.capToRenderMs == null ? '—' : `${fmtInt(stats.capToRenderMs)} ms`],
        // R5 Q3 + R12 T2: the playout mode, from the pipeline's own context
        // (ground truth — a toggle that failed to cross the worker shows
        // here). Adaptive shows the live offset (T3 makes it dynamic).
        ['Playout', stats == null ? '—' : stats.playoutMode === 'adaptive' ? `adaptive (+${fmtInt(stats.playoutOffsetMs)} ms)` : stats.playoutMode === 'fixed' ? `fixed (+${fmtInt(stats.playoutOffsetMs)} ms)` : 'live-edge'],
        // R12 T1: the jitter trio (docs/17 Decision 1). Render cadence σ is
        // what T2's paced presentation must move; arrival jitter sizes T3's
        // adaptive offset; decode jitter sizes the decode lead.
        ['Render cadence σ', stats?.renderCadenceStdDevMs == null ? '—' : `${fmt(stats.renderCadenceStdDevMs)} ms`],
        ['Arrival jitter (p95−min)', stats?.arrivalJitterMs == null ? '—' : `${fmtInt(stats.arrivalJitterMs)} ms`],
        ['Decode jitter σ', stats?.decodeJitterMs == null ? '—' : `${fmt(stats.decodeJitterMs)} ms`],
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
        ['Video bitrate (recv)', fmtBits(bitrateBps)],
        ['Datagrams', String(stats?.datagramsReceived ?? '—')],
        ['Bad datagrams', String(stats?.badDatagrams ?? '—')],
      ],
    },
    // R15 (docs/20 N6): the Audio section, rendered only when the stream
    // actually carries audio — a video-only viewer's overlay is unchanged.
    ...(stats && stats.audioState !== 'absent'
      ? [
          {
            title: 'Audio',
            rows: [
              [
                'State',
                stats.audioState === 'active'
                  ? 'Active'
                  : stats.audioState === 'unsupported'
                    ? 'Unsupported here'
                    : 'Error',
              ],
              [
                'Format',
                stats.audioCodec
                  ? `${stats.audioCodec} · ${stats.audioSampleRate ?? '—'} Hz · ${stats.audioChannels ?? '—'}ch`
                  : '—',
              ],
              ['Packets received', String(stats.audioPacketsReceived)],
              ['Packets decoded', String(stats.audioPacketsDecoded)],
              // R15 N5: the sync numbers. Positive skew = video ahead of
              // audio (the forgiving direction); target median ≤ 60 ms.
              ['A/V skew', stats.avSkewMs == null ? '—' : `${fmt(stats.avSkewMs)} ms`],
              [
                'Sync master',
                stats.avMaster === 'video'
                  ? 'Video (audio aligned)'
                  : stats.avMaster === 'free'
                    ? 'Video (audio free-running)'
                    : '—',
              ],
              // docs/20 field finding 6: the jitter-buffer counters. Buffer
              // depth well below target + climbing underruns is the "audio
              // starved for cushion" signature (near-silent live-edge audio).
              ...(stats.audioBuffer
                ? ([
                    [
                      'Buffer depth',
                      `${fmt(stats.audioBuffer.bufferedMs)} / ${fmt(stats.audioBuffer.targetMs)} ms`,
                    ],
                    [
                      'Alignment hold',
                      stats.audioBuffer.alignmentHoldMs == null
                        ? '—'
                        : `${fmt(stats.audioBuffer.alignmentHoldMs)} ms`,
                    ],
                    ['Underruns', String(stats.audioBuffer.underruns)],
                    // Filled vs skipped separates "loss was concealed" from
                    // "loss was skipped inside the lead budget"; overflow
                    // climbing alongside filled is the finding-8 latch.
                    [
                      'Gaps filled / skipped',
                      `${stats.audioBuffer.gapsConcealed} / ${stats.audioBuffer.gapsSkipped}`,
                    ],
                    [
                      'Late / overflow drops',
                      `${stats.audioBuffer.lateDrops} / ${stats.audioBuffer.overflowDrops}`,
                    ],
                    // Re-anchors (timeline restarts + field-finding-7 stall
                    // recoveries): a climbing count is the sink stalling.
                    ['Recoveries', String(stats.audioBuffer.resets)],
                    // The context is free to run at the device rate rather
                    // than the stream's; the worklet resamples, but this is
                    // the number to read first when audio sounds slow.
                    [
                      'Sink rate',
                      stats.audioBuffer.contextSampleRate == null
                        ? '—'
                        : `${stats.audioBuffer.contextSampleRate} Hz${
                            stats.audioSampleRate != null &&
                            stats.audioSampleRate !== stats.audioBuffer.contextSampleRate
                              ? ' (resampling)'
                              : ''
                          }`,
                    ],
                  ] as StatsRow[])
                : []),
            ] as StatsRow[],
          },
        ]
      : []),
    // R16: which conditional features are live on this client — rendered on
    // every viewer. The value stays a bare ✓/✗ (the full detail string
    // overflowed the grid); the detail shows as a hover tooltip on the value
    // and always travels in Copy diagnostics.
    ...(featureGates && featureGates.length > 0
      ? [
          {
            title: 'Feature Gates',
            rows: featureGates.map((g): StatsRow => [g.name, g.active ? '✓' : '✗', g.detail]),
          },
        ]
      : []),
    // R16 U4: the native-fullscreen debugging section (gated devices only) —
    // "is the tee writing frames" vs "is the <video> receiving/playing them".
    ...(surface
      ? [
          {
            title: 'Native Fullscreen',
            rows: [
              ['Fullscreen tier', surface.tier ?? '—'],
              [
                'Tee',
                `${surface.armed ? 'armed' : 'idle'} · ${surface.teedFrames} frames · ${surface.teeErrors} err`,
              ],
              [
                'Video element',
                surface.elementReadyState == null
                  ? '—'
                  : `readyState ${surface.elementReadyState} · ${surface.elementPaused ? 'paused' : 'playing'}`,
              ],
              [
                'Element size',
                surface.elementWidth ? `${surface.elementWidth}×${surface.elementHeight}` : '—',
              ],
              ['Element frames', surface.elementFrames == null ? '—' : String(surface.elementFrames)],
              [
                'Content sample',
                surface.elementContentPeak == null ? '—' : `peak ${surface.elementContentPeak}/255`,
              ],
            ] as StatsRow[],
          },
        ]
      : []),
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
