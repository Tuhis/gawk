// @vitest-environment jsdom
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { SeverityBadge } from './SeverityBadge.tsx';
import { GLYPH } from '../lib/severity.ts';

// Severity is carried by THREE channels and never by hue alone: the page has to
// survive a colour-blind reader, a greyscale screenshot pasted into a chat, and
// the CSS failing to load entirely — in which case the glyph and the word are
// all that is left.
describe('SeverityBadge', () => {
  it('renders a glyph AND the word for every state', () => {
    for (const s of ['ok', 'warn', 'bad', 'unknown'] as const) {
      const { unmount } = render(<SeverityBadge severity={s} />);
      expect(screen.getByText(GLYPH[s])).toBeTruthy();
      expect(screen.getByText(s)).toBeTruthy();
      unmount();
    }
  });

  // Even compact, the word stays in the accessibility tree. A glyph alone tells
  // a screen reader nothing.
  it('keeps the word available when compact', () => {
    render(<SeverityBadge severity="bad" compact />);
    expect(screen.getByText('bad')).toBeTruthy();
  });

  // ok and unknown must not share a class, because they must not share a
  // colour: "checked, and it is healthy" and "nothing has ever reported" are
  // different claims and painting the second as the first is the one thing an
  // ops dashboard must never do.
  it('distinguishes ok from unknown', () => {
    const { container: ok, unmount } = render(<SeverityBadge severity="ok" />);
    const okClass = ok.querySelector('span')?.className ?? '';
    unmount();
    const { container: unknown } = render(<SeverityBadge severity="unknown" />);
    const unknownClass = unknown.querySelector('span')?.className ?? '';
    expect(okClass).not.toEqual(unknownClass);
  });
});
