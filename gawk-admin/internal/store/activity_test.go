package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

// TestAppendBusEventIsExactlyOnce is what turns the bus's at-least-once
// delivery into exactly-once storage: the second copy of a message collides on
// `source` and writes nothing, and that is not an error — the consumer acks
// either way (docs/51 D5).
func TestAppendBusEventIsExactlyOnce(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	row := store.Event{
		Type:         store.EventRoomParticipantJoined,
		Category:     store.CategoryActivity,
		Actor:        "system",
		BroadcastKey: "aa11bb22cc33",
		OccurredAt:   time.Now(),
		Payload:      json.RawMessage(`{"summary":"tuhis joined room aa11bb22cc33"}`),
	}
	inserted, err := s.AppendBusEvent(ctx, row, "pod-a:1051", nil)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	inserted, err = s.AppendBusEvent(ctx, row, "pod-a:1051", nil)
	if err != nil {
		t.Fatalf("redelivery returned an error: %v", err)
	}
	if inserted {
		t.Error("a redelivered message wrote a second row")
	}

	got, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryActivity})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("activity rows = %d, want 1", len(got))
	}
	if got[0].Source != "pod-a:1051" || got[0].Category != store.CategoryActivity {
		t.Errorf("row = %+v", got[0])
	}
}

// TestCategoryFilterSeparatesTheTrailFromTheFirehose: an operator reading the
// audit trail must not have to scroll past a thousand joins.
func TestCategoryFilterSeparatesTheTrailFromTheFirehose(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	if _, err := s.AppendEvent(ctx, store.Event{
		Type: store.EventBanCreated, Actor: "op@example.com", BroadcastKey: "3f9a1c2b4d5e",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventBroadcastStarted, Actor: "system", BroadcastKey: "3f9a1c2b4d5e",
	}, "pod-a:1", nil); err != nil {
		t.Fatal(err)
	}

	mod, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryModeration})
	if err != nil {
		t.Fatal(err)
	}
	if len(mod) != 1 || mod[0].Type != store.EventBanCreated {
		t.Errorf("moderation page = %+v", mod)
	}
	act, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryActivity})
	if err != nil {
		t.Fatal(err)
	}
	if len(act) != 1 || act[0].Type != store.EventBroadcastStarted {
		t.Errorf("activity page = %+v", act)
	}
	both, err := s.ListEvents(ctx, store.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Errorf("unfiltered page = %d rows, want both", len(both))
	}
	// A row written before R50 has no category column value of its own; the
	// migration's default is what classifies it, and this asserts it.
	if mod[0].Category != store.CategoryModeration {
		t.Errorf("a portal-written row is %q, want moderation", mod[0].Category)
	}
}

// TestPruneLeavesTheAuditTrailAlone: activity ages out, moderation never does.
func TestPruneLeavesTheAuditTrailAlone(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	old := time.Now().Add(-100 * time.Hour)

	if _, err := s.AppendEvent(ctx, store.Event{
		Type: store.EventBanCreated, Actor: "op@example.com", OccurredAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantJoined, Actor: "system", OccurredAt: old,
	}, "pod-a:1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantLeft, Actor: "system", OccurredAt: time.Now(),
	}, "pod-a:2", nil); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneActivityEvents(ctx, time.Now().Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}
	mod, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryModeration})
	if err != nil {
		t.Fatal(err)
	}
	if len(mod) != 1 {
		t.Error("the prune took an audit row with it")
	}
}

// R49 review finding (PR #388): since an activity row can have deliveries —
// any webhook that lists a room activity type — the prune must take the
// expired rows' delivery rows with them. Before the fix the DELETE hit the
// deliveries' foreign key, failed as a whole, and nothing was ever pruned
// again.
func TestPruneTakesExpiredActivityDeliveriesWithIt(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()
	old := time.Now().Add(-100 * time.Hour)
	bot := []store.ConfigWebhook{{Name: "bot", Events: []string{store.EventRoomParticipantJoined}}}
	join := json.RawMessage(`{"event":{"data":{"nickname":"tuhis","rejoin":false}}}`)

	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantJoined, Actor: "system", OccurredAt: old, Payload: join,
	}, "pod-a:1", bot); err != nil {
		t.Fatal(err)
	}
	// A second expired row with no delivery, and a fresh one with one.
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantLeft, Actor: "system", OccurredAt: old,
	}, "pod-a:2", bot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantJoined, Actor: "system", OccurredAt: time.Now(), Payload: join,
	}, "pod-a:3", bot); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneActivityEvents(ctx, time.Now().Add(-72*time.Hour))
	if err != nil {
		t.Fatalf("prune with a delivered activity row: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows, want 2", n)
	}
	left, err := s.ListEvents(ctx, store.EventQuery{Category: store.CategoryActivity})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("%d activity rows left, want the fresh one", len(left))
	}
	byEvent, err := s.ListDeliveriesForEvents(ctx, []int64{left[0].ID})
	if err != nil || len(byEvent[left[0].ID]) != 1 {
		t.Fatalf("the fresh row's delivery = %v (err=%v), want it untouched", byEvent[left[0].ID], err)
	}
}

// TestActivityRowsDoNotPageAnyone: with the default filter no webhook receives
// an activity row (R49 made four types deliverable only to a webhook that
// lists them), so ingesting a join must enqueue no delivery even with a
// webhook configured.
func TestActivityRowsDoNotPageAnyone(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomParticipantJoined, Actor: "system",
	}, "pod-a:1", hooks("ops-pager")); err != nil {
		t.Fatal(err)
	}
	// The mapped room.ended, by contrast, is the row a receiver has had since
	// R42 and keeps getting.
	if _, err := s.AppendBusEvent(ctx, store.Event{
		Type: store.EventRoomEnded, Category: store.CategoryModeration, Actor: "system",
		Payload: json.RawMessage(`{"room":"pf4tzn"}`),
	}, "pod-a:2", hooks("ops-pager")); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListEvents(ctx, store.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	deliveries, err := s.ListDeliveriesForEvents(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.Type {
		case store.EventRoomEnded:
			if len(deliveries[r.ID]) != 1 {
				t.Errorf("room.ended enqueued %d deliveries, want 1", len(deliveries[r.ID]))
			}
		default:
			if len(deliveries[r.ID]) != 0 {
				t.Errorf("%s enqueued %d deliveries, want none", r.Type, len(deliveries[r.ID]))
			}
		}
	}
}
