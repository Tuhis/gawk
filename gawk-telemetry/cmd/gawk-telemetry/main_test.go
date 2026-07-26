package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const key64 = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

// Without the fleet key the service could only reject everything or accept
// anything. Refusing to start is the only honest third option.
func TestRefusesToStartWithoutTheFleetKey(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-telemetry-key", ""},
		{"-telemetry-key", "tooshort"},
		{"-telemetry-key", "zz" + key64[2:]},
	} {
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("parseFlags(%v) succeeded without a usable key", args)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	// The two listeners are separate by default: only ingest is ever routed
	// publicly (D1/D14).
	if c.ingestAddr == c.readAddr {
		t.Error("ingest and read share a listener; the read side must be separately exposable")
	}
	if c.retentionDays != 14 {
		t.Errorf("retentionDays = %d, want 14", c.retentionDays)
	}
	if c.scrapeInterval != 5*time.Second {
		t.Errorf("scrapeInterval = %v, want 5s", c.scrapeInterval)
	}
	// D11: the SQL passthrough is a deliberate act, never a default.
	if c.enableSQL {
		t.Error("query_sql enabled by default")
	}
	// No auth by default — cluster-internal is R9 D1's posture.
	if c.basicAuthUser != "" {
		t.Error("read listener has auth by default")
	}
	if !c.mcpEnabled {
		t.Error("MCP off by default; it is the item's primary read surface")
	}
}

func TestEnvFallbackAndFlagPrecedence(t *testing.T) {
	env := envMap(map[string]string{
		"GAWK_TELEMETRY_KEY":            key64,
		"GAWK_TELEMETRY_INGEST_ADDR":    ":9000",
		"GAWK_TELEMETRY_RETENTION_DAYS": "30",
		"GAWK_TELEMETRY_QUERY_SQL":      "true",
		"GAWK_TELEMETRY_RELAY_HEADLESS": "gawk-server-metrics-headless.gawk.svc",
	})
	c, err := parseFlags(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ingestAddr != ":9000" || c.retentionDays != 30 || !c.enableSQL {
		t.Errorf("env not applied: %+v", c)
	}
	if !strings.Contains(c.relayTargetDescription(), "headless") {
		t.Errorf("relay target = %q, want the headless Service", c.relayTargetDescription())
	}

	c, err = parseFlags([]string{"-ingest-addr", ":7000"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.ingestAddr != ":7000" {
		t.Errorf("ingestAddr = %q, want the flag to win over env", c.ingestAddr)
	}
}

func TestRejectsInvalidDurationsAndRetention(t *testing.T) {
	for _, args := range [][]string{
		{"-telemetry-key", key64, "-scrape-interval", "nonsense"},
		{"-telemetry-key", key64, "-scrape-interval", "0s"},
		{"-telemetry-key", key64, "-session-idle", "-1m"},
		{"-telemetry-key", key64, "-retention-days", "0"},
	} {
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("parseFlags(%v) accepted an invalid value", args)
		}
	}
}

// With no relay configured the service still runs — client-only telemetry —
// and says so rather than pretending it has a relay view.
func TestNoRelayConfiguredIsAValidMode(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.relayTargetDescription(), "client-only") {
		t.Errorf("relay target = %q, want it to name the client-only mode", c.relayTargetDescription())
	}
	addrs, err := c.resolver()(nil)
	if err != nil || len(addrs) != 0 {
		t.Errorf("resolver = %v / %v, want an empty target list", addrs, err)
	}
}

func TestStaticRelayAddrsOverrideTheHeadlessService(t *testing.T) {
	c, err := parseFlags([]string{
		"-telemetry-key", key64,
		"-relay-headless-service", "headless.svc",
		"-relay-addrs", "10.0.0.1:2112, 10.0.0.2:2112",
	}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := c.resolver()(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 || addrs[0] != "10.0.0.1:2112" {
		t.Errorf("addrs = %v", addrs)
	}
}

// CORS is for the SPLIT-ORIGIN deployment only (D1). The same-origin default
// must add no cross-origin surface at all.
func TestCORSOriginsAreOptIn(t *testing.T) {
	c, err := parseFlags([]string{"-telemetry-key", key64}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.corsOrigins) != 0 {
		t.Errorf("corsOrigins = %v by default, want none", c.corsOrigins)
	}

	c, err = parseFlags([]string{
		"-telemetry-key", key64,
		"-cors-origin", "https://gawk.example, http://127.0.0.1:4173",
	}, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.corsOrigins) != 2 || c.corsOrigins[0] != "https://gawk.example" {
		t.Errorf("corsOrigins = %v", c.corsOrigins)
	}
}

// A fixed housekeeping ticker would silently dominate any shorter
// -session-idle, making the knob appear to do nothing.
func TestSweepIntervalFollowsTheIdleTimeout(t *testing.T) {
	for idle, want := range map[time.Duration]time.Duration{
		2 * time.Second:  2 * time.Second,  // floor
		6 * time.Second:  2 * time.Second,  // floor
		40 * time.Second: 10 * time.Second, // idle/4
		2 * time.Minute:  30 * time.Second, // ceiling
		1 * time.Hour:    30 * time.Second, // ceiling
	} {
		if got := sweepInterval(idle); got != want {
			t.Errorf("sweepInterval(%v) = %v, want %v", idle, got, want)
		}
	}
}

// The routes must be registered WITHOUT a method. Go's ServeMux matches on
// method, so a "POST /v1/ingest" pattern 404s the CORS preflight — leaving the
// handler's own OPTIONS branch unreachable and every cross-origin POST blocked
// by the browser before it is ever sent.
//
// This is a MUX-level test on purpose: every handler-level test calls
// ServeHTTP directly and would pass with the bug present. It was found by the
// e2e pass, not by the unit suite.
func TestIngestRoutesAcceptPreflights(t *testing.T) {
	mux := http.NewServeMux()
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Reached-Handler", r.Method)
		w.WriteHeader(http.StatusNoContent)
	})
	// Mirrors the registration in run().
	mux.Handle("/api/telemetry/v1/ingest", probe)
	mux.Handle("/v1/ingest", probe)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/api/telemetry/v1/ingest", "/v1/ingest"} {
		for _, method := range []string{http.MethodOptions, http.MethodPost} {
			req, err := http.NewRequest(method, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := resp.Header.Get("X-Reached-Handler"); got != method {
				t.Errorf("%s %s did not reach the handler (status %d) — a method-scoped "+
					"route would block the CORS preflight", method, path, resp.StatusCode)
			}
		}
	}
}

// The wiring finding (review finding 1). `Projection.EndSession` existed, was
// unit-tested, and had no production caller: a viewer's row stayed on the
// dashboard forever after its session ended. The point of this test is that it
// goes through the REAL writer construction — the seam the projection's own
// tests could not reach, and the reason a green suite shipped a dead hook.
func TestFinalizingASessionRemovesItFromTheLiveView(t *testing.T) {
	st, err := store.New(store.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	projection := live.New(nil)
	api, err := readapi.New(readapi.Options{Store: st, Live: projection})
	if err != nil {
		t.Fatalf("readapi: %v", err)
	}
	cfg := config{sessionIdle: time.Minute, scrapeInterval: 5 * time.Second}
	w := newWriter(st, slog.New(slog.DiscardHandler), cfg, api, projection)

	accepted := ingest.Accepted{
		SessionID: "1c1c1c1c1c1c1c1c1c1c1c1c", BroadcastKey: "1a2b3c4d5e6f", Role: "viewer",
		App:     ingest.AppInfo{Version: "0.33.2", Surface: "viewer", Browser: "Chrome 152", OS: "Windows"},
		Samples: []ingest.Sample{{TMs: 0, Stats: map[string]any{"receivedFps": 60.0}}},
	}
	if err := w.Accept(accepted); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if n := countSessions(projection.Snapshot()); n != 1 {
		t.Fatalf("live sessions after one batch = %d, want 1", n)
	}

	// The clean end: a `final` batch. The idle sweep and shutdown converge on
	// the same finalize, so covering one covers the shape of all three.
	accepted.Seq, accepted.Final = 1, true
	if err := w.Accept(accepted); err != nil {
		t.Fatalf("final accept: %v", err)
	}
	if n := countSessions(projection.Snapshot()); n != 0 {
		t.Errorf("live sessions after finalize = %d; the row outlives the session it describes", n)
	}
}

func countSessions(snap live.Snapshot) int {
	n := 0
	for _, b := range snap.Live {
		n += len(b.Sessions)
	}
	return n
}

// The optional gate for exposing the fleet-wide read listener through an
// Ingress — which is exactly the configuration where a timing side channel on
// the comparison would matter, since everything behind it aggregates every
// broadcast on the fleet.
func TestBasicAuthAcceptsOnlyTheExactCredentials(t *testing.T) {
	h := basicAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "ops", "s3cret")

	for _, tc := range []struct {
		name       string
		user, pass string
		creds      bool
		want       int
	}{
		{"correct", "ops", "s3cret", true, http.StatusOK},
		{"wrong password", "ops", "wrong", true, http.StatusUnauthorized},
		{"wrong user", "nope", "s3cret", true, http.StatusUnauthorized},
		{"both wrong", "nope", "wrong", true, http.StatusUnauthorized},
		{"prefix of the password", "ops", "s3cre", true, http.StatusUnauthorized},
		{"no credentials at all", "", "", false, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/live", nil)
			if tc.creds {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 without WWW-Authenticate leaves a browser with no way to authenticate")
			}
		})
	}
}
