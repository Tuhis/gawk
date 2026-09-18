package eventbus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/events"
)

// The event → row mapping (docs/51 D5) without a database in the way. It is
// the part of this milestone with judgement in it: which types become rows,
// which category each row is, and which of the contract's properties the row
// carries.
//
// The property names come from R51's schemas, and reading the wrong one fails
// silently — a row that names nothing — so these tests spell the payloads the
// way the contract does.

func busEvent(t *testing.T, typ, subject string, data any) Event {
	t.Helper()
	at := time.Date(2026, 9, 18, 9, 30, 0, 0, time.UTC)
	body, err := events.Marshal(events.New(typ, "pod-a:1051", events.SourceRelay("pod-a"), subject, at, data))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func payloadOf(t *testing.T, row store.Event) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(row.Payload, &out); err != nil {
		t.Fatalf("row payload is not an object: %v", err)
	}
	return out
}

// TestActivityRowNamesTheRoom: a room event's row must carry the room's code
// and key under the keys the portal renders rooms by. This is where reading
// "code" instead of the contract's "roomCode" would show up.
func TestActivityRowNamesTheRoom(t *testing.T) {
	ev := busEvent(t, events.TypeRoomParticipantJoined, "aa11bb22cc33",
		events.RoomParticipantJoinedData{
			RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", ParticipantID: 7,
			Nickname: "tuhis", ClientKind: events.ClientKindWebBroadcaster,
		})

	row, stored := activityRow(ev)
	if !stored {
		t.Fatal("a participant join is a stored activity row")
	}
	if row.Type != store.EventRoomParticipantJoined || row.Category != store.CategoryActivity {
		t.Errorf("row = %+v", row)
	}
	if row.Actor != "system" {
		t.Errorf("actor = %q, want system: nobody in the portal did this", row.Actor)
	}
	if !row.OccurredAt.Equal(ev.Time) {
		t.Errorf("occurredAt = %v, want the event's own time %v", row.OccurredAt, ev.Time)
	}
	p := payloadOf(t, row)
	if p[store.PayloadRoom] != "pf4tzn" {
		t.Errorf("payload %q = %v, want the raw room code", store.PayloadRoom, p[store.PayloadRoom])
	}
	if p[store.PayloadRoomKey] != "aa11bb22cc33" {
		t.Errorf("payload %q = %v", store.PayloadRoomKey, p[store.PayloadRoomKey])
	}
	summary, _ := p[store.PayloadSummary].(string)
	if !strings.Contains(summary, "tuhis") {
		t.Errorf("summary = %q, want the nickname in it", summary)
	}
	// The whole CloudEvent is kept verbatim: the row IS the record of what
	// arrived, and the webhook projection is a separate step (docs/52 D4).
	if _, ok := p["event"]; !ok {
		t.Error("the payload does not carry the event verbatim")
	}
}

// TestActivityRowNamesTheBroadcast: the broadcast half of the same rule.
func TestActivityRowNamesTheBroadcast(t *testing.T) {
	ev := busEvent(t, events.TypeBroadcastStarted, "3f9a1c4e7b2d",
		events.BroadcastStartedData{
			BroadcastID: "K7M2Q9", BroadcastKey: "3f9a1c4e7b2d",
			Role: events.RoleOrigin, StartedAt: "2026-09-18T09:30:00Z",
		})

	row, stored := activityRow(ev)
	if !stored {
		t.Fatal("a broadcast start is a stored activity row")
	}
	if row.BroadcastID != "K7M2Q9" {
		t.Errorf("broadcastId = %q, want the raw ID from the event", row.BroadcastID)
	}
	if row.BroadcastKey != "3f9a1c4e7b2d" {
		t.Errorf("broadcastKey = %q, want the event's subject", row.BroadcastKey)
	}
}

// TestDeltasAndUnknownTypesAreNotRows: the two coalesced types update the live
// view, and a type from a newer relay is unknown rather than an error. Neither
// becomes a row.
func TestDeltasAndUnknownTypesAreNotRows(t *testing.T) {
	for _, typ := range []string{
		events.TypeBroadcastViewers,
		events.TypeRoomAttachmentUpdated,
		"fi.ioio.gawk.room.something_newer",
	} {
		ev := busEvent(t, typ, "aa11bb22cc33", map[string]any{"roomKey": "aa11bb22cc33"})
		if _, stored := activityRow(ev); stored {
			t.Errorf("%s became a row", typ)
		}
	}
}

// TestRoomClosedBecomesTheSweepsRow is docs/51 D5's mapping: the relay's
// room.closed is recorded as the MODERATION room.ended the reconciler's sweep
// used to write, with the relay's reason attached — which is what lets the
// sweep be retired without a webhook receiver seeing any change.
func TestRoomClosedBecomesTheSweepsRow(t *testing.T) {
	ev := busEvent(t, events.TypeRoomClosed, "aa11bb22cc33",
		events.RoomClosedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", Reason: events.RoomClosedGrace})

	row := roomClosedRow(ev)
	if row.Type != store.EventRoomEnded {
		t.Errorf("type = %q, want the store's own room.ended", row.Type)
	}
	if row.Category != store.CategoryModeration {
		t.Errorf("category = %q: this row is the audit trail's, not activity's", row.Category)
	}
	if row.Actor != "system" {
		t.Errorf("actor = %q, want system", row.Actor)
	}
	p := payloadOf(t, row)
	if p[store.PayloadRoom] != "pf4tzn" || p[store.PayloadRoomKey] != "aa11bb22cc33" {
		t.Errorf("payload = %v", p)
	}
	if p[store.PayloadReason] != events.RoomClosedGrace {
		t.Errorf("reason = %v, want the relay's own", p[store.PayloadReason])
	}
	if summary, _ := p[store.PayloadSummary].(string); summary == "" {
		t.Error("no summary: every receiver is promised one sentence")
	}
	// It must never name the raw code in the sentence a webhook may forward.
	if summary, _ := p[store.PayloadSummary].(string); strings.Contains(summary, "pf4tzn") {
		t.Errorf("summary %q names the joinable room code", summary)
	}
}

// TestEveryStoredTypeSummarises: a row whose summary is its own type string is
// a row nobody can read in a notification.
func TestEveryStoredTypeSummarises(t *testing.T) {
	for _, rowType := range store.ActivityEventTypes() {
		summary := store.SummarizeActivity(rowType, "aa11bb22cc33", map[string]any{
			"nickname": "tuhis", "kind": events.RoomKindDynamic, "reason": events.BroadcastEndedGC,
		})
		if summary == rowType {
			t.Errorf("%s has no summary sentence of its own", rowType)
		}
		if strings.Contains(summary, "%!") {
			t.Errorf("%s summary is malformed: %q", rowType, summary)
		}
	}
}

// fakeRows stands in for the store: the dedup rule and the wake-the-dispatcher
// rule are the parts worth testing, and neither is about Postgres.
type fakeRows struct {
	appended  []store.Event
	sources   []string
	inserted  bool
	roomEnded bool
	since     time.Time
	err       error
}

func (f *fakeRows) AppendBusEvent(_ context.Context, e store.Event, source string, _ []string) (bool, error) {
	f.appended = append(f.appended, e)
	f.sources = append(f.sources, source)
	return f.inserted, f.err
}

func (f *fakeRows) RoomEndedSince(_ context.Context, _ string, since time.Time) (bool, error) {
	f.since = since
	return f.roomEnded, f.err
}

// TestIngestDedupsAgainstTheOperatorsOwnRow is docs/51 D5's other half: the
// portal records room.ended inline when an operator ends a room, and the relay
// then publishes room.closed for the same end. Recording both would page every
// webhook twice for one event.
func TestIngestDedupsAgainstTheOperatorsOwnRow(t *testing.T) {
	ev := busEvent(t, events.TypeRoomClosed, "aa11bb22cc33",
		events.RoomClosedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", Reason: events.RoomClosedOperator})

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rows := &fakeRows{roomEnded: true}
	ing := &StoreIngester{Store: rows, RoomDedupWindow: 5 * time.Minute, Now: func() time.Time { return now }}

	inserted, err := ing.Ingest(t.Context(), ev)
	if err != nil || inserted {
		t.Fatalf("inserted=%v err=%v, want no second row", inserted, err)
	}
	if len(rows.appended) != 0 {
		t.Errorf("wrote %d rows for an end the portal already recorded", len(rows.appended))
	}
	if want := now.Add(-5 * time.Minute); !rows.since.Equal(want) {
		t.Errorf("dedup window looked back to %v, want %v", rows.since, want)
	}

	// With no such row, the same event IS recorded.
	rows.roomEnded, rows.inserted = false, true
	if inserted, err = ing.Ingest(t.Context(), ev); err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v, want the row", inserted, err)
	}
	if len(rows.appended) != 1 || rows.appended[0].Type != store.EventRoomEnded {
		t.Fatalf("appended %+v", rows.appended)
	}
	if rows.sources[0] != ev.ID {
		t.Errorf("source = %q, want the CloudEvents id — the dedup key", rows.sources[0])
	}
}

// TestIngestWakesTheDispatcherOnlyOnAWrite: a duplicate must not kick the
// webhook loop, and a real row must.
func TestIngestWakesTheDispatcherOnlyOnAWrite(t *testing.T) {
	ev := busEvent(t, events.TypeRoomParticipantJoined, "aa11bb22cc33",
		events.RoomParticipantJoinedData{RoomCode: "pf4tzn", RoomKey: "aa11bb22cc33", ParticipantID: 7})

	woken := 0
	rows := &fakeRows{inserted: false}
	ing := &StoreIngester{Store: rows, Notify: func() { woken++ }}
	if _, err := ing.Ingest(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if woken != 0 {
		t.Errorf("a duplicate woke the dispatcher %d times", woken)
	}

	rows.inserted = true
	if _, err := ing.Ingest(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if woken != 1 {
		t.Errorf("a written row woke the dispatcher %d times, want 1", woken)
	}
}

// TestIngestSkipsWhatItDoesNotStore: a delta reaches the store not at all.
func TestIngestSkipsWhatItDoesNotStore(t *testing.T) {
	ev := busEvent(t, events.TypeBroadcastViewers, "3f9a1c4e7b2d",
		events.BroadcastViewersData{BroadcastKey: "3f9a1c4e7b2d", Role: events.RoleOrigin, ViewersLocal: 42})
	rows := &fakeRows{inserted: true}
	ing := &StoreIngester{Store: rows}
	inserted, err := ing.Ingest(t.Context(), ev)
	if err != nil || inserted {
		t.Fatalf("inserted=%v err=%v, want a delta to be stored nowhere", inserted, err)
	}
	if len(rows.appended) != 0 {
		t.Errorf("a delta reached the store: %+v", rows.appended)
	}
}
