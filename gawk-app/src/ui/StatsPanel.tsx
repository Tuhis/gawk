import type { ReactNode } from 'react';
import styles from './statsPanel.module.css';
import { GlassPanel } from './GlassPanel';
import { IconButton } from './IconButton';
import { CloseIcon } from './Icons';

// The shared sectioned stats overlay: the viewer and broadcaster overlays are
// thin row-builders over this. Values arrive
// pre-formatted ("—" for unavailable) so the panel stays purely presentational.

// The optional third element renders as a hover tooltip on the value.
export type StatsRow = [label: string, value: string, title?: string];

export interface StatsSection {
  title: string;
  rows: StatsRow[];
}

interface Props {
  ariaLabel: string;
  sections: StatsSection[];
  footer?: ReactNode;
  onClose: () => void;
  // Copy diagnostics: the owning screen serializes and writes the clipboard;
  // the panel hosts the button and the "Copied" flash.
  onCopy?: () => void;
  copied?: boolean;
}

export function StatsPanel({ ariaLabel, sections, footer, onClose, onCopy, copied }: Props) {
  return (
    <GlassPanel className={styles.overlay} role="dialog" aria-label={ariaLabel}>
      <div className={styles.head}>
        <span className={styles.title}>Stats</span>
        <IconButton label="Close stats" className={styles.close} onClick={onClose}>
          <CloseIcon />
        </IconButton>
      </div>
      <div className={styles.body}>
        {sections.map((s) => (
          <section key={s.title} className={styles.section}>
            <h3 className={styles.sectionTitle}>{s.title}</h3>
            <dl className={styles.grid}>
              {s.rows.map(([label, value, title]) => (
                <div key={label} className={styles.row}>
                  <dt>{label}</dt>
                  <dd title={title}>{value}</dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
      <div className={styles.foot}>
        {onCopy && (
          <button type="button" className={styles.copyBtn} onClick={onCopy}>
            {copied ? 'Copied' : 'Copy diagnostics'}
          </button>
        )}
        {footer && <div className={styles.footNote}>{footer}</div>}
      </div>
    </GlassPanel>
  );
}
