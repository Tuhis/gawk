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
// THIS FILE IS THE ONLY PLACE IN THE RELAY THAT IMPORTS AN OIDC LIBRARY, and
// it must stay that way: the data plane's dependency surface is a security
// property, not an accident. Nothing in internal/transport, internal/hub or
// the media path may reach go-oidc.
//
// Why a library at all: hand-rolled JWS verification on an authentication
// path is exactly the class of mistake CODE-REVIEW.md exists to prevent, and
// using go-oidc makes the relay's verification semantics identical to
// gawk-admin's (docs/42 §4.8, AP5) — two implementations of "is this token
// good?" would eventually disagree, invisibly.
//
// DEVIATION from the AP3 brief, deliberate and reported: the brief named
// oidc.NewRemoteKeySet. That type fetches the JWKS INLINE, on the first
// verification and again whenever it meets an unknown `kid` — so a request
// could block on the IdP, and a caller feeding random kids would make the
// relay hammer it once per request. The brief also requires per-request
// verification to be "fully offline". Those two cannot both hold, so this
// uses oidc.StaticKeySet (go-oidc's own offline verifier) behind a
// background refresher: every request verifies against an in-memory snapshot
// and never touches the network. go-oidc still does all claim validation and
// go-jose still does all key parsing and signature checking — no crypto is
// hand-rolled here.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// JWKS refresh cadence and the retry cadence for discovery. Both are
// constants, not knobs: they are correctness plumbing (a rotated signing key
// must be picked up before the old one's tokens run out), not fleet capacity,
// and docs/42 §4.5 specifies no operator control over them.
const (
	jwksRefreshInterval  = 5 * time.Minute
	discoveryRetryMin    = 5 * time.Second
	discoveryRetryMax    = 2 * time.Minute
	idpRequestTimeout    = 10 * time.Second
	adminBearerPrefixLen = len("Bearer ")
)

// adminSigningAlgs is the allowlist of JWS algorithms an admin token may use.
// Asymmetric only, and enumerated rather than inherited from the discovery
// document: "none" and the HMAC family must never be reachable, and a
// provider that advertises them must not be able to widen us.
var adminSigningAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512,
	oidc.EdDSA,
}

// AdminAuthOptions configures NewAdminAuth. Everything but Log comes straight
// from config.Config; the two test seams are HTTPClient and RefreshInterval.
type AdminAuthOptions struct {
	// Token is the static -admin-api-token; empty disables that path.
	Token string
	// Issuer / Audience enable the JWT path; both or neither (config.ParseFlags
	// rejects a half-configured pair before we ever get here).
	Issuer   string
	Audience string
	// RolesClaim is a dot-path with "{audience}" substituted; Role is the
	// value the resolved array must contain.
	RolesClaim string
	Role       string

	Log *slog.Logger
	// HTTPClient talks to the IdP. Nil means a bounded-timeout default —
	// never http.DefaultClient, whose zero timeout is an unbounded hang.
	HTTPClient *http.Client
	// RefreshInterval overrides the JWKS refresh cadence (tests).
	RefreshInterval time.Duration
}

// AdminAuth authorizes one request against the configured credentials.
type AdminAuth struct {
	token      []byte
	issuer     string
	audience   string
	rolesPath  []string
	role       string
	log        *slog.Logger
	httpClient *http.Client
	refresh    time.Duration

	// keys is the offline key snapshot the background refresher swaps in.
	// Nil until the first successful JWKS fetch — which is why an
	// unreachable IdP answers 401 rather than 500 or a startup failure.
	keys     atomic.Pointer[oidc.StaticKeySet]
	verifier *oidc.IDTokenVerifier

	// jwksReady closes after the first successful key fetch, so tests can wait
	// for the refresher without polling. Never waited on by the request path.
	jwksReady chan struct{}
	readyOnce atomic.Bool
}

// NewAdminAuth builds the authenticator and, when OIDC is configured, starts
// the background discovery + JWKS refresher.
//
// It NEVER fails on an unreachable IdP: discovery is retried in the
// background for as long as ctx lives, and until it succeeds every JWT is
// rejected with 401. The relay starting is not allowed to depend on the
// identity provider (docs/42 §6: "the IdP is availability-critical for the
// portal, never for enforcement").
func NewAdminAuth(ctx context.Context, opts AdminAuthOptions) *AdminAuth {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: idpRequestTimeout}
	}
	refresh := opts.RefreshInterval
	if refresh <= 0 {
		refresh = jwksRefreshInterval
	}
	a := &AdminAuth{
		issuer:     opts.Issuer,
		audience:   opts.Audience,
		role:       opts.Role,
		log:        log,
		httpClient: client,
		refresh:    refresh,
		jwksReady:  make(chan struct{}),
	}
	if opts.Token != "" {
		a.token = []byte(opts.Token)
	}
	if opts.Issuer == "" {
		return a
	}
	claim := strings.ReplaceAll(opts.RolesClaim, "{audience}", opts.Audience)
	a.rolesPath = strings.Split(claim, ".")
	a.verifier = oidc.NewVerifier(opts.Issuer, keySetFunc(a.verifySignature), &oidc.Config{
		ClientID:             opts.Audience,
		SupportedSigningAlgs: adminSigningAlgs,
	})
	go a.run(ctx)
	return a
}

// Configured reports whether ANY credential is available. False means the
// admin routes are never registered — the surface stays dark, not merely
// locked (docs/42 §4.3).
func (a *AdminAuth) Configured() bool {
	return a != nil && (len(a.token) > 0 || a.verifier != nil)
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
	if a.verifier == nil {
		return http.StatusUnauthorized, "bearer token did not match the static admin token"
	}

	// Verification is offline: the key snapshot is in memory and the context
	// carries no network client. A verifier that has never seen keys fails
	// here, which is the 401 an unreachable IdP produces.
	tok, err := a.verifier.Verify(r.Context(), cred)
	if err != nil {
		return http.StatusUnauthorized, "jwt rejected: " + err.Error()
	}
	ok, err := a.hasRole(tok)
	if err != nil {
		// A malformed or missing roles claim is "not authorized", never a
		// 500: a token that cannot prove the role does not have it.
		return http.StatusForbidden, "roles claim unusable: " + err.Error()
	}
	if !ok {
		return http.StatusForbidden, "token lacks the required role"
	}
	return http.StatusOK, ""
}

// hasRole resolves the configured dot-path in the token's claims and reports
// whether the required role is present.
func (a *AdminAuth) hasRole(tok *oidc.IDToken) (bool, error) {
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return false, err
	}
	var node any = claims
	for _, seg := range a.rolesPath {
		m, ok := node.(map[string]any)
		if !ok {
			return false, fmt.Errorf("claim path %q: %q is not an object",
				strings.Join(a.rolesPath, "."), seg)
		}
		node, ok = m[seg]
		if !ok {
			return false, fmt.Errorf("claim path %q: %q absent",
				strings.Join(a.rolesPath, "."), seg)
		}
	}
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == a.role {
				return true, nil
			}
		}
		return false, nil
	case string:
		// Some IdPs render a single role as a bare string.
		return v == a.role, nil
	default:
		return false, fmt.Errorf("claim path %q: not a string or array of strings",
			strings.Join(a.rolesPath, "."))
	}
}

// keySetFunc adapts a func to oidc.KeySet.
type keySetFunc func(ctx context.Context, jwt string) ([]byte, error)

func (f keySetFunc) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	return f(ctx, jwt)
}

// verifySignature checks a JWS against the current in-memory key snapshot.
// It performs NO I/O — that is the whole point (see the file comment).
func (a *AdminAuth) verifySignature(ctx context.Context, jwt string) ([]byte, error) {
	ks := a.keys.Load()
	if ks == nil {
		return nil, errors.New("oidc: no JWKS cached yet (identity provider not reached)")
	}
	return ks.VerifySignature(ctx, jwt)
}

// run resolves discovery (retrying forever) and then refreshes the JWKS on a
// ticker until ctx is cancelled.
func (a *AdminAuth) run(ctx context.Context) {
	jwksURL := a.resolveJWKSURL(ctx)
	if jwksURL == "" {
		return // ctx cancelled
	}
	a.log.Info("admin oidc discovery resolved", "issuer", a.issuer, "jwks_url", jwksURL)

	backoff := discoveryRetryMin
	for {
		if err := a.refreshKeys(ctx, jwksURL); err != nil {
			a.log.Warn("admin oidc JWKS refresh failed: still serving the last good key set",
				"issuer", a.issuer, "err", err)
		} else {
			backoff = discoveryRetryMin
		}
		wait := a.refresh
		if a.keys.Load() == nil {
			// Nothing cached yet — retry faster than the steady-state
			// cadence so a relay that started before the IdP does not sit
			// 401ing for five minutes after it comes up.
			wait = backoff
			backoff = min(backoff*2, discoveryRetryMax)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// resolveJWKSURL retries OIDC discovery until it succeeds or ctx ends.
// oidc.NewProvider is what validates that the document's `issuer` matches the
// configured one — the check that stops a hijacked discovery URL pointing us
// at somebody else's keys.
func (a *AdminAuth) resolveJWKSURL(ctx context.Context) string {
	backoff := discoveryRetryMin
	for {
		reqCtx := oidc.ClientContext(ctx, a.httpClient)
		provider, err := oidc.NewProvider(reqCtx, a.issuer)
		if err == nil {
			var meta struct {
				JWKSURL string `json:"jwks_uri"`
			}
			if err = provider.Claims(&meta); err == nil && meta.JWKSURL != "" {
				return meta.JWKSURL
			}
			if err == nil {
				err = errors.New("discovery document carries no jwks_uri")
			}
		}
		a.log.Warn("admin oidc discovery failed: JWT credentials are rejected until it succeeds",
			"issuer", a.issuer, "retry_in", backoff, "err", err)
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, discoveryRetryMax)
	}
}

// refreshKeys fetches the JWKS and swaps in a new offline snapshot. A failure
// leaves the previous snapshot in place: losing contact with the IdP must not
// revoke access that is already working (docs/42 §6).
func (a *AdminAuth) refreshKeys(ctx context.Context, jwksURL string) error {
	reqCtx, cancel := context.WithTimeout(ctx, idpRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks %s: HTTP %d", jwksURL, resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("jwks %s: %w", jwksURL, err)
	}
	pubs := make([]crypto.PublicKey, 0, len(set.Keys))
	for _, k := range set.Keys {
		// Signature keys only, public halves only, and only the three key
		// types oidc.StaticKeySet accepts — it ERRORS OUT on the first
		// unsupported type rather than skipping it, so one stray key would
		// otherwise break verification for every good one.
		if k.Use == "enc" || !k.IsPublic() {
			continue
		}
		switch k.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			pubs = append(pubs, k.Key)
		}
	}
	if len(pubs) == 0 {
		return fmt.Errorf("jwks %s: no usable public signing keys", jwksURL)
	}
	a.keys.Store(&oidc.StaticKeySet{PublicKeys: pubs})
	if a.readyOnce.CompareAndSwap(false, true) {
		close(a.jwksReady)
	}
	return nil
}
