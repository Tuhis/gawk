import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from 'react';
import { IconButton } from './IconButton';
import { SpeakerIcon, SpeakerLowIcon, SpeakerMutedIcon } from './Icons';
import styles from './VolumeControl.module.css';

// Arrow keys (in the popover) and the wheel (over the button) move the level
// by this much.
const STEP = 0.05;
// Vertical travel before a press on the button becomes a drag rather than a
// tap.
const DRAG_THRESHOLD_PX = 6;
// Drag travel for the whole range: the popover track's length (`.vertical`,
// 8rem at the default root size), so the level moves with the pointer.
const DRAG_RANGE_PX = 128;
// Unmuting a level of zero would still be silent; it comes back at this.
const UNMUTE_LEVEL = 0.25;

const clampLevel = (v: number, max: number) => Math.round(Math.min(max, Math.max(0, v)) * 100) / 100;

// 'focus' opened from a tap or the keyboard (the slider takes focus); 'drag'
// opened by pressing and dragging on the button (focus stays put).
type OpenMode = false | 'focus' | 'drag';

interface Props {
  // Below the narrow breakpoint: the button opens a vertical slider in a
  // popover instead of muting, and the inline slider goes away.
  compact: boolean;
  // The slider's accessible name ("Volume", "Master volume").
  label: string;
  muteLabel: string;
  unmuteLabel: string;
  volume: number;
  muted: boolean;
  onVolume: (volume: number) => void;
  onMutedChange: (muted: boolean) => void;
  // The callers pin their auto-hiding control bar while the popover is open.
  onOpenChange?: (open: boolean) => void;
  className?: string;
  // The top of the range, 1 (100 %) unless a caller allows a boost: a room
  // voice speaker goes to 2 (R81). Above 1 the slider marks where 100 % is.
  max?: number;
}

export function VolumeControl({
  compact,
  label,
  muteLabel,
  unmuteLabel,
  volume,
  muted,
  onVolume,
  onMutedChange,
  onOpenChange,
  className,
  max = 1,
}: Props) {
  const shown = muted ? 0 : volume;
  const pct = Math.round(shown * 100);
  // The ring shows the share of the range, which is the level itself at max 1.
  const ringPct = Math.round((shown / max) * 100);

  const setLevel = (v: number) => {
    const next = clampLevel(v, max);
    onVolume(next);
    if (muted && next > 0) onMutedChange(false);
  };
  const toggleMute = () => {
    if (!muted) {
      onMutedChange(true);
      return;
    }
    if (volume === 0) onVolume(UNMUTE_LEVEL);
    onMutedChange(false);
  };

  const levelIcon = shown === 0 ? <SpeakerMutedIcon /> : shown < 0.5 ? <SpeakerLowIcon /> : <SpeakerIcon />;

  const [mode, setMode] = useState<OpenMode>(false);
  const open = mode !== false;
  const rootRef = useRef<HTMLSpanElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const popoverId = useId();

  const onOpenChangeRef = useRef(onOpenChange);
  useEffect(() => {
    onOpenChangeRef.current = onOpenChange;
  });
  const isOpenRef = useRef(false);
  const setOpen = useCallback((next: OpenMode) => {
    setMode(next);
    const nowOpen = next !== false;
    if (nowOpen !== isOpenRef.current) {
      isOpenRef.current = nowOpen;
      onOpenChangeRef.current?.(nowOpen);
    }
  }, []);

  // Widening past the breakpoint drops the popover; report it closed, or the
  // caller's control bar would stay pinned. Unmounting while open likewise.
  useEffect(() => {
    if (!compact) setOpen(false);
  }, [compact, setOpen]);
  useEffect(
    () => () => {
      if (isOpenRef.current) onOpenChangeRef.current?.(false);
    },
    [],
  );

  // A press anywhere outside closes it. The button is inside, so it toggles.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('pointerdown', onDown, true);
    return () => document.removeEventListener('pointerdown', onDown, true);
  }, [open, setOpen]);

  // Press and drag up on the button: open and set the level in one movement.
  const drag = useRef<{ id: number; startY: number; start: number; active: boolean } | null>(null);
  // The click that ends a drag must not toggle the popover.
  const swallowClick = useRef(false);

  const onPointerDown = (e: ReactPointerEvent<HTMLButtonElement>) => {
    swallowClick.current = false;
    if (e.button !== 0) return;
    drag.current = { id: e.pointerId, startY: e.clientY, start: shown, active: false };
  };
  const onPointerMove = (e: ReactPointerEvent<HTMLButtonElement>) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    const dy = d.startY - e.clientY;
    if (!d.active) {
      if (Math.abs(dy) < DRAG_THRESHOLD_PX) return;
      d.active = true;
      e.currentTarget.setPointerCapture?.(e.pointerId);
      setOpen('drag');
    }
    setLevel(d.start + dy / DRAG_RANGE_PX);
  };
  const endDrag = (e: ReactPointerEvent<HTMLButtonElement>) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    drag.current = null;
    if (d.active) {
      swallowClick.current = true;
      setOpen(false);
    }
  };

  const onPopoverKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      setOpen(false);
      triggerRef.current?.focus();
    } else if ((e.key === 'm' || e.key === 'M') && !e.ctrlKey && !e.metaKey && !e.altKey) {
      toggleMute();
    }
  };
  // The native step (1%) makes the keyboard a hundred presses end to end.
  const onSliderKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    const dir = e.key === 'ArrowUp' || e.key === 'ArrowRight' ? 1 : e.key === 'ArrowDown' || e.key === 'ArrowLeft' ? -1 : 0;
    if (dir === 0) return;
    e.preventDefault();
    setLevel(shown + dir * STEP);
  };

  if (!compact) {
    return (
      <span className={[styles.root, className].filter(Boolean).join(' ')}>
        <IconButton label={muted ? unmuteLabel : muteLabel} onClick={toggleMute}>
          {levelIcon}
        </IconButton>
        {max > 1 ? (
          <span className={styles.inlineWrap}>
            <input
              className={styles.inline}
              type="range"
              min={0}
              max={max}
              step={0.01}
              value={shown}
              aria-label={label}
              aria-valuetext={`${pct}%`}
              onChange={(e) => setLevel(Number(e.target.value))}
            />
            <span className={styles.unity} style={{ left: `${100 / max}%` }} aria-hidden="true" />
          </span>
        ) : (
          <input
            className={styles.inline}
            type="range"
            min={0}
            max={1}
            step={0.01}
            value={shown}
            aria-label={label}
            onChange={(e) => setLevel(Number(e.target.value))}
          />
        )}
      </span>
    );
  }

  return (
    <span ref={rootRef} className={[styles.root, className].filter(Boolean).join(' ')}>
      <IconButton
        ref={triggerRef}
        className={styles.trigger}
        label={muted ? `${label}: muted` : `${label}: ${pct}%`}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? popoverId : undefined}
        onClick={() => {
          if (swallowClick.current) {
            swallowClick.current = false;
            return;
          }
          setOpen(open ? false : 'focus');
        }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onWheel={(e) => {
          if (e.deltaY !== 0) setLevel(shown + (e.deltaY < 0 ? STEP : -STEP));
        }}
      >
        {!open && (
          <svg className={styles.ring} viewBox="0 0 40 40" aria-hidden="true">
            <circle className={styles.ringTrack} cx="20" cy="20" r="18.5" />
            {ringPct > 0 && (
              <circle
                className={styles.ringLevel}
                cx="20"
                cy="20"
                r="18.5"
                pathLength={100}
                strokeDasharray={`${ringPct} 100`}
              />
            )}
          </svg>
        )}
        {levelIcon}
      </IconButton>
      {open && (
        <div id={popoverId} role="dialog" aria-label={label} className={styles.popover} onKeyDown={onPopoverKeyDown}>
          <span className={styles.readout} aria-hidden="true">
            {muted ? 'Off' : pct}
          </span>
          <input
            className={styles.vertical}
            type="range"
            min={0}
            max={max}
            step={0.01}
            value={shown}
            aria-label={label}
            aria-valuetext={muted ? 'Muted' : `${pct}%`}
            autoFocus={mode === 'focus'}
            style={{ '--level': `${ringPct}%` } as CSSProperties}
            onChange={(e) => setLevel(Number(e.target.value))}
            onKeyDown={onSliderKeyDown}
          />
          <span className={styles.divider} aria-hidden="true" />
          <IconButton className={styles.muteToggle} label={muteLabel} aria-pressed={muted} onClick={toggleMute}>
            <SpeakerMutedIcon />
          </IconButton>
        </div>
      )}
    </span>
  );
}
