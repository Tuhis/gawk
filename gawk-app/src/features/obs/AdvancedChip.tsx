import styles from './obs.module.css';

// R33 (docs/83 §5.5 cue 1): the neutral ADVANCED chip. An outline pill in
// --border/--muted, never amber: in gawk amber means something is wrong.
export function AdvancedChip() {
  return (
    <span className={styles.chip} data-testid="advanced-chip">
      Advanced
    </span>
  );
}
