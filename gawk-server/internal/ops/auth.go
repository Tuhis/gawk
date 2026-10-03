package ops

// Authentication for the R39 relay admin API (docs/42 §4.5).
//
// Two credentials, one header. `Authorization: Bearer <credential>` is either
// the static -admin-api-token (the machine path gawk-admin uses, compared in
// constant time) or an OIDC JWT from -admin-oidc-issuer with
// -admin-oidc-audience, carrying -admin-oidc-role in -admin-oidc-roles-claim.
// The JWT path is what lets the IdP — not a shared string — govern who may
// read raw broadcast IDs, and gives an operator with a phone the same
// identity the portal uses.
//
// THIS FILE IS THE ONLY PLACE IN THE RELAY THAT REACHES AN OIDC LIBRARY, and
// it must stay that way: the data plane's dependency surface is a security
// property, not an accident. Nothing in internal/transport, internal/hub or
// the media path may reach go-oidc, directly or through the shared verifier
// (asserted by auth_import_test.go).
//
// The verifier itself is gawk-server's public oidcauth package (R53, docs/55
// D2), shared with gawk-admin and gawk-telemetry: one answer to "is this token
// good?" — JWKS caching and its fetch floor, discovery with backoff, key-set
// priming, the `alg` allowlist — instead of the twin copies R39 kept in step
// by review. What stays here is what is the relay's own: the static machine
// token, which is a credential rather than a verification mechanism, and the
// mapping of a verdict onto 200/401/403.

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
	"github.com/Tuhis/gawk/gawk-server/oidcroles"
)

const (
	// discoveryRetry* bound the retry cadence for OIDC discovery and key-set
	// priming. Constants, not knobs: they are correctness plumbing, not fleet
	// capacity, and docs/42 §4.5 specifies no operator control over them.
	discoveryRetryMin = 5 * time.Second
	discoveryRetryMax = 2 * time.Minute

	adminBearerPrefixLen = len("Bearer ")
)

// AdminAuthOptions configures NewAdminAuth. Everything but Log comes straight
// from config.Config; the rest are test seams.
type AdminAuthOptions struct {
	// Token is the static -admin-api-token; empty disables that path.
	Token string
	// Issuer / Audience enable the JWT path; both or neither (config.ParseFlags
	// rejects a half-configured pair before we ever get here).
	Issuer   string
	Audience string
	// RolesClaim is a dot-path template with oidcroles.Placeholder
	// substituted per segment; Role is the value the resolved array must
	// contain.
	RolesClaim string
	Role       string

	Log *slog.Logger
	// HTTPClient talks to the IdP. Nil means a bounded-timeout default —
	// never http.DefaultClient, whose zero timeout is an unbounded hang.
	HTTPClient *http.Client
	// Now defaults to time.Now (tests drive the fetch bucket with it).
	Now func() time.Time
	// JWKSFetchInterval and JWKSFetchBurst size the JWKS fetch bucket: one
	// token accrues per interval, the bucket holds burst of them and starts
	// full. Zero means the default (three fetches per minute).
	JWKSFetchInterval time.Duration
	JWKSFetchBurst    int

	// primeRetry is the delay before the first discovery or key-set priming
	// retry, doubling to discoveryRetryMax. Unexported on purpose: it is
	// correctness plumbing rather than an operator knob, and it exists only so
	// a test need not wait out the production backoff.
	primeRetry time.Duration
}

// AdminAuth authorizes one request against the configured credentials.
type AdminAuth struct {
	token []byte
	role  string

	// issuer is the configured OIDC issuer; non-empty means the JWT path is
	// configured, whether or not oidc could be built.
	issuer string
	// oidc is the shared verifier, nil when no issuer is configured or when
	// oidcauth refused the configuration (logged at Error; every JWT is then
	// a 401).
	oidc *oidcauth.Verifier
	// rolesUnusable is set when the roles claim (or the role) can never be
	// satisfied. Every JWT the verifier accepts is then a 403 — the answer R39
	// gave for this misconfiguration, kept: a token that cannot prove the role
	// does not have it, but it is still a good token.
	rolesUnusable bool
}

// NewAdminAuth builds the authenticator and, when OIDC is configured, starts
// background discovery.
//
// It NEVER fails on an unreachable IdP: discovery is retried in the background
// for as long as ctx lives, and until it succeeds every JWT is rejected with
// 401. The relay starting is not allowed to depend on the identity provider
// (docs/42 §6: "the IdP is availability-critical for the portal, never for
// enforcement").
func NewAdminAuth(ctx context.Context, opts AdminAuthOptions) *AdminAuth {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	a := &AdminAuth{issuer: opts.Issuer, role: opts.Role}
	if opts.Token != "" {
		a.token = []byte(opts.Token)
	}
	if opts.Issuer == "" {
		return a
	}

	cfg := oidcauth.Config{
		Issuer:     opts.Issuer,
		Audience:   opts.Audience,
		RolesClaim: opts.RolesClaim,
		Role:       opts.Role,
	}
	if _, err := oidcroles.ParsePath(opts.RolesClaim, opts.Audience); err != nil || strings.TrimSpace(opts.Role) == "" {
		// Fail closed, and loudly. config.ParseFlags already refuses an empty
		// claim and an empty role, so reaching here means a path that cannot
		// address anything (an empty segment). oidcauth refuses to build on
		// such a path; the relay still verifies signatures — on a placeholder
		// path it never consults — so that a good token is a 403 and a bad
		// one a 401, rather than letting an unreadable claim mean "no
		// constraint".
		log.Error("admin oidc roles claim is unusable: every JWT will be refused",
			"claim", opts.RolesClaim, "err", err)
		a.rolesUnusable = true
		cfg.RolesClaim, cfg.Role = oidcroles.DefaultClaim, "unusable"
	}

	primeRetry := opts.primeRetry
	if primeRetry <= 0 {
		primeRetry = discoveryRetryMin
	}
	v, err := oidcauth.New(ctx, cfg, oidcauth.Options{
		Logger:               log,
		HTTPClient:           opts.HTTPClient,
		Now:                  opts.Now,
		JWKSFetchInterval:    opts.JWKSFetchInterval,
		JWKSFetchBurst:       opts.JWKSFetchBurst,
		ResolveRetryInterval: primeRetry,
		ResolveRetryMax:      discoveryRetryMax,
		// R39's relay verifier enumerated its algorithms rather than
		// inheriting the discovery document's, and still does.
		SigningAlgorithms: oidcauth.AsymmetricSigningAlgs(),
	})
	if err != nil {
		// Unreachable through config.ParseFlags (a blank audience next to an
		// issuer is refused there). Configured() stays true — the issuer IS
		// set — and every JWT is a 401.
		log.Error("admin oidc is misconfigured: every JWT will be refused", "err", err)
		return a
	}
	a.oidc = v
	return a
}

// Configured reports whether ANY credential is available. False means the
// admin routes are never registered — the surface stays dark, not merely
// locked (docs/42 §4.3).
func (a *AdminAuth) Configured() bool {
	return a != nil && (len(a.token) > 0 || a.issuer != "")
}

// authorize maps a request onto an HTTP status: 200 to proceed, 401 for any
// invalid credential, 403 for a valid token without the required role.
// The second return is the reason, for the Debug log only — an authorization
// failure's detail is never in the response body.
func (a *AdminAuth) authorize(r *http.Request) (int, string) {
	raw := r.Header.Get("Authorization")
	if len(raw) <= adminBearerPrefixLen || !strings.EqualFold(raw[:adminBearerPrefixLen], "Bearer ") {
		return http.StatusUnauthorized, "missing or malformed Authorization header"
	}
	cred := strings.TrimSpace(raw[adminBearerPrefixLen:])
	if cred == "" {
		return http.StatusUnauthorized, "empty bearer credential"
	}

	// Constant-time compare, and only when a token is configured: with no
	// token the branch must not run at all, or an empty-secret deployment
	// would accept an empty credential.
	if len(a.token) > 0 && subtle.ConstantTimeCompare([]byte(cred), a.token) == 1 {
		return http.StatusOK, ""
	}
	if a.issuer == "" {
		return http.StatusUnauthorized, "bearer token did not match the static admin token"
	}
	if a.oidc == nil {
		return http.StatusUnauthorized, "jwt rejected: the admin oidc configuration is unusable"
	}

	// In the steady state this is offline: the key that signed the token is
	// already cached and the check is pure CPU. A verifier whose discovery has
	// never resolved fails here (oidcauth.ErrNotReady), which is the 401 an
	// unreachable IdP produces.
	id, err := a.oidc.Verify(r.Context(), cred)
	if err != nil {
		return http.StatusUnauthorized, "jwt rejected: " + err.Error()
	}
	if a.rolesUnusable {
		return http.StatusForbidden, "roles claim unusable: the configured claim path cannot address anything"
	}
	// A malformed or missing roles claim verifies with no roles (oidcauth
	// Debug-logs why), so it lands here too: "not authorized", never a 500 —
	// a token that cannot prove the role does not have it.
	if !id.HasRole(a.role) {
		return http.StatusForbidden, "token lacks the required role"
	}
	return http.StatusOK, ""
}
