// Mirrors the Go types in internal/live, internal/rules, internal/readapi,
// internal/schema and internal/annotations. Kept as a hand-written mirror
// rather than generated: the surface is small, and the JSON tags are the
// contract either way.
//
// Everything optional here is optional in Go too (`omitempty`), and the reason
// matters — an absent value and a zero are DIFFERENT claims on this page. A
// missing `capToRenderMs` means "not measured", not "0 ms", and rendering the
// second for the first is the exact failure the health model exists to prevent.
// So the types keep `undefined` and the formatters render it as `—`.
//
// R31 note: there is exactly ONE thing this file must never mirror, and that is
// a field LIST. `/v1/fields` and `/v1/rules` are served precisely so the UI
// does not carry a second copy of `schema.ViewerFields` or of the playbook's
// thresholds (UD8, UD20). Types describing the SHAPE of those responses are
// fine; an array of field names in this file would not be.

export type Severity = 'ok' | 'warn' | 'bad' | 'unknown';

/** `reporting | stale | unknown` for the client side, `observed | stale | unknown` for the relay. */
export type Freshness = string;

export type Provenance = 'relay' | 'client' | 'derived';

export interface Evidence {
  signal: string;
  value?: number;
  text?: string;
  unit?: string;
  from?: Provenance;
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

/** A rule that could not run, with the signal it was missing. */
export interface Missing {
  id: string;
  missingSignals: string[];
}

/** diagnose()'s whole output — D6/D7's provenance apparatus, all of it. */
export interface Report {
  subject: string;
  scope: string;
  healthy: boolean;
  findings?: Finding[];
  passed?: string[];
  unavailable?: Missing[];
  caveats?: string[];
  dashboardUrl?: string;
}

export interface SessionView {
  sessionId: string;
  broadcastKey: string;
  /** R42: the HMAC'd room the client said it was in. Absent outside a room. */
  roomKey?: string;
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
  /** docs/78 D3: the issuer key of the PoP that served it; empty for home. */
  issuer?: string;
  /** R36 item 5 (TU6): that PoP's display name. */
  pop?: string;
  /** R36 item 4 (TU5): this viewer's stripe legs, joined by the relay's owner key. */
  legs?: LegView[];
}

/** One live stripe leg of a viewer (TU5): plumbing, never an audience member. */
export interface LegView {
  member: number;
  n: number;
  pod: string;
  role: string;
  /**
   * The relay's per-subscriber `dropped` counter over the last scrape window
   * (queue overflow plus bandwidth-cap drops). Absent until scraped twice.
   * The relay keeps no per-subscriber SENT counter, so there is none here.
   */
  datagramsDropped?: number;
  queueDepth: number;
  relayAgeMs: number;
}

/** `GET /live/sparks` (TU7): one broadcast's 2-minute tails. */
export interface Sparks {
  atMs: number;
  /** Adaptive: the narrowest bucket ≥ 1.6× the broadcast's sample gap. */
  stepMs: number;
  windowMs: number;
  sessions: Record<
    string,
    {
      field: string;
      /** Lowest sample per bucket, oldest first; null is a gap (UD9). */
      values: Array<number | null>;
      /**
       * The newest instant this session's samples cover. Clients upload in
       * ~10 s batches, so buckets after it are "not arrived yet", not gaps.
       */
      throughMs?: number;
    }
  >;
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
  /** TU5: live stripe legs on this broadcast, attributed or not. Not viewers. */
  stripeLegs?: number;
}

export interface Snapshot {
  atMs: number;
  /** Live and ended are never interleaved: the grouping IS the precedence. */
  live: BroadcastView[] | null;
  ended: BroadcastView[] | null;
}

// --- meta -------------------------------------------------------------------

/** What this deployment can do and where its boundaries are. Asked once. */
/**
 * `GET /v1/me` (docs/55 D5): the identity the bearer token carries. OIDC mode
 * only. `roles` is what the token grants on this client — the missing-role
 * page shows it, because "which roles DO I have?" is the operator's next
 * question.
 */
export interface Me {
  subject: string;
  email?: string;
  roles?: string[];
}

export interface Meta {
  retentionDays: number;
  /** The raw-retention boundary as an instant. Older sessions are rollup-only. */
  rawFromMs: number;
  rollupsFromMs?: number;
  scrapeIntervalMs: number;
  annotations: boolean;
  sql: boolean;
  sqlReason?: string;
  /** The engine was configured and failed to open — not a build without one. */
  sqlFailed?: boolean;
  resolve: boolean;
  streamIntervalMs: number;
  /** The SERVICE's clock, so the page can detect a browser that disagrees. */
  serverNowMs: number;
}

// --- coverage ---------------------------------------------------------------

/**
 * UD10 as a wire type. The failure it prevents is concluding "nothing was
 * wrong" from "nothing was kept", so `note` is rendered whenever it is present
 * and never summarised away.
 */
export interface Coverage {
  rawFromMs: number;
  rollupsFromMs?: number;
  retentionDays?: number;
  note?: string;
  /**
   * R36 (docs/86 TU4): WHY the note fires. `before-data` is the service's own
   * age — drawn as the axis extent, never as a warning; `raw-pruned` is
   * retention, said once and quietly; `none` is a store with nothing in it.
   */
  reason?: 'none' | 'before-data' | 'raw-pruned';
}

// --- session detail (TH2) ---------------------------------------------------

export interface StoredEvent {
  kind: string;
  sessionId?: string;
  tMs?: number;
  event?: string;
  detail?: string;
  receivedAtMs?: number;
}

export interface Timeline {
  sessionId: string;
  role: string;
  fields: string[];
  points: Array<Record<string, number>>;
  events?: StoredEvent[];
  downsampled: boolean;
  totalSamples: number;
  config?: Record<string, string>;
  note?: string;
  // Present only with `detail=1`; see the Go type's comment on why.
  broadcastKey?: string;
  /** R42: the HMAC'd room this session reported, detail-only like broadcastKey. */
  roomKey?: string;
  startedAtMs?: number;
  endedAtMs?: number;
  clockOffsetMs?: number;
  step?: number;
  fromMs?: number;
  toMs?: number;
  available?: string[];
  truncated?: boolean;
  /**
   * Whether the projection still holds this session open.
   *
   * Not inferable from `endedAtMs`: a session that supplies no end of its own
   * gets the last receive time as one, so EVERY session comes back with an end.
   */
  live?: boolean;
}

export interface Delta {
  session: number;
  fleet: number;
  ratio?: number;
}

export interface Comparison {
  sessionId: string;
  class: string;
  deltas: Record<string, Delta>;
  note?: string;
}

// --- history (TH3) ----------------------------------------------------------

export interface SessionSummary {
  sessionId: string;
  broadcastKey: string;
  role: string;
  browser?: string;
  os?: string;
  startedAtMs?: number;
  durationMs?: number;
  severity: Severity;
  stalls: number;
  reconnects: number;
  relayCoverage: string;
  distrust?: string;
}

export interface HistoryRow extends SessionSummary {
  /** R42: the HMAC'd room, on the history row only (the MCP summary never moves). */
  roomKey?: string;
  appVersion?: string;
  endedAtMs?: number;
  deliveryMode?: string;
  verdict?: string;
  findings: number;
  rollupOnly?: boolean;
  live?: boolean;
}

export interface HistoryPage {
  rows: HistoryRow[];
  total: number;
  nextCursor?: string;
  coverage: Coverage;
}

export interface BroadcastRow {
  broadcastKey: string;
  /** R42: the room any of this broadcast's sessions reported. */
  roomKey?: string;
  sessions: number;
  viewers: number;
  firstSeenMs: number;
  lastSeenMs: number;
  worstVerdict: Severity;
  live: boolean;
  broadcasterSessionId?: string;
  appVersion?: string;
  findings: number;
  rollupOnly?: boolean;
}

export interface BroadcastPage {
  rows: BroadcastRow[];
  total: number;
  nextCursor?: string;
  coverage: Coverage;
}

// --- broadcast timeline (TH4) -----------------------------------------------

export interface LanePoint {
  atMs: number;
  value: number;
}

export interface DipSpan {
  fromMs: number;
  toMs: number;
  worstValue: number;
  baseline: number;
  deltas?: Record<string, number>;
  before?: Record<string, number>;
  after?: Record<string, number>;
}

export interface Interval {
  fromMs: number;
  toMs: number;
}

export interface LaneEvent {
  atMs: number;
  kind: string;
  detail?: string;
}

export interface Lane {
  kind: 'broadcaster' | 'viewer' | 'relay' | string;
  sessionId?: string;
  browser?: string;
  os?: string;
  appVersion?: string;
  deliveryMode?: string;
  severity: Severity;
  verdict?: string;
  startedAtMs: number;
  endedAtMs?: number;
  /** This source's own reporting interval. UD9: no lane is drawn finer. */
  cadenceMs: number;
  clockOffsetMs?: number;
  primary?: string;
  unit?: string;
  points?: LanePoint[];
  dips?: DipSpan[];
  events?: LaneEvent[];
  hidden?: Interval[];
  rollupOnly?: boolean;
  note?: string;
  downsampled?: boolean;
  /**
   * R36 item 4 (docs/86 TU5): a striped viewer's stripe legs, attributed to
   * it through the relay's keyed owner field. Never a lane of their own and
   * never counted as viewers.
   */
  legs?: LegLane[];
  /** Leg sessions beyond the 16 a lane carries. */
  legsOmitted?: number;
  /** R36 item 5 (TU6): the PoP that served this viewer, when not home. */
  issuer?: string;
  pop?: string;
}

/** One stripe leg of a viewer lane (TU5). */
export interface LegLane {
  member: number;
  n: number;
  pod: string;
  role?: string;
  /** Dropped datagrams per second over each scrape interval; a counter reset is a break. */
  points?: LanePoint[];
  unit?: string;
  /** The leg's last cumulative `dropped`. */
  datagramsDropped?: number;
  downsampled?: boolean;
}

export interface Rehome {
  atMs: number;
  fromPod?: string;
  toPod: string;
  fromRole?: string;
  toRole?: string;
}

export interface BroadcastDetail {
  broadcastKey: string;
  fromMs: number;
  toMs: number;
  live: boolean;
  lanes: Lane[];
  rehomes?: Rehome[];
  lanesOmitted?: number;
  coverage: Coverage;
}

// --- field catalogue (TH5) --------------------------------------------------

export type Semantic = 'gauge' | 'counter' | 'bool' | 'text' | 'object';

export interface FieldDoc {
  name: string;
  roles: string[];
  semantic: Semantic;
  unit?: string;
  description?: string;
  legacy?: boolean;
}

// --- rule catalogue and trace (TH6) -----------------------------------------

export interface Threshold {
  name: string;
  value: number;
  unit?: string;
  note?: string;
}

export interface RuleDoc {
  id: string;
  scope: string;
  verdict: string;
  action?: string;
  why?: string;
  requires: string[];
  provenance: string[];
  thresholds?: Threshold[];
  clientOnly: boolean;
  maxConfidence: number;
}

export interface Trace {
  id: string;
  scope: string;
  outcome: 'fired' | 'passed' | 'unavailable' | 'out-of-scope' | string;
  read?: Record<string, number>;
  readText?: Record<string, string>;
  missing?: string[];
  severity?: Severity;
}

export interface DiagnoseTrace {
  report: Report;
  trace: Trace[];
}

// --- fleet + trends (TH7) ---------------------------------------------------

export interface SeverityBand {
  fromMs: number;
  toMs: number;
  severity: Severity;
}

export interface FleetTimelineRow {
  broadcastKey: string;
  fromMs: number;
  toMs: number;
  severity: Severity;
  sessions: number;
  viewers: number;
  live?: boolean;
  bands?: SeverityBand[];
  rollupOnly?: boolean;
}

export interface FleetTimeline {
  fromMs: number;
  toMs: number;
  rows: FleetTimelineRow[];
  rowsOmitted?: number;
  coverage: Coverage;
}

export interface TrendPoint {
  atMs: number;
  value: number;
  sessions: number;
  /** Too few sessions to claim anything. Never drawn as a confident line. */
  thin?: boolean;
}

export interface TrendSeries {
  group: string;
  points: TrendPoint[];
  sessions: number;
}

export interface Trend {
  metric: string;
  groupBy?: string;
  stat: string;
  bucketMs: number;
  series: TrendSeries[];
  coverage: Coverage;
  note?: string;
}

export interface CohortArm {
  label: string;
  value: number;
  sessions: number;
  thin?: boolean;
}

export interface Cohort {
  metric: string;
  stat: string;
  a: CohortArm;
  b: CohortArm;
  delta: number;
  ratio?: number;
  note?: string;
  coverage: Coverage;
}

// --- annotations (TH8) ------------------------------------------------------

export interface Annotation {
  id: string;
  createdAtMs: number;
  sessionId?: string;
  broadcastKey?: string;
  atMs?: number;
  text: string;
  author?: string;
}

// --- dips (TH9) -------------------------------------------------------------

export interface Mover {
  signal: string;
  from: number;
  to: number;
  magnitude: number;
  unit?: string;
  semantic: string;
  provenance: string;
}

export interface PeerDip {
  sessionId: string;
  role: string;
  reporting: boolean;
  dipped: boolean;
  worstValue?: number;
  baseline?: number;
}

export interface Correlation {
  peersReporting: number;
  peersDipped: number;
  peers?: PeerDip[];
  confidence: number;
  /** Co-occurrence wording, built server-side so it cannot drift into cause. */
  statement: string;
}

export interface DipExplanation {
  fromMs: number;
  toMs: number;
  durationMs: number;
  worstValue: number;
  baseline: number;
  movers?: Mover[];
  moversOmitted?: number;
  correlation: Correlation;
}

export interface DipReport {
  sessionId: string;
  broadcastKey: string;
  role: string;
  primary: string;
  episodes: DipExplanation[];
  note?: string;
}

// --- SQL console (TH10) -----------------------------------------------------

export interface ViewDoc {
  name: string;
  description: string;
  available: boolean;
}

export interface QueryStatus {
  enabled: boolean;
  reason?: string;
  /** The engine was configured and failed to open — not a build without one. */
  failed?: boolean;
  views?: ViewDoc[];
  rowLimit: number;
  timeoutMs: number;
  /** R82's Ask box. Absent from a server that predates it. */
  ask?: AskStatus;
}

/**
 * R82 (docs/84 D8): the question box over the console. `enabled` needs both a
 * provider and an engine; `reason` says which is missing when one was asked
 * for. `provider: 'none'` is the default, and shows nothing.
 */
export interface AskStatus {
  enabled: boolean;
  provider: string;
  model?: string;
  context?: string;
  /** The provider is outside the cluster: the question leaves it (rows never do). */
  external?: boolean;
  endpoint?: string;
  reason?: string;
}

/** A draft from a question: SQL to read and run, never a result (docs/84 D3). */
export interface AskAnswer {
  interpretation: string;
  sql: string;
  checks: number;
  /** The shadow catalogue's verdict on the final SQL: "OK" or the error Run would meet. */
  check: string;
  provider: string;
  model: string;
  elapsedMs: number;
}

export interface QueryResult {
  columns: string[];
  types?: string[];
  rows: unknown[][];
  rowCount: number;
  truncated?: boolean;
  elapsedMs: number;
  views?: ViewDoc[];
}
