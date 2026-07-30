// Series transforms shared by the explorer and anything else that plots a
// catalogued field.
//
// They live here rather than inside a view because the RULES they encode come
// from the field catalogue's semantics (UD8), not from any one screen: a
// counter is a counter wherever it is drawn.

/**
 * Counter → per-second rate.
 *
 * A cumulative counter's LEVEL says almost nothing — every session's
 * `datagramsReceived` is a ramp — and what an operator wants is how fast it
 * moved. Two behaviours are load-bearing:
 *
 *   * a `null` input stays `null`, because a gap is a BREAK and not a zero
 *     (UD9);
 *   * a counter that went BACKWARDS is a restart, not negative traffic, so the
 *     line breaks there too. Drawing the dive would invent a period of
 *     impossible negative throughput at exactly the moment something restarted.
 */
export function toRate(
  points: Array<[number, number | null]>,
): Array<[number, number | null]> {
  const out: Array<[number, number | null]> = [];
  let prev: [number, number] | null = null;
  for (const [t, v] of points) {
    if (v === null) {
      out.push([t, null]);
      prev = null;
      continue;
    }
    if (prev === null || t <= prev[0] || v < prev[1]) {
      out.push([t, null]);
      prev = [t, v];
      continue;
    }
    out.push([t, ((v - prev[1]) * 1000) / (t - prev[0])]);
    prev = [t, v];
  }
  return out;
}

/**
 * Fold a boolean series into contiguous spans, for shading.
 *
 * A state is a band, never a line between 0 and 1: a line implies a transition
 * through 0.5, which is not a thing `documentHidden` ever was.
 */
export function toSpans(
  points: Array<[number, number | null]>,
): Array<{ fromMs: number; toMs: number }> {
  const out: Array<{ fromMs: number; toMs: number }> = [];
  let start: number | null = null;
  for (const [t, v] of points) {
    const on = v !== null && v !== 0;
    if (on && start === null) start = t;
    if (!on && start !== null) {
      out.push({ fromMs: start, toMs: t });
      start = null;
    }
  }
  if (start !== null && points.length) {
    out.push({ fromMs: start, toMs: points[points.length - 1][0] });
  }
  return out;
}
