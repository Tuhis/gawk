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
    timeSyncRttMs: 6.2,
    autoRung: null,
    autoAtFloor: false,
    autoStepDowns: 0,
    autoStepUps: 0,
    encoderPressure: false,
    autoCeiling: null,
    autoFps: null,
    pipelineContext: 'worker',
    viewerCount: 4,
    audioState: 'active',
    audioEncodedPackets: 1500,
    audioPacketsSent: 1498,
    audioBytesSent: 500_000,
    audioConfigsSent: 30,
    audioEncodedPerSec: 50.1,
    audioSentPerSec: 49.9,
    audioSampleRate: 48000,
    audioChannels: 2,
    audioCodec: 'opus',
    audioBitrateBps: 128_000,
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
    expect(screen.getByText('Pipeline').nextSibling?.textContent).toBe('Worker');
    expect(screen.getByText('Capture fps').nextSibling?.textContent).toBe('60.3');
    expect(screen.getByText('Encoder fps').nextSibling?.textContent).toBe('30.1');
    expect(screen.getByText('Sent fps').nextSibling?.textContent).toBe('30.0');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('18.4 ms');
    expect(screen.getByText('RTT (time-sync)').nextSibling?.textContent).toBe('6.2 ms');
    expect(screen.getByText('Send rate (est.)').nextSibling?.textContent).toBe('25.0 Mbps');
    expect(screen.getByText('At capacity').nextSibling?.textContent).toBe('no');
    expect(screen.getByText('Dgrams lost (out)').nextSibling?.textContent).toBe('15');
    expect(screen.getByText('Video bitrate (sent)').nextSibling?.textContent).toBe('12.0 Mbps');
    expect(screen.getByText('Encode mode').nextSibling?.textContent).toBe('hardware');
    // R18: the relay's live audience push.
    expect(screen.getByText('Watching').nextSibling?.textContent).toBe('4');
  });

  it('renders the R13 auto ceiling + auto fps rows when in auto mode', () => {
    render(
      <BroadcasterStatsOverlay
        stats={{ ...fullStats(), autoCeiling: 1080, autoFps: 60 }}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Auto ceiling').nextSibling?.textContent).toBe('1080p');
    expect(screen.getByText('Auto fps').nextSibling?.textContent).toBe('60');
  });

  it('renders — for the auto rows on explicit selections', () => {
    render(
      <BroadcasterStatsOverlay
        stats={fullStats()}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Auto ceiling').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Auto fps').nextSibling?.textContent).toBe('—');
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
    expect(screen.getByText('Pipeline').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('Capture fps').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('RTT').nextSibling?.textContent).toBe('—');
    expect(screen.getByText('At capacity').nextSibling?.textContent).toBe('—');
  });

  // R16 (docs/21 Decision 9): the Feature Gates section is reported-gates-only
  // and the broadcaster reports none — its overlay must stay unchanged.
  it('has no Feature Gates section', () => {
    render(
      <BroadcasterStatsOverlay
        stats={fullStats()}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.queryByText('Feature Gates')).toBeNull();
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

// R15 N6 (docs/20): the broadcaster Audio section appears only when the
// experimental toggle asked for audio.
describe('BroadcasterStatsOverlay audio section (R15)', () => {
  it('renders no Audio section when the toggle is off', () => {
    render(
      <BroadcasterStatsOverlay
        stats={{ ...fullStats(), audioState: 'off' }}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.queryByText('Audio')).toBeNull();
  });

  it('shows the graceful no-audio-shared state', () => {
    render(
      <BroadcasterStatsOverlay
        stats={{ ...fullStats(), audioState: 'no-track', audioCodec: null }}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Audio')).toBeTruthy();
    expect(screen.getByText('No audio shared')).toBeTruthy();
  });

  it('shows format and rates while active', () => {
    render(
      <BroadcasterStatsOverlay
        stats={fullStats()}
        encoderInfo={encoderInfo}
        bitrateBps={null}
        onClose={() => {}}
        onCopy={() => {}}
        copied={false}
      />,
    );
    expect(screen.getByText('Audio')).toBeTruthy();
    expect(screen.getByText(/opus · 48000 Hz · 2ch/)).toBeTruthy();
  });
});
