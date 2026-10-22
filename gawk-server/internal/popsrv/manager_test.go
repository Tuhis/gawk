package popsrv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

const fileUID = "0a1b2c3d-4e5f-4a6b-8c7d-9e8f7a6b5c4d"

func writePopsFile(t *testing.T, path, credSHA string) {
	t.Helper()
	body := "- name: lan-party\n  uid: " + fileUID + "\n  displayName: LAN party\n  credentialSHA256: " + credSHA + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	// The file source polls mtime: make each write visibly newer.
	future := time.Now().Add(time.Duration(len(credSHA)%7+1) * time.Second)
	_ = os.Chtimes(path, future, future)
}

// -pops-file: a single-pod home with no pairing. The entry's credential
// hash is the credential; a "gawk-cred:" string links with it; editing
// the hash ends the link (rotating means editing the file).
func TestPopsFileCredentialsLinkWithoutPairing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := harnessLog(t)
	cred, _ := pops.NewCredential()
	path := filepath.Join(t.TempDir(), "pops.yaml")
	writePopsFile(t, path, pops.CredentialSHA256(cred))

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)
	cfg := config.Config{
		Addr: fmt.Sprintf("127.0.0.1:%d", port), MaxSubscribers: 4, MaxIdleTimeout: 30 * time.Second,
		KeepAlivePeriod: 10 * time.Second, BroadcastGrace: time.Minute, Pops: true, PopsFile: path,
	}
	r := hub.NewRegistry(log, hub.Options{MaxSubscribers: 4, BroadcastGrace: time.Minute})
	srv := transport.New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, log,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	src, err := popsrc.StartFile(ctx, path, log)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Pod: "home", PodAddr: cfg.Addr, Source: src, Status: popsrc.NewMemStatus(src), Registry: r,
		HomeOrigin: "https://gawk.example", Log: log, LeaseRenew: 100 * time.Millisecond, ByeGrace: 200 * time.Millisecond})
	t.Cleanup(m.Close)
	srv.SetPopHome(m)
	done := make(chan struct{})
	go func() { _ = srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	pool := certPool(cert)
	c := popclient.New(popclient.Options{
		StateDir: t.TempDir(), Version: "test", Log: log, Dial: popclient.Dialer(pool),
		RetryMin: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond, TerminalRetry: 200 * time.Millisecond,
	})
	cctx, ccancel := context.WithCancel(ctx)
	cdone := make(chan struct{})
	go func() { _ = c.Run(cctx); close(cdone) }()
	t.Cleanup(func() { ccancel(); <-cdone })

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	if err := c.Pair(ctx, popclient.CredentialPrefix+endpoint+"/"+cred); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "file link up", func() bool { return c.State().Header == popclient.HeaderLinked })
	if cfgRec := c.State().Config; cfgRec.PopName != "lan-party" || cfgRec.UID != fileUID || cfgRec.DisplayName != "LAN party" {
		t.Fatalf("config = %+v", cfgRec)
	}
	if rows := m.AdminPops(); len(rows) != 1 || rows[0].Name != "lan-party" || !rows[0].Linked {
		t.Fatalf("AdminPops = %+v", rows)
	}
	if addr, gen, ok := m.LinkHolder("lan-party"); !ok || addr != cfg.Addr || gen != 1 {
		t.Fatalf("LinkHolder = %q %d %v", addr, gen, ok)
	}

	// Rotate: a new hash in the file ends the link, and the old credential
	// is 401 from then on.
	other, _ := pops.NewCredential()
	writePopsFile(t, path, strings.ToUpper(pops.CredentialSHA256(other)))
	WaitFor(t, "rotation ends the link", func() bool { return c.State().Header == popclient.HeaderRepair })
	if st := c.State(); st.ByeReason != poplink.ByeRepair {
		t.Errorf("state = %+v", st)
	}
	if err := c.Pair(ctx, popclient.CredentialPrefix+endpoint+"/"+other); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "relink with the rotated credential", func() bool { return c.State().Header == popclient.HeaderLinked })
}

func certPool(cert tls.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(cert.Leaf)
	return p
}

// fakeSource is a Source over a map, for the unit tests.
type fakeSource struct {
	pops map[string]*pops.Pop
	subs []func(string)
}

func (f *fakeSource) Get(name string) (*pops.Pop, bool) {
	p, ok := f.pops[name]
	if !ok {
		return nil, false
	}
	return p.DeepCopy(), true
}

func (f *fakeSource) List() []pops.Pop {
	var out []pops.Pop
	for _, p := range f.pops {
		out = append(out, *p.DeepCopy())
	}
	return out
}

func (f *fakeSource) Subscribe(fn func(string)) func() { f.subs = append(f.subs, fn); return func() {} }
func (f *fakeSource) Synced() bool                     { return true }

func TestAuthenticateFailureLimiter(t *testing.T) {
	now := time.Now()
	cred, _ := pops.NewCredential()
	p := pops.FilePop{Name: "lan", UID: fileUID, CredentialSHA256: pops.CredentialSHA256(cred)}.ToPop()
	src := &fakeSource{pops: map[string]*pops.Pop{"lan": &p}}
	m := New(Options{Source: src, Status: popsrc.NewMemStatus(src), Now: func() time.Time { return now }, Log: harnessLog(t)})
	req := func(bearer, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodConnect, "/pop/link?proto=1", nil)
		r.RemoteAddr = remote
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}
	// The scan finds the Pop by its credential hash with no ?pop=.
	if got, status, err := m.Authenticate(req(cred, "198.51.100.1:5000")); err != nil || status != 200 || got.Name != "lan" {
		t.Fatalf("good credential: %v %d %v", got, status, err)
	}
	for i := range failLimit {
		if _, status, _ := m.Authenticate(req("wrong", "198.51.100.2:5000")); status != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d", i, status)
		}
	}
	if _, status, err := m.Authenticate(req("wrong", "198.51.100.2:6000")); status != http.StatusTooManyRequests || !errors.Is(err, ErrTooManyFailures) {
		t.Fatalf("over the limit: %d %v", status, err)
	}
	// The PoP shares the venue's public address with every guest: a guest
	// spraying junk from it must not lock the PoP's valid credential out.
	if got, status, err := m.Authenticate(req(cred, "198.51.100.2:6000")); err != nil || status != 200 || got.Name != "lan" {
		t.Fatalf("valid credential from a source over the limit: %v %d %v", got, status, err)
	}
	if _, status, _ := m.Authenticate(req("", "198.51.100.2:6000")); status != http.StatusTooManyRequests {
		t.Fatalf("no credential from a source over the limit: %d", status)
	}
	// Other sources are unaffected; the window passes.
	if _, status, _ := m.Authenticate(req(cred, "198.51.100.3:5000")); status != 200 {
		t.Fatalf("another source: %d", status)
	}
	now = now.Add(failWindow)
	if _, status, _ := m.Authenticate(req(cred, "198.51.100.2:5000")); status != 200 {
		t.Fatalf("after the window: %d", status)
	}
	// Revoked and ended Pops are 403.
	p.Spec.Revoked = true
	if _, status, err := m.Authenticate(req(cred, "198.51.100.4:5000")); status != http.StatusForbidden || !errors.Is(err, pops.ErrRevoked) {
		t.Fatalf("revoked: %d %v", status, err)
	}
	p.Spec.Revoked = false
	end := metav1.NewTime(now.Add(-time.Second))
	p.Spec.NotAfter = &end
	if _, status, err := m.Authenticate(req(cred, "198.51.100.4:5000")); status != http.StatusForbidden || !errors.Is(err, pops.ErrExpired) {
		t.Fatalf("expired: %d %v", status, err)
	}
}

// A pairing dial with a live string is not refused because others failed
// from the same (shared venue) address; junk from it still is.
func TestCheckPairingLetsAValidStringThroughASourceOverTheLimit(t *testing.T) {
	token, _ := pops.NewPairingToken()
	p := pops.FilePop{Name: "lan", UID: fileUID}.ToPop()
	p.Spec.Pairing.TokenSHA256 = pops.TokenSHA256(token)
	exp := metav1.NewTime(time.Now().Add(time.Hour))
	p.Spec.Pairing.ExpiresAt = &exp
	src := &fakeSource{pops: map[string]*pops.Pop{"lan": &p}}
	m := New(Options{Source: src, Status: popsrc.NewMemStatus(src), Log: harnessLog(t)})
	req := func(pair string) *http.Request {
		r := httptest.NewRequest(http.MethodConnect, "/pop/link?proto=1&pair="+pair, nil)
		r.RemoteAddr = "198.51.100.2:5000"
		return r
	}
	for range failLimit {
		m.CheckPairing(req("junk"))
	}
	if _, status, err := m.CheckPairing(req("junk")); status != http.StatusTooManyRequests || !errors.Is(err, ErrTooManyFailures) {
		t.Fatalf("junk over the limit: %d %v", status, err)
	}
	if got, status, err := m.CheckPairing(req(token)); err != nil || status != 200 || got.Name != "lan" {
		t.Fatalf("live string over the limit: %v %d %v", got, status, err)
	}
}

// A link's verdict on the Pop as it now reads (docs/76 item 9): revoke,
// deletion, recreation, end date, Re-pair and (file mode) a rotated hash
// end it; a lagging informer that has not seen this link's own redeem does
// not.
func TestLinkVerdict(t *testing.T) {
	now := time.Now()
	m := &Manager{opts: Options{Now: func() time.Time { return now }}}
	l := &link{m: m, uid: "u1", credSHA: "aa", tokenSHA: "t1"}
	base := func() *pops.Pop {
		return &pops.Pop{
			ObjectMeta: metav1.ObjectMeta{UID: "u1"},
			Spec:       pops.PopSpec{Pairing: pops.Pairing{TokenSHA256: "t1"}},
			Status:     pops.PopStatus{Credential: &pops.Credential{SHA256: "aa", PairingTokenSHA256: "t1"}},
		}
	}
	past := metav1.NewTime(now.Add(-time.Second))
	for _, tc := range []struct {
		name   string
		mutate func(p *pops.Pop)
		found  bool
		want   string
	}{
		{"current", func(*pops.Pop) {}, true, ""},
		{"lagging informer: no credential yet", func(p *pops.Pop) { p.Status.Credential = nil }, true, ""},
		{"lagging informer: the previous credential", func(p *pops.Pop) {
			p.Status.Credential = &pops.Credential{SHA256: "old", PairingTokenSHA256: "t0"}
		}, true, ""},
		{"deleted", func(*pops.Pop) {}, false, poplink.ByeRevoked},
		{"recreated", func(p *pops.Pop) { p.UID = "u2" }, true, poplink.ByeRevoked},
		{"revoked", func(p *pops.Pop) { p.Spec.Revoked = true }, true, poplink.ByeRevoked},
		{"ended", func(p *pops.Pop) { p.Spec.NotAfter = &past }, true, poplink.ByeExpired},
		{"re-paired", func(p *pops.Pop) { p.Spec.Pairing.TokenSHA256 = "t2" }, true, poplink.ByeRepair},
		{"rotated file hash", func(p *pops.Pop) { p.Status.Credential.SHA256 = "bb" }, true, poplink.ByeRepair},
	} {
		p := base()
		tc.mutate(p)
		reason, bye := l.verdict(p, tc.found)
		if reason != tc.want || bye != (tc.want != "") {
			t.Errorf("%s: verdict = %q %v, want %q", tc.name, reason, bye, tc.want)
		}
	}
}

func TestPublicAddrKeys(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":            "203.0.113.7",
		"::ffff:203.0.113.7":     "203.0.113.7",
		"2001:db8:1:2:aaaa::bbb": "2001:db8:1:2::/64",
	} {
		if got := publicAddrKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("publicAddrKey(%s) = %s, want %s", in, got, want)
		}
	}
	ps := parsePrefixes([]string{"203.0.113.7", "2001:db8:1:2::/64", "junk"})
	if len(ps) != 2 || !ps[0].Contains(netip.MustParseAddr("203.0.113.7")) || ps[0].Bits() != 32 ||
		!ps[1].Contains(netip.MustParseAddr("2001:db8:1:2::9")) {
		t.Fatalf("parsePrefixes = %v", ps)
	}
}

func TestLoweredBudget(t *testing.T) {
	for _, tc := range []struct{ ceiling, host, want int }{{25, 0, 25}, {25, 10, 10}, {25, 40, 25}, {0, 10, 10}, {0, 0, 0}} {
		got := lowered(pops.Budget{MaxMbps: tc.ceiling}, pops.Budget{MaxMbps: tc.host})
		if got.MaxMbps != tc.want {
			t.Errorf("lowered(%d, %d) = %d, want %d", tc.ceiling, tc.host, got.MaxMbps, tc.want)
		}
	}
}
