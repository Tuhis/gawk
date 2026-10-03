package main

// The read listener's routing and its auth gate (R53, docs/55 D1/D5/D6/D8).
//
// One function builds it for every mode so the tests drive the production
// wiring, not a restatement of it:
//
//	path                                  none   basic  oidc
//	/ and the embedded bundle             open   basic  open
//	GET /auth/config                      404    404    open
//	GET /readyz                           200    200    open (503 until discovery)
//	GET /v1/me                            (404)  basic  bearer, no role
//	/v1/*, /live, /live/*, /mcp           open   basic  bearer + role
//	/.well-known/oauth-protected-resource/mcp  as today  open (RFC 9728)
//
// In none and basic mode every response is what it was before R53 except
// that /auth/config and /readyz answer and the security headers are stamped
// (docs/55 G8, D5).

import (
	"encoding/json"
	"net/http"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/readapi"
)

// mcpPath is where the MCP endpoint is mounted, and therefore the resource
// whose RFC 9728 metadata the OIDC mode serves.
const mcpPath = "/mcp"

// readRoutes is what the read listener serves, independent of the gate.
type readRoutes struct {
	dashboard http.Handler
	api       http.Handler
	mcp       http.Handler // nil when -mcp=false
	devProxy  http.Handler // nil unless -dev-oidc-proxy
}

// newReadHandler wraps routes in the gate cfg selects. verifier is required
// in the OIDC mode and ignored otherwise.
func newReadHandler(cfg config, routes readRoutes, verifier *oidcauth.Verifier) http.Handler {
	var mux http.Handler
	if cfg.readAuthMode() == readAuthOIDC {
		mux = oidcReadMux(cfg, routes, verifier)
	} else {
		mux = legacyReadMux(cfg, routes)
	}
	// Every mode, every response (D5): strictly tightening, and the
	// frame-ancestors half is worth having without OIDC. With no issuer the
	// policy's connect-src is 'self' alone.
	return oidcauth.SecurityHeaders(cfg.oidcIssuer)(mux)
}

// legacyReadMux is the none/basic shape: today's routes, behind basic auth
// when it is configured, plus the two answers R53 adds.
func legacyReadMux(cfg config, routes readRoutes) http.Handler {
	inner := http.NewServeMux()
	inner.Handle("/", routes.dashboard)
	inner.Handle("/v1/", routes.api)
	inner.Handle("GET /live", routes.api)
	// UD22's SSE endpoint. A separate pattern because "/live" is an exact
	// match in Go's mux and would not carry the sub-path.
	inner.Handle("GET /live/", routes.api)
	if routes.mcp != nil {
		inner.Handle(mcpPath, routes.mcp)
	}
	var gated http.Handler = inner
	if cfg.readAuthMode() == readAuthBasic {
		gated = basicAuth(inner, cfg.basicAuthUser, cfg.basicAuthPass)
	}

	outer := http.NewServeMux()
	outer.Handle("/", gated)
	// Outside the basic gate: neither carries data, and the SPA's bootstrap
	// reads "404 here" as "not the OIDC mode" — without the explicit route
	// the SPA fallback would answer it with the page itself.
	outer.Handle("/auth/config", http.NotFoundHandler())
	outer.Handle("GET /readyz", readyzHandler(nil))
	if routes.devProxy != nil {
		outer.Handle(oidcauth.DevProxyPath, routes.devProxy)
	}
	return outer
}

// oidcReadMux is the OIDC shape (D5): the shell and its bootstrap open,
// everything that carries data behind a bearer token holding the role.
func oidcReadMux(cfg config, routes readRoutes, v *oidcauth.Verifier) http.Handler {
	role := v.RequireRole(cfg.oidcRole)
	// A bare `Bearer` challenge on every 401 (RFC 6750 §3); /mcp's names its
	// metadata document instead.
	challenge := oidcauth.Challenge(nil)
	gate := func(h http.Handler) http.Handler {
		return challenge(v.Middleware(role(capStreamAtExpiry(h))))
	}

	mux := http.NewServeMux()
	// The shell holds no data; it is how the browser learns which IdP to
	// bounce to.
	mux.Handle("/", routes.dashboard)
	mux.Handle("/auth/config", v.ConfigHandler())
	mux.Handle("GET /readyz", readyzHandler(v))
	// Authenticated but NOT role-gated: it is what lets the SPA tell "signed
	// in without the role" (its 403 page) from "not signed in", and what an
	// MCP client probes to see who it is.
	mux.Handle("GET /v1/me", challenge(v.Middleware(http.HandlerFunc(serveMe))))
	mux.Handle("/v1/", gate(routes.api))
	mux.Handle("GET /live", gate(routes.api))
	mux.Handle("GET /live/", gate(routes.api))
	// Nothing else is served under /.well-known — in particular no copy of the
	// metadata at the bare oauth-protected-resource URI, which would describe
	// the whole origin (RFC 9728 §3.1) — and the SPA fallback must not answer
	// for it either.
	mux.Handle("/.well-known/", http.NotFoundHandler())
	if routes.mcp != nil {
		resource := oidcauth.RequestResourceURL(mcpPath)
		mux.Handle(oidcauth.ProtectedResourceMetadataPath(mcpPath),
			oidcauth.ProtectedResourceHandler(cfg.oidcIssuer, resource))
		// Origin first: a browser-originated request is refused before its
		// credential is even looked at (D6).
		mux.Handle(mcpPath, oidcauth.RefuseBrowserOrigin(
			oidcauth.Challenge(resource)(v.Middleware(role(routes.mcp)))))
	}
	if routes.devProxy != nil {
		mux.Handle(oidcauth.DevProxyPath, routes.devProxy)
	}
	return mux
}

// capStreamAtExpiry hands the verified token's `exp` to the live stream,
// which ends there (D4). A zero expiry — unreachable, since verification
// requires `exp` — ends the stream at once rather than leaving it unbounded.
func capStreamAtExpiry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := oidcauth.FromContext(r.Context())
		next.ServeHTTP(w, r.WithContext(readapi.WithStreamDeadline(r.Context(), id.Expiry)))
	})
}

// meResponse is /v1/me's body: who the token says the caller is.
type meResponse struct {
	Subject string   `json:"subject"`
	Email   string   `json:"email"`
	Roles   []string `json:"roles"`
}

func serveMe(w http.ResponseWriter, r *http.Request) {
	id, ok := oidcauth.FromContext(r.Context())
	if !ok {
		// Only reachable by wiring serveMe without Middleware.
		writeAuthJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"code": oidcauth.CodeInternal, "message": "authentication middleware is not wired"},
		})
		return
	}
	roles := id.Roles
	if roles == nil {
		roles = []string{}
	}
	writeAuthJSON(w, http.StatusOK, meResponse{Subject: id.Subject, Email: id.Email, Roles: roles})
}

// readyzBody is /readyz's answer (D8). `idp` is "none" without the OIDC mode,
// "resolved" once discovery has answered, "unresolved" (503) until then.
type readyzBody struct {
	IDP   string `json:"idp"`
	Error string `json:"error,omitempty"`
}

// readyzHandler reports the read side's readiness. It is NOT the pod's
// readiness probe, which stays on the ingest /healthz: readiness is pod-wide,
// and an IdP outage must never take the public ingest Service down (D8).
func readyzHandler(v *oidcauth.Verifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case v == nil:
			writeAuthJSON(w, http.StatusOK, readyzBody{IDP: "none"})
		case v.Ready():
			writeAuthJSON(w, http.StatusOK, readyzBody{IDP: "resolved"})
		default:
			msg := "OIDC discovery has not completed yet"
			if err := v.ResolveError(); err != nil {
				msg = err.Error()
			}
			writeAuthJSON(w, http.StatusServiceUnavailable, readyzBody{IDP: "unresolved", Error: msg})
		}
	})
}

func writeAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
