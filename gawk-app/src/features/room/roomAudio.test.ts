import { afterEach, describe, expect, it, vi } from 'vitest';
import { RoomAudioMixer } from './roomAudio';

function stubWebAudio() {
  const contexts: FakeAudioContext[] = [];
  class FakeAudioContext {
    state = 'suspended';
    destination = { kind: 'destination' };
    audioWorklet = { addModule: vi.fn(() => Promise.resolve()) };
    gains: { gain: { value: number }; connect: ReturnType<typeof vi.fn> }[] = [];
    createGain = vi.fn(() => {
      const g = { gain: { value: 1 }, connect: vi.fn(), disconnect: vi.fn() };
      this.gains.push(g);
      return g;
    });
    resume = vi.fn(() => {
      this.state = 'running';
      return Promise.resolve();
    });
    close = vi.fn(() => Promise.resolve());
    constructor() {
      contexts.push(this);
    }
  }
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', class {});
  vi.stubGlobal('Blob', class {});
  vi.stubGlobal('URL', { createObjectURL: vi.fn(() => 'blob:stub'), revokeObjectURL: vi.fn() });
  return contexts;
}

afterEach(() => vi.unstubAllGlobals());

describe('RoomAudioMixer', () => {
  it('opens one context lazily, shares it across tiles, and registers the worklet once', async () => {
    const contexts = stubWebAudio();
    const mixer = new RoomAudioMixer();
    expect(contexts).toHaveLength(0);
    const a = mixer.output()!;
    const b = mixer.output()!;
    expect(contexts).toHaveLength(1);
    expect(a.context).toBe(b.context);
    expect(a.destination).toBe(b.destination);
    // The master gain feeds the real destination.
    expect(contexts[0].gains[0].connect).toHaveBeenCalledWith(contexts[0].destination);
    await Promise.all([a.ensureWorklet(), b.ensureWorklet(), a.ensureWorklet()]);
    expect(contexts[0].audioWorklet.addModule).toHaveBeenCalledTimes(1);
    mixer.dispose();
    expect(contexts[0].close).toHaveBeenCalledTimes(1);
    expect(mixer.output()).toBeNull();
  });

  it('master volume and mute drive the shared gain, before and after the context exists', () => {
    const contexts = stubWebAudio();
    const mixer = new RoomAudioMixer();
    mixer.setMasterVolume(0.4);
    mixer.setMasterMuted(true);
    mixer.output();
    expect(contexts[0].gains[0].gain.value).toBe(0);
    mixer.setMasterMuted(false);
    expect(contexts[0].gains[0].gain.value).toBe(0.4);
    mixer.setMasterVolume(2);
    expect(mixer.masterVolume).toBe(1);
    expect(contexts[0].gains[0].gain.value).toBe(1);
  });

  it('reports the gesture gate for the shared context and resumes it', async () => {
    const contexts = stubWebAudio();
    const mixer = new RoomAudioMixer();
    expect(mixer.needsGesture).toBe(false);
    mixer.output();
    expect(mixer.needsGesture).toBe(true);
    await mixer.resume();
    expect(contexts[0].resume).toHaveBeenCalled();
    expect(mixer.needsGesture).toBe(false);
  });

  it('is null without Web Audio', () => {
    vi.stubGlobal('AudioContext', undefined);
    expect(new RoomAudioMixer().output()).toBeNull();
  });
});
