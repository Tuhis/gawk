package tlsutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
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

// R38 (docs/41 LD1): WriteCertPair is the single PEM encoder for both the
// standalone gawk-devcert tool and the relay's generate-if-absent path, so a
// round-trip through it has to produce a pair crypto/tls will load back.
func TestWriteCertPairRoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	cert, err := GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	if err := WriteCertPair(certPath, keyPath, cert); err != nil {
		t.Fatalf("WriteCertPair: %v", err)
	}

	loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair on what we wrote: %v", err)
	}
	if !bytes.Equal(loaded.Certificate[0], cert.Certificate[0]) {
		t.Error("loaded DER differs from what was written")
	}

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{certPath, 0o644},
		{keyPath, 0o600}, // the key is owner-only regardless of umask
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(tc.path), got, tc.want)
		}
	}

	// The block types gawk-devcert has always produced — changing them would
	// break anything reading these files with a stricter parser.
	certBlock, _ := pem.Decode(readFile(t, certPath))
	keyBlock, _ := pem.Decode(readFile(t, keyPath))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Errorf("cert PEM block = %v, want CERTIFICATE", certBlock)
	}
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		t.Errorf("key PEM block = %v, want EC PRIVATE KEY", keyBlock)
	}
}

func TestWriteCertPairRejectsNonECDSAKey(t *testing.T) {
	dir := t.TempDir()
	cert, err := GenerateDevCert([]string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert.PrivateKey = struct{}{}
	if err := WriteCertPair(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"), cert); err == nil {
		t.Error("expected an error for a non-ECDSA key, got nil")
	}
}

func TestLoadOrGenerateWritesThenLoads(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	first, generated, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !generated {
		t.Error("generated = false on the first call, want true")
	}
	if first.Leaf == nil {
		t.Fatal("Leaf not populated")
	}

	second, generated, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity)
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if generated {
		t.Error("generated = true on the second call; the pair on disk must be reused")
	}
	if CertHashHex(first.Leaf) != CertHashHex(second.Leaf) {
		t.Error("the second call returned a different certificate")
	}
}

func TestLoadOrGenerateRefusesHalfPresentPair(t *testing.T) {
	for _, name := range []string{"cert.pem", "key.pem"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			certPath := filepath.Join(dir, "cert.pem")
			keyPath := filepath.Join(dir, "key.pem")
			if err := os.WriteFile(filepath.Join(dir, name), []byte("stray\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, generated, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if generated {
				t.Error("generated = true on the error path")
			}
			if got := readFile(t, filepath.Join(dir, name)); string(got) != "stray\n" {
				t.Errorf("the existing file was overwritten: %q", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("directory holds %d files, want only the pre-existing one", len(entries))
			}
		})
	}
}

func TestLoadOrGenerateSurfacesACorruptPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	for _, p := range []string{certPath, keyPath} {
		if err := os.WriteFile(p, []byte("not pem\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity); err == nil {
		t.Error("expected an error for an unparseable pair, got nil")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
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

// writeExpiredPair writes a self-signed pair that expired yesterday, with the
// given Subject, so the expiry paths can be driven without waiting 14 days.
func writeExpiredPair(t *testing.T, certPath, keyPath, commonName string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-14 * 24 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCertPair(certPath, keyPath, tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}); err != nil {
		t.Fatal(err)
	}
}

// An expired dev certificate must be replaced, not served. Nothing downstream
// catches this: the healthcheck skips verification, config-gen happily hashes
// the expired leaf, and the browser then rejects the handshake — the opaque
// failure R38 exists to remove (docs/41 §4.2.1).
func TestLoadOrGenerateReplacesAnExpiredDevCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeExpiredPair(t, certPath, keyPath, devCertCommonName)
	before := readFile(t, certPath)

	cert, generated, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if !generated {
		t.Error("generated = false; an expired dev certificate must be regenerated")
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf not populated")
	}
	if time.Now().After(cert.Leaf.NotAfter) {
		t.Errorf("still serving an expired certificate (not_after %s)", cert.Leaf.NotAfter)
	}
	if bytes.Equal(before, readFile(t, certPath)) {
		t.Error("the expired pair is still on disk; the replacement was not persisted")
	}
}

// ...but only ours. A certificate this package did not issue is the operator's
// to fix, however expired: silently overwriting one loses a key they meant to
// keep, which is the rule the half-present-pair arm already enforces.
func TestLoadOrGenerateKeepsAnExpiredForeignCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeExpiredPair(t, certPath, keyPath, "relay.example.com")
	before := readFile(t, certPath)

	cert, generated, err := LoadOrGenerate(certPath, keyPath, []string{"localhost"}, MaxDevCertValidity)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if generated {
		t.Fatal("generated = true; a certificate this package did not issue must never be overwritten")
	}
	if !bytes.Equal(before, readFile(t, certPath)) {
		t.Error("the existing certificate was rewritten")
	}
	if cert.Leaf == nil || cert.Leaf.Subject.CommonName != "relay.example.com" {
		t.Error("the caller did not get the certificate that is on disk")
	}
}
