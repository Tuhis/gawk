package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
)

// StoreIngester writes bus events into moderation_events (docs/51 D5).
//
// Two bus types are MAPPED rather than ingested as themselves, because the
// portal already has a row type that means the same thing to an operator:
//
//   - room.closed becomes exactly the row the reconciler's sweep writes today
//     — a moderation room.ended with actor "system" — deduplicated against an
//     operator-recorded end the same way the sweep is. That is what lets the
//     sweep be retired without a webhook receiver noticing anything (R49 D8).
//   - room.opened becomes an activity row of that type. Nothing records a
//     dynamic room's birth today; a static room's room.created stays the
//     operator's moderation row, so an operator-created room has one row of
//     each, of different types, and needs no dedup.
//
// Everything else is an activity row of its own short type.
type StoreIngester struct {
	// Store is the narrow slice of the store this needs. *store.Store
	// satisfies it; an interface because the mapping and the dedup rule are
	// the parts worth testing, and neither is about Postgres.
	Store Rows
	// RoomDedupWindow is how far back the room.closed mapping looks for an
	// operator-recorded room.ended before writing a "system" one. It mirrors
	// the reconciler's own window.
	RoomDedupWindow time.Duration
	// ConfigWebhooks are the enabled chart-defined webhook names, which are
	// not rows. Passed through to the fan-out so the mapped room.ended keeps
	// reaching a chart-configured receiver exactly as the sweep's did.
	ConfigWebhooks []string
	// Notify wakes the webhook dispatcher after a row that fans out. Without
	// it the delivery still goes, on the dispatcher's next poll; with it a
	// receiver hears about a room ending as promptly as it did when the
	// portal recorded that row itself.
	Notify func()
	Now    func() time.Time
}

func (s *StoreIngester) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// The `data` property names are R51's (docs/52 D2), not invented here: a room
// event names the room `roomCode`/`roomKey`, a broadcast event names it
// `broadcastId`/`broadcastKey`. Reading the wrong key silently yields "" and a
// row that names nothing, which is why activityRow is tested against the
// contract's own vectors.
const (
	keyRoomCode    = "roomCode"
	keyBroadcastID = "broadcastId"
	keyKind        = "kind"
	keyReason      = "reason"
	keyNickname    = "nickname"
)

// str reads a string property, or "" when it is absent or not a string.
func (e Event) str(key string) string {
	v, _ := e.Data[key].(string)
	return v
}

// Rows is what ingesting needs from the store, and nothing more.
type Rows interface {
	// AppendBusEvent inserts the row unless its source has been seen before.
	AppendBusEvent(ctx context.Context, e store.Event, source string, configNames []string) (bool, error)
	// RoomEndedSince reports whether a room.ended naming this room was
	// recorded at or after `since` — the portal's own record of an operator
	// ending it, which the bus must not duplicate.
	RoomEndedSince(ctx context.Context, room string, since time.Time) (bool, error)
}

// Ingest records one bus event. It returns inserted=false for a duplicate,
// which is the ordinary outcome of the bus's at-least-once delivery and not an
// error — the consumer acks either way.
func (s *StoreIngester) Ingest(ctx context.Context, ev Event) (bool, error) {
	if ev.Type == events.TypeRoomClosed {
		return s.ingestRoomClosed(ctx, ev)
	}
	row, stored := activityRow(ev)
	if !stored {
		// An unknown or delta type. Acked and forgotten: the contract says to
		// treat an unknown type as unknown, and a newer relay may publish one.
		return false, nil
	}
	return s.append(ctx, row, ev.ID)
}

// activityRow is the whole event → row mapping, with no database in it, so the
// thing this milestone is actually about can be tested against the contract's
// own vectors rather than only through Postgres.
func activityRow(ev Event) (store.Event, bool) {
	rowType, stored := store.RowTypeForBusType(ev.Type)
	if !stored {
		return store.Event{}, false
	}
	return store.Event{
		Type:       rowType,
		Category:   store.CategoryActivity,
		OccurredAt: ev.Time,
		// Nobody in the portal did this. "system" is what the sweep already
		// writes for a relay-originated fact, so the feed reads consistently.
		Actor:        "system",
		BroadcastKey: ev.Subject,
		BroadcastID:  ev.str(keyBroadcastID),
		Payload:      activityPayload(ev, rowType),
	}, true
}

// append is the one write path, so nothing can record a row and forget to wake
// the dispatcher.
func (s *StoreIngester) append(ctx context.Context, row store.Event, source string) (bool, error) {
	inserted, err := s.Store.AppendBusEvent(ctx, row, source, s.ConfigWebhooks)
	if inserted && s.Notify != nil {
		s.Notify()
	}
	return inserted, err
}

// ingestRoomClosed writes the moderation row the reconciler's sweep used to
// write, with the relay's own reason attached — and writes nothing when the
// portal already recorded the operator who ended the room.
func (s *StoreIngester) ingestRoomClosed(ctx context.Context, ev Event) (bool, error) {
	code := ev.str(keyRoomCode)
	window := s.RoomDedupWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	if code != "" {
		found, err := s.Store.RoomEndedSince(ctx, code, s.now().Add(-window))
		if err != nil {
			return false, fmt.Errorf("eventbus: room ended dedup: %w", err)
		}
		if found {
			// The API recorded the operator's own room.ended. Recording a
			// second, "system" one would page every webhook twice for one end.
			return false, nil
		}
	}
	return s.append(ctx, roomClosedRow(ev), ev.ID)
}

// roomClosedRow is the row the reconciler's sweep used to write: a MODERATION
// room.ended with actor "system", not an activity row. It IS the row a webhook
// receiver has been getting since R42, and it stays in the audit trail rather
// than being pruned — which is what lets the sweep be retired without any
// receiver noticing (docs/51 D5).
func roomClosedRow(ev Event) store.Event {
	kind := ev.str(keyKind)
	payload := map[string]any{
		store.PayloadRoomKey: ev.Subject,
		store.PayloadRoom:    ev.str(keyRoomCode),
		store.PayloadSummary: store.SummarizeRoom(store.EventRoomEnded, kind, "system"),
	}
	if kind != "" {
		payload[store.PayloadRoomKind] = kind
	}
	if reason := ev.str(keyReason); reason != "" {
		payload[store.PayloadReason] = reason
	}
	return store.Event{
		Type:       store.EventRoomEnded,
		Category:   store.CategoryModeration,
		OccurredAt: ev.Time,
		Actor:      "system",
		Payload:    mustJSON(payload),
	}
}

// payload is the row's jsonb: the whole CloudEvent verbatim plus the one human
// sentence and, for a room event, the keys the portal renders rooms by.
//
// Verbatim because the row is the record of what arrived — raw identifiers
// included, which is allowed in this portal-only column and is why the webhook
// projection is a separate step (docs/52 D4).
func activityPayload(ev Event, rowType string) json.RawMessage {
	out := map[string]any{
		"event":              json.RawMessage(ev.Raw),
		store.PayloadSummary: store.SummarizeActivity(rowType, ev.Subject, ev.Data),
	}
	if code := ev.str(keyRoomCode); code != "" {
		out[store.PayloadRoom] = code
		out[store.PayloadRoomKey] = ev.Subject
	}
	if kind := ev.str(keyKind); kind != "" {
		out[store.PayloadRoomKind] = kind
	}
	if reason := ev.str(keyReason); reason != "" {
		out[store.PayloadReason] = reason
	}
	return mustJSON(out)
}

// mustJSON encodes a payload map. Every value in one is a string or raw JSON
// that already parsed, so the error path is unreachable — and an empty payload
// beats losing the row if it ever is reached.
func mustJSON(v map[string]any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return body
}
