import type { ReactNode } from 'react';
import styles from './debug.module.css';

const LINKS = [
  ['#/debug/broadcast', 'Broadcast'],
  ['#/debug/view', 'View'],
  ['#/debug/loopback', 'Loopback'],
] as const;

// Chrome for the frozen diagnostic pages (docs/10 Decision 1). Not linked from
// the production UI — reachable only by typing #/debug. Kept deliberately
// plain; the production surfaces do not share components with it.
export function DebugShell({ active, children }: { active?: string; children: ReactNode }) {
  return (
    <>
      <nav className={styles.nav}>
        <a className={styles.brand} href="#/">
          gawk
        </a>
        <span className={styles.tag}>debug</span>
        {LINKS.map(([hash, label]) => (
          <a key={hash} href={hash} className={active === hash ? styles.activeLink : undefined}>
            {label}
          </a>
        ))}
        <a className={styles.exit} href="#/">
          ← exit debug
        </a>
      </nav>
      {children}
    </>
  );
}
