package oidcauth

import (
	"slices"
	"testing"
)

// The narrowing both algorithm sources go through: the provider's advertised
// list (the portal's R39 behaviour) and Options.SigningAlgorithms (the
// relay's, which passes the whole allowlist).
func TestSigningAlgsNarrowsToTheAsymmetricAllowlist(t *testing.T) {
	got := signingAlgs([]string{"none", "HS256", "RS256", "ES384", "HS512", "EdDSA"})
	if want := []string{"RS256", "ES384", "EdDSA"}; !slices.Equal(got, want) {
		t.Errorf("signingAlgs = %v, want %v", got, want)
	}
	// Nothing usable leaves go-oidc on its RS256 default rather than open.
	if got := signingAlgs([]string{"HS256", "none"}); got != nil {
		t.Errorf("signingAlgs of only symmetric algorithms = %v, want nil", got)
	}
	if got := signingAlgs(AsymmetricSigningAlgs()); !slices.Equal(got, asymmetricAlgs) {
		t.Errorf("the allowlist does not survive its own narrowing: %v", got)
	}
}
