package auth

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseClaimPath(t *testing.T) {
	ok := map[string][]string{
		"groups":                         {"groups"},
		"resource_access.gawk.roles":     {"resource_access", "gawk", "roles"},
		"  realm_access.roles  ":         {"realm_access", "roles"},
		"a.b.c.d.e":                      {"a", "b", "c", "d", "e"},
		"https://gawk.example/roles-ish": {"https://gawk", "example/roles-ish"}, // no escaping: documented in roles.go
	}
	for path, want := range ok {
		got, err := parseClaimPath(path)
		if err != nil {
			t.Errorf("parseClaimPath(%q): %v", path, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseClaimPath(%q) = %v, want %v", path, got, want)
		}
	}

	for _, path := range []string{"", "   ", ".", "a.", ".a", "a..b"} {
		if _, err := parseClaimPath(path); err == nil {
			t.Errorf("parseClaimPath(%q) succeeded; want a refusal", path)
		}
	}
}

func claimsFrom(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return m
}

func TestRolesFromClaims(t *testing.T) {
	// Claim values arrive as decoded JSON, so the test feeds decoded JSON —
	// a Go literal would quietly use types (e.g. []string) the wire never
	// produces.
	claims := claimsFrom(t, `{
		"sub": "u1",
		"groups": ["operator", "flagger"],
		"realm_access": {"roles": ["default-roles"]},
		"resource_access": {"gawk-admin": {"roles": ["operator"]}, "other": {"roles": ["admin"]}},
		"empty": {"roles": []}
	}`)

	cases := []struct {
		path string
		want []string
	}{
		{"groups", []string{"operator", "flagger"}},
		{"realm_access.roles", []string{"default-roles"}},
		{"resource_access.gawk-admin.roles", []string{"operator"}},
		{"empty.roles", []string{}},
	}
	for _, tc := range cases {
		path, err := parseClaimPath(tc.path)
		if err != nil {
			t.Fatalf("parseClaimPath(%q): %v", tc.path, err)
		}
		got, err := rolesFromClaims(claims, path)
		if err != nil {
			t.Errorf("rolesFromClaims(%q): %v", tc.path, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("rolesFromClaims(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestRolesFromClaimsRejectsUnusableShapes(t *testing.T) {
	claims := claimsFrom(t, `{
		"scope": "openid profile",
		"resource_access": {"gawk-admin": {"roles": "operator"}},
		"mixed": {"roles": ["operator", 7]},
		"scalar": 3
	}`)

	for _, path := range []string{
		"absent",
		"scope",                            // a string where an array belongs
		"scope.roles",                      // walking through a string
		"resource_access.gawk-admin.roles", // roles is a string
		"mixed.roles",                      // a non-string entry
		"scalar.roles",
	} {
		segments, err := parseClaimPath(path)
		if err != nil {
			t.Fatalf("parseClaimPath(%q): %v", path, err)
		}
		roles, err := rolesFromClaims(claims, segments)
		if err == nil {
			t.Errorf("rolesFromClaims(%q) = %v, want an error", path, roles)
		}
		if roles != nil {
			t.Errorf("rolesFromClaims(%q) returned roles alongside its error: %v", path, roles)
		}
	}
}
