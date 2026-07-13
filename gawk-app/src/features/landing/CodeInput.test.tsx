// @vitest-environment jsdom
//
// Segmented broadcast-code input. Behavior is written first (CODE-REVIEW.md):
// sanitized typing/paste, invalid-char rejection, completion signaling, and
// the active-box indicator that gives the "auto-advance / step back" feel.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { CodeInput } from './CodeInput';

afterEach(cleanup);

describe('CodeInput', () => {
  it('sanitizes and uppercases pasted/typed input', () => {
    const onChange = vi.fn();
    render(<CodeInput value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'ab2cd3' } });
    expect(onChange).toHaveBeenCalledWith('AB2CD3');
  });

  it('rejects characters outside the alphabet (0/O/1/I/L and punctuation)', () => {
    const onChange = vi.fn();
    render(<CodeInput value="" onChange={onChange} />);
    // Of "A1!KO0", only A and K survive (1, !, O, 0 are all excluded).
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'A1!KO0' } });
    expect(onChange).toHaveBeenCalledWith('AK');
  });

  it('caps at 6 characters', () => {
    const onChange = vi.fn();
    render(<CodeInput value="" onChange={onChange} />);
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'AB2CD3XY' } });
    expect(onChange).toHaveBeenCalledWith('AB2CD3');
  });

  it('fires onComplete only when 6 valid chars are present', () => {
    const onComplete = vi.fn();
    const { rerender } = render(<CodeInput value="" onChange={() => {}} onComplete={onComplete} />);
    const input = screen.getByLabelText('Broadcast code');

    fireEvent.change(input, { target: { value: 'AB2CD' } }); // 5 chars
    expect(onComplete).not.toHaveBeenCalled();

    rerender(<CodeInput value="AB2CD" onChange={() => {}} onComplete={onComplete} />);
    fireEvent.change(input, { target: { value: 'AB2CD3' } }); // 6 chars
    expect(onComplete).toHaveBeenCalledWith('AB2CD3');
  });

  it('marks the next empty box active (auto-advance) and steps back on shorten', () => {
    const { rerender, container } = render(<CodeInput value="AB" onChange={() => {}} />);
    const activeAt = () =>
      Array.from(container.querySelectorAll('[data-active="true"]')).map((el) =>
        Array.from(el.parentElement!.children).indexOf(el),
      );
    expect(activeAt()).toEqual([2]); // typed 2 → box 3 (index 2) is active

    rerender(<CodeInput value="A" onChange={() => {}} />);
    expect(activeAt()).toEqual([1]); // backspaced → box 2 (index 1) is active
  });

  it('fires onEnter when Enter is pressed', () => {
    const onEnter = vi.fn();
    render(<CodeInput value="AB2CD3" onChange={() => {}} onEnter={onEnter} />);
    fireEvent.keyDown(screen.getByLabelText('Broadcast code'), { key: 'Enter' });
    expect(onEnter).toHaveBeenCalled();
  });
});
