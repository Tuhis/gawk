// Package tlsutil provides the two certificate paths the server supports:
// ephemeral self-signed dev certificates that Chromium accepts for
// WebTransport, and file-backed certificates (e.g. mounted from a
// cert-manager Secret) that reload transparently when renewed.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// MaxDevCertValidity is Chromium's hard upper limit on the validity period
// of certificates used with serverCertificateHashes; longer-lived certs are
// rejected for QUIC regardless of trust flags.
const MaxDevCertValidity = 14 * 24 * time.Hour

// GenerateDevCert creates a self-signed ECDSA P-256 certificate for the
// given hosts (DNS names or IP addresses). Chromium requires ECDSA and a
// validity period of at most 14 days for WebTransport; validity is capped
// accordingly. The returned certificate has Leaf populated.
func GenerateDevCert(hosts []string, validity time.Duration) (tls.Certificate, error) {
	if len(hosts) == 0 {
		return tls.Certificate{}, fmt.Errorf("tlsutil: no hosts given")
	}
	if validity <= 0 || validity > MaxDevCertValidity {
		validity = MaxDevCertValidity
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: generate serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gawk-server dev cert"},
		NotBefore:    now.Add(-time.Hour), // tolerate clock skew
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: parse generated certificate: %w", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// SPKIFingerprint returns base64(SHA-256(SubjectPublicKeyInfo)), the value
// Chromium expects in --ignore-certificate-errors-spki-list.
func SPKIFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// CertHashHex returns hex(SHA-256(certificate DER)), the copy-paste form of
// the hash the JS WebTransport constructor takes in serverCertificateHashes.
func CertHashHex(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
