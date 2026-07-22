import { useEffect, useRef, useState, type RefObject } from 'react';
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
  // Which corner of the menu (x, y) is. Default 'top-left' — a pointer
  // position the menu grows down-right from, i.e. a right-click. Anchoring to
  // a *button* needs 'bottom-right' instead, so the menu grows up-left and
  // never covers the control that opened it (the viewer's overflow button
  // sits in the bottom control bar).
  anchor?: 'top-left' | 'bottom-right';
  // The control that owns the menu (the button that opened it), when there is
  // one. A pointerdown inside it does not count as "outside": otherwise this
  // listener closes the menu and the button's own click re-opens it, so the
  // button can never dismiss what it opened. Whether that race is even
  // visible depends on when React flushes the close between the two
  // listeners — jsdom (act) and Chrome disagree, which is exactly why the
  // toggle must not depend on it.
  anchorRef?: RefObject<HTMLElement | null>;
  onClose: () => void;
}

// Distance kept from every viewport edge.
const PAD = 8;

// A small controlled context menu: the parent tracks open state + coordinates
// and renders this when open. Closes on outside-click, Esc, blur, or after an
// item fires. Keyboard-navigable (Up/Down/Enter/Esc). Positioned fixed and
// clamped into the viewport.
export function ContextMenu({ items, x, y, anchor = 'top-left', anchorRef, onClose }: Props) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [active, setActive] = useState(0);
  // Measured from the top-left pad corner, never from the requested position:
  // a fixed element's shrink-to-fit width is computed against the space left
  // of the viewport edge, so measuring it where it was asked to appear can
  // return a squeezed width/height — which then feeds the placement math and
  // lands the menu somewhere else entirely. Neutral corner ⇒ true size.
  const [pos, setPos] = useState({ x: PAD, y: PAD });
  // Placement needs the rendered size, so the first paint would otherwise
  // flash at the unresolved position — a whole menu-height jump on the
  // bottom-right anchor. Hidden (not unmounted: it must be measurable).
  const [placed, setPlaced] = useState(false);

  // Resolve the anchor and clamp into the viewport once mounted (so it never
  // overflows off-screen).
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    // offsetWidth/Height, not getBoundingClientRect: the open animation
    // starts at scale(0.97), so the visual box under-reports the layout box
    // by 3 % — enough to drift the menu off its anchor and onto the button.
    const width = el.offsetWidth;
    const height = el.offsetHeight;
    const rawX = anchor === 'bottom-right' ? x - width : x;
    const rawY = anchor === 'bottom-right' ? y - height : y;
    const nx = Math.min(rawX, window.innerWidth - width - PAD);
    const ny = Math.min(rawY, window.innerHeight - height - PAD);
    setPos({ x: Math.max(PAD, nx), y: Math.max(PAD, ny) });
    setPlaced(true);
    el.focus();
  }, [x, y, anchor]);

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (anchorRef?.current?.contains(target)) return;
      if (ref.current && !ref.current.contains(target)) onClose();
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
  }, [onClose, anchorRef]);

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
      style={{ left: pos.x, top: pos.y, visibility: placed ? undefined : 'hidden' }}
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
