import { useRef } from 'react';

import type { ColumnSpec, Columns } from '../lib/columns.ts';
import t from './tables.module.css';

// The furniture of an operator-resizable table (R36 item 1, docs/86 TU2):
// the grip on each header boundary, the line that follows a drag, and the
// footer that says how the widths were chosen and offers to reset them.

/**
 * A grip on a header's right edge. Drag to resize; double-click to fit the
 * column to its content; arrow keys nudge it by 16 px for keyboard users.
 */
export function Grip<T>({ columns, col }: { columns: Columns; col: ColumnSpec<T> }) {
  const start = useRef<{ x: number; w: number } | null>(null);
  const width = columns.widths[col.key];
  const dragging = columns.drag?.key === col.key;

  return (
    <span
      role="separator"
      aria-orientation="vertical"
      aria-label={`Resize the ${col.label} column`}
      aria-valuenow={width}
      tabIndex={0}
      title="Drag to resize · double-click to fit"
      className={`${t.grip} ${dragging ? t.gripDrag : ''}`}
      onPointerDown={(e) => {
        e.preventDefault();
        e.stopPropagation();
        (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
        start.current = { x: e.clientX, w: width };
      }}
      onPointerMove={(e) => {
        if (!start.current) return;
        columns.setWidth(col.key, start.current.w + (e.clientX - start.current.x), false);
      }}
      onPointerUp={(e) => {
        if (!start.current) return;
        columns.setWidth(col.key, start.current.w + (e.clientX - start.current.x), true);
        start.current = null;
      }}
      onPointerCancel={() => {
        if (start.current) columns.setWidth(col.key, start.current.w, true);
        start.current = null;
      }}
      onDoubleClick={(e) => {
        e.stopPropagation();
        columns.fit(col.key);
      }}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
          e.preventDefault();
          columns.setWidth(col.key, width + (e.key === 'ArrowRight' ? 16 : -16), true);
        } else if (e.key === 'Enter') {
          e.preventDefault();
          columns.fit(col.key);
        }
      }}
    />
  );
}

/**
 * The line a drag draws down the whole table, with the width it would set.
 * Positioned from the widths themselves, so it needs no DOM measurement.
 */
export function DragLine<T>({ columns, cols, offset = 0 }: { columns: Columns; cols: ColumnSpec<T>[]; offset?: number }) {
  const drag = columns.drag;
  if (!drag) return null;
  let left = offset;
  for (const c of cols) {
    left += c.key === drag.key ? drag.px : columns.widths[c.key];
    if (c.key === drag.key) break;
  }
  const label = cols.find((c) => c.key === drag.key)?.label ?? drag.key;
  return (
    <>
      <div className={t.dragLine} style={{ left }} aria-hidden />
      <span className={t.dragHint} style={{ left }} aria-hidden>
        {label} · {drag.px} px — double-click to fit
      </span>
    </>
  );
}

/** "column widths: fitted to content, client widened by you" + reset. */
export function WidthsNote<T>({ columns, cols }: { columns: Columns; cols: ColumnSpec<T>[] }) {
  const names = columns.overridden
    .map((k) => cols.find((c) => c.key === k)?.label)
    .filter(Boolean)
    .join(', ');
  return (
    <>
      <span>
        column widths: fitted to content
        {names ? `, ${names} set by you` : ''}
      </span>
      {columns.overridden.length > 0 && (
        <button type="button" className={t.reset} onClick={columns.reset}>
          reset widths
        </button>
      )}
    </>
  );
}
