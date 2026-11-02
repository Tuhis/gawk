// R33 (docs/83 D9, §5.6): the gateway's verdict on an OBS stream key, and
// the sentence the OBS page says for each state, warning and reason.
//
// `GET /whip/status` answers for the presenting key only (Authorization:
// Bearer <key>; never a query parameter, so the key stays out of URLs and
// access logs). A malformed key gets 401; a well-formed one the gateway
// doesn't know reads `idle`. `DELETE /whip/status` clears a D7 backoff and
// the lastError that started it (the page's Retry now).

export type WhipState = 'idle' | 'live' | 'ended';

export interface WhipWarning {
  reason: string;
  gopMs?: number;
}

export interface WhipLastError {
  reason: string;
  gopMs?: number;
  codec?: string;
}

export interface WhipStatus {
  state: WhipState;
  codec?: string;
  resolution?: { width: number; height: number };
  fps?: number;
  gopMs?: number;
  bitrateKbps?: number;
  lossPct?: number;
  viewers?: number;
  warnings: WhipWarning[];
  lastError?: WhipLastError;
  retryAfterMs?: number;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

// A defensive read of the response body: unknown fields are ignored, a
// field of the wrong type is dropped, and a body without a known `state` is
// no status at all (the page keeps what it last showed).
export function parseWhipStatus(body: unknown): WhipStatus | null {
  if (!isRecord(body)) return null;
  const state = body.state;
  if (state !== 'idle' && state !== 'live' && state !== 'ended') return null;
  const out: WhipStatus = { state, warnings: [] };
  const codec = str(body.codec);
  if (codec) out.codec = codec;
  if (isRecord(body.resolution)) {
    const width = num(body.resolution.width);
    const height = num(body.resolution.height);
    if (width !== undefined && height !== undefined) out.resolution = { width, height };
  }
  for (const k of ['fps', 'gopMs', 'bitrateKbps', 'lossPct', 'viewers', 'retryAfterMs'] as const) {
    const v = num(body[k]);
    if (v !== undefined) out[k] = v;
  }
  if (Array.isArray(body.warnings)) {
    for (const w of body.warnings) {
      if (!isRecord(w)) continue;
      const reason = str(w.reason);
      if (!reason) continue;
      const gopMs = num(w.gopMs);
      out.warnings.push(gopMs === undefined ? { reason } : { reason, gopMs });
    }
  }
  if (isRecord(body.lastError)) {
    const reason = str(body.lastError.reason);
    if (reason) {
      const e: WhipLastError = { reason };
      const gopMs = num(body.lastError.gopMs);
      if (gopMs !== undefined) e.gopMs = gopMs;
      const c = str(body.lastError.codec);
      if (c) e.codec = c;
      out.lastError = e;
    }
  }
  return out;
}

export type StatusRead =
  | { kind: 'ok'; status: WhipStatus }
  // 401: the gateway can't parse the key.
  | { kind: 'malformed-key' }
  // The network, CORS, a 5xx or a body that isn't a status.
  | { kind: 'error' };

function authHeaders(key: string): HeadersInit {
  return { Authorization: `Bearer ${key}` };
}

export async function readWhipStatus(
  statusUrl: string,
  key: string,
  signal?: AbortSignal,
  fetchImpl: typeof fetch = fetch,
): Promise<StatusRead> {
  try {
    const res = await fetchImpl(statusUrl, { headers: authHeaders(key), cache: 'no-store', signal });
    if (res.status === 401) return { kind: 'malformed-key' };
    if (!res.ok) return { kind: 'error' };
    const status = parseWhipStatus(await res.json());
    return status === null ? { kind: 'error' } : { kind: 'ok', status };
  } catch {
    return { kind: 'error' };
  }
}

// Retry now: clears the backoff. True when the gateway accepted it.
export async function clearWhipBackoff(statusUrl: string, key: string, fetchImpl: typeof fetch = fetch): Promise<boolean> {
  try {
    const res = await fetchImpl(statusUrl, { method: 'DELETE', headers: authHeaders(key), cache: 'no-store' });
    return res.ok;
  } catch {
    return false;
  }
}

// ── What each state says (§5.6) ──

// The D7 reasons a backoff starts with: the gateway ended a live session
// (S4), so the page says "gawk ended your stream".
export const BACKOFF_REASONS = ['gop_too_long', 'bframes', 'invalid_h264'] as const;

// Every lastError reason WG7 defines, for the table test (U4).
export const LAST_ERROR_REASONS = [
  'lost',
  ...BACKOFF_REASONS,
  'codec',
  'simulcast',
  'gateway_full',
  'relay_busy',
  'rejected',
  'banned',
  'reserved',
  'client_ip_unknown',
  'killed',
] as const;

export const WARNING_REASONS = ['gop_long'] as const;

export interface Cause {
  // What happened.
  cause: string;
  // What to change, by OBS's own menu names; null when there is nothing the
  // broadcaster can change.
  fix: string | null;
}

export type ObsView =
  // S2 step 3: no session yet.
  | { kind: 'waiting'; title: string; body: string }
  // S3, with the amber banner when the gateway warns about the GOP.
  | { kind: 'live'; title: string; warning: (Cause & { cell: 'keyframes' }) | null }
  // S5: the stream ended (OBS's Stop Streaming, Retry now after S4, or
  // OBS vanishing). Not red: an end the user chose isn't an error.
  | { kind: 'ended'; title: string; body: string }
  // S4: the gateway ended the stream itself (D7). Retry now and the
  // countdown only while the backoff runs (retryAfterMs).
  | { kind: 'stopped'; title: string; body: string; reason: Cause; retryAfterMs: number | null }
  // A refusal without a backoff: the red box alone.
  | { kind: 'refused'; reason: Cause };

// Seconds with one decimal, as the page writes GOPs ("2.0").
export function fmtSeconds(ms: number | undefined): string {
  return ms === undefined ? '?' : (ms / 1000).toFixed(1);
}

const KEYFRAME_FIX_LIVE = 'In OBS: Settings → Output → Keyframe Interval → 1 s. It applies the next time you start streaming.';

export const GENERIC_CAUSE: Cause = {
  cause: 'OBS couldn’t stream to gawk.',
  fix: 'Check the three steps above, then press Start Streaming again.',
};

// 401 from the status endpoint: the gateway can't read the key at all. The
// format check on paste makes this rare; Forget and a fresh paste fix it.
export const MALFORMED_KEY_CAUSE: Cause = {
  cause: 'The server can’t read this key.',
  fix: 'Forget it here, then paste the key from OBS (Settings → Stream) again, or reset it.',
};

// The sentence for one lastError. Unknown reasons get the generic line, so
// no raw reason string ever reaches the screen.
export function causeFor(e: WhipLastError, status?: Pick<WhipStatus, 'gopMs'>): Cause {
  switch (e.reason) {
    case 'gop_too_long':
      return {
        cause: `OBS sent a keyframe only every ${fmtSeconds(e.gopMs ?? status?.gopMs)} s. Viewers who joined, or hit a glitch, saw a frozen picture for seconds at a time.`,
        fix: 'In OBS: Settings → Output → Keyframe Interval → 1 s. If it says 0 (auto), that is the cause.',
      };
    case 'bframes':
      return {
        cause: 'Your encoder sends B-frames, which gawk can’t play live.',
        fix: 'Set B-frames to 0 in the encoder’s settings (Settings → Output), then retry.',
      };
    case 'invalid_h264':
      return {
        cause: 'gawk couldn’t read the video OBS sent.',
        fix: 'Choose another H.264 encoder in Settings → Output, then retry.',
      };
    case 'codec': {
      const codec = e.codec === 'AV1' || e.codec === 'HEVC' ? e.codec : 'a codec other than H.264';
      return {
        cause: `OBS is sending ${codec}. gawk takes H.264.`,
        fix: 'In OBS: Settings → Output → Video Encoder → an H.264 one.',
      };
    }
    case 'simulcast':
      return { cause: 'OBS is sending several layers (simulcast).', fix: 'Turn Simulcast off in Settings → Stream.' };
    case 'gateway_full':
      return {
        cause: 'This server is streaming as many OBS broadcasts as it can.',
        fix: 'Wait a minute, then press Start Streaming in OBS again.',
      };
    case 'relay_busy':
      return { cause: 'This server has no room for another stream right now.', fix: 'Try again in a few minutes.' };
    case 'rejected':
      return {
        cause: 'The server didn’t accept this key. It may have been reset by whoever runs it.',
        fix: 'Reset the key here, then paste the new one into OBS.',
      };
    case 'banned':
      return { cause: 'The server’s operator has blocked this stream.', fix: null };
    case 'reserved':
      return { cause: 'This code is in use on another gawk server.', fix: 'Reset the key to get a new code.' };
    case 'client_ip_unknown':
      return {
        cause: 'This server is set up wrong: it can’t see where your stream comes from.',
        fix: 'Tell whoever runs it.',
      };
    case 'killed':
      return { cause: 'The server’s operator ended this stream.', fix: null };
    default:
      return GENERIC_CAUSE;
  }
}

// The page's whole reading of one status (U4). Pure: the countdown is
// retryAfterMs as read; the page ticks it down itself.
export function describeObsStatus(status: WhipStatus, code: string): ObsView {
  if (status.state === 'live') {
    // A session that goes live clears lastError at the gateway (D9); only
    // the warnings speak here. Unknown warnings are not shown.
    const gop = status.warnings.find((w) => w.reason === 'gop_long');
    return {
      kind: 'live',
      title: 'You’re live from OBS',
      warning: gop
        ? {
            cause: `OBS sends a keyframe every ${fmtSeconds(gop.gopMs ?? status.gopMs)} s. At 1 s, viewers who join or hit a glitch see a moving picture again sooner.`,
            fix: KEYFRAME_FIX_LIVE,
            cell: 'keyframes',
          }
        : null,
    };
  }

  const e = status.lastError;
  const backoff = status.retryAfterMs !== undefined && status.retryAfterMs > 0 ? status.retryAfterMs : null;
  if (e?.reason === 'lost') {
    return {
      kind: 'ended',
      title: 'OBS disconnected',
      body: `If OBS is still open, it reconnects on its own. If not, press Start Streaming in OBS again; your code stays ${code}.`,
    };
  }
  if (e && ((BACKOFF_REASONS as readonly string[]).includes(e.reason) || backoff !== null)) {
    return {
      kind: 'stopped',
      title: 'gawk ended your stream',
      body: `Viewers on ${code} are waiting; they pick the stream up again when you’re back.`,
      reason: causeFor(e, status),
      retryAfterMs: backoff,
    };
  }
  if (e) return { kind: 'refused', reason: causeFor(e, status) };

  if (status.state === 'ended') {
    return {
      kind: 'ended',
      title: 'Your stream has ended',
      body: `Press Start Streaming in OBS to go live again on ${code}.`,
    };
  }
  return { kind: 'waiting', title: 'Waiting for OBS…', body: 'This page updates on its own when OBS connects.' };
}

// "0:48" for the backoff countdown.
export function fmtCountdown(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${m}:${String(s).padStart(2, '0')}`;
}
