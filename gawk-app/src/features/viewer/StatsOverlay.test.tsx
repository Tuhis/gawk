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
