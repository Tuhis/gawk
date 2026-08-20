// Wire shapes for the portal API (docs/42 §4.7), pinned to what `internal/api`
// (AP4) actually serves. One home for them, so no view re-describes a response
// inline.
//
// The shapes here are the contract, not a guess: every list route answers with
// a KEYED envelope (`{"broadcasts": [...]}`), single objects come back bare
// except `kill`, and the refusal envelope is
// `{"error": {code, message}}` — carrying `ban` too on the two refusals that
// are about a specific ban. An earlier draft of this file accepted either a
// bare array or an envelope, because §4.7 named the rows without naming the
// wrapper. It now has one authority, so it has one shape.

/** `GET /api/v1/me` — the SPA's authorization probe. */
export interface Me {
  email: string;
  subject: string;
  roles: string[];
  /**
   * Server-side defaults the portal pre-fills into dialogs. AP4 always sends
   * this, sourced from `cfg.KillCooldown`; it stays optional only so a portal
   * served by an older binary still renders a usable kill dialog against the
   * documented fallback (§4.12).
   */
  defaults?: {
    /** `-kill-cooldown` in seconds. Documented default 600 (§4.7, §4.12). */
    killCooldownSeconds?: number;
  };
}

export type BanTargetType = 'broadcastId' | 'ip';
export type BanState = 'active' | 'expired' | 'removed';

export interface BanTarget {
  type: BanTargetType;
  value: string;
  /**
   * The operator-confirmed prefix length for an IP ban (§4.9): v4 defaults to
   * `/32`, v6 to `/64` because privacy-address rotation makes `/128` nearly
   * useless. Sent alongside the literal `"publisher"` value, which the server
   * resolves against relayscan.
   */
  prefixLength?: number;
}

export interface Ban {
  id: string;
  target: BanTarget;
  state: BanState;
  reason: string;
  createdAt: string;
  createdBy: string;
  /** null = permanent. */
  expiresAt: string | null;
  removedAt?: string | null;
  removedBy?: string | null;
  sourceBroadcastId?: string | null;
  crName?: string;
}

export interface BroadcastPod {
  pod: string;
  role: string;
  viewersLocal: number;
}

/**
 * Deep links, governed by `-app-base-url` / `-telemetry-base-url` (§4.12). The
 * server OMITS a link whose base URL is unconfigured, and the UI must then show
 * nothing — a dead link is worse than no link.
 */
export interface BroadcastLinks {
  /** `<appBaseUrl>/#/view/<id>` */
  watch?: string;
  /** `<telemetryBaseUrl>/#/broadcast/<key>` */
  telemetry?: string;
}

/**
 * Whether an active ban already covers this broadcast.
 *
 * `ban` is null when `banned` is false. The whole object being null is a THIRD
 * state and means *unknown*: AP4 degrades the fleet read rather than 503ing it
 * when Postgres is unreachable, so an operator can still see what is
 * broadcasting during a database outage — it just cannot say whether any of it
 * is banned. Rendering that as "not banned" would tell an operator a banned
 * broadcast is clean, which is the exact confusion the null exists to prevent.
 */
export interface BroadcastBanState {
  banned: boolean;
  ban: Ban | null;
}

export interface Broadcast {
  /** Raw, joinable. Portal-only: never in a webhook payload or a log (D8). */
  id: string;
  /** The HMAC'd key — the one form safe to export. */
  key: string;
  publisherActive: boolean;
  publisherRemoteIp?: string | null;
  startedAt: string;
  viewersGlobal: number;
  pods: BroadcastPod[];
  links?: BroadcastLinks;
  /** null = unknown (see BroadcastBanState), NOT "not banned". */
  banState: BroadcastBanState | null;
}

export type DeliveryState = 'pending' | 'delivered' | 'failed';

/**
 * One webhook delivery attempt-set for one event. Rendered per event in the
 * feed because "a failed delivery must be *seen*" (§4.10) — R40's DSA posture
 * inherits this pipe.
 */
export interface WebhookDelivery {
  webhookName: string;
  state: DeliveryState;
  attempts: number;
  lastError?: string | null;
  deliveredAt?: string | null;
  nextAttemptAt?: string | null;
}

export interface ModerationEvent {
  id: number;
  type: string;
  occurredAt: string;
  actor: string;
  broadcastKey?: string | null;
  broadcastId?: string | null;
  reason?: string | null;
  summary?: string | null;
  deliveries?: WebhookDelivery[];
}

export interface EventPage {
  events: ModerationEvent[];
  /**
   * Cursor for the next (older) page. The key is ALWAYS present; it is
   * non-null only when this page came back full, so `null` means the feed is
   * exhausted and there is nothing more to ask for.
   */
  nextAfterId: number | null;
}

export interface Relay {
  pod: string;
  reachable: boolean;
  version?: string;
  /** The relay's sanitized effective config — read-only in the portal (D10). */
  config?: Record<string, unknown> | null;
  error?: string | null;
}

export type WebhookSource = 'config' | 'ui';

export interface Webhook {
  /** Absent for config-sourced rows: they have no database identity. */
  id?: string;
  name: string;
  url: string;
  enabled: boolean;
  source: WebhookSource;
}

/** `POST /api/v1/webhooks/{name}/test` — the delivery outcome, for both sources. */
export interface WebhookTestResult {
  ok: boolean;
  status?: number;
  error?: string | null;
  deliveryId?: string;
}

/**
 * Every `error.code` `internal/api` emits. A union rather than a bare string so
 * a view that branches on one cannot quietly branch on a code that does not
 * exist — `source_immutable` and `projection_failed` both drive real UI.
 */
export type ApiErrorCode =
  | 'bad_request'
  | 'not_found'
  | 'duplicate_active'
  | 'duplicate_name'
  | 'source_immutable'
  | 'invalid_target'
  | 'ban_not_active'
  | 'projection_failed'
  | 'unavailable'
  | 'internal';

/** `POST /broadcasts/{id}/kill` — the one single-object route that is enveloped. */
export interface KillResponse {
  ban: Ban;
}

export interface KillRequest {
  reason: string;
  cooldownSeconds?: number;
}

export interface CreateBanRequest {
  target: BanTarget;
  /** RFC3339, or null for permanent. */
  expiresAt: string | null;
  reason: string;
  sourceBroadcastId?: string;
}

export interface CreateWebhookRequest {
  name: string;
  url: string;
  /** Write-only: the API never returns a secret, for either source (§4.7). */
  secret?: string;
  enabled: boolean;
}
