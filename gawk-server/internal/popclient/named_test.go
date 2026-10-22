package popclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

const testHost = "p-7k2m9x.pop.gawk.test"

var lan = netip.MustParseAddr("192.168.1.20")

type sent struct {
	mu   sync.Mutex
	recs []any
}

func (s *sent) send(rec any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return nil
}

func (s *sent) csrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.recs {
		if c, ok := r.(poplink.CSR); ok {
			out = append(out, c.PEM)
		}
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ACME"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{key: key, cert: cert, pool: pool}
}

// sign issues a 6-day leaf (the shortlived profile) for csrPEM from nb.
func (ca *testCA) sign(t *testing.T, csrPEM string, serial int64, nb time.Time) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: csr.DNSNames,
		NotBefore: nb, NotAfter: nb.Add(6 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
}

func newTestNamed(t *testing.T, dir string, clk *clock, s *sent, mod ...func(*NamedOptions)) *Named {
	t.Helper()
	o := NamedOptions{StateDir: dir, Now: clk.now, Send: s.send, LAN: func() netip.Addr { return lan },
		System: fakeResolver{}, Public: fakeResolver{}, Log: slog.New(slog.DiscardHandler)}
	for _, m := range mod {
		m(&o)
	}
	return NewNamed(nil, o)
}

func serial(n *Named) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cert == nil {
		return 0
	}
	return n.cert.Leaf.SerialNumber.Int64()
}

// docs/76 §11 PP12: the PoP's CSR names the host it got in config, and
// only that host; the key is in the state directory and nowhere else.
func TestNamedCSRNamesTheAssignedHost(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Now()}
	s := &sent{}
	n := newTestNamed(t, dir, clk, s)
	n.onConfig("")
	if len(s.csrs()) != 0 {
		t.Fatal("a CSR was sent with no host assigned")
	}
	n.onConfig(strings.ToUpper(testHost) + ".")
	csrs := s.csrs()
	if len(csrs) != 1 {
		t.Fatalf("CSRs sent = %d, want 1", len(csrs))
	}
	csr, err := pops.ValidateCSR(csrs[0], testHost)
	if err != nil {
		t.Fatalf("the PoP's own CSR is not valid for its host: %v", err)
	}
	if csr.DNSNames[0] != testHost {
		t.Fatalf("SAN = %v", csr.DNSNames)
	}
	for _, r := range s.recs {
		if strings.Contains(fmt.Sprint(r), "PRIVATE KEY") {
			t.Fatal("a record carried the private key")
		}
	}
	// A config for the same host (every link-up) re-sends the same CSR.
	n.onConfig(testHost)
	if got := s.csrs(); len(got) != 2 || got[1] != csrs[0] {
		t.Fatalf("relink resend: %d CSRs", len(got))
	}
}

// The PoP never installs the previous chain for a renewal, nor a chain for
// another key; it renews at half-life, once.
func TestNamedRenewsAtHalfLifeAndIgnoresThePreviousChain(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Truncate(time.Second)
	clk := &clock{t: start}
	s := &sent{}
	ca := newCA(t)
	n := newTestNamed(t, dir, clk, s)
	n.onConfig(testHost)
	csr1 := s.csrs()[0]
	chain1 := ca.sign(t, csr1, 1, start)
	n.onCertificate(poplink.Certificate{ChainPEM: chain1, ForCSRSHA256: pops.CSRSHA256(csr1)})
	if n.Host() != testHost || serial(n) != 1 {
		t.Fatalf("after issuance host=%q serial=%d", n.Host(), serial(n))
	}

	// Before half-life (3 days of 6): nothing.
	clk.add(72*time.Hour - time.Minute)
	n.request(false)
	if len(s.csrs()) != 1 {
		t.Fatal("renewed before half-life")
	}
	clk.add(2 * time.Minute)
	n.request(false)
	n.request(false)
	csrs := s.csrs()
	if len(csrs) != 2 {
		t.Fatalf("CSRs after half-life = %d, want exactly 2", len(csrs))
	}
	csr2 := csrs[1]
	if csr2 == csr1 || pops.CSRSHA256(csr2) == pops.CSRSHA256(csr1) {
		t.Fatal("the renewal reused the CSR")
	}
	if n.Host() != testHost || serial(n) != 1 {
		t.Fatal("the serving certificate was dropped before the renewal arrived")
	}

	// The previous chain, labelled for the previous CSR, is ignored.
	n.onCertificate(poplink.Certificate{ChainPEM: chain1, ForCSRSHA256: pops.CSRSHA256(csr1)})
	// The previous chain labelled for the new CSR does not carry its key.
	n.onCertificate(poplink.Certificate{ChainPEM: chain1, ForCSRSHA256: pops.CSRSHA256(csr2)})
	// A chain for another host's request.
	other := ca.sign(t, csrFor(t, "p-zzzzzz.pop.gawk.test"), 7, clk.now())
	n.onCertificate(poplink.Certificate{ChainPEM: other, ForCSRSHA256: pops.CSRSHA256(csr2)})
	if serial(n) != 1 {
		t.Fatalf("serial = %d after wrong chains, want 1", serial(n))
	}
	chain2 := ca.sign(t, csr2, 2, clk.now())
	n.onCertificate(poplink.Certificate{ChainPEM: chain2, ForCSRSHA256: pops.CSRSHA256(csr2)})
	if serial(n) != 2 {
		t.Fatalf("serial = %d, want the renewal (2)", serial(n))
	}
	n.request(false)
	if len(s.csrs()) != 2 {
		t.Fatal("a fresh certificate asked again")
	}
}

func csrFor(t *testing.T, host string) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{host}}, key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// An unanswered CSR is re-sent every ResendEvery, and a restart keeps the
// pending request and the serving pair.
func TestNamedStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Now()}
	s := &sent{}
	ca := newCA(t)
	n := newTestNamed(t, dir, clk, s, func(o *NamedOptions) { o.ResendEvery = 10 * time.Minute })
	n.onConfig(testHost)
	clk.add(5 * time.Minute)
	n.request(false)
	if len(s.csrs()) != 1 {
		t.Fatal("re-sent before ResendEvery")
	}
	clk.add(6 * time.Minute)
	n.request(false)
	if got := s.csrs(); len(got) != 2 || got[0] != got[1] {
		t.Fatalf("resend: %d CSRs", len(got))
	}
	csr := s.csrs()[0]

	// Restart with the request pending: the answer still installs.
	s2 := &sent{}
	n2 := newTestNamed(t, dir, clk, s2)
	n2.onCertificate(poplink.Certificate{ChainPEM: ca.sign(t, csr, 3, clk.now()), ForCSRSHA256: pops.CSRSHA256(csr)})
	if n2.Host() != testHost {
		t.Fatal("a restarted PoP lost its pending request")
	}
	// Restart with the pair: served at once, before any link.
	n3 := newTestNamed(t, dir, clk, &sent{})
	if n3.Host() != testHost || serial(n3) != 3 {
		t.Fatalf("restart host=%q serial=%d", n3.Host(), serial(n3))
	}
	if c := n3.Certificate(&tls.ClientHelloInfo{ServerName: testHost}); c == nil {
		t.Fatal("no certificate for the named SNI")
	}
	for _, sni := range []string{"", "192.168.1.20", "p-other.pop.gawk.test"} {
		if c := n3.Certificate(&tls.ClientHelloInfo{ServerName: sni}); c != nil {
			t.Fatalf("named certificate served for SNI %q", sni)
		}
	}
	// Home stops assigning the name: no longer served.
	n3.onConfig("")
	if n3.Host() != "" || n3.Certificate(&tls.ClientHelloInfo{ServerName: testHost}) != nil {
		t.Fatal("served a name home no longer assigns")
	}
}

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

type errResolver struct{}

func (errResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("network unreachable")
}

func issued(t *testing.T, mod ...func(*NamedOptions)) *Named {
	t.Helper()
	clk := &clock{t: time.Now()}
	s := &sent{}
	n := newTestNamed(t, t.TempDir(), clk, s, mod...)
	n.onConfig(testHost)
	csr := s.csrs()[0]
	n.onCertificate(poplink.Certificate{ChainPEM: newCA(t).sign(t, csr, 1, clk.now()), ForCSRSHA256: pops.CSRSHA256(csr)})
	if n.Host() != testHost {
		t.Fatal("not issued")
	}
	return n
}

// docs/76 §11 PP12 / item 40: the rebinding self-check with fake resolvers.
func TestNamedRebindingSelfCheck(t *testing.T) {
	other := netip.MustParseAddr("192.168.1.99")
	cases := []struct {
		name           string
		system, public Resolver
		lan            netip.Addr
		want           string
	}{
		{"venue resolves it", fakeResolver{testHost: {lan}}, fakeResolver{testHost: {lan}}, lan, DNSOK},
		{"venue resolves it, no internet", fakeResolver{testHost: {lan}}, errResolver{}, lan, DNSOK},
		{"venue filters private answers", fakeResolver{}, fakeResolver{testHost: {lan}}, lan, DNSFiltered},
		{"venue answers a stale address", fakeResolver{testHost: {other}}, fakeResolver{testHost: {lan}}, lan, DNSFiltered},
		{"not published yet", fakeResolver{}, fakeResolver{}, lan, DNSPending},
		{"published for the old address", fakeResolver{testHost: {other}}, fakeResolver{testHost: {other}}, lan, DNSPending},
		{"no LAN address", fakeResolver{testHost: {lan}}, fakeResolver{testHost: {lan}}, netip.Addr{}, DNSNoLAN},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := issued(t, func(o *NamedOptions) {
				o.System, o.Public = c.system, c.public
				l := c.lan
				o.LAN = func() netip.Addr { return l }
			})
			if n.GuestHost() != "" {
				t.Fatal("guests were sent to the name before any check")
			}
			if got := n.CheckDNS(context.Background()); got != c.want {
				t.Fatalf("CheckDNS = %s, want %s", got, c.want)
			}
			wantGuest := ""
			if c.want == DNSOK {
				wantGuest = testHost
			}
			if n.GuestHost() != wantGuest {
				t.Fatalf("GuestHost = %q, want %q", n.GuestHost(), wantGuest)
			}
			if (n.Notice() == DNSFilteredNotice) != (c.want == DNSFiltered) {
				t.Fatalf("Notice = %q", n.Notice())
			}
		})
	}
}

func TestNamedConfigJSGolden(t *testing.T) {
	got := string(NamedConfigJS(testHost, 4433))
	want := "// Generated by gawk-relay for its named address (docs/76 §6.11 item 39).\n" +
		`window.__GAWK_CONFIG__ = {"relayUrl":"https://p-7k2m9x.pop.gawk.test:4433","requirePublishSecret":false};` + "\n"
	if got != want {
		t.Fatalf("config.js =\n%s\nwant\n%s", got, want)
	}
	if got := string(NamedConfigJS("", 0)); !strings.Contains(got, `{"requirePublishSecret":false}`) {
		t.Fatalf("config.js with no host = %s", got)
	}
}

func TestNamedHandlerServesTheAppAndConfig(t *testing.T) {
	n := issued(t, func(o *NamedOptions) {
		o.App = fstest.MapFS{"index.html": {Data: []byte("<html>gawk</html>")}, "assets/a.js": {Data: []byte("js")}}
		o.RelayPort = 4433
	})
	h := n.Handler()
	for path, want := range map[string]string{"/": "<html>gawk</html>", "/assets/a.js": "js", "/config.js": "https://" + testHost + ":4433"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("GET %s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d", rec.Code)
	}
}

// The page over real TLS: a browser that trusts the issuer loads it by
// name from the TCP listener, which opens only once a certificate is held.
func TestNamedServesHTTPSOnTheUIAddrOnceIssued(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	clk := &clock{t: time.Now()}
	s := &sent{}
	ca := newCA(t)
	n := newTestNamed(t, t.TempDir(), clk, s, func(o *NamedOptions) {
		o.UIAddr = addr
		o.App = fstest.MapFS{"index.html": {Data: []byte("<html>gawk</html>")}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = n.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(100 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("the UI listener opened before a certificate was held")
	}
	n.onConfig(testHost)
	csr := s.csrs()[0]
	n.onCertificate(poplink.Certificate{ChainPEM: ca.sign(t, csr, 1, clk.now()), ForCSRSHA256: pops.CSRSHA256(csr)})

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.pool, ServerName: testHost},
	}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rsp, err := client.Get("https://" + addr + "/")
		if err == nil {
			b, _ := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if string(b) != "<html>gawk</html>" {
				t.Fatalf("page = %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET https://%s/: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hello names the host the PoP holds a certificate for (docs/76 item 11).
func TestHelloCarriesTheNamedHost(t *testing.T) {
	c := New(Options{StateDir: t.TempDir()})
	if c.hello().NamedHost != "" {
		t.Fatal("a named host with no named tier")
	}
	clk := &clock{t: time.Now()}
	s := &sent{}
	n := NewNamed(c, NamedOptions{StateDir: t.TempDir(), Now: clk.now, Send: s.send, LAN: func() netip.Addr { return lan },
		System: fakeResolver{}, Public: fakeResolver{}})
	if c.hello().NamedHost != "" {
		t.Fatal("a named host before a certificate")
	}
	n.onConfig(testHost)
	csr := s.csrs()[0]
	n.onCertificate(poplink.Certificate{ChainPEM: newCA(t).sign(t, csr, 1, clk.now()), ForCSRSHA256: pops.CSRSHA256(csr)})
	if got := c.hello().NamedHost; got != testHost {
		t.Fatalf("hello.NamedHost = %q", got)
	}
	// The new host is announced with a fresh hello on the link.
	var hello *poplink.Hello
	for _, r := range s.recs {
		if h, ok := r.(poplink.Hello); ok {
			hello = &h
		}
	}
	if hello == nil || hello.NamedHost != testHost {
		t.Fatalf("no hello with the named host was sent: %+v", hello)
	}
}
