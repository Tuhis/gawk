import styles from './Sparkline.module.css';

// UD14 promised "a sparkline on every row" and none shipped (R36 item 6). This
// is it: a session's last two minutes of frame rate, beside the number.
//
// The tail is served per broadcast (`/live/sparks`, docs/86 TU7) rather
// than carried in the SSE snapshot, so the live feed stays as small as UD1
// bounds it, and it is polled only while a card is open.
//
// UD9 holds in miniature: a bucket with no sample is a BREAK in the line,
// never a glide; a session with no tail at all shows "—", never a flat line.

interface Props {
  values?: Array<number | null>;
  width?: number;
  height?: number;
  /** Scale ceiling; defaults to the tail's own maximum. */
  max?: number;
  quiet?: boolean;
  label?: string;
}

export function Sparkline({ values, width = 96, height = 20, max, quiet, label }: Props) {
  if (!values || !values.some((v) => v !== null)) {
    return (
      <span className={styles.none} style={{ width }} title="No samples in the last two minutes">
        —
      </span>
    );
  }
  const top = max ?? Math.max(1, ...values.filter((v): v is number => v !== null));
  const n = values.length;
  const x = (i: number) => (n === 1 ? width / 2 : (i * width) / (n - 1));
  const y = (v: number) => height - 1 - (Math.max(0, Math.min(top, v)) / top) * (height - 2);
  let d = '';
  let pen = false;
  // A bucket with no neighbour is a lone `M` — a subpath with no length,
  // which draws nothing. It is still a sample, so it gets a dot.
  const dots: Array<[number, number]> = [];
  values.forEach((v, i) => {
    if (v === null) {
      pen = false;
      return;
    }
    const prev = i > 0 ? values[i - 1] : null;
    const next = i < n - 1 ? values[i + 1] : null;
    if (prev === null && next === null) dots.push([x(i), y(v)]);
    d += `${pen ? 'L' : 'M'}${x(i).toFixed(1)} ${y(v).toFixed(1)} `;
    pen = true;
  });
  return (
    <svg
      className={`${styles.spark} ${quiet ? styles.quiet : ''}`}
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label={label ?? 'last two minutes'}
    >
      <path d={d} />
      {dots.map(([cx, cy]) => (
        <circle key={cx} cx={cx} cy={cy} r={1.4} />
      ))}
    </svg>
  );
}
