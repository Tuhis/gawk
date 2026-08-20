import { useEffect, useRef } from 'react';
import type { ReactNode } from 'react';

import ui from '../styles/ui.module.css';

/**
 * The modal shell every confirm dialog uses.
 *
 * A plain overlay rather than `<dialog>.showModal()`: jsdom does not implement
 * it, and a kill confirmation is the last place we want behaviour that only
 * exists in the browser and never in a test.
 *
 * Escape cancels. There is deliberately NO click-outside-to-cancel: these
 * dialogs are the last stop before an action that ends someone's broadcast, and
 * a stray tap on a phone should not be able to move that decision either way.
 */
export function Dialog({
  title,
  children,
  onCancel,
}: {
  title: string;
  children: ReactNode;
  onCancel: () => void;
}) {
  const box = useRef<HTMLDivElement>(null);
  const cancel = useRef(onCancel);
  // Kept current in an effect rather than during render: the key handler below
  // must call the LATEST onCancel without re-binding, and a ref written during
  // render is a render side effect.
  useEffect(() => {
    cancel.current = onCancel;
  });

  useEffect(() => {
    // Mount-only, and it reads the handler through a ref: an effect that ACTS
    // must not re-bind on every render (CODE-REVIEW.md).
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') cancel.current();
    };
    document.addEventListener('keydown', onKey);
    box.current?.focus();
    return () => document.removeEventListener('keydown', onKey);
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
