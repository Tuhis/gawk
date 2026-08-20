// The `/api/v1` client (docs/42 §4.7).
//
// Every call goes through `AuthSession.authorizedFetch`, so there is exactly
// one place a bearer token is attached and exactly one place a 401 or a 403 is
// interpreted. A view that reached for `fetch` directly would silently bypass
// both — hence no bare `fetch` anywhere in this file.
//
// Paths are RELATIVE. The page is served by the binary that answers these, so a
// relative path works identically on `/`, on a port-forward and under an
// Ingress sub-path.

import type { AuthSession } from '../auth/session.ts';
import type {
  Ban,
  Broadcast,
  CreateBanRequest,
  CreateWebhookRequest,
  EventPage,
  KillRequest,
  Me,
  Relay,
  Webhook,
  WebhookTestResult,
} from './types.ts';

/**
 * An error carrying the API's own words. §4.7 specifies
 * `{"error":{"code","message"}}`; `code` is what the UI branches on
 * (`source_immutable` on a config-sourced webhook write, for one), `message` is
 * what it shows.
 */
export class ApiError extends Error {
  // Plain fields rather than constructor parameter properties:
  // `erasableSyntaxOnly` is on, matching gawk-app's and telemetry's tsconfig.
  status: number;
  code: string;

  constructor(message: string, status: number, code = '') {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

const BASE = 'api/v1/';

/**
 * Pull a list out of a response that may be bare or enveloped.
 *
 * §4.7 names the rows each route returns but not whether they arrive as a bare
 * array or under a key, and `internal/api` (AP4) is the authority on that. This
 * is the one seam where the SPA accepts both rather than pinning a shape the
 * design doc does not.
 */
function list<T>(body: unknown, key: string): T[] {
  if (Array.isArray(body)) return body as T[];
  if (body && typeof body === 'object') {
    const inner = (body as Record<string, unknown>)[key];
    if (Array.isArray(inner)) return inner as T[];
  }
  return [];
}

export class ApiClient {
  private readonly session: AuthSession;

  constructor(session: AuthSession) {
    this.session = session;
  }

  private async request(path: string, init: RequestInit = {}): Promise<Response> {
    const res = await this.session.authorizedFetch(BASE + path, init);
    if (res.ok) return res;
    throw await apiError(res);
  }

  private async json<T>(path: string, init: RequestInit = {}): Promise<T> {
    const res = await this.request(path, init);
    return (await res.json()) as T;
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return this.json<T>(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  }

  /** The authorization probe: 403 here means "no operator role" (§4.7). */
  me(): Promise<Me> {
    return this.json<Me>('me');
  }

  async broadcasts(): Promise<Broadcast[]> {
    return list<Broadcast>(await this.json<unknown>('broadcasts'), 'broadcasts');
  }

  kill(id: string, req: KillRequest): Promise<{ ban?: Ban }> {
    return this.post<{ ban?: Ban }>(`broadcasts/${encodeURIComponent(id)}/kill`, req);
  }

  async bans(state: 'active' | 'all'): Promise<Ban[]> {
    return list<Ban>(await this.json<unknown>(`bans?state=${state}`), 'bans');
  }

  createBan(req: CreateBanRequest): Promise<Ban> {
    return this.post<Ban>('bans', req);
  }

  async unban(id: string): Promise<void> {
    // 204, no body to read.
    await this.request(`bans/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  async events(afterId?: number, limit = 50): Promise<EventPage> {
    const qs = new URLSearchParams({ limit: String(limit) });
    if (afterId !== undefined) qs.set('afterId', String(afterId));
    const body = await this.json<unknown>(`events?${qs.toString()}`);
    const events = list<EventPage['events'][number]>(body, 'events');
    const next =
      body && typeof body === 'object' && !Array.isArray(body)
        ? ((body as Record<string, unknown>).nextAfterId as number | null | undefined)
        : undefined;
    return { events, nextAfterId: next ?? null };
  }

  async relays(): Promise<Relay[]> {
    return list<Relay>(await this.json<unknown>('relays'), 'relays');
  }

  async webhooks(): Promise<Webhook[]> {
    return list<Webhook>(await this.json<unknown>('webhooks'), 'webhooks');
  }

  createWebhook(req: CreateWebhookRequest): Promise<Webhook> {
    return this.post<Webhook>('webhooks', req);
  }

  updateWebhook(id: string, req: CreateWebhookRequest): Promise<Webhook> {
    return this.json<Webhook>(`webhooks/${encodeURIComponent(id)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(req),
    });
  }

  async deleteWebhook(id: string): Promise<void> {
    await this.request(`webhooks/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  /** Test-send. Keyed by NAME, and works for config- and UI-sourced alike. */
  testWebhook(name: string): Promise<WebhookTestResult> {
    return this.post<WebhookTestResult>(`webhooks/${encodeURIComponent(name)}/test`, {});
  }
}

/** Turn a failed response into the API's own error, falling back to the status. */
async function apiError(res: Response): Promise<ApiError> {
  try {
    const body = (await res.json()) as { error?: { code?: string; message?: string } };
    const err = body.error;
    if (err?.message) return new ApiError(err.message, res.status, err.code ?? '');
  } catch {
    // Not JSON, or no body at all — the status is all we have.
  }
  return new ApiError(`HTTP ${res.status}`, res.status);
}
