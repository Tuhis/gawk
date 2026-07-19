// R15 field finding (2026-07-19, docs/20): "Enable audio (experimental)" took
// the whole broadcast down on platforms that cannot capture system audio.
// Chromium does not treat getDisplayMedia audio as best-effort — when no
// system-audio source can start (Linux/macOS screen or window shares; only
// Windows/ChromeOS and tab shares have one) it rejects the ENTIRE request with
// NotReadableError "Could not start audio source", video included. docs/20
// Decision 6 says audio may annotate, never abort, so the audio-bearing grant
// falls back to a video-only one.

import { afterEach, describe, expect, it, vi } from 'vitest';

import { acquireDisplayStream } from './capture';
import { DEFAULT_CAPTURE_CONFIG } from './types';

const AUDIO_CONFIG = { ...DEFAULT_CAPTURE_CONFIG, audio: true };

function fakeStream(): MediaStream {
  const video = { kind: 'video' } as MediaStreamTrack;
  return {
    getVideoTracks: () => [video],
    getAudioTracks: () => [],
    getTracks: () => [video],
  } as unknown as MediaStream;
}

// DOMException isn't what matters here — the name is (that's all the browser
// gives us to tell "the audio source failed" from "the user said no").
function domError(name: string, message: string): Error {
  const e = new Error(message);
  e.name = name;
  return e;
}

function stubDisplayMedia(impl: (opts: DisplayMediaStreamOptions) => Promise<MediaStream>) {
  const getDisplayMedia = vi.fn(impl);
  vi.stubGlobal('navigator', { mediaDevices: { getDisplayMedia } });
  return getDisplayMedia;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('acquireDisplayStream', () => {
  it('falls back to a video-only grant when the browser refuses the audio source', async () => {
    const getDisplayMedia = stubDisplayMedia(async (opts) => {
      if (opts.audio) throw domError('NotReadableError', 'Could not start audio source');
      return fakeStream();
    });

    const grant = await acquireDisplayStream(AUDIO_CONFIG);

    expect(grant.track).toBeDefined();
    // The flag is what tells the pipeline this is "the browser refused"
    // rather than "the user left the picker's audio box unchecked".
    expect(grant.audioUnavailable).toBe(true);
    expect(getDisplayMedia).toHaveBeenCalledTimes(2);
    expect(getDisplayMedia.mock.calls[1]![0].audio).toBe(false);
  });

  it('never re-prompts when the user cancelled the picker', async () => {
    const getDisplayMedia = stubDisplayMedia(async () => {
      throw domError('NotAllowedError', 'Permission denied');
    });

    await expect(acquireDisplayStream(AUDIO_CONFIG)).rejects.toThrow(/Permission denied/);
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
  });

  it('blames audio, not the retry, when the video-only retry also fails', async () => {
    // The common landing spot in practice: the retry needs its own transient
    // activation, which the seconds spent in the picker have already spent.
    const getDisplayMedia = stubDisplayMedia(async (opts) => {
      if (opts.audio) throw domError('NotReadableError', 'Could not start audio source');
      throw domError('InvalidStateError', 'getDisplayMedia() requires transient activation');
    });

    // The message must carry the original cause AND the way out; the
    // activation error the user never asked for must not be the headline.
    await expect(acquireDisplayStream(AUDIO_CONFIG)).rejects.toThrow(
      /Could not start audio source[\s\S]*Enable audio/,
    );
    expect(getDisplayMedia).toHaveBeenCalledTimes(2);
  });

  it('asks once, without audio, when the toggle is off', async () => {
    const getDisplayMedia = stubDisplayMedia(async () => fakeStream());

    const grant = await acquireDisplayStream(DEFAULT_CAPTURE_CONFIG);

    expect(grant.audioUnavailable).toBe(false);
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(getDisplayMedia.mock.calls[0]![0].audio).toBe(false);
  });
});
