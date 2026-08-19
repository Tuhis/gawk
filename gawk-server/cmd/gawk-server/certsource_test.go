package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

// The docs/41 §4.2.1 truth table, one test per row. The table is the R38
// specification for certSource, and the row this milestone exists for is
// "-dev-cert with -cert-file, both files present ⇒ load, do not regenerate":
// without it the dev certificate changes on every start and the hash a
// browser was given is stale before the page reloads (docs/gotchas.md).

func devCertConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	return config.Config{
		Addr:         ":4433",
		DevCert:      true,
		DevCertHosts: "localhost,127.0.0.1",
		CertFile:     filepath.Join(dir, "cert.pem"),
		KeyFile:      filepath.Join(dir, "key.pem"),
	}
}

// logSink returns a logger and the buffer it writes to, so the expiry warning
// (which is a log line and nothing else) is observable.
func logSink() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// servedLeaf calls the handshake callback the way quic-go would and parses
// what it hands back — the certificate this server would actually present.
func servedLeaf(t *testing.T, get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *x509.Certificate {
	t.Helper()
	cert, err := get(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert.Leaf != nil {
		return cert.Leaf
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse served certificate: %v", err)
	}
	return leaf
}

// Row: -dev-cert, -cert-file set, both files absent ⇒ generate and write.
func TestCertSourceGeneratesWhenBothAbsent(t *testing.T) {
	dir := t.TempDir()
	cfg := devCertConfig(t, dir)
	log, buf := logSink()

	get, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("certSource: %v", err)
	}
	leaf := servedLeaf(t, get)

	for _, p := range []string{cfg.CertFile, cfg.KeyFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to be written: %v", p, err)
		}
	}
	fi, err := os.Stat(cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("key mode = %o, want 600", got)
	}

	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key = %T (%v), want ECDSA P-256", leaf.PublicKey, leaf.PublicKey)
	}
	if span := leaf.NotAfter.Sub(leaf.NotBefore); span > tlsutil.MaxDevCertValidity {
		t.Errorf("validity span %v exceeds Chromium's %v limit", span, tlsutil.MaxDevCertValidity)
	}
	// The hash a developer has to be able to find in the log.
	if want := tlsutil.CertHashHex(leaf); !strings.Contains(buf.String(), want) {
		t.Errorf("startup log does not carry cert_hash_hex %s:\n%s", want, buf)
	}
	if !strings.Contains(buf.String(), "generated=true") {
		t.Errorf("startup log does not report generated=true:\n%s", buf)
	}
}

// Row: -dev-cert, -cert-file set, both files present ⇒ load, do not
// regenerate. THE assertion this milestone exists for.
func TestCertSourceHashIsStableAcrossStarts(t *testing.T) {
	dir := t.TempDir()
	cfg := devCertConfig(t, dir)
	log, _ := logSink()

	first, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("first certSource: %v", err)
	}
	hash1 := tlsutil.CertHashHex(servedLeaf(t, first))

	certBefore, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		t.Fatal(err)
	}

	second, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("second certSource: %v", err)
	}
	hash2 := tlsutil.CertHashHex(servedLeaf(t, second))

	if hash1 != hash2 {
		t.Errorf("cert hash changed across starts: %s then %s", hash1, hash2)
	}
	certAfter, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(certBefore, certAfter) {
		t.Error("second start rewrote the certificate file")
	}
}

// Row: -dev-cert, -cert-file set, exactly one file present ⇒ error, and
// nothing on disk is touched.
func TestCertSourceRefusesHalfPresentPair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present func(cfg config.Config) string
	}{
		{"cert only", func(c config.Config) string { return c.CertFile }},
		{"key only", func(c config.Config) string { return c.KeyFile }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := devCertConfig(t, dir)
			path := tc.present(cfg)
			const sentinel = "not a pem file, but somebody's\n"
			if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}

			log, _ := logSink()
			if _, err := certSource(cfg, log); err == nil {
				t.Fatal("expected an error, got nil")
			} else if !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Errorf("error = %v, want it to say it refuses to overwrite", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the existing file was removed: %v", err)
			}
			if string(got) != sentinel {
				t.Errorf("the existing file was rewritten: %q", got)
			}
			// The absent half stays absent — no half-written pair.
			absent := cfg.KeyFile
			if path == cfg.KeyFile {
				absent = cfg.CertFile
			}
			if _, err := os.Stat(absent); err == nil {
				t.Errorf("%s was written despite the error", absent)
			}
		})
	}
}

// Row: -cert-file WITHOUT -dev-cert and no file on disk ⇒ still fails fast.
// Load-bearing: a production deployment whose cert Secret failed to mount must
// crash, never self-sign.
func TestCertSourceFileBackedStillFailsFastWithoutDevCert(t *testing.T) {
	dir := t.TempDir()
	cfg := devCertConfig(t, dir)
	cfg.DevCert = false
	log, _ := logSink()

	if _, err := certSource(cfg, log); err == nil {
		t.Fatal("expected an error for a missing cert file, got nil")
	}
	if _, err := os.Stat(cfg.CertFile); err == nil {
		t.Error("a certificate was generated without -dev-cert")
	}
}

// Row: -cert-file WITHOUT -dev-cert, both files present ⇒ reloader, plus the
// R38 identity logging that gives a host-loop developer the hash.
func TestCertSourceFileBackedLogsIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := devCertConfig(t, dir)
	cert, err := tlsutil.GenerateDevCert([]string{"localhost"}, tlsutil.MaxDevCertValidity)
	if err != nil {
		t.Fatal(err)
	}
	if err := tlsutil.WriteCertPair(cfg.CertFile, cfg.KeyFile, cert); err != nil {
		t.Fatal(err)
	}

	cfg.DevCert = false
	log, buf := logSink()
	get, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("certSource: %v", err)
	}
	leaf := servedLeaf(t, get)
	for _, want := range []string{tlsutil.CertHashHex(leaf), tlsutil.SPKIFingerprint(leaf), "not_after"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("startup log is missing %q:\n%s", want, buf)
		}
	}
}

// Row: -dev-cert alone ⇒ today's ephemeral in-memory certificate, and it is
// still ephemeral (a second call must NOT return the same one).
func TestCertSourceDevCertAloneStaysEphemeral(t *testing.T) {
	cfg := config.Config{Addr: ":4433", DevCert: true, DevCertHosts: "localhost,127.0.0.1"}
	log, _ := logSink()

	first, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("certSource: %v", err)
	}
	second, err := certSource(cfg, log)
	if err != nil {
		t.Fatalf("certSource: %v", err)
	}
	if tlsutil.CertHashHex(servedLeaf(t, first)) == tlsutil.CertHashHex(servedLeaf(t, second)) {
		t.Error("two -dev-cert starts produced the same certificate; it is meant to be ephemeral")
	}
}

// Row: neither flag ⇒ the unchanged refusal to start.
func TestCertSourceNoCertificateConfigured(t *testing.T) {
	log, _ := logSink()
	_, err := certSource(config.Config{Addr: ":4433"}, log)
	if err == nil || !strings.Contains(err.Error(), "no certificate configured") {
		t.Errorf("err = %v, want the no-certificate-configured refusal", err)
	}
}

// -dev-cert with -cert-file but no -key-file is a configuration mistake with
// no sensible reading; name it rather than failing inside the loader.
func TestCertSourceDevCertWithoutKeyFile(t *testing.T) {
	dir := t.TempDir()
	cfg := devCertConfig(t, dir)
	cfg.KeyFile = ""
	log, _ := logSink()

	if _, err := certSource(cfg, log); err == nil || !strings.Contains(err.Error(), "-key-file") {
		t.Errorf("err = %v, want it to name -key-file", err)
	}
}

// The expiry warning fires under 72 h and not above it.
func TestCertSourceExpiryWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		validity time.Duration
		wantWarn bool
	}{
		{"about to expire", time.Hour, true},
		{"fresh", tlsutil.MaxDevCertValidity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := devCertConfig(t, dir)
			cert, err := tlsutil.GenerateDevCert([]string{"localhost"}, tc.validity)
			if err != nil {
				t.Fatal(err)
			}
			if err := tlsutil.WriteCertPair(cfg.CertFile, cfg.KeyFile, cert); err != nil {
				t.Fatal(err)
			}

			log, buf := logSink()
			if _, err := certSource(cfg, log); err != nil {
				t.Fatalf("certSource: %v", err)
			}
			warned := strings.Contains(buf.String(), "certificate expires soon")
			if warned != tc.wantWarn {
				t.Errorf("expiry warning = %v, want %v (validity %v):\n%s", warned, tc.wantWarn, tc.validity, buf)
			}
			if tc.wantWarn && !strings.Contains(buf.String(), "remedy") {
				t.Errorf("the warning does not name a remedy:\n%s", buf)
			}
		})
	}
}
