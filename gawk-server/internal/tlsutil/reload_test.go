package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// writeCertPair generates a fresh dev cert, writes it as PEM to dir with the
// given mtime, and returns the leaf serial for identification.
func writeCertPair(t *testing.T, dir string, mtime time.Time) string {
	t.Helper()
	cert, err := GenerateDevCert([]string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := WriteCertPair(certPath, keyPath, cert); err != nil {
		t.Fatalf("WriteCertPair: %v", err)
	}
	// Set an explicit mtime so consecutive writes are always distinguishable
	// regardless of filesystem timestamp granularity.
	if err := os.Chtimes(certPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return cert.Leaf.SerialNumber.String()
}

func serialOf(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse served cert: %v", err)
	}
	return leaf.SerialNumber.String()
}

func TestReloaderServesAndReloads(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	v1 := writeCertPair(t, dir, base)

	r, err := NewReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), discardLog)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	clock := base
	r.now = func() time.Time { return clock }

	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if s := serialOf(t, got); s != v1 {
		t.Errorf("served serial %s, want v1 %s", s, v1)
	}

	// Renew the cert on disk; before the stat throttle expires the old one
	// is still served.
	v2 := writeCertPair(t, dir, base.Add(time.Minute))
	got, _ = r.GetCertificate(nil)
	if s := serialOf(t, got); s != v1 {
		t.Errorf("served serial %s before throttle expiry, want v1 %s", s, v1)
	}

	// After the throttle window the new mtime is noticed and v2 served.
	clock = clock.Add(statInterval + time.Second)
	got, err = r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after renewal: %v", err)
	}
	if s := serialOf(t, got); s != v2 {
		t.Errorf("served serial %s after renewal, want v2 %s", s, v2)
	}
}

func TestReloaderKeepsCertOnCorruptReload(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	v1 := writeCertPair(t, dir, base)
	certPath := filepath.Join(dir, "tls.crt")

	r, err := NewReloader(certPath, filepath.Join(dir, "tls.key"), discardLog)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	clock := base
	r.now = func() time.Time { return clock }

	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(certPath, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(statInterval + time.Second)
	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if s := serialOf(t, got); s != v1 {
		t.Errorf("served serial %s after corrupt reload, want v1 %s", s, v1)
	}
}

func TestNewReloaderFailsFast(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewReloader(filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key"), discardLog); err == nil {
		t.Error("expected error for missing files, got nil")
	}
}
