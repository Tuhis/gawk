import { useCallback, useId, useRef } from 'react';
import styles from './room.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { ScreenIcon, VideoOffIcon } from '../../ui/Icons';
import { fmtWatching } from '../../lib/format';
import { useDismiss } from '../../lib/useDismiss';
import type { RoomVisibility } from './roomPrefs';
import type { NumberedStream } from './roomVisibility';
import { VISIBILITY_KEYS_NOTE, VISIBILITY_OPTIONS, VISIBILITY_TITLE } from './roomCopy';

export interface VisibilityControlProps {
  visibility: RoomVisibility;
  // Every stream in the room, in room order, and which of them are shown.
  streams: NumberedStream[];
  shownIds: ReadonlySet<string>;
  // The broadcaster's own stream: marked "you", and None reads Preview only.
  ownId: string | null;
  preview: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onVisibility: (v: RoomVisibility) => void;
  onToggle: (broadcastId: string) => void;
}

// R86 (docs/90 D1, D11): the footer pill that says what is on the stage, and
// its popover: All streams · Choose · None, then under Choose a checkbox per
// stream. The three are radio buttons like the layout segment's — a click or
// Enter picks one, and arrow keys don't, because each pick opens or closes
// media sessions.
export function VisibilityControl({
  visibility,
  streams,
  shownIds,
  ownId,
  preview,
  open,
  onOpenChange,
  onVisibility,
  onToggle,
}: VisibilityControlProps) {
  const rootRef = useRef<HTMLSpanElement | null>(null);
  const pillRef = useRef<HTMLButtonElement | null>(null);
  const baseId = useId();

  // A press outside closes it; Escape closes back onto the pill.
  useDismiss(
    rootRef,
    open,
    useCallback(
      (via: 'outside' | 'escape') => {
        onOpenChange(false);
        if (via === 'escape') pillRef.current?.focus();
      },
      [onOpenChange],
    ),
  );

  const none = preview ? VISIBILITY_OPTIONS.preview : VISIBILITY_OPTIONS.none;
  const counted = `${shownIds.size} of ${streams.length}`;
  const text = visibility === 'all' ? VISIBILITY_OPTIONS.all.label : visibility === 'none' ? none.label : counted;
  // A phone's footer has no room for the words beside the quality pill: "All",
  // "3/5", and None as its icon alone. The full text stays the name.
  const short = visibility === 'all' ? 'All' : visibility === 'none' ? '' : `${shownIds.size}/${streams.length}`;

  const option = (v: RoomVisibility, copy: { label: string; note: string }) => (
    <button
      type="button"
      role="radio"
      aria-checked={visibility === v}
      className={styles.visOption}
      onClick={() => onVisibility(v)}
      aria-labelledby={`${baseId}-${v}-l`}
      aria-describedby={`${baseId}-${v}-n`}
    >
      <span className={styles.visRing} aria-hidden="true" />
      <span className={styles.visOptionText}>
        <span id={`${baseId}-${v}-l`}>{copy.label}</span>
        <span id={`${baseId}-${v}-n`} className={styles.visNote}>
          {copy.note}
        </span>
      </span>
    </button>
  );

  return (
    <span ref={rootRef} className={styles.visWrap}>
      <button
        ref={pillRef}
        type="button"
        className={styles.visPill}
        data-vis={visibility}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={`${VISIBILITY_TITLE}: ${text}`}
        onClick={() => onOpenChange(!open)}
      >
        {visibility === 'none' && (preview ? <ScreenIcon /> : <VideoOffIcon />)}
        <span className={styles.visLong}>{text}</span>
        {short !== '' && <span className={styles.visShort}>{short}</span>}
        <span className={styles.presetCaret} aria-hidden="true">
          ▾
        </span>
      </button>
      {/* After the pill in the DOM, so Tab goes from the pill into it. */}
      {open && (
        <GlassPanel
          className={styles.visPopover}
          role="dialog"
          aria-label={VISIBILITY_TITLE}
          data-testid="visibility-popover"
        >
          <div role="radiogroup" aria-label={VISIBILITY_TITLE} className={styles.visGroup}>
            {option('all', VISIBILITY_OPTIONS.all)}
            {option('some', VISIBILITY_OPTIONS.some)}
            {option('none', none)}
          </div>
          {/* Its own group, not inside the radiogroup, which may own radios
              only. */}
          {visibility === 'some' && (
            <div role="group" aria-labelledby={`${baseId}-streams`} className={styles.visStreams}>
              <span id={`${baseId}-streams`} className={styles.visStreamsTitle}>
                Streams · {counted} shown
              </span>
              {streams.length === 0 ? (
                <p className={styles.visNote}>Nobody is streaming yet.</p>
              ) : (
                <ul className={styles.visList}>
                  {streams.map(({ attachment: a, index }) => {
                    const shown = shownIds.has(a.broadcastId);
                    const nameId = `${baseId}-s${index}`;
                    const noteId = `${baseId}-s${index}-n`;
                    return (
                      <li key={a.broadcastId}>
                        <label className={styles.visPick} data-on={shown ? 'true' : 'false'}>
                          <input
                            type="checkbox"
                            checked={shown}
                            onChange={() => onToggle(a.broadcastId)}
                            aria-labelledby={nameId}
                            aria-describedby={noteId}
                          />
                          <span className={styles.visKey} aria-hidden="true">
                            {index}
                          </span>
                          <span id={nameId} className={styles.visName}>
                            {a.label || a.broadcastId}
                          </span>
                          <span id={noteId} className={styles.visNote} data-live={a.live ? 'true' : 'false'}>
                            {a.broadcastId === ownId ? 'you' : a.live ? fmtWatching(a.viewerCount) : 'away'}
                          </span>
                        </label>
                      </li>
                    );
                  })}
                </ul>
              )}
            </div>
          )}
          <p className={[styles.visNote, styles.visKeys].join(' ')}>{VISIBILITY_KEYS_NOTE}</p>
        </GlassPanel>
      )}
    </span>
  );
}
