// Package oidcauth is the one OIDC bearer-token verifier in this repository
// (R53, docs/55 D2): the relay's admin API (internal/ops), the moderation
// portal (gawk-admin) and the telemetry service all verify through it.
//
// R39 shipped the same verifier twice — once in the relay, once in the portal
// — and kept the copies in step by review (docs/42 §11.1, twice). What they
// would otherwise diverge on is security-critical: the JWKS fetch floor, the
// `alg` allowlist, the capped body, the discovery retry. A third consumer made
// sharing it cheaper than keeping three copies honest, so this package is the
// lifted union of the two, with the relay's containment rule extended to cover
// it (internal/ops/auth_import_test.go): inside the relay module only the ops
// auth path may import it.
//
// Every protected request proves who it is with an `Authorization: Bearer
// <JWT>` minted by the configured OIDC provider. Validation is stateless —
// signature against the issuer's JWKS, then `iss`, `aud`, `exp`, `nbf` — and
// authorization is a role read out of a claim in that same token. There is no
// session, no cookie and therefore no CSRF surface at all (docs/42 D17): a
// `Set-Cookie` on any response is a bug.
//
// The properties this package is built around:
//
//   - **Per-request verification is offline.** The JWKS is cached by
//     go-oidc's oidc.RemoteKeySet, whose cache has no expiry, so an operator
//     can act while the IdP is slow or down. The only request that reaches
//     the IdP is one whose signature no cached key verifies — a key rotation,
//     or a forgery — and even that fetch is rate-floored so a fuzzing loop
//     cannot turn into a stampede against the IdP (keyset.go).
//   - **The refresh horizon is the revocation horizon.** A JWT cannot be
//     revoked server-side before it expires, so removing a role at the IdP
//     takes effect at the next access-token refresh — which is exactly why
//     docs/42 §4.8 recommends 5–15 minute access tokens.
//   - **An unreachable IdP degrades the service; it does not kill the pod.**
//     Discovery happens in the background, with retries, so New never fails
//     on a transient IdP outage. Until it succeeds, Verify fails with
//     ErrNotReady, Middleware answers 401 and Ready reports false — the signal
//     a consumer folds into /readyz. Refusing to boot instead would turn a
//     30-second IdP blip into a CrashLoopBackOff, and would buy no safety: New
//     already refuses a configuration that could serve with authorization
//     effectively off (docs/42 D7).
//
// A consumer wires the exported surface like this (gawk-admin's shape):
//
//	v, err := oidcauth.New(ctx, cfg, oidcauth.Options{Logger: log})
//	mux.Handle("GET /auth/config", v.ConfigHandler())
//	mux.Handle("/api/v1/", v.Middleware(v.RequireRole(cfg.Role)(apiHandler)))
//	srv.Handler = oidcauth.SecurityHeaders(cfg.Issuer)(mux)
//	ready := store.Ready() && v.Ready()
//
// or calls Verify directly, as the relay's ops listener does to put its own
// static machine token in front of the same check.
//
// SecurityHeaders MUST wrap the whole mux: the SPA, /healthz and every other
// response need the CSP too, and only the mux-level wrap can guarantee that
// (docs/42 §4.8 "headers on every response"). Middleware and ConfigHandler set
// the same headers themselves so a protected response is never bare even if
// that wiring is forgotten; setting them twice is idempotent.
package oidcauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/Tuhis/gawk/gawk-server/oidcroles"
)

// Defaults. Every one of these is overridable through Options, but the zero
// Options value is what production runs.
const (
	// DefaultJWKSFetchInterval and DefaultJWKSFetchBurst size the JWKS fetch
	// bucket (keyset.go): three fetches per minute, bucket full at startup.
	// The burst is what makes a genuine rotation free — the herd it produces
	// is coalesced into one fetch upstream, and the bucket has three. The
	// interval is what an attacker feeding unverifiable tokens is reduced to,
	// and it is also the worst case a rotation landing mid-attack waits: 20
	// seconds, one retry away for the operator.
	DefaultJWKSFetchInterval = 20 * time.Second
	DefaultJWKSFetchBurst    = 3
	// defaultFetchTimeout bounds a single JWKS request. It is also the longest
	// a request can wait on the IdP, and only ever on a cache-miss path.
	defaultFetchTimeout = 5 * time.Second
	// defaultHTTPTimeout bounds discovery and JWKS requests end to end. Never
	// http.DefaultClient, whose zero timeout is an unbounded hang.
	defaultHTTPTimeout = 10 * time.Second
	// defaultFailureRate/Burst damp invalid-credential responses per client IP
	// (docs/42 §4.8). Ten failures back to back, then one per second: far
	// above what a browser tab does when its access token expires, far below
	// a fuzzing loop.
	defaultFailureRate  = 1.0
	defaultFailureBurst = 10
	// limiterSweepInterval evicts idle failure buckets.
	limiterSweepInterval = 5 * time.Minute
	// defaultResolveRetryInterval is the delay before the first retry of OIDC
	// discovery; it doubles up to the retry cap. Short at first because the
	// usual case is "the IdP is still starting alongside us".
	defaultResolveRetryInterval = time.Second
	// defaultResolveRetryMax caps the backoff. A service that cannot
	// authenticate is worth retrying forever, but not worth retrying hard.
	defaultResolveRetryMax = 30 * time.Second
)

// ErrNotReady is what Verify returns while OIDC discovery has not resolved:
// no credential can be judged yet. Callers answer it with 401 — nothing is
// broken, and retrying is the right reaction — and, unlike an invalid
// credential, it must not spend the caller's failure budget: the failure is
// ours, not theirs.
var ErrNotReady = errors.New("oidcauth: the identity provider has not been reached yet")

// Config is what a deployment configures. Issuer, Audience, RolesClaim and
// Role are required: each blank one would accept tokens it should not.
type Config struct {
	// Issuer is the OIDC issuer URL. Discovery is fetched from it, and a
	// discovery document whose own `issuer` differs is refused.
	Issuer string
	// Audience is the value a token's `aud` must carry — the resource server
	// the token was minted for, which for an access token is not the SPA's
	// client ID (docs/42 §4.12).
	Audience string
	// RolesClaim is the dot-path to the token's roles array, with
	// oidcroles.Placeholder substituted by Audience per segment.
	RolesClaim string
	// Role is the deployment's required role. New refuses it blank — with no
	// required role every valid token would be an operator — and Role()
	// returns it for a consumer's RequireRole wiring; Middleware itself
	// authorizes nothing.
	Role string
	// ClientID is the public SPA client ID that ConfigHandler publishes. New
	// does not require it, because a pure resource server (the relay) has no
	// browser client; a consumer that serves ConfigHandler must validate it
	// itself, and ConfigHandler panics on a blank one.
	ClientID string
}

// Options tunes the verifier. The zero value is production's configuration;
// each field exists because a test must drive it (clock, HTTP client) or a
// consumer must tune it.
type Options struct {
	// Logger receives Debug-level rejection detail and Warn-level discovery and
	// priming failures. Defaults to slog.Default(). Rejections carry the
	// client IP and the validation error, which is why they are Debug-only:
	// IPs must not appear in logs above Debug (docs/42 §5).
	Logger *slog.Logger
	// HTTPClient talks to the IdP — discovery at startup, JWKS afterwards.
	// Nil means a bounded-timeout default.
	HTTPClient *http.Client
	// Now defaults to time.Now. It drives token expiry checks, the JWKS fetch
	// bucket and the failure limiter.
	Now func() time.Time
	// JWKSFetchInterval and JWKSFetchBurst size the JWKS fetch bucket: one
	// token accrues per interval, the bucket holds burst of them and starts
	// full. Zero means the default (three fetches per minute). Only a
	// verification that no cached key satisfies ever spends one.
	JWKSFetchInterval time.Duration
	JWKSFetchBurst    int
	// ResolveRetryInterval is the delay before the first retry of OIDC
	// discovery, and of key-set priming, doubling up to ResolveRetryMax.
	// Defaults to one second.
	ResolveRetryInterval time.Duration
	// ResolveRetryMax caps that backoff; it is never below
	// ResolveRetryInterval. Defaults to 30 seconds.
	ResolveRetryMax time.Duration
	// FailureRate and FailureBurst size Middleware's per-IP invalid-credential
	// bucket. Zero means the defaults (ten, then one per second).
	FailureRate  float64
	FailureBurst int
	// SigningAlgorithms, when non-nil, is the exact JWS algorithm list a token
	// may use, instead of the provider's advertised
	// id_token_signing_alg_values_supported. Either way the list is narrowed
	// to AsymmetricSigningAlgs, so neither the provider nor a caller can widen
	// it to "none" or the HMAC family.
	SigningAlgorithms []string
}

// Verifier validates tokens and authorizes roles. Construct it with New; it
// owns a background goroutine (issuer resolution, key-set priming, limiter
// sweep) that Close — or the end of New's ctx — stops.
type Verifier struct {
	issuer   string
	clientID string
	audience string
	role     string
	// rolesPath is the configured dot-path, pre-split and with the audience
	// substituted. The walk lives in gawk-server's public oidcroles package:
	// R39 first shipped it twice, in two placeholder dialects, and only one
	// copy carried the dotted-audience bug — which is how a mirror hides a
	// defect instead of doubling it.
	rolesPath oidcroles.Path

	client       *http.Client
	now          func() time.Time
	resolveRetry time.Duration
	retryMax     time.Duration
	algs         []string // nil: the provider's advertised set
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

	// resolved closes when the verifier is published (Ready turns true).
	resolved chan struct{}
	// primed closes once the key set holds keys. It is strictly later than the
	// verifier being published and nothing gates on it: Ready, and every
	// authenticated route, go live the moment discovery resolves.
	primed chan struct{}

	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// New validates the configuration and starts the background worker that
// resolves the provider (OIDC discovery), retrying until it succeeds.
//
// It fails ONLY on a configuration that could never be safe — a blank issuer,
// audience, roles-claim path or role. That is docs/42 D7's actual concern, and
// it is decided without touching the network. An IdP that is merely
// unreachable is a transient condition: New succeeds, Ready reports false,
// Verify answers ErrNotReady, and the process stays up (docs/42 §6).
//
// ctx bounds the background worker, so it must be the process context, not a
// request's.
func New(ctx context.Context, cfg Config, opts Options) (*Verifier, error) {
	// A programmatic caller must hit the same refusals a flag parser does:
	// "no roles claim" or "no required role" would authorize every valid
	// token (docs/42 §4.8, AP5).
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("oidcauth: OIDC issuer must not be empty")
	}
	if strings.TrimSpace(cfg.Audience) == "" {
		return nil, errors.New("oidcauth: OIDC audience must not be empty: an unvalidated audience accepts tokens minted for another application")
	}
	if strings.TrimSpace(cfg.Role) == "" {
		return nil, errors.New("oidcauth: required role must not be empty: with no required role every valid token would be an operator")
	}
	// Substituted with the AUDIENCE, not the SPA's client ID: the roles that
	// govern this API are the ones the IdP put under the resource server the
	// token was minted for, which is what `aud` names (docs/42 §4.12).
	rolesPath, err := oidcroles.ParsePath(cfg.RolesClaim, cfg.Audience)
	if err != nil {
		return nil, fmt.Errorf("oidcauth: roles claim path: %w", err)
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
	retryMax := opts.ResolveRetryMax
	if retryMax <= 0 {
		retryMax = defaultResolveRetryMax
	}
	rate := opts.FailureRate
	if rate <= 0 {
		rate = defaultFailureRate
	}
	burst := opts.FailureBurst
	if burst <= 0 {
		burst = defaultFailureBurst
	}
	var algs []string
	if opts.SigningAlgorithms != nil {
		algs = signingAlgs(opts.SigningAlgorithms)
	}

	runCtx, cancel := context.WithCancel(ctx)
	v := &Verifier{
		// The configured issuer is what /auth/config publishes and what the
		// CSP allows, both of which must work before (and during) an IdP
		// outage — the SPA can still be bounced to the IdP, which is where
		// that failure belongs. Discovery only ever confirms this string:
		// go-oidc refuses a document whose own `issuer` differs.
		issuer:       cfg.Issuer,
		clientID:     cfg.ClientID,
		audience:     cfg.Audience,
		role:         cfg.Role,
		rolesPath:    rolesPath,
		client:       client,
		now:          now,
		resolveRetry: resolveRetry,
		retryMax:     max(retryMax, resolveRetry),
		algs:         algs,
		throttle:     newJWKSThrottle(opts.JWKSFetchInterval, opts.JWKSFetchBurst, now),
		limiter:      newIPLimiter(rate, burst, now),
		log:          log,
		csp:          buildCSP(cfg.Issuer),
		resolved:     make(chan struct{}),
		primed:       make(chan struct{}),
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	go v.run(runCtx)
	return v, nil
}

// Role is the deployment's required role (Config.Role).
func (v *Verifier) Role() string { return v.role }

// Ready reports whether the provider has been resolved: OIDC discovery
// answered and the verifier exists. Until it does, every token is refused, so
// a consumer folds this into /readyz — an unready pod should not take
// authenticated traffic.
//
// It never goes back to false, and it deliberately does NOT wait for the key
// set: startup must not be able to hang on the IdP twice (docs/42 §6, D16).
// The key set is instead primed right after resolution, in the background and
// with retries (primeKeys), so a ready pod can normally verify offline without
// that ever having been a precondition for going ready.
func (v *Verifier) Ready() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.verifier != nil
}

// ResolveError returns why the provider is not resolved yet, or nil once it
// is (and before the first attempt has failed). It exists so /readyz can say
// *what* is wrong rather than just "not ready".
func (v *Verifier) ResolveError() error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.verifier != nil {
		return nil
	}
	return v.resolveErr
}

// Resolved is closed once discovery has resolved — the moment Ready turns
// true. Nothing in the request path waits on it; it lets a test or a startup
// probe wait without polling.
func (v *Verifier) Resolved() <-chan struct{} { return v.resolved }

// Primed is closed once the key set holds keys, which is strictly later than
// Resolved. Nothing gates on it: a ready verifier whose key set is not primed
// yet fetches on the first verification instead.
func (v *Verifier) Primed() <-chan struct{} { return v.primed }

// JWKSFetchTokensLeft reports the JWKS fetch bucket's contents WITHOUT
// refilling or spending. It is diagnostic: "this verification did not touch
// the IdP" is the guarantee docs/42 §4.5 and §4.8 rest on, and an unchanged,
// non-empty bucket is direct evidence that the fetch path was not consulted.
func (v *Verifier) JWKSFetchTokensLeft() float64 { return v.throttle.tokensLeft() }

// currentVerifier returns the current verifier, or false while unresolved.
func (v *Verifier) currentVerifier() (*oidc.IDTokenVerifier, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.verifier, v.verifier != nil
}

// Verify validates rawJWT — signature, `iss`, `aud`, `exp`, `nbf`, algorithm —
// and projects it onto an Identity. It authorizes nothing: a token whose roles
// claim is missing or malformed verifies with an empty role set, so a role
// check then refuses it (403, never a 500 — a claim shape we do not recognise
// is the IdP's configuration talking, not a bug in this process).
//
// It returns ErrNotReady while discovery has not resolved, and any other
// error for a credential that is not good. In the steady state it is offline:
// the key that signed the token is already cached and the check is pure CPU.
func (v *Verifier) Verify(ctx context.Context, rawJWT string) (Identity, error) {
	verifier, ready := v.currentVerifier()
	if !ready {
		return Identity{}, ErrNotReady
	}
	token, err := verifier.Verify(ctx, rawJWT)
	if err != nil {
		return Identity{}, err
	}
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		// The payload verified but is not a JSON object. An invalid
		// credential rather than an internal error: nothing on our side is
		// broken.
		return Identity{}, fmt.Errorf("decoding claims: %w", err)
	}

	// Email is optional: a client-credentials service identity (R40's
	// sampler, docs/42 §4.11) has no user behind it, and Identity.Actor()
	// falls back to the subject so an audit row is never blank.
	id := Identity{Subject: token.Subject}
	if email, ok := claims["email"].(string); ok {
		id.Email = email
	}
	roles, err := v.rolesPath.Roles(claims)
	if err != nil {
		// Debug only: this is a legitimately authenticated caller whose token
		// does not carry the claim we were told to read.
		v.log.Debug("roles claim unusable", "path", v.rolesPath.String(), "err", err)
	}
	id.Roles = roles
	return id, nil
}

// keySource bundles the key set with the transport that fetches for it.
// primeKeys needs both: only the key set can be made to fetch, and only the
// transport carries the throttle exemption and the fetch counter.
type keySource struct {
	keys    *oidc.RemoteKeySet
	fetcher *throttledTransport
}

// resolve performs OIDC discovery and publishes the verifier, after which
// Ready is true forever. It does not itself touch the JWKS — that is
// primeKeys' job, deliberately after this returns, so readiness never waits
// on the key set (keyset.go).
func (v *Verifier) resolve(ctx context.Context) (*keySource, error) {
	// go-oidc verifies that the document's own `issuer` matches the URL we
	// asked for, which is the check that makes the rest of the validation
	// meaningful — it stops a hijacked discovery URL pointing us at somebody
	// else's keys.
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, v.client), v.issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery for %q: %w", v.issuer, err)
	}
	// go-oidc exposes neither the issuer it validated nor the jwks_uri as
	// fields, so read them back off the raw discovery document. Issuer is
	// already guaranteed to equal v.issuer — NewProvider refuses the mismatch
	// — and using the discovered spelling keeps the value the token's `iss` is
	// compared against and the value the provider published the same string,
	// byte for byte.
	var meta struct {
		Issuer     string   `json:"issuer"`
		JWKSURL    string   `json:"jwks_uri"`
		Algorithms []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("reading provider metadata: %w", err)
	}
	if meta.JWKSURL == "" {
		return nil, fmt.Errorf("provider %q advertises no jwks_uri", v.issuer)
	}
	if meta.Issuer == "" {
		meta.Issuer = v.issuer
	}
	algs := v.algs
	if algs == nil {
		algs = signingAlgs(meta.Algorithms)
	}

	keys, fetcher := newRemoteKeySet(ctx, meta.JWKSURL, v.client, v.throttle, v.log)
	verifier := oidc.NewVerifier(meta.Issuer, keys, &oidc.Config{
		// go-oidc names this ClientID; it is the value compared against the
		// token's `aud`, which for an access token is the audience the IdP
		// stamped, not the SPA's client ID (docs/42 §4.12).
		ClientID:             v.audience,
		SupportedSigningAlgs: algs,
		Now:                  v.now,
	})

	v.mu.Lock()
	v.verifier = verifier
	failures := v.failures
	v.resolveErr = nil
	v.mu.Unlock()
	close(v.resolved)

	if failures > 0 {
		// A state change an operator watching the log needs to see: the
		// service went from "up but unusable" to "usable".
		v.log.Warn("oidc issuer resolved: authentication is available again",
			"issuer", v.issuer, "failedAttempts", failures)
	} else {
		v.log.Info("oidc issuer resolved", "issuer", v.issuer)
	}
	return &keySource{keys: keys, fetcher: fetcher}, nil
}

// primingJWS is a syntactically valid compact JWS that nothing can ever
// verify: `{"alg":"RS256","kid":"gawk-key-set-priming"}` over `{}`, signed with
// the ASCII bytes "priming".
//
// It exists because oidc.RemoteKeySet exposes no "fetch now": the only way in
// is a verification whose `kid` misses the cache, which is precisely what this
// string is. VerifySignature parses it, finds no key, fetches the JWKS — the
// point of the exercise — then fails the signature check, and the error is
// discarded. Nothing is ever trusted from it; a priming attempt can warm a
// cache and cannot authenticate anybody.
const primingJWS = "eyJhbGciOiJSUzI1NiIsImtpZCI6Imdhd2sta2V5LXNldC1wcmltaW5nIn0.e30.cHJpbWluZw"

// primeKeys fetches the key set once, in the background, retrying until it
// lands or ctx ends.
//
// WHY IT IS NOT LAZY. oidc.RemoteKeySet fetches on the first verification that
// misses its cache, and every pod restarts with that cache empty — so after a
// rolling deploy every replica needs one IdP round trip at the moment its
// first operator arrives, which may be hours later and mid-incident. An IdP
// that is down across that window 401s a still-valid token on every fresh pod,
// and a mix of warm and cold replicas behind one Service flaps. docs/42 §6
// makes the IdP availability-critical for the portal and never for
// enforcement; lazy priming quietly extends that criticality to
// first-use-per-pod-lifetime. One bounded GET per pod restart is much cheaper
// than the failure it prevents.
//
// WHY IT GATES NOTHING. It runs after the verifier is published, so Ready and
// every authenticated route are live from the moment discovery resolves.
// Startup still cannot depend on the IdP (D16).
func (v *Verifier) primeKeys(ctx context.Context, src *keySource) {
	backoff := v.resolveRetry
	for {
		before := src.fetcher.fetched.Load()
		// Throttle-exempt: this fetch neither spends from the bucket a genuine
		// key rotation draws on nor can be refused by it.
		src.fetcher.exempt.Store(true)
		_, _ = src.keys.VerifySignature(ctx, primingJWS)
		// A fetch the IdP actually served is the signal, not the verification
		// error — that one is non-nil either way, and go-oidc reports "the
		// fetch failed" and "no key matched" as the same kind of value.
		if src.fetcher.fetched.Load() > before {
			// Coalescing may have carried this attempt on somebody else's
			// fetch, leaving the exemption unspent; drop it rather than bank a
			// free fetch forever.
			src.fetcher.exempt.Store(false)
			close(v.primed)
			v.log.Info("oidc key set primed", "issuer", v.issuer)
			return
		}
		if ctx.Err() != nil {
			return
		}
		v.log.Warn("oidc key set not primed: the first operator after this restart will need the IdP",
			"issuer", v.issuer, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, v.retryMax)
	}
}

// noteResolveFailure records why we are still unresolved and says so once,
// loudly. Repeats drop to Debug: an IdP that is down for an hour should not
// bury every other line in the log, and the state has not changed.
func (v *Verifier) noteResolveFailure(err error, retryIn time.Duration) {
	v.mu.Lock()
	v.failures++
	attempts := v.failures
	v.resolveErr = err
	v.mu.Unlock()

	if attempts == 1 {
		// The wording is the portal's (R39 AP5 asserts it verbatim); it reads
		// the same for every consumer that serves while unresolved.
		v.log.Warn("oidc issuer unresolved: the portal is serving but no request can authenticate until the IdP answers",
			"issuer", v.issuer, "err", err, "retryIn", retryIn.String())
		return
	}
	v.log.Debug("oidc issuer still unresolved",
		"issuer", v.issuer, "attempts", attempts, "err", err, "retryIn", retryIn.String())
}

// Close stops the background worker and waits for it. Idempotent.
func (v *Verifier) Close() error {
	v.closeOnce.Do(func() {
		v.cancel()
		<-v.done
	})
	return nil
}

// run is the background worker: retry discovery until it resolves, prime the
// key set once it has, and evict limiter buckets throughout — 401s (and
// therefore failure budgets) exist before resolution as well as after.
//
// Its only JWKS traffic is that one priming fetch. There is no periodic
// refresh: oidc.RemoteKeySet fetches on a verification miss and its cache
// never expires, so a periodic refresh would be a request to the IdP that
// changes nothing, on every replica, forever.
func (v *Verifier) run(ctx context.Context) {
	// Priming runs beside this loop rather than inside it, so a slow or
	// retrying IdP cannot stall the limiter sweep; Close still waits for it,
	// because a goroutine outliving Close is a leak in every test that makes
	// one.
	var priming sync.WaitGroup
	defer func() {
		priming.Wait()
		close(v.done)
	}()

	sweep := time.NewTicker(limiterSweepInterval)
	defer sweep.Stop()
	// Fire immediately: the common case is an IdP that is already up.
	attempt := time.NewTimer(0)
	defer attempt.Stop()

	backoff := v.resolveRetry
	for {
		var attemptC <-chan time.Time
		if attempt != nil {
			attemptC = attempt.C
		}

		select {
		case <-ctx.Done():
			return

		case <-sweep.C:
			v.limiter.sweep()

		case <-attemptC:
			src, err := v.resolve(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // shutting down, not a real failure
				}
				v.noteResolveFailure(err, backoff)
				attempt.Reset(backoff)
				backoff = min(2*backoff, v.retryMax)
				continue
			}
			// Resolution happens once. From here the only IdP traffic is the
			// priming fetch and, later, a throttled fetch behind a
			// verification miss.
			priming.Add(1)
			go func() {
				defer priming.Done()
				v.primeKeys(ctx, src)
			}()
			attempt.Stop()
			attempt = nil
		}
	}
}

// asymmetricAlgs is the allowlist of JWS algorithms a token may use.
// Asymmetric only: "none" and the HMAC family must never be reachable — an IdP
// that advertises HS256 would otherwise hand us the classic
// algorithm-confusion attack, where a public key everyone can fetch doubles as
// an HMAC secret.
var asymmetricAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512,
	oidc.EdDSA,
}

// AsymmetricSigningAlgs returns (a copy of) the asymmetric JWS algorithm
// allowlist every verification is narrowed to.
func AsymmetricSigningAlgs() []string { return append([]string(nil), asymmetricAlgs...) }

// signingAlgs narrows a list of algorithms to the asymmetric allowlist.
//
// The filter is the point: neither a provider's discovery document nor a
// caller's Options may widen the set. An empty result leaves go-oidc on its
// RS256 default, which is the one algorithm OIDC mandates.
func signingAlgs(advertised []string) []string {
	allowed := make(map[string]bool, len(asymmetricAlgs))
	for _, alg := range asymmetricAlgs {
		allowed[alg] = true
	}
	var out []string
	for _, alg := range advertised {
		if allowed[alg] {
			out = append(out, alg)
		}
	}
	return out
}
