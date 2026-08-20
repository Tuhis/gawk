package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
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
