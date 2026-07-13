import styles from './debug.module.css';

const CARDS = [
  ['#/debug/broadcast', 'Broadcast', 'Publish a screen capture; full encoder + transport stats.'],
  ['#/debug/view', 'View', 'Subscribe by code; full decoder + reassembly stats.'],
  ['#/debug/loopback', 'Loopback', 'Local capture → encode → decode, no network.'],
] as const;

// The troubleshooting story (docs/10): the stats-heavy pages, kept alive but
// off the production paths.
export function DebugIndex() {
  return (
    <div className={styles.index}>
      <h1>Debug surface</h1>
      <p>
        The original diagnostic pages. These carry the full stats grids and are
        the "is it the stream or my machine" tool; the production UI stays out of
        their way.
      </p>
      <div className={styles.cards}>
        {CARDS.map(([hash, title, desc]) => (
          <a key={hash} href={hash} className={styles.card}>
            <b>{title}</b>
            <span>{desc}</span>
          </a>
        ))}
      </div>
    </div>
  );
}
