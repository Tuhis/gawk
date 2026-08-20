package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// maxJWKSBytes bounds what we will read from the IdP. A JWKS is a handful of
// kilobytes; anything past this is a misconfigured or hostile endpoint, and
// this process must not be knocked over by it.
const maxJWKSBytes = 512 << 10

// errRefreshThrottled means an unknown-key refresh was skipped because one was
// attempted too recently. The token is refused; the next one after the floor
// elapses gets a real fetch.
var errRefreshThrottled = errors.New("jwks refresh throttled")

// keySet is the cached, background-refreshed JWKS §4.8 requires.
//
// go-oidc ships oidc.RemoteKeySet, which caches keys but (a) never refreshes
// them in the background and (b) puts no floor on how often an unknown `kid`
// may trigger a fetch. §4.8 asks for both by name — "cached,
// background-refreshed — per-request verification is offline" — so the cache
// is ours. The signature check itself still runs inside go-oidc
// (oidc.StaticKeySet), which keeps the crypto in one reviewed place.
type keySet struct {
	url    string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	// minRefresh floors how often a verification miss may reach the IdP. A
	// token signed by an unseen key is either a rotation (rare) or a forgery
	// (possibly a tight loop); only the first deserves a network round trip.
	minRefresh time.Duration
	// fetchTimeout bounds one JWKS request. It is also the longest a request
	// can wait on the IdP, and only ever on the unknown-key path.
	fetchTimeout time.Duration

	// fetchMu serializes fetches so a burst of unknown-key requests produces
	// one round trip, not one each. It is never held while verifying.
	fetchMu sync.Mutex

	mu   sync.RWMutex
	keys []crypto.PublicKey
	// generation counts successful cache replacements. A request that waited
	// on fetchMu compares it to decide whether someone else's refresh already
	// did its work.
	generation uint64
	// lastOnDemand is when a VERIFICATION last triggered a fetch. Startup and
	// background refreshes deliberately do not touch it: if they did, the
	// refresh that runs seconds before a key rotation would spend the budget
	// and every operator would be refused until the floor elapsed.
	lastOnDemand time.Time

	// testHookAfterCachedVerify runs between the cached verification and the
	// on-demand refresh, so a test can land another request's refresh exactly
	// in that window without goroutines or timing. Nil in production; the same
	// seam R1's post-upgrade race needed (CODE-REVIEW.md).
	testHookAfterCachedVerify func()
}

// VerifySignature implements oidc.KeySet.
//
// The cached path is pure CPU: with the issuer switched off, every token
// signed by a key we already hold still validates. That is the guarantee the
// portal's availability rests on (§4.8, D7).
func (k *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	// Keys and generation are snapshotted TOGETHER, and the generation is the
	// one that belongs to the key set this verification actually tried. Taking
	// it later — inside refreshForVerify — makes a refresh that lands in
	// between invisible: the waiter then sees an unchanged generation, hits the
	// floor, and answers from the cache it was queued to replace. That was a
	// live 401-during-rotation bug, reproduced by
	// TestRefreshLandingBetweenCachedVerifyAndRefreshIsNoticed.
	keys, seen := k.snapshot()
	payload, cachedErr := verifyWith(ctx, keys, jwt)
	if cachedErr == nil {
		return payload, nil
	}
	if hook := k.testHookAfterCachedVerify; hook != nil {
		hook()
	}
	// No cached key verified this token. Either the IdP rotated its signing
	// key, or the token is a forgery. Refresh (at most once per minRefresh)
	// and try again — the rotation case must not require a restart, and the
	// forgery case must not become a stampede against the IdP.
	if err := k.refreshForVerify(ctx, seen); err != nil {
		if errors.Is(err, errRefreshThrottled) {
			return nil, cachedErr
		}
		return nil, fmt.Errorf("%w (jwks refresh failed: %v)", cachedErr, err)
	}
	keys, _ = k.snapshot()
	return verifyWith(ctx, keys, jwt)
}

// snapshot returns the current keys and the generation they belong to. The
// slice is replaced wholesale on every refresh and never mutated in place, so
// the snapshot is safe to use unlocked.
func (k *keySet) snapshot() ([]crypto.PublicKey, uint64) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.keys, k.generation
}

// verifyWith runs the signature check inside go-oidc, which keeps the crypto
// in one reviewed place.
func verifyWith(ctx context.Context, keys []crypto.PublicKey, jwt string) ([]byte, error) {
	return (&oidc.StaticKeySet{PublicKeys: keys}).VerifySignature(ctx, jwt)
}

// fetch replaces the cache unconditionally, honouring ctx. Used to prime at
// startup and by the background refresher, whose ctx is cancelled at shutdown
// — an in-flight refresh must not hold the process open for the fetch timeout.
func (k *keySet) fetch(ctx context.Context) error {
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()
	return k.fetchLocked(ctx)
}

// refreshForVerify is the on-demand path: floored by minRefresh, and shared by
// everyone who arrives while it runs. A nil return means "the cache is now as
// fresh as a fetch of our own would make it" — the caller retries against it.
//
// seen is the generation of the key set whose verification failed, captured by
// the caller before it ran that verification.
func (k *keySet) refreshForVerify(ctx context.Context, seen uint64) error {
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()

	k.mu.RLock()
	current, last := k.generation, k.lastOnDemand
	k.mu.RUnlock()
	if current != seen {
		// The key set changed since the verification that failed — someone
		// else's refresh already did our work, whether it landed while we
		// waited for the lock or just before we asked for it. A rotation is a
		// thundering herd — every operator's next token carries the new key at
		// once — so answering these from the stale cache we were queued to
		// replace would 401 the whole fleet off one refresh.
		return nil
	}
	// Throttle on the last on-demand *attempt*, not on its success: an IdP
	// that is down must not turn every forged token into another failing round
	// trip.
	if !last.IsZero() && k.now().Sub(last) < k.minRefresh {
		return errRefreshThrottled
	}
	k.mu.Lock()
	k.lastOnDemand = k.now()
	k.mu.Unlock()
	// Detached: this fetch serves every request waiting behind it, so the one
	// that happened to trigger it going away must not cancel it. The timeout
	// inside fetchLocked keeps it bounded.
	return k.fetchLocked(context.WithoutCancel(ctx))
}

// fetchLocked performs the request. Callers hold fetchMu.
func (k *keySet) fetchLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return fmt.Errorf("reading jwks: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned %s", resp.Status)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}

	k.mu.Lock()
	k.keys = keys
	k.generation++
	k.mu.Unlock()
	k.log.Debug("jwks refreshed", "url", k.url, "keys", len(keys))
	return nil
}

// parseJWKS decodes the key set, keeping only keys this package will verify
// with.
//
// Keys are decoded one at a time so that a single key using an algorithm we do
// not know (providers do ship such keys — ES256K, post-quantum experiments)
// cannot invalidate the whole set. **Only asymmetric public keys survive the
// filter**: a symmetric (`oct`) key from a public JWKS endpoint is the
// algorithm-confusion attack in raw form, and oidc.StaticKeySet rejects the
// entire set if it is handed one.
func parseJWKS(body []byte) ([]crypto.PublicKey, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding jwks: %w", err)
	}
	var out []crypto.PublicKey
	for _, entry := range raw.Keys {
		var jwk jose.JSONWebKey
		if err := json.Unmarshal(entry, &jwk); err != nil {
			continue
		}
		if jwk.Use == "enc" {
			continue
		}
		switch jwk.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			out = append(out, jwk.Key)
		default:
			continue
		}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks contains no usable public keys")
	}
	return out, nil
}
