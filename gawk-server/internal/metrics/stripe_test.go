package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R30 (docs/35 §7): striping has to be scrapeable — the legs gauge is what an
// operator sizes maxSubscribers against, and transitions churning against a
// flat gauge is the flapping signature.
func TestStripeMetricsExposeLegsAndSuppression(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{ParityDefault: 2, StripedDelivery: true})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, err := r.SubscribeParity(id, fakeConn{}, 2)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	for j := 0; j < 2; j++ {
		if _, err := r.SubscribeStripeLeg(id, fakeConn{}, hub.StripeLeg{N: 2, Member: j}, 2); err != nil {
			t.Fatalf("SubscribeStripeLeg(%d): %v", j, err)
		}
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	pub.HandleDatagram(deltaDgram(t, 1)) // withheld from the striped primary

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(NewRegistryCollector(r))
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	b := map[string]string{"broadcast": "*"}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"gawk_broadcast_stripe_legs", b, 2},
		{"gawk_broadcast_striped_primaries", b, 1},
		{"gawk_broadcast_stripe_suppressed_datagrams_total", b, 1},
		{"gawk_broadcast_stripe_transitions_total", b, 1},
		{"gawk_relay_stripe_suppressed_datagrams_total", nil, 1},
		{"gawk_relay_stripe_transitions_total", nil, 1},
	} {
		if got := value(mfs, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// A fleet with striping unused must read zero, not absent — the parity rule
// (a series that appears only under load is worse than one that reads 0).
func TestStripeMetricsZeroWhenUnused(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, err := r.Subscribe(id, fakeConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pub.HandleDatagram(deltaDgram(t, 1))

	promReg := prometheus.NewPedanticRegistry()
	promReg.MustRegister(NewRegistryCollector(r))
	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, name := range []string{
		"gawk_relay_stripe_suppressed_datagrams_total",
		"gawk_relay_stripe_transitions_total",
	} {
		if got := value(mfs, name, nil); got != 0 {
			t.Errorf("%s = %v on a striping-unused fleet, want 0", name, got)
		}
	}
}
