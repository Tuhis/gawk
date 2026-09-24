import { useEffect, useRef } from 'react';
import styles from './room.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { CREATOR_HELP } from './roomCopy';

interface Props {
  // Controlled by the room view, which counts an open help as an open
  // overlay so the header does not fade while it is being read.
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// The creator's role, made visible.
// A chip beside the room code; a click opens what the role can (and cannot)
// do. Escape or a click outside closes it.
export function CreatorBadge({ open, onOpenChange }: Props) {
  const wrapRef = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) onOpenChange(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onOpenChange(false);
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open, onOpenChange]);

  return (
    <span ref={wrapRef} className={styles.creatorWrap}>
      <button
        type="button"
        className={styles.creatorBadge}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
      >
        Creator
      </button>
      {open && (
        <GlassPanel className={styles.creatorHelp} role="dialog" aria-label="Your role in this room">
          <strong className={styles.creatorHelpTitle}>{CREATOR_HELP.title}</strong>
          <ul className={styles.creatorHelpList}>
            {CREATOR_HELP.points.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </GlassPanel>
      )}
    </span>
  );
}
