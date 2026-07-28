// R29 finding 2 (docs/34): the viewer's incoming-datagram buffer.
//
// The WebTransport receive queue is bounded and, when it overflows, the spec
// has the user agent drop from the HEAD of the queue — the OLDEST datagrams,
// not the newest. gawk hands the browser each delta frame as one back-to-back
// burst of ~9-11 datagrams (its chunks, then its two parity symbols, which
// `broadcaster.ts` deliberately sends last), 28 times a second. If the queue
// is shallower than a burst, the datagrams are evicted before the read loop is
// ever scheduled, and no reader can win that race.
//
// That is what a live Firefox 154 session measured: 10.5 % of delta chunks
// gone against 0.05 % of parity, on one FIFO over one connection, with the
// relay's counters showing every datagram handed to QUIC. Losing the head of
// each burst is also the one loss shape a per-frame k=2 code cannot repair —
// three erasures in one frame — which is why parity read as "ineffective"
// while working exactly as designed.
//
// So the queue depth is the lever, and this module is the whole of it: raise
// it, and report honestly whether the browser took the hint.

// A burst is ~9-11 datagrams and several frames are in flight at once, so this
// is roughly twenty frames of headroom (~300 KB at the 1200 B cap). It buys
// burst absorption, not latency: the reader keeps up on average, and a
// datagram that arrives late rather than never is one the reorder buffer can
// account for (`framesDroppedLate`) instead of one that vanishes silently.
export const INCOMING_DATAGRAM_BUFFER = 256;

// The attribute was renamed: `incomingMaxBufferedDatagrams` is the spec name,
// `incomingHighWaterMark` the original. Firefox — the browser the finding was
// measured on — ships only the old one, and Chromium is removing it, so
// neither name alone covers the fleet. Spec name first; they are aliases, so
// setting one is setting both.
const PROPERTIES = ['incomingMaxBufferedDatagrams', 'incomingHighWaterMark'] as const;

export type DatagramBufferProperty = (typeof PROPERTIES)[number];

// Only the spec-named attribute is tied to [[IncomingMaxBufferedDatagrams]],
// the value the receive algorithm compares the queue against before dropping
// from its head. `incomingHighWaterMark` is the pre-rename attribute, and its
// documented meaning is the readable stream's queuing high-water mark — a
// backpressure signal, not a drop threshold. Writing it succeeds and reads
// back, so a readback proves storage and nothing more.
//
// Measured, not assumed (docs/34 finding 3): a paired A/B against a live
// broadcast — two subscribers, same seconds, hwm 1 vs 8192, reader stalled to
// stress the queue — moves delivery by less than its own A/A noise floor, with
// the repeats disagreeing on direction. `e2e/firefox-datagram-buffer.mjs`
// re-runs it when Firefox ships the real attribute.
const DROP_THRESHOLD_PROPERTY: DatagramBufferProperty = 'incomingMaxBufferedDatagrams';

export interface DatagramBufferStats {
  // Which attribute this browser exposes; null where it exposes neither and
  // the queue depth is whatever the implementation chose.
  property: DatagramBufferProperty | null;
  requested: number;
  // The depth the browser chose before we wrote anything. The single most
  // diagnostic number here: Firefox 154 reports 1 — one datagram, against a
  // delta frame's burst of ~11 (docs/34 finding 3).
  defaultDepth: number | null;
  // What the attribute reads back as afterwards. Null when absent or
  // non-numeric — never assumed equal to `requested`, because a user agent may
  // accept the assignment and ignore it, and that silence is the failure mode
  // this whole module exists to make visible.
  effective: number | null;
  // The write landed. NOT "the queue got deeper" — see governsDrops.
  applied: boolean;
  // Whether the attribute we wrote is the one the spec makes the drop
  // threshold. False on a browser that only exposes the legacy name, where a
  // green `applied` says nothing about whether datagrams stop being dropped.
  governsDrops: boolean;
}

function numberAt(bag: Record<string, unknown>, key: string): number | null {
  const v = bag[key];
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

// Raises the session's incoming datagram queue to `requested` and reports what
// the browser actually did. Never throws: a failure here must degrade to the
// browser default, never take down a connect.
export function applyIncomingDatagramBuffer(
  datagrams: unknown,
  requested: number = INCOMING_DATAGRAM_BUFFER,
): DatagramBufferStats {
  const stats: DatagramBufferStats = {
    property: null,
    requested,
    defaultDepth: null,
    effective: null,
    applied: false,
    governsDrops: false,
  };
  if (datagrams == null || typeof datagrams !== 'object') return stats;
  const bag = datagrams as Record<string, unknown>;
  for (const property of PROPERTIES) {
    // `in` walks the prototype chain, which is where a real
    // WebTransportDatagramDuplexStream keeps its accessors.
    if (!(property in bag)) continue;
    stats.property = property;
    stats.governsDrops = property === DROP_THRESHOLD_PROPERTY;
    const before = numberAt(bag, property);
    stats.defaultDepth = before;
    // Only ever raise. The default is implementation-defined and may already
    // be deeper than we ask for; clamping it down would be this change causing
    // the very loss it exists to stop.
    if (before === null || before < requested) {
      try {
        bag[property] = requested;
      } catch {
        // Exposed read-only, or refused. A no-op, not a fault — the readback
        // below is what decides the verdict either way.
      }
    }
    stats.effective = numberAt(bag, property);
    stats.applied = stats.effective !== null && stats.effective >= requested;
    return stats;
  }
  return stats;
}
