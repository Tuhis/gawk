// gawk-devcert generates a self-signed development certificate that
// Chromium accepts for WebTransport (ECDSA P-256, ≤ 14 days), writes it as
// a PEM pair and prints the hashes and ready-to-paste snippets needed to
// connect to it from a browser.
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gawk-devcert:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", ".", "directory to write cert.pem and key.pem into")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "comma-separated DNS names / IPs for the certificate")
	days := flag.Int("days", 13, "validity in days (max 14, Chromium requirement)")
	port := flag.Int("port", 4433, "server port used in the printed Chrome flags")
	flag.Parse()

	cert, err := tlsutil.GenerateDevCert(strings.Split(*hosts, ","), time.Duration(*days)*24*time.Hour)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	certPath := filepath.Join(*out, "cert.pem")
	keyPath := filepath.Join(*out, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	spki := tlsutil.SPKIFingerprint(cert.Leaf)
	certHash := tlsutil.CertHashHex(cert.Leaf)
	firstHost := strings.TrimSpace(strings.Split(*hosts, ",")[0])

	fmt.Printf(`wrote %s and %s (valid until %s)

SPKI fingerprint (base64 SHA-256):
  %s

Certificate hash (hex SHA-256 of DER):
  %s

Chrome launch flags:
  --origin-to-force-quic-on=%s:%d --ignore-certificate-errors-spki-list=%s

JS (no flags needed, works in stock Chrome):
  const hash = Uint8Array.from("%s".match(/../g), b => parseInt(b, 16));
  const wt = new WebTransport("https://%s:%d/echo", {
    serverCertificateHashes: [{ algorithm: "sha-256", value: hash }],
  });
`, certPath, keyPath, cert.Leaf.NotAfter.Format(time.RFC3339),
		spki, certHash,
		firstHost, *port, spki,
		certHash, firstHost, *port)

	return nil
}
