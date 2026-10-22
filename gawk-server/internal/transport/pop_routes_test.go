package transport

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// fakePopHome is a PopHome that admits one credential and records calls.
type fakePopHome struct {
	mu       sync.Mutex
	auths    int
	addrs    []netip.Addr
	served   chan string
	pairTok  string
	redeemed bool
}

func (f *fakePopHome) Authenticate(r *http.Request) (*pops.Pop, int, error) {
	f.mu.Lock()
	f.auths++
	f.mu.Unlock()
	if poplink.BearerToken(r.Header.Get("Authorization")) != "good" {
		return nil, http.StatusUnauthorized, pops.ErrCredentialNotCurrent
	}
	return &pops.Pop{}, http.StatusOK, nil
}

func (f *fakePopHome) CheckPairing(r *http.Request) (*pops.Pop, int, error) {
	if r.URL.Query().Get("pair") != f.pairTok {
		return nil, http.StatusNotFound, pops.ErrPairingUnknown
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.redeemed {
		return nil, http.StatusGone, pops.ErrPairingUsed
	}
	return &pops.Pop{}, http.StatusOK, nil
}

func (f *fakePopHome) RedeemPairing(context.Context, *pops.Pop) (*pops.Pop, string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeemed = true
	return &pops.Pop{}, "minted", http.StatusOK, nil
}

func (f *fakePopHome) ServeLink(ctx context.Context, _ *pops.Pop, sess poplink.Session, issued string) error {
	f.served <- issued
	<-sess.Context().Done()
	return nil
}

func (f *fakePopHome) RecordAddr(_ string, remote netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs = append(f.addrs, remote)
}

func popDial(t *testing.T, port int, clientTLS *tls.Config, path string, h http.Header) (*webtransport.Session, *http.Response, error) {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	if h == nil {
		h = http.Header{}
	}
	h.Set("Origin", popOrigin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rsp, sess, err := d.Dial(ctx, "https://127.0.0.1:"+strconv.Itoa(port)+path, h)
	if sess != nil {
		t.Cleanup(func() { sess.CloseWithError(0, "") })
	}
	return sess, rsp, err
}

func startPopHome(t *testing.T, mutate func(*config.Config)) (int, *tls.Config, *Server, *fakePopHome) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := config.Config{
		MaxSubscribers: 2, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second,
		Pops: true, AllowedOrigins: []string{"https://gawk.example"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, cfg, discardLog)
	srv.testHookOriginCheckLoopback.Store(true)
	home := &fakePopHome{served: make(chan string, 4), pairTok: "TOKEN"}
	srv.SetPopHome(home)
	return port, clientTLS, srv, home
}

func TestPopLinkRefusesAnotherProtoMajorWith426(t *testing.T) {
	port, clientTLS, _, home := startPopHome(t, nil)
	for _, path := range []string{"/pop/link", "/pop/link?proto=2", "/pop/link?proto=x", "/pop/addr?proto=0"} {
		_, rsp, err := popDial(t, port, clientTLS, path, http.Header{"Authorization": {"Bearer good"}})
		if err == nil || rsp == nil || rsp.StatusCode != http.StatusUpgradeRequired ||
			rsp.Header.Get(poplink.ReasonHeader) != poplink.ReasonProto {
			t.Errorf("%s: %v %+v", path, err, rsp)
		}
	}
	if home.auths != 0 {
		t.Errorf("a protocol mismatch reached Authenticate %d times", home.auths)
	}
}

func TestPopLinkAuthenticatesAndPairsBeforeTheUpgrade(t *testing.T) {
	port, clientTLS, _, home := startPopHome(t, nil)
	_, rsp, err := popDial(t, port, clientTLS, "/pop/link?proto=1", http.Header{"Authorization": {"Bearer bad"}})
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusUnauthorized || rsp.Header.Get(poplink.ReasonHeader) != poplink.ByeRepair {
		t.Fatalf("bad credential: %v %+v", err, rsp)
	}
	if _, _, err := popDial(t, port, clientTLS, "/pop/link?proto=1", http.Header{"Authorization": {"Bearer good"}}); err != nil {
		t.Fatalf("good credential: %v", err)
	}
	if got := <-home.served; got != "" {
		t.Errorf("an ordinary link issued %q", got)
	}
	_, rsp, err = popDial(t, port, clientTLS, "/pop/link?proto=1&pair=NOPE", nil)
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusNotFound || rsp.Header.Get(poplink.ReasonHeader) != poplink.ReasonUnknown {
		t.Fatalf("unknown pairing: %v %+v", err, rsp)
	}
	if _, _, err := popDial(t, port, clientTLS, "/pop/link?proto=1&pair=TOKEN", nil); err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if got := <-home.served; got != "minted" {
		t.Errorf("the pairing link issued %q, want the minted credential", got)
	}
	_, rsp, err = popDial(t, port, clientTLS, "/pop/link?proto=1&pair=TOKEN", nil)
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusGone || rsp.Header.Get(poplink.ReasonHeader) != poplink.ReasonUsed {
		t.Fatalf("used pairing: %v %+v", err, rsp)
	}
}

func TestPopAddrRecordsThePeerAndCloses(t *testing.T) {
	port, clientTLS, _, home := startPopHome(t, nil)
	sess, _, err := popDial(t, port, clientTLS, "/pop/addr?proto=1", http.Header{"Authorization": {"Bearer good"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("/pop/addr did not close the session")
	}
	home.mu.Lock()
	defer home.mu.Unlock()
	if len(home.addrs) != 1 || home.addrs[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("recorded %v", home.addrs)
	}
}

// The /pop/* routes never consult the per-IP connection limiter (docs/76
// item 30): a venue's guests share its public address.
func TestPopRoutesSkipThePerIPLimiter(t *testing.T) {
	port, clientTLS, srv, home := startPopHome(t, func(c *config.Config) { c.ConnRateLimit = 0.001; c.ConnBurstLimit = 1 })
	srv.testHookRateLimitLoopback.Store(true)
	for i := range 3 {
		if _, _, err := popDial(t, port, clientTLS, "/pop/link?proto=1", http.Header{"Authorization": {"Bearer good"}}); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
		<-home.served
	}
	// The limiter is live: the client routes do hit it.
	var limited bool
	for range 3 {
		if _, rsp, err := popDial(t, port, clientTLS, "/echo", nil); err != nil && rsp != nil && rsp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the limiter never refused /echo; the exemption test proves nothing")
	}
}

// Without a manager the routes answer 404; with -pops off they do not
// exist at all, and the admin route is never on the public mux.
func TestPopRoutesAreAbsentWithoutPopsAndAdminIsNeverPublic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config.Config{MaxSubscribers: 2, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, Pops: true}
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, cfg, discardLog)
	_, rsp, err := popDial(t, port, clientTLS, "/pop/link?proto=1", nil)
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("no manager: %v %+v", err, rsp)
	}
	for _, path := range []string{"/pop/publish/ABC234", "/pop/subscribe/ABC234", "/internal/pop/assembly-2026"} {
		w := httptest.NewRecorder()
		srv.wt.H3.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodConnect, path, nil))
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501 until Wave 3", path, w.Code)
		}
	}
	for _, s := range []*Server{srv, New(config.Config{}, nil, nil, discardLog, nil)} {
		for _, path := range []string{"/internal/admin/pops", "/internal/admin/broadcasts"} {
			w := httptest.NewRecorder()
			s.wt.H3.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("public mux GET %s = %d, want 404", path, w.Code)
			}
		}
	}
	off := New(config.Config{}, nil, nil, discardLog, nil)
	for _, path := range []string{"/pop/link", "/pop/addr", "/pop/publish/ABC234", "/pop/subscribe/ABC234", "/internal/pop/x"} {
		w := httptest.NewRecorder()
		off.wt.H3.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodConnect, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("-pops off: CONNECT %s = %d, want the mux's 404", path, w.Code)
		}
	}
}

// poplink.Origin is the transport's popOrigin: the PoP's dials pass the
// origin check only because they are the same string.
func TestPopOriginMatchesThePopLinkOrigin(t *testing.T) {
	if poplink.Origin != popOrigin {
		t.Fatalf("poplink.Origin %q != popOrigin %q", poplink.Origin, popOrigin)
	}
}
