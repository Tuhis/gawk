// @vitest-environment jsdom
//
// R24 (docs/30) CG1: the pure guidance model. These pin the browser-aware
// decisions and the note gating without a React tree — the load-bearing rule
// being that a note NEVER fires on a healthy broadcast or on a browser that
// can't do audio at all.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  AUDIO_NOTE,
  AUDIO_SETTINGS,
  AUDIO_TIP,
  audioGuidanceForBrowser,
  audioReactiveNote,
  captureSurfaceNote,
  dismissHint,
  HINT_AUDIO_MISSING_KEY,
  HINT_WINDOW_SHARE_KEY,
  isHintDismissed,
  WINDOW_NOTE,
} from './captureGuidance';

// The six-member audioState union, enumerated for the exhaustive matrix.
const ALL_STATES = ['off', 'no-track', 'unavailable', 'unsupported', 'active', 'error'] as const;

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  delete (globalThis as Record<string, unknown>).AudioEncoder;
  delete (globalThis as Record<string, unknown>).MediaStreamTrackProcessor;
});

describe('audioGuidanceForBrowser (CG1.1)', () => {
  it('maps the capability boolean to the discriminant', () => {
    expect(audioGuidanceForBrowser(true)).toBe('chromium');
    expect(audioGuidanceForBrowser(false)).toBe('unsupported');
  });

  it('defaults to the live feature check (absent in jsdom → unsupported)', () => {
    expect(audioGuidanceForBrowser()).toBe('unsupported');
  });

  it('defaults to chromium when AudioEncoder + MSTP are present', () => {
    (globalThis as Record<string, unknown>).AudioEncoder = function () {};
    (globalThis as Record<string, unknown>).MediaStreamTrackProcessor = function () {};
    expect(audioGuidanceForBrowser()).toBe('chromium');
  });

  it('selects distinct tip and settings copy per discriminant', () => {
    expect(AUDIO_TIP.chromium).not.toBe(AUDIO_TIP.unsupported);
    expect(AUDIO_SETTINGS.chromium).not.toBe(AUDIO_SETTINGS.unsupported);
    // The unsupported copy names the fix (a Chromium browser); the chromium
    // copy names the picker action.
    expect(AUDIO_TIP.unsupported).toMatch(/chromium/i);
    expect(AUDIO_TIP.chromium).toMatch(/share audio/i);
  });
});

describe('audioReactiveNote (CG1.2 / CG1.3)', () => {
  it('fires only for no-track / unavailable, and only when supported', () => {
    for (const state of ALL_STATES) {
      const supported = audioReactiveNote(state, true);
      const unsupported = audioReactiveNote(state, false);
      // A browser that can't do audio is never nagged, for any state.
      expect(unsupported).toBeNull();
      if (state === 'no-track' || state === 'unavailable') {
        expect(supported).not.toBeNull();
      } else {
        // The healthy-broadcast guarantee: active/off/error/unsupported → null.
        expect(supported).toBeNull();
      }
    }
  });

  it('gives no-track and unavailable different, cue-specific copy (CG1.3)', () => {
    const noTrack = audioReactiveNote('no-track', true);
    const unavailable = audioReactiveNote('unavailable', true);
    expect(noTrack?.text).toBe(AUDIO_NOTE.noTrack);
    expect(unavailable?.text).toBe(AUDIO_NOTE.unavailable);
    expect(noTrack?.text).not.toBe(unavailable?.text);
    expect(noTrack?.text).toMatch(/share audio/i); // "tick the box"
    expect(unavailable?.text).toMatch(/browser tab/i); // "use a tab instead"
  });
});

describe('captureSurfaceNote (CG1.4)', () => {
  it('nudges only on a window share', () => {
    expect(captureSurfaceNote('window')?.text).toBe(WINDOW_NOTE);
    expect(captureSurfaceNote('monitor')).toBeNull();
    expect(captureSurfaceNote('browser')).toBeNull();
    expect(captureSurfaceNote(undefined)).toBeNull();
  });
});

describe('dismissal memory (CG1.5)', () => {
  it('round-trips a dismissal and keeps the two keys independent', () => {
    expect(HINT_AUDIO_MISSING_KEY).not.toBe(HINT_WINDOW_SHARE_KEY);
    expect(isHintDismissed(HINT_AUDIO_MISSING_KEY)).toBe(false);
    dismissHint(HINT_AUDIO_MISSING_KEY);
    expect(isHintDismissed(HINT_AUDIO_MISSING_KEY)).toBe(true);
    // Dismissing one leaves the other untouched.
    expect(isHintDismissed(HINT_WINDOW_SHARE_KEY)).toBe(false);
  });

  it('never propagates a throwing storage (private mode)', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('private mode');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('private mode');
    });
    expect(() => dismissHint(HINT_AUDIO_MISSING_KEY)).not.toThrow();
    expect(isHintDismissed(HINT_AUDIO_MISSING_KEY)).toBe(false);
  });
});
