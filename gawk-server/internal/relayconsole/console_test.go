package relayconsole

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// fakeProvider records what the console asked of it.
type fakeProvider struct {
	mu        sync.Mutex
	state     State
	paired    []string
	lan       []netip.Addr
	uplink    []Budget
	downlink  []Budget
	alone     int
	firewall  int
	err       error
	pairCtxOK bool
}

func (f *fakeProvider) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeProvider) Pair(ctx context.Context, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, f.pairCtxOK = ctx.Deadline()
	f.paired = append(f.paired, s)
	return f.err
}

func (f *fakeProvider) RunStandalone() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alone++
	return f.err
}

func (f *fakeProvider) SetLANOverride(a netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lan = append(f.lan, a)
	return f.err
}

func (f *fakeProvider) LowerUplink(b Budget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uplink = append(f.uplink, b)
	return f.err
}

func (f *fakeProvider) LowerDownlink(b Budget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downlink = append(f.downlink, b)
	return f.err
}

// firewallProvider adds the optional firewall button.
type firewallProvider struct{ *fakeProvider }

func (f firewallProvider) AllowFirewall(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firewall++
	return nil
}

var testApp = fstest.MapFS{
	"index.html":       {Data: []byte("<!doctype html><title>gawk app</title>")},
	"assets/index.js":  {Data: []byte("console.log(1)")},
	"config.js":        {Data: []byte("stale")},
	"sub/.hidden.json": {Data: []byte("{}")},
}

func newTestHandler(p Provider) http.Handler {
	return Handler(p, testApp, func() []byte { return []byte("window.__GAWK_CONFIG__ = {};\n") })
}

func liveState() State {
	s := lanState(HeaderStandalone)
	s.Live = []LiveRow{{Code: "K7M2Q9", LocalViewers: 3}}
	s.Version = "1.2.3"
	return s
}

// do sends one request as the host's own browser would: from loopback, to
// a loopback Host.
func do(t *testing.T, h http.Handler, method, path string, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:4480"+path, r)
	req.RemoteAddr = "127.0.0.1:51234"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:4480")
	}
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// docs/76 PP3: the console refuses a non-loopback bind outright, so no
// flag value can put it on the LAN.
func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.168.1.20:0", "10.0.0.1:4480", "example.com:0", "no-port", ""} {
		if l, err := Listen(addr); err == nil {
			l.Close()
			t.Errorf("Listen(%q) succeeded; want a refusal", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		l, err := Listen(addr)
		if err != nil {
			t.Errorf("Listen(%q): %v", addr, err)
			continue
		}
		if ap, err := netip.ParseAddrPort(l.Addr().String()); err != nil || !ap.Addr().IsLoopback() {
			t.Errorf("Listen(%q) bound %s", addr, l.Addr())
		}
		l.Close()
	}
	// ::1 only where the host has IPv6 loopback.
	if l, err := Listen("[::1]:0"); err == nil {
		l.Close()
	} else if !strings.Contains(err.Error(), "assign") && !strings.Contains(err.Error(), "not available") {
		t.Errorf("Listen([::1]:0): %v", err)
	}
}

// docs/76 PP3: every request is checked, so a listener that somehow faces
// the LAN (a port forward, a proxy) still serves nobody but this machine.
func TestHandlerRefusesNonLoopbackPeers(t *testing.T) {
	h := newTestHandler(&fakeProvider{state: liveState()})
	for _, peer := range []string{"192.168.1.30:50000", "[fd12::5]:50000", "10.0.0.2:1", "garbage", ""} {
		for _, path := range []string{"/", "/api/state", "/app/", "/app/config.js", "/healthz", "/qr.svg"} {
			rec := do(t, h, http.MethodGet, path, "", func(r *http.Request) { r.RemoteAddr = peer })
			if rec.Code != http.StatusForbidden {
				t.Errorf("peer %q %s: status %d, want 403", peer, path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "K7M2Q9") {
				t.Errorf("peer %q %s: leaked a raw code", peer, path)
			}
		}
	}
	for _, peer := range []string{"127.0.0.1:1", "127.8.9.10:2", "[::1]:3", "[::ffff:127.0.0.1]:4"} {
		if rec := do(t, h, http.MethodGet, "/healthz", "", func(r *http.Request) { r.RemoteAddr = peer }); rec.Code != http.StatusOK {
			t.Errorf("peer %q: status %d, want 200", peer, rec.Code)
		}
	}
}

// DNS rebinding: a page on evil.example whose name now resolves to
// 127.0.0.1 reaches the listener from the host's own browser, with a
// loopback peer. Its Host header still names evil.example.
func TestHandlerRefusesNonLoopbackHost(t *testing.T) {
	h := newTestHandler(&fakeProvider{state: liveState()})
	for _, host := range []string{"evil.example:4480", "evil.example", "192.168.1.20:4480", "0.0.0.0:4480", "localhost.evil.example", "127.0.0.1.nip.io:4480", ""} {
		rec := do(t, h, http.MethodGet, "/api/state", "", func(r *http.Request) { r.Host = host })
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:4480", "localhost:4480", "LOCALHOST", "localhost.:4480", "[::1]:4480", "127.0.0.1"} {
		if rec := do(t, h, http.MethodGet, "/api/state", "", func(r *http.Request) { r.Host = host }); rec.Code != http.StatusOK {
			t.Errorf("Host %q: status %d, want 200", host, rec.Code)
		}
	}
}

// The same two checks over a real loopback socket: the peer is loopback,
// so only the Host header can refuse, and does.
func TestHandlerOverARealListener(t *testing.T) {
	l, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: newTestHandler(&fakeProvider{state: liveState()}), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(l)
	defer srv.Close()
	base := "http://" + l.Addr().String()

	rsp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK || strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("/healthz: %d %q", rsp.StatusCode, b)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/api/state", nil)
	req.Host = "evil.example"
	rsp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rsp.Body.Close()
	if rsp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding Host over a socket: %d, want 403", rsp.StatusCode)
	}
}

// A state change needs a same-origin JSON POST: a cross-site page can send
// a simple POST to 127.0.0.1 (no preflight), and Host alone does not stop
// it, so Origin, Sec-Fetch-Site and the content type do.
func TestStateChangesNeedSameOriginJSON(t *testing.T) {
	p := &fakeProvider{state: liveState()}
	h := newTestHandler(p)
	body := `{"pairing":"gawk-pair:api.gawk.ioio.fi:4433/ABC"}`
	for name, mod := range map[string]func(*http.Request){
		"cross-site Origin":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"null Origin":           func(r *http.Request) { r.Header.Set("Origin", "null") },
		"cross-site fetch":      func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site fetch":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"form content type":     func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"text content type":     func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"no content type":       func(r *http.Request) { r.Header.Del("Content-Type") },
		"other loopback port":   func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:5173") },
		"https loopback origin": func(r *http.Request) { r.Header.Set("Origin", "https://127.0.0.1:4480") },
		"rebound Host and Origin": func(r *http.Request) {
			r.Host = "evil.example:4480"
			r.Header.Set("Origin", "http://evil.example:4480")
		},
	} {
		rec := do(t, h, http.MethodPost, "/api/pair", body, mod)
		if rec.Code < 400 {
			t.Errorf("%s: status %d, want a refusal", name, rec.Code)
		}
	}
	if len(p.paired) != 0 {
		t.Fatalf("a refused request reached the provider: %q", p.paired)
	}
	if rec := do(t, h, http.MethodGet, "/api/pair", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/pair: %d, want 405", rec.Code)
	}

	for name, mod := range map[string]func(*http.Request){
		"same origin":             nil,
		"localhost origin":        func(r *http.Request) { r.Host = "localhost:4480"; r.Header.Set("Origin", "http://localhost:4480") },
		"no Origin (same-origin)": func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "same-origin") },
		"json with charset":       func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") },
	} {
		if rec := do(t, h, http.MethodPost, "/api/pair", body, mod); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d (%s), want 200", name, rec.Code, rec.Body)
		}
	}
	if len(p.paired) != 4 || p.paired[0] != "gawk-pair:api.gawk.ioio.fi:4433/ABC" {
		t.Fatalf("paired = %q", p.paired)
	}
	if !p.pairCtxOK {
		t.Error("Pair ran without a deadline")
	}
}

func TestActionsReachTheProvider(t *testing.T) {
	p := &fakeProvider{state: liveState()}
	h := newTestHandler(p)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/standalone", `{}`, 200},
		{"/api/lan", `{"addr":"192.168.1.40"}`, 200},
		{"/api/lan", `{"addr":""}`, 200},
		{"/api/lan", `{"addr":"not-an-ip"}`, 400},
		{"/api/uplink", `{"mbps":10,"streams":2}`, 200},
		{"/api/uplink", `{"mbps":-1}`, 400},
		{"/api/downlink", `{"mbps":50}`, 200},
		{"/api/pair", `{"pairing":"  "}`, 400},
		{"/api/pair", `not json`, 400},
		{"/api/pair", `{"pairing":"` + strings.Repeat("x", 9000) + `"}`, 400},
	} {
		if rec := do(t, h, http.MethodPost, c.path, c.body, nil); rec.Code != c.want {
			t.Errorf("POST %s %s: %d (%s), want %d", c.path, c.body[:min(len(c.body), 40)], rec.Code, rec.Body, c.want)
		}
	}
	if p.alone != 1 {
		t.Errorf("RunStandalone called %d times", p.alone)
	}
	if len(p.lan) != 2 || p.lan[0] != netip.MustParseAddr("192.168.1.40") || p.lan[1].IsValid() {
		t.Errorf("SetLANOverride calls = %v (want the address, then the invalid Addr for auto)", p.lan)
	}
	if len(p.uplink) != 1 || p.uplink[0] != (Budget{Mbps: 10, Streams: 2}) {
		t.Errorf("LowerUplink calls = %v", p.uplink)
	}
	if len(p.downlink) != 1 || p.downlink[0] != (Budget{Mbps: 50}) {
		t.Errorf("LowerDownlink calls = %v", p.downlink)
	}

	// A provider's refusal reaches the page as its message.
	p.err = errors.New("over the home fleet's ceiling")
	rec := do(t, h, http.MethodPost, "/api/uplink", `{"mbps":99}`, nil)
	var e struct{ Error string }
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error != "over the home fleet's ceiling" {
		t.Errorf("refusal: %d %s", rec.Code, rec.Body)
	}
}

// The firewall button exists only on a build and a provider that can act
// on it.
func TestFirewallEndpoint(t *testing.T) {
	plain := newTestHandler(&fakeProvider{state: liveState()})
	if rec := do(t, plain, http.MethodPost, "/api/firewall", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("provider without AllowFirewall: %d, want 404", rec.Code)
	}
	fp := firewallProvider{&fakeProvider{state: liveState()}}
	rec := do(t, newTestHandler(fp), http.MethodPost, "/api/firewall", `{}`, nil)
	want := http.StatusNotFound
	if FirewallSupported {
		want = http.StatusOK
	}
	if rec.Code != want {
		t.Errorf("firewall provider: %d, want %d", rec.Code, want)
	}
}

// /api/state is the page's one read: the State, the join link and tier,
// and the apps' gawk://server form.
func TestStateView(t *testing.T) {
	rec := do(t, newTestHandler(&fakeProvider{state: liveState()}), http.MethodGet, "/api/state", "", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/api/state: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", rec.Header().Get("Cache-Control"))
	}
	var v struct {
		State
		Join struct {
			URL  string `json:"url"`
			Tier string `json:"tier"`
		} `json:"join"`
		ServerLink string `json:"serverLink"`
		Firewall   bool   `json:"firewall"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	wantURL, wantTier := JoinQR(liveState())
	if v.Join.URL != wantURL || v.Join.Tier != wantTier || v.ServerLink != ServerLink(liveState()) {
		t.Errorf("join = %+v, serverLink %q", v.Join, v.ServerLink)
	}
	if len(v.Live) != 1 || v.Live[0].Code != "K7M2Q9" || v.Header != HeaderStandalone || v.Version != "1.2.3" {
		t.Errorf("state = %+v", v.State)
	}
	if v.Firewall {
		t.Error("firewall offered without an AllowFirewall provider")
	}
}

func TestQRCodeSVG(t *testing.T) {
	rec := do(t, newTestHandler(&fakeProvider{state: liveState()}), http.MethodGet, "/qr.svg", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(rec.Body.String(), "<svg ") {
		t.Fatalf("/qr.svg: %d %q %.40s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	url, _ := JoinQR(liveState())
	q, _ := EncodeQR([]byte(url))
	if rec.Body.String() != string(q.SVG()) {
		t.Error("/qr.svg is not the join link's symbol")
	}
	none := liveState()
	none.LanAddr = netip.Addr{}
	if rec := do(t, newTestHandler(&fakeProvider{state: none}), http.MethodGet, "/qr.svg", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no join link: %d, want 404", rec.Code)
	}
}

// docs/76 §6.1 item 6: /app/ is the embedded web app, with the generated
// config.js in place of any static one.
func TestAppAndConfigJS(t *testing.T) {
	h := newTestHandler(&fakeProvider{state: liveState()})
	rec := do(t, h, http.MethodGet, "/app/config.js", "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "window.__GAWK_CONFIG__ = {};\n" {
		t.Fatalf("config.js: %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("config.js headers: %q %q", ct, rec.Header().Get("Cache-Control"))
	}
	if rec := do(t, h, http.MethodGet, "/app/", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gawk app") {
		t.Errorf("/app/: %d %q", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/app/assets/index.js", "", nil); rec.Code != http.StatusOK {
		t.Errorf("/app/assets/index.js: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/app", "", nil); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/app/" {
		t.Errorf("/app: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, h, http.MethodGet, "/app/missing.js", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/app/missing.js: %d", rec.Code)
	}
}

// The console page itself: its own script and style (no inline script, so
// the CSP can forbid it), never framed.
func TestConsolePage(t *testing.T) {
	h := newTestHandler(&fakeProvider{state: liveState()})
	rec := do(t, h, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.js") {
		t.Fatalf("/: %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("console page can be framed")
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("console page carries an inline script")
	}
	for _, path := range []string{"/console.js", "/console.css"} {
		if rec := do(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, "/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/nope: %d", rec.Code)
	}
}
