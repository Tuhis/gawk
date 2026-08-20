package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildCSPCarriesOnlyTheIssuerOrigin(t *testing.T) {
	// A path-bearing issuer (Keycloak realms always have one) contributes its
	// origin and nothing else — a CSP source is an origin, and pinning a path
	// would silently not match.
	csp := buildCSP("https://id.example.test:8443/realms/gawk")
	if !strings.Contains(csp, "connect-src 'self' https://id.example.test:8443;") {
		t.Errorf("CSP = %q, want the issuer origin (with port) in connect-src", csp)
	}
	if strings.Contains(csp, "/realms/gawk") {
		t.Errorf("CSP = %q, want no issuer path", csp)
	}
	for _, want := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP = %q, want it to contain %q", csp, want)
		}
	}
}

func TestBuildCSPWithAnUnusableIssuerStaysStrict(t *testing.T) {
	for _, issuer := range []string{"", "   ", "not a url", "://nope"} {
		csp := buildCSP(issuer)
		if !strings.Contains(csp, "connect-src 'self';") {
			t.Errorf("buildCSP(%q) = %q, want connect-src 'self' with no extra origin", issuer, csp)
		}
	}
}

func TestSecurityHeadersWrapsAnyHandler(t *testing.T) {
	h := SecurityHeaders("https://id.example.test/realms/gawk")(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the wrapped handler's 418", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if len(rec.Result().Header.Values("Set-Cookie")) != 0 {
		t.Error("the security-header middleware set a cookie")
	}
}
