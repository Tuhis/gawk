// @vitest-environment jsdom
//
// The unsupported-browser warning.
// The load-bearing parts are that it says what is missing, that it cannot be
// dismissed by accident (the point is an acknowledgment, not a toast), and that
// continuing is always available — nobody is locked out of the app.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { UnsupportedBrowserModal } from './UnsupportedBrowserModal';

afterEach(cleanup);

const noWebTransport = {
  supported: false,
  reason: 'no-webtransport',
  browserLabel: 'This browser',
} as const;

describe('UnsupportedBrowserModal', () => {
  it('names the missing API and the browsers that have it', () => {
    render(<UnsupportedBrowserModal support={noWebTransport} onContinue={vi.fn()} />);
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByRole('heading').textContent).toContain('This browser');
    expect(document.body.textContent).toContain('WebTransport');
    // Safari is a supported viewer; the copy must not steer users away.
    expect(document.body.textContent).toContain('Safari');
    expect(document.body.textContent).not.toContain('WebKit');
  });

  it('continues when the acknowledgment is clicked', () => {
    const onContinue = vi.fn();
    render(<UnsupportedBrowserModal support={noWebTransport} onContinue={onContinue} />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  // A stray click on the backdrop must not count as "I understand" — the whole
  // point of this modal is a deliberate acknowledgment.
  it('does not dismiss when the scrim is clicked', () => {
    const onContinue = vi.fn();
    const { container } = render(
      <UnsupportedBrowserModal support={noWebTransport} onContinue={onContinue} />,
    );
    const scrim = container.querySelector('[data-testid="scrim"]');
    expect(scrim).toBeTruthy();
    fireEvent.click(scrim!);
    expect(onContinue).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeTruthy();
  });

  it('focuses the acknowledgment so keyboard and screen-reader users land on it', () => {
    render(<UnsupportedBrowserModal support={noWebTransport} onContinue={vi.fn()} />);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /continue/i }));
  });
});
