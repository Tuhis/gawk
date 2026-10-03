// Package identity carries the authenticated caller across gawk-admin's
// request path (R39, docs/42 D17).
//
// It is a leaf on purpose. `internal/auth` validates the OIDC-issued JWT and
// PUTS an Identity here; `internal/api` READS it. Neither imports the other,
// so the authentication mechanism and the routes it protects can be built,
// reviewed and tested independently.
//
// Since R53 (docs/55 D2) the type and its context key live in gawk-server's
// public oidcauth package, the verifier shared by the relay, this portal and
// telemetry; the names here are aliases of those, not a copy — a value put on
// the context by oidcauth's Middleware is the value FromContext returns.
//
// There is no session and no cookie anywhere in this service: the Identity is
// derived fresh from the bearer token on every request (D17), which is what
// makes the API stateless enough to run at replicaCount 2 (D16).
package identity

import (
	"context"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
)

// Identity is the authenticated caller, projected from validated JWT claims
// (oidcauth.Identity).
type Identity = oidcauth.Identity

// NewContext returns ctx carrying id (oidcauth.NewContext).
func NewContext(ctx context.Context, id Identity) context.Context {
	return oidcauth.NewContext(ctx, id)
}

// FromContext returns the identity the middleware attached
// (oidcauth.FromContext). The false case is a programming error on an
// authenticated route — handlers should treat it as a 500, not as an
// anonymous caller.
func FromContext(ctx context.Context) (Identity, bool) {
	return oidcauth.FromContext(ctx)
}
