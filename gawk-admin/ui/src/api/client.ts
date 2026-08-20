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
  ApiErrorCode,
  Ban,
  Broadcast,
  CreateBanRequest,
  CreateWebhookRequest,
  EventPage,
  KillRequest,
  KillResponse,
  Me,
  Relay,
  Webhook,
  WebhookTestResult,
} from './types.ts';

/**
 * An error carrying the API's own words: `{"error": {code, message}}` (§4.7).
 * `code` is what the UI branches on, `message` is what it shows.
 *
 * `ban` is the part that is easy to miss. Two refusals are *about* a specific
 * ban and return it alongside the error:
 *
 *   * `409 duplicate_active` — the ban already in force on that target.
 *   * `502 projection_failed` — the ban row WAS committed; only the projection
 *     to a Ban CR failed, so the record exists and the reconciler will heal it,
 *     but enforcement is not live yet. A caller that treated this as a plain
 *     failure would tell the operator nothing happened, which is false.
 */
export class ApiError extends Error {
  // Plain fields rather than constructor parameter properties:
  // `erasableSyntaxOnly` is on, matching gawk-app's and telemetry's tsconfig.
  status: number;
  code: ApiErrorCode | '';
  ban: Ban | null;

  constructor(message: string, status: number, code: ApiErrorCode | '' = '', ban: Ban | null = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.ban = ban;
  }
}

const BASE = 'api/v1/';

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

  // Every list route answers with a keyed envelope, so each of these reads its
  // one key. `?? []` covers a null/absent list on an otherwise valid response
  // (a degraded read), never a differently-shaped one.
  async broadcasts(): Promise<Broadcast[]> {
    return (await this.json<{ broadcasts: Broadcast[] }>('broadcasts')).broadcasts ?? [];
  }

  /** The one single-object route that is enveloped: `201 {"ban": {...}}`. */
  kill(id: string, req: KillRequest): Promise<KillResponse> {
    return this.post<KillResponse>(`broadcasts/${encodeURIComponent(id)}/kill`, req);
  }

  async bans(state: 'active' | 'all'): Promise<Ban[]> {
    return (await this.json<{ bans: Ban[] }>(`bans?state=${state}`)).bans ?? [];
  }

  createBan(req: CreateBanRequest): Promise<Ban> {
    return this.post<Ban>('bans', req);
  }

  async unban(id: string): Promise<void> {
    // 204, no body to read.
    await this.request(`bans/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }

  /**
   * A page of the audit feed. `nextAfterId` is always present and is non-null
   * only when this page came back full — so null means exhausted, and the view
   * stops offering "Load older" rather than paging into an empty response.
   */
  async events(afterId?: number, limit = 50): Promise<EventPage> {
    const qs = new URLSearchParams({ limit: String(limit) });
    if (afterId !== undefined) qs.set('afterId', String(afterId));
    const page = await this.json<EventPage>(`events?${qs.toString()}`);
    return { events: page.events ?? [], nextAfterId: page.nextAfterId ?? null };
  }

  async relays(): Promise<Relay[]> {
    return (await this.json<{ relays: Relay[] }>('relays')).relays ?? [];
  }

  async webhooks(): Promise<Webhook[]> {
    return (await this.json<{ webhooks: Webhook[] }>('webhooks')).webhooks ?? [];
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
    const body = (await res.json()) as {
      error?: { code?: ApiErrorCode; message?: string };
      ban?: Ban;
    };
    const err = body.error;
    if (err?.message) {
      return new ApiError(err.message, res.status, err.code ?? '', body.ban ?? null);
    }
  } catch {
    // Not JSON, or no body at all — the status is all we have.
  }
  return new ApiError(`HTTP ${res.status}`, res.status);
}
