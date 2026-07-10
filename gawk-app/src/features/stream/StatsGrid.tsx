import styles from './stream.module.css';

interface Props {
  items: Array<[label: string, value: string]>;
}

export function StatsGrid({ items }: Props) {
  return (
    <div className={styles.statsPanel}>
      <div className={styles.statsGrid}>
        {items.map(([label, value]) => (
          <div key={label} className={styles.statItem}>
            <span className={styles.statLabel}>{label}</span>
            <span className={styles.statValue}>{value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
