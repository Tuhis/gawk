// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ContextMenu } from './ContextMenu';

afterEach(cleanup);

describe('ContextMenu', () => {
  it('renders items and dispatches + closes on select', () => {
    const onSelect = vi.fn();
    const onClose = vi.fn();
    render(
      <ContextMenu
        x={10}
        y={10}
        onClose={onClose}
        items={[
          { label: 'Stats', onSelect },
          { label: 'Leave', onSelect: () => {} },
        ]}
      />,
    );
    fireEvent.click(screen.getByText('Stats'));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    render(<ContextMenu x={0} y={0} onClose={onClose} items={[{ label: 'A', onSelect: () => {} }]} />);
    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on an outside pointer down', () => {
    const onClose = vi.fn();
    render(<ContextMenu x={0} y={0} onClose={onClose} items={[{ label: 'A', onSelect: () => {} }]} />);
    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  // Without this, the anchor button can never dismiss the menu it opened: this
  // listener closes on its pointerdown and the ensuing click re-opens. Whether
  // that is visible depends on React's flush timing between the two listeners
  // (jsdom's act() hides it; Chrome does not), so the anchor is excluded
  // outright rather than raced against.
  it('ignores a pointer down inside its anchor — the anchor owns dismissal', () => {
    const onClose = vi.fn();
    const anchor = document.createElement('button');
    document.body.appendChild(anchor);
    const anchorRef = { current: anchor as HTMLElement | null };
    render(
      <ContextMenu
        x={0}
        y={0}
        anchorRef={anchorRef}
        onClose={onClose}
        items={[{ label: 'A', onSelect: () => {} }]}
      />,
    );

    fireEvent.pointerDown(anchor);
    expect(onClose).not.toHaveBeenCalled();

    // Everything else still closes it.
    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenCalledTimes(1);
    anchor.remove();
  });
});

// Placement. jsdom measures everything as 0×0, so these stub the menu's
// rendered size — the anchor math is pure arithmetic over it, and it is
// load-bearing: a real-browser check caught the bottom-right case covering
// the very button that opens it (docs/24 review finding PRODUCT-2).
describe('ContextMenu placement', () => {
  // The component measures its layout box (offsetWidth/Height) — jsdom
  // reports 0 for both, as it does for getBoundingClientRect.
  const sized = (width: number, height: number) => {
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(width);
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(height);
  };
  afterEach(() => vi.restoreAllMocks());

  const menu = () => screen.getByRole('menu');
  const items = [{ label: 'A', onSelect: () => {} }];

  it('treats (x, y) as the top-left corner by default (a pointer position)', () => {
    sized(200, 120);
    render(<ContextMenu x={100} y={100} onClose={() => {}} items={items} />);
    expect(menu().style.left).toBe('100px');
    expect(menu().style.top).toBe('100px');
  });

  it('grows up-left from (x, y) when anchored bottom-right, never covering it', () => {
    sized(200, 120);
    // jsdom's viewport is 1024×768, so neither edge clamps here.
    render(<ContextMenu x={900} y={700} anchor="bottom-right" onClose={() => {}} items={items} />);
    expect(menu().style.left).toBe('700px');
    expect(menu().style.top).toBe('580px');
    // The invariant that matters: the menu ends above/left of its anchor.
    expect(580 + 120).toBeLessThanOrEqual(700);
    expect(700 + 200).toBeLessThanOrEqual(900);
  });

  it('still clamps a bottom-right anchor into the viewport', () => {
    sized(200, 120);
    render(<ContextMenu x={80} y={40} anchor="bottom-right" onClose={() => {}} items={items} />);
    // Would be (-120, -80) unclamped; the 8 px pad wins.
    expect(menu().style.left).toBe('8px');
    expect(menu().style.top).toBe('8px');
  });
});
