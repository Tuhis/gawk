package auth

// JWKS handling for the portal's token verification (docs/42 §4.8).
//
// The key set is go-oidc's own oidc.RemoteKeySet. R39 shipped a bespoke cache
// instead, on the premise that RemoteKeySet could not give §4.8 its two named
// properties. Re-reading the upstream source (go-oidc v3.20.0, oidc/jwks.go)
// says otherwise — it already has all three of the properties that mattered:
//
//   - **Cached keys never expire.** keysFromCache() has no freshness check at
//     all, so once a `kid` is in the cache every later token signed by it
//     verifies from memory. Steady-state verification is pure CPU, and an IdP
//     outage cannot break it. That is the guarantee §4.8 and §6 rest on, and
//     it is the one the bespoke cache existed to provide.
//   - **Concurrent misses share one fetch.** keysFromRemote() coalesces
//     through an inflight record, so a rotation's thundering herd — every
//     operator's next token carrying a key this process has not seen — costs
//     one round trip, and every waiter is answered from that fetch's result
//     rather than from the cache it was queued to replace. Hand-rolling that
//     is what produced R39's own generation-snapshot race, which would have
//     401'd the whole fleet through a key rotation.
//   - **A failed fetch leaves the cache alone.** cachedKeys is replaced only
//     on success, so losing contact with the IdP never revokes access that is
//     already working.
//
// It is also at least as strict as the bespoke key filter was: `alg` values
// outside the asymmetric set are skipped when the JWKS is decoded, and the
// verifier's own SupportedSigningAlgs allowlist (auth.go, signingAlgs) is
// applied before any key is tried — so "none" and the HMAC family are
// unreachable from both ends.
//
// What upstream does NOT have is a floor on how often a verification may
// reach the network, nor any way to ask it to fetch. This file fills the first
// gap; auth.go's primeKeys works around the second, using the exemption and
// the fetch counter this transport exposes.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// maxJWKSBytes bounds what we will read from the IdP. A JWKS is a handful of
// kilobytes; anything past this is a misconfigured or hostile endpoint, and
// this process must not be knocked over by it. go-oidc reads the body with an
// unbounded io.ReadAll, so the limit is applied here, on the response body the
// throttling transport hands back.
const maxJWKSBytes = 512 << 10

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
// interval: 20 seconds at the defaults (defaultJWKSFetchInterval, burst
// defaultJWKSFetchBurst = 3 fetches per minute). An operator whose token was
// minted by the new key inside that window retries and is in.
//
// This type — and auth.go's primeKeys, which spends its exemption — is a
// deliberate twin of gawk-server's internal/ops/auth.go. Sharing them would
// mean a public package importing go-oidc, importable by the whole relay,
// which is the dependency containment gawk-server's auth_import_test.go
// exists to hold. The roles-claim walk moved to gawk-server/oidcroles instead,
// precisely because it needs no OIDC library at all.
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
// IdP" is the guarantee §4.8 rests on and the hardest one to observe from
// outside, and an unchanged, non-empty bucket is direct evidence that the
// fetch path was never even consulted.
func (t *jwksThrottle) tokensLeft() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokens
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

	// exempt lets exactly one request past the rate floor, and is how startup
	// priming is "throttle-exempt" (auth.go, primeKeys). A bool rather than a
	// counter: an attempt that granted an exemption and then rode somebody
	// else's in-flight fetch must not leave a second one banked.
	exempt atomic.Bool
	// fetched counts JWKS responses the IdP actually served. It is the only
	// way to tell a landed fetch from a failed one without matching on
	// go-oidc's error strings — RemoteKeySet.verify wraps both in an error.
	fetched atomic.Int64
}

func (t *throttledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.exempt.CompareAndSwap(true, false) && !t.throttle.allow() {
		// RoundTrip owns the body once it is called, on the error path too.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		t.log.Debug("jwks fetch throttled", "url", req.URL.String())
		return nil, errJWKSThrottled
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.fetched.Add(1)
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

// newRemoteKeySet builds the throttled key set for jwksURL, and returns the
// transport alongside it — priming needs to grant that transport its exemption
// and read back whether a fetch landed.
//
// base supplies the transport (so an injected client — a test's counting
// RoundTripper, an operator's proxy — still applies) but not the timeout: a
// JWKS fetch happens on the REQUEST path here, so it gets the tighter of
// defaultFetchTimeout and whatever bound the caller set.
func newRemoteKeySet(ctx context.Context, jwksURL string, base *http.Client, throttle *jwksThrottle, log *slog.Logger) (*oidc.RemoteKeySet, *throttledTransport) {
	timeout := defaultFetchTimeout
	if base.Timeout > 0 && base.Timeout < timeout {
		timeout = base.Timeout
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	fetcher := &throttledTransport{base: transport, throttle: throttle, log: log}
	client := &http.Client{Transport: fetcher, Timeout: timeout}
	// RemoteKeySet reads its HTTP client off this context and deliberately
	// drops the cancellation (context.WithoutCancel, upstream), so a fetch is
	// bounded by the client timeout alone — which is why that timeout must
	// never be zero, and why shutdown never waits on an in-flight fetch.
	return oidc.NewRemoteKeySet(oidc.ClientContext(ctx, client), jwksURL), fetcher
}
