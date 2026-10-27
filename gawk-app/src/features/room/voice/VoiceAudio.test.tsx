// @vitest-environment jsdom
// The Audio section (docs/81 D15, NS4): every control, the meter, the
// fallback message, the deferred state, and no Off where the browser has no
// noiseSuppression constraint.
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AudioSettings, METER_INTERVAL_MS } from './VoiceAudio';
import { useVoiceStore } from './voiceStore';
import type { VoiceActions } from './useRoomVoice';
import type { NoiseStatus } from './voiceCapture';
import type { VoiceMeter } from './voiceController';

let meter: VoiceMeter | null;

function fakeActions(): VoiceActions {
  return {
    join: vi.fn(),
    leave: vi.fn(),
    retry: vi.fn(),
    setMuted: vi.fn(),
    setDeafened: vi.fn(),
    setTalkMode: vi.fn(),
    setPttHeld: vi.fn(),
    setPttKey: vi.fn(),
    setSpeakerVolume: vi.fn(),
    isSpeakerMuted: () => false,
    setNoiseMode: vi.fn(),
    setSpeechThreshold: vi.fn(),
    setLevelThresholdDb: vi.fn(),
    setHearSelf: vi.fn(),
    resetAudio: vi.fn(),
    meter: () => meter,
  };
}

const running = (over: Partial<NoiseStatus> = {}): NoiseStatus => ({
  requested: 'rnnoise',
  running: 'rnnoise',
  loading: false,
  fallback: null,
  pending: false,
  offAvailable: true,
  ...over,
});

function open() {
  const details = screen.getByTestId('voice-audio') as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event('toggle'));
}

beforeEach(() => {
  vi.useFakeTimers();
  meter = null;
  localStorage.clear();
  useVoiceStore.getState().reset();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('AudioSettings', () => {
  it('is collapsed by default and offers RNNoise · Browser · Off, the saved one checked', () => {
    useVoiceStore.getState().set({ noise: running() });
    render(<AudioSettings actions={fakeActions()} />);
    expect((screen.getByTestId('voice-audio') as HTMLDetailsElement).open).toBe(false);
    const radios = screen.getAllByRole('radio');
    expect(radios.map((r) => r.textContent)).toEqual(['RNNoise', 'Browser', 'Off']);
    expect(radios[0].getAttribute('aria-checked')).toBe('true');
    expect(screen.getByText(/RNNoise adds 20 ms/)).toBeTruthy();
  });

  it('each control calls its action', () => {
    useVoiceStore.getState().set({ noise: running() });
    const a = fakeActions();
    render(<AudioSettings actions={a} />);
    fireEvent.click(screen.getByRole('radio', { name: 'Browser' }));
    expect(a.setNoiseMode).toHaveBeenCalledWith('browser');
    fireEvent.change(screen.getByLabelText(/Speech threshold/), { target: { value: '0.75' } });
    expect(a.setSpeechThreshold).toHaveBeenCalledWith(0.75);
    fireEvent.click(screen.getByLabelText('Hear yourself'));
    expect(a.setHearSelf).toHaveBeenCalledWith(true);
    fireEvent.click(screen.getByRole('button', { name: 'Reset to defaults' }));
    expect(a.resetAudio).toHaveBeenCalled();
  });

  it('the threshold is the level gate’s, in dBFS, while RNNoise is not deciding', () => {
    useVoiceStore.getState().set({ noise: running({ running: 'browser', requested: 'browser' }), noiseMode: 'browser' });
    const a = fakeActions();
    render(<AudioSettings actions={a} />);
    const slider = screen.getByLabelText(/Level threshold/) as HTMLInputElement;
    expect(slider.min).toBe('-70');
    expect(slider.value).toBe('-40');
    expect(screen.getByText('-40 dBFS')).toBeTruthy();
    fireEvent.change(slider, { target: { value: '-52' } });
    expect(a.setLevelThresholdDb).toHaveBeenCalledWith(-52);
    expect(screen.getByText(/no added delay/)).toBeTruthy();
  });

  it('while RNNoise loads it says so, and the level gate decides', () => {
    useVoiceStore.getState().set({ noise: running({ loading: true }) });
    render(<AudioSettings actions={fakeActions()} />);
    expect(screen.getByRole('status').textContent).toBe('RNNoise is loading…');
    expect(screen.getByLabelText(/Level threshold/)).toBeTruthy();
  });

  it('shows the fallback and the mode it ended in', () => {
    useVoiceStore.getState().set({ noise: running({ running: 'off', fallback: 'the download failed (HTTP 404)' }) });
    render(<AudioSettings actions={fakeActions()} />);
    const status = screen.getByRole('status');
    expect(status.textContent).toBe('RNNoise isn’t available here: the download failed (HTTP 404). Using Off.');
    expect(status.getAttribute('data-tone')).toBe('warn');
    // The saved choice stays RNNoise.
    expect(screen.getByRole('radio', { name: 'RNNoise' }).getAttribute('aria-checked')).toBe('true');
  });

  it('a deferred switch shows the saved choice with “applies when you next join” beside the running mode', () => {
    useVoiceStore.getState().set({ noiseMode: 'browser', noise: running({ requested: 'browser', pending: true }) });
    render(<AudioSettings actions={fakeActions()} />);
    expect(screen.getByRole('radio', { name: 'Browser' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('status').textContent).toBe('Browser applies when you next join; RNNoise for now.');
    expect(screen.getByText(/RNNoise adds 20 ms/)).toBeTruthy();
  });

  it('offers no Off without a noiseSuppression constraint, and shows a saved Off as Browser', () => {
    useVoiceStore.getState().set({ noiseMode: 'off', noise: running({ requested: 'off', running: 'browser', offAvailable: false }) });
    render(<AudioSettings actions={fakeActions()} />);
    expect(screen.getAllByRole('radio').map((r) => r.textContent)).toEqual(['RNNoise', 'Browser']);
    expect(screen.getByRole('radio', { name: 'Browser' }).getAttribute('aria-checked')).toBe('true');
  });

  it('the meter runs only while open: probability, threshold mark and the lamp', () => {
    useVoiceStore.getState().set({ noise: running(), speechThreshold: 0.8 });
    render(<AudioSettings actions={fakeActions()} />);
    expect(screen.queryByTestId('voice-meter')).toBeNull();
    open();
    meter = { prob: 0.95, levelDb: -20, open: true };
    act(() => void vi.advanceTimersByTime(METER_INTERVAL_MS));
    const m = screen.getByTestId('voice-meter');
    expect(m.getAttribute('aria-valuenow')).toBe('0.95');
    expect((m.lastElementChild as HTMLElement).style.left).toBe('80%');
    expect(screen.getByTestId('voice-meter-lamp').getAttribute('data-open')).toBe('true');
    meter = { prob: 0.1, levelDb: -60, open: false };
    act(() => void vi.advanceTimersByTime(METER_INTERVAL_MS));
    expect(screen.getByTestId('voice-meter-lamp').getAttribute('data-open')).toBe('false');
  });

  it('the level meter reads dBFS', () => {
    useVoiceStore.getState().set({ noise: running({ running: 'off', requested: 'off' }), noiseMode: 'off' });
    render(<AudioSettings actions={fakeActions()} />);
    open();
    meter = { prob: null, levelDb: -35, open: false };
    act(() => void vi.advanceTimersByTime(METER_INTERVAL_MS));
    expect(screen.getByTestId('voice-meter').getAttribute('aria-valuenow')).toBe('-35');
  });

  it('Hear yourself warns to use headphones', () => {
    useVoiceStore.getState().set({ noise: running(), hearSelf: true });
    render(<AudioSettings actions={fakeActions()} />);
    expect((screen.getByLabelText('Hear yourself') as HTMLInputElement).checked).toBe(true);
    expect(screen.getByText(/Use headphones/)).toBeTruthy();
  });
});
