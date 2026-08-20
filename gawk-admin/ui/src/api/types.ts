// Wire shapes for the portal API (docs/42 §4.7). One home for them, so no view
// re-describes a response inline.
//
// Where §4.7 pins a field, the type pins it. Where it does not — the inner
// shape of `banState`, the events envelope — the type is deliberately tolerant
// and the views render defensively; `internal/api` (AP4) is the authority and
// this file follows it rather than inventing a second contract.

/** `GET /api/v1/me` — the SPA's authorization probe. */
export interface Me {
  email: string;
  subject: string;
  roles: string[];
  /**
   * Server-side defaults the portal pre-fills into dialogs. Optional: the
   * dialogs fall back to the documented values (§4.12) when absent, so an
   * older backend still renders a usable kill dialog.
   */
  defaults?: {
    /** `-kill-cooldown` in seconds. Default 600 (§4.7, §4.12). */
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

/** Whether an active ban already covers this broadcast. */
export interface BroadcastBanState {
  banned: boolean;
  ban?: Ban | null;
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
  banState?: BroadcastBanState | null;
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
  /** Cursor for the next (older) page; absent when the feed is exhausted. */
  nextAfterId?: number | null;
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
