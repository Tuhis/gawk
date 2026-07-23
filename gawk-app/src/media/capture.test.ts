// R15 field finding (2026-07-19, docs/20): "Enable audio (experimental)" took
// the whole broadcast down on platforms that cannot capture system audio.
// Chromium does not treat getDisplayMedia audio as best-effort — when no
// system-audio source can start (Linux/macOS screen or window shares; only
// Windows/ChromeOS and tab shares have one) it rejects the ENTIRE request with
// NotReadableError "Could not start audio source", video included. docs/20
// Decision 6 says audio may annotate, never abort, so the audio-bearing grant
// falls back to a video-only one.
//
// 2026-07-23: with the toggle removed (audio is requested on every broadcast),
// the module also remembers a refusal for the rest of the page session — that
// memory *is* the escape hatch the toggle used to be, since the video-only
// retry usually has no transient activation left. Each case re-imports the
// module so it starts with a fresh (unrefused) session.

import { afterEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_CAPTURE_CONFIG } from './types';

const AUDIO_CONFIG = { ...DEFAULT_CAPTURE_CONFIG, audio: true };

async function loadAcquire() {
  vi.resetModules();
  return (await import('./capture')).acquireDisplayStream;
}

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

// The platform that has no system-audio source at all: every audio-bearing
// request is refused, every video-only one succeeds.
function stubAudioRefusingPlatform() {
  return stubDisplayMedia(async (opts) => {
    if (opts.audio) throw domError('NotReadableError', 'Could not start audio source');
    return fakeStream();
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('acquireDisplayStream', () => {
  it('falls back to a video-only grant when the browser refuses the audio source', async () => {
    const getDisplayMedia = stubAudioRefusingPlatform();
    const acquireDisplayStream = await loadAcquire();

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
    const acquireDisplayStream = await loadAcquire();

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
    const acquireDisplayStream = await loadAcquire();

    // The message must carry the original cause AND the way out; the
    // activation error the user never asked for must not be the headline.
    await expect(acquireDisplayStream(AUDIO_CONFIG)).rejects.toThrow(
      /Could not start audio source[\s\S]*Start the broadcast again to continue without audio/,
    );
    expect(getDisplayMedia).toHaveBeenCalledTimes(2);
  });

  it('asks once, without audio, when the caller does not want it', async () => {
    const getDisplayMedia = stubDisplayMedia(async () => fakeStream());
    const acquireDisplayStream = await loadAcquire();

    const grant = await acquireDisplayStream(DEFAULT_CAPTURE_CONFIG);

    expect(grant.audioUnavailable).toBe(false);
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(getDisplayMedia.mock.calls[0]![0].audio).toBe(false);
  });
});

// With no toggle to turn off, "start again" is the only move a broadcaster
// has left on a machine that cannot capture system audio — so the second
// start must not spend its one grant re-discovering that.
describe('acquireDisplayStream after an audio refusal (session memory)', () => {
  it('asks for video only on the next start, and still says "unavailable"', async () => {
    const getDisplayMedia = stubAudioRefusingPlatform();
    const acquireDisplayStream = await loadAcquire();

    await acquireDisplayStream(AUDIO_CONFIG);
    getDisplayMedia.mockClear();

    const grant = await acquireDisplayStream(AUDIO_CONFIG);

    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(getDisplayMedia.mock.calls[0]![0].audio).toBe(false);
    // Audio was asked for and cannot be had: the overlay's 'unavailable'
    // state, not the silent 'no audio shared' one.
    expect(grant.audioUnavailable).toBe(true);
  });

  it('remembers even when the video-only retry failed — the failed start is the last one', async () => {
    // The escape hatch that replaces the toggle: attempt 1 dies (no
    // activation left for the retry), attempt 2 goes straight to video-only
    // and broadcasts.
    let allowVideoOnly = false;
    const getDisplayMedia = stubDisplayMedia(async (opts) => {
      if (opts.audio) throw domError('NotReadableError', 'Could not start audio source');
      if (!allowVideoOnly) {
        throw domError('InvalidStateError', 'getDisplayMedia() requires transient activation');
      }
      return fakeStream();
    });
    const acquireDisplayStream = await loadAcquire();

    await expect(acquireDisplayStream(AUDIO_CONFIG)).rejects.toThrow(/Could not start audio source/);

    // A fresh user gesture: the next start has activation again.
    allowVideoOnly = true;
    getDisplayMedia.mockClear();

    const grant = await acquireDisplayStream(AUDIO_CONFIG);

    expect(grant.track).toBeDefined();
    expect(getDisplayMedia).toHaveBeenCalledTimes(1);
    expect(getDisplayMedia.mock.calls[0]![0].audio).toBe(false);
    expect(grant.audioUnavailable).toBe(true);
  });

  it('does not leak the refusal across a page load', async () => {
    const first = stubAudioRefusingPlatform();
    const acquireDisplayStream = await loadAcquire();
    await acquireDisplayStream(AUDIO_CONFIG);
    expect(first).toHaveBeenCalledTimes(2);

    // A reload re-evaluates the module — device state changes (a woken
    // output endpoint, a tab share instead of a screen), so audio is worth
    // one more try.
    const reloaded = stubAudioRefusingPlatform();
    const afterReload = await loadAcquire();
    await afterReload(AUDIO_CONFIG);

    expect(reloaded.mock.calls[0]![0].audio).toBeTruthy();
  });
});
