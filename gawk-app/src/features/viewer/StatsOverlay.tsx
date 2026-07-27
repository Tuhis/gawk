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
  // R28 (docs/33 §4.13): this session's telemetry id, or null when the fleet
  // collects nothing. The row it feeds is how a viewer on the phone tells an
  // operator which dashboard row is them.
  telemetrySessionId?: string | null;
  onClose: () => void;
  onCopy: () => void;
  copied: boolean;
}

// How much of the 24-hex sessionId the row shows. Eight characters is what the
// telemetry dashboard's session column prints
// (gawk-telemetry/internal/dashboard/assets/app.js: `sessionId.slice(0, 8)`),
// so the two match by construction — and it is about as much as anyone can
// read down a voice call, which is the whole use case. The full id rides the
// tooltip and Copy diagnostics, where `diagnose()` needs all 24.
const SESSION_ID_DISPLAY_CHARS = 8;

// The viewer stats overlay (docs/10 J4, extended by R9 M7) — "is it the
// stream or my machine". Sections follow the docs/13 funnel: video (decode),
// delivery (frames arriving/dropping), network (this leg's health).
export function StatsOverlay({ stats, codec, bitrateBps, featureGates, presentationSurface, telemetrySessionId, onClose, onCopy, copied }: Props) {
  const conn = stats?.connection ?? null;
  const surface = presentationSurface ?? null;
  const sections: StatsSection[] = [
    // R28: the identity of everything below it — first, because the one person
    // who needs it is reading it aloud to someone else and should not have to
    // scroll a phone-sized panel to find it. Rendered even with no session, so
    // "this viewer is not reporting" is an answer the overlay gives rather than
    // a section whose absence has to be noticed.
    {
      title: 'Telemetry',
      rows: [
        [
          'Session id',
          telemetrySessionId == null
            ? '—'
            : `${telemetrySessionId.slice(0, SESSION_ID_DISPLAY_CHARS)}…`,
          telemetrySessionId ?? 'no session — this relay is not collecting telemetry',
        ],
      ],
    },
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
        // R21: 'dvr' is the relay's own word for it (the join-time DeliveryAck)
        // — a replayed GOP looks exactly like a live one, so nothing the viewer
        // can observe would tell these two apart.
        ['Delivery mode', stats == null ? '—' : stats.deliveryMode === 'dvr' ? `ring-backed (buffer ${fmtInt(stats.dvrBufferMs)} ms)` : stats.deliveryMode === 'reliable' ? 'reliable (resilient)' : stats.deliveryMode === 'reliable-requested' ? 'reliable requested / datagrams served' : 'datagrams (live-edge)'],
        ...(stats?.deliveryMode !== 'datagrams' && stats != null
          ? ([
              ['Carrier streams', stats.carrierStreams == null ? '—' : `${stats.carrierStreams} (${stats.carrierStreamsAborted ?? 0} aborted)`],
              ['Carrier records', stats.carrierRecords == null ? '—' : String(stats.carrierRecords)],
            ] as StatsRow[])
          : []),
        ['Completed', String(stats?.framesCompleted ?? '—')],
        ['Dropped (incomplete)', String(stats?.framesDroppedIncomplete ?? '—')],
        ['Dropped (late)', String(stats?.framesDroppedLate ?? '—')],
        // R29 (docs/34 §7.1): parity's own line. "Recovered" is the headline —
        // frames that would have been dropped incomplete and instead decoded.
        // Zero recovered while chunks arrive means the link is clean, which is
        // the good case; chunks at zero while the fleet is on means the
        // producer is not emitting, which is the case worth noticing.
        ['Parity chunks', String(stats?.parityChunksReceived ?? '—')],
        ['Parity recovered', String(stats?.framesRecoveredByParity ?? '—')],
        ...(stats?.parityRecoveryFailures
          ? ([['Parity too weak', String(stats.parityRecoveryFailures)]] as StatsRow[])
          : []),
        ['Awaiting keyframe', String(stats?.framesDiscardedAwaitingKey ?? '—')],
        ['Keyframe streams', String(stats?.keyframeStreamsReceived ?? '—')],
        ['Gap resyncs', String(stats?.reorderGapResyncs ?? '—')],
        ['Loss-allowance skips', String(stats?.framesSkippedWithinAllowance ?? '—')],
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
              // docs/20 field finding 12: the discriminator for the row above.
              // ~1× means the audio timeline is keeping up, so a skew there is
              // a real lip-sync offset; below 1× the worklet is starving and
              // the skew is starvation debt accruing at (1 − ratio) per second
              // — the shape that read in the thousands while audio sounded
              // fine. A skew without this number cannot be interpreted.
              [
                'Playhead advance',
                stats.avPlayheadAdvance == null
                  ? '—'
                  : `${stats.avPlayheadAdvance.toFixed(3)}×${
                      stats.avPlayheadAdvance < 0.99 ? ' (starving)' : ''
                    }`,
              ],
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
                    // docs/20 field finding 13: what the device adds between a
                    // sample being written and heard. Both the alignment hold
                    // and A/V skew correct for it, so a big number here is not
                    // itself a fault — but it IS the size of the lip-sync
                    // error either correction would cost if it regressed, and
                    // on Bluetooth/HDMI that is a quarter second.
                    [
                      'Output latency',
                      stats.audioBuffer.outputLatencyMs == null
                        ? '—'
                        : `${fmt(stats.audioBuffer.outputLatencyMs)} ms`,
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
    // R16 U4, reshaped by R22 (docs/27 Decision 8): the native-fullscreen
    // debugging section (gated devices only) — each hop of the worker-muxer →
    // MMS → <video> chain reports, so a broken fullscreen localizes remotely.
    ...(surface
      ? [
          {
            title: 'Native Fullscreen',
            rows: [
              ['Fullscreen tier', surface.tier ?? '—'],
              [
                'Muxer',
                `${surface.armed ? 'armed' : 'idle'} · ${surface.muxMediaSegments} seg (${surface.muxInitSegments} init) · ${surface.muxErrors} err`,
              ],
              ['Appends', `${surface.segmentsAppended} · ${surface.appendErrors} err`],
              // docs/27 finding 7: where segments stop. Received counts what
              // reached the main thread (0 against a climbing muxer = a broken
              // sink), queued what the appender is holding (full against 0
              // appends = the system is not taking them), no-init what was
              // discarded for want of the session's one init segment.
              [
                'Segments',
                `${surface.segmentsReceived} recv · ${surface.segmentsQueued} queued` +
                  (surface.segmentsDroppedNoInit > 0
                    ? ` · ${surface.segmentsDroppedNoInit} dropped (no init)`
                    : ''),
              ],
              [
                'MMS streaming',
                surface.mmsStreaming == null
                  ? '— (classic MediaSource)'
                  : surface.mmsStreaming
                    ? 'on — system is asking for data'
                    : 'off — parked',
              ],
              // docs/27 finding 6: the reason, not just the count — an append
              // error is otherwise indistinguishable from a quota drop, and the
              // difference is a dead audio track versus a routine resync.
              ...(surface.lastAppendError
                ? ([['Last append error', surface.lastAppendError]] as StatsRow[])
                : []),
              [
                'Live duration',
                surface.liveDuration == null
                  ? '—'
                  : surface.liveDuration
                    ? 'infinite (LIVE)'
                    : 'finite — no LIVE badge',
              ],
              [
                'Audio track',
                surface.audioMode.startsWith('muxed')
                  ? `${surface.audioMode} · ${surface.audioSegmentsAppended} appended (${surface.muxAudioSegments} muxed, ${surface.muxAudioHoles} holes)${surface.audioTrackActive ? '' : ' · awaiting first sample'}`
                  : surface.audioMode,
              ],
              // The element's own count is the only proof the demuxer ACCEPTED
              // the muxed audio: appends can succeed into a track that never
              // materializes (docs/27 finding 6).
              ...(surface.audioMode.startsWith('muxed')
                ? ([
                    [
                      'Element audio tracks',
                      surface.elementAudioTracks == null
                        ? '— (unavailable)'
                        : surface.elementAudioTracks === 0
                          ? '0 — muxed audio not accepted'
                          : String(surface.elementAudioTracks),
                    ],
                  ] as StatsRow[])
                : []),
              ...(surface.audioTranscode
                ? ([['Audio transcode', surface.audioTranscode]] as StatsRow[])
                : []),
              [
                'Buffered',
                surface.bufferedMs == null
                  ? '—'
                  : `${(surface.bufferedMs / 1000).toFixed(1)} s · ${((surface.bufferedAheadMs ?? 0) / 1000).toFixed(1)} s behind edge` +
                    (surface.bufferedRanges != null && surface.bufferedRanges > 1
                      ? ` · ${surface.bufferedRanges} ranges (hole)`
                      : ''),
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
