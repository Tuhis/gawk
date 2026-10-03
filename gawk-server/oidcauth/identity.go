package oidcauth

import (
	"context"
	"time"
)

// Identity is the authenticated caller, projected from validated JWT claims.
// Roles are the IdP-managed authorization state (docs/42 D17) — never a
// config-file allowlist.
//
// It is derived fresh from the bearer token on every request: there is no
// session and no cookie anywhere behind this package, which is what keeps its
// consumers stateless enough to run more than one replica (docs/42 D16).
type Identity struct {
	// Subject is the token's `sub` claim: the stable, provider-scoped user ID.
	Subject string
	// Email is the `email` claim when the provider issues one. It is what
	// audit rows and webhook payloads render as the actor, so it is
	// informational — authorization never keys on it.
	Email string
	// Roles is the string array read from the configured roles claim. Empty
	// when that claim is missing or malformed: a token that cannot prove a
	// role does not have it.
	Roles []string
	// Expiry is the token's validated `exp`. Authorization is decided per
	// request, so a consumer that holds one response open beyond the request
	// that authorized it — telemetry's live stream (docs/55 D4) — must end
	// that response here, or the stream outlives the revocation horizon.
	Expiry time.Time
}

// Actor is what an audit row or an event records for this caller. Email when
// the provider issued one, subject otherwise — an audit trail with a blank
// actor is worse than one with an opaque ID.
func (i Identity) Actor() string {
	if i.Email != "" {
		return i.Email
	}
	return i.Subject
}

// HasRole reports whether the token carried role. Comparison is exact and
// case-sensitive: OIDC role names are opaque strings and folding case here
// would silently widen access.
func (i Identity) HasRole(role string) bool {
	for _, r := range i.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type contextKey struct{}

// NewContext returns ctx carrying id. Called by Middleware once per request,
// after the token validates.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity Middleware attached. The false case is a
// programming error on an authenticated route (Middleware refuses the request
// long before a handler runs) — handlers should treat it as a 500, not as an
// anonymous caller.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}
