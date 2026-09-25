import type { ReactNode } from 'react';
import styles from './Toast.module.css';

// The transient bottom-centre pill every surface flashes ("Link copied"). The
// parent owns the timing: render it while the message should show.
export function Toast({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div role="status" className={[styles.toast, className].filter(Boolean).join(' ')}>
      {children}
    </div>
  );
}
