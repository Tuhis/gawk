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
  /**
   * Optional surfaces this deployment serves. `rooms` (R42, docs/44 D20) is
   * default-off: with it off `/api/v1/rooms` answers 404, so the shell shows
   * no navigation to it. Optional so an older binary reads as "nothing extra".
   */
  features?: {
    rooms?: boolean;
    /**
     * R40 content flagging (docs/71 §9.4), default-off like rooms: with it
     * off every screening route answers 404, so the shell offers no
     * calibration view and the fleet view polls no flags.
     */
    screening?: boolean;
    /** R74 PoP relays (docs/76 §7.2), default-off: the PoPs view and routes. */
    pops?: boolean;
    /** The named tier is configured, so a PoP can get a named address. */
    popNamedAddress?: boolean;
  };
}

export type RoomKind = 'static' | 'dynamic';

/** Everyone in a live room, as its home relay pod sees them (R49). */
export interface RoomParticipant {
  id: number;
  /** Chosen by the participant: unauthenticated free text. */
  nickname: string;
  clientKind: 'web-viewer' | 'web-broadcaster' | 'native';
  streaming: boolean;
  /** Reserved for a voice bridge; false today. */
  speaking: boolean;
  identity?: string;
}

/**
 * One attached broadcast. `live` and `viewers` are absent when the room is not
 * live: the CR says what is attached, never whether it is streaming.
 */
export interface RoomAttachment {
  broadcastId: string;
  label?: string;
  live?: boolean;
  viewers?: number;
  attachedAt?: string;
  links?: { watch: string };
}

export interface RoomCounts {
  participants: number;
  /** Participants with a broadcast attached; `watching` is the rest. */
  streaming: number;
  watching: number;
  attachments: number;
}

/**
 * `GET /api/v1/rooms` rows (R42, reshaped in R49). `name` is the raw code — a
 * joinable secret exactly like a broadcast ID (docs/44 D16). `key` is the
 * fleet's HMAC'd handle, empty until a relay pod has homed the room.
 *
 * `live` says whether a reachable relay pod is home for the room; when false
 * the counts of people are unknown, not zero. `attachments` and
 * `participants` are only on `GET /api/v1/rooms/{name}` (docs/50 D4).
 */
export interface Room {
  name: string;
  kind: RoomKind | '';
  /** Display form: the slug as configured, or the upper-cased dynamic code. */
  code: string;
  displayName?: string;
  maxBroadcasts?: number;
  live: boolean;
  links?: { join: string };
  counts: RoomCounts;
  attachments?: RoomAttachment[];
  participants?: RoomParticipant[];
  homeHolder?: string;
  key?: string;
  createdAt?: string;
  emptySince?: string;
  /** Whether a static room is gated. The secret itself is never returned. */
  hasAttachSecret: boolean;
  /** Created by the portal; false for a `kubectl apply`'d room. */
  managed: boolean;
  /**
   * The Room CR's `spec.screening` (R40, docs/71 D11): `exempt` while the
   * sampler skips the room's broadcasts. Absent (an older binary) reads as
   * screened, the default.
   */
  screening?: RoomScreening;
  /**
   * The Room CR's `spec.integrations.mumble` (R79, docs/80 D13): the room's
   * voice channel. Absent means the room has no voice.
   */
  mumble?: RoomMumble;
}

/**
 * A room's Mumble voice channel (docs/80 D13). A static room names an
 * existing channel as a "/"-separated path below the root (empty or absent:
 * the root); a dynamic room created with voice has `temporary: true`.
 */
export interface RoomMumble {
  channel?: string;
  temporary?: boolean;
}

/**
 * `exempt` or `''` (screened). Only a static room with an attach secret may be
 * exempt (docs/71 D11): an open or dynamic room would be an unscreened lane for
 * anyone who found the link.
 */
export type RoomScreening = 'exempt' | '';

export interface CreateRoomRequest {
  code: string;
  displayName?: string;
  maxBroadcasts?: number;
  withAttachSecret?: boolean;
  /** R79: voice on an existing channel; never `temporary` for a static room. */
  mumble?: RoomMumble;
}

/**
 * The create (201) and rotate (200) body: the room plus the ONE-TIME attach
 * secret. Absent when no secret was minted. This is the only place a secret
 * is ever on the wire, and the view shows it exactly once.
 */
export interface RoomWithSecret {
  room: Room;
  attachSecret?: string;
}

/**
 * A PoP's card state (docs/76 §7.2). `waiting-for-pairing` holds until the
 * PoP pairs with its CURRENT string, so it is also the state right after a
 * re-pair.
 */
export type PopState = 'waiting-for-pairing' | 'linked' | 'offline' | 'revoked' | 'ended' | 'invalid';

export interface PopBudget {
  maxMbps: number;
  maxStreams: number;
}

export interface PopBroadcast {
  /** Raw and joinable (docs/42 D8). */
  id: string;
  label?: string;
  publisherLanAddr?: string;
  localViewers: number;
  outsideViewers: number;
  /** Home is carrying it now; otherwise it is party-only. */
  forwarded: boolean;
  wall?: 'on' | 'hidden';
  links?: { watch: string };
}

export interface PopHealthLine {
  key: string;
  ok: boolean;
  detail?: string;
}

/**
 * `GET /api/v1/pops` rows (R74): the Pop CR merged with the link-home pod's
 * live view. `link` and `broadcasts` carry the PoP's and its publishers'
 * addresses, which this portal may show and events never carry (docs/76 §8).
 * `broadcasts` and `health` are only on `GET /api/v1/pops/{name}`.
 */
export interface Pop {
  name: string;
  displayName?: string;
  state: PopState;
  stateSince?: string;
  notAfter?: string;
  createdAt?: string;
  managed: boolean;
  revoked: boolean;
  pairing: { issuedAt?: string; expiresAt?: string; paired: boolean; pairedAt?: string };
  uplink: PopBudget;
  downlink: PopBudget;
  limits: { maxBroadcasts: number; maxDownlinkStreams: number };
  guests: { autoSwitch: boolean };
  wall?: { roomCode: string };
  namedAddress: { enabled: boolean; host?: string; certificateNotAfter?: string };
  link?: {
    connected: boolean;
    since?: string;
    version?: string;
    proto?: number;
    publicAddrs?: string[];
    lanAddr?: string;
    pod?: string;
  };
  live?: {
    pod: string;
    live: number;
    forwarded: number;
    outsideViewers: number;
    uplinkMbps: number;
    uplinkStreams: number;
    uplink: PopBudget;
    downlink: PopBudget;
    downlinkStreams: number;
    guestsConnected: number;
    offersSent: number;
  };
  broadcasts?: PopBroadcast[];
  health?: PopHealthLine[];
}

/** The only shape that carries a pairing string; shown once, never again. */
export interface PopWithPairing {
  pop: Pop;
  pairingString: string;
  expiresAt: string;
}

export interface PopBudgetRequest {
  maxMbps?: number;
  maxStreams?: number;
}

export interface CreatePopRequest {
  name: string;
  displayName?: string;
  notAfter?: string;
  uplink?: PopBudgetRequest;
  downlink?: PopBudgetRequest;
  limits?: { maxBroadcasts?: number; maxDownlinkStreams?: number };
  guests?: { autoSwitch?: boolean };
  wall?: { roomCode?: string; maxParticipants?: number };
  namedAddress?: boolean;
}

/** An absent field is left alone; `notAfter: ''` clears the end date. */
export interface PopPatchRequest {
  displayName?: string;
  notAfter?: string;
  uplink?: PopBudgetRequest;
  downlink?: PopBudgetRequest;
  limits?: { maxBroadcasts?: number; maxDownlinkStreams?: number };
  guests?: { autoSwitch?: boolean };
  namedAddress?: boolean;
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

/**
 * Whether the Kubernetes enforcement object currently MATCHES this record.
 *
 * Present only on a mutation's own response, and only on the `202 Accepted`
 * that says the two disagree: the `bans` row is committed, its `Ban` CR write
 * is not, and the reconciler will close the gap within a minute. List and read
 * routes never carry it — a stored row has no in-flight projection to report
 * on — so `enforcement === undefined` is the ordinary case and means there is
 * nothing to say.
 *
 * `inSync` and not `enforced` because it has to read correctly in BOTH
 * directions. A pending create means recorded and NOT yet enforced; a pending
 * unban means lifted in the record and STILL enforced. `detail` is the
 * server's own sentence for the case at hand, and the UI renders it verbatim.
 */
export interface BanEnforcement {
  inSync: boolean;
  detail?: string;
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
  /** Only on a `202` mutation response; see BanEnforcement. */
  enforcement?: BanEnforcement;
}

/**
 * The composite history cursor (docs/42 §11.2): ban rows are UUID-keyed, so a
 * timestamp alone is not unique and a UUID alone does not order — the pair
 * does both. Opaque to the UI: it round-trips the server's own values.
 */
export interface BanCursor {
  createdAt: string;
  id: string;
}

/**
 * `GET /api/v1/bans` — the rows plus the history cursor.
 *
 * `nextAfter` mirrors the events feed's contract: the key is always present,
 * non-null only when a `state=all` page came back full. The active set is a
 * SET (janitor-bounded), never paged, so its `nextAfter` is always null.
 */
export interface BanPage {
  bans: Ban[];
  nextAfter: BanCursor | null;
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

/**
 * `GET /api/v1/broadcasts` — the rows plus the scan's coverage.
 *
 * Relayscan treats a pod-level scrape failure as data, not an error, so the
 * list can be silently short — or empty — while relay pods are carrying live
 * broadcasts it could not see. `podsAnswered < podsResolved` is how the view
 * knows to say so instead of rendering a reassuring "nothing is broadcasting"
 * (the same honesty rule `banState: null` enforces on the Postgres axis).
 */
export interface BroadcastsPage {
  broadcasts: Broadcast[];
  /** Relay pods the headless-Service lookup resolved. */
  podsResolved: number;
  /** Of those, how many answered the broadcasts scrape. */
  podsAnswered: number;
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
  /**
   * The broadcast's unresolved `flag`-mode content flag (R40, docs/71 §9.4),
   * which pins the row to the top of the fleet view. Absent or null when
   * there is none — or when screening is off.
   */
  contentFlag?: ContentFlag | null;
  /**
   * The PoP a party broadcast started at (R74, docs/76 §7.2). Absent on every
   * other broadcast. A party-only row (home does not carry it) has an empty
   * `key`, no pods and no publisher IP.
   */
  pop?: BroadcastPop;
}

/** A party broadcast's PoP badge. */
export interface BroadcastPop {
  name: string;
  /** Home carries it now; false is party-only. */
  forwarded: boolean;
  label?: string;
  /** The broadcaster's address on the party LAN, as the PoP reports it. */
  publisherLanAddr?: string;
  localViewers: number;
  outsideViewers: number;
}

// ---------------------------------------------------------------------------
// R40 content flagging (docs/71 §9.4)
// ---------------------------------------------------------------------------

/** `flag` pages a human; `observe` is calibration and pages no one (D19). */
export type ContentFlagMode = 'flag' | 'observe';

/** A flag is UNRESOLVED while `open` or `acknowledged` (D9). */
export type ContentFlagState = 'open' | 'acknowledged' | 'resolved';

export type ContentFlagResolution = 'cleared' | 'dismissed' | 'killed' | 'auto_killed' | 'ended';

/**
 * What the sampler last said (D9). `unknown` means no report for longer than
 * `-content-flag-stale-after`: a silent sampler is NOT evidence of a clean
 * broadcast, so the flag keeps re-paging.
 */
export type ContentFlagSignal = 'live' | 'unknown' | 'cleared';

/** The sampler's window at its last report: never a frame (D8). */
export interface ContentFlagWindow {
  start: string;
  end: string;
  samples: number;
  flaggedSamples: number;
}

export interface ContentFlagScores {
  max: number;
  mean: number;
}

/** One content flag, as the portal sees it (§9.4). */
export interface ContentFlag {
  id: number;
  mode: ContentFlagMode;
  state: ContentFlagState;
  resolution?: ContentFlagResolution | null;
  /** `stale`, `deadline` or `policy` — why an automatic resolution happened. */
  resolutionReason?: string | null;
  /** Raw, joinable: portal-only, like every broadcast ID on screen (D8). */
  broadcastId: string;
  broadcastKey: string;
  publisherEpoch: string;
  samplerId: string;
  raisedAt: string;
  updatedAt: string;
  lastPostAt: string;
  acknowledgedAt?: string | null;
  acknowledgedBy?: string | null;
  resolvedAt?: string | null;
  resolvedBy?: string | null;
  signal: ContentFlagSignal;
  window: ContentFlagWindow;
  scores: ContentFlagScores;
  /** The flagged labels that fired in the window, and in how many samples. */
  labels: string[];
  labelCounts?: Record<string, number>;
  peakScore: number;
  /** How many `content_flag.reminded` pages this flag has sent. */
  reminders: number;
  /**
   * A dismissed flag: when the mute its dismissal set ends (an `episode`
   * mute ends at the clear that ended it). Any other: while a mute covers
   * the flag's session, when it ends (D10, docs/72 D5).
   */
  mutedUntil?: string | null;
  /** The dismissal's scope (R70); absent when the flag was not dismissed. */
  muteScope?: MuteScope | null;
  /** Window trips this one incident absorbed (R70, docs/72 D3). */
  trips?: number;
  /** Set while the sampler's clear is held for the rejoin window (R70). */
  clearedAt?: string | null;
  /** An earlier incident on the broadcast was reviewed and the signal came back (R70). */
  returnedAfterReview?: boolean;
  /**
   * The watch link, built server-side from `-app-base-url` exactly like a
   * broadcast row's and omitted when that is unset. It is what makes an
   * observed flag worth listing: the operator joins live while the trigger is
   * on screen (OD14).
   */
  links?: { watch?: string };
}

/**
 * The screening plane's health, read by the portal's two banners: no pager
 * (D12) and missing sampler shards (D22).
 */
export interface ScreeningStatus {
  /** False when no enabled webhook would deliver `content_flag.raised`. */
  flagRoute: boolean;
  /** The `of` of the most recent sampler poll; 0 before any sampler polled. */
  expectedShards: number;
  /** Shard indices that have not polled for 30 s. */
  missingShards: number[];
  lastPollAt?: string | null;
}

/** `GET /api/v1/content-flags`. */
export interface ContentFlagPage {
  contentFlags: ContentFlag[];
  screening: ScreeningStatus;
}

/** How long a dismissal mutes re-flagging (R70, docs/72 D5). */
export type MuteScope = 'episode' | 'duration' | 'session';

/** Why an operator judged a flag not a violation (R70, docs/72 D5). */
export type DismissReason = 'game_scene' | 'not_explicit' | 'detector_error' | 'other';

/** `POST /api/v1/content-flags/{id}/dismiss`. Both fields optional. */
export interface DismissRequest {
  reason?: DismissReason;
  scope?: MuteScope;
}

/** The timeline's patterns (R70, docs/72 D8). */
export type TimelinePattern = 'none' | 'isolated' | 'flicker' | 'sustained' | 'recurring' | 'unknown';

/** Every level the timeline draws (docs/72 §4). */
export interface TimelineLevels {
  floor: number;
  /** The newest session's reported threshold; absent from a pre-R70 sampler. */
  threshold?: number | null;
  window?: number | null;
  windowHits?: number | null;
  critical: number;
  sustainedAfterSeconds: number;
  /** 0 when the rejoin window is off. */
  rejoinSeconds: number;
}

export interface TimelineSession {
  publisherEpoch: string;
  broadcastId: string;
  firstSampleAt: string;
  lastSampleAt: string;
  samplesTaken: number;
  samplesStored: number;
  floor: number;
  threshold?: number | null;
  window?: number | null;
  windowHits?: number | null;
  mode: ContentFlagMode;
  samplerId: string;
}

/** One time bucket holding stored samples; empty buckets are omitted. */
export interface TimelineBucket {
  start: string;
  max: number;
  scores: Record<string, number>;
  stored: number;
  above: number;
  critical: number;
}

export interface TimelineEvent {
  id: number;
  type: string;
  occurredAt: string;
  actor: string;
  summary: string;
}

export interface TimelineSummary {
  pattern: TimelinePattern;
  thresholdKnown: boolean;
  sampledSeconds: number;
  aboveSeconds: number;
  criticalSeconds: number;
  longestRunSeconds: number;
  runs: number;
  sustainedRuns: number;
  peak: number;
  peakLabel?: string | null;
}

export interface TimelineSuggestion {
  verdict?: 'violation' | 'not_violation' | null;
  banSeconds?: number | null;
  why?: string | null;
}

/** `GET /api/v1/screening/timeline` (R70, docs/72 D7). */
export interface Timeline {
  schema: string;
  broadcastKey: string;
  broadcastId?: string | null;
  live: boolean;
  from: string;
  to: string;
  bucketSeconds: number;
  truncated: boolean;
  levels: TimelineLevels;
  labels: string[];
  sessions: TimelineSession[];
  buckets: TimelineBucket[];
  incidents: ContentFlag[];
  events: TimelineEvent[];
  summary: TimelineSummary;
  suggestion: TimelineSuggestion;
  links?: { watch?: string } | null;
}

/** `POST /api/v1/screening/replay` — a candidate window rule over stored samples (D21). */
export interface ReplayRequest {
  threshold: number;
  window: number;
  windowHits: number;
  /** A subset of the stored labels; empty means all of them. */
  labels?: string[];
  /** RFC3339. */
  from: string;
  to: string;
}

export interface ReplayTrip {
  broadcastKey: string;
  broadcastId: string;
  publisherEpoch: string;
  at: string;
  peakScore: number;
}

export interface ReplayResponse {
  schema: string;
  request: ReplayRequest;
  /** The highest sample floor among the replayed sessions. */
  floor: number;
  sessions: number;
  samplesTaken: number;
  samplesStored: number;
  broadcastHours: number;
  wouldTrips: number;
  wouldTripsPer100h: number;
  /** Newest first, capped; `truncated` says the list was cut (the count is exact). */
  trips: ReplayTrip[];
  truncated: boolean;
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

/** Which kind of row an event is (R50). */
export type EventCategory = 'moderation' | 'activity';

export interface ModerationEvent {
  id: number;
  type: string;
  occurredAt: string;
  actor: string;
  /**
   * `moderation` is the audit trail of operator actions; `activity` is what
   * the fleet did, ingested from the relay event bus. Always present.
   */
  category: EventCategory;
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

/**
 * A value of a webhook's event filter (R49, docs/50 D8). The moderation types,
 * the four room activity types, and `room.participant_rejoined`, which is not
 * an event but opts into the reconnect pair a room's move to another relay pod
 * produces. R40 (docs/71 D16) adds three moderation types and the opt-in
 * `content_flag.observed`, which a webhook receives only when its filter
 * names it — calibration data must never page an existing pager. R74 adds the
 * five `pop.*` moderation types, an operator's changes to a PoP relay.
 */
export type WebhookEventName =
  | 'broadcast.killed'
  | 'ban.created'
  | 'ban.expired'
  | 'ban.removed'
  | 'content_flag.raised'
  | 'content_flag.reminded'
  | 'content_flag.resolved'
  | 'content_flag.observed'
  | 'room.created'
  | 'room.ended'
  | 'room.secret_rotated'
  | 'room.screening_changed'
  | 'pop.created'
  | 'pop.updated'
  | 'pop.repaired'
  | 'pop.revoked'
  | 'pop.deleted'
  | 'room.attached'
  | 'room.detached'
  | 'room.participant_joined'
  | 'room.participant_left'
  | 'room.participant_rejoined';

export interface Webhook {
  /** Absent for config-sourced rows: they have no database identity. */
  id?: string;
  name: string;
  url: string;
  enabled: boolean;
  source: WebhookSource;
  /**
   * The event filter. `null` (or absent, from an older server) is the
   * default: every moderation event and no room activity event.
   */
  events?: WebhookEventName[] | null;
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
 * exist — `source_immutable` drives real UI, and `duplicate_active` is the one
 * refusal that comes back with a `ban` attached.
 *
 * There is deliberately no `projection_failed` here any more. A ban row that
 * committed without its CR is a `202 Accepted` carrying `enforcement`, not an
 * error at all — see BanEnforcement.
 */
export type ApiErrorCode =
  | 'bad_request'
  | 'not_found'
  | 'duplicate_active'
  | 'duplicate_name'
  | 'source_immutable'
  | 'invalid_target'
  | 'ban_not_active'
  | 'room_exists'
  | 'room_not_static'
  | 'room_not_dynamic'
  | 'room_not_owned'
  | 'screening_not_allowed'
  | 'no_flag_route'
  | 'pop_exists'
  | 'pop_revoked'
  | 'wall_code_taken'
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
  /** `null` keeps the default filter; an update replaces the filter. */
  events?: WebhookEventName[] | null;
}
