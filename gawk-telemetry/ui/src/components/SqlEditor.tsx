import { useRef } from 'react';

import { tokenize } from '../lib/sqlTokens.ts';
import styles from './SqlEditor.module.css';

// The SQL console's editor (R36, docs/86 TU10): a plain <textarea> — so
// selection, undo, IME and every browser affordance stay native — laid over a
// highlighted copy of its own text, with line numbers in a gutter. Nothing is
// parsed beyond a tokenizer; a highlight that disagreed with the engine would
// be the only harm, and the engine has the last word on Run.

interface Props {
  value: string;
  onChange: (v: string) => void;
  onRun: () => void;
  placeholder?: string;
}

export function SqlEditor({ value, onChange, onRun, placeholder }: Props) {
  const pre = useRef<HTMLPreElement | null>(null);
  const gutter = useRef<HTMLDivElement | null>(null);
  const lines = Math.max(1, value.split('\n').length);
  const rows = Math.min(24, Math.max(7, lines + 1));

  return (
    <div className={styles.editor}>
      <div className={styles.gutter} ref={gutter} aria-hidden>
        {Array.from({ length: Math.max(lines, rows) }, (_, i) => (
          <span key={i}>{i + 1}</span>
        ))}
      </div>
      <div className={styles.code}>
        <pre className={styles.hl} ref={pre} aria-hidden>
          {tokenize(value).map((tk, i) =>
            tk.t === 'tx' ? (
              tk.s
            ) : (
              <span key={i} className={styles[tk.t]}>
                {tk.s}
              </span>
            ),
          )}
          {/* A trailing newline needs a character after it to take height. */}
          {'\n'}
        </pre>
        <textarea
          className={styles.input}
          value={value}
          spellCheck={false}
          rows={rows}
          placeholder={placeholder}
          aria-label="SQL"
          wrap="off"
          onChange={(e) => onChange(e.target.value)}
          onScroll={(e) => {
            const el = e.currentTarget;
            if (pre.current) {
              pre.current.scrollTop = el.scrollTop;
              pre.current.scrollLeft = el.scrollLeft;
            }
            if (gutter.current) gutter.current.scrollTop = el.scrollTop;
          }}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault();
              onRun();
            }
          }}
        />
      </div>
    </div>
  );
}
