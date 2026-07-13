import { useRef } from 'react';
import styles from './CodeInput.module.css';
import { BROADCAST_ID_LENGTH, sanitizeBroadcastId } from '../../lib/broadcastId';

interface Props {
  value: string;
  onChange: (value: string) => void;
  onComplete?: (value: string) => void;
  onEnter?: () => void;
  length?: number;
  autoFocus?: boolean;
  ariaLabel?: string;
}

// Segmented code entry (docs/10 J2). One real, visually-hidden <input> captures
// all typing/paste/backspace; the N boxes are presentational, rendered from the
// value. This makes paste, caret handling, and IME "just work" (the browser
// owns the field) while we still sanitize to the code alphabet on every change.
// The active box is the next empty slot, which reads as auto-advance.
export function CodeInput({
  value,
  onChange,
  onComplete,
  onEnter,
  length = BROADCAST_ID_LENGTH,
  autoFocus,
  ariaLabel = 'Broadcast code',
}: Props) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const activeIndex = Math.min(value.length, length - 1);

  return (
    <div className={styles.wrap} onClick={() => inputRef.current?.focus()}>
      <div className={styles.boxes} aria-hidden="true">
        {Array.from({ length }, (_, i) => (
          <div
            key={i}
            className={[
              styles.box,
              value[i] ? styles.filled : '',
              i === activeIndex ? styles.active : '',
            ]
              .filter(Boolean)
              .join(' ')}
            data-active={i === activeIndex ? 'true' : undefined}
          >
            {value[i] ?? ''}
          </div>
        ))}
      </div>
      <input
        ref={inputRef}
        className={styles.input}
        value={value}
        onChange={(e) => {
          const clean = sanitizeBroadcastId(e.target.value);
          if (clean === value) return;
          onChange(clean);
          if (clean.length === length) onComplete?.(clean);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') onEnter?.();
        }}
        inputMode="text"
        autoComplete="one-time-code"
        autoCapitalize="characters"
        spellCheck={false}
        maxLength={length}
        aria-label={ariaLabel}
        // eslint-disable-next-line jsx-a11y/no-autofocus -- the code field is
        // the sole purpose of the landing page; focusing it is expected.
        autoFocus={autoFocus}
      />
    </div>
  );
}
