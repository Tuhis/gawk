// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { Dialog } from './Dialog.tsx';

afterEach(cleanup);

// The hand-rolled overlay owns what `showModal()` would have provided
// (PR #280 review): `aria-modal="true"` is a promise that the page behind is
// inert, so Tab must not walk out of the box, focus must come back to the
// opener on close, and Escape must respect `busy` the way the disabled
// buttons do.
describe('the modal shell', () => {
  it('cancels on Escape', () => {
    const onCancel = vi.fn();
    render(
      <Dialog title="Kill broadcast" onCancel={onCancel}>
        <button type="button">Confirm</button>
      </Dialog>,
    );
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('ignores Escape while busy — the keyboard must not bypass the disabled buttons', () => {
    const onCancel = vi.fn();
    render(
      <Dialog title="Kill broadcast" busy onCancel={onCancel}>
        <button type="button" disabled>
          Confirm
        </button>
      </Dialog>,
    );
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('traps Tab inside the box instead of walking into the background', () => {
    render(
      <div>
        <button type="button">background</button>
        <Dialog title="Kill broadcast" onCancel={() => undefined}>
          <button type="button">first</button>
          <button type="button">last</button>
        </Dialog>
      </div>,
    );
    const first = screen.getByRole('button', { name: 'first' });
    const last = screen.getByRole('button', { name: 'last' });

    last.focus();
    fireEvent.keyDown(document, { key: 'Tab' });
    expect(document.activeElement).toBe(first);

    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(last);
  });

  it('traps shift+Tab as the VERY FIRST keystroke, while the box itself holds focus', () => {
    // The state every dialog opens in: focus on the box (tabIndex -1), which
    // `contains` includes — so without an explicit case the shift branch hits
    // neither guard and the browser's default backward navigation walks into
    // the background table. jsdom does not implement the default action, so
    // the assertable half is that the handler claims the key and wraps.
    render(
      <div>
        <button type="button">background</button>
        <Dialog title="Kill broadcast" onCancel={() => undefined}>
          <button type="button">first</button>
          <button type="button">last</button>
        </Dialog>
      </div>,
    );
    // Mount focused the box itself.
    expect((document.activeElement as HTMLElement).getAttribute('role')).toBe('dialog');
    const claimed = !fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
    expect(claimed).toBe(true); // preventDefault fired — the browser default never runs
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'last' }));
  });

  it('pulls focus back into the box when it has escaped', () => {
    render(
      <div>
        <button type="button">background</button>
        <Dialog title="Kill broadcast" onCancel={() => undefined}>
          <button type="button">first</button>
        </Dialog>
      </div>,
    );
    screen.getByRole('button', { name: 'background' }).focus();
    fireEvent.keyDown(document, { key: 'Tab' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'first' }));
  });

  it('restores focus to the opener on close', () => {
    function Harness() {
      return (
        <div>
          <button type="button">open</button>
        </div>
      );
    }
    const page = render(<Harness />);
    const opener = screen.getByRole('button', { name: 'open' });
    opener.focus();

    const dialog = render(
      <Dialog title="Kill broadcast" onCancel={() => undefined}>
        <button type="button">Confirm</button>
      </Dialog>,
    );
    // Mounting moved focus into the box…
    expect(document.activeElement).not.toBe(opener);
    // …and unmounting hands it back to whoever opened it.
    dialog.unmount();
    expect(document.activeElement).toBe(opener);
    page.unmount();
  });
});
