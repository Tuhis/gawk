import type { ReactNode } from 'react';
import styles from './Toast.module.css';

// The transient bottom-centre pill every surface flashes ("Link copied",
// "tuhis' stream was removed from the room"). Promoted from the viewer's and
// broadcaster's private copies for R42, whose room view needs it too — one
// pill, one animation, one place to change it. The parent owns the timing:
// render it while the message should show, unmount it after.
export function Toast({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <div role="status" className={[styles.toast, className].filter(Boolean).join(' ')}>
      {children}
    </div>
  );
}
