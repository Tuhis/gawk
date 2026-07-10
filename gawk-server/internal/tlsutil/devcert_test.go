package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"os"
	"slices"
	"testing"
	"time"
)

func TestGenerateDevCertProperties(t *testing.T) {
	cert, err := GenerateDevCert([]string{"localhost", "127.0.0.1", "gawk.lan"}, 13*24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	leaf := cert.Leaf
	if leaf == nil {
		t.Fatal("Leaf not populated")
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T, want *ecdsa.PublicKey", leaf.PublicKey)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("curve = %v, want P-256", pub.Curve.Params().Name)
	}

	// Chromium rejects serverCertificateHashes certs whose total span
	// exceeds 14 days; the clock-skew backdate must not push it over.
	if v := leaf.NotAfter.Sub(leaf.NotBefore); v > MaxDevCertValidity {
		t.Errorf("validity span %v exceeds Chromium's 14-day limit", v)
	}

	if !slices.Contains(leaf.DNSNames, "localhost") || !slices.Contains(leaf.DNSNames, "gawk.lan") {
		t.Errorf("DNSNames = %v, want localhost and gawk.lan", leaf.DNSNames)
	}
	foundIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "127.0.0.1" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("IPAddresses = %v, want 127.0.0.1", leaf.IPAddresses)
	}
}

func TestGenerateDevCertCapsValidity(t *testing.T) {
	cert, err := GenerateDevCert([]string{"localhost"}, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	// NotBefore is backdated 1h for clock skew, but the total span (which is
	// what Chromium limits) must still stay ≤ 14 days.
	if v := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore); v > MaxDevCertValidity {
		t.Errorf("validity span %v not capped to 14 days", v)
	}
}

func TestGenerateDevCertNoHosts(t *testing.T) {
	if _, err := GenerateDevCert(nil, time.Hour); err == nil {
		t.Error("expected error for empty hosts, got nil")
	}
}

// Fixture hashes computed independently with openssl against
// testdata/fixture_cert.pem:
//
//	openssl x509 -pubkey -noout | openssl pkey -pubin -outform DER \
//	  | openssl dgst -sha256 -binary | openssl base64
//	openssl x509 -outform DER | openssl dgst -sha256 -r
const (
	fixtureSPKI     = "k25OsJAR1wtwNZLW6w6vCNRihmJ/eNT22HcnEOAtxoI="
	fixtureCertHash = "8a3d3606c8e6c47609675dd586d86a523646c0ba5e3bec3fe09242837d96a1f9"
)

func loadFixtureCert(t *testing.T) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile("testdata/fixture_cert.pem")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block in fixture")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return cert
}

func TestSPKIFingerprintMatchesOpenSSL(t *testing.T) {
	if got := SPKIFingerprint(loadFixtureCert(t)); got != fixtureSPKI {
		t.Errorf("SPKIFingerprint = %q, want %q", got, fixtureSPKI)
	}
}

func TestCertHashHexMatchesOpenSSL(t *testing.T) {
	if got := CertHashHex(loadFixtureCert(t)); got != fixtureCertHash {
		t.Errorf("CertHashHex = %q, want %q", got, fixtureCertHash)
	}
}
