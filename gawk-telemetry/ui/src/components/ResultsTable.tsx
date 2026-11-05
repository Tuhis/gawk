import { useEffect, useMemo, useRef, useState } from 'react';

import type { QueryResult } from '../api/types.ts';
import { objectLoadKey, useColumns, type ColumnSpec } from '../lib/columns.ts';
import { cell } from '../lib/cells.ts';
import { copyToClipboard } from '../lib/export.ts';
import { DragLine, Grip, WidthsNote } from './Columns.tsx';
import t from './tables.module.css';
import styles from './ResultsTable.module.css';
import view from '../views/view.module.css';

// The SQL console's result grid (R36 item 1, docs/86 TU10).
//
// R31 ellipsised every cell past 40ch with no title, so a truncated value
// could not be read at all. Here every column fits its content up to a cap,
// the operator can widen any of them, and a cell that still does not fit
// opens in a popover — click it, or focus it and press Enter — with the whole
// value and a copy button.

type Row = unknown[];

function looksNumeric(rows: Row[], i: number): boolean {
  let seen = 0;
  for (const r of rows.slice(0, 50)) {
    const v = r[i];
    if (v === null || v === undefined || v === '') continue;
    if (typeof v !== 'number') return false;
    seen++;
  }
  return seen > 0;
}

interface Open {
  row: number;
  col: number;
  top: number;
  left: number;
}

export function ResultsTable({ result }: { result: QueryResult }) {
  const cols = useMemo<ColumnSpec<Row>[]>(
    () =>
      result.columns.map((name, i) => ({
        key: `${i}:${name}`,
        label: name,
        text: (r: Row) => cell(r[i]),
        max: 360,
        min: 64,
        flex: i === result.columns.length - 1 && result.columns.length > 1,
        flexMin: 200,
      })),
    [result.columns],
  );
  const numeric = useMemo(() => result.columns.map((_, i) => looksNumeric(result.rows, i)), [result]);
  const columns = useColumns(`sql:${result.columns.join(',')}`, cols, result.rows, objectLoadKey(result));
  const [open, setOpen] = useState<Open | null>(null);
  const frame = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(null);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open]);
  useEffect(() => setOpen(null), [result]);

  const openCell = (el: HTMLElement, row: number, col: number) => {
    const box = frame.current?.getBoundingClientRect();
    const r = el.getBoundingClientRect();
    if (!box) return;
    setOpen({
      row,
      col,
      top: r.bottom - box.top + 4,
      left: Math.max(8, Math.min(r.left - box.left, box.width - 448)),
    });
  };

  const value = open ? result.rows[open.row]?.[open.col] : undefined;
  const full = typeof value === 'object' && value !== null ? JSON.stringify(value, null, 2) : cell(value);

  return (
    <section className={`${view.panel} ${styles.frame}`} ref={frame}>
      <div className={t.wrap}>
        <table className={t.tbl} style={{ minWidth: columns.minTotal }}>
          <colgroup>
            {cols.map((c) => (
              <col key={c.key} style={c.flex ? undefined : { width: columns.widths[c.key] }} />
            ))}
          </colgroup>
          <thead>
            <tr>
              {cols.map((c, i) => (
                <th key={c.key} className={numeric[i] ? t.r : undefined} title={c.label}>
                  {c.label}
                  {!c.flex && <Grip columns={columns} col={c} />}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {result.rows.map((r, ri) => (
              <tr key={ri}>
                {r.map((v, ci) => {
                  const text = cell(v);
                  const isOpen = open?.row === ri && open.col === ci;
                  return (
                    <td
                      key={ci}
                      className={`${numeric[ci] ? t.r : ''} ${isOpen ? styles.cellOpen : ''} ${styles.cell}`}
                      title={text.length > 24 ? text : undefined}
                      tabIndex={text.length > 24 ? 0 : undefined}
                      onClick={(e) => {
                        const el = e.currentTarget;
                        if (el.scrollWidth > el.clientWidth || text.includes('\n')) openCell(el, ri, ci);
                      }}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') openCell(e.currentTarget, ri, ci);
                      }}
                    >
                      {v === null ? <span className={styles.null}>null</span> : text}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
        <DragLine columns={columns} cols={cols} />
      </div>
      {open && (
        <div className={styles.pop} style={{ top: open.top, left: open.left }} role="dialog" aria-label="Full cell value">
          <div className={styles.popHead}>
            <span className={view.eyebrow}>
              {result.columns[open.col]} · row {open.row + 1}
            </span>
            <span className={view.spacer} />
            <button
              type="button"
              className={`${view.button} ${view.buttonQuiet} ${view.buttonSmall}`}
              onClick={() => void copyToClipboard(full)}
            >
              copy
            </button>
            <button
              type="button"
              className={`${view.button} ${view.buttonQuiet} ${view.buttonSmall}`}
              aria-label="Close"
              onClick={() => setOpen(null)}
            >
              ×
            </button>
          </div>
          <pre className={styles.popValue}>{full}</pre>
        </div>
      )}
      <div className={t.foot}>
        <span>A cell that does not fit ends in an ellipsis; click it, or focus it and press Enter, to read and copy the whole value.</span>
        <span className={view.spacer} />
        <WidthsNote columns={columns} cols={cols} />
      </div>
    </section>
  );
}
