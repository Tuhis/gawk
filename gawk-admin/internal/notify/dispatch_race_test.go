package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-admin/internal/store/storetest"
)

// TestTwoDispatchersNeverDoubleSend is the SKIP LOCKED criterion (§4.10, D16):
// two dispatchers over ONE queue — the shape of a leadership handover, where
// the old leader has not noticed yet and the new one has already started —
// must deliver each row exactly once.
//
// Two separate *store.Store handles on one database, not one shared pool, so
// this is genuinely two replicas rather than two goroutines sharing a
// connection pool.
func TestTwoDispatchersNeverDoubleSend(t *testing.T) {
	ctx := t.Context()
	dsn := storetest.FreshDSN(t)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stA, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open replica A: %v", err)
	}
	defer stA.Close()
	stB, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open replica B: %v", err)
	}
	defer stB.Close()

	rec := newReceiver(t)

	// overlap closes the first time two requests are in flight at once. Each
	// dispatcher sends strictly one at a time (Concurrency: 1), so two
	// simultaneous requests can only mean BOTH dispatchers are sending — which
	// is the condition this test needs to have actually exercised.
	var (
		mu       sync.Mutex
		inFlight int
		overlap  = make(chan struct{})
		closed   bool
	)
	rec.setHook(func(capture) {
		mu.Lock()
		inFlight++
		if inFlight >= 2 && !closed {
			closed = true
			close(overlap)
		}
		done := closed
		mu.Unlock()
		if !done {
			// Hold the response briefly so the other dispatcher has a chance
			// to overlap. Bounded, so a single-dispatcher run still finishes.
			select {
			case <-overlap:
			case <-time.After(2 * time.Second):
			}
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	})

	cfg := config.Config{
		ExternalURL: "https://admin.example.com",
		StaticWebhooks: []config.StaticWebhook{
			{Name: "chart-pager", URL: rec.url("/chart-pager"), SecretEnv: "S", Secret: "chart-secret"},
			{Name: "chart-matrix", URL: rec.url("/chart-matrix"), SecretEnv: "S2", Secret: "matrix-secret"},
		},
	}
	mustCreateWebhook(t, stA, "ui-slack", rec.url("/ui-slack"), "ui-secret", true)

	// BatchSize 1 keeps one dispatcher from swallowing the whole queue in a
	// single claim, so both really race for rows.
	newRacer := func(st *store.Store) *Dispatcher {
		return newDispatcher(t, st, cfg, func(o *Options) { o.BatchSize = 1 })
	}
	dA, dB := newRacer(stA), newRacer(stB)

	const events = 6
	wantDeliveries := events * 3 // three enabled webhooks
	eventIDs := make([]int64, 0, events)
	for range events {
		ev := mustRecord(t, dA, killEvent("ZXQ7K2"))
		eventIDs = append(eventIDs, ev.ID)
	}

	var wg sync.WaitGroup
	for _, d := range []*Dispatcher{dA, dB} {
		wg.Add(1)
		go func(d *Dispatcher) {
			defer wg.Done()
			// Drain until the queue stops handing out work. Each pass claims
			// at most one row, so both dispatchers keep coming back for more.
			idle := 0
			for idle < 3 {
				n, err := d.DispatchOnce(ctx)
				if err != nil {
					t.Errorf("DispatchOnce: %v", err)
					return
				}
				if n == 0 {
					idle++
					time.Sleep(10 * time.Millisecond)
					continue
				}
				idle = 0
			}
		}(d)
	}
	wg.Wait()

	select {
	case <-overlap:
	default:
		t.Fatal("the two dispatchers never had requests in flight simultaneously: the SKIP LOCKED race was not exercised")
	}

	// Exactly once, per delivery row: the header carries the row's derived
	// UUID, so counting distinct values against total requests catches both a
	// double-send and a lost send.
	seen := map[string]int{}
	for _, c := range rec.captures() {
		seen[c.delivery]++
	}
	if len(rec.captures()) != wantDeliveries {
		t.Errorf("receiver saw %d requests, want %d (one per queued delivery)", len(rec.captures()), wantDeliveries)
	}
	if len(seen) != wantDeliveries {
		t.Errorf("saw %d distinct delivery ids, want %d", len(seen), wantDeliveries)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("delivery %s was sent %d times: two dispatchers double-sent it", id, n)
		}
	}

	// And the queue agrees: every row delivered exactly one attempt.
	byEvent, err := stA.ListDeliveriesForEvents(context.Background(), eventIDs)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	total := 0
	for _, rows := range byEvent {
		for _, row := range rows {
			total++
			if row.State != store.DeliveryDelivered {
				t.Errorf("delivery %d (%s): state = %q, want delivered", row.ID, row.WebhookName, row.State)
			}
			if row.Attempts != 1 {
				t.Errorf("delivery %d (%s): attempts = %d, want 1 — a second dispatcher claimed a row that was already in flight",
					row.ID, row.WebhookName, row.Attempts)
			}
		}
	}
	if total != wantDeliveries {
		t.Errorf("queue holds %d delivery rows, want %d", total, wantDeliveries)
	}
}
