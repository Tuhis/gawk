package wire

import (
	"bytes"
	"testing"
	"time"
)

// docs/78 D3: a token home mints for a PoP carries the PoP's issuer key in
// its nonce, authenticated by the same tag; a token a home pod mints for its
// own session carries none.
func TestTelemetryIssuerRidesTheNonceUnderTheTag(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	now := time.Unix(1785715200, 0).UTC()
	issuer := TelemetryIssuerKey(key, "c0ffee00-pop-uid")
	if len(issuer) != TelemetryIssuerKeySize {
		t.Fatalf("issuer key is %d bytes", len(issuer))
	}
	if bytes.Equal(issuer, TelemetryIssuerKey(key, "another-pop")) {
		t.Fatal("two PoPs share an issuer key")
	}

	for _, role := range []TelemetryRole{TelemetryRoleViewer, TelemetryRoleBroadcaster} {
		tok, err := MintTelemetrySessionTokenForIssuer(key, goldenTelemetryBroadcastKey, role, now, issuer)
		if err != nil {
			t.Fatalf("mint for issuer: %v", err)
		}
		// The ingest's check is unchanged: the tag verifies as any token's.
		if _, err := VerifyTelemetrySessionToken(key, tok, goldenTelemetryBroadcastKey, role, now); err != nil {
			t.Fatalf("verify: %v", err)
		}
		got, ok := TelemetryTokenIssuer(key, tok)
		if !ok || !bytes.Equal(got, issuer) {
			t.Fatalf("issuer = %x, %v; want %x", got, ok, issuer)
		}
	}

	// A home pod's tokens name no issuer — every one of them.
	for i := 0; i < 256; i++ {
		tok, err := MintTelemetrySessionToken(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := TelemetryTokenIssuer(key, tok); ok {
			t.Fatalf("a home token named issuer %x", got)
		}
	}
}

// The marker's check is keyed: a nonce that merely has the bit set (a token
// an older home pod minted, or one a client tampered with, which also fails
// the tag) does not name an issuer, and neither does an issuer token read
// under another fleet's key.
func TestTelemetryIssuerNeedsItsKeyedCheck(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, TelemetryKeySize)
	now := time.Unix(1785715200, 0).UTC()
	issuer := TelemetryIssuerKey(key, "pop-a")
	tok, err := MintTelemetrySessionTokenForIssuer(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := TelemetryTokenIssuer(bytes.Repeat([]byte{0x22}, TelemetryKeySize), tok); ok {
		t.Error("an issuer read under another key")
	}
	forged := append([]byte(nil), tok...)
	forged[5] ^= 0x01 // inside the check
	if _, ok := TelemetryTokenIssuer(key, forged); ok {
		t.Error("a corrupted check still named an issuer")
	}
	// An older pod's random nonce with the bit set: 1 in 2^15 passes.
	old := append([]byte(nil), tok...)
	old[4] = 0x80
	old[5] = 0x00
	if _, ok := TelemetryTokenIssuer(key, old); ok {
		t.Error("a bare marker bit named an issuer")
	}
	if _, err := MintTelemetrySessionTokenForIssuer(key, goldenTelemetryBroadcastKey, TelemetryRoleViewer, now, []byte{1, 2}); err == nil {
		t.Error("a short issuer key minted")
	}
}
