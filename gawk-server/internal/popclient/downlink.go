package popclient

// downlink.go: the downlink (docs/76 §6.6, items 23-24). A party viewer's
// local miss is resolved at home through /pop/subscribe: the transport's
// PoP edge manager (transport/pop_downlink.go) dials it. The Client is that
// manager's link source: it implements transport.PopDownlinkSource, and
// the relay installs it with SetLocalRelay, so installing the link wires
// the downlink too.

import (
	"context"
	"errors"
	"net/http"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

var _ transport.PopDownlinkSource = (*Client)(nil)

// errNotMediaSession: Options.Dial returned a session the downlink cannot
// read media from.
var errNotMediaSession = errors.New("popclient: the dialed leg is not a WebTransport session")

// PopDownlink is home as the downlink dials it: the endpoint and
// credential while linked (ok false otherwise, so a miss stays local), the
// leg delivery (-pop-leg-delivery: the carrier unless "datagram") and the
// effective downlink budget.
func (c *Client) PopDownlink() (transport.PopDownlinkTarget, bool) {
	st := c.State()
	c.mu.Lock()
	stored := c.stored
	c.mu.Unlock()
	if st.Header != HeaderLinked || st.Config == nil || stored == nil {
		return transport.PopDownlinkTarget{}, false
	}
	t := transport.PopDownlinkTarget{
		Endpoint:   stored.Endpoint,
		Credential: stored.Credential,
		Pop:        st.Config.PopName,
		Carrier:    c.opts.Relay.PopLegDelivery != config.PopLegDatagram,
		Budget:     downlinkBudget(st.Config.Downlink, c.opts.Relay.DownlinkBudget),
		RootCAs:    c.opts.RootCAs,
	}
	if c.opts.Dial != nil {
		t.Dial = c.legDial
	}
	return t, true
}

// legDial is the downlink's leg dial through Options.Dial (a test's load
// balancer): the session must be a WebTransport one.
func (c *Client) legDial(ctx context.Context, target string, h http.Header) (transport.PopLegSession, *http.Response, error) {
	sess, rsp, err := c.opts.Dial(ctx, "udp", target, h)
	if err != nil {
		return nil, rsp, err
	}
	leg, ok := sess.(transport.PopLegSession)
	if !ok {
		_ = sess.CloseWithError(0, "not a media session")
		return nil, rsp, errNotMediaSession
	}
	return leg, rsp, nil
}

// downlinkBudget is home's ceiling lowered by the host's -downlink-budget:
// per field, the smaller non-zero value (zero is unlimited on both sides).
func downlinkBudget(home pops.Budget, host config.Budget) transport.PopBudget {
	return transport.PopBudget{
		MaxMbps:    lowerOf(home.MaxMbps, host.Mbps),
		MaxStreams: lowerOf(home.MaxStreams, host.Streams),
	}
}

func lowerOf(a, b int) int {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	}
	return min(a, b)
}

// DownlinkUse is the downlink as the console and the stats record show it.
func (c *Client) DownlinkUse() transport.PopDownlinkUse {
	if c.opts.Server == nil {
		return transport.PopDownlinkUse{}
	}
	return c.opts.Server.PopDownlinkUse()
}
