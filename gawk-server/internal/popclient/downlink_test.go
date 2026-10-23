package popclient

import (
	"context"
	"net/http"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// docs/76 items 23-24: the downlink dials home only while linked, with the
// stored endpoint and credential, the leg delivery from -pop-leg-delivery
// and home's downlink ceiling lowered by the host's -downlink-budget.
func TestPopDownlinkTarget(t *testing.T) {
	c := New(Options{Relay: config.RelayConfig{PopLegDelivery: config.PopLegCarrier, DownlinkBudget: config.Budget{Mbps: 40}}})
	if _, ok := c.PopDownlink(); ok {
		t.Fatal("standalone: a downlink target")
	}
	c.stored = &storedCredential{Endpoint: "api.gawk.test:4433", Credential: "cred"}
	cfg := &poplink.Config{PopName: "assembly", Downlink: pops.Budget{MaxMbps: 100, MaxStreams: 4}}
	for _, h := range []string{HeaderWaiting, HeaderOffline, HeaderRevoked, HeaderRepair} {
		c.setState(func(s *LinkState) { s.Header, s.Config = h, cfg })
		if _, ok := c.PopDownlink(); ok {
			t.Fatalf("%s: a downlink target", h)
		}
	}
	c.setState(func(s *LinkState) { s.Header = HeaderLinked })
	got, ok := c.PopDownlink()
	want := transport.PopDownlinkTarget{
		Endpoint: "api.gawk.test:4433", Credential: "cred", Pop: "assembly", Carrier: true,
		Budget: transport.PopBudget{MaxMbps: 40, MaxStreams: 4},
	}
	if !ok || got.Endpoint != want.Endpoint || got.Credential != want.Credential || got.Pop != want.Pop ||
		got.Carrier != want.Carrier || got.Budget != want.Budget || got.Dial != nil {
		t.Fatalf("linked: %+v %v, want %+v", got, ok, want)
	}
	c.opts.Relay.PopLegDelivery = config.PopLegDatagram
	if got, _ := c.PopDownlink(); got.Carrier {
		t.Fatal("-pop-leg-delivery=datagram still asks for the carrier")
	}
	// A test's Options.Dial (a load balancer) carries the legs too.
	c.opts.Dial = func(context.Context, string, string, http.Header) (Session, *http.Response, error) {
		return nil, nil, context.Canceled
	}
	if got, _ := c.PopDownlink(); got.Dial == nil {
		t.Fatal("Options.Dial not used for the legs")
	}
}

func TestDownlinkBudget(t *testing.T) {
	for _, tc := range []struct {
		home pops.Budget
		host config.Budget
		want transport.PopBudget
	}{
		{pops.Budget{}, config.Budget{}, transport.PopBudget{}},
		{pops.Budget{MaxMbps: 100}, config.Budget{}, transport.PopBudget{MaxMbps: 100}},
		{pops.Budget{}, config.Budget{Mbps: 30, Streams: 2}, transport.PopBudget{MaxMbps: 30, MaxStreams: 2}},
		{pops.Budget{MaxMbps: 100, MaxStreams: 1}, config.Budget{Mbps: 30, Streams: 5}, transport.PopBudget{MaxMbps: 30, MaxStreams: 1}},
	} {
		if got := downlinkBudget(tc.home, tc.host); got != tc.want {
			t.Errorf("downlinkBudget(%+v, %+v) = %+v, want %+v", tc.home, tc.host, got, tc.want)
		}
	}
}

// docs/76 §7.1: the console's Lower reaches the legs' admission — the
// downlink's and the uplink's — below the host's flags and home's ceiling,
// and the stats record reports the lowered budgets. Clearing a field goes
// back to the configured value; a lower never raises one.
func TestHostLoweredBudgetsReachTheLegs(t *testing.T) {
	c := New(Options{Relay: config.RelayConfig{
		DownlinkBudget: config.Budget{Mbps: 40},
		UplinkBudget:   config.Budget{Mbps: 20, Streams: 3},
	}})
	c.stored = &storedCredential{Endpoint: "api.gawk.test:4433", Credential: "cred"}
	cfg := &poplink.Config{PopName: "assembly",
		Uplink:   pops.Budget{MaxMbps: 25, MaxStreams: 3},
		Downlink: pops.Budget{MaxMbps: 100, MaxStreams: 4}}
	c.setState(func(s *LinkState) { s.Header, s.Config = HeaderLinked, cfg })

	c.LowerBudgets(config.Budget{Streams: 1}, config.Budget{Mbps: 10, Streams: 2})
	got, _ := c.PopDownlink()
	if want := (transport.PopBudget{MaxMbps: 10, MaxStreams: 2}); got.Budget != want {
		t.Fatalf("lowered downlink: %+v, want %+v", got.Budget, want)
	}
	if got, want := c.up.budget(), (pops.Budget{MaxMbps: 20, MaxStreams: 1}); got != want {
		t.Fatalf("lowered uplink: %+v, want %+v", got, want)
	}
	st := c.stats()
	if st.Downlink != (pops.Budget{MaxMbps: 10, MaxStreams: 2}) || st.Uplink != (pops.Budget{MaxMbps: 20, MaxStreams: 1}) {
		t.Fatalf("stats report %+v / %+v, not the lowered budgets", st.Uplink, st.Downlink)
	}

	// Lowering one field leaves the other at its configured value.
	c.LowerBudgets(config.Budget{}, config.Budget{Streams: 2})
	if got, _ := c.PopDownlink(); got.Budget != (transport.PopBudget{MaxMbps: 40, MaxStreams: 2}) {
		t.Fatalf("streams-only lower: %+v", got.Budget)
	}
	// A "lower" above the configured value does not raise it.
	c.LowerBudgets(config.Budget{Mbps: 50}, config.Budget{Mbps: 90})
	if got, _ := c.PopDownlink(); got.Budget != (transport.PopBudget{MaxMbps: 40, MaxStreams: 4}) {
		t.Fatalf("raised downlink: %+v", got.Budget)
	}
	if got, want := c.up.budget(), (pops.Budget{MaxMbps: 20, MaxStreams: 3}); got != want {
		t.Fatalf("raised uplink: %+v, want %+v", got, want)
	}
	// Cleared: the configured budgets again.
	c.LowerBudgets(config.Budget{}, config.Budget{})
	if got, _ := c.PopDownlink(); got.Budget != (transport.PopBudget{MaxMbps: 40, MaxStreams: 4}) {
		t.Fatalf("cleared: %+v", got.Budget)
	}
}
