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
  BanCursor,
  BanPage,
  BroadcastsPage,
  CreateBanRequest,
  CreateWebhookRequest,
  EventPage,
  KillRequest,
  KillResponse,
  Me,
  Relay,
  Room,
  RoomWithSecret,
  CreateRoomRequest,
  Webhook,
  WebhookTestResult,
} from './types.ts';

/**
 * An error carrying the API's own words: `{"error": {code, message}}` (§4.7).
 * `code` is what the UI branches on, `message` is what it shows.
 *
 * `ban` is the part that is easy to miss: `409 duplicate_active` is *about* a
 * specific ban — the one already in force on that target — and returns it
 * alongside the error, so a caller can show what is already there rather than
 * a bare conflict.
 *
 * A ban whose row committed but whose `Ban` CR did not is NOT here. It is a
 * `202 Accepted` with the ban in the body and `enforcement.inSync: false` on
 * it — a success, because the record is durable and the reconciler finishes
 * the job. Treating it as an error told the operator nothing had happened,
 * which was false, and invited a retry that now 409s against the row that does
 * exist.
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

/**
 * The sentence to show when a mutation came back `202 Accepted` — the record
 * landed, the Kubernetes enforcement object did not (yet).
 *
 * Returns the server's own `detail` (it knows which direction is out of step:
 * a pending ban is not enforced, a pending unban is still enforced), or null
 * when there is nothing to report. Absent `enforcement` is the ordinary case:
 * a `201`/`204` and every list row carry none.
 */
export function enforcementNotice(ban: Ban | null | undefined): string | null {
  if (!ban?.enforcement || ban.enforcement.inSync) return null;
  return (
    ban.enforcement.detail ??
    'This was recorded, but the relay-side enforcement object is not in step with it yet. ' +
      'The reconciler retries automatically; do not re-submit.'
  );
}

const BASE = 'api/v1/';

export class ApiClient {
  private readonly session: AuthSession;

  constructor(session: AuthSession) {
    this.session = session;
  }

  // `res.ok` is 2xx, so a 202 is a success here exactly like a 201 — the
  // caller reads `enforcement` to learn the difference. Only a non-2xx becomes
  // an ApiError.
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
  //
  // Broadcasts keeps its whole envelope: the coverage counters are what let
  // the view distinguish a quiet fleet from an unreachable one. Absent
  // counters (an older binary) default to "full coverage" — 0/0 — so nothing
  // is flagged on a response that carries no coverage to report.
  async broadcasts(): Promise<BroadcastsPage> {
    const page = await this.json<Partial<BroadcastsPage>>('broadcasts');
    return {
      broadcasts: page.broadcasts ?? [],
      podsResolved: page.podsResolved ?? 0,
      podsAnswered: page.podsAnswered ?? 0,
    };
  }

  /**
   * The one single-object route that is enveloped: `201 {"ban": {...}}` — and
   * `202 {"ban": {...}}` in the same shape when the row landed and the CR did
   * not, so there is one body to parse either way.
   */
  kill(id: string, req: KillRequest): Promise<KillResponse> {
    return this.post<KillResponse>(`broadcasts/${encodeURIComponent(id)}/kill`, req);
  }

  /**
   * A page of bans. `state=active` is the whole set and always exhausts;
   * `state=all` is history, paged by the composite cursor — pass the previous
   * page's `nextAfter` to continue, and a null one means the feed ended.
   */
  async bans(state: 'active' | 'all', after?: BanCursor, limit = 50): Promise<BanPage> {
    const qs = new URLSearchParams({ state, limit: String(limit) });
    if (after) {
      qs.set('afterCreatedAt', after.createdAt);
      qs.set('afterId', after.id);
    }
    const page = await this.json<Partial<BanPage>>(`bans?${qs.toString()}`);
    return { bans: page.bans ?? [], nextAfter: page.nextAfter ?? null };
  }

  /** `201` or `202`, both bare `Ban`; check `enforcementNotice` on the result. */
  createBan(req: CreateBanRequest): Promise<Ban> {
    return this.post<Ban>('bans', req);
  }

  /**
   * Unban. `204` is the clean case and has NO body — parsing one would throw
   * on an empty response — so it resolves to null. `202` means the row says
   * `removed` while the CR delete failed and the target is still banned; that
   * one answers WITH the removed ban, which the caller shows.
   */
  async unban(id: string): Promise<Ban | null> {
    const res = await this.request(`bans/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (res.status === 204) return null;
    return (await res.json()) as Ban;
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

  // R42 rooms (docs/44 D20). Every route 404s when the deployment has rooms
  // off; the shell consults `me.features.rooms` before offering the view.

  async rooms(): Promise<Room[]> {
    return (await this.json<{ rooms: Room[] }>('rooms')).rooms ?? [];
  }

  /** `201 {room, attachSecret?}` — the secret is shown once and never again. */
  createRoom(req: CreateRoomRequest): Promise<RoomWithSecret> {
    return this.post<RoomWithSecret>('rooms', req);
  }

  /** `200 {room, attachSecret}` — static rooms only (409 room_not_static). */
  rotateRoomSecret(name: string): Promise<RoomWithSecret> {
    return this.post<RoomWithSecret>(`rooms/${encodeURIComponent(name)}/rotate-secret`, {});
  }

  /** Deletes a room of either kind; for a dynamic room this IS "end room". */
  async deleteRoom(name: string): Promise<void> {
    await this.request(`rooms/${encodeURIComponent(name)}`, { method: 'DELETE' });
  }

  /** The dynamic-only alias of delete (409 room_not_dynamic on a static room). */
  async endRoom(name: string): Promise<void> {
    await this.request(`rooms/${encodeURIComponent(name)}/end`, { method: 'POST' });
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
