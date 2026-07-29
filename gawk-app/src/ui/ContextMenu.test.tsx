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

  // R32 UX1.1. The menu grew past the viewport as milestones added rows (17 in
  // the worst pre-R32 case, ~740 px at the touch row height) and had neither a
  // max-height nor an overflow rule — so on a phone in landscape the tail of
  // the menu simply rendered below the screen with no way to reach it. The
  // clamp floors `top` at PAD, which keeps the *head* on screen and says
  // nothing about the tail.
  it('caps its height to the viewport and scrolls, so the last item stays reachable', () => {
    // A menu taller than jsdom's 768 px viewport.
    sized(200, 1400);
    render(<ContextMenu x={0} y={0} onClose={() => {}} items={items} />);
    const el = menu();
    expect(el.style.top).toBe('8px');
    // Bounded by the viewport less both pads, and scrollable within it.
    expect(el.style.maxHeight).toBe('752px');
    expect(el.style.overflowY).toBe('auto');
  });
});

// R32 UX1.2–UX1.4: the state and availability vocabulary the viewer settings
// need. Before this, a "checked" item was a '✓' glued onto the label string —
// no aria-checked, and an accessible name that changed when only the state
// did — and there was no way to render an option that exists but does not
// apply, which is why R19/R29/R30 controls were filtered out of the array
// instead (docs/37 §1.2).
describe('ContextMenu item state', () => {
  const menu = () => screen.getByRole('menu');

  it('renders a checked item as a radio with aria-checked, leaving the name unchanged', () => {
    const { unmount } = render(
      <ContextMenu
        x={0}
        y={0}
        onClose={() => {}}
        items={[{ label: 'Balanced', onSelect: () => {}, checked: true }]}
      />,
    );
    const on = screen.getByRole('menuitemradio');
    expect(on.getAttribute('aria-checked')).toBe('true');
    const checkedName = on.textContent;
    unmount();

    render(
      <ContextMenu
        x={0}
        y={0}
        onClose={() => {}}
        items={[{ label: 'Balanced', onSelect: () => {}, checked: false }]}
      />,
    );
    const off = screen.getByRole('menuitemradio');
    expect(off.getAttribute('aria-checked')).toBe('false');
    // The whole point: only the state changed, so only aria-checked may.
    expect(off.textContent).toBe(checkedName);
  });

  it('never selects a disabled item, by click or by Enter', () => {
    const onSelect = vi.fn();
    const onClose = vi.fn();
    render(
      <ContextMenu
        x={0}
        y={0}
        onClose={onClose}
        items={[{ label: 'Striping', onSelect, disabled: true }]}
      />,
    );
    fireEvent.click(screen.getByText('Striping'));
    expect(onSelect).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.keyDown(menu(), { key: 'Enter' });
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('skips disabled items when arrowing', () => {
    const first = vi.fn();
    const last = vi.fn();
    render(
      <ContextMenu
        x={0}
        y={0}
        onClose={() => {}}
        items={[
          { label: 'First', onSelect: first },
          { label: 'Middle', onSelect: () => {}, disabled: true },
          { label: 'Last', onSelect: last },
        ]}
      />,
    );
    // Active starts on the first enabled item; one step down must land past
    // the disabled middle.
    fireEvent.keyDown(menu(), { key: 'ArrowDown' });
    fireEvent.keyDown(menu(), { key: 'Enter' });
    expect(last).toHaveBeenCalledTimes(1);
    expect(first).not.toHaveBeenCalled();
  });

  it('shows a disabled item’s reason as visible text', () => {
    render(
      <ContextMenu
        x={0}
        y={0}
        onClose={() => {}}
        items={[
          {
            label: 'Loss protection',
            onSelect: () => {},
            disabled: true,
            reason: 'Not used in this mode.',
          },
        ]}
      />,
    );
    expect(screen.getByText('Not used in this mode.')).toBeTruthy();
  });

  // The first enabled item, not index 0 — otherwise Enter on a freshly opened
  // menu whose head is disabled does nothing at all.
  it('starts the active index on the first enabled item', () => {
    const second = vi.fn();
    render(
      <ContextMenu
        x={0}
        y={0}
        onClose={() => {}}
        items={[
          { label: 'Disabled head', onSelect: () => {}, disabled: true },
          { label: 'Real', onSelect: second },
        ]}
      />,
    );
    fireEvent.keyDown(menu(), { key: 'Enter' });
    expect(second).toHaveBeenCalledTimes(1);
  });
});
