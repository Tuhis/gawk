package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The migrate subcommand must be reachable with nothing but a DSN — the Helm
// hook Job carries no OIDC configuration, and demanding it there would make
// the schema step depend on the IdP (docs/42 §4.15).
func TestMigrateModeNeedsOnlyTheDSN(t *testing.T) {
	err := run([]string{"migrate"}, envFrom(map[string]string{
		"GAWK_ADMIN_PG_DSN": "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=1",
	}))
	// It must get as far as TRYING to migrate — i.e. fail on the database,
	// not on missing configuration. Anything mentioning a required flag means
	// validation rejected it before the DSN was ever used.
	if err == nil {
		t.Fatal("expected a connection failure against a dead DSN")
	}
	if strings.Contains(err.Error(), "is required") {
		t.Fatalf("migrate mode demanded serving configuration: %v", err)
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("error should name the migrate step, got: %v", err)
	}
}

// A serving process refuses to start without the knobs that make it safe.
// This is the guard that matters most: the one failure this service must never
// have is coming up with authentication silently off (docs/42 D7).
func TestServeModeRefusesIncompleteConfiguration(t *testing.T) {
	err := run(nil, envFrom(map[string]string{
		"GAWK_ADMIN_PG_DSN": "postgres://u:p@127.0.0.1:1/x",
	}))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "is required") {
		t.Fatalf("want a required-knob refusal, got: %v", err)
	}
}

// The election library rejects an empty identity outright, so a run outside
// Kubernetes must still produce one rather than failing at leader election
// with an error that says nothing about the environment.
func TestPodIdentityAlwaysResolves(t *testing.T) {
	if got := podIdentity(envFrom(map[string]string{"POD_NAME": "gawk-admin-7d4b"})); got != "gawk-admin-7d4b" {
		t.Errorf("POD_NAME should win, got %q", got)
	}
	if got := podIdentity(envFrom(nil)); got == "" {
		t.Error("identity must never be empty")
	}
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// The dev OIDC proxy (docs/41's compose lane): /idp/* reaches the dev IdP
// with the prefix stripped and the query intact — the discovery, authorize
// and token endpoints all ride it.
func TestDevOIDCProxyStripsThePrefixAndKeepsTheQuery(t *testing.T) {
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	proxy, err := newDevOIDCProxy(backend.URL)
	if err != nil {
		t.Fatalf("newDevOIDCProxy: %v", err)
	}
	for _, path := range []string{
		"/idp/.well-known/openid-configuration",
		"/idp/authorize?client_id=gawk-admin-spa&state=s",
		"/idp/token",
		"/idp", // the bare mount: forwarded as the backend's root
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: proxied status = %d", path, rec.Code)
		}
	}
	want := []string{
		"/.well-known/openid-configuration?",
		"/authorize?client_id=gawk-admin-spa&state=s",
		"/token?",
		"/?",
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("request %d reached the backend as %q, want %q", i, got[i], w)
		}
	}
}

func TestDevOIDCProxyRefusesANonURL(t *testing.T) {
	for _, bad := range []string{"fakeidp:8080", "://nope", ""} {
		if _, err := newDevOIDCProxy(bad); err == nil {
			t.Fatalf("newDevOIDCProxy(%q) accepted a non-base-URL", bad)
		}
	}
}
