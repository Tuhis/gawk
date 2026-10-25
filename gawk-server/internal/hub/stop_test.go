package hub

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R78 (docs/79): a publisher that stops on purpose ends its broadcast now,
// through the same removal a GC takes, instead of leaving it in grace.

func TestPublisherStopEndsTheBroadcastNow(t *testing.T) {
	rec := &busRecorder{}
	var expired []string
	r := NewRegistry(discardLog, Options{
		BroadcastGrace:     5 * time.Minute,
		OnEvent:            rec.hook,
		OnBroadcastExpired: func(id string) { expired = append(expired, id) },
	})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &fakeSender{}
	sub, err := r.Subscribe(id, viewer)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if !pub.Stop() {
		t.Fatal("Stop did not end the broadcast")
	}
	viewer.mu.Lock()
	closed, code := viewer.closed, viewer.closeCode
	viewer.mu.Unlock()
	if !closed || code != wire.CloseCodeBroadcasterStopped {
		t.Errorf("viewer closed=%v with %d, want closed with 4014", closed, code)
	}
	if err := r.CheckSubscribe(id); err == nil {
		t.Error("the hub is still joinable after a stop; it should be gone, not in grace")
	}
	if len(expired) != 1 || expired[0] != id {
		t.Errorf("OnBroadcastExpired = %v, want [%s] (the Lease and rooms go with it)", expired, id)
	}
	ended := rec.only(events.TypeBroadcastEnded)
	if len(ended) != 1 {
		t.Fatalf("got %d broadcast.ended, want 1", len(ended))
	}
	if d := ended[0].Data.(events.BroadcastEndedData); d.Reason != events.BroadcastEndedStopped {
		t.Errorf("end reason = %q, want stopped", d.Reason)
	}
	// The publish handler's deferred Close still runs; it must not arm a
	// grace timer on the removed hub.
	pub.Close()
	if r.Stats().Totals.Broadcasts != 0 {
		t.Error("a broadcast is still registered after Stop and the deferred Close")
	}
}

func TestPlainPublisherCloseKeepsTheGrace(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	pub.Close()
	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("a plain close ended the broadcast (%v); it should wait out the grace", err)
	}
}

// docs/79 D3: a stop ends only the session that sent it. A session deposed
// by a token-bearing reclaim must not end its successor on the way out.
func TestStaleStopDoesNotEndTheSuccessor(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, old, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	_, successor, err := r.TakeOverPublish(id)
	if err != nil {
		t.Fatal(err)
	}
	if old.Stop() {
		t.Error("a deposed publisher's Stop ended the broadcast its successor holds")
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast gone after a stale stop: %v", err)
	}

	// And once the successor has left too, a stop read late from the deposed
	// session still must not end what is now the successor's grace.
	successor.Close()
	if old.Stop() {
		t.Error("a deposed publisher's late Stop ended the successor's graced broadcast")
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast gone after a late stale stop: %v", err)
	}
}

// A stop followed by a token-bearing reclaim of the same code (web Stop then
// Start, desktop End's undo) starts the broadcast again on that code.
func TestPublisherStopThenReclaimRecreates(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Stop() {
		t.Fatal("Stop did not end the broadcast")
	}
	gotID, again, err := r.ResumePublish(id)
	if err != nil {
		t.Fatalf("reclaim after a stop: %v", err)
	}
	defer again.Close()
	if gotID != id {
		t.Errorf("reclaim got %q, want %q", gotID, id)
	}
	// The first session's Stop, read again, must not end the new one.
	if pub.Stop() {
		t.Error("the stopped session's second Stop ended the recreated broadcast")
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("recreated broadcast not joinable: %v", err)
	}
}
