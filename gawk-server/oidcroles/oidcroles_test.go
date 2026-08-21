package oidcroles

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// claimsFrom decodes JSON, because claim values arrive as decoded JSON. A Go
// literal would quietly use types the wire never produces — []string where an
// IdP sends []any, int where it sends float64 — and the walk would be tested
// against a shape it never meets.
func claimsFrom(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return m
}

func TestParsePath(t *testing.T) {
	cases := map[string]struct {
		template string
		audience string
		want     Path
	}{
		"top-level claim":              {"groups", "gawk-admin", Path{"groups"}},
		"realm roles":                  {"realm_access.roles", "gawk-admin", Path{"realm_access", "roles"}},
		"surrounding spaces":           {"  realm_access.roles  ", "gawk-admin", Path{"realm_access", "roles"}},
		"deep path":                    {"a.b.c.d.e", "x", Path{"a", "b", "c", "d", "e"}},
		"default":                      {DefaultClaim, "gawk-admin", Path{"resource_access", "gawk-admin", "roles"}},
		"no placeholder used":          {"resource_access.other.roles", "gawk-admin", Path{"resource_access", "other", "roles"}},
		"placeholder alone":            {Placeholder, "groups", Path{"groups"}},
		"placeholder within a segment": {"roles_" + Placeholder, "gawk-admin", Path{"roles_gawk-admin"}},
	}
	for name, tc := range cases {
		got, err := ParsePath(tc.template, tc.audience)
		if err != nil {
			t.Errorf("%s: ParsePath(%q, %q): %v", name, tc.template, tc.audience, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ParsePath(%q, %q) = %v, want %v", name, tc.template, tc.audience, got, tc.want)
		}
	}
}

// THE REGRESSION THIS PACKAGE EXISTS FOR (PR #280, review 3826627571). Dots
// are ordinary in Keycloak and Entra client IDs and unavoidable in a
// URL-shaped audience. Substituting before splitting turns the default path
// into a four-segment walk no correctly-minted token satisfies, and 403s every
// operator.
func TestParsePathSubstitutesPerSegmentSoADottedAudienceStaysOneSegment(t *testing.T) {
	for _, audience := range []string{"gawk.admin", "https://api.gawk.example/admin", "a.b.c"} {
		got, err := ParsePath(DefaultClaim, audience)
		if err != nil {
			t.Fatalf("ParsePath(%q, %q): %v", DefaultClaim, audience, err)
		}
		want := Path{"resource_access", audience, "roles"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParsePath(%q, %q) = %v, want %v — the audience must occupy ONE segment",
				DefaultClaim, audience, got, want)
		}
	}
}

func TestParsePathRefusesUnusableTemplates(t *testing.T) {
	for _, tc := range []struct{ template, audience string }{
		{"", "gawk-admin"},
		{"   ", "gawk-admin"},
		{".", "gawk-admin"},
		{"a.", "gawk-admin"},
		{".a", "gawk-admin"},
		{"a..b", "gawk-admin"},
		// A blank audience would silently empty the segment the placeholder
		// occupies, which is the same "path resolving to nothing" failure.
		{DefaultClaim, ""},
		{DefaultClaim, "   "},
	} {
		if got, err := ParsePath(tc.template, tc.audience); err == nil {
			t.Errorf("ParsePath(%q, %q) = %v, want a refusal", tc.template, tc.audience, got)
		}
	}
}

func TestRoles(t *testing.T) {
	claims := claimsFrom(t, `{
		"sub": "u1",
		"groups": ["operator", "flagger"],
		"realm_access": {"roles": ["default-roles"]},
		"resource_access": {
			"gawk-admin": {"roles": ["operator"]},
			"gawk.admin": {"roles": ["operator", "flagger"]},
			"other": {"roles": ["admin"]}
		},
		"single": {"roles": "operator"},
		"empty": {"roles": []}
	}`)

	for _, tc := range []struct {
		template string
		audience string
		want     []string
	}{
		{"groups", "unused", []string{"operator", "flagger"}},
		{"realm_access.roles", "unused", []string{"default-roles"}},
		{DefaultClaim, "gawk-admin", []string{"operator"}},
		// The dotted audience resolves the claim it names, not a nested one.
		{DefaultClaim, "gawk.admin", []string{"operator", "flagger"}},
		// A single role rendered as a bare string is one role, not an error.
		{"single.roles", "unused", []string{"operator"}},
		// An empty array is a valid answer meaning "no roles", and must be
		// distinguishable from a failure: non-nil, zero length, no error.
		{"empty.roles", "unused", []string{}},
	} {
		path, err := ParsePath(tc.template, tc.audience)
		if err != nil {
			t.Fatalf("ParsePath(%q, %q): %v", tc.template, tc.audience, err)
		}
		got, err := path.Roles(claims)
		if err != nil {
			t.Errorf("Roles(%v): %v", path, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Roles(%v) = %#v, want %#v", path, got, tc.want)
		}
	}
}

func TestRolesRejectsUnusableShapes(t *testing.T) {
	claims := claimsFrom(t, `{
		"scope": "openid profile",
		"resource_access": {"gawk-admin": {"roles": {"nested": "object"}}},
		"mixed": {"roles": ["operator", 7]},
		"scalar": 3
	}`)

	for _, template := range []string{
		"absent",
		"absent.deeper",
		"scope.roles",                      // walking through a string
		"resource_access.gawk-admin.roles", // an object where an array belongs
		"mixed.roles",                      // a non-string entry
		"scalar.roles",
	} {
		path, err := ParsePath(template, "gawk-admin")
		if err != nil {
			t.Fatalf("ParsePath(%q): %v", template, err)
		}
		roles, err := path.Roles(claims)
		if err == nil {
			t.Errorf("Roles(%q) = %v, want an error", template, roles)
			continue
		}
		// The caller records "no roles" and answers 403; a non-nil slice
		// alongside an error invites a caller to use the half-built one.
		if roles != nil {
			t.Errorf("Roles(%q) returned roles alongside its error: %v", template, roles)
		}
		// The message has to name the path, or a 403 in production is a
		// mystery — this is the only place the operator's misconfiguration is
		// ever described.
		if !strings.Contains(err.Error(), path.String()) {
			t.Errorf("Roles(%q) error %q does not name the path", template, err)
		}
	}
}

// An unparsed path must never behave like "no constraint". A zero Path walks
// nowhere, and the temptation is to let it return the whole claims object.
func TestRolesRefusesAnEmptyPath(t *testing.T) {
	claims := claimsFrom(t, `{"groups": ["operator"]}`)
	for _, p := range []Path{nil, {}} {
		roles, err := p.Roles(claims)
		if err == nil {
			t.Errorf("Roles on an empty path = %v, want a refusal", roles)
		}
	}
	if ok, err := Path(nil).Has(claims, "operator"); ok || err == nil {
		t.Errorf("Has on an empty path = %v, %v; want false and a refusal", ok, err)
	}
}

func TestHas(t *testing.T) {
	claims := claimsFrom(t, `{"resource_access": {"gawk.admin": {"roles": ["operator", "viewer"]}}}`)
	path, err := ParsePath(DefaultClaim, "gawk.admin")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	for role, want := range map[string]bool{
		"operator": true,
		"viewer":   true,
		// Exact match only: no prefix, no case folding, no wildcard.
		"oper":      false,
		"OPERATOR":  false,
		"operator ": false,
		"":          false,
	} {
		got, err := path.Has(claims, role)
		if err != nil {
			t.Errorf("Has(%q): %v", role, err)
			continue
		}
		if got != want {
			t.Errorf("Has(%q) = %v, want %v", role, got, want)
		}
	}

	// A shape failure is reported, not swallowed into "false": the caller
	// distinguishes "this token lacks the role" from "this claim is not what
	// you configured", and only the second is worth an operator's attention.
	bad, err := ParsePath("resource_access", "unused")
	if err != nil {
		t.Fatalf("ParsePath: %v", err)
	}
	if ok, err := bad.Has(claims, "operator"); ok || err == nil {
		t.Errorf("Has on an object-valued claim = %v, %v; want false and an error", ok, err)
	}
}

// The default is Keycloak's client-roles shape, and it is the string both
// modules' flag defaults carry. If it changes, both change together — that is
// the entire point of it living here.
func TestDefaultClaimIsTheKeycloakClientRolesShape(t *testing.T) {
	if DefaultClaim != "resource_access.{audience}.roles" {
		t.Errorf("DefaultClaim = %q, want resource_access.{audience}.roles", DefaultClaim)
	}
	if Placeholder != "{audience}" {
		t.Errorf("Placeholder = %q, want {audience}", Placeholder)
	}
	if !strings.Contains(DefaultClaim, Placeholder) {
		t.Error("DefaultClaim does not contain Placeholder: the default would ignore the audience entirely")
	}
}
