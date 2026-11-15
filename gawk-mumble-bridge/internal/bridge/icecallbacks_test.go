package bridge

import (
	"testing"
	"time"
)

// D15's state machine (docs/89 MI2 criterion 2): each case drives the FSM
// the way the mirror does and counts the addCallback calls it asks for.
func TestCallbackFSM(t *testing.T) {
	const probe = 10 * time.Minute
	t0 := time.Unix(1_700_000_000, 0)
	type step struct {
		do string // connect, unsafe, event, drift, steady, probe, link, tick(+d)
		d  time.Duration
	}
	cases := []struct {
		name       string
		steps      []step
		adds       int
		removes    int
		state      string
		unanswered int
		backoff    time.Duration
	}{
		{name: "start registers once", steps: []step{{do: "connect"}}, adds: 1, state: cbRegistered, unanswered: 1, backoff: probe},
		{name: "a change that reverts within an interval: no drift, no call",
			steps: []step{{do: "connect"}, {do: "event"}, {do: "steady"}, {do: "steady"}}, adds: 1, state: cbRegistered, unanswered: 1, backoff: probe},
		{name: "a drifting reconcile after earlier events re-adds once",
			steps: []step{{do: "connect"}, {do: "event"}, {do: "drift"}}, adds: 2, state: cbRegistered, unanswered: 2, backoff: probe},
		{name: "a second drifting reconcile with no event gives up",
			steps: []step{{do: "connect"}, {do: "event"}, {do: "drift"}, {do: "drift"}}, adds: 2, removes: 1, state: cbUnreachable, unanswered: 2, backoff: probe},
		{name: "events received, then every delivery fails: exactly one addCallback before unreachable",
			steps: []step{{do: "connect"}, {do: "event"}, {do: "event"}, {do: "drift"}, {do: "drift"}, {do: "drift"}}, adds: 2, removes: 1, state: cbUnreachable, unanswered: 2, backoff: probe},
		{name: "never reachable: two registrations, then unreachable",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}}, adds: 2, removes: 1, state: cbUnreachable, unanswered: 2, backoff: probe},
		{name: "unreachable: N reconnects and link cycles make no call",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "connect"}, {do: "link"}, {do: "connect"}, {do: "link"}, {do: "connect"}, {do: "drift"}},
			adds:  2, removes: 1, state: cbUnreachable, unanswered: 2, backoff: probe},
		{name: "count at 1: a reconnect then a link cycle makes at most one call",
			steps: []step{{do: "connect"}, {do: "event"}, {do: "connect"}, {do: "link"}}, adds: 2, state: cbRegistered, unanswered: 2, backoff: probe},
		{name: "reconnects without events stop at the bound",
			steps: []step{{do: "connect"}, {do: "connect"}, {do: "connect"}, {do: "connect"}}, adds: 2, state: cbRegistered, unanswered: 2, backoff: probe},
		{name: "a probe that gets an event",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe}, {do: "probe"}, {do: "event"}},
			adds:  3, removes: 1, state: cbRegistered, unanswered: 1, backoff: probe},
		{name: "a probe that drifts doubles the backoff",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe}, {do: "probe"}, {do: "drift"}},
			adds:  3, removes: 2, state: cbUnreachable, unanswered: 3, backoff: 2 * probe},
		{name: "a probe with no drift stays pending",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe}, {do: "probe"}, {do: "steady"}, {do: "steady"}},
			adds:  3, removes: 1, state: cbProbing, unanswered: 3, backoff: probe},
		{name: "a probe is not due before its backoff",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe - time.Second}, {do: "probe"}},
			adds:  2, removes: 1, state: cbUnreachable, unanswered: 2, backoff: probe},
		{name: "a reconnect interrupts a probe: not run, backoff unchanged",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe}, {do: "probe"}, {do: "connect"}},
			adds:  3, removes: 2, state: cbUnreachable, unanswered: 3, backoff: probe},
		{name: "two interrupted probes in a row count as one failed",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "tick", d: probe}, {do: "probe"}, {do: "connect"},
				{do: "tick", d: probe}, {do: "probe"}, {do: "connect"}},
			adds: 4, removes: 3, state: cbUnreachable, unanswered: 4, backoff: 2 * probe},
		{name: "an event while unreachable: registered, count 1",
			steps: []step{{do: "connect"}, {do: "drift"}, {do: "drift"}, {do: "event"}}, adds: 2, removes: 1, state: cbRegistered, unanswered: 1, backoff: probe},
		{name: "the backoff doubles up to 2 h",
			steps: func() []step {
				s := []step{{do: "connect"}, {do: "drift"}, {do: "drift"}}
				for range 6 {
					s = append(s, step{do: "tick", d: 3 * time.Hour}, step{do: "probe"}, step{do: "drift"})
				}
				return s
			}(),
			adds: 8, removes: 7, state: cbUnreachable, unanswered: 8, backoff: 2 * time.Hour},
		{name: "1.6.870 never registers, through connects and drifts",
			steps: []step{{do: "unsafe"}, {do: "drift"}, {do: "unsafe"}, {do: "event"}, {do: "tick", d: 3 * time.Hour}, {do: "probe"}},
			state: cbUnsafe, backoff: probe},
		{name: "an upgraded murmur leaves unsafe and registers",
			steps: []step{{do: "unsafe"}, {do: "connect"}}, adds: 1, state: cbRegistered, unanswered: 1, backoff: probe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCallbackFSM(probe)
			now := t0
			adds, removes := 0, 0
			count := func(a cbAction) {
				if a.add {
					adds++
				}
				if a.remove {
					removes++
				}
			}
			for _, s := range c.steps {
				switch s.do {
				case "connect":
					count(f.onConnect(false, now))
				case "unsafe":
					count(f.onConnect(true, now))
				case "event":
					f.onEvent()
				case "drift":
					a, _ := f.onDrift(now)
					count(a)
				case "steady", "link": // no drift; links never touch the FSM
				case "probe":
					count(f.onProbe(now))
				case "tick":
					now = now.Add(s.d)
				}
			}
			if adds != c.adds || removes != c.removes || f.state != c.state || f.unanswered != c.unanswered || f.backoff != c.backoff {
				t.Fatalf("adds %d removes %d state %s unanswered %d backoff %s; want %d %d %s %d %s",
					adds, removes, f.state, f.unanswered, f.backoff, c.adds, c.removes, c.state, c.unanswered, c.backoff)
			}
		})
	}
}

// Giving up is reported once, on the drift that does it.
func TestCallbackFSMReportsGivingUpOnce(t *testing.T) {
	f := newCallbackFSM(time.Minute)
	now := time.Now()
	f.onConnect(false, now)
	if _, g := f.onDrift(now); g {
		t.Fatal("gave up on the first drift")
	}
	if _, g := f.onDrift(now); !g {
		t.Fatal("did not give up on the second")
	}
	if _, g := f.onDrift(now); g {
		t.Fatal("gave up twice")
	}
	if at, due := f.probeDue(); !due || !at.Equal(now.Add(time.Minute)) {
		t.Fatalf("probe due %v %v", at, due)
	}
}
