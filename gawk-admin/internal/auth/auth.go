// Package auth is gawk-admin's authentication and authorization boundary
// (R39, docs/42 D7, D17, §4.8).
//
// Every /api/v1 request proves who it is with an `Authorization: Bearer <JWT>`
// minted by the configured OIDC provider; authorization is a role read out of
// a claim in that same token. There is no session, no cookie and therefore no
// CSRF surface at all (D17).
//
// The mechanism is NOT here. Since R53 (docs/55 D2) the verifier — JWKS
// caching and its fetch floor, discovery with backoff, key-set priming, the
// per-IP invalid-credential limiter, the security headers, Middleware,
// RequireRole and /auth/config — is gawk-server's public oidcauth package,
// shared with the relay's admin API and with gawk-telemetry. R39 shipped it
// twice and kept the copies in step by review; this package now only maps
// gawk-admin's configuration onto it and names the contract internal/api is
// written against.
//
// The parent (cmd/gawk-admin) wires the exported surface:
//
//	a, err := auth.New(ctx, cfg, auth.Options{Logger: log})
//	mux.Handle("/auth/config", a.ConfigHandler())
//	mux.Handle("/api/v1/", a.Middleware(a.RequireRole(cfg.OperatorRole)(apiHandler)))
//	srv.Handler = auth.SecurityHeaders(cfg.OIDCIssuer)(mux)
//	// /readyz: ready only when the store AND the IdP are usable.
//	ready := store.Ready() && a.Ready()
//
// SecurityHeaders MUST wrap the whole mux: the SPA, /healthz and every other
// response need the CSP too, and only the mux-level wrap can guarantee that
// (§4.8 "headers on every response").
//
// internal/api consumes Middleware and RequireRole as injected behaviour and
// never validates a token itself; internal/identity (an alias of
// oidcauth.Identity) is the only type shared between the two halves. Neither
// package imports the other.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-server/oidcauth"
)

// Authenticator is the contract internal/api is written against (the package
// contracts brief): authentication as injected behaviour, so the routes and
// the mechanism guarding them stay independently buildable and testable.
type Authenticator interface {
	// Middleware runs next only for a request carrying a valid token, with
	// that token's identity on the request context. Any invalid credential is
	// answered 401 (or 429 once this client IP has spent its failure budget).
	Middleware(next http.Handler) http.Handler
	// RequireRole runs next only when the context identity carries role. It
	// must be wrapped by Middleware; a valid token without the role is 403.
	RequireRole(role string) func(http.Handler) http.Handler
	// RequireAnyRole is RequireRole for a route more than one role may call
	// (R49: the room reads admit operator OR rooms-reader, docs/50 D1).
	RequireAnyRole(roles ...string) func(http.Handler) http.Handler
}

var _ Authenticator = (*Auth)(nil)

// Auth validates tokens and authorizes roles: the shared verifier.
type Auth = oidcauth.Verifier

// Options tunes the authenticator. The zero value is production's
// configuration.
type Options = oidcauth.Options

// New validates the configuration and starts the verifier's background
// discovery (oidcauth.New).
//
// It fails ONLY on a configuration that could never be safe — a blank issuer,
// client ID, audience, roles-claim path or operator role (D7). An IdP that is
// merely unreachable is a transient condition: New succeeds, Ready reports
// false, authenticated routes answer 401, and the pod stays up and serving
// its SPA and health endpoints (docs/42 §6).
//
// ctx bounds the background worker, so it must be the process context, not a
// request's.
func New(ctx context.Context, cfg config.Config, opts Options) (*Auth, error) {
	// config.validate() refuses the same, but ParseFlags is not the only way
	// to build a Config. The client ID is checked here because oidcauth does
	// not require one (the relay has no browser client), while this service
	// publishes it on /auth/config and the SPA cannot log in without it.
	if strings.TrimSpace(cfg.OIDCClientID) == "" {
		return nil, errors.New("auth: OIDC client ID must not be empty")
	}
	return oidcauth.New(ctx, oidcauth.Config{
		Issuer:     cfg.OIDCIssuer,
		ClientID:   cfg.OIDCClientID,
		Audience:   cfg.OIDCAudience,
		RolesClaim: cfg.OIDCRolesClaim,
		Role:       cfg.OperatorRole,
	}, opts)
}

// SecurityHeaders returns middleware that stamps docs/42 §4.8's headers onto
// every response (oidcauth.SecurityHeaders). Wrap the whole mux with it.
func SecurityHeaders(issuerURL string) func(http.Handler) http.Handler {
	return oidcauth.SecurityHeaders(issuerURL)
}
