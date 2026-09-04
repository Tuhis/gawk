package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

func TestAppendAndPageEvents(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	var ids []int64
	for i := range 5 {
		e, err := s.AppendEvent(ctx, store.Event{
			Type:         store.EventBanCreated,
			Actor:        "op@example.com",
			BroadcastKey: "3f9a1c2b4d5e",
			BroadcastID:  "ABC123",
			OccurredAt:   time.Now().Add(time.Duration(i) * time.Second),
			Payload:      json.RawMessage(`{"reason":"spam"}`),
		})
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		ids = append(ids, e.ID)
	}

	// Newest first.
	page, err := s.ListEvents(ctx, 0, 2)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page) != 2 || page[0].ID != ids[4] || page[1].ID != ids[3] {
		t.Fatalf("first page = %v, want newest-first %v", eventIDs(page), []int64{ids[4], ids[3]})
	}
	if got := page[0].PayloadString(store.PayloadReason); got != "spam" {
		t.Fatalf("payload reason = %q", got)
	}

	// The cursor is the last ID of the previous page: strictly older rows.
	page2, err := s.ListEvents(ctx, page[1].ID, 2)
	if err != nil {
		t.Fatalf("ListEvents(page2): %v", err)
	}
	if len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("second page = %v", eventIDs(page2))
	}
	page3, err := s.ListEvents(ctx, page2[1].ID, 2)
	if err != nil {
		t.Fatalf("ListEvents(page3): %v", err)
	}
	if len(page3) != 1 || page3[0].ID != ids[0] {
		t.Fatalf("third page = %v", eventIDs(page3))
	}
	last, err := s.ListEvents(ctx, page3[0].ID, 2)
	if err != nil || len(last) != 0 {
		t.Fatalf("page past the oldest event = %v (err=%v)", eventIDs(last), err)
	}
}

func TestAppendEventDefaultsPayload(t *testing.T) {
	s := storetest.New(t)
	e, err := s.AppendEvent(t.Context(), store.Event{Type: store.EventBanExpired, Actor: "system"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if string(e.Payload) != "{}" {
		t.Fatalf("payload = %q, want an empty object", e.Payload)
	}
	if e.OccurredAt.IsZero() {
		t.Fatalf("occurredAt was not defaulted")
	}
	if e.BroadcastKey != "" || e.BroadcastID != "" {
		t.Fatalf("blank broadcast fields round-tripped as %q/%q", e.BroadcastKey, e.BroadcastID)
	}
}

func eventIDs(es []store.Event) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// --- Summarize --------------------------------------------------------

// The sentence must be graded on enforcement, because it is the one a dumb
// webhook-to-push bridge shows on a phone with no templating (docs/42 §4.10).
// "a broadcast was terminated" while nothing has been terminated is not a
// statement of something that happened — it is the lie an operator reads while
// the relay is still carrying the broadcast.
//
// The copy is pinned in full rather than matched loosely: it is operator-facing
// text, and both the direction ("STILL banned" on a lift) and the verb ("was
// recorded", not "was terminated") are the load-bearing parts.
func TestSummarizeGradesTheSentenceOnEnforcement(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		target    moderation.TargetType
		key       string
		actor     string
		inSync    string
		pending   string
	}{
		{
			name:      "kill of a live broadcast",
			eventType: store.EventBroadcastKilled,
			target:    moderation.TargetBroadcastID,
			key:       "3f9a1c2b4d5e",
			actor:     "op@example.com",
			inSync:    "broadcast 3f9a1c2b4d5e was terminated by op@example.com",
			pending:   "a kill of broadcast 3f9a1c2b4d5e was recorded by op@example.com — NOT enforced yet, the broadcast is still live",
		},
		{
			// No key: the broadcast had already ended, so the event names no
			// handle at all rather than falling back to the raw ID (D8).
			name:      "kill with no broadcast key",
			eventType: store.EventBroadcastKilled,
			target:    moderation.TargetBroadcastID,
			actor:     "op@example.com",
			inSync:    "a broadcast was terminated by op@example.com",
			pending:   "a kill of a broadcast was recorded by op@example.com — NOT enforced yet, the broadcast is still live",
		},
		{
			name:      "kill with no actor",
			eventType: store.EventBroadcastKilled,
			target:    moderation.TargetBroadcastID,
			key:       "3f9a1c2b4d5e",
			inSync:    "broadcast 3f9a1c2b4d5e was terminated by an operator",
			pending:   "a kill of broadcast 3f9a1c2b4d5e was recorded by an operator — NOT enforced yet, the broadcast is still live",
		},
		{
			name:      "broadcast ban created",
			eventType: store.EventBanCreated,
			target:    moderation.TargetBroadcastID,
			actor:     "op@example.com",
			inSync:    "a broadcast ban was created by op@example.com",
			pending:   "a broadcast ban was recorded by op@example.com — NOT enforced yet",
		},
		{
			name:      "IP ban created",
			eventType: store.EventBanCreated,
			target:    moderation.TargetIP,
			actor:     "op@example.com",
			inSync:    "a publisher IP ban was created by op@example.com",
			pending:   "a publisher IP ban was recorded by op@example.com — NOT enforced yet",
		},
		{
			// The direction an operator is most likely to misread: the record
			// says lifted while the target is still banned.
			name:      "ban removed",
			eventType: store.EventBanRemoved,
			target:    moderation.TargetBroadcastID,
			actor:     "op@example.com",
			inSync:    "a broadcast ban was lifted by op@example.com",
			pending:   "a broadcast ban was lifted in the record by op@example.com — the target is STILL banned",
		},
		{
			name:      "IP ban expired",
			eventType: store.EventBanExpired,
			target:    moderation.TargetIP,
			inSync:    "a publisher IP ban expired",
			pending:   "a publisher IP ban expired in the record — the target is STILL banned",
		},
		{
			// R40's reserved type falls to the default arm, where the grade is
			// DIRECTION-FREE: with no idea whether the event asserts a ban or
			// its lifting, "NOT enforced yet" could be the backwards half.
			name:      "an ungraded event type",
			eventType: store.EventContentFlag,
			target:    moderation.TargetBroadcastID,
			inSync:    "content_flag.raised",
			pending:   "content_flag.raised — enforcement pending",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.SummarizeWithEnforcement(tc.eventType, tc.target, tc.key, tc.actor, store.EnforcementInSync)
			if got != tc.inSync {
				t.Errorf("in sync = %q, want %q", got, tc.inSync)
			}
			// The four-argument form is the same sentence: one summariser, so
			// the two grades cannot drift apart.
			if plain := store.Summarize(tc.eventType, tc.target, tc.key, tc.actor); plain != tc.inSync {
				t.Errorf("Summarize = %q, want the in-sync sentence %q", plain, tc.inSync)
			}
			pending := store.SummarizeWithEnforcement(tc.eventType, tc.target, tc.key, tc.actor, store.EnforcementPending)
			if pending != tc.pending {
				t.Errorf("pending = %q, want %q", pending, tc.pending)
			}
			if pending == got {
				t.Errorf("the pending sentence is identical to the in-sync one (%q): a pending event would read as a completed one", got)
			}
		})
	}
}

// The payload key's vocabulary is CLOSED on the read side. That is what makes
// it forwardable to a webhook without a per-value review: whatever a producer
// wrote, this accessor answers with one of two constants, so free text — or a
// raw broadcast ID — cannot ride the field out of the deployment (D8).
func TestEventEnforcementStateIsAClosedVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    store.EnforcementState
	}{
		{name: "no payload at all", payload: "", want: store.EnforcementInSync},
		{name: "no enforcement key", payload: `{"reason":"spam"}`, want: store.EnforcementInSync},
		{name: "pending", payload: `{"enforcement":"pending"}`, want: store.EnforcementPending},
		{name: "the wrong case", payload: `{"enforcement":"PENDING"}`, want: store.EnforcementInSync},
		{name: "free text", payload: `{"enforcement":"pending: could not reach ABC234"}`, want: store.EnforcementInSync},
		{name: "a raw broadcast ID", payload: `{"enforcement":"ZXQ7K2"}`, want: store.EnforcementInSync},
		{name: "not a string", payload: `{"enforcement":{"inSync":false}}`, want: store.EnforcementInSync},
		{name: "not even JSON", payload: `nonsense`, want: store.EnforcementInSync},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := store.Event{Type: store.EventBanCreated}
			if tc.payload != "" {
				ev.Payload = json.RawMessage(tc.payload)
			}
			if got := ev.EnforcementState(); got != tc.want {
				t.Fatalf("EnforcementState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The room sentence names the KIND and nothing else (R42, docs/44 D16): a
// room code is a joinable secret like a broadcast ID, so the one string that
// is copied verbatim into every webhook must never be able to carry it.
func TestSummarizeRoomNamesTheKindOnly(t *testing.T) {
	cases := []struct {
		eventType, kind, actor, want string
	}{
		{store.EventRoomCreated, "static", "op@example.com", "a static room was created by op@example.com"},
		{store.EventRoomCreated, "", "", "a room was created by an operator"},
		{store.EventRoomSecretRotated, "static", "op@example.com", "the attach secret of a static room was rotated by op@example.com"},
		{store.EventRoomEnded, "dynamic", "op@example.com", "a dynamic room was ended by op@example.com"},
		{store.EventRoomEnded, "static", "op@example.com", "a static room was deleted by op@example.com"},
		{store.EventRoomEnded, "dynamic", "system", "a dynamic room ended"},
		{store.EventRoomEnded, "dynamic", "", "a dynamic room ended"},
	}
	for _, tc := range cases {
		if got := store.SummarizeRoom(tc.eventType, tc.kind, tc.actor); got != tc.want {
			t.Errorf("SummarizeRoom(%s, %q, %q) = %q, want %q", tc.eventType, tc.kind, tc.actor, got, tc.want)
		}
	}
	// The grading entry point routes room types here too, without an
	// enforcement qualifier: a CR write has no "not enforced yet" state.
	got := store.SummarizeWithEnforcement(store.EventRoomCreated, moderation.TargetBroadcastID, "", "op@example.com", store.EnforcementPending)
	if got != "a room was created by op@example.com" {
		t.Errorf("SummarizeWithEnforcement(room.created) = %q", got)
	}
}

// Event.RoomKey is the room twin of EnforcementState's closed vocabulary: only
// a lower-case hex digest passes, so a producer that wrote the CODE under the
// webhook-safe key by mistake leaks nothing (docs/44 D16, docs/42 D8).
func TestEventRoomKeyIsAClosedVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "no payload", payload: "", want: ""},
		{name: "no roomKey", payload: `{"room":"tuhisroom"}`, want: ""},
		{name: "the relay's 12-hex key", payload: `{"roomKey":"9c1d2e3f4a5b"}`, want: "9c1d2e3f4a5b"},
		{name: "a 64-hex key", payload: `{"roomKey":"` + strings.Repeat("ab", 32) + `"}`, want: strings.Repeat("ab", 32)},
		{name: "a dynamic room code", payload: `{"roomKey":"R7K3MX"}`, want: ""},
		{name: "a static slug", payload: `{"roomKey":"tuhisroom"}`, want: ""},
		{name: "upper-case hex", payload: `{"roomKey":"9C1D2E3F4A5B"}`, want: ""},
		{name: "odd length", payload: `{"roomKey":"9c1d2e3f4a5"}`, want: ""},
		{name: "too short", payload: `{"roomKey":"9c1d2e"}`, want: ""},
		{name: "too long", payload: `{"roomKey":"` + strings.Repeat("ab", 33) + `"}`, want: ""},
		{name: "not a string", payload: `{"roomKey":12345678}`, want: ""},
		{name: "not JSON", payload: `nonsense`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := store.Event{Type: store.EventRoomEnded}
			if tc.payload != "" {
				ev.Payload = json.RawMessage(tc.payload)
			}
			if got := ev.RoomKey(); got != tc.want {
				t.Fatalf("RoomKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// RoomEndedSince is the reconciler's deduplication against the portal's own
// inline room.ended (R42): it matches the raw code under PayloadRoom, the
// room.ended type only, and the window's lower bound inclusively.
func TestRoomEndedSince(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	record := func(eventType, room string, when time.Time) {
		t.Helper()
		if _, err := s.AppendEvent(ctx, store.Event{
			Type: eventType, Actor: "op@example.com", OccurredAt: when,
			Payload: json.RawMessage(`{"room":"` + room + `","kind":"dynamic"}`),
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	record(store.EventRoomEnded, "r7k3mx", at)
	record(store.EventRoomCreated, "tuhisroom", at)

	cases := []struct {
		name  string
		room  string
		since time.Time
		want  bool
	}{
		{"recorded at the bound", "r7k3mx", at, true},
		{"recorded inside the window", "r7k3mx", at.Add(-time.Hour), true},
		{"recorded before the window", "r7k3mx", at.Add(time.Second), false},
		{"another room", "abc234", at.Add(-time.Hour), false},
		{"a different event type for the room", "tuhisroom", at.Add(-time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.RoomEndedSince(ctx, tc.room, tc.since)
			if err != nil {
				t.Fatalf("RoomEndedSince: %v", err)
			}
			if got != tc.want {
				t.Fatalf("RoomEndedSince(%q, %v) = %v, want %v", tc.room, tc.since, got, tc.want)
			}
		})
	}
}
