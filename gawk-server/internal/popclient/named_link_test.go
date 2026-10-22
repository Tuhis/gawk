package popclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/lanaddr"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

func healthKeys(lines []adminapi.HealthLine) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		out[l.Key] = l.OK
	}
	return out
}

// docs/76 items 38-39 over a real link: home's config names the host, the
// CSR goes up the link, home's certificate is installed, and the named
// host reaches home in a fresh hello and the health record.
func TestNamedOverTheLink(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	withCredential(t, o, "")
	c := New(o)
	n := NewNamed(c, NamedOptions{StateDir: t.TempDir(), LAN: func() netip.Addr { return lan },
		System: fakeResolver{testHost: {lan}}, Public: fakeResolver{}, Log: discard()})
	run(t, c)
	l := f.nextLink(t)
	if l.hello.NamedHost != "" {
		t.Fatalf("hello named host %q before a certificate", l.hello.NamedHost)
	}
	l.send(poplink.Config{PopName: "p", NamedHost: testHost})
	csr := expect[*poplink.CSR](t, l)
	if _, err := pops.ValidateCSR(csr.PEM, testHost); err != nil {
		t.Fatalf("CSR: %v", err)
	}
	waiting := expect[*poplink.Health](t, l)
	if ok, has := healthKeys(waiting.Lines)["named-certificate"]; !has || ok {
		t.Fatalf("health before issue %+v", waiting.Lines)
	}
	l.send(poplink.Certificate{ChainPEM: newCA(t).sign(t, csr.PEM, 1, time.Now()), ForCSRSHA256: pops.CSRSHA256(csr.PEM)})
	if h := expect[*poplink.Hello](t, l); h.NamedHost != testHost {
		t.Fatalf("refreshed hello named host %q", h.NamedHost)
	}
	if n.Host() != testHost {
		t.Fatalf("Host = %q", n.Host())
	}
	if ok := healthKeys(n.Health())["named-certificate"]; !ok {
		t.Fatalf("Health after issue %+v", n.Health())
	}
	// A named tier built on a client already linked follows its config.
	n2 := NewNamed(c, NamedOptions{StateDir: t.TempDir(), LAN: func() netip.Addr { return lan },
		System: fakeResolver{}, Public: fakeResolver{}, Log: discard()})
	if csr2 := expect[*poplink.CSR](t, l); csr2.PEM == csr.PEM {
		t.Fatal("the second tier resent the first one's CSR")
	}
	if !strings.Contains(n2.String(), "dns=unchecked") {
		t.Fatalf("String = %s", n2.String())
	}
}

// The health lines: nothing with no host; waiting while home issues; the
// certificate and the DNS self-check's verdict once issued.
func TestNamedHealthLines(t *testing.T) {
	clk := &clock{t: time.Now()}
	n := newTestNamed(t, t.TempDir(), clk, &sent{})
	if h := n.Health(); h != nil {
		t.Fatalf("Health with no host = %+v", h)
	}
	other := netip.MustParseAddr("192.168.1.99")
	for _, tc := range []struct {
		name           string
		system, public Resolver
		lan            netip.Addr
		dnsOK          bool
		detail         string
	}{
		{"ok", fakeResolver{testHost: {lan}}, fakeResolver{}, lan, true, "resolves " + testHost},
		{"filtered", fakeResolver{}, fakeResolver{testHost: {lan}}, lan, false, DNSFilteredNotice},
		{"pending", fakeResolver{testHost: {other}}, fakeResolver{}, lan, false, "does not point at this machine"},
		{"no-lan", fakeResolver{}, fakeResolver{}, netip.Addr{}, false, "no LAN address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := issued(t, func(o *NamedOptions) {
				o.System, o.Public = tc.system, tc.public
				a := tc.lan
				o.LAN = func() netip.Addr { return a }
			})
			if h := healthKeys(n.Health()); !h["named-certificate"] {
				t.Fatalf("certificate line %+v", n.Health())
			}
			if _, has := healthKeys(n.Health())["dns"]; has {
				t.Fatal("a dns line before any check")
			}
			n.CheckDNS(t.Context())
			var dns *adminapi.HealthLine
			for _, l := range n.Health() {
				if l.Key == "dns" {
					dns = &l
				}
			}
			if dns == nil || dns.OK != tc.dnsOK || !strings.Contains(dns.Detail, tc.detail) {
				t.Fatalf("dns line %+v", dns)
			}
			if !strings.Contains(n.String(), testHost) || !strings.Contains(n.String(), "dns="+tc.name) {
				t.Fatalf("String = %s", n.String())
			}
		})
	}
}

// Run checks DNS at start and again when the LAN address changes, and
// returns nil when its context ends.
func TestNamedRunRechecksOnALANChange(t *testing.T) {
	w := lanaddr.NewWatcher("192.168.1.20", 0)
	c := New(Options{Log: discard(), LAN: w})
	clk := &clock{t: time.Now()}
	s := &sent{}
	n := NewNamed(c, NamedOptions{StateDir: t.TempDir(), Now: clk.now, Send: s.send,
		System: fakeResolver{testHost: {lan}}, Public: fakeResolver{}, Log: discard(),
		CheckEvery: time.Hour, RenewEvery: 10 * time.Millisecond})
	n.onConfig(testHost)
	csr := s.csrs()[0]
	n.onCertificate(poplink.Certificate{ChainPEM: newCA(t).sign(t, csr, 1, clk.now()), ForCSRSHA256: pops.CSRSHA256(csr)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	waitUntil(t, "the start-up check", func() bool { return n.DNSCheck() == DNSOK })
	w.SetOverride(netip.MustParseAddr("192.168.1.77"))
	waitUntil(t, "the recheck on the LAN change", func() bool { return n.DNSCheck() == DNSPending })
	// The renewal timer runs without asking again: the certificate is fresh.
	time.Sleep(50 * time.Millisecond)
	if got := len(s.csrs()); got != 1 {
		t.Fatalf("%d CSRs with a fresh certificate", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Run did not return")
	}
}

// The default resolvers: the public one speaks plain DNS through Go's
// resolver whatever the system's is.
func TestNamedDefaultResolvers(t *testing.T) {
	r, ok := PublicResolver().(*net.Resolver)
	if !ok || !r.PreferGo || r.Dial == nil {
		t.Fatalf("PublicResolver = %#v", PublicResolver())
	}
	n := NewNamed(nil, NamedOptions{StateDir: t.TempDir(), Log: discard()})
	if n.opts.System != net.DefaultResolver {
		t.Fatal("System does not default to net.DefaultResolver")
	}
	if _, ok := n.opts.Public.(*net.Resolver); !ok {
		t.Fatal("Public does not default to PublicResolver")
	}
	if n.opts.LAN().IsValid() {
		t.Fatal("a LAN address with no client")
	}
	if got := n.CheckDNS(t.Context()); got != DNSUnchecked {
		t.Fatalf("CheckDNS with no host = %s", got)
	}
}

// The downlink's leg dial through Options.Dial: a media session is handed
// over, anything else is closed and refused, and a refusal passes home's
// response through.
func TestDownlinkLegDial(t *testing.T) {
	var answer func() (Session, *http.Response, error)
	c := New(Options{Log: discard(), Dial: func(context.Context, string, string, http.Header) (Session, *http.Response, error) {
		return answer()
	}})
	c.stored = &storedCredential{Endpoint: "home.example:4433", Credential: "c"}
	c.setState(func(s *LinkState) { s.Header, s.Config = HeaderLinked, &poplink.Config{PopName: "p"} })
	tgt, ok := c.PopDownlink()
	if !ok || tgt.Dial == nil {
		t.Fatalf("PopDownlink = %+v %v", tgt, ok)
	}
	leg := newFakeLeg()
	answer = func() (Session, *http.Response, error) { return leg, nil, nil }
	got, _, err := tgt.Dial(t.Context(), "https://home.example:4433/pop/subscribe/ABC234", http.Header{})
	if err != nil || got != transport.PopLegSession(leg) {
		t.Fatalf("media leg = %v, %v", got, err)
	}
	pop, _ := poplink.Pipe(popAddr, homeAddr)
	answer = func() (Session, *http.Response, error) { return pop, nil, nil }
	if _, _, err := tgt.Dial(t.Context(), "https://x/pop/subscribe/ABC234", nil); !errors.Is(err, errNotMediaSession) {
		t.Fatalf("non-media leg = %v", err)
	}
	if poplink.SessionEndErr(pop) == nil {
		t.Fatal("the non-media session was left open")
	}
	rsp := &http.Response{StatusCode: http.StatusTooManyRequests}
	answer = func() (Session, *http.Response, error) { return nil, rsp, errors.New("cap") }
	if _, r, err := tgt.Dial(t.Context(), "https://x/pop/subscribe/ABC234", nil); err == nil || r != rsp {
		t.Fatalf("refused leg = %v, %v", r, err)
	}
	// A relay server that runs no downlink reports no use.
	_, srv := relayParts(t)
	c2 := New(Options{Log: discard(), Server: srv})
	if u := c2.DownlinkUse(); u != (transport.PopDownlinkUse{}) {
		t.Fatalf("DownlinkUse = %+v", u)
	}
}
