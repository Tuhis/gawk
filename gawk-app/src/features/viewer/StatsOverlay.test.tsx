// @vitest-environment jsdom
//
// R9 M7: the viewer overlay renders full and degraded stats (nulls → "—") and
// hosts the copy-diagnostics action.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { StatsOverlay } from './StatsOverlay';
import type { ViewerStats } from '../../transport/viewer';

afterEach(cleanup);

function fullStats(): ViewerStats {
  return {
    datagramsReceived: 1000,
    badDatagrams: 1,
    duplicateChunks: 0,
    duplicateConfigs: 0,
    framesCompleted: 500,
    framesDroppedIncomplete: 2,
    framesDroppedLate: 3,
    parityChunksReceived: 120,
    framesRecoveredByParity: 14,
    parityRecoveryFailures: 1,
    parityInsufficient: 3,
    framesSkippedWithinAllowance: 3,
    decodedFrames: 495,
    decoderQueueDepth: 1,
    decoderFps: 29.7,
    configsApplied: 1,
    framesDiscardedAwaitingKey: 4,
    lastDecodeLatencyMs: 2.5,
    isHardwareAccelerated: true,
    frameWidth: 1920,
    frameHeight: 1080,
    keyframeStreamsReceived: 12,
    reorderGapResyncs: 1,
    reorderKeyframeWaitDrops: 0,
    reorderBuffered: 2,
    receivedFps: 30.2,
    renderedFps: 29.5,
    renderer: 'webgl',
    pipelineContext: 'worker',
    transport: 'worker',
    timeSinceLastFrameMs: 33,
    timeSinceLastInboundMs: 12,
    dvrBufferMs: 0,
    lastKeyframeAgeMs: 210,
    liveEdgeDriftMs: 42,
    capToRenderMs: 384,
    timeSyncRttMs: 8.4,
    playoutOffsetMs: 0,
    playoutMode: 'off',
    presentation: 'immediate',
    interpolation: 'off',
    renderCadenceStdDevMs: 3.2,
    renderCadenceP95Ms: 9.1,
    arrivalJitterMs: 12,
    decodeJitterMs: 1.4,
    videoBytesReceived: 6_000_000,
    viewerCount: 3,
    deliveryMode: 'datagrams',
    carrierStreams: null,
    carrierRecords: null,
    carrierStreamsAborted: null,
    datagramBuffer: null,
    connection: {
      rttMs: 24.5,
      rttVarMs: 3.1,
      packetsSent: 100,
      packetsReceived: 1200,
      packetsLost: 7,
      bytesSent: 10_000,
      bytesReceived: 5_000_000,
      estimatedSendRateBps: null,
      atSendCapacity: null,
      datagramsExpiredOutgoing: null,
      datagramsLostOutgoing: null,
      datagramsDroppedIncoming: 5,
    },
    audioState: 'absent',
    audioPacketsReceived: 0,
    audioPacketsDecoded: 0,
    audioBytesReceived: 0,
    audioCodec: null,
    audioSampleRate: null,
    audioChannels: null,
    avSkewMs: null,
    avPlayheadAdvance: null,
    avMaster: null,
  videoScheduleBaseEpochMs: null,
  };
}

describe('StatsOverlay', () => {
  it('renders the funnel and network sections from full stats', () => {
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="avc1.42E01F"
        bitrateBps={4_200_000}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('avc1.42E01F')).toBeTruthy();
    expect(screen.getByText('Resolution').nextSibling?.textContent).toBe('1920×1080 @ 30');
    expect(screen.getByText('Received fps').nextSibling?.textContent).toBe('30.2');
    expect(screen.getByText('Rendered fps').nextSibling?.textContent).toBe('29.5');
    expect(screen.getByText('Renderer').nextSibling?.textContent).toBe('WebGL');
    expect(screen.getByText('Pipeline').nextSibling?.textContent).toBe('Worker');
    expect(screen.getByText('Transport').nextSibling?.textContent).toBe('Worker');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('24.5 ms');
    expect(screen.getByText('Video bitrate (recv)').nextSibling?.textContent).toBe('4.2 Mbps');
    expect(screen.getByText('Keyframe age').nextSibling?.textContent).toBe('210 ms');
    expect(screen.getByText('Live-edge drift').nextSibling?.textContent).toBe('42 ms');
    expect(screen.getByText('Latency (capture→render)').nextSibling?.textContent).toBe('384 ms');
    expect(screen.getByText('RTT (time-sync)').nextSibling?.textContent).toBe('8.4 ms');
    expect(screen.getByText('Playout').nextSibling?.textContent).toBe('live-edge');
    expect(screen.getByText('Presentation').nextSibling?.textContent).toBe('Immediate');
    expect(screen.getByText('Interpolation').nextSibling?.textContent).toBe('Off');
    // R12 T1: the jitter rows.
    expect(screen.getByText('Render cadence σ').nextSibling?.textContent).toBe('3.2 ms');
    expect(screen.getByText('Arrival jitter (p95−min)').nextSibling?.textContent).toBe('12 ms');
    expect(screen.getByText('Decode jitter σ').nextSibling?.textContent).toBe('1.4 ms');
    // R18: the relay's live audience push.
    expect(screen.getByText('Watching').nextSibling?.textContent).toBe('3');

    cleanup();
    render(
      <StatsOverlay
        stats={{ ...fullStats(), playoutMode: 'fixed', playoutOffsetMs: 150 }}
        codec="vp8"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Playout').nextSibling?.textContent).toBe('fixed (+150 ms)');

    cleanup();
    // R12 T2: adaptive mode shows the live offset and the pacing placement.
    render(
      <StatsOverlay
        stats={{ ...fullStats(), playoutMode: 'adaptive', playoutOffsetMs: 187, presentation: 'paced-raf', interpolation: 'on' }}
        codec="vp8"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Playout').nextSibling?.textContent).toBe('adaptive (+187 ms)');
    expect(screen.getByText('Presentation').nextSibling?.textContent).toBe('Paced (rAF)');
    expect(screen.getByText('Interpolation').nextSibling?.textContent).toBe('On (blend)');
  });

  // R19 (docs/24 Decision 10): the Delivery row tells the truth in all three
  // states; carrier rows appear only outside plain datagram mode.
  it('renders the delivery mode row truthfully in all three states', () => {
    render(
      <StatsOverlay stats={fullStats()} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.getByText('Delivery mode').nextSibling?.textContent).toBe('datagrams (live-edge)');
    expect(screen.queryByText('Carrier streams')).toBeNull();
    expect(screen.queryByText('Carrier records')).toBeNull();

    cleanup();
    render(
      <StatsOverlay
        stats={{ ...fullStats(), deliveryMode: 'reliable', carrierStreams: 24, carrierRecords: 700, carrierStreamsAborted: 1 }}
        codec="vp8"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Delivery mode').nextSibling?.textContent).toBe('reliable (resilient)');
    expect(screen.getByText('Carrier streams').nextSibling?.textContent).toBe('24 (1 aborted)');
    expect(screen.getByText('Carrier records').nextSibling?.textContent).toBe('700');

    cleanup();
    // Decision 8 degradation: requested but the relay serves datagrams.
    render(
      <StatsOverlay
        stats={{ ...fullStats(), deliveryMode: 'reliable-requested', carrierStreams: 0, carrierRecords: 0, carrierStreamsAborted: 0 }}
        codec="vp8"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Delivery mode').nextSibling?.textContent).toBe('reliable requested / datagrams served');
  });

  it('renders — for everything when stats are absent or degraded', () => {
    render(
      <StatsOverlay stats={null} codec={null} bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.getByText('Codec').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Resolution').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Rendered fps').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Live-edge drift').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Latency (capture→render)').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('RTT (time-sync)').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Render cadence σ').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Arrival jitter (p95−min)').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Decode jitter σ').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Presentation').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Interpolation').nextSibling?.textContent).toBe('—');

    cleanup();
    // Connection-less stats (Firefox: no getStats) degrade only the network rows.
    const degraded = { ...fullStats(), connection: null, renderedFps: null };
    render(
      <StatsOverlay stats={degraded} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Received fps').nextSibling?.textContent).toBe('30.2');
  });

  // R16 (docs/21 Decision 9): the Feature Gates section renders only when the
  // surface reports at least one gate. The value is a bare ✓/✗; the detail is
  // the value's hover tooltip (title attribute).
  it('renders the Feature Gates section only when gates are reported', () => {
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="vp8"
        bitrateBps={null}
        featureGates={[{ name: 'NativeVideoFullscreen', active: true, detail: 'armed' }]}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Feature Gates')).toBeTruthy();
    let value = screen.getByText('NativeVideoFullscreen').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✓');
    expect(value.getAttribute('title')).toBe('armed');

    cleanup();
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="vp8"
        bitrateBps={null}
        featureGates={[
          { name: 'NativeVideoFullscreen', active: false, detail: 'element fullscreen available' },
        ]}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    value = screen.getByText('NativeVideoFullscreen').nextSibling as HTMLElement;
    expect(value.textContent).toBe('✗');
    expect(value.getAttribute('title')).toBe('element fullscreen available');

    cleanup();
    // No gates reported (prop absent or empty) ⇒ no section.
    render(
      <StatsOverlay stats={fullStats()} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.queryByText('Feature Gates')).toBeNull();
    cleanup();
    render(
      <StatsOverlay stats={fullStats()} codec="vp8" bitrateBps={null} featureGates={[]} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.queryByText('Feature Gates')).toBeNull();
  });

  // R28 (docs/33 §4.13): the row a viewer reads aloud to an operator. The
  // value is the dashboard's own 8-character prefix; the full 24 stay in the
  // tooltip, where `diagnose()` can be handed all of it.
  it('renders the telemetry session id, short on screen and whole in the tooltip', () => {
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="vp8"
        bitrateBps={null}
        telemetrySessionId="000102030405060708090a0b"
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    const value = screen.getByText('Session id').nextSibling as HTMLElement;
    expect(value.textContent).toBe('00010203…');
    expect(value.getAttribute('title')).toBe('000102030405060708090a0b');
  });

  it('says so when there is no telemetry session to name', () => {
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="vp8"
        bitrateBps={null}
        telemetrySessionId={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    const value = screen.getByText('Session id').nextSibling as HTMLElement;
    expect(value.textContent).toBe('—');
    expect(value.getAttribute('title')).toBe('no session — this relay is not collecting telemetry');
  });

  it('wires the copy-diagnostics button and the copied flash', () => {
    const onCopy = vi.fn();
    const { rerender } = render(
      <StatsOverlay stats={fullStats()} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={onCopy} copied={false} />,
    );
    fireEvent.click(screen.getByText('Copy diagnostics'));
    expect(onCopy).toHaveBeenCalledTimes(1);
    rerender(
      <StatsOverlay stats={fullStats()} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={onCopy} copied={true} />,
    );
    expect(screen.getByText('Copied')).toBeTruthy();
  });
});

// R15 N6 (docs/20): the Audio section is gated on audio actually being in
// the stream — a video-only viewer's overlay is unchanged.
describe('StatsOverlay audio section (R15)', () => {
  it('renders no Audio section for a video-only stream', () => {
    render(
      <StatsOverlay
        stats={fullStats()}
        codec="avc1.42E01F"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.queryByText('Audio')).toBeNull();
    expect(screen.queryByText('A/V skew')).toBeNull();
  });

  it('renders format, counters and the sync rows once audio is active', () => {
    render(
      <StatsOverlay
        stats={{
          ...fullStats(),
          audioState: 'active',
          audioCodec: 'opus',
          audioSampleRate: 48000,
          audioChannels: 2,
          audioPacketsReceived: 500,
          audioPacketsDecoded: 498,
          avSkewMs: 42.4,
          avMaster: 'video',
        }}
        codec="avc1.42E01F"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Audio')).toBeTruthy();
    expect(screen.getByText(/opus · 48000 Hz · 2ch/)).toBeTruthy();
    expect(screen.getByText('A/V skew')).toBeTruthy();
    expect(screen.getByText('Video (audio aligned)')).toBeTruthy();
    // No sink stats merged in yet ⇒ no jitter-buffer rows.
    expect(screen.queryByText('Buffer depth')).toBeNull();
    expect(screen.queryByText('Underruns')).toBeNull();
  });

  // docs/20 field finding 6: the jitter-buffer counters, merged in on the main
  // thread from the AudioSink, surface as their own rows (buffer depth / target,
  // underruns, drops) — the "audio starved for cushion" diagnostic.
  it('renders the jitter-buffer rows when the sink stats are present', () => {
    render(
      <StatsOverlay
        stats={{
          ...fullStats(),
          audioState: 'active',
          audioCodec: 'opus',
          audioSampleRate: 48_000,
          avSkewMs: 12,
          avPlayheadAdvance: 0.934,
          avMaster: 'video',
          audioBuffer: {
            bufferedMs: 38.2,
            targetMs: 120,
            alignmentHoldMs: 95,
            underruns: 7,
            gapsConcealed: 2,
            gapsSkipped: 3,
            contextSampleRate: 44100,
            lateDrops: 1,
            overflowDrops: 0,
            resets: 0,
            outputLatencyMs: 128,
          },
        }}
        codec="avc1.42E01F"
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Buffer depth').nextSibling?.textContent).toBe('38.2 / 120.0 ms');
    expect(screen.getByText('Alignment hold').nextSibling?.textContent).toBe('95.0 ms');
    expect(screen.getByText('Underruns').nextSibling?.textContent).toBe('7');
    expect(screen.getByText('Gaps filled / skipped').nextSibling?.textContent).toBe('2 / 3');
    expect(screen.getByText('Late / overflow drops').nextSibling?.textContent).toBe('1 / 0');
    // The stream is 48 kHz (fixture above), the context 44.1 kHz: annotated.
    expect(screen.getByText('Sink rate').nextSibling?.textContent).toBe('44100 Hz (resampling)');
    // docs/20 field finding 13: the device's own delay. Without this row a
    // capture cannot tell a correctly-synced Bluetooth session from one where
    // the correction regressed — the two differ by exactly this number.
    expect(screen.getByText('Output latency').nextSibling?.textContent).toBe('128.0 ms');
    // docs/20 field finding 12: a skew read while the audio timeline is losing
    // ground is starvation debt, not lip sync — the row says so out loud, so a
    // capture can no longer be read as a lip-sync error it is not. 0.934x is
    // the ratio that produced the field capture's 1986 ms over 30 s.
    expect(screen.getByText('Playhead advance').nextSibling?.textContent).toBe('0.934× (starving)');
  });
});
