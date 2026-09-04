import type {
  Annotation,
  BroadcastDetail,
  BroadcastPage,
  Cohort,
  Comparison,
  DiagnoseTrace,
  DipReport,
  FieldDoc,
  FleetTimeline,
  HistoryPage,
  Meta,
  QueryResult,
  QueryStatus,
  Report,
  RuleDoc,
  Snapshot,
  Timeline,
  Trend,
} from './types.ts';

// All paths are RELATIVE. The page is served by the same binary that answers
// these, so a relative path works identically on `/`, on a port-forward, and
// under an Ingress sub-path. An absolute path would break two of the three.
//
// Two R31 rules ride on top of that one:
//
//   * **Every historical read is windowed or paged.** UD4 puts filtering,
//     sorting and bucketing on the server; a function here that fetched
//     everything and filtered in the browser would quietly undo that.
//   * **Nothing here carries a field list, a threshold or a rule name.** The
//     catalogue endpoints exist so the UI does not become the second copy that
//     drifts (UD8, UD20).

export class ApiError extends Error {
  // A plain field, not a constructor parameter property: `erasableSyntaxOnly`
  // is on, matching gawk-app's tsconfig.
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, { cache: 'no-store', signal });
  if (!res.ok) throw new ApiError(`HTTP ${res.status}`, res.status);
  return (await res.json()) as T;
}

/** Build a query string, dropping anything absent. */
function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const out = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    // `0` and `false` are legitimate values and must survive; only absence is
    // dropped. Treating them as empty is the same absent-vs-zero confusion the
    // whole type layer exists to avoid.
    if (v === undefined || v === null || v === '') continue;
    out.set(k, String(v));
  }
  const s = out.toString();
  return s ? `?${s}` : '';
}

export function fetchLive(signal?: AbortSignal): Promise<Snapshot> {
  return getJSON<Snapshot>('live', signal);
}

export function fetchMeta(signal?: AbortSignal): Promise<Meta> {
  return getJSON<Meta>('v1/meta', signal);
}

// --- TH2: session detail ----------------------------------------------------

export interface SessionParams {
  fields?: string[];
  points?: number;
  fromMs?: number;
  toMs?: number;
  full?: boolean;
}

/**
 * A session's stored timeline. `detail=1` is always sent from here — it is the
 * opt-in UD1 requires, and the UI is the caller it was added for.
 */
export function fetchSession(
  id: string,
  p: SessionParams = {},
  signal?: AbortSignal,
): Promise<Timeline> {
  return getJSON<Timeline>(
    `v1/sessions/${id}${qs({
      detail: 1,
      fields: p.fields?.length ? p.fields.join(',') : undefined,
      points: p.points,
      from: p.fromMs,
      to: p.toMs,
      full: p.full ? 1 : undefined,
    })}`,
    signal,
  );
}

export function fetchDiagnose(id: string, signal?: AbortSignal): Promise<Report> {
  return getJSON<Report>(`v1/sessions/${id}/diagnose`, signal);
}

export function fetchDiagnoseTrace(id: string, signal?: AbortSignal): Promise<DiagnoseTrace> {
  return getJSON<DiagnoseTrace>(`v1/sessions/${id}/diagnose?trace=1`, signal);
}

export function fetchCompare(id: string, sinceMs?: number, signal?: AbortSignal): Promise<Comparison> {
  return getJSON<Comparison>(
    `v1/sessions/${id}/compare${qs({ since: sinceMs ? new Date(sinceMs).toISOString() : undefined })}`,
    signal,
  );
}

export function fetchDips(id: string, sinceMs?: number, signal?: AbortSignal): Promise<DipReport> {
  return getJSON<DipReport>(
    `v1/sessions/${id}/dips${qs({ since: sinceMs ? new Date(sinceMs).toISOString() : undefined })}`,
    signal,
  );
}

// --- TH3: history -----------------------------------------------------------

export interface HistoryParams {
  fromMs?: number;
  toMs?: number;
  broadcast?: string;
  /** R42: scope to one room's sessions (the HMAC'd room key). */
  room?: string;
  role?: string;
  verdict?: string;
  browser?: string;
  os?: string;
  appVersion?: string;
  deliveryMode?: string;
  hasFindings?: boolean;
  distrusted?: boolean;
  sort?: string;
  asc?: boolean;
  cursor?: string;
  limit?: number;
}

function historyQuery(p: HistoryParams): string {
  return qs({
    since: p.fromMs ? new Date(p.fromMs).toISOString() : undefined,
    to: p.toMs,
    broadcast: p.broadcast,
    room: p.room,
    role: p.role,
    verdict: p.verdict,
    browser: p.browser,
    os: p.os,
    appVersion: p.appVersion,
    deliveryMode: p.deliveryMode,
    hasFindings: p.hasFindings === undefined ? undefined : p.hasFindings ? 1 : 0,
    distrusted: p.distrusted === undefined ? undefined : p.distrusted ? 1 : 0,
    sort: p.sort,
    asc: p.asc ? 1 : undefined,
    cursor: p.cursor,
    limit: p.limit,
  });
}

export function fetchHistorySessions(p: HistoryParams, signal?: AbortSignal): Promise<HistoryPage> {
  return getJSON<HistoryPage>(`v1/history/sessions${historyQuery(p)}`, signal);
}

export function fetchHistoryBroadcasts(p: HistoryParams, signal?: AbortSignal): Promise<BroadcastPage> {
  return getJSON<BroadcastPage>(`v1/history/broadcasts${historyQuery(p)}`, signal);
}

// --- TH4: broadcast timeline ------------------------------------------------

export function fetchBroadcast(
  key: string,
  p: { fromMs?: number; toMs?: number; sinceMs?: number } = {},
  signal?: AbortSignal,
): Promise<BroadcastDetail> {
  return getJSON<BroadcastDetail>(
    `v1/broadcasts/${key}${qs({
      from: p.fromMs,
      to: p.toMs,
      since: p.sinceMs ? new Date(p.sinceMs).toISOString() : undefined,
    })}`,
    signal,
  );
}

export function fetchBroadcastDiagnose(key: string, signal?: AbortSignal): Promise<Report> {
  return getJSON<Report>(`v1/broadcasts/${key}/diagnose`, signal);
}

// --- TH5 / TH6: catalogues --------------------------------------------------

export function fetchFields(signal?: AbortSignal): Promise<FieldDoc[]> {
  return getJSON<FieldDoc[]>('v1/fields', signal);
}

export function fetchRules(signal?: AbortSignal): Promise<RuleDoc[]> {
  return getJSON<RuleDoc[]>('v1/rules', signal);
}

// --- TH7: fleet + trends ----------------------------------------------------

export function fetchFleetTimeline(p: HistoryParams, signal?: AbortSignal): Promise<FleetTimeline> {
  return getJSON<FleetTimeline>(`v1/fleet/timeline${historyQuery(p)}`, signal);
}

export interface TrendParams {
  fromMs?: number;
  toMs?: number;
  metric?: string;
  stat?: string;
  groupBy?: string;
  role?: string;
  bucketMs?: number;
}

export function fetchTrends(p: TrendParams, signal?: AbortSignal): Promise<Trend> {
  return getJSON<Trend>(
    `v1/trends${qs({
      since: p.fromMs ? new Date(p.fromMs).toISOString() : undefined,
      to: p.toMs,
      metric: p.metric,
      stat: p.stat,
      groupBy: p.groupBy,
      role: p.role,
      bucket: p.bucketMs,
    })}`,
    signal,
  );
}

export interface CohortParams {
  metric?: string;
  stat?: string;
  role?: string;
  groupBy?: string;
  a?: string;
  b?: string;
  aFromMs?: number;
  aToMs?: number;
  bFromMs?: number;
  bToMs?: number;
}

export function fetchCohorts(p: CohortParams, signal?: AbortSignal): Promise<Cohort> {
  return getJSON<Cohort>(
    `v1/cohorts${qs({
      metric: p.metric,
      stat: p.stat,
      role: p.role,
      groupBy: p.groupBy,
      a: p.a,
      b: p.b,
      aFrom: p.aFromMs,
      aTo: p.aToMs,
      bFrom: p.bFromMs,
      bTo: p.bToMs,
    })}`,
    signal,
  );
}

// --- TH8: annotations -------------------------------------------------------

export function fetchAnnotations(
  p: { sessionId?: string; broadcastKey?: string; fromMs?: number; toMs?: number } = {},
  signal?: AbortSignal,
): Promise<Annotation[]> {
  return getJSON<Annotation[]>(
    `v1/annotations${qs({
      session: p.sessionId,
      broadcast: p.broadcastKey,
      from: p.fromMs,
      to: p.toMs,
    })}`,
    signal,
  );
}

export async function createAnnotation(a: Omit<Annotation, 'id' | 'createdAtMs'>): Promise<Annotation> {
  const res = await fetch('v1/annotations', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(a),
  });
  if (!res.ok) throw new ApiError(await errorText(res), res.status);
  return (await res.json()) as Annotation;
}

export async function deleteAnnotation(id: string): Promise<void> {
  const res = await fetch(`v1/annotations/${id}`, { method: 'DELETE' });
  if (!res.ok) throw new ApiError(await errorText(res), res.status);
}

// --- TH10: SQL console ------------------------------------------------------

export function fetchQueryStatus(signal?: AbortSignal): Promise<QueryStatus> {
  return getJSON<QueryStatus>('v1/query', signal);
}

export async function runQuery(sql: string, signal?: AbortSignal): Promise<QueryResult> {
  const res = await fetch('v1/query', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sql }),
    signal,
  });
  if (!res.ok) throw new ApiError(await errorText(res), res.status);
  return (await res.json()) as QueryResult;
}

/**
 * The server's own words where it has any. A console that answered every
 * failure with "HTTP 400" would throw away the readable message TH10's
 * criteria require, and a refusal reads very differently from a syntax error.
 */
async function errorText(res: Response): Promise<string> {
  try {
    const body = (await res.text()).trim();
    return body || `HTTP ${res.status}`;
  } catch {
    return `HTTP ${res.status}`;
  }
}

// --- resolve (R28, extended by TH3) ----------------------------------------

/**
 * Resolve a broadcast code to the obfuscated key the page displays.
 *
 * One-way and server-side by design: the raw code is a join credential and the
 * digest is keyed by a fleet secret that must never reach a browser. POST, so
 * the code never lands in browser history, a Referer, or a proxy log.
 *
 * Returns `null` when the backend does not offer the lookup at all — 404 on a
 * build predating it, 501 when it is built but no stats key was configured.
 * Both mean the same thing to the UI: do not offer the affordance.
 */
export async function resolveCode(code: string): Promise<string | null> {
  const res = await fetch('v1/resolve', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  });
  if (res.status === 404 || res.status === 501) return null;
  if (!res.ok) throw new ApiError(`HTTP ${res.status}`, res.status);
  return ((await res.json()) as { broadcastKey: string }).broadcastKey;
}

/**
 * Resolve a ROOM code (R42) to the HMAC'd room key the page groups by. Same
 * one-way, server-side posture as `resolveCode`; the server lower-cases (room
 * codes double as CR names) where broadcast codes upper-case, so the two never
 * share a digest even for the same six characters.
 */
export async function resolveRoom(room: string): Promise<string | null> {
  const res = await fetch('v1/resolve', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ room }),
  });
  if (res.status === 404 || res.status === 501) return null;
  if (!res.ok) throw new ApiError(`HTTP ${res.status}`, res.status);
  const body = (await res.json()) as { roomKey?: string };
  // A backend predating R42 answers a `room` body with 400 (no code); one that
  // ignores the field would return a broadcastKey and no roomKey — treat that
  // as "not offered" rather than grouping by the wrong digest.
  return body.roomKey ?? null;
}

/**
 * Whether the resolve lookup exists on this backend. Probed with an empty code:
 * a backend that has it rejects that with 400 (it got as far as validating),
 * which distinguishes "present but unconfigured" from "present and working"
 * without needing a real code.
 */
export async function probeResolve(): Promise<boolean> {
  try {
    const res = await fetch('v1/resolve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: '' }),
    });
    return res.status === 400;
  } catch {
    return false;
  }
}
