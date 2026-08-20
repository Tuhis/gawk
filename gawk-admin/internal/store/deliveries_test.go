package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

func TestDeliveryQueueLifecycle(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	ev, err := s.AppendEvent(ctx, store.Event{Type: store.EventBroadcastKilled, Actor: "op"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.EnqueueDeliveries(ctx, ev.ID, []string{"ntfy", "slack"}); err != nil {
		t.Fatalf("EnqueueDeliveries: %v", err)
	}
	// Idempotent: a retried enqueue adds nothing.
	if err := s.EnqueueDeliveries(ctx, ev.ID, []string{"ntfy", "slack"}); err != nil {
		t.Fatalf("EnqueueDeliveries (retry): %v", err)
	}
	byEvent, err := s.ListDeliveriesForEvents(ctx, []int64{ev.ID})
	if err != nil {
		t.Fatalf("ListDeliveriesForEvents: %v", err)
	}
	if len(byEvent[ev.ID]) != 2 {
		t.Fatalf("a repeated enqueue produced %d deliveries, want 2", len(byEvent[ev.ID]))
	}

	now := time.Now()
	claimed, err := s.ClaimDueDeliveries(ctx, now, 10)
	if err != nil {
		t.Fatalf("ClaimDueDeliveries: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d deliveries, want 2", len(claimed))
	}
	for _, d := range claimed {
		if d.Attempts != 1 {
			t.Fatalf("attempts on claim = %d, want 1", d.Attempts)
		}
	}
	// A claimed row is not due again until its claim lease expires.
	again, err := s.ClaimDueDeliveries(ctx, now, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("re-claim before the lease expired returned %d rows (err=%v)", len(again), err)
	}

	if err := s.MarkDelivered(ctx, claimed[0].ID, now); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	retryAt := now.Add(5 * time.Second)
	if err := s.ScheduleRetry(ctx, claimed[1].ID, retryAt, "connection refused"); err != nil {
		t.Fatalf("ScheduleRetry: %v", err)
	}

	// The retry is due at its scheduled time and nowhere earlier.
	due, err := s.ClaimDueDeliveries(ctx, retryAt.Add(-time.Second), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("claim before the retry time returned %d rows (err=%v)", len(due), err)
	}
	due, err = s.ClaimDueDeliveries(ctx, retryAt, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim at the retry time returned %d rows (err=%v)", len(due), err)
	}
	if due[0].Attempts != 2 || due[0].LastError != "connection refused" {
		t.Fatalf("retried delivery = %+v", due[0])
	}

	if err := s.MarkDeliveryFailed(ctx, due[0].ID, "gave up"); err != nil {
		t.Fatalf("MarkDeliveryFailed: %v", err)
	}
	byEvent, err = s.ListDeliveriesForEvents(ctx, []int64{ev.ID})
	if err != nil {
		t.Fatalf("ListDeliveriesForEvents: %v", err)
	}
	states := map[store.DeliveryState]int{}
	for _, d := range byEvent[ev.ID] {
		states[d.State]++
	}
	if states[store.DeliveryDelivered] != 1 || states[store.DeliveryFailed] != 1 {
		t.Fatalf("final delivery states = %v", states)
	}
	// A failed delivery must stay visible — that is how the portal shows it.
	if len(byEvent[ev.ID]) != 2 {
		t.Fatalf("a terminal failure removed the row")
	}
}

// Two dispatchers over one queue must never claim the same row: FOR UPDATE
// SKIP LOCKED is what makes a leadership handover safe (docs/42 §4.10).
func TestClaimDueDeliveriesNeverDoubleClaims(t *testing.T) {
	s := storetest.New(t)
	ctx := t.Context()

	ev, err := s.AppendEvent(ctx, store.Event{Type: store.EventBanCreated, Actor: "op"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if err := s.EnqueueDeliveries(ctx, ev.ID, names); err != nil {
		t.Fatalf("EnqueueDeliveries: %v", err)
	}

	now := time.Now()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[int64]int{}
		total int
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimDueDeliveries(ctx, now, len(names))
			if err != nil {
				t.Errorf("ClaimDueDeliveries: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range claimed {
				seen[d.ID]++
				total++
			}
		}()
	}
	wg.Wait()

	for id, n := range seen {
		if n != 1 {
			t.Fatalf("delivery %d was claimed %d times", id, n)
		}
	}
	if total != len(names) {
		t.Fatalf("claimed %d of %d deliveries in total", total, len(names))
	}
}
