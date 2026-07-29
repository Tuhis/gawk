import { useEffect, useRef } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';

import styles from './VirtualRows.module.css';

// UD12's virtualization, as one small component.
//
// TanStack Virtual because it is headless and tiny: it computes which indices
// are visible and nothing else. A table component would have brought its own
// markup, its own styling and its own opinions about sorting — and sorting is
// server-side here (UD4), so most of what such a library offers would have to
// be switched off.
//
// The measured criterion this exists for: **2 000 rows scroll at 60 fps and
// hold bounded memory.** A tab left open for a day must not grow, which is why
// the row height is fixed rather than measured — dynamic measurement retains a
// cache entry per row ever rendered.

interface Props<T> {
  rows: T[];
  rowHeight: number;
  height: number;
  renderRow: (row: T, index: number) => React.ReactNode;
  keyOf: (row: T, index: number) => string;
  ariaLabel?: string;
  /** Called when the viewport nears the end — the cursor-paging trigger. */
  onEndReached?: () => void;
}

export function VirtualRows<T>({
  rows,
  rowHeight,
  height,
  renderRow,
  keyOf,
  ariaLabel,
  onEndReached,
}: Props<T>) {
  const parentRef = useRef<HTMLDivElement | null>(null);
  const virtual = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => rowHeight,
    // A few rows of headroom, so a fast scroll does not show blanks. More than
    // this buys nothing and costs DOM.
    overscan: 8,
  });

  const items = virtual.getVirtualItems();
  const lastIndex = items.length ? items[items.length - 1].index : -1;
  // The paging trigger fires from an EFFECT, not from the render body. Calling
  // a parent's setState during render is how a "load more when you near the
  // end" hook becomes a render loop; an effect runs after commit and only when
  // the visible window actually moved.
  useEffect(() => {
    if (!onEndReached || rows.length === 0) return;
    if (lastIndex >= rows.length - 5) onEndReached();
  }, [onEndReached, lastIndex, rows.length]);

  return (
    <div
      ref={parentRef}
      className={styles.scroller}
      style={{ height }}
      role="list"
      aria-label={ariaLabel}
    >
      <div className={styles.spacer} style={{ height: virtual.getTotalSize() }}>
        {items.map((v) => (
          <div
            key={keyOf(rows[v.index], v.index)}
            className={styles.row}
            role="listitem"
            style={{ height: rowHeight, transform: `translateY(${v.start}px)` }}
          >
            {renderRow(rows[v.index], v.index)}
          </div>
        ))}
      </div>
    </div>
  );
}
