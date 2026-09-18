package hub

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type busRecorder struct {
	mu  sync.Mutex
	evs []eventbus.Event
}

func (b *busRecorder) hook(ev eventbus.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.evs = append(b.evs, ev)
}

func (b *busRecorder) types() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.evs))
	for i, ev := range b.evs {
		// "broadcast.started", not the bare "started" events.Name gives.
		out[i] = strings.TrimPrefix(ev.Type, events.TypePrefix)
	}
	return out
}

func (b *busRecorder) only(typ string) []eventbus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []eventbus.Event
	for _, ev := range b.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func equalTypes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBroadcastLifecycleOnTheBus walks the sequence docs/51 EB2 names:
// publish, a token-bearing reclaim that supersedes it, and the grace GC. The
// three end reasons are distinct because a consumer acts differently on each
// — a replacement is a broadcaster reconnecting, a gc is a stream ending.
func TestBroadcastLifecycleOnTheBus(t *testing.T) {
	rec := &busRecorder{}
	r := NewRegistry(discardLog, Options{OnEvent: rec.hook})

	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	// A token-bearing reclaim: the incumbent is deposed, not asked to leave.
	_, pub, err := r.TakeOverPublish(id)
	if err != nil {
		t.Fatal(err)
	}
	// The broadcaster goes away and the grace expires with nobody publishing.
	pub.Close()
	if !r.expireBroadcast(id, func(*broadcastHub) bool { return true }) {
		t.Fatal("grace expiry did not remove the hub")
	}

	want := []string{"broadcast.started", "broadcast.ended", "broadcast.started", "broadcast.ended"}
	if got := rec.types(); !equalTypes(got, want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	ended := rec.only(events.TypeBroadcastEnded)
	if d := ended[0].Data.(events.BroadcastEndedData); d.Reason != events.BroadcastEndedReplaced {
		t.Errorf("first end reason = %q, want replaced", d.Reason)
	}
	if d := ended[1].Data.(events.BroadcastEndedData); d.Reason != events.BroadcastEndedGC {
		t.Errorf("second end reason = %q, want gc", d.Reason)
	}
	// The subject is the HMAC'd key; the raw joinable ID stays in the body,
	// which is the internal/public split the bus rests on (docs/51 D2).
	started := rec.only(events.TypeBroadcastStarted)
	if started[0].Key != r.ObfuscateID(id) || started[0].Key == id {
		t.Errorf("subject = %q, want the obfuscated key of %q", started[0].Key, id)
	}
	if d := started[0].Data.(events.BroadcastStartedData); d.BroadcastID != id || d.Role != events.RoleOrigin {
		t.Errorf("started data = %+v", d)
	}
}

// TestKillIsNotAGC: an operator ending a broadcast (R39) and a grace expiry
// are the same removal in the code and two different facts on the bus.
func TestKillIsNotAGC(t *testing.T) {
	rec := &busRecorder{}
	r := NewRegistry(discardLog, Options{OnEvent: rec.hook})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	if !r.TerminateBroadcast(id, uint32(wire.CloseCodeTerminatedByOperator), "banned") {
		t.Fatal("terminate did not remove the hub")
	}
	ended := rec.only(events.TypeBroadcastEnded)
	if len(ended) != 1 {
		t.Fatalf("got %d broadcast.ended, want 1", len(ended))
	}
	if d := ended[0].Data.(events.BroadcastEndedData); d.Reason != events.BroadcastEndedKilled {
		t.Errorf("end reason = %q, want killed", d.Reason)
	}
}

// TestEdgePublishesOnlyItsOwnViewers is docs/51 D4's one exception: an edge
// pod knows its local count and nothing else, and says role: edge so a
// consumer never mistakes it for the fleet-wide number.
func TestEdgePublishesOnlyItsOwnViewers(t *testing.T) {
	rec := &busRecorder{}
	r := NewRegistry(discardLog, Options{OnEvent: rec.hook})
	id, _, err := r.EdgePublish("ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.only(events.TypeBroadcastStarted); len(got) != 0 {
		t.Errorf("an edge pod published %d broadcast.started events, want none — "+
			"the origin publishes that fact", len(got))
	}
	r.PumpViewerCounts(time.Now())
	viewers := rec.only(events.TypeBroadcastViewers)
	if len(viewers) != 1 {
		t.Fatalf("got %d broadcast.viewers, want 1: %v", len(viewers), rec.types())
	}
	d := viewers[0].Data.(events.BroadcastViewersData)
	// An edge reports its own count under both properties: the schema requires
	// viewersGlobal, and a zero there would read as "nobody is watching".
	if d.Role != events.RoleEdge || d.ViewersGlobal != d.ViewersLocal || d.BroadcastID != id {
		t.Errorf("viewers data = %+v", d)
	}
}

// TestStallTransitionsReachTheBus: the away/back pair is computed once per
// transition by the stall sweep, so it cannot flap on the bus without
// flapping in the fleet.
func TestStallTransitionsReachTheBus(t *testing.T) {
	rec := &busRecorder{}
	r := NewRegistry(discardLog, Options{OnEvent: rec.hook})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	r.busStalled(id, true)
	r.busStalled(id, false)
	if got := rec.types(); !equalTypes(got[1:], []string{"broadcast.publisher_away", "broadcast.publisher_back"}) {
		t.Errorf("stall events = %v", got)
	}
}
