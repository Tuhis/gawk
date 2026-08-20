package auth

import (
	"errors"
	"fmt"
	"strings"
)

// parseClaimPath splits the configured dot-path into segments.
//
// The path addresses either a top-level claim (`groups`) or a nested one
// (Keycloak's client roles, `resource_access.{clientId}.roles`, with
// {clientId} already substituted by config.RolesClaimPath). An empty path — or
// one with an empty segment — is refused at construction rather than at
// request time: a path that resolves to nothing would leave every valid token
// role-less, or worse, invite a "no claim means allow" reading (§4.8).
//
// Known limitation, stated because the default path is built from a
// configured value: there is no escaping, so a client ID containing a dot
// cannot be addressed by the default path. Such a deployment must point
// -oidc-roles-claim at a differently-shaped claim (e.g. a top-level `groups`).
func parseClaimPath(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("must not be empty: with no roles claim every valid token would be an operator")
	}
	segments := strings.Split(path, ".")
	for _, s := range segments {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%q has an empty segment", path)
		}
	}
	return segments, nil
}

// rolesFromClaims resolves the roles array at path.
//
// Every failure here is the IdP's token shape disagreeing with our
// configuration — a missing claim, an object where an array was expected, a
// number inside the array. None of them is an error in this process, so none
// of them may become a 500: the caller records an empty role set and
// RequireRole answers 403 (AP5, "missing/malformed claim → 403, never 500").
func rolesFromClaims(claims map[string]any, path []string) ([]string, error) {
	var current any = claims
	for i, segment := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("claim %q is not an object", strings.Join(path[:i], "."))
		}
		next, ok := obj[segment]
		if !ok {
			return nil, fmt.Errorf("claim %q is absent", strings.Join(path[:i+1], "."))
		}
		current = next
	}
	list, ok := current.([]any)
	if !ok {
		return nil, fmt.Errorf("claim %q is not an array", strings.Join(path, "."))
	}
	roles := make([]string, 0, len(list))
	for _, entry := range list {
		role, ok := entry.(string)
		if !ok {
			// Strict: a mixed array is a claim we do not understand, and
			// guessing which half to trust is how authorization bugs start.
			return nil, fmt.Errorf("claim %q contains a non-string entry", strings.Join(path, "."))
		}
		roles = append(roles, role)
	}
	return roles, nil
}
