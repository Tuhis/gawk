import type { Severity } from '../api/types.ts';
import { GLYPH } from '../lib/severity.ts';
import styles from './SeverityBadge.module.css';

interface Props {
  severity: Severity;
  /** `compact` drops the word where the row already names the state nearby. */
  compact?: boolean;
}

/**
 * Glyph + word + hue. Never hue alone — the page must survive a colour-blind
 * reader, a greyscale screenshot and the CSS failing to load, and in the last
 * case the glyph and word are all that is left.
 */
export function SeverityBadge({ severity, compact }: Props) {
  return (
    <span className={`${styles.badge} ${styles[severity]}`} title={severity}>
      <span aria-hidden>{GLYPH[severity]}</span>
      {!compact && <span className={styles.word}>{severity}</span>}
      {compact && <span className={styles.srOnly}>{severity}</span>}
    </span>
  );
}
