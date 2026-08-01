package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwgraph"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwtest"
)

// The registry's opening burst is the one delivery that cannot be retried.
//
// `pw_core_get_registry` answers with every existing global exactly once, per
// binding — so an application that was already playing audio when the helper
// connected is announced in that burst and never again. If those callbacks land
// before the connection is reachable from Go, the app is invisible for the
// helper's whole life: the card lists nothing while a game is audibly playing,
// and no round-trip recovers it.
//
// This test drives connect() in-process rather than the shipped binary, because
// what it is about is the ordering *inside* connect — and it widens the gap
// with testHookBeforeLoopStart so the answer is a fact rather than a race the
// scheduler usually wins (docs/39 F8).
func TestTheOpeningRegistryBurstIsNotDropped(t *testing.T) {
	d := pwtest.Start(t)
	adoptDaemonEnv(t, d)

	// Already emitting, and already in the graph, before we connect: its
	// globals can only reach us in the opening burst.
	d.StartEmitter("burst-game", 440)

	restore := testHookBeforeLoopStart
	testHookBeforeLoopStart = func() { time.Sleep(250 * time.Millisecond) }
	t.Cleanup(func() { testHookBeforeLoopStart = restore })

	c, err := connect()
	if err != nil {
		t.Skipf("could not connect to the test daemon in-process: %v", err)
	}
	t.Cleanup(c.close)

	// The same two round-trips run() performs: the first walks the registry,
	// the second collects the bound objects' identities.
	graph := pwgraph.New()
	for i := 0; i < 2; i++ {
		if err := c.roundtrip(10 * time.Second); err != nil {
			t.Fatalf("round-trip %d: %v", i+1, err)
		}
		for _, q := range c.drain() {
			switch q.op {
			case queuedAdd:
				graph.Add(q.global)
			case queuedMerge:
				graph.Merge(q.id, q.props)
			case queuedRemove:
				graph.Remove(q.id)
			}
		}
	}

	apps := graph.Apps()
	for _, a := range apps {
		if a.Binary == "burst-game" {
			return
		}
	}
	t.Errorf("the application playing audio before we connected is missing from the graph; "+
		"its registry globals were dropped. Apps() = %+v", apps)
}

// adoptDaemonEnv points this process at the test daemon. The other tests in
// this package spawn the helper as a child and hand it d.Env; an in-process
// connection needs the same variables on itself.
func adoptDaemonEnv(t *testing.T, d *pwtest.Daemon) {
	t.Helper()
	for _, kv := range d.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if os.Getenv(k) != v {
			t.Setenv(k, v)
		}
	}
}
