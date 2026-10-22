package transport

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
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
