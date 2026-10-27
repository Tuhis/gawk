// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { VolumeControl } from './VolumeControl';

afterEach(cleanup);

interface HarnessProps {
  compact: boolean;
  max?: number;
  volume?: number;
  muted?: boolean;
  onOpenChange?: (open: boolean) => void;
}

// A controlled harness: the control owns no audio state, so the tests hold it
// the way ViewerScreen and RoomScreen do and assert what the callbacks did.
function Harness({ compact, max, volume = 0.64, muted = false, onOpenChange }: HarnessProps) {
  const [v, setV] = useState(volume);
  const [m, setM] = useState(muted);
  return (
    <>
      <VolumeControl
        compact={compact}
        label="Volume"
        muteLabel="Mute"
        unmuteLabel="Unmute"
        volume={v}
        muted={m}
        onVolume={setV}
        onMutedChange={setM}
        onOpenChange={onOpenChange}
        max={max}
      />
      <output data-testid="state">{`${v.toFixed(2)} ${m ? 'muted' : 'on'}`}</output>
      <button type="button">elsewhere</button>
    </>
  );
}

const state = () => screen.getByTestId('state').textContent;
const trigger = () => screen.getByRole('button', { name: /^Volume/ });

describe('VolumeControl, wide', () => {
  it('is the mute button beside an always-visible slider', () => {
    render(<Harness compact={false} />);
    const slider = screen.getByRole('slider', { name: 'Volume' }) as HTMLInputElement;
    expect(slider.value).toBe('0.64');
    fireEvent.click(screen.getByRole('button', { name: 'Mute' }));
    expect(state()).toBe('0.64 muted');
    expect((screen.getByRole('slider', { name: 'Volume' }) as HTMLInputElement).value).toBe('0');
  });

  it('dragging the slider while muted unmutes', () => {
    render(<Harness compact={false} muted />);
    fireEvent.change(screen.getByRole('slider', { name: 'Volume' }), { target: { value: '0.3' } });
    expect(state()).toBe('0.30 on');
  });

  // R81: a room voice speaker can be boosted past 100 %.
  it('a max above 1 widens the range and clamps there', () => {
    render(<Harness compact={false} max={2} />);
    const slider = screen.getByRole('slider', { name: 'Volume' }) as HTMLInputElement;
    expect(slider.max).toBe('2');
    expect(slider.getAttribute('aria-valuetext')).toBe('64%');
    fireEvent.change(slider, { target: { value: '1.6' } });
    expect(state()).toBe('1.60 on');
    fireEvent.change(slider, { target: { value: '3' } });
    expect(state()).toBe('2.00 on');
  });

  it('without max it stays a 0..1 control', () => {
    render(<Harness compact={false} />);
    fireEvent.change(screen.getByRole('slider', { name: 'Volume' }), { target: { value: '1.6' } });
    expect(state()).toBe('1.00 on');
  });
});

// Below the narrow breakpoint the slider used to be hidden outright, leaving
// only mute: the volume must stay adjustable there.
describe('VolumeControl, compact', () => {
  it('keeps the slider one tap away and names the level on the button', () => {
    const onOpenChange = vi.fn();
    render(<Harness compact onOpenChange={onOpenChange} />);
    expect(screen.queryByRole('slider')).toBeNull();
    expect(trigger().getAttribute('aria-label')).toBe('Volume: 64%');
    expect(trigger().getAttribute('aria-expanded')).toBe('false');

    fireEvent.click(trigger());
    expect(trigger().getAttribute('aria-expanded')).toBe('true');
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
    const slider = screen.getByRole('slider', { name: 'Volume' });
    expect(document.activeElement).toBe(slider);

    fireEvent.change(slider, { target: { value: '0.2' } });
    expect(state()).toBe('0.20 on');
    expect(trigger().getAttribute('aria-label')).toBe('Volume: 20%');

    fireEvent.click(trigger());
    expect(screen.queryByRole('slider')).toBeNull();
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });

  it('mutes from a pressed toggle inside the popover, and slides out of mute', () => {
    render(<Harness compact />);
    fireEvent.click(trigger());
    const mute = screen.getByRole('button', { name: 'Mute' });
    expect(mute.getAttribute('aria-pressed')).toBe('false');
    fireEvent.click(mute);
    expect(state()).toBe('0.64 muted');
    expect(mute.getAttribute('aria-pressed')).toBe('true');
    expect(trigger().getAttribute('aria-label')).toBe('Volume: muted');

    fireEvent.change(screen.getByRole('slider', { name: 'Volume' }), { target: { value: '0.5' } });
    expect(state()).toBe('0.50 on');
  });

  it('unmuting a level of zero brings back an audible level', () => {
    render(<Harness compact volume={0} muted />);
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole('button', { name: 'Mute' }));
    expect(state()).toBe('0.25 on');
  });

  it('arrow keys step 5%, M toggles mute, Escape closes back onto the button', () => {
    const onOpenChange = vi.fn();
    render(<Harness compact onOpenChange={onOpenChange} />);
    fireEvent.click(trigger());
    const slider = screen.getByRole('slider', { name: 'Volume' });
    fireEvent.keyDown(slider, { key: 'ArrowUp' });
    expect(state()).toBe('0.69 on');
    fireEvent.keyDown(slider, { key: 'ArrowDown' });
    fireEvent.keyDown(slider, { key: 'ArrowLeft' });
    expect(state()).toBe('0.59 on');
    fireEvent.keyDown(slider, { key: 'm' });
    expect(state()).toBe('0.59 muted');

    fireEvent.keyDown(slider, { key: 'Escape' });
    expect(screen.queryByRole('slider')).toBeNull();
    expect(document.activeElement).toBe(trigger());
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });

  it('a pointer down outside closes it; one inside does not', () => {
    render(<Harness compact />);
    fireEvent.click(trigger());
    fireEvent.pointerDown(screen.getByRole('slider'));
    expect(screen.queryByRole('slider')).not.toBeNull();
    fireEvent.pointerDown(screen.getByRole('button', { name: 'elsewhere' }));
    expect(screen.queryByRole('slider')).toBeNull();
  });

  it('the wheel over the button nudges the level without opening', () => {
    render(<Harness compact />);
    fireEvent.wheel(trigger(), { deltaY: -100 });
    expect(state()).toBe('0.69 on');
    fireEvent.wheel(trigger(), { deltaY: 100 });
    fireEvent.wheel(trigger(), { deltaY: 100 });
    expect(state()).toBe('0.59 on');
    expect(screen.queryByRole('slider')).toBeNull();
  });

  it('press and drag up on the button sets the level in one movement', () => {
    render(<Harness compact volume={0.5} />);
    const btn = trigger();
    fireEvent.pointerDown(btn, { pointerId: 1, clientY: 500, button: 0 });
    fireEvent.pointerMove(btn, { pointerId: 1, clientY: 480 });
    expect(screen.queryByRole('slider')).not.toBeNull();
    // 32 px of travel on a 128 px track is a quarter of the range.
    fireEvent.pointerMove(btn, { pointerId: 1, clientY: 468 });
    expect(Number(state()!.split(' ')[0])).toBeCloseTo(0.75, 2);
    fireEvent.pointerUp(btn, { pointerId: 1, clientY: 468 });
    // The click that ends the gesture must not toggle the popover back open.
    fireEvent.click(btn);
    expect(screen.queryByRole('slider')).toBeNull();
    expect(Number(state()!.split(' ')[0])).toBeCloseTo(0.75, 2);

    // A plain tap still opens it.
    fireEvent.pointerDown(btn, { pointerId: 2, clientY: 500, button: 0 });
    fireEvent.pointerUp(btn, { pointerId: 2, clientY: 500 });
    fireEvent.click(btn);
    expect(screen.queryByRole('slider')).not.toBeNull();
  });

  it('widening the window while open closes it, so nothing stays pinned', () => {
    const onOpenChange = vi.fn();
    const { rerender } = render(<Harness compact onOpenChange={onOpenChange} />);
    fireEvent.click(trigger());
    expect(onOpenChange).toHaveBeenLastCalledWith(true);
    rerender(<Harness compact={false} onOpenChange={onOpenChange} />);
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });

  it('unmounting while open reports it closed', () => {
    const onOpenChange = vi.fn();
    const { unmount } = render(<Harness compact onOpenChange={onOpenChange} />);
    fireEvent.click(trigger());
    unmount();
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
  });
});
