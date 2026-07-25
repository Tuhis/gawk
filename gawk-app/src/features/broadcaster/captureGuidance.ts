// R24 (docs/30): the broadcaster capture & audio guidance model — the single
// home for the *decisions* (which words, which note, which browser) and the
// copy, so the React surfaces stay dumb renderers and the branch logic is
// unit-tested without a DOM.
//
// The cross-browser fact this whole item exists for: audio is Chromium-only in
// practice (Firefox has neither AudioEncoder nor MediaStreamTrackProcessor and
// no system-audio source). We decide that by feature detection — never UA
// sniffing — reusing the pipeline's own predicate, and we gate on the
// *capability*, never on the `audioState` string (which cannot tell "Firefox,
// can't do audio" from "Chromium, box unticked" — both are 'no-track').

import { audioLaneSupported } from '../../media/audio-lane';
import type { BroadcastStats } from '../../transport/broadcaster';

// Re-export so the UI has one import site and one source of truth (CODE-REVIEW
// one-definition rule): capability answered here, not re-derived per surface.
export { audioLaneSupported };

// One home for the union — imported, never re-declared (CODE-REVIEW).
type AudioState = BroadcastStats['audioState'];

export type AudioGuidance = 'chromium' | 'unsupported';

// ── Copy (the deliverable) ────────────────────────────────────────────────
// All guidance strings live here as named constants; every surface imports
// them, so nothing inlines a second copy. Curly quotes match the surrounding
// production UI.

export const WHOLE_SCREEN_TIP =
  'Share your whole screen for fullscreen games — a single window can freeze ' +
  'when you alt-tab or the game goes fullscreen. Pick a window only to show ' +
  'one app and keep the rest private.';

// Pre-start "Sharing tips" — the audio line, by browser capability.
export const AUDIO_TIP: Record<AudioGuidance, string> = {
  chromium:
    'Audio is captured automatically — just tick “Share audio” (or “Share tab ' +
    'audio”) in the picker. System audio works on Windows; on macOS or Linux, ' +
    'share a browser tab to carry its sound.',
  unsupported:
    'Audio isn’t supported in this browser — you’ll stream video only. Use a ' +
    'Chromium-based browser (Chrome, Edge) to include sound.',
};

// The compact echo in the settings panel (same fact, fewer words).
export const AUDIO_SETTINGS: Record<AudioGuidance, string> = {
  chromium:
    'Captured automatically. Tick “Share audio” in the picker; on macOS or ' +
    'Linux, share a browser tab for sound.',
  unsupported: 'Not supported in this browser — video only. Use a Chromium browser for sound.',
};

// The stats-overlay Audio "State" row, when the browser can't do audio at all
// — honest where the raw audioState ('no-track') would read "No audio shared".
export const OVERLAY_UNSUPPORTED = 'Not supported here';

// Reactive live notes.
export const AUDIO_NOTE = {
  // Chromium, the picker's "Share audio" box was left unticked.
  noTrack:
    'Streaming without audio — “Share audio” wasn’t ticked in the picker. Stop ' +
    'and start again with the box checked to include sound.',
  // Chromium on an OS with no system-audio source (macOS/Linux screen share):
  // the whole grant would have failed, so we retried video-only.
  unavailable:
    'Audio couldn’t be captured on this system. Share a browser tab to include ' +
    'its sound, or continue video-only.',
} as const;

export const WINDOW_NOTE =
  'You’re sharing a single window. If your game runs fullscreen it may look ' +
  'frozen to viewers — share the whole screen to be safe.';

// ── Decisions ─────────────────────────────────────────────────────────────

// 'chromium' iff this browser can actually run the audio lane. The default
// argument reads the live capability; callers that already have the boolean
// (the UI computes it once) pass it in — which also keeps this trivially
// testable and dodges the ESM internal-binding trap.
export function audioGuidanceForBrowser(supported: boolean = audioLaneSupported()): AudioGuidance {
  return supported ? 'chromium' : 'unsupported';
}

// The reactive audio note, or null. Fires ONLY where audio is achievable
// (supported) and the outcome was a fixable silence. Every other state —
// 'active' (working), 'off' (not requested), 'error' (a bug the overlay owns),
// 'unsupported', and anything at all when !supported (Firefox) — returns null,
// so no note ever fires on a healthy broadcast or nags a browser that can't.
export function audioReactiveNote(
  audioState: AudioState,
  supported: boolean,
): { text: string } | null {
  if (!supported) return null;
  if (audioState === 'no-track') return { text: AUDIO_NOTE.noTrack };
  if (audioState === 'unavailable') return { text: AUDIO_NOTE.unavailable };
  return null;
}

// The reactive capture-surface note, or null. Only a *window* share earns the
// nudge toward whole-screen; a monitor share is the recommendation and a
// browser-tab share is the reliable audio path — neither is warned. undefined
// (a browser that doesn't populate displaySurface, or a teardown race) is "no
// hint", always safe. displaySurface is an advisory *category* here, never
// pipeline config, so the docs/01 "trust the frame" rule doesn't apply.
export function captureSurfaceNote(displaySurface: string | undefined): { text: string } | null {
  return displaySurface === 'window' ? { text: WINDOW_NOTE } : null;
}

// ── Dismissal memory (localStorage, gawk:* convention) ─────────────────────
// Persisting the dismissal is a conscious trade (docs/30 decisions 3–4): it
// serves "don't nag experienced users" at the cost of not re-warning a
// forgetful repeat mistake. The two keys are distinct so the two notes dismiss
// independently. All access is try/catch-guarded — a private-mode / disabled
// storage must never throw on the broadcast path (terms/acceptance.ts idiom).

export const HINT_AUDIO_MISSING_KEY = 'gawk:hint-audio-missing';
export const HINT_WINDOW_SHARE_KEY = 'gawk:hint-window-share';

export function isHintDismissed(key: string): boolean {
  try {
    return localStorage.getItem(key) === '1';
  } catch {
    return false; // storage unavailable: show the hint rather than throw
  }
}

export function dismissHint(key: string): void {
  try {
    localStorage.setItem(key, '1');
  } catch {
    // Nothing to persist to; the hint may re-show next time, never a throw.
  }
}
