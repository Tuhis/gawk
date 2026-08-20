package auth

import (
	"net/http"
	"net/url"
	"strings"
)

// SecurityHeaders returns middleware that stamps docs/42 §4.8's headers onto
// every response. Wrap the whole mux with it: the SPA, /auth/config, /healthz
// and /api/v1 all need them, and only a mux-level wrap can promise "every
// response".
//
// issuerURL is the configured OIDC issuer. Its origin — and only its origin —
// is added to connect-src, because the SPA runs the code+PKCE flow against the
// IdP directly (§4.8). Everything else stays 'self': with no cookies, the
// residual browser risk is XSS against the in-memory access token, and a
// strict CSP plus the embedded-assets rule are the mitigation (§5).
func SecurityHeaders(issuerURL string) func(http.Handler) http.Handler {
	csp := buildCSP(issuerURL)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w, csp)
			next.ServeHTTP(w, r)
		})
	}
}

// setSecurityHeaders is the single definition of the header set. Middleware,
// RequireRole and ConfigHandler call it too, so an /api/v1 response carries
// the CSP even if the mux-level wrap is ever forgotten; re-setting a header to
// the same value is idempotent.
//
// Note what is NOT here: Set-Cookie. gawk-admin has no session and no cookie
// anywhere (D17), which is what deletes the entire CSRF class. HSTS is the
// Ingress's job (§4.8).
func setSecurityHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// buildCSP renders the policy. An unparsable or empty issuer simply
// contributes no origin — the policy stays valid and strictly tighter, and
// New refuses a blank issuer anyway.
func buildCSP(issuerURL string) string {
	connect := "'self'"
	if origin := originOf(issuerURL); origin != "" {
		connect += " " + origin
	}
	return strings.Join([]string{
		"default-src 'self'",
		"connect-src " + connect,
		// Vite inlines small assets as data: URIs, and the SPA renders inline
		// style attributes (style={{…}}, telemetry's UI pattern). Neither
		// widens the script surface, which is the one that matters for an
		// in-memory token: script-src inherits 'self' from default-src, so no
		// inline or third-party script can run.
		"img-src 'self' data:",
		"style-src 'self' 'unsafe-inline'",
		"font-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		// The portal is never a frame: clickjacking a kill button is a real
		// attack against a moderation surface.
		"frame-ancestors 'none'",
	}, "; ")
}

// originOf reduces a URL to scheme://host[:port].
func originOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
