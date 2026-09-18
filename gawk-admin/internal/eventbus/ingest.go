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
	Store *store.Store
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

// Ingest records one bus event. It returns inserted=false for a duplicate,
// which is the ordinary outcome of the bus's at-least-once delivery and not an
// error — the consumer acks either way.
func (s *StoreIngester) Ingest(ctx context.Context, ev Event) (bool, error) {
	str := func(k string) string {
		v, _ := ev.Data[k].(string)
		return v
	}

	if ev.Type == events.TypeRoomClosed {
		return s.ingestRoomClosed(ctx, ev, str("code"), str("kind"))
	}

	rowType, stored := store.RowTypeForBusType(ev.Type)
	if !stored {
		// An unknown or delta type. Acked and forgotten: the contract says to
		// treat an unknown type as unknown, and a newer relay may publish one.
		return false, nil
	}

	row := store.Event{
		Type:       rowType,
		Category:   store.CategoryActivity,
		OccurredAt: ev.Time,
		// Nobody in the portal did this. "system" is what the sweep already
		// writes for a relay-originated fact, so the feed reads consistently.
		Actor:        "system",
		BroadcastKey: ev.Subject,
		BroadcastID:  str("id"),
		Payload:      s.payload(ev, rowType),
	}
	return s.append(ctx, row, ev.ID)
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
func (s *StoreIngester) ingestRoomClosed(ctx context.Context, ev Event, code, kind string) (bool, error) {
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
	payload := map[string]any{
		store.PayloadRoomKey: ev.Subject,
		store.PayloadRoom:    code,
		store.PayloadSummary: store.SummarizeRoom(store.EventRoomEnded, kind, "system"),
	}
	if kind != "" {
		payload[store.PayloadRoomKind] = kind
	}
	if reason, ok := ev.Data["reason"].(string); ok && reason != "" {
		payload[store.PayloadReason] = reason
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("eventbus: room.closed payload: %w", err)
	}
	// A moderation row, not an activity one: it IS the row a webhook receiver
	// has been getting since R42, byte for byte, and it stays in the audit
	// trail rather than being pruned.
	return s.append(ctx, store.Event{
		Type:       store.EventRoomEnded,
		Category:   store.CategoryModeration,
		OccurredAt: ev.Time,
		Actor:      "system",
		Payload:    body,
	}, ev.ID)
}

// payload is the row's jsonb: the whole CloudEvent verbatim plus the one human
// sentence and, for a room event, the keys the portal renders rooms by.
//
// Verbatim because the row is the record of what arrived — raw identifiers
// included, which is allowed in this portal-only column and is why the webhook
// projection is a separate step (docs/52 D4).
func (s *StoreIngester) payload(ev Event, rowType string) json.RawMessage {
	out := map[string]any{
		"event":              json.RawMessage(ev.Raw),
		store.PayloadSummary: store.SummarizeActivity(rowType, ev.Subject, ev.Data),
	}
	if code, ok := ev.Data["code"].(string); ok && code != "" {
		out[store.PayloadRoom] = code
		out[store.PayloadRoomKey] = ev.Subject
	}
	if kind, ok := ev.Data["kind"].(string); ok && kind != "" {
		out[store.PayloadRoomKind] = kind
	}
	if reason, ok := ev.Data["reason"].(string); ok && reason != "" {
		out[store.PayloadReason] = reason
	}
	body, err := json.Marshal(out)
	if err != nil {
		// Unreachable: every value here is a string or raw JSON that already
		// parsed. An empty payload beats losing the row.
		return json.RawMessage(`{}`)
	}
	return body
}
