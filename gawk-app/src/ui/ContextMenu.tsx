import { useEffect, useRef, useState } from 'react';
import styles from './ContextMenu.module.css';

export interface MenuItem {
  label: string;
  onSelect: () => void;
}

interface Props {
  items: MenuItem[];
  // Viewport coordinates of the click that opened the menu.
  x: number;
  y: number;
  onClose: () => void;
}

// A small controlled context menu: the parent tracks open state + coordinates
// and renders this when open. Closes on outside-click, Esc, blur, or after an
// item fires. Keyboard-navigable (Up/Down/Enter/Esc). Positioned fixed and
// clamped into the viewport.
export function ContextMenu({ items, x, y, onClose }: Props) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [active, setActive] = useState(0);
  const [pos, setPos] = useState({ x, y });

  // Clamp into the viewport once mounted (so it never overflows off-screen).
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const pad = 8;
    const nx = Math.min(x, window.innerWidth - rect.width - pad);
    const ny = Math.min(y, window.innerHeight - rect.height - pad);
    setPos({ x: Math.max(pad, nx), y: Math.max(pad, ny) });
    el.focus();
  }, [x, y]);

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    // Any scroll/resize dismisses, matching native menus.
    const onScroll = () => onClose();
    window.addEventListener('pointerdown', onDown, true);
    window.addEventListener('scroll', onScroll, true);
    window.addEventListener('resize', onScroll);
    return () => {
      window.removeEventListener('pointerdown', onDown, true);
      window.removeEventListener('scroll', onScroll, true);
      window.removeEventListener('resize', onScroll);
    };
  }, [onClose]);

  const choose = (i: number) => {
    const item = items[i];
    if (!item) return;
    onClose();
    item.onSelect();
  };

  return (
    <div
      ref={ref}
      className={styles.menu}
      role="menu"
      tabIndex={-1}
      style={{ left: pos.x, top: pos.y }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault();
          onClose();
        } else if (e.key === 'ArrowDown') {
          e.preventDefault();
          setActive((a) => (a + 1) % items.length);
        } else if (e.key === 'ArrowUp') {
          e.preventDefault();
          setActive((a) => (a - 1 + items.length) % items.length);
        } else if (e.key === 'Enter') {
          e.preventDefault();
          choose(active);
        }
      }}
    >
      {items.map((item, i) => (
        <button
          key={item.label}
          type="button"
          role="menuitem"
          className={[styles.item, i === active ? styles.active : ''].filter(Boolean).join(' ')}
          onMouseEnter={() => setActive(i)}
          onClick={() => choose(i)}
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}
