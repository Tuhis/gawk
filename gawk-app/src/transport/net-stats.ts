// WebTransport connection health sampling (R9 M6, docs/13 D7). Each client
// measures its own leg to the relay — RTT, packet loss, send-rate ceiling —
// which is what lets a problem be attributed to broadcaster-uplink vs
// viewer-downlink without any cross-reporting channel.
//
// `WebTransport.getStats()` is spec'd but currently shipped by NO browser:
// Chromium removed its pre-spec implementation (verified absent in 150
// stable / 151 beta / 152 dev, 2026-07-14 — the spec-conformant rewrite is
// "in development": https://chromestatus.com/feature/5194440034746368,
// https://issues.chromium.org/issues/41492543) and Firefox never had it.
// Every field is nullable and the sampler never throws: unsupported simply
// yields null and the overlays render "—". The mapping below matches the
// current spec's WebTransportConnectionStats, so it lights up again
// automatically when Chromium re-ships.

export interface TransportConnectionStats {
  rttMs: number | null;
  rttVarMs: number | null;
  packetsSent: number | null;
  packetsReceived: number | null;
  packetsLost: number | null;
  bytesSent: number | null;
  bytesReceived: number | null;
  // Sender-side signals (meaningful on the broadcaster).
  estimatedSendRateBps: number | null;
  atSendCapacity: boolean | null;
  datagramsExpiredOutgoing: number | null; // died in the local send queue
  datagramsLostOutgoing: number | null; // lost on the wire (acked missing)
  // Receiver-side signal (meaningful on the viewer).
  datagramsDroppedIncoming: number | null;
}

// The subset of WebTransport we probe, typed loosely on purpose: the real
// dictionary differs across browser versions.
type StatsCapable = {
  getStats?: () => Promise<Record<string, unknown>>;
};

function num(v: unknown): number | null {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function bool(v: unknown): boolean | null {
  return typeof v === 'boolean' ? v : null;
}

// One raw sample, defensively mapped. Returns null when getStats is missing
// or rejects (both mean "this browser can't tell us").
export async function sampleConnectionStats(wt: unknown): Promise<TransportConnectionStats | null> {
  const capable = wt as StatsCapable | null | undefined;
  if (!capable || typeof capable.getStats !== 'function') return null;
  let raw: Record<string, unknown>;
  try {
    raw = await capable.getStats();
  } catch {
    return null;
  }
  if (!raw || typeof raw !== 'object') return null;
  const datagrams = (raw.datagrams ?? {}) as Record<string, unknown>;
  return {
    rttMs: num(raw.smoothedRtt),
    rttVarMs: num(raw.rttVariation),
    packetsSent: num(raw.packetsSent),
    packetsReceived: num(raw.packetsReceived),
    packetsLost: num(raw.packetsLost),
    bytesSent: num(raw.bytesSent),
    bytesReceived: num(raw.bytesReceived),
    estimatedSendRateBps: num(raw.estimatedSendRate),
    atSendCapacity: bool(raw.atSendCapacity),
    datagramsExpiredOutgoing: num(datagrams.expiredOutgoing),
    datagramsLostOutgoing: num(datagrams.lostOutgoing),
    datagramsDroppedIncoming: num(datagrams.droppedIncoming),
  };
}

// Keeps the latest sample so a synchronous stats tick can attach connection
// stats without awaiting: tick() fires an async refresh, latest() returns
// whatever the most recent refresh produced. At a 500 ms stats cadence the
// one-tick lag is irrelevant.
export class ConnectionStatsSampler {
  private wt: unknown;
  private last: TransportConnectionStats | null = null;
  private inFlight = false;

  constructor(wt: unknown) {
    this.wt = wt;
  }

  tick(): void {
    if (this.inFlight) return;
    this.inFlight = true;
    void sampleConnectionStats(this.wt)
      .then((s) => {
        this.last = s;
      })
      .finally(() => {
        this.inFlight = false;
      });
  }

  latest(): TransportConnectionStats | null {
    return this.last;
  }
}
