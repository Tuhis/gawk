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
// the media path may reach go-oidc (asserted by auth_import_test.go).
//
// Why a library at all: hand-rolled JWS verification on an authentication
// path is exactly the class of mistake CODE-REVIEW.md exists to prevent, and
// using go-oidc makes the relay's verification semantics identical to
// gawk-admin's (docs/42 §4.8, AP5) — two implementations of "is this token
// good?" would eventually disagree, invisibly.
//
// The key set is go-oidc's own oidc.RemoteKeySet. R39 first shipped a bespoke
// background-refreshed oidc.StaticKeySet here, on the premise that
// RemoteKeySet could not make per-request verification offline. Re-reading the
// upstream source (go-oidc v3.20.0, oidc/jwks.go) says otherwise:
// keysFromCache() has NO expiry check, so once a key is cached every later
// token signed by it verifies from memory and an IdP outage cannot break it;
// keysFromRemote() coalesces concurrent misses into one fetch; and a failed
// fetch leaves the cached keys in place. All three are what the bespoke cache
// was written to provide.
//
// What upstream lacks is a floor on how often a verification may reach the
// network, and that gap — and only that gap — is what jwksThrottle below adds.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// discoveryRetry* bound the retry cadence for OIDC discovery. Constants,
	// not knobs: they are correctness plumbing, not fleet capacity, and
	// docs/42 §4.5 specifies no operator control over them.
	discoveryRetryMin = 5 * time.Second
	discoveryRetryMax = 2 * time.Minute
	// idpRequestTimeout bounds discovery. jwksFetchTimeout bounds a JWKS
	// fetch, which happens on the REQUEST path and so gets the tighter bound.
	idpRequestTimeout = 10 * time.Second
	jwksFetchTimeout  = 5 * time.Second
	// defaultJWKSFetchInterval and defaultJWKSFetchBurst size the JWKS fetch
	// bucket: three fetches per minute, bucket full at startup. See
	// jwksThrottle.
	defaultJWKSFetchInterval = 20 * time.Second
	defaultJWKSFetchBurst    = 3
	// maxJWKSBytes bounds what we will read from the IdP. A JWKS is a handful
	// of kilobytes; anything past this is a misconfigured or hostile endpoint,
	// and the relay must not be knocked over by it. go-oidc reads the body
	// with an unbounded io.ReadAll, so the cap is applied here.
	maxJWKSBytes = 512 << 10

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
// from config.Config; the rest are test seams.
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
	// Now defaults to time.Now (tests drive the fetch bucket with it).
	Now func() time.Time
	// JWKSFetchInterval and JWKSFetchBurst size the JWKS fetch bucket: one
	// token accrues per interval, the bucket holds burst of them and starts
	// full. Zero means the default (three fetches per minute).
	JWKSFetchInterval time.Duration
	JWKSFetchBurst    int
}

// AdminAuth authorizes one request against the configured credentials.
type AdminAuth struct {
	token     []byte
	issuer    string
	audience  string
	rolesPath []string
	role      string
	log       *slog.Logger

	keys     *remoteKeys
	verifier *oidc.IDTokenVerifier
	throttle *jwksThrottle

	// resolved closes once OIDC discovery succeeds and the key set is
	// published, so tests can wait for it without polling. Never waited on by
	// the request path — until it closes, every JWT is answered 401.
	resolved chan struct{}
}

// NewAdminAuth builds the authenticator and, when OIDC is configured, starts
// background discovery.
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
	a := &AdminAuth{
		issuer:   opts.Issuer,
		audience: opts.Audience,
		role:     opts.Role,
		log:      log,
		keys:     &remoteKeys{},
		resolved: make(chan struct{}),
	}
	if opts.Token != "" {
		a.token = []byte(opts.Token)
	}
	if opts.Issuer == "" {
		return a
	}
	claim := strings.ReplaceAll(opts.RolesClaim, "{audience}", opts.Audience)
	a.rolesPath = strings.Split(claim, ".")
	// The verifier is built now, before discovery has answered: its issuer,
	// audience and algorithm allowlist are all configuration, and building it
	// up front keeps Configured() — and therefore whether the admin routes
	// exist at all — a synchronous fact rather than a race with the IdP.
	a.verifier = oidc.NewVerifier(opts.Issuer, a.keys, &oidc.Config{
		ClientID:             opts.Audience,
		SupportedSigningAlgs: adminSigningAlgs,
	})
	a.throttle = newJWKSThrottle(opts.JWKSFetchInterval, opts.JWKSFetchBurst, opts.Now)
	go a.run(ctx, client)
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

	// In the steady state this is offline: the key that signed the token is
	// already cached and the check is pure CPU. A verifier whose key set has
	// never resolved fails here, which is the 401 an unreachable IdP produces.
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

// remoteKeys is the oidc.KeySet the verifier is built against, holding the
// real key set once discovery has produced a jwks_uri. It exists so the
// verifier can be constructed before the IdP has been reached at all.
type remoteKeys struct {
	set atomic.Pointer[oidc.RemoteKeySet]
}

func (k *remoteKeys) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	set := k.set.Load()
	if set == nil {
		return nil, errors.New("oidc: the identity provider has not been reached yet")
	}
	return set.VerifySignature(ctx, jwt)
}

// run resolves discovery (retrying until ctx ends) and publishes the key set.
// There is no refresh loop: oidc.RemoteKeySet fetches on a verification miss
// and its cache never expires, so a periodic refresh would be a request to the
// IdP from every relay pod, forever, that changes nothing.
func (a *AdminAuth) run(ctx context.Context, client *http.Client) {
	jwksURL := a.resolveJWKSURL(ctx, client)
	if jwksURL == "" {
		return // ctx cancelled
	}
	a.keys.set.Store(newRemoteKeySet(ctx, jwksURL, client, a.throttle, a.log))
	close(a.resolved)
	a.log.Info("admin oidc discovery resolved", "issuer", a.issuer, "jwks_url", jwksURL)
}

// resolveJWKSURL retries OIDC discovery until it succeeds or ctx ends.
// oidc.NewProvider is what validates that the document's `issuer` matches the
// configured one — the check that stops a hijacked discovery URL pointing us
// at somebody else's keys.
func (a *AdminAuth) resolveJWKSURL(ctx context.Context, client *http.Client) string {
	backoff := discoveryRetryMin
	for {
		reqCtx := oidc.ClientContext(ctx, client)
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

// errJWKSThrottled is what a refused fetch surfaces. It travels back through
// the key set as an ordinary verification failure, so the caller answers 401 —
// never a 5xx, and never a valid token.
var errJWKSThrottled = errors.New("jwks fetch throttled: too many verification misses to fetch the key set again yet")

// jwksThrottle is the rate floor go-oidc lacks: a token bucket spent by JWKS
// fetches, installed as the RoundTripper of the HTTP client oidc.RemoteKeySet
// uses. A fetch with no token in the bucket never leaves the process — the key
// set sees a transport error, verification fails, and the request is answered
// 401.
//
// THE BUCKET SITS AT THE TRANSPORT, not in an oidc.KeySet wrapper that guesses
// from the token's `kid` whether the inner call will fetch, and that placement
// is the crux of the design. RemoteKeySet.verify() falls through to a fetch on
// ANY verification miss, not merely an unknown `kid`: a valid `kid` carrying a
// garbage signature misses too, and so does the post-rotation tail of tokens
// still signed by the retired key. A wrapper that throttled only unseen-`kid`
// calls would therefore leave the two commonest fetch-per-request paths
// completely unthrottled. Gating the transport needs no guess about what the
// inner call is about to do: a token is spent when, and only when, a request
// is actually made.
//
// The bucket starts FULL, so a genuine key rotation gets its fetch
// immediately and costs zero 401s. Only an attack — or a rotation that lands
// while one is in progress — ever waits, and then for at most one refill
// interval: 20 seconds at the defaults (three fetches per minute). An
// operator whose token was minted by the new key inside that window retries
// and is in.
//
// This type is a deliberate ~60-line twin of gawk-admin's
// internal/auth/keyset.go. The two live in separate Go modules with only the
// moderation contract shared between them (docs/42 D13), and a package
// existing solely to share a token bucket would cost more than it saves.
type jwksThrottle struct {
	burst    float64
	interval time.Duration // time to accrue one token
	now      func() time.Time

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newJWKSThrottle(interval time.Duration, burst int, now func() time.Time) *jwksThrottle {
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = defaultJWKSFetchInterval
	}
	if burst <= 0 {
		burst = defaultJWKSFetchBurst
	}
	return &jwksThrottle{
		burst:    float64(burst),
		interval: interval,
		now:      now,
		tokens:   float64(burst), // full: the first rotation never waits
		last:     now(),
	}
}

// tokensLeft reports the bucket's current contents WITHOUT refilling or
// spending. It exists for the tests: "this verification did not touch the
// IdP" is the guarantee docs/42 §4.5 rests on and the hardest one to observe
// from outside, and an unchanged, non-empty bucket is direct evidence that
// the fetch path was never even consulted.
func (t *jwksThrottle) tokensLeft() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokens
}

// throttleTokensLeft exposes the same reading for a configured AdminAuth.
func (a *AdminAuth) throttleTokensLeft() float64 {
	if a.throttle == nil {
		return 0
	}
	return a.throttle.tokensLeft()
}

// allow spends one token and reports whether there was one.
func (t *jwksThrottle) allow() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if elapsed := now.Sub(t.last); elapsed > 0 {
		t.tokens = min(t.burst, t.tokens+float64(elapsed)/float64(t.interval))
		t.last = now
	}
	if t.tokens < 1 {
		return false
	}
	t.tokens--
	return true
}

// throttledTransport spends a throttle token per JWKS request and caps the
// response body.
type throttledTransport struct {
	base     http.RoundTripper
	throttle *jwksThrottle
	log      *slog.Logger
}

func (t *throttledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.throttle.allow() {
		// RoundTrip owns the body once it is called, on the error path too.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		t.log.Debug("admin oidc JWKS fetch throttled", "url", req.URL.String())
		return nil, errJWKSThrottled
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	resp.Body = cappedBody{Reader: io.LimitReader(resp.Body, maxJWKSBytes), Closer: resp.Body}
	return resp, nil
}

// cappedBody is a response body truncated at maxJWKSBytes. Truncation fails
// closed: the JSON decode of a clipped document errors, the fetch fails, and
// the previously cached keys stay in place.
type cappedBody struct {
	io.Reader
	io.Closer
}

// newRemoteKeySet builds the throttled key set for jwksURL.
//
// base supplies the transport (so an injected client still applies) but not
// the timeout: a JWKS fetch happens on the REQUEST path here, so it gets the
// tighter of jwksFetchTimeout and whatever bound the caller set.
func newRemoteKeySet(ctx context.Context, jwksURL string, base *http.Client,
	throttle *jwksThrottle, log *slog.Logger) *oidc.RemoteKeySet {
	timeout := jwksFetchTimeout
	if base.Timeout > 0 && base.Timeout < timeout {
		timeout = base.Timeout
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: &throttledTransport{base: transport, throttle: throttle, log: log},
		Timeout:   timeout,
	}
	// RemoteKeySet reads its HTTP client off this context and deliberately
	// drops the cancellation (context.WithoutCancel, upstream), so a fetch is
	// bounded by the client timeout alone — which is why that timeout must
	// never be zero, and why shutdown never waits on an in-flight fetch.
	return oidc.NewRemoteKeySet(oidc.ClientContext(ctx, client), jwksURL)
}
