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
    expect(screen.getByText('Received fps').nextSibling?.textContent).toBe('30.2');
    expect(screen.getByText('Rendered fps').nextSibling?.textContent).toBe('29.5');
    expect(screen.getByText('Renderer').nextSibling?.textContent).toBe('WebGL');
    expect(screen.getByText('Pipeline').nextSibling?.textContent).toBe('Worker');
    expect(screen.getByText('Transport').nextSibling?.textContent).toBe('Worker');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('24.5 ms');
    expect(screen.getByText('Bitrate (recv)').nextSibling?.textContent).toBe('4.2 Mbps');
    expect(screen.getByText('Keyframe age').nextSibling?.textContent).toBe('210 ms');
  });

  it('renders — for everything when stats are absent or degraded', () => {
    render(
      <StatsOverlay stats={null} codec={null} bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.getByText('Codec').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Rendered fps').nextSibling?.textContent).toBe('—');

    cleanup();
    // Connection-less stats (Firefox: no getStats) degrade only the network rows.
    const degraded = { ...fullStats(), connection: null, renderedFps: null };
    render(
      <StatsOverlay stats={degraded} codec="vp8" bitrateBps={null} onClose={() => {}} onCopy={() => {}} copied={false} />,
    );
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Received fps').nextSibling?.textContent).toBe('30.2');
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
