import type { HTMLAttributes } from 'react';
import styles from './GlassPanel.module.css';

// Translucent, blurred surface for anything that floats over live pixels
// (viewer/broadcaster chrome, the landing card, menus, the stats overlay).
export function GlassPanel({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={[styles.glass, className].filter(Boolean).join(' ')} {...rest} />;
}
