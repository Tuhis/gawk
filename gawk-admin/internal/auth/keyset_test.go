package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func newTestKeySet(t *testing.T, idp *fakeIDP) *keySet {
	t.Helper()
	k := &keySet{
		url:        idp.url() + "/keys",
		client:     &http.Client{Timeout: 5 * time.Second},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        time.Now,
		minRefresh: defaultJWKSMinRefreshInterval,
		//nolint:mnd // mirrors the production default
		fetchTimeout: defaultFetchTimeout,
	}
	if err := k.fetch(context.Background()); err != nil {
		t.Fatalf("priming the key set: %v", err)
	}
	return k
}

// The interleaving the rotation herd actually hits: this request reads the
// pre-rotation cache, fails, and only THEN does another request's refresh land.
// The refresh it was about to perform has already happened, so it must retry
// against the new keys — not fall through to the throttle and answer from the
// cache it was queued to replace.
//
// This is the flake behind the concurrent herd test, made deterministic: the
// window is between the cached verification and the refresh decision, so the
// test drives it with the hook rather than with goroutines.
func TestRefreshLandingBetweenCachedVerifyAndRefreshIsNoticed(t *testing.T) {
	idp := newFakeIDP(t)
	k := newTestKeySet(t, idp)
	ctx := context.Background()

	idp.useKey("key-b", keyB())
	mine := idp.mint(t, idp.claims())
	theirs := idp.mint(t, idp.claims())

	fired := false
	k.testHookAfterCachedVerify = func() {
		if fired {
			return
		}
		fired = true
		// Another operator's request completes in full — including the
		// on-demand refresh that spends the floor's budget.
		if _, err := k.VerifySignature(ctx, theirs); err != nil {
			t.Errorf("the concurrent request's own verification failed: %v", err)
		}
	}

	if _, err := k.VerifySignature(ctx, mine); err != nil {
		t.Fatalf("verification after a concurrent refresh: %v", err)
	}
	if !fired {
		t.Fatal("the hook never ran; the test did not exercise the window it claims to")
	}
	// One fetch for both requests: the floor still holds.
	if got := idp.keyFetches.Load(); got != 2 {
		t.Errorf("JWKS fetches = %d, want 2 (one to prime, one for the rotation)", got)
	}
}
