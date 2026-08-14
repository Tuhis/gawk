// @vitest-environment jsdom
//
// The unsupported-browser warning. Behavior is written first (CODE-REVIEW.md).
// The load-bearing parts are that it names the *actual* browser, that it cannot
// be dismissed by accident (the point is an acknowledgment, not a toast), and
// that continuing is always available — nobody is locked out of the app.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { UnsupportedBrowserModal } from './UnsupportedBrowserModal';

afterEach(cleanup);

describe('UnsupportedBrowserModal', () => {
  it('names the detected browser in the WebKit case', () => {
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Safari' }}
        onContinue={vi.fn()}
      />,
    );
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(screen.getByRole('heading').textContent).toContain('Safari');
  });

  it('names a WebKit re-brand by its own name, not "Safari"', () => {
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Chrome on iOS' }}
        onContinue={vi.fn()}
      />,
    );
    expect(screen.getByRole('heading').textContent).toContain('Chrome on iOS');
  });

  it('explains that switching browsers on iOS will not help', () => {
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Safari on iOS' }}
        onContinue={vi.fn()}
      />,
    );
    expect(document.body.textContent).toContain('iPhone and iPad');
  });

  it('uses different copy when WebTransport is missing outright', () => {
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'no-webtransport', browserLabel: 'This browser' }}
        onContinue={vi.fn()}
      />,
    );
    expect(document.body.textContent).toContain('WebTransport');
    expect(document.body.textContent).not.toContain('WebKit');
  });

  it('continues when the acknowledgment is clicked', () => {
    const onContinue = vi.fn();
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Safari' }}
        onContinue={onContinue}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  // A stray click on the backdrop must not count as "I understand" — the whole
  // point of this modal is a deliberate acknowledgment.
  it('does not dismiss when the scrim is clicked', () => {
    const onContinue = vi.fn();
    const { container } = render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Safari' }}
        onContinue={onContinue}
      />,
    );
    const scrim = container.querySelector('[data-testid="scrim"]');
    expect(scrim).toBeTruthy();
    fireEvent.click(scrim!);
    expect(onContinue).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeTruthy();
  });

  it('focuses the acknowledgment so keyboard and screen-reader users land on it', () => {
    render(
      <UnsupportedBrowserModal
        support={{ supported: false, reason: 'webkit', browserLabel: 'Safari' }}
        onContinue={vi.fn()}
      />,
    );
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /continue/i }));
  });
});
