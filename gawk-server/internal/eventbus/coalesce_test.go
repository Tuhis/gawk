package eventbus

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The coalescer is pure and is tested without a server: a thousand-viewer
// broadcast must not be a thousand messages a second, and a dashboard must
// still see a value at most one interval stale (docs/51 D3).
func TestCoalescer(t *testing.T) {
	const interval = 5 * time.Second
	t0 := time.Date(2026, 9, 18, 9, 30, 0, 0, time.UTC)
	viewers := func(n int) Event {
		return Event{Type: events.TypeBroadcastViewers, Key: "3f9a1c4e7b2d",
			Data: events.BroadcastViewersData{BroadcastID: "k7m2q9", BroadcastKey: "3f9a1c4e7b2d",
				Role: events.RoleOrigin, ViewersLocal: n}}
	}

	c := newCoalescer(interval)
	if !c.isDelta(events.TypeBroadcastViewers) || !c.isDelta(events.TypeRoomAttachmentUpdated) {
		t.Fatal("the delta types are the two coalesced ones")
	}
	if c.isDelta(events.TypeRoomParticipantJoined) {
		t.Fatal("a discrete transition must never be coalesced")
	}

	// First value goes out immediately: a consumer should not wait an interval
	// for the first number.
	if _, ok := c.offer(viewers(1), t0); !ok {
		t.Fatal("the first value was withheld")
	}
	// Same value again, later: nothing changed, so nothing is published.
	if _, ok := c.offer(viewers(1), t0.Add(interval)); ok {
		t.Error("an unchanged value was published")
	}
	// Changes inside the interval are held, not dropped...
	if _, ok := c.offer(viewers(2), t0.Add(time.Second)); ok {
		t.Error("a change inside the interval was published immediately")
	}
	if _, ok := c.offer(viewers(3), t0.Add(2*time.Second)); ok {
		t.Error("a change inside the interval was published immediately")
	}
	if got := c.flush(t0.Add(time.Second)); len(got) != 0 {
		t.Error("flushed before the interval elapsed")
	}
	// ...and the LATEST one is what goes out when the interval elapses.
	out := c.flush(t0.Add(interval))
	if len(out) != 1 {
		t.Fatalf("flush returned %d events, want 1", len(out))
	}
	if got := out[0].Data.(events.BroadcastViewersData).ViewersLocal; got != 3 {
		t.Errorf("flushed viewersLocal %d, want the latest (3)", got)
	}
	// Nothing held any more.
	if got := c.flush(t0.Add(2 * interval)); len(got) != 0 {
		t.Errorf("flush repeated an event: %v", got)
	}
}

// TestCoalescerIsPerKey: two broadcasts do not throttle each other.
func TestCoalescerIsPerKey(t *testing.T) {
	c := newCoalescer(5 * time.Second)
	now := time.Now()
	for _, key := range []string{"aaa", "bbb"} {
		ev := Event{Type: events.TypeBroadcastViewers, Key: key,
			Data: events.BroadcastViewersData{BroadcastKey: key, ViewersLocal: 1}}
		if _, ok := c.offer(ev, now); !ok {
			t.Errorf("%s: first value withheld", key)
		}
	}
}
