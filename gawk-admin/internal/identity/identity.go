// Package identity carries the authenticated caller across gawk-admin's
// request path (R39, docs/42 D17).
//
// It is a leaf on purpose. `internal/auth` validates the OIDC-issued JWT and
// PUTS an Identity here; `internal/api` READS it. Neither imports the other,
// so the authentication mechanism and the routes it protects can be built,
// reviewed and tested independently — and swapping the mechanism later touches
// one package rather than every handler.
//
// There is no session and no cookie anywhere in this service: the Identity is
// derived fresh from the bearer token on every request (D17), which is what
// makes the API stateless enough to run at replicaCount 2 (D16).
package identity

import "context"

// Identity is the authenticated caller, projected from validated JWT claims.
// Roles are the IdP-managed authorization state (D17) — never a config-file
// allowlist.
type Identity struct {
	// Subject is the token's `sub` claim: the stable, provider-scoped user ID.
	Subject string
	// Email is the `email` claim when the provider issues one. It is what
	// audit rows and webhook payloads render as the actor, so it is
	// informational — authorization never keys on it.
	Email string
	// Roles is the string array read from the configured roles claim.
	Roles []string
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

// NewContext returns ctx carrying id. Called by the authentication middleware
// once per request, after the token validates.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// FromContext returns the identity the middleware attached. The false case is
// a programming error on an authenticated route (the middleware refuses the
// request long before a handler runs) — handlers should treat it as a 500, not
// as an anonymous caller.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}
