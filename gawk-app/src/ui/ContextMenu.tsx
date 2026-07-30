import { useEffect, useId, useRef, useState, type RefObject } from 'react';
import styles from './ContextMenu.module.css';

export interface MenuItem {
  label: string;
  onSelect: () => void;
  // R32 UX1: selection state, rendered as a mark *and* as ARIA. Before this,
  // callers glued a '✓' onto the label string, so the accessible name changed
  // when only the state did and no assistive technology was told the item was
  // a choice at all. `undefined` keeps a plain `menuitem`.
  checked?: boolean;
  // Present but not applicable. The option still renders — removing it is what
  // made the viewer's menu change length with the delivery mode (docs/37 §1.2)
  // — it is just inert, skipped by the keyboard, and carries `reason`.
  disabled?: boolean;
  // Why a disabled item is unavailable. Visible text, never a tooltip: touch
  // has no hover, and a grayed row with no explanation is worse than an absent
  // one (docs/37 decision 4).
  reason?: string;
  // A quiet second line on an *enabled* item — the cost or consequence of
  // choosing it (e.g. "· switching reconnects").
  note?: string;
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
  // The first *enabled* item, not index 0: a menu whose head is inert would
  // otherwise do nothing on Enter.
  const [active, setActive] = useState(() => items.findIndex((it) => !it.disabled));
  // Height available to the menu. Kept in state alongside the position because
  // both come from the same measurement pass.
  const [maxHeight, setMaxHeight] = useState<number | null>(null);
  // Stable id root for the per-item label/description wiring below.
  const baseId = useId();
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
    // R32 UX1.1: a menu taller than the viewport used to render its tail below
    // the screen with no way to reach it — the clamp below floors `top` at
    // PAD, which keeps the *head* on screen and says nothing about the tail.
    // Cap the height here (rather than in CSS) so the placement math below
    // measures the box the user will actually see: an uncapped offsetHeight
    // would push a bottom-right anchor far off the top of the viewport.
    const available = window.innerHeight - PAD * 2;
    const height = Math.min(el.offsetHeight, available);
    setMaxHeight(available);
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
    // A disabled item is inert on every path — it is present to explain
    // itself, not to be picked. Deliberately not the `disabled` attribute: an
    // aria-disabled button stays discoverable to assistive technology, which
    // is the only way its `reason` gets read.
    if (!item || item.disabled) return;
    onClose();
    item.onSelect();
  };

  // Move to the next enabled item, wrapping. Returns `from` when nothing is
  // selectable, so an all-disabled menu can't spin.
  const step = (from: number, dir: 1 | -1) => {
    const n = items.length;
    if (n === 0) return from;
    let i = from;
    for (let k = 0; k < n; k++) {
      i = (i + dir + n) % n;
      if (!items[i]?.disabled) return i;
    }
    return from;
  };

  return (
    <div
      ref={ref}
      className={styles.menu}
      role="menu"
      tabIndex={-1}
      style={{
        left: pos.x,
        top: pos.y,
        visibility: placed ? undefined : 'hidden',
        ...(maxHeight != null ? { maxHeight, overflowY: 'auto' as const } : {}),
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault();
          onClose();
        } else if (e.key === 'ArrowDown') {
          e.preventDefault();
          setActive((a) => step(a, 1));
        } else if (e.key === 'ArrowUp') {
          e.preventDefault();
          setActive((a) => step(a, -1));
        } else if (e.key === 'Enter') {
          e.preventDefault();
          choose(active);
        }
      }}
    >
      {items.map((item, i) => {
        // A disabled item shows why; an enabled one may show what it costs.
        const secondary = item.disabled ? item.reason : item.note;
        const labelId = `${baseId}-l${i}`;
        const noteId = `${baseId}-n${i}`;
        return (
          <button
            key={item.label}
            type="button"
            role={item.checked == null ? 'menuitem' : 'menuitemradio'}
            {...(item.checked == null ? {} : { 'aria-checked': item.checked })}
            {...(item.disabled ? { 'aria-disabled': true } : {})}
            // Name is the label; the second line is a *description*. Folding
            // the cost line into the name would make every row's name a
            // sentence, which is worse to navigate by and would defeat the
            // point of UX1.2 — a stable, state-free accessible name.
            aria-labelledby={labelId}
            {...(secondary ? { 'aria-describedby': noteId } : {})}
            className={[styles.item, i === active && !item.disabled ? styles.active : '']
              .filter(Boolean)
              .join(' ')}
            onMouseEnter={() => !item.disabled && setActive(i)}
            onClick={() => choose(i)}
          >
            {/* The check mark is drawn by CSS off aria-checked, never as a text
                node: a rendered '✓' would put the state into textContent, i.e.
                into the accessible name, which is exactly the defect this
                replaces (labels used to be built as `'Paced playback ✓'`). */}
            <span id={labelId} className={styles.itemLabel}>
              {item.label}
            </span>
            {secondary && (
              <span id={noteId} className={styles.itemNote}>
                {secondary}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
