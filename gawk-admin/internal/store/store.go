// Package store is gawk-admin's system of record: the Postgres half of R39
// (docs/42 D3, §4.6).
//
// Two rules shape everything here.
//
//   - **The serving process never runs DDL.** Migrations are applied by a
//     separate step (Migrate, driven by the `migrate` subcommand from a Helm
//     hook Job — §4.15/D18); a serving pod only READS the schema version and
//     refuses to serve when it is older than this binary's minimum. That is
//     what makes a multi-replica rollout safe and rollback mean "redeploy the
//     previous version" instead of "restore the database".
//   - **Concurrency correctness lives in the database, not in Go.** Two
//     gawk-admin replicas serve writes simultaneously (D16), so
//     one-active-ban-per-target is a partial unique index and delivery claims
//     are FOR UPDATE SKIP LOCKED. Nothing here assumes a single writer.
//
// Ban targets are normalized and named through gawk-server/moderation (D13) —
// never re-implemented — so a row and the Ban CR the relay enforces can never
// disagree about what "the same target" means.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// Sentinel errors. Check with errors.Is.
var (
	// ErrNotFound: no row with that identity.
	ErrNotFound = errors.New("store: not found")
	// ErrDuplicateActive: an active ban already covers that target. The API
	// turns this into a 409 carrying the ban that already exists (§4.7).
	ErrDuplicateActive = errors.New("store: an active ban already covers this target")
	// ErrDuplicateName: a webhook with that name already exists.
	ErrDuplicateName = errors.New("store: a webhook with that name already exists")
	// ErrNotActive: the ban is not active, so the requested transition is not
	// one the state machine allows (active → expired | removed, one way).
	ErrNotActive = errors.New("store: ban is not active")
	// ErrSchemaTooOld: the database schema predates this binary's minimum.
	// Readiness fails on it; the fix is to run the migrate step, never to let
	// the serving process apply DDL of its own.
	ErrSchemaTooOld = errors.New("store: database schema is older than this build requires")
	// ErrSchemaDirty: a previous migration failed part-way. Operator action.
	ErrSchemaDirty = errors.New("store: database schema is marked dirty by a failed migration")
)

// BanState is the ban lifecycle. The only legal transitions are
// active → expired and active → removed; nothing ever returns to active. A
// re-ban of the same target is a NEW row (and, because CR names are
// deterministic, the same CR updated in place).
type BanState string

const (
	BanActive  BanState = "active"
	BanExpired BanState = "expired"
	BanRemoved BanState = "removed"
)

// StateFilter values accepted by ListBans.
const (
	FilterActive = "active"
	FilterAll    = "all"
)

// Ban is one row of the `bans` table.
//
// Target is a moderation.Target rather than two loose strings so that the
// compiler carries the D13 contract: anything constructing a Ban goes through
// the same normalization the relay evaluates against.
type Ban struct {
	ID     uuid.UUID
	Target moderation.Target
	State  BanState
	// Reason is operator-private context. It renders in the portal and lives
	// in Postgres; relays log it at Debug only (docs/42 §5).
	Reason    string
	CreatedAt time.Time
	CreatedBy string
	// ExpiresAt nil means permanent.
	ExpiresAt *time.Time
	RemovedAt *time.Time
	RemovedBy string
	// SourceBroadcastID is the raw broadcast ID the action was taken against,
	// including for IP bans (it is how the portal explains "why this IP").
	SourceBroadcastID string
	// CRName is the deterministic Ban CR name (moderation.CRName). Stored so a
	// row and its CR correlate from psql alone.
	CRName string
}

// BroadcastID is the broadcast a ban is about: the one it was taken from, or —
// for a ban on a broadcast ID — its target; "" for an IP ban typed in by
// address. It is the ONE rule every producer of a ban event uses (internal/api
// for created/removed and the inline expiry, internal/kube for adoption and
// expiry), so all of a ban's events carry the same raw ID in their event
// column, their `subject` and their summary (docs/52 D9).
func (b Ban) BroadcastID() string {
	if b.SourceBroadcastID != "" {
		return b.SourceBroadcastID
	}
	if b.Target.Type == moderation.TargetBroadcastID {
		return b.Target.Value
	}
	return ""
}

// Active reports whether the ban is in force at now. It answers from the row's
// own expiry, exactly as the relay does at check time — a row whose janitor
// sweep has not run yet is still expired.
func (b Ban) Active(now time.Time) bool {
	if b.State != BanActive {
		return false
	}
	return b.ExpiresAt == nil || now.Before(*b.ExpiresAt)
}

// Record projects the row into the shared evaluation record — the exact shape
// that becomes a Ban CR spec.
func (b Ban) Record() moderation.Record {
	return moderation.Record{
		Target:    b.Target,
		ExpiresAt: b.ExpiresAt,
		Reason:    b.Reason,
		CreatedBy: b.CreatedBy,
	}
}

// Event types written to moderation_events. content_flag.raised is reserved
// for R40 (docs/42 §4.11) and is deliberately named here so the vocabulary is
// fixed before anything produces it.
const (
	EventBroadcastKilled = "broadcast.killed"
	EventBanCreated      = "ban.created"
	EventBanExpired      = "ban.expired"
	EventBanRemoved      = "ban.removed"
	EventContentFlag     = "content_flag.raised" // R40

	// Room events (R42, docs/44 D20 / §9 RM7). room.ended is recorded both by
	// the portal (an operator ended or deleted a room) and by the reconciler's
	// sweep (the relay ended a dynamic room on its own — grace expiry or the
	// creator); the two are deduplicated through RoomEndedSince.
	EventRoomCreated       = "room.created"
	EventRoomEnded         = "room.ended"
	EventRoomSecretRotated = "room.secret_rotated"
)

// AllEventTypes is the closed vocabulary of `type` values a moderation_events
// row may carry: the enum of the `type` filter on GET /api/v1/events and of
// the `type` field on every event it returns (R48 docs/49 D3).
//
// It is a FUNCTION returning a fresh slice rather than an exported slice
// variable, because a package-level slice is writable by any importer and this
// one is a contract the OpenAPI drift test holds the document to.
//
// Order is the order a reader expects to meet them in, not alphabetical:
// enforcement first, then rooms. R50 appends the ingested activity types here
// (docs/51); R49 is where WebhookEventTypes first differs from this list.
func AllEventTypes() []string {
	return []string{
		EventBroadcastKilled,
		EventBanCreated,
		EventBanExpired,
		EventBanRemoved,
		EventContentFlag,
		EventRoomCreated,
		EventRoomEnded,
		EventRoomSecretRotated,
	}
}

// FeedEventTypes is every `type` a row on GET /api/v1/events may carry: the
// moderation vocabulary plus R50's ingested activity types (docs/51 D5).
//
// It is deliberately NOT AllEventTypes: that one is the MODERATION row table,
// which R51's contract holds equal to events.ModerationRowTypes() so every
// row type maps to exactly one CloudEvents type and can be delivered to a
// webhook. An activity row is never delivered — it is ingested from the bus,
// which already carries the CloudEvent — so it belongs to the feed's
// vocabulary and to no other.
func FeedEventTypes() []string {
	return append(AllEventTypes(), ActivityEventTypes()...)
}

// WebhookEventTypes is the subset of AllEventTypes the dispatcher may forward
// to a webhook.
//
// Today it is the whole vocabulary — every stored event is a moderation event
// and every moderation event pages someone. It exists as its own helper
// because the two stop being the same list at R49 (docs/50 D6), when R50's
// ingested activity events become stored-but-not-forwarded by default, and
// because docs/52 D7 holds THIS list — not AllEventTypes — equal to the
// AsyncAPI `webhook` channel's messages.
func WebhookEventTypes() []string {
	return AllEventTypes()
}

// IsEventType reports whether t is in the closed vocabulary. The API validates
// a `type` filter value with it, so an unknown name is a 400 rather than a
// silently empty page.
func IsEventType(t string) bool {
	for _, known := range FeedEventTypes() {
		if known == t {
			return true
		}
	}
	return false
}

// The payload keys a delivery is built from.
//
// This was a security boundary until docs/52 D9: Payload may carry raw
// broadcast IDs, IP addresses and CIDRs (the portal needs them), and the rule
// was that none of it reached a webhook. What survives of that rule is the
// part that never moved — **no IP address and no attach secret is ever built
// into a delivery** — and the way it is enforced: internal/notify reads these
// named keys into the typed `data` of the event, and nothing else in the
// payload is even looked at.
const (
	PayloadReason  = "reason"
	PayloadSummary = "summary"
	// PayloadEnforcement carries the EnforcementState the producer graded the
	// event with. It is webhook-safe by CONSTRUCTION rather than by review:
	// its vocabulary is closed (see Event.EnforcementState), so unlike the
	// other two it cannot be turned into a channel for free text — a producer
	// that wrote a raw ID under this key would find it dropped, not forwarded.
	PayloadEnforcement = "enforcement"
	// PayloadRoomKey carries the fleet's HMAC'd handle for a room (the Room
	// CR's status.key, docs/44 D16): what the portal's rooms view, its links
	// and its filters are keyed by. The read side (Event.RoomKey) accepts only
	// a hex digest, so a producer that wrote the code under this key by
	// mistake gets it dropped rather than built into a portal link. The code
	// itself travels under PayloadRoom and is delivered as `roomCode` since
	// docs/52 D9.
	PayloadRoomKey = "roomKey"
	// PayloadRoom is the room's raw code (the CR name). Since docs/52 D9 it
	// is delivered too, as the event's `roomCode` and its `subject`: it is a
	// join capability and the receiver is a channel the operator chose.
	PayloadRoom = "room"
	// PayloadDisplayCode is the code as shown to people — the static slug's
	// configured casing, which PayloadRoom has normalised away. Delivered
	// beside it for the same reason: "TuhisTestLab" is what the operator
	// named the room, and a notification that says `tuhistestlab` is one they
	// have to translate.
	PayloadDisplayCode = "displayCode"
	// PayloadRoomKind is "static" or "dynamic". The summary sentence names
	// the kind too; the property is what a consumer filters on.
	PayloadRoomKind = "kind"
)

// EnforcementState says whether the Kubernetes object that actually ENFORCES
// an event was in step with the record when the event was written.
//
// A ban is two writes in two systems that cannot share a transaction (the row
// and the Ban CR), so an event is not automatically a true statement about
// enforcement: a recorded-but-unprojected kill has not killed anything yet.
// Events therefore carry which of the two they are, and both the webhook
// payload and Summarize's sentence are graded on it.
//
// `InSync`/`Pending` rather than `enforced`/`not enforced` because the state
// has to read correctly in BOTH directions — a pending create is recorded and
// NOT enforced; a pending removal is lifted in the record while the target is
// STILL enforced. The same reason internal/api's wire field is named `inSync`.
type EnforcementState string

const (
	// EnforcementInSync is the ordinary case: record and enforcement agree.
	// It is the EMPTY string so that "there is nothing to say" is the zero
	// value — a payload key that is simply absent, exactly as internal/api's
	// 201 carries no `enforcement` object.
	EnforcementInSync EnforcementState = ""
	// EnforcementPending: the row is committed and the CR write did not land.
	// The reconciler heals it; until then the record is ahead of (create) or
	// behind (removal) what the relays enforce.
	EnforcementPending EnforcementState = "pending"
)

// Event is one row of moderation_events — the audit trail and the source every
// webhook delivery is derived from.
type Event struct {
	ID         int64
	Type       string
	OccurredAt time.Time
	Actor      string
	// BroadcastKey is the HMAC'd key (Registry.ObfuscateID): what the portal's
	// links and filters are keyed by.
	BroadcastKey string
	// BroadcastID is the raw, joinable ID. Delivered in the event's webhook
	// (its `subject` and `broadcastId`) since docs/52 D9; still never logged
	// above Debug (D8).
	BroadcastID string
	// Payload is free-form context. See the PayloadReason/PayloadSummary
	// comment above before forwarding any of it anywhere.
	Payload json.RawMessage
	// Category is CategoryModeration (what an operator did) or
	// CategoryActivity (what the fleet did, ingested from the R50 bus).
	// Empty on a value being written means moderation.
	Category string
	// Source is the CloudEvents id of the bus message an activity row came
	// from, and the UNIQUE key that makes ingest exactly-once. Empty for every
	// portal-originated row.
	Source string
}

// EnforcementState reports how the producer graded this event's enforcement.
//
// The vocabulary is CLOSED here, on the read side: anything that is not the
// one recognized value reads as EnforcementInSync. That is what makes
// PayloadEnforcement safe to forward to a webhook without a per-value review —
// the accessor can only ever return one of two constants, so no producer's
// free text (or raw broadcast ID) can ride the field out of the deployment.
func (e Event) EnforcementState() EnforcementState {
	if e.PayloadString(PayloadEnforcement) == string(EnforcementPending) {
		return EnforcementPending
	}
	return EnforcementInSync
}

// RoomKey returns the HMAC'd room key this event carries, or "".
//
// The vocabulary is CLOSED on the read side, like EnforcementState: only a
// lower-case hex digest of 8–64 characters passes, so this accessor cannot
// return a room code — the broadcast alphabet has no lower-case letters and a
// static slug is refused unless it happens to be pure hex, which the producers
// never write here (they write the code under PayloadRoom). That is what makes
// PayloadRoomKey safe for internal/notify to copy into a webhook body.
func (e Event) RoomKey() string {
	key := e.PayloadString(PayloadRoomKey)
	if !isHexKey(key) {
		return ""
	}
	return key
}

// isHexKey reports whether s looks like a per-process HMAC digest as the relay
// renders one: lower-case hex, an even length between 8 and 64.
func isHexKey(s string) bool {
	if n := len(s); n < 8 || n > 64 || n%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PayloadString returns a top-level string field of the payload, or "".
// TargetType returns the TYPE of the ban target a ban event records under
// "target", or "" when there is none. Only the type is ever read — the value
// of an IP ban is an address, which must not reach a delivery — and the
// vocabulary is closed like EnforcementState's: anything that is not one of
// moderation's target types reads as "".
func (e Event) TargetType() moderation.TargetType {
	if len(e.Payload) == 0 {
		return ""
	}
	var m struct {
		Target struct {
			Type string `json:"type"`
		} `json:"target"`
	}
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		return ""
	}
	switch t := moderation.TargetType(m.Target.Type); t {
	case moderation.TargetBroadcastID, moderation.TargetIP:
		return t
	default:
		return ""
	}
}

func (e Event) PayloadString(key string) string {
	if len(e.Payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// Webhook is one UI-created webhook. Chart-defined webhooks are NOT rows here
// (config.StaticWebhook is their home) — that split is what keeps their
// signing secrets in Kubernetes Secrets instead of in the database.
type Webhook struct {
	ID   uuid.UUID
	Name string
	URL  string
	// Secret is the HMAC signing key. ListWebhooks leaves it empty on purpose:
	// only GetWebhookByName — the dispatcher's accessor — ever loads it, so a
	// handler cannot render a secret it was never given. The json:"-" tag is
	// the second layer: even a Webhook that DOES carry a secret cannot be
	// marshalled into a response by accident (§4.7 — "secrets are never
	// returned, for either source").
	Secret    string `json:"-"`
	Enabled   bool
	CreatedAt time.Time
	CreatedBy string
}

// DeliveryState is a webhook delivery's lifecycle.
type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryFailed    DeliveryState = "failed"
)

// Delivery is one (event, webhook) delivery attempt-set.
type Delivery struct {
	ID          int64
	EventID     int64
	WebhookName string
	State       DeliveryState
	// Attempts is incremented when the row is CLAIMED, so it counts attempts
	// made rather than attempts that reported back. A dispatcher that dies
	// mid-send has still spent an attempt.
	Attempts      int
	NextAttemptAt *time.Time
	LastError     string
	DeliveredAt   *time.Time
}

// Store is the Postgres handle. Safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	// MinVersion is the schema version this process requires. Open sets it to
	// MinSchemaVersion; it is exported as a test seam so the "refuses to serve
	// on a too-old schema" path is exercisable while only one migration
	// exists in the tree. Production never assigns it.
	MinVersion uint
	// Now is the clock. Exported for tests; nil means time.Now.
	Now func() time.Time
}

// Open connects to Postgres. It does not — and must not — touch the schema:
// applying DDL is the migrate step's job alone (§4.15).
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	return &Store{pool: pool, MinVersion: MinSchemaVersion}, nil
}

// NewWithPool wraps an existing pool. Used by tests and by any caller that
// owns pool lifecycle itself.
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, MinVersion: MinSchemaVersion}
}

// Close releases the pool.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pool for callers that need raw SQL (tests, and
// any future read model). Handlers should use the typed methods.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping reports whether Postgres answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Ready is the readiness check behind GET /readyz: Postgres reachable AND the
// schema at least this build's minimum (§4.6). A newer schema inside the
// compatibility window passes by construction — that is what expand-contract
// buys (§4.15).
func (s *Store) Ready(ctx context.Context) error {
	if err := s.Ping(ctx); err != nil {
		return err
	}
	version, dirty, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%w: version %d", ErrSchemaDirty, version)
	}
	if version < s.MinVersion {
		return fmt.Errorf("%w: schema is at version %d, this build requires at least %d — run `gawk-admin migrate`",
			ErrSchemaTooOld, version, s.MinVersion)
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// isUniqueViolation reports whether err is a Postgres unique-index violation on
// the named constraint (empty name matches any).
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// noRows maps pgx's sentinel onto the package's.
func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
