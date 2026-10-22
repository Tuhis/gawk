package transport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// A second PoP's downlink leg waiting on a reservation's hub is somebody
// waiting: the demand loop keeps asking for it after the first want, which
// may have been lost (the reserving PoP's link was down), as it does for a
// viewer. Legs are internal subscribers, so ExternalSubscribers missed it.
func TestTheDemandLoopKeepsAskingForAWaitingPoPLeg(t *testing.T) {
	reg := hub.NewRegistry(discardLog, hub.Options{})
	srv := New(config.Config{Addr: "127.0.0.1:0"}, reg, nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.SetCluster(&fakeCoordinator{leases: map[string]fakeLease{
		"K7XQ2M": {origin: cluster.Origin{Pop: testPop}},
	}}, "pod-self")
	if _, _, err := reg.EdgePublish("K7XQ2M"); err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	l := &demandLoop{s: srv, id: "K7XQ2M", pop: testPop}
	if l.stillWaiting(false) {
		t.Fatal("still waiting with nobody downstream")
	}
	leg, err := reg.SubscribeLeg("K7XQ2M", &edgeConn{}, hub.LegOptions{})
	if err != nil {
		t.Fatalf("SubscribeLeg: %v", err)
	}
	defer leg.Close()
	if !l.stillWaiting(false) {
		t.Fatal("a waiting PoP downlink leg does not keep the demand loop asking")
	}
}

// fakeDemandHome is a PopDemandHome that holds every link and records the
// wants it is asked to forward.
type fakeDemandHome struct {
	mu    sync.Mutex
	wants []string
}

func (f *fakeDemandHome) LinkHolder(string) (string, int64, bool) { return "", 0, false }
func (f *fakeDemandHome) HoldsLink(string) (int64, bool)          { return 1, true }
func (f *fakeDemandHome) ReservedFor(string) (string, bool)       { return testPop, true }
func (f *fakeDemandHome) Want(_ context.Context, _, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wants = append(f.wants, id)
	return "", nil
}

func (f *fakeDemandHome) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wants)
}

// A leg that ended without 4013 (the PoP host restarted, and with it the
// PoP's memory of the want) leaves its viewers on this pod's graced hub,
// where no subscribe miss will ever fire a want again. The leg's end asks
// the PoP again, and keeps asking while they wait and no leg is back.
func TestAnEndedLegsWaitingViewersAskThePoPAgain(t *testing.T) {
	reg := hub.NewRegistry(discardLog, hub.Options{})
	srv := New(config.Config{Addr: "127.0.0.1:0", PopDemandTimeout: 50 * time.Millisecond}, reg, nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	coord := &fakeCoordinator{leases: map[string]fakeLease{
		"K7XQ2M": {origin: cluster.Origin{Holder: "pod-self", Generation: 1, Pop: testPop}, inGrace: true},
	}}
	srv.SetCluster(coord, "pod-self")
	home := &fakeDemandHome{}
	srv.SetPopDemand(home)

	_, pub, err := reg.ResumePublish("K7XQ2M")
	if err != nil {
		t.Fatal(err)
	}
	v, err := reg.Subscribe("K7XQ2M", &edgeConn{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	pub.Close()

	srv.popLegEnded("K7XQ2M", testPop)
	waitFor(t, 5*time.Second, func() bool { return home.count() >= 2 }, "the want, then a second one")

	// Another pod's leg claims the Lease: the running loop stops asking
	// and goes (it must not outlive the test, reading the coordinator).
	coord.setLease("K7XQ2M", fakeLease{origin: cluster.Origin{Holder: "pod-other", Generation: 2, Pop: testPop}})
	l := &demandLoop{s: srv, id: "K7XQ2M", pop: testPop}
	if l.stillWaiting(false) {
		t.Error("still waiting once another pod's leg holds the Lease")
	}
	waitFor(t, 5*time.Second, func() bool {
		srv.uplink.mu.Lock()
		defer srv.uplink.mu.Unlock()
		return srv.uplink.loops["K7XQ2M"] == nil
	}, "the demand loop to end once another pod's leg holds the Lease")
}

// The single-pod waiting hub is no broadcast home ever had: making it and
// removing it emits no broadcast.started/ended, no publisher-away hook and
// no usage sample. The PoP's leg claiming it is the start.
func TestTheSinglePodWaitingHubIsNoBroadcastLifecycle(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, s)
	}
	reg := hub.NewRegistry(discardLog, hub.Options{
		BroadcastGrace:    time.Minute,
		OnEvent:           func(ev eventbus.Event) { note(ev.Type) },
		OnPublisherClosed: func(string) { note("publisher closed") },
		OnOriginEnded:     func(hub.BroadcastEnd) { note("origin ended") },
	})
	srv := New(config.Config{Addr: "127.0.0.1:0", PopDemandTimeout: 50 * time.Millisecond}, reg, nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.SetPopDemand(&fakeDemandHome{})
	if !srv.popWaitSinglePod(context.Background(), "K7XQ2M") {
		t.Fatal("no waiting hub")
	}
	if _, _, known := reg.BroadcastState("K7XQ2M"); !known {
		t.Fatal("the waiting hub is not there")
	}
	reg.TerminateBroadcast("K7XQ2M", 4012, "full")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("lifecycle signals for a waiting hub: %v", seen)
	}
}
