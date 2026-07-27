// Mirrors the Go types in internal/live and internal/rules. Kept as a
// hand-written mirror rather than generated: the surface is small, and the JSON
// tags are the contract either way.
//
// Everything optional here is optional in Go too (`omitempty`), and the reason
// matters — an absent value and a zero are DIFFERENT claims on this page. A
// missing `capToRenderMs` means "not measured", not "0 ms", and rendering the
// second for the first is the exact failure the health model exists to prevent.
// So the types keep `undefined` and the formatters render it as `—`.

export type Severity = 'ok' | 'warn' | 'bad' | 'unknown';

/** `reporting | stale | unknown` for the client side, `observed | stale | unknown` for the relay. */
export type Freshness = string;

export interface Evidence {
  signal: string;
  value?: number;
  unit?: string;
  from?: 'relay' | 'client' | 'derived';
  comparison?: string;
}

export interface Finding {
  id: string;
  verdict: string;
  severity: Severity;
  confidence?: number;
  evidence?: Evidence[];
  action?: string;
}

export interface SessionView {
  sessionId: string;
  broadcastKey: string;
  role: 'broadcaster' | 'viewer' | string;
  browser?: string;
  os?: string;
  appVersion?: string;
  startedAtMs?: number;
  severity: Severity;
  verdict?: string;
  findings?: Finding[];
  clientAgeMs: number;
  relayAgeMs: number;
  clientState: Freshness;
  relayState: Freshness;
  metrics?: Record<string, number>;
  config?: Record<string, string>;
}

export interface BroadcastView {
  broadcastKey: string;
  lifecycle: 'live' | 'away' | 'ended' | string;
  severity: Severity;
  /** Worst severity among this broadcast's viewers — what a fleet scan needs. */
  worstViewer: Severity;
  viewers: number;
  uptimeMs: number;
  endedAgoMs?: number;
  pod?: string;
  role?: string;
  findings?: Finding[];
  sessions?: SessionView[];
  metrics?: Record<string, number>;
}

export interface Snapshot {
  atMs: number;
  /** Live and ended are never interleaved: the grouping IS the precedence. */
  live: BroadcastView[] | null;
  ended: BroadcastView[] | null;
}
