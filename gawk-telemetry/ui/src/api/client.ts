import type { Snapshot } from './types.ts';

// All paths are RELATIVE. The page is served by the same binary that answers
// these, so a relative path works identically on `/`, on a port-forward, and
// under an Ingress sub-path. An absolute path would break two of the three.

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

export function fetchLive(signal?: AbortSignal): Promise<Snapshot> {
  return getJSON<Snapshot>('live', signal);
}

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
