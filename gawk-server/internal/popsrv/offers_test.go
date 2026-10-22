package popsrv

// The PP13 acceptance rows (docs/76 §11): who gets a LocalRelayOffer, what
// it carries, that its handle is the same on every pod and across a home
// restart, and the lr=1 demand deferral.

import (
	"context"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// demandDeferrer restates the interface W3-uplink declares in
// transport/pop_demand.go, so this package proves it implements it without
// referencing a same-wave symbol (contract §4).
type demandDeferrer interface {
	Defer(remote netip.Addr, lr bool) time.Duration
}

var (
	_ transport.Offerer = (*Offers)(nil)
	_ demandDeferrer    = (*Offers)(nil)
)

const (
	offerUID   = "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab"
	offerHashA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	offerHashB = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
)

// linkedFilePop is a Pop the source reports as linked on another pod, with
// the docs/76 §10 example's public addresses.
func linkedFilePop(name, uid string) *pops.Pop {
	p := pops.FilePop{Name: name, UID: uid, DisplayName: "Assembly 2026", CredentialSHA256: pops.CredentialSHA256("c")}.ToPop()
	now := metav1.Now()
	p.Status.Lease = &pops.Lease{Holder: "home-1", Addr: "10.0.0.2:4433", Generation: 1, RenewedAt: &now}
	p.Status.Link = &pops.Link{
		Connected:    true,
		Since:        &now,
		PublicAddrs:  []string{"203.0.113.7", "2001:db8:1:2::/64"},
		LanAddr:      "192.168.1.20",
		PinnedHashes: []string{offerHashA, offerHashB},
	}
	return &p
}

func offersOver(t *testing.T, defer_ time.Duration, ps ...*pops.Pop) (*Offers, *Manager, *fakeSource) {
	t.Helper()
	src := &fakeSource{pops: map[string]*pops.Pop{}}
	for _, p := range ps {
		src.pops[p.Name] = p
	}
	m := New(Options{Pod: "home-0", Source: src, Status: popsrc.NewMemStatus(src), Log: harnessLog(t)})
	t.Cleanup(m.Close)
	return NewOffers(m, OffersOptions{Defer: defer_, Log: harnessLog(t)}), m, src
}

func hashOf(t *testing.T, h string) [32]byte {
	t.Helper()
	var out [32]byte
	b, ok := decodeHash(h)
	if !ok {
		t.Fatalf("bad hash %q", h)
	}
	copy(out[:], b[:])
	return out
}

// IPv4 matches exactly, IPv6 on the /64; everything else gets nothing.
func TestOfferMatchesV4ExactlyAndV6OnTheSlash64(t *testing.T) {
	o, _, _ := offersOver(t, 2*time.Second, linkedFilePop("assembly-2026", offerUID))
	for _, tc := range []struct {
		remote string
		want   bool
	}{
		{"203.0.113.7", true},
		{"::ffff:203.0.113.7", true},
		{"203.0.113.8", false},
		{"198.51.100.7", false},
		{"2001:db8:1:2::1", true},
		{"2001:db8:1:2:a1b2:c3d4:e5f6:7788", true},
		{"2001:db8:1:3::1", false},
		{"2001:db8::1", false},
	} {
		_, ok := o.Offer(netip.MustParseAddr(tc.remote), false)
		if ok != tc.want {
			t.Errorf("Offer(%s) ok = %v, want %v", tc.remote, ok, tc.want)
		}
	}
	if _, ok := o.Offer(netip.Addr{}, true); ok {
		t.Error("an unknown remote address got an offer")
	}
}

// The offer names the PoP by its UID, carries its display name, a
// validity and the pinned IP-literal candidate with both hashes; it
// encodes on the wire.
func TestOfferCarriesTheHandleNameAndPinnedCandidate(t *testing.T) {
	o, _, _ := offersOver(t, 2*time.Second, linkedFilePop("assembly-2026", offerUID))
	got, ok := o.Offer(netip.MustParseAddr("203.0.113.7"), true)
	if !ok {
		t.Fatal("no offer for a matching client")
	}
	if got.Handle != offerUID || got.Name != "Assembly 2026" || got.Validity != uint32(OfferValidity/time.Second) {
		t.Fatalf("offer = %+v", got)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].URL != "https://192.168.1.20:4433" ||
		len(got.Candidates[0].Hashes) != 2 || got.Candidates[0].Hashes[0] != hashOf(t, offerHashA) ||
		got.Candidates[0].Hashes[1] != hashOf(t, offerHashB) {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
	msg, err := wire.AppendLocalRelayOffer(nil, got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := wire.ParseLocalRelayOffer(msg)
	if err != nil || back.Handle != offerUID || len(back.Candidates) != 1 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

// An issued named host comes first, unpinned; the IP literal keeps its
// hashes. A v6 LAN address is bracketed, and a LAN address with a port
// keeps it.
func TestOfferCandidates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*pops.Pop)
		want    []string
		nHashes []int
	}{
		{"named host issued", func(p *pops.Pop) {
			p.Spec.NamedAddress.Enabled = true
			p.Status.TLS = &pops.TLS{Host: "p-7k2m9x.pop.gawk.ioio.fi", Certificate: "chain"}
		}, []string{"https://p-7k2m9x.pop.gawk.ioio.fi:4433", "https://192.168.1.20:4433"}, []int{0, 2}},
		{"named host issued, named address turned off", func(p *pops.Pop) {
			p.Status.TLS = &pops.TLS{Host: "p-7k2m9x.pop.gawk.ioio.fi", Certificate: "chain"}
		}, []string{"https://192.168.1.20:4433"}, []int{2}},
		{"named host assigned, not issued", func(p *pops.Pop) {
			p.Spec.NamedAddress.Enabled = true
			p.Status.TLS = &pops.TLS{Host: "p-7k2m9x.pop.gawk.ioio.fi"}
		}, []string{"https://192.168.1.20:4433"}, []int{2}},
		{"v6 LAN address", func(p *pops.Pop) { p.Status.Link.LanAddr = "fd00::20" },
			[]string{"https://[fd00::20]:4433"}, []int{2}},
		{"LAN address with a port", func(p *pops.Pop) { p.Status.Link.LanAddr = "192.168.1.20:4434" },
			[]string{"https://192.168.1.20:4434"}, []int{2}},
		{"one hash, one garbage", func(p *pops.Pop) { p.Status.Link.PinnedHashes = []string{offerHashA, "zz"} },
			[]string{"https://192.168.1.20:4433"}, []int{1}},
		{"three hashes keep two", func(p *pops.Pop) {
			p.Status.Link.PinnedHashes = []string{offerHashA, offerHashB, strings.Repeat("ab", 32)}
		}, []string{"https://192.168.1.20:4433"}, []int{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := linkedFilePop("assembly-2026", offerUID)
			tc.mutate(p)
			o, _, _ := offersOver(t, 0, p)
			got, ok := o.Offer(netip.MustParseAddr("203.0.113.7"), false)
			if !ok {
				t.Fatal("no offer")
			}
			if len(got.Candidates) != len(tc.want) {
				t.Fatalf("candidates = %+v, want %v", got.Candidates, tc.want)
			}
			for i, c := range got.Candidates {
				if c.URL != tc.want[i] || len(c.Hashes) != tc.nHashes[i] {
					t.Errorf("candidate %d = %s with %d hashes, want %s with %d", i, c.URL, len(c.Hashes), tc.want[i], tc.nHashes[i])
				}
			}
			if _, err := wire.AppendLocalRelayOffer(nil, got); err != nil {
				t.Fatalf("encode: %v", err)
			}
		})
	}
}

// autoSwitch false, a revoked PoP, a PoP that is not linked and one with
// nowhere to send a guest never offer.
func TestNoOfferWithoutAReachableAutoSwitchingLinkedPoP(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name   string
		mutate func(*pops.Pop)
	}{
		{"autoSwitch false", func(p *pops.Pop) { p.Spec.Guests.AutoSwitch = &off }},
		{"revoked", func(p *pops.Pop) { p.Spec.Revoked = true }},
		{"link down", func(p *pops.Pop) { p.Status.Link.Connected = false }},
		{"no lease holder", func(p *pops.Pop) { p.Status.Lease.Holder = "" }},
		{"no LAN address and no name", func(p *pops.Pop) { p.Status.Link.LanAddr = "" }},
		{"LAN address unparseable", func(p *pops.Pop) { p.Status.Link.LanAddr = "router.local" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := linkedFilePop("assembly-2026", offerUID)
			tc.mutate(p)
			o, _, _ := offersOver(t, 2*time.Second, p)
			if got, ok := o.Offer(netip.MustParseAddr("203.0.113.7"), true); ok {
				t.Fatalf("offer = %+v", got)
			}
			if d := o.Defer(netip.MustParseAddr("203.0.113.7"), true); d != 0 {
				t.Fatalf("Defer = %v for a session that gets no offer", d)
			}
		})
	}
}

// A display name past the wire's 64 bytes is cut on a rune boundary, not
// dropped with the whole offer.
func TestOfferTruncatesALongDisplayName(t *testing.T) {
	p := linkedFilePop("assembly-2026", offerUID)
	p.Spec.DisplayName = strings.Repeat("ä", 40) // 80 bytes
	o, _, _ := offersOver(t, 0, p)
	got, ok := o.Offer(netip.MustParseAddr("203.0.113.7"), false)
	if !ok {
		t.Fatal("no offer")
	}
	if len(got.Name) != 64 || got.Name != strings.Repeat("ä", 32) {
		t.Fatalf("name = %q (%d bytes)", got.Name, len(got.Name))
	}
}

// The handle is the Pop's metadata.uid: identical on every pod and after a
// home restart. Each Manager here has a fresh hub Registry, whose stats
// key (no -stats-key) is per-process random, so nothing keyed by it could
// pass.
func TestTheHandleIsIdenticalAcrossPodsAndARestart(t *testing.T) {
	p := linkedFilePop("assembly-2026", offerUID)
	src := &fakeSource{pops: map[string]*pops.Pop{p.Name: p}}
	var handles []string
	var obfuscated []string
	for _, pod := range []string{"home-0", "home-1", "home-0-restarted"} {
		r := hub.NewRegistry(harnessLog(t), hub.Options{})
		m := New(Options{Pod: pod, Source: src, Status: popsrc.NewMemStatus(src), Registry: r, Log: harnessLog(t)})
		got, ok := NewOffers(m, OffersOptions{}).Offer(netip.MustParseAddr("203.0.113.7"), false)
		m.Close()
		if !ok {
			t.Fatalf("%s: no offer", pod)
		}
		handles = append(handles, got.Handle)
		obfuscated = append(obfuscated, r.ObfuscateID(p.Name))
	}
	for i, h := range handles {
		if h != offerUID {
			t.Fatalf("handle %d = %q, want the Pop UID %q", i, h, offerUID)
		}
	}
	if obfuscated[0] == obfuscated[1] {
		t.Fatal("the registries share a stats key; the test proves nothing")
	}
}

// A matching lr=1 session's demand is held for -pop-offer-defer; one
// without lr, or without a match, is not held at all.
func TestDeferHoldsOnlyAMatchingFollower(t *testing.T) {
	o, _, _ := offersOver(t, 1500*time.Millisecond, linkedFilePop("assembly-2026", offerUID))
	for _, tc := range []struct {
		remote string
		lr     bool
		want   time.Duration
	}{
		{"203.0.113.7", true, 1500 * time.Millisecond},
		{"2001:db8:1:2::99", true, 1500 * time.Millisecond},
		{"203.0.113.7", false, 0},
		{"198.51.100.7", true, 0},
		{"198.51.100.7", false, 0},
	} {
		if got := o.Defer(netip.MustParseAddr(tc.remote), tc.lr); got != tc.want {
			t.Errorf("Defer(%s, lr=%v) = %v, want %v", tc.remote, tc.lr, got, tc.want)
		}
	}
	if got := o.Defer(netip.Addr{}, true); got != 0 {
		t.Errorf("Defer(unknown remote) = %v", got)
	}
	// A zero defer (-pop-offer-defer 0s) turns the hold off.
	o0, _, _ := offersOver(t, 0, linkedFilePop("assembly-2026", offerUID))
	if got := o0.Defer(netip.MustParseAddr("203.0.113.7"), true); got != 0 {
		t.Errorf("Defer with -pop-offer-defer 0 = %v", got)
	}
}

// Offers and holds are counted per PoP and in the metrics.
func TestOffersAreCounted(t *testing.T) {
	p := linkedFilePop("assembly-2026", offerUID)
	src := &fakeSource{pops: map[string]*pops.Pop{p.Name: p}}
	m := New(Options{Source: src, Status: popsrc.NewMemStatus(src), Log: harnessLog(t)})
	defer m.Close()
	reg := prometheus.NewRegistry()
	o := NewOffers(m, OffersOptions{Defer: time.Second, Prom: reg})
	o.Offer(netip.MustParseAddr("203.0.113.7"), true)
	o.Offer(netip.MustParseAddr("203.0.113.7"), false)
	o.Offer(netip.MustParseAddr("198.51.100.7"), true)
	o.Defer(netip.MustParseAddr("203.0.113.7"), true)
	o.Defer(netip.MustParseAddr("203.0.113.7"), false)
	if n := o.Sent("assembly-2026"); n != 2 {
		t.Fatalf("Sent = %d, want 2", n)
	}
	if n := testutil.ToFloat64(o.metrics.offers); n != 2 {
		t.Fatalf("gawk_pop_offers_total = %v, want 2", n)
	}
	if n := testutil.ToFloat64(o.metrics.defers); n != 1 {
		t.Fatalf("gawk_pop_offer_defers_total = %v, want 1", n)
	}
	// A second Offers on the same registry (a re-wire) must not panic.
	NewOffers(m, OffersOptions{Prom: reg})
}

// In-process, against the harness fleet: a guest behind the PoP's public
// address (127.0.0.1 here) gets the offer on /echo from either pod, with
// the Pop UID as the handle and the PoP's LAN address and rolling hashes as
// the candidate; the link-home pod's /internal/admin/pops row counts it;
// autoSwitch false stops it.
func TestAMatchingGuestGetsTheOfferFromEveryPod(t *testing.T) {
	rolling, err := tlsutil.NewRolling(tlsutil.RollingOptions{Dir: t.TempDir(), Hosts: func() []string { return []string{"localhost"} }})
	if err != nil {
		t.Fatal(err)
	}
	f, pop := StartHomeAndPop(t, 2, WithPopClient(func(o *popclient.Options) {
		o.LAN = lanaddr.NewWatcher("192.168.1.20", time.Hour)
		o.Rolling = rolling
	}))
	offers := make([]*Offers, len(f.Pods))
	for i, pod := range f.Pods {
		offers[i] = NewOffers(pod.Manager, OffersOptions{Defer: 2 * time.Second})
		pod.Server.SetOfferer(offers[i])
	}
	f.Route(1)
	pairLinked(t, f, pop)
	uid := string(f.GetPop(t, popName).UID)
	cur, next := rolling.Hashes()
	loopback := netip.MustParseAddr("127.0.0.1")
	WaitFor(t, "home-0 sees the link in status", func() bool {
		_, ok := offers[0].Offer(loopback, false)
		return ok
	})

	for i, pod := range f.Pods {
		got, ok := echoOffer(t, f, pod)
		if !ok {
			t.Fatalf("%s sent no offer", pod.Name)
		}
		if got.Handle != uid || got.Name != "Assembly 2026" || len(got.Candidates) != 1 ||
			got.Candidates[0].URL != "https://192.168.1.20:4433" || len(got.Candidates[0].Hashes) != 2 ||
			got.Candidates[0].Hashes[0] != hashOf(t, cur) || got.Candidates[0].Hashes[1] != hashOf(t, next) {
			t.Fatalf("pod %d offer = %+v", i, got)
		}
	}
	WaitFor(t, "the link-home pod counts its offers", func() bool {
		rows := f.Pods[1].Manager.AdminPops()
		return len(rows) == 1 && rows[0].OffersSent >= 1
	})

	off := false
	f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Guests.AutoSwitch = &off })
	for i := range f.Pods {
		WaitFor(t, "autoSwitch false reaches the matcher", func() bool {
			_, ok := offers[i].Offer(loopback, false)
			return !ok
		})
	}
	if got, ok := echoOffer(t, f, f.Pods[1]); ok {
		t.Fatalf("offer with autoSwitch false: %+v", got)
	}
}

// echoOffer opens a guest /echo session on a home pod and returns its
// LocalRelayOffer, if one arrives within a second of the other notices.
func echoOffer(t *testing.T, f *HomeFleet, pod *HomePod) (wire.LocalRelayOffer, bool) {
	t.Helper()
	sess := clientDial(t, f, "https://"+pod.Addr()+"/echo?lr=1")
	defer sess.CloseWithError(0, "")
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		st, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			return wire.LocalRelayOffer{}, false
		}
		data, _ := io.ReadAll(st)
		if len(data) >= 2 && data[1] == wire.TypeLocalRelayOffer {
			offer, err := wire.ParseLocalRelayOffer(data)
			if err != nil {
				t.Fatal(err)
			}
			return offer, true
		}
	}
}
