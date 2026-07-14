// @vitest-environment jsdom
//
// R9 M7: the broadcaster overlay renders the encode funnel and this leg's
// connection health, degrading to "—" wherever data is unavailable.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { BroadcasterStatsOverlay } from './BroadcasterStatsOverlay';
import type { BroadcastStats } from '../../transport/broadcaster';
import type { EncoderConfigured } from '../../media/encoder';

afterEach(cleanup);

function fullStats(): BroadcastStats {
  return {
    encodedFrames: 900,
    keyframes: 30,
    droppedFrames: 2,
    fpsGateDropped: 450,
    datagramsSent: 2500,
    bytesSent: 9_000_000,
    configsSent: 30,
    keyframeStreamsSent: 30,
    keyframeStreamsFailed: 1,
    keyframeBytesSent: 3_000_000,
    encoderQueueDepth: 1,
    encoderFps: 30.1,
    lastEncodeLatencyMs: 4.2,
    captureFps: 60.3,
    sentFps: 30.0,
    connection: {
      rttMs: 18.4,
      rttVarMs: 2.2,
      packetsSent: 5000,
      packetsReceived: 300,
      packetsLost: 12,
      bytesSent: 9_100_000,
      bytesReceived: 20_000,
      estimatedSendRateBps: 25_000_000,
      atSendCapacity: false,
      datagramsExpiredOutgoing: 8,
      datagramsLostOutgoing: 15,
      datagramsDroppedIncoming: null,
    },
    autoRung: null,
    autoAtFloor: false,
    autoStepDowns: 0,
    autoStepUps: 0,
    encoderPressure: false,
  };
}

const encoderInfo: EncoderConfigured = {
  codec: 'avc1.42E01F',
  variantLabel: 'hw-realtime',
  acceleration: 'hardware',
  width: 1920,
  height: 1080,
  framerate: 60,
  bitrate: 8_000_000,
} as EncoderConfigured;

describe('BroadcasterStatsOverlay', () => {
  it('renders the encode funnel and network health', () => {
    render(
      <BroadcasterStatsOverlay
        stats={fullStats()}
        encoderInfo={encoderInfo}
        bitrateBps={12_000_000}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Capture fps').nextSibling?.textContent).toBe('60.3');
    expect(screen.getByText('Encoder fps').nextSibling?.textContent).toBe('30.1');
    expect(screen.getByText('Sent fps').nextSibling?.textContent).toBe('30.0');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('18.4 ms');
    expect(screen.getByText('Send rate (est.)').nextSibling?.textContent).toBe('25.0 Mbps');
    expect(screen.getByText('At capacity').nextSibling?.textContent).toBe('no');
    expect(screen.getByText('Dgrams lost (out)').nextSibling?.textContent).toBe('15');
    expect(screen.getByText('Bitrate (sent)').nextSibling?.textContent).toBe('12.0 Mbps');
  });

  it('renders — before stats exist and without connection support', () => {
    render(
      <BroadcasterStatsOverlay
        stats={null}
        encoderInfo={null}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Codec').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Capture fps').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('At capacity').nextSibling?.textContent).toBe('—');
  });

  it('fires onCopy and closes via the close button', () => {
    const onCopy = vi.fn();
    const onClose = vi.fn();
    render(
      <BroadcasterStatsOverlay
        stats={fullStats()}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={onClose}
        onCopy={onCopy}
        copied={false}
      />,
    );
    fireEvent.click(screen.getByText('Copy diagnostics'));
    expect(onCopy).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByLabelText('Close stats'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
