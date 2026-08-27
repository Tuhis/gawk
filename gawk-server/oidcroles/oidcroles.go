// Package oidcroles resolves the roles claim inside an OIDC token: one
// implementation of "which roles does this token carry?", shared by the relay's
// ops listener (gawk-server/internal/ops) and the portal (gawk-admin's
// internal/auth).
//
// It is public in gawk-server for the same reason `moderation` and `wire` are:
// gawk-admin already imports this module, and a second copy of an
// authorization rule is a second answer that drifts. R39 shipped exactly that
// — two dot-path walks, spelling the placeholder `{audience}` on one side and
// `{clientId}` on the other — and only one of them carried the dotted-audience
// bug, which is how a mirror hides a defect rather than doubling it.
//
// It deliberately takes decoded claims (map[string]any) rather than an
// *oidc.IDToken, so it imports no OIDC library at all: internal/ops/auth.go
// stays the relay's ONLY reachable go-oidc import, which auth_import_test.go
// asserts. Signature verification is that file's job; deciding what the
// verified claims mean is this one's.
//
// THE PLACEHOLDER IS {audience}. It names the resource server the token was
// minted for — the same value the token's `aud` is checked against — which is
// the Keycloak client whose `resource_access` entry carries the API's roles. A
// portal's public SPA client ID is a different thing that usually holds no API
// roles; that the reference deployment gives both the same name is a
// convenience of the recipe, not the model. It is also the only identifier
// both callers have: the relay's admin auth knows an issuer and an audience
// and has no client-ID concept to substitute.
package oidcroles

import (
	"errors"
	"fmt"
	"strings"
)

// Placeholder is substituted into a roles-claim template, per path segment.
const Placeholder = "{audience}"

// DefaultClaim is Keycloak's client-roles path, and the default both
// -admin-oidc-roles-claim (relay) and -oidc-roles-claim (portal) carry.
const DefaultClaim = "resource_access." + Placeholder + ".roles"

// Path is a parsed roles-claim dot-path: the sequence of claim keys to walk.
type Path []string

// String renders the path for a log line or an error. It joins with "." and so
// cannot represent a segment that contains one — which is exactly why the
// errors below name the offending segment separately.
func (p Path) String() string { return strings.Join(p, ".") }

// ParsePath splits a roles-claim template into segments and substitutes
// Placeholder with audience.
//
// THE SPLIT HAPPENS FIRST, AND THE SUBSTITUTION IS PER SEGMENT. Doing it the
// other way round — substitute, then split the whole string on "." — makes the
// default path unusable for any audience containing a dot, which is an
// ordinary Keycloak or Entra client ID and a certainty for a URL-shaped one:
// "resource_access.{audience}.roles" with audience "gawk.admin" would become a
// four-segment walk that no correctly-minted token can satisfy, and every
// operator would be refused with a reason visible only at Debug.
//
// An empty path — or one with an empty segment — is refused here rather than
// at request time: a path resolving to nothing would leave every valid token
// role-less, or worse invite a "no claim means allow" reading.
func ParsePath(template, audience string) (Path, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil, errors.New("must not be empty: with no roles claim every valid token would be an operator")
	}
	segments := strings.Split(template, ".")
	for i, seg := range segments {
		seg = strings.ReplaceAll(seg, Placeholder, audience)
		if strings.TrimSpace(seg) == "" {
			return nil, fmt.Errorf("%q has an empty segment (position %d)", template, i+1)
		}
		segments[i] = seg
	}
	return segments, nil
}

// Roles resolves the path in claims and returns the roles it names.
//
// Every failure here is the IdP's token shape disagreeing with our
// configuration — a missing claim, an object where an array was expected, a
// number inside the array. None of them is an error in this process, so none
// may become a 500: the caller answers 403. A token that cannot prove the role
// does not have it.
func (p Path) Roles(claims map[string]any) ([]string, error) {
	if len(p) == 0 {
		return nil, errors.New("the roles-claim path is empty; it was never parsed, or parsing it failed")
	}
	var node any = claims
	for i, segment := range p {
		obj, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claim path %q: %q is not an object", p, walked(p, i))
		}
		next, ok := obj[segment]
		if !ok {
			return nil, fmt.Errorf("claim path %q: segment %q is absent", p, segment)
		}
		node = next
	}
	switch v := node.(type) {
	case []any:
		roles := make([]string, 0, len(v))
		for _, entry := range v {
			role, ok := entry.(string)
			if !ok {
				// Strict: a mixed array is a claim we do not understand, and
				// guessing which half to trust is how authorization bugs start.
				return nil, fmt.Errorf("claim path %q: contains a non-string entry (%T)", p, entry)
			}
			roles = append(roles, role)
		}
		return roles, nil
	case string:
		// Some IdPs render a single role as a bare string. There is nothing to
		// guess about it — one string is one role — so it is accepted, unlike
		// the mixed array above.
		return []string{v}, nil
	default:
		return nil, fmt.Errorf("claim path %q: not a string or an array of strings (%T)", p, node)
	}
}

// Has reports whether claims carry role at this path. It is Roles plus an
// exact match; no prefix, case or wildcard matching exists anywhere here.
func (p Path) Has(claims map[string]any, role string) (bool, error) {
	roles, err := p.Roles(claims)
	if err != nil {
		return false, err
	}
	for _, r := range roles {
		if r == role {
			return true, nil
		}
	}
	return false, nil
}

// walked renders the prefix of p that was successfully traversed before
// segment i, so "is not an object" names the claim that disappointed us rather
// than the whole path.
func walked(p Path, i int) string {
	if i == 0 {
		return "(the token's claims)"
	}
	return Path(p[:i]).String()
}
