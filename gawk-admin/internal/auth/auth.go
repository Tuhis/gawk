// Package auth is gawk-admin's authentication and authorization boundary
// (R39, docs/42 D7, D17, §4.8).
//
// Every /api/v1 request proves who it is with an `Authorization: Bearer <JWT>`
// minted by the configured OIDC provider. Validation is stateless — signature
// against the issuer's JWKS, then `iss`, `aud`, `exp`, `nbf` — and
// authorization is a role read out of a claim in that same token. There is no
// session, no cookie and therefore no CSRF surface at all (D17): a `Set-Cookie`
// on any response is a bug, and the tests assert its absence.
//
// Two properties this package is built around:
//
//   - **Per-request verification is offline.** The JWKS is cached by
//     go-oidc's oidc.RemoteKeySet, whose cache has no expiry, so an operator
//     can kill a stream while the IdP is slow or down. The only request that
//     reaches the IdP is one whose signature no cached key verifies — a key
//     rotation, or a forgery — and even that fetch is rate-floored so a
//     fuzzing loop cannot turn into a stampede against the IdP (keyset.go).
//   - **The refresh horizon is the revocation horizon.** A JWT cannot be
//     revoked server-side before it expires, so removing an operator's role at
//     the IdP takes effect at the next access-token refresh — which is exactly
//     why §4.8 recommends 5–15 minute access tokens. A token minted after the
//     role is removed is refused here; one minted before it keeps working
//     until it expires. That trade is stated, not hidden (§5).
//   - **An unreachable IdP degrades the portal; it does not kill the pod.**
//     Discovery happens in the background, with retries, so New never fails on
//     a transient IdP outage. Until it
//     succeeds, authenticated routes answer 401 and Ready reports false — the
//     signal cmd/gawk-admin folds into /readyz. §6's "OIDC provider down" row
//     describes exactly this: no new logins, enforcement untouched. Refusing
//     to boot instead would turn a 30-second IdP blip into a CrashLoopBackOff
//     across both replicas (D16), and would buy no safety: config.ParseFlags
//     (and New) already refuse a configuration that could serve with
//     authorization effectively off, which is D7's actual concern.
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
// (§4.8 "headers on every response"). Middleware and ConfigHandler set the
// same headers themselves so an /api/v1 response is never bare even if that
// wiring is forgotten; setting them twice is idempotent.
//
// internal/api consumes Middleware and RequireRole as injected behaviour and
// never validates a token itself; internal/identity is the only type shared
// between the two halves. Neither package imports the other.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/Tuhis/gawk/gawk-admin/internal/config"
	"github.com/Tuhis/gawk/gawk-admin/internal/identity"
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
}

var _ Authenticator = (*Auth)(nil)

// Defaults. Every one of these is overridable through Options, but the zero
// Options value is what production runs.
const (
	// defaultJWKSFetchInterval and defaultJWKSFetchBurst size the JWKS fetch
	// bucket (keyset.go): three fetches per minute, bucket full at startup.
	// The burst is what makes a genuine rotation free — the herd it produces
	// is coalesced into one fetch upstream, and the bucket has three. The
	// interval is what an attacker feeding unverifiable tokens is reduced to,
	// and it is also the worst case a rotation landing mid-attack waits: 20
	// seconds, one retry away for the operator.
	defaultJWKSFetchInterval = 20 * time.Second
	defaultJWKSFetchBurst    = 3
	// defaultFetchTimeout bounds a single JWKS request. It is also the longest
	// a request can wait on the IdP, and only ever on a cache-miss path.
	defaultFetchTimeout = 5 * time.Second
	// defaultHTTPTimeout bounds discovery and JWKS requests end to end.
	defaultHTTPTimeout = 10 * time.Second
	// defaultFailureRate/Burst damp invalid-credential responses per client IP
	// (§4.8). Ten failures back to back, then one per second: far above what a
	// browser tab does when its access token expires, far below a fuzzing loop.
	defaultFailureRate  = 1.0
	defaultFailureBurst = 10
	// limiterSweepInterval evicts idle failure buckets.
	limiterSweepInterval = 5 * time.Minute
	// defaultResolveRetryInterval is the delay before the first retry of OIDC
	// discovery; it doubles up to resolveRetryMax. Short at first because the
	// usual case is "the IdP is still starting alongside us".
	defaultResolveRetryInterval = time.Second
	// resolveRetryMax caps the backoff. A portal that cannot authenticate is
	// worth retrying forever, but not worth retrying hard.
	resolveRetryMax = 30 * time.Second
)

// Options tunes the authenticator. The zero value is production's
// configuration; each field exists because a test must drive it (clock, HTTP
// client) or an operator might need to tune it.
type Options struct {
	// Logger receives Debug-level rejection detail and Warn-level JWKS refresh
	// failures. Defaults to slog.Default(). Rejections carry the client IP and
	// the validation error, which is why they are Debug-only: IPs must not
	// appear in logs above Debug (docs/42 §5).
	Logger *slog.Logger
	// HTTPClient talks to the IdP — discovery at startup, JWKS afterwards.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
	// JWKSFetchInterval and JWKSFetchBurst size the JWKS fetch bucket: one
	// token accrues per interval, the bucket holds burst of them and starts
	// full. Zero means the default (three fetches per minute). Only a
	// verification that no cached key satisfies ever spends one.
	JWKSFetchInterval time.Duration
	JWKSFetchBurst    int
	// ResolveRetryInterval is the delay before the first retry of OIDC
	// discovery, doubling up to 30s. Defaults to one second.
	ResolveRetryInterval time.Duration
	// FailureRate and FailureBurst size the per-IP invalid-credential bucket.
	FailureRate  float64
	FailureBurst int
}

// Auth validates tokens and authorizes roles. Construct it with New; it owns a
// background goroutine (issuer resolution + limiter sweep) that Close stops.
type Auth struct {
	issuer   string
	clientID string
	audience string
	// rolesPath is the configured dot-path, pre-split. See roles.go.
	rolesPath []string

	client       *http.Client
	now          func() time.Time
	resolveRetry time.Duration
	throttle     *jwksThrottle

	limiter *ipLimiter
	log     *slog.Logger
	csp     string

	// mu guards the resolution state below. verifier is nil until discovery
	// succeeds; once set it is never unset — from then on the cached JWKS
	// keeps verifying tokens whatever the IdP is doing (docs/42 §6).
	mu         sync.RWMutex
	verifier   *oidc.IDTokenVerifier
	resolveErr error
	failures   int

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// New validates the configuration and starts the background worker that
// resolves the provider (OIDC discovery), retrying until it succeeds.
//
// It fails ONLY on a configuration that could never be safe — a blank issuer,
// client ID, audience, roles-claim path or operator role. That is D7's actual
// concern, and it is decided without touching the network. An IdP that is
// merely unreachable is a transient condition: New succeeds, Ready reports
// false, authenticated routes answer 401, and the pod stays up and serving
// its SPA and health endpoints (docs/42 §6). Crashlooping both replicas
// through an IdP restart would help nobody.
//
// ctx bounds the background worker, so it must be the process context, not a
// request's.
func New(ctx context.Context, cfg config.Config, opts Options) (*Auth, error) {
	// These four checks duplicate config.validate() on purpose. ParseFlags is
	// not the only way to build a Config, and "no roles claim" or "no required
	// role" would authorize every valid token — the refusal has to hold for a
	// programmatic caller too (docs/42 §4.8, AP5).
	if strings.TrimSpace(cfg.OIDCIssuer) == "" {
		return nil, errors.New("auth: OIDC issuer must not be empty")
	}
	if strings.TrimSpace(cfg.OIDCClientID) == "" {
		return nil, errors.New("auth: OIDC client ID must not be empty")
	}
	if strings.TrimSpace(cfg.OIDCAudience) == "" {
		return nil, errors.New("auth: OIDC audience must not be empty: an unvalidated audience accepts tokens minted for another application")
	}
	if strings.TrimSpace(cfg.OperatorRole) == "" {
		return nil, errors.New("auth: operator role must not be empty: with no required role every valid token would be an operator")
	}
	rolesPath, err := parseClaimPath(cfg.RolesClaimPath())
	if err != nil {
		return nil, fmt.Errorf("auth: roles claim path: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	resolveRetry := opts.ResolveRetryInterval
	if resolveRetry <= 0 {
		resolveRetry = defaultResolveRetryInterval
	}
	rate := opts.FailureRate
	if rate <= 0 {
		rate = defaultFailureRate
	}
	burst := opts.FailureBurst
	if burst <= 0 {
		burst = defaultFailureBurst
	}

	runCtx, cancel := context.WithCancel(ctx)
	a := &Auth{
		// The configured issuer is what /auth/config publishes and what the
		// CSP allows, both of which must work before (and during) an IdP
		// outage — the SPA can still be bounced to the IdP, which is where
		// that failure belongs. Discovery only ever confirms this string:
		// go-oidc refuses a document whose own `issuer` differs.
		issuer:       cfg.OIDCIssuer,
		clientID:     cfg.OIDCClientID,
		audience:     cfg.OIDCAudience,
		rolesPath:    rolesPath,
		client:       client,
		now:          now,
		resolveRetry: resolveRetry,
		throttle:     newJWKSThrottle(opts.JWKSFetchInterval, opts.JWKSFetchBurst, now),
		limiter:      newIPLimiter(rate, burst, now),
		log:          log,
		csp:          buildCSP(cfg.OIDCIssuer),
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	go a.run(runCtx)
	return a, nil
}

// Ready reports whether the provider has been resolved: OIDC discovery
// answered and the verifier exists. Until it does, every authenticated route
// answers 401, so cmd/gawk-admin folds this into /readyz alongside the store's
// own check — an unready pod should not take portal traffic.
//
// It never goes back to false. Note what it does NOT claim: the JWKS is
// fetched lazily, on the first token whose signature no cached key verifies,
// so a ready pod that has not yet served an authenticated request still needs
// one round trip to the IdP. From that fetch onwards verification is offline
// and an IdP outage no longer affects this process (§4.8, §6). Priming the
// cache at resolve time would buy a narrow window — a pod that went ready and
// then never saw a request until the IdP was down — at the cost of a fetch
// every pod makes whether or not anyone authenticates.
func (a *Auth) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.verifier != nil
}

// ResolveError returns why the provider is not resolved yet, or nil once it
// is. It exists so /readyz can say *what* is wrong rather than just "not
// ready".
func (a *Auth) ResolveError() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.verifier != nil {
		return nil
	}
	return a.resolveErr
}

// verifier returns the current verifier, or false while unresolved.
func (a *Auth) currentVerifier() (*oidc.IDTokenVerifier, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.verifier, a.verifier != nil
}

// resolve performs OIDC discovery and publishes the verifier, after which
// Ready is true forever. It does not touch the JWKS: the key set fetches
// lazily and caches for the process lifetime (keyset.go).
func (a *Auth) resolve(ctx context.Context) error {
	// go-oidc verifies that the document's own `issuer` matches the URL we
	// asked for, which is the check that makes the rest of the validation
	// meaningful.
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, a.client), a.issuer)
	if err != nil {
		return fmt.Errorf("discovery for %q: %w", a.issuer, err)
	}
	// go-oidc exposes neither the issuer it validated nor the jwks_uri as
	// fields, so read them back off the raw discovery document. Issuer is
	// already guaranteed to equal a.issuer — NewProvider refuses the mismatch
	// — and using the discovered spelling keeps the value the token's `iss` is
	// compared against and the value the provider published the same string,
	// byte for byte.
	var meta struct {
		Issuer     string   `json:"issuer"`
		JWKSURL    string   `json:"jwks_uri"`
		Algorithms []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&meta); err != nil {
		return fmt.Errorf("reading provider metadata: %w", err)
	}
	if meta.JWKSURL == "" {
		return fmt.Errorf("provider %q advertises no jwks_uri", a.issuer)
	}
	if meta.Issuer == "" {
		meta.Issuer = a.issuer
	}

	keys := newRemoteKeySet(ctx, meta.JWKSURL, a.client, a.throttle, a.log)
	verifier := oidc.NewVerifier(meta.Issuer, keys, &oidc.Config{
		// go-oidc names this ClientID; it is the value compared against the
		// token's `aud`, which for an access token is the audience the IdP
		// stamped, not the SPA's client ID (§4.12).
		ClientID:             a.audience,
		SupportedSigningAlgs: signingAlgs(meta.Algorithms),
		Now:                  a.now,
	})

	a.mu.Lock()
	a.verifier = verifier
	failures := a.failures
	a.resolveErr = nil
	a.mu.Unlock()

	if failures > 0 {
		// A state change an operator watching the log needs to see: the portal
		// went from "up but unusable" to "usable".
		a.log.Warn("oidc issuer resolved: authentication is available again",
			"issuer", a.issuer, "failedAttempts", failures)
	} else {
		a.log.Info("oidc issuer resolved", "issuer", a.issuer)
	}
	return nil
}

// noteResolveFailure records why we are still unresolved and says so once,
// loudly. Repeats drop to Debug: an IdP that is down for an hour should not
// bury every other line in the log, and the state has not changed.
func (a *Auth) noteResolveFailure(err error, retryIn time.Duration) {
	a.mu.Lock()
	a.failures++
	attempts := a.failures
	a.resolveErr = err
	a.mu.Unlock()

	if attempts == 1 {
		a.log.Warn("oidc issuer unresolved: the portal is serving but no request can authenticate until the IdP answers",
			"issuer", a.issuer, "err", err, "retryIn", retryIn.String())
		return
	}
	a.log.Debug("oidc issuer still unresolved",
		"issuer", a.issuer, "attempts", attempts, "err", err, "retryIn", retryIn.String())
}

// Close stops the background worker. Idempotent.
func (a *Auth) Close() error {
	a.closeOnce.Do(func() {
		a.cancel()
		<-a.done
	})
	return nil
}

// run is the single background goroutine: retry discovery until it resolves,
// and evict limiter buckets throughout — 401s (and therefore failure budgets)
// exist before resolution as well as after.
//
// It performs no JWKS traffic at all. R39's background refresher is gone with
// the bespoke cache: oidc.RemoteKeySet fetches on a verification miss and its
// cache never expires, so a periodic refresh would be a request to the IdP
// that changes nothing, on every replica, forever.
func (a *Auth) run(ctx context.Context) {
	defer close(a.done)

	sweep := time.NewTicker(limiterSweepInterval)
	defer sweep.Stop()
	// Fire immediately: the common case is an IdP that is already up.
	attempt := time.NewTimer(0)
	defer attempt.Stop()

	backoff := a.resolveRetry
	for {
		var attemptC <-chan time.Time
		if attempt != nil {
			attemptC = attempt.C
		}

		select {
		case <-ctx.Done():
			return

		case <-sweep.C:
			a.limiter.sweep()

		case <-attemptC:
			if err := a.resolve(ctx); err != nil {
				if ctx.Err() != nil {
					return // shutting down, not a real failure
				}
				a.noteResolveFailure(err, backoff)
				attempt.Reset(backoff)
				backoff = min(2*backoff, max(resolveRetryMax, a.resolveRetry))
				continue
			}
			// Resolution happens once. From here the only IdP traffic is a
			// throttled fetch behind a verification miss.
			attempt.Stop()
			attempt = nil
		}
	}
}

// Middleware authenticates the request and puts the caller's identity on the
// context. It authorizes nothing: a token whose roles claim is missing or
// malformed authenticates with an empty role set, and RequireRole then refuses
// it with 403 (never a 500 — a claim shape we do not recognise is the IdP's
// configuration talking, not a bug in this process).
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, a.csp)

		verifier, ready := a.currentVerifier()
		if !ready {
			// We cannot judge this credential yet, so we refuse it — 401, not
			// 500: nothing is broken here, and the SPA's existing 401 handling
			// (refresh, then re-run the redirect flow) is the right reaction.
			// This costs the caller nothing: the failure is ours, so it does
			// not spend their invalid-credential budget.
			a.log.Debug("request refused while the issuer is unresolved", "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "idp_unavailable",
				"the identity provider is not reachable yet; retry shortly")
			return
		}

		raw, err := bearerToken(r)
		if err != nil {
			a.denyCredential(w, r, err)
			return
		}
		token, err := verifier.Verify(r.Context(), raw)
		if err != nil {
			a.denyCredential(w, r, err)
			return
		}
		var claims map[string]any
		if err := token.Claims(&claims); err != nil {
			// The payload verified but is not a JSON object. Treat it as an
			// invalid credential rather than an internal error: nothing on our
			// side is broken.
			a.denyCredential(w, r, fmt.Errorf("decoding claims: %w", err))
			return
		}

		// Email is optional: a client-credentials service identity (R40's
		// sampler, §4.11) has no user behind it, and identity.Actor() falls
		// back to the subject so an audit row is never blank.
		id := identity.Identity{Subject: token.Subject}
		if email, ok := claims["email"].(string); ok {
			id.Email = email
		}
		roles, err := rolesFromClaims(claims, a.rolesPath)
		if err != nil {
			// Debug only: this is a legitimately authenticated caller whose
			// token does not carry the claim we were told to read.
			a.log.Debug("roles claim unusable", "path", strings.Join(a.rolesPath, "."), "err", err)
		}
		id.Roles = roles

		next.ServeHTTP(w, r.WithContext(identity.NewContext(r.Context(), id)))
	})
}

// RequireRole refuses a valid token that does not carry role (403).
//
// Every R39 route requires cfg.OperatorRole. The role is a parameter rather
// than a constant because R40's content-flag route authorizes a different one
// — cfg.FlaggerRole, held by the sampler's client-credentials service identity
// (§4.11) — and because kill/ban rights must stay bound to a role, never to a
// merely-valid token.
//
// An empty role panics rather than returning a middleware: routes are wired at
// startup, so this is the same "refuse to boot" the config validation performs
// (§4.8) — a RequireRole("") that quietly authorized everyone is precisely the
// failure that rule exists to prevent.
func (a *Auth) RequireRole(role string) func(http.Handler) http.Handler {
	if strings.TrimSpace(role) == "" {
		panic("auth: RequireRole(\"\"): with no required role every valid token would be an operator")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setSecurityHeaders(w, a.csp)
			id, ok := identity.FromContext(r.Context())
			if !ok {
				// Only reachable by wiring RequireRole without Middleware:
				// a programming error, not a client error (identity.go).
				a.log.Error("RequireRole reached without an authenticated identity; check the middleware wiring", "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "authentication middleware is not wired")
				return
			}
			if !id.HasRole(role) {
				a.log.Debug("role missing", "role", role, "subject", id.Subject)
				writeError(w, http.StatusForbidden, "forbidden", "this account does not hold the "+role+" role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authConfig is the SPA's unauthenticated bootstrap payload: exactly the three
// values a public OIDC client needs to start the PKCE flow, and nothing else.
// No secret exists to leak — the SPA is a public client (§4.8) — but this
// endpoint is world-readable wherever the portal is exposed, so the struct is
// deliberately closed. Anything added here is published.
type authConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	Audience string `json:"audience"`
}

// ConfigHandler serves GET /auth/config, unauthenticated (§4.8).
func (a *Auth) ConfigHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, a.csp)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
			return
		}
		writeJSON(w, http.StatusOK, authConfig{
			Issuer:   a.issuer,
			ClientID: a.clientID,
			Audience: a.audience,
		})
	})
}

// denyCredential answers a request whose credential did not validate, and
// spends one of this client IP's failure tokens.
//
// Only failures spend tokens, and the budget decides the *status code* rather
// than short-circuiting the request. That ordering is deliberate: behind an
// Ingress every operator can share one observed source IP, so a scheme that
// refused requests from a "hot" IP before validating them would let one
// fuzzing loop lock a paged operator out of the portal (§4.8, D7). Here a
// valid token always passes, whatever the attacker in the next NAT session is
// doing, and the attacker's own responses degrade to 429 within `burst`
// requests.
func (a *Auth) denyCredential(w http.ResponseWriter, r *http.Request, cause error) {
	ip := clientIP(r)
	if !a.limiter.Allow(ip) {
		// Debug: the IP must not appear above Debug (docs/42 §5).
		a.log.Debug("invalid credentials rate-limited", "ip", ip, "path", r.URL.Path)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many invalid credentials; slow down")
		return
	}
	a.log.Debug("rejected credential", "ip", ip, "path", r.URL.Path, "err", cause)
	// The client learns nothing beyond "not accepted": which check failed is
	// useful only to someone probing the boundary. The detail is in the log.
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
}

// bearerToken extracts the credential. A missing or malformed Authorization
// header is an invalid credential like any other — there is no anonymous mode.
func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", errors.New("no Authorization header")
	}
	scheme, value, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("Authorization header is not a Bearer credential")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty Bearer credential")
	}
	return value, nil
}

// clientIP keys the failure budget. r.RemoteAddr only — X-Forwarded-For is
// attacker-controlled, and honouring it without a trusted-proxy list would let
// one client mint a fresh budget per request, which is worse than the coarse
// bucketing an Ingress imposes.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// apiError is the error envelope docs/42 §4.7 specifies for the portal API.
// internal/api renders the same shape for its own errors; the two are
// deliberately independent (neither package imports the other) and the shape
// is one line, so this is a restatement, not a fork of logic.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Nothing this package emits is cacheable: /auth/config is a bootstrap the
	// SPA re-reads on load, and an auth failure must never be served from a
	// cache to the next request.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// signingAlgs narrows the provider's advertised algorithms to the asymmetric
// set this package accepts.
//
// The filter is the point: an IdP that advertises `HS256` or `none` would
// otherwise hand us the classic algorithm-confusion attack, where a public key
// that everyone can fetch doubles as an HMAC secret. An empty result leaves
// go-oidc on its RS256 default, which is the one algorithm OIDC mandates.
func signingAlgs(advertised []string) []string {
	allowed := map[string]bool{
		oidc.RS256: true, oidc.RS384: true, oidc.RS512: true,
		oidc.ES256: true, oidc.ES384: true, oidc.ES512: true,
		oidc.PS256: true, oidc.PS384: true, oidc.PS512: true,
		oidc.EdDSA: true,
	}
	var out []string
	for _, alg := range advertised {
		if allowed[alg] {
			out = append(out, alg)
		}
	}
	return out
}
