package oidcauth

// Moved from gawk-admin/cmd/gawk-admin with the proxy itself (R53, docs/55
// D2), so telemetry can mount the identical route.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

	proxy, err := NewDevProxy(backend.URL)
	if err != nil {
		t.Fatalf("NewDevProxy: %v", err)
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
		if _, err := NewDevProxy(bad); err == nil {
			t.Fatalf("NewDevProxy(%q) accepted a non-base-URL", bad)
		}
	}
}
