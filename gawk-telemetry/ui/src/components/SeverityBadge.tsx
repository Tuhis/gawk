import type { Severity } from '../api/types.ts';
import { GLYPH } from '../lib/severity.ts';
import styles from './SeverityBadge.module.css';

interface Props {
  severity: Severity;
  /** `compact` drops the visible word where the row already names the state nearby. */
  compact?: boolean;
}

/**
 * Glyph + word + hue. Never hue alone — the page must survive a colour-blind
 * reader, a greyscale screenshot and the CSS failing to load, and in the last
 * case the glyph and word are all that is left.
 */
export function SeverityBadge({ severity, compact }: Props) {
  return (
    <span
      className={`${styles.badge} ${styles[severity]} ${compact ? styles.compact : ''}`}
      title={severity}
    >
      <span className={styles.glyph} aria-hidden>
        {GLYPH[severity]}
      </span>
      {!compact && <span className={styles.word}>{severity}</span>}
      {compact && <span className={styles.srOnly}>{severity}</span>}
    </span>
  );
}

/**
 * The quietest severity mark: a dot. Only where the verdict is written out
 * right beside it, so hue is still never the sole carrier — and the word stays
 * in the accessibility tree.
 */
export function SeverityDot({ severity }: { severity: Severity }) {
  return (
    <span className={`${styles.dot} ${styles[severity]}`} title={severity}>
      <span className={styles.srOnly}>{severity}</span>
    </span>
  );
}
