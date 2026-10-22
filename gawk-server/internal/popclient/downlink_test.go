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
