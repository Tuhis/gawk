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

});

describe('broadcastSettingsStore framerate rung', () => {
  // The fan-out is capped to 30fps by default (halves the datagram rate and
  // viewer decode load; spectators watch smoothly at 30). An explicit choice
  // still wins and persists.
  it('defaults to 30 when nothing is persisted', async () => {
    const s = await loadStore();
    expect(s.framerateRung).toBe(30);
  });

  it('defaults to 30 for a garbage persisted value', async () => {
    localStorage.setItem(LS_FRAMERATE, 'garbage');
    const s = await loadStore();
    expect(s.framerateRung).toBe(30);
  });

  it('loads a persisted native rung unchanged', async () => {
    localStorage.setItem(LS_FRAMERATE, 'native');
    const s = await loadStore();
    expect(s.framerateRung).toBe('native');
  });

  it('loads a persisted 60 rung unchanged', async () => {
    localStorage.setItem(LS_FRAMERATE, '60');
    const s = await loadStore();
    expect(s.framerateRung).toBe(60);
  });
});
