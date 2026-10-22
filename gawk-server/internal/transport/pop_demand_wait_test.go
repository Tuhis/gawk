package transport

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
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

// refusingDemandHome is a single-pod PopDemandHome whose PoP refuses every
// want for its budget, once release closes.
type refusingDemandHome struct {
	fakeDemandHome
	release chan struct{}
}

func (f *refusingDemandHome) Want(ctx context.Context, pop, id string) (string, error) {
	_, _ = f.fakeDemandHome.Want(ctx, pop, id)
	select {
	case <-f.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return poplink.RefuseBudget, nil
}

// The want goes once the viewer is on the waiting hub (docs/76 item 17: B
// upgrades the viewer, then asks). Sent from the subscribe miss, before the
// upgrade, a fast refusal ended the hub before the viewer subscribed, and the
// viewer got 4000 "broadcast ended" instead of 4012 (CI,
// TestFourthStreamIsRefusedWith4012). The hook holds the viewer between its
// upgrade and its subscribe; any want asked by then is answered at once.
func TestARefusedWantReachesAViewerStillUpgrading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, reg, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:   15,
		MaxIdleTimeout:   30 * time.Second,
		KeepAlivePeriod:  10 * time.Second,
		BroadcastGrace:   5 * time.Minute,
		PopDemandTimeout: 5 * time.Second,
	}, discardLog)
	home := &refusingDemandHome{release: make(chan struct{})}
	srv.SetPopDemand(home)

	var askedEarly atomic.Bool
	var once sync.Once
	hook := func(id string) {
		once.Do(func() {
			srv.uplink.mu.Lock()
			early := srv.uplink.loops[id] != nil
			srv.uplink.mu.Unlock()
			askedEarly.Store(early)
			close(home.release)
			if !early {
				return
			}
			// An early want's refusal ends the hub under the viewer: let it
			// (no t.Fatal here, on the handler goroutine).
			deadline := time.Now().Add(5 * time.Second)
			for !errors.Is(reg.CheckSubscribe(id), hub.ErrNotFound) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
	srv.testHookPostUpgradeSubscribe.Store(&hook)

	sub := dialSubscriber(t, ctx, port, "K7XQ2M", clientTLS)
	n, nAt, code, cAt := drainUntilClosed(t, ctx, sub)
	if askedEarly.Load() {
		t.Error("the want was asked before the viewer was on the waiting hub")
	}
	assertNoticedClose(t, "viewer", wire.CloseCodePopUplinkFull, n, nAt, code, cAt)
	if home.count() == 0 {
		t.Error("no want was asked")
	}
}

// reservingFakeCoordinator resolves from its leases, as Lookup does: a
// reservation resolves to its PoP with no holder.
type reservingFakeCoordinator struct{ *fakeCoordinator }

func (f reservingFakeCoordinator) Resolve(_ context.Context, id string) (cluster.Origin, error) {
	o, _, _, ok := f.Lookup(id)
	if !ok {
		return cluster.Origin{}, cluster.ErrNotFound
	}
	return o, nil
}

// linkedPopHome is fakePopHome holding every PoP's link on this pod, the
// fleet-wide leg count's locator.
type linkedPopHome struct{ *fakePopHome }

func (linkedPopHome) LinkHolder(string) (string, int64, bool) { return "", 1, true }
func (linkedPopHome) HoldsLink(string) (int64, bool)          { return 1, true }

// Another PoP's downlink leg on a reservation waits on the hub like a
// viewer, so its want goes once the leg is on the hub too. Asked before,
// a quick refusal ended the hub first and the leg read 4000 "broadcast
// ended", which its PoP passes on to its party viewers as final; 4012 is
// a refusal it retries.
func TestARefusedWantReachesADownlinkLegStillUpgrading(t *testing.T) {
	port, clientTLS, srv, fph := startPopHome(t, nil)
	srv.SetPopHome(linkedPopHome{fph})
	srv.SetCluster(reservingFakeCoordinator{&fakeCoordinator{leases: map[string]fakeLease{
		"K7XQ2M": {origin: cluster.Origin{Pop: testPop}},
	}}}, "pod-self")
	home := &refusingDemandHome{release: make(chan struct{})}
	srv.SetPopDemand(home)
	reg := srv.registry

	var askedEarly atomic.Bool
	var once sync.Once
	hook := func(id string) {
		once.Do(func() {
			srv.uplink.mu.Lock()
			early := srv.uplink.loops[id] != nil
			srv.uplink.mu.Unlock()
			askedEarly.Store(early)
			close(home.release)
			if !early {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for !errors.Is(reg.CheckSubscribe(id), hub.ErrNotFound) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
	srv.testHookPostUpgradeSubscribe.Store(&hook)

	leg, rsp, err := popDial(t, port, clientTLS, "/pop/subscribe/K7XQ2M?proto=1",
		http.Header{"Authorization": {"Bearer good"}})
	if err != nil {
		t.Fatalf("leg refused: %v %+v", err, rsp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var code webtransport.SessionErrorCode
	for {
		if _, err := leg.AcceptUniStream(ctx); err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				t.Fatalf("leg ended with %v, want a session close", err)
			}
			code = se.ErrorCode
			break
		}
	}
	if askedEarly.Load() {
		t.Error("the want was asked before the leg was on the waiting hub")
	}
	if code != webtransport.SessionErrorCode(wire.CloseCodePopUplinkFull) {
		t.Errorf("leg closed with %d, want %d", code, wire.CloseCodePopUplinkFull)
	}
}
