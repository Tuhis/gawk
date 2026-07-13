// @vitest-environment jsdom

// R4 (docs/09 I3): the resolution axis defaults to 'auto' when the
// localStorage key is missing or invalid; a previously persisted explicit
// rung (including 'native') keeps its exact meaning. The store reads
// localStorage at module-evaluation time, so each case resets modules and
// re-imports with a fresh localStorage.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const LS_RESOLUTION = 'gawk.resolutionRung';
const LS_FRAMERATE = 'gawk.framerateRung';

async function loadStore() {
  vi.resetModules();
  const mod = await import('./broadcastSettingsStore');
  return mod.useBroadcastSettingsStore.getState();
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  localStorage.clear();
});

describe('broadcastSettingsStore resolution selection', () => {
  it('defaults to auto when nothing is persisted', async () => {
    const s = await loadStore();
    expect(s.resolutionSelection).toBe('auto');
  });

  it('defaults to auto for a garbage persisted value', async () => {
    localStorage.setItem(LS_RESOLUTION, 'banana');
    const s = await loadStore();
    expect(s.resolutionSelection).toBe('auto');
  });

  it('loads a persisted numeric rung unchanged', async () => {
    localStorage.setItem(LS_RESOLUTION, '720');
    const s = await loadStore();
    expect(s.resolutionSelection).toBe(720);
  });

  it('loads a persisted native rung unchanged', async () => {
    localStorage.setItem(LS_RESOLUTION, 'native');
    const s = await loadStore();
    expect(s.resolutionSelection).toBe('native');
  });

  it('loads a persisted auto selection', async () => {
    localStorage.setItem(LS_RESOLUTION, 'auto');
    const s = await loadStore();
    expect(s.resolutionSelection).toBe('auto');
  });

  it('persists a selection as its string form on set', async () => {
    const s = await loadStore();
    s.setResolutionSelection(480);
    expect(localStorage.getItem(LS_RESOLUTION)).toBe('480');
    s.setResolutionSelection('auto');
    expect(localStorage.getItem(LS_RESOLUTION)).toBe('auto');
  });

  it('keeps framerate defaulting to native', async () => {
    localStorage.setItem(LS_FRAMERATE, 'garbage');
    const s = await loadStore();
    expect(s.framerateRung).toBe('native');
  });
});
