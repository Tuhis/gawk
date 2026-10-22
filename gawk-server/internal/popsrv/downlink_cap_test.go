package popsrv

// docs/76 item 24 across the fleet: home caps a PoP's concurrent downlink
// legs (spec.limits.maxDownlinkStreams) once, on the PoP's link-home pod,
// however the UDP load balancer spreads the legs over the pods.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// capFleet is three home pods with the PoP linked on pod 0 and n home
// broadcasts.
func capFleet(t *testing.T, n int) (*HomeFleet, *PoP, []string) {
	t.Helper()
	f, pop := StartHomeAndPop(t, 3)
	f.Route(0)
	pairLinked(t, f, pop)
	if f.LinkHolder(popName) != f.Pods[0] {
		t.Fatal("the link is not on pod 0")
	}
	var ids []string
	for range n {
		_, id := homeBroadcast(t, f, f.Pods[0])
		ids = append(ids, id)
	}
	return f, pop, ids
}

// The 21st distinct broadcast is refused although no single pod serves 20
// legs: the count is the link-home pod's, fleet-wide. A broadcast already
// pulled through one pod still fits on another (it counts once), and a
// leg's end gives its slot back.
func TestDownlinkLegCapIsFleetWide(t *testing.T) {
	max := pops.DefaultMaxDownlinkStreams
	f, pop, ids := capFleet(t, max+1)
	home := f.Pods[0]

	legs := map[string]*webtransport.Session{}
	for i, id := range ids[:max] {
		sess, status, reason := rawLeg(t, f, f.Pods[i%3], pop, id, nil)
		if status != http.StatusOK {
			t.Fatalf("leg %d on pod %d: %d %q", i, i%3, status, reason)
		}
		legs[id] = sess
	}
	for i, p := range f.Pods {
		if got := p.Server.PopLegs(popName); got >= max {
			t.Fatalf("pod %d serves %d legs: the test must spread them", i, got)
		}
	}
	for _, i := range []int{0, 1, 2} {
		if _, status, reason := rawLeg(t, f, f.Pods[i], pop, ids[max], nil); status != http.StatusTooManyRequests || reason != "downlink-limit" {
			t.Fatalf("the 21st broadcast on pod %d: %d %q, want 429 downlink-limit", i, status, reason)
		}
	}
	if got := home.Server.PopLegSlots(popName); got != max {
		t.Fatalf("fleet-wide slots at the link home = %d, want %d", got, max)
	}
	// ids[1] was pulled through pod 1; a second leg on pod 2 is the same
	// broadcast and fits.
	if _, status, reason := rawLeg(t, f, f.Pods[2], pop, ids[1], nil); status != http.StatusOK {
		t.Fatalf("a second leg of a pulled broadcast: %d %q", status, reason)
	}
	if got := home.Server.PopLegSlots(popName); got != max {
		t.Fatalf("slots after a duplicate leg = %d, want %d", got, max)
	}

	// The leg on pod 2 for ids[2] ends: its slot goes back.
	legs[ids[2]].CloseWithError(0, "")
	WaitFor(t, "the ended leg's slot released", func() bool { return home.Server.PopLegSlots(popName) == max-1 })
	if _, status, reason := rawLeg(t, f, f.Pods[1], pop, ids[max], nil); status != http.StatusOK {
		t.Fatalf("the 21st broadcast after a release: %d %q", status, reason)
	}
}

// No link, no leg: a pod that cannot reach the count refuses (fail
// closed), with the cap's own 429.
func TestDownlinkLegCapFailsClosedWithoutALink(t *testing.T) {
	f, pop, ids := capFleet(t, 1)
	pop.Stop(t)
	WaitFor(t, "the link gone from the fleet", func() bool {
		for _, p := range f.Pods {
			if _, _, ok := p.Manager.LinkHolder(popName); ok {
				return false
			}
		}
		return true
	})
	for i, p := range f.Pods {
		if _, status, reason := rawLeg(t, f, p, pop, ids[0], nil); status != http.StatusTooManyRequests || reason != "downlink-limit" {
			t.Fatalf("pod %d without a link: %d %q, want 429 downlink-limit", i, status, reason)
		}
	}
}

// A pod that dies without closing (no CONNECTION_CLOSE ever arrives) does
// not hold its slots forever: they are tied to its /internal/pop session,
// which the link home ends at the QUIC idle timeout.
func TestDownlinkLegSlotsOfACrashedPodExpire(t *testing.T) {
	f, _, ids := capFleet(t, 1)
	home := f.Pods[0]
	_, gen, ok := home.Manager.LinkHolder(popName)
	if !ok {
		t.Fatal("no link")
	}
	conn := crashablePodConn(t)
	path := fmt.Sprintf("/internal/pop/%s?leg=%s&gen=%d&max=%d&proto=%d&psk=%s",
		popName, ids[0], gen, pops.DefaultMaxDownlinkStreams, wire.Version, HarnessPSK)
	d := &webtransport.Transport{
		TLSClientConfig: f.ClientTLS,
		QUICConfig: &quic.Config{
			EnableDatagrams: true, EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout: time.Second, KeepAlivePeriod: 200 * time.Millisecond,
		},
		DialAddr: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			ua, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			return (&quic.Transport{Conn: conn}).DialEarly(ctx, ua, tlsCfg, cfg)
		},
	}
	t.Cleanup(func() { d.Close() })
	rsp, _, err := d.Dial(pairCtx(t), "https://"+home.Addr()+path, http.Header{"Origin": {"gawk-server://native-internal-edge"}})
	if err != nil {
		status := 0
		if rsp != nil {
			status = rsp.StatusCode
		}
		t.Fatalf("slot dial: status %d: %v", status, err)
	}
	WaitFor(t, "the slot held", func() bool { return home.Server.PopLegSlots(popName) == 1 })
	// Kept alive, it stays held well past the idle timeout.
	time.Sleep(2 * time.Second)
	if got := home.Server.PopLegSlots(popName); got != 1 {
		t.Fatalf("a live holder's slot: %d, want 1", got)
	}
	conn.crash()
	WaitFor(t, "the crashed pod's slot expired", func() bool { return home.Server.PopLegSlots(popName) == 0 })
}

// The link moves (its pod drains, the PoP relinks elsewhere): the legs on
// the surviving pods re-acquire their slots at the new link home, so the
// new count starts from the legs that are live, not from zero.
func TestDownlinkLegSlotsFollowTheLink(t *testing.T) {
	f, pop, ids := capFleet(t, 4)
	for i, id := range ids {
		if _, status, reason := rawLeg(t, f, f.Pods[1+i%2], pop, id, nil); status != http.StatusOK {
			t.Fatalf("leg %d: %d %q", i, status, reason)
		}
	}
	if got := f.Pods[0].Server.PopLegSlots(popName); got != len(ids) {
		t.Fatalf("slots at pod 0 = %d, want %d", got, len(ids))
	}
	f.Route(1)
	f.Pods[0].Drain(t)
	WaitFor(t, "the PoP relinked on pod 1", func() bool { return f.LinkHolder(popName) == f.Pods[1] })
	WaitFor(t, "the live legs re-counted at the new link home", func() bool {
		return f.Pods[1].Server.PopLegSlots(popName) == len(ids)
	})
}

// crashConn is a UDP socket that can go silent at once, as a pod whose
// process died: nothing more is sent, nothing more is read. It has only
// the net.PacketConn methods, so quic-go cannot go around them.
type crashConn struct {
	c    *net.UDPConn
	dead atomic.Bool
}

func crashablePodConn(t *testing.T) *crashConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &crashConn{c: c}
}

func (c *crashConn) crash() { c.dead.Store(true) }

func (c *crashConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.dead.Load() {
		return len(b), nil
	}
	return c.c.WriteTo(b, addr)
}

func (c *crashConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.c.ReadFrom(b)
		if err != nil || !c.dead.Load() {
			return n, addr, err
		}
	}
}

func (c *crashConn) Close() error                       { return c.c.Close() }
func (c *crashConn) LocalAddr() net.Addr                { return c.c.LocalAddr() }
func (c *crashConn) SetDeadline(t time.Time) error      { return c.c.SetDeadline(t) }
func (c *crashConn) SetReadDeadline(t time.Time) error  { return c.c.SetReadDeadline(t) }
func (c *crashConn) SetWriteDeadline(t time.Time) error { return c.c.SetWriteDeadline(t) }
