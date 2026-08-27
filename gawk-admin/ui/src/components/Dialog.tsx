import { useEffect, useRef } from 'react';
import type { ReactNode } from 'react';

import ui from '../styles/ui.module.css';

/**
 * The modal shell every confirm dialog uses.
 *
 * A plain overlay rather than `<dialog>.showModal()`: jsdom does not implement
 * it, and a kill confirmation is the last place we want behaviour that only
 * exists in the browser and never in a test. `showModal()` provided focus
 * trapping and focus restore for free, so hand-rolling the overlay means
 * owning both — `aria-modal="true"` is a promise to assistive tech that the
 * rest of the page is inert, and a Tab that walks out into the background
 * table would make it a lie.
 *
 * Escape cancels — unless `busy`, so the keyboard cannot bypass the disabled
 * state the buttons assert while a request is in flight. There is deliberately
 * NO click-outside-to-cancel: these dialogs are the last stop before an action
 * that ends someone's broadcast, and a stray tap on a phone should not be able
 * to move that decision either way.
 */
export function Dialog({
  title,
  children,
  busy = false,
  onCancel,
}: {
  title: string;
  children: ReactNode;
  /** While true, Escape is inert — matching the disabled dialog buttons. */
  busy?: boolean;
  onCancel: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const cancel = useRef(onCancel);
  const busyNow = useRef(busy);
  // Kept current in an effect rather than during render: the key handler below
  // must see the LATEST onCancel/busy without re-binding, and a ref written
  // during render is a render side effect.
  useEffect(() => {
    cancel.current = onCancel;
    busyNow.current = busy;
  });

  useEffect(() => {
    // Mount-only, and it reads the handlers through refs: an effect that ACTS
    // must not re-bind on every render (CODE-REVIEW.md).
    const opener = document.activeElement;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (!busyNow.current) cancel.current();
        return;
      }
      if (e.key !== 'Tab') return;
      // The trap half of aria-modal: Tab cycles inside the box instead of
      // walking into the background the attribute declares inert.
      const el = box.current;
      if (!el) return;
      const focusables = el.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (focusables.length === 0) {
        e.preventDefault();
        el.focus();
        return;
      }
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      const active = document.activeElement;
      if (e.shiftKey) {
        // `active === el` matters: the box itself holds focus when the dialog
        // opens, `contains` is inclusive, and the browser's default BACKWARD
        // navigation from there walks into the background — the forward
        // default enters the box's own first descendant, so only this branch
        // needs the case.
        if (active === first || active === el || !el.contains(active)) {
          e.preventDefault();
          last.focus();
        }
      } else if (active === last || !el.contains(active)) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKey);
    box.current?.focus();
    return () => {
      document.removeEventListener('keydown', onKey);
      // The restore half: a keyboard user who acted from a row must land back
      // on that row's button, not on <body>.
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  }, []);

  return (
    <div className={ui.overlay}>
      <div
        className={ui.dialog}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        ref={box}
      >
        <h2>{title}</h2>
        {children}
      </div>
    </div>
  );
}
