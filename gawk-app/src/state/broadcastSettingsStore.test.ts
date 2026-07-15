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

describe('broadcastSettingsStore framerate selection (R12)', () => {
  // docs/17 Decision 4: the default is 'auto' (probe-resolved — 60 when
  // hardware supports it, else 30). A previously persisted explicit rung
  // keeps its exact meaning across the widening.
  it('defaults to auto when nothing is persisted', async () => {
    const s = await loadStore();
    expect(s.framerateSelection).toBe('auto');
  });

  it('defaults to auto for a garbage persisted value', async () => {
    localStorage.setItem(LS_FRAMERATE, 'garbage');
    const s = await loadStore();
    expect(s.framerateSelection).toBe('auto');
  });

  it('a persisted explicit rung survives the widening unchanged', async () => {
    localStorage.setItem(LS_FRAMERATE, '30');
    expect((await loadStore()).framerateSelection).toBe(30);
    localStorage.setItem(LS_FRAMERATE, '60');
    expect((await loadStore()).framerateSelection).toBe(60);
    localStorage.setItem(LS_FRAMERATE, 'native');
    expect((await loadStore()).framerateSelection).toBe('native');
  });

  it('persists auto on set', async () => {
    const s = await loadStore();
    s.setFramerateSelection('auto');
    expect(localStorage.getItem(LS_FRAMERATE)).toBe('auto');
  });
});

describe('broadcastSettingsStore advanced axes (R12 L4)', () => {
  it('hwPreference defaults to auto and validates persisted values', async () => {
    expect((await loadStore()).hwPreference).toBe('auto');
    localStorage.setItem('gawk.hwPreference', 'hardware');
    expect((await loadStore()).hwPreference).toBe('hardware');
    localStorage.setItem('gawk.hwPreference', 'turbo');
    expect((await loadStore()).hwPreference).toBe('auto');
  });

  it('hwPreference persists on set', async () => {
    const s = await loadStore();
    s.setHwPreference('software');
    expect(localStorage.getItem('gawk.hwPreference')).toBe('software');
  });

  it('bitrateOverride defaults to null, persists clamped, and clears', async () => {
    expect((await loadStore()).bitrateOverride).toBeNull();
    const s = await loadStore();
    s.setBitrateOverride(80_000_000);
    expect(localStorage.getItem('gawk.bitrateOverride')).toBe('50000000');
    s.setBitrateOverride(null);
    expect(localStorage.getItem('gawk.bitrateOverride')).toBeNull();
  });

  it('bitrateOverride rejects garbage persisted values', async () => {
    localStorage.setItem('gawk.bitrateOverride', 'lots');
    expect((await loadStore()).bitrateOverride).toBeNull();
    localStorage.setItem('gawk.bitrateOverride', '-5');
    expect((await loadStore()).bitrateOverride).toBeNull();
    localStorage.setItem('gawk.bitrateOverride', '2000000');
    expect((await loadStore()).bitrateOverride).toBe(2_000_000);
  });

  it('codecOverride only accepts codecs from the preference list', async () => {
    expect((await loadStore()).codecOverride).toBeNull();
    localStorage.setItem('gawk.codecOverride', 'vp8');
    expect((await loadStore()).codecOverride).toBe('vp8');
    localStorage.setItem('gawk.codecOverride', 'h265-dreams');
    expect((await loadStore()).codecOverride).toBeNull();
  });

  it('codecOverride persists and clears on set', async () => {
    const s = await loadStore();
    s.setCodecOverride('vp8');
    expect(localStorage.getItem('gawk.codecOverride')).toBe('vp8');
    s.setCodecOverride(null);
    expect(localStorage.getItem('gawk.codecOverride')).toBeNull();
  });
});
