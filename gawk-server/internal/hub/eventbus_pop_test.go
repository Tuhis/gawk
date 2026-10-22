package hub

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// TestBroadcastEventsCarryThePop: with Options.PopOf naming a broadcast's
// R74 PoP relay (docs/76 §6.4, PP8), every broadcast event the hub publishes
// for it says so, and a home broadcast's events carry none.
func TestBroadcastEventsCarryThePop(t *testing.T) {
	rec := &busRecorder{}
	var pop string
	r := NewRegistry(discardLog, Options{OnEvent: rec.hook, PopOf: func(string) string { return pop }})

	pop = "assembly-2026"
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	r.busStalled(id, true)
	r.busStalled(id, false)
	r.PumpViewerCounts(time.Now())
	pub.Close()
	if !r.expireBroadcast(id, func(*broadcastHub) bool { return true }) {
		t.Fatal("grace expiry did not remove the hub")
	}
	popOf := func(d any) string {
		switch d := d.(type) {
		case events.BroadcastStartedData:
			return d.Pop
		case events.BroadcastEndedData:
			return d.Pop
		case events.BroadcastPublisherAwayData:
			return d.Pop
		case events.BroadcastPublisherBackData:
			return d.Pop
		case events.BroadcastViewersData:
			return d.Pop
		}
		t.Fatalf("unexpected event data %T", d)
		return ""
	}
	seen := map[string]bool{}
	rec.mu.Lock()
	evs := append([]any(nil), func() []any {
		out := make([]any, 0, len(rec.evs))
		for _, ev := range rec.evs {
			seen[ev.Type] = true
			out = append(out, ev.Data)
		}
		return out
	}()...)
	rec.mu.Unlock()
	for _, typ := range []string{events.TypeBroadcastStarted, events.TypeBroadcastPublisherAway,
		events.TypeBroadcastPublisherBack, events.TypeBroadcastViewers, events.TypeBroadcastEnded} {
		if !seen[typ] {
			t.Errorf("no %s published", typ)
		}
	}
	for _, d := range evs {
		if got := popOf(d); got != "assembly-2026" {
			t.Errorf("%T.Pop = %q, want assembly-2026", d, got)
		}
	}

	// A home broadcast: PopOf says "", and the events carry no pop.
	pop = ""
	rec2 := &busRecorder{}
	r2 := NewRegistry(discardLog, Options{OnEvent: rec2.hook, PopOf: func(string) string { return pop }})
	if _, _, err := r2.StartPublish(""); err != nil {
		t.Fatal(err)
	}
	if d := rec2.only(events.TypeBroadcastStarted)[0].Data.(events.BroadcastStartedData); d.Pop != "" {
		t.Errorf("home broadcast's started event names pop %q", d.Pop)
	}
	// Nil PopOf (every relay before R74) is the same.
	rec3 := &busRecorder{}
	r3 := NewRegistry(discardLog, Options{OnEvent: rec3.hook})
	if _, _, err := r3.StartPublish(""); err != nil {
		t.Fatal(err)
	}
	if d := rec3.only(events.TypeBroadcastStarted)[0].Data.(events.BroadcastStartedData); d.Pop != "" {
		t.Errorf("started event without PopOf names pop %q", d.Pop)
	}
}
