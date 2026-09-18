package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// Activity rows (R50, docs/51 D5): what the fleet is DOING, as opposed to what
// an operator did about it.
//
// Both live in moderation_events, separated by `category`, because they are
// the same shape and the portal reads them through one feed with one cursor.
// They differ in three ways that the code, not a convention, enforces:
//
//   - An activity row carries `source`, the CloudEvents id of the bus message
//     it came from, UNIQUE. That is what turns the bus's at-least-once
//     delivery into exactly-once at the table.
//   - Activity rows are pruned by age; moderation rows are the audit trail and
//     are never pruned here.
//   - Activity types are not webhook-eligible by default. R49 is where four of
//     them become so, and it says so in WebhookEventTypes, not here.

// Event categories.
const (
	CategoryModeration = "moderation"
	CategoryActivity   = "activity"
)

// Activity event types: the bus types this deployment stores, in the short row
// form (the CloudEvents type minus its reverse-DNS prefix, docs/52 D6 (e) — a
// row type string is never equal to a CloudEvents type string).
//
// The two delta types are deliberately absent: broadcast.viewers and
// room.attachment_updated update an in-memory live view and are acked, never
// stored. A write per five seconds per live thing, for data nobody audits, is
// not an audit trail.
const (
	EventBroadcastStarted       = "broadcast.started"
	EventBroadcastPublisherAway = "broadcast.publisher_away"
	EventBroadcastPublisherBack = "broadcast.publisher_back"
	EventBroadcastEnded         = "broadcast.ended"
	EventRoomOpened             = "room.opened"
	EventRoomHomeChanged        = "room.home_changed"
	EventRoomAttached           = "room.attached"
	EventRoomDetached           = "room.detached"
	EventRoomParticipantJoined  = "room.participant_joined"
	EventRoomParticipantLeft    = "room.participant_left"
	EventRoomParticipantUpdated = "room.participant_updated"
)

// ActivityEventTypes is the closed vocabulary of stored activity rows, in
// lifecycle order.
func ActivityEventTypes() []string {
	return []string{
		EventBroadcastStarted,
		EventBroadcastPublisherAway,
		EventBroadcastPublisherBack,
		EventBroadcastEnded,
		EventRoomOpened,
		EventRoomHomeChanged,
		EventRoomAttached,
		EventRoomDetached,
		EventRoomParticipantJoined,
		EventRoomParticipantLeft,
		EventRoomParticipantUpdated,
	}
}

// RowTypeForBusType maps a CloudEvents type onto the row type an activity row
// carries, and reports whether this type is stored at all. A delta type
// returns false — it is acked and forgotten.
func RowTypeForBusType(busType string) (string, bool) {
	short := strings.TrimPrefix(busType, events.TypePrefix)
	for _, t := range ActivityEventTypes() {
		if t == short {
			return short, true
		}
	}
	return "", false
}

// AppendBusEvent inserts one row ingested from the relay event bus, ignoring a
// duplicate, and fans it out to webhooks when its type is webhook-eligible.
//
// The (inserted=false, err=nil) return is the ordinary case for a redelivery,
// not an error: the consumer acks either way, which is exactly what makes the
// bus's at-least-once delivery exactly-once storage.
//
// e.Category selects the row's kind — activity for the bus's own types, and
// moderation for the one type that is MAPPED onto a row the portal already
// wrote before R50 (room.closed becomes room.ended, docs/51 D5). The insert
// and the delivery rows commit together, the same rule AppendEventAndEnqueue
// exists for: an event that claims to be recorded must have its fan-out.
func (s *Store) AppendBusEvent(ctx context.Context, e Event, source string, configNames []string) (inserted bool, err error) {
	if source == "" {
		return false, fmt.Errorf("store: append bus event: empty source (the dedup key)")
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = s.now()
	}
	category := e.Category
	if category == "" {
		category = CategoryActivity
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: append bus event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `INSERT INTO moderation_events
		(type, occurred_at, actor, broadcast_key, broadcast_id, payload, category, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (source) WHERE source IS NOT NULL DO NOTHING
		RETURNING id`
	var id int64
	switch err := tx.QueryRow(ctx, q, e.Type, e.OccurredAt.UTC(), e.Actor,
		nullString(e.BroadcastKey), nullString(e.BroadcastID), []byte(payload),
		category, source).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		// The duplicate case: this exact bus message is already a row.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: append bus event: %w", err)
	}

	// Only webhook-eligible types fan out. The activity types are not on that
	// list today — R49 is where four of them join it — so an ingested join
	// does not page anyone, while the mapped room.ended keeps reaching every
	// receiver that has always had it.
	if isWebhookEventType(e.Type) {
		if err := s.enqueueDeliveriesTx(ctx, tx, id, configNames); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: append bus event: %w", err)
	}
	return true, nil
}

func isWebhookEventType(t string) bool {
	for _, known := range WebhookEventTypes() {
		if known == t {
			return true
		}
	}
	return false
}

// PruneActivityEvents deletes activity rows older than the cutoff and returns
// how many went. Moderation rows are never touched: the audit trail is the one
// thing in this table nobody is allowed to age out.
func (s *Store) PruneActivityEvents(ctx context.Context, olderThan time.Time) (int64, error) {
	const q = `DELETE FROM moderation_events WHERE category = 'activity' AND occurred_at < $1`
	tag, err := s.pool.Exec(ctx, q, olderThan.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: prune activity events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SummarizeActivity is the one human sentence an activity row carries, the
// activity counterpart of Summarize.
//
// Same security rule: it names the HMAC'd key and never the joinable ID or
// room code, because a summary is one of the few things that may reach a
// webhook (docs/42 D8).
func SummarizeActivity(eventType, key string, data map[string]any) string {
	what := "a broadcast"
	if key != "" {
		what = "broadcast " + key
	}
	room := "a room"
	if key != "" {
		room = "room " + key
	}
	str := func(k string) string {
		v, _ := data[k].(string)
		return v
	}
	switch eventType {
	case EventBroadcastStarted:
		return what + " started"
	case EventBroadcastPublisherAway:
		return "the broadcaster of " + what + " went away"
	case EventBroadcastPublisherBack:
		return "the broadcaster of " + what + " is back"
	case EventBroadcastEnded:
		switch str("reason") {
		case events.BroadcastEndedKilled:
			return what + " was ended by an operator"
		case events.BroadcastEndedReplaced:
			return "the publisher of " + what + " was replaced"
		default:
			return what + " ended"
		}
	case EventRoomHomeChanged:
		if prev := str("previousPod"); prev != "" {
			return room + " moved to this pod from " + prev
		}
		return room + " moved to another pod"
	case EventRoomOpened:
		kind := str("kind")
		if kind != "" {
			return "a " + kind + " " + room + " opened"
		}
		return room + " opened"
	case EventRoomAttached:
		return "a stream attached to " + room
	case EventRoomDetached:
		return "a stream left " + room
	case EventRoomParticipantJoined:
		return nickOr("someone", str("nickname")) + " joined " + room
	case EventRoomParticipantLeft:
		// Three of the four reasons are not departures at all, and a feed that
		// rendered them as one would read as an exodus every time a pod rolls.
		who := nickOr("someone", str("nickname"))
		switch str("reason") {
		case events.ParticipantLeftHomeMoved:
			return who + " is reconnecting to " + room + "'s new pod"
		case events.ParticipantLeftRoomEnded:
			return who + " was disconnected when " + room + " ended"
		case events.ParticipantLeftTimeout:
			return who + " stopped responding and was dropped from " + room
		default:
			return who + " left " + room
		}
	case EventRoomParticipantUpdated:
		return nickOr("a participant", str("nickname")) + " changed in " + room
	default:
		return eventType
	}
}

// nickOr keeps a missing nickname from rendering as an empty subject. A
// nickname is free text and not sensitive — the same posture the webhook docs
// already state for reasons and labels.
func nickOr(fallback, nick string) string {
	if nick == "" {
		return fallback
	}
	return nick
}
