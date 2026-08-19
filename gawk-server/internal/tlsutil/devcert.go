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
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// MaxDevCertValidity is Chromium's hard upper limit on the validity period
// of certificates used with serverCertificateHashes; longer-lived certs are
// rejected for QUIC regardless of trust flags.
const MaxDevCertValidity = 14 * 24 * time.Hour

// devCertCommonName is the Subject this package stamps on the certificates it
// generates. It is load-bearing, not decoration: it is how LoadOrGenerate
// tells a pair it may replace from one it must never touch.
const devCertCommonName = "gawk-server dev cert"

// GenerateDevCert creates a self-signed ECDSA P-256 certificate for the
// given hosts (DNS names or IP addresses). Chromium requires ECDSA and a
// validity period of at most 14 days for WebTransport; validity is capped
// accordingly. The returned certificate has Leaf populated.
func GenerateDevCert(hosts []string, validity time.Duration) (tls.Certificate, error) {
	if len(hosts) == 0 {
		return tls.Certificate{}, fmt.Errorf("tlsutil: no hosts given")
	}
	// Chromium rejects certs used with serverCertificateHashes whose total
	// span (NotAfter-NotBefore) exceeds 14 days. We backdate NotBefore by
	// clockSkew for tolerance, so the future validity must be capped to leave
	// room for it: skew + validity <= MaxDevCertValidity.
	const clockSkew = time.Hour
	if validity <= 0 || validity > MaxDevCertValidity-clockSkew {
		validity = MaxDevCertValidity - clockSkew
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
		Subject:      pkix.Name{CommonName: devCertCommonName},
		NotBefore:    now.Add(-clockSkew), // tolerate clock skew
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

// WriteCertPair writes cert (0644) and key (0600) as PEM to the given paths:
// a CERTIFICATE block and an EC PRIVATE KEY block, the encoding gawk-devcert
// has always produced. Parent directories must already exist. The modes are
// applied explicitly rather than left to the umask, so a key written under a
// permissive umask is still owner-only.
func WriteCertPair(certPath, keyPath string, cert tls.Certificate) error {
	if len(cert.Certificate) == 0 {
		return fmt.Errorf("tlsutil: certificate has no DER blocks")
	}
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("tlsutil: private key is %T, want *ecdsa.PrivateKey", cert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("tlsutil: marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeFileMode(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("tlsutil: write certificate: %w", err)
	}
	if err := writeFileMode(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("tlsutil: write key: %w", err)
	}
	return nil
}

func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// isReplaceableDevCert reports whether leaf is a certificate this package
// generated: self-issued, and carrying the Subject GenerateDevCert stamps.
// Both halves matter — the Subject alone is copyable by anything, and
// self-issued alone would match a developer's own private CA root.
func isReplaceableDevCert(leaf *x509.Certificate) bool {
	if leaf == nil {
		return false
	}
	// CheckSignature, not CheckSignatureFrom: the latter first insists the
	// issuer be a CA, and these leaves deliberately carry no basic
	// constraints, so it rejects every certificate this package has ever
	// produced. What is being asked here is only "did this key sign this
	// certificate".
	return leaf.Subject.CommonName == devCertCommonName &&
		leaf.Issuer.CommonName == devCertCommonName &&
		leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
}

// LoadOrGenerate returns the certificate at certPath/keyPath, generating and
// writing a fresh dev certificate for hosts when NEITHER file exists, or when
// the pair on disk is an EXPIRED certificate this package issued. generated
// reports whether it wrote one. If exactly one of the two paths exists it
// returns an error without writing anything: half-generating over a stray
// file is how a developer loses a key they meant to keep.
//
// This is what makes a local dev certificate survive a restart (R38, docs/41
// D3) — the hash a browser was given stays valid instead of being invalidated
// by every `-dev-cert` start.
//
// Expiry is the other half of that bargain, and it is not cosmetic: dev certs
// live at most 14 days, and nothing downstream notices a dead one. The
// healthcheck dials without -cert-hash so it skips verification, config-gen
// renders the expired leaf's hash quite happily, and the only thing that
// fails is the browser — opaquely, which is the failure class this milestone
// exists to remove. Regeneration is gated on the certificate being *ours*: an
// expired certificate we did not issue belongs to whoever put it there.
func LoadOrGenerate(certPath, keyPath string, hosts []string, validity time.Duration) (tls.Certificate, bool, error) {
	certExists, err := regularFileExists(certPath)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	keyExists, err := regularFileExists(keyPath)
	if err != nil {
		return tls.Certificate{}, false, err
	}

	switch {
	case certExists && keyExists:
		cert, err := loadPairWithLeaf(certPath, keyPath)
		if err != nil {
			return cert, false, err
		}
		// Fall through to generation only for our own expired pair; anything
		// else on disk is returned exactly as it was found.
		if !time.Now().After(cert.Leaf.NotAfter) || !isReplaceableDevCert(cert.Leaf) {
			return cert, false, nil
		}
	case certExists != keyExists:
		kind, present, absent := "cert", certPath, keyPath
		if keyExists {
			kind, present, absent = "key", keyPath, certPath
		}
		return tls.Certificate{}, false, fmt.Errorf(
			"tlsutil: refusing to overwrite an existing %s file: %s exists but %s does not",
			kind, present, absent)
	}

	cert, err := GenerateDevCert(hosts, validity)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	if err := WriteCertPair(certPath, keyPath, cert); err != nil {
		return tls.Certificate{}, false, err
	}
	// Re-read what was written rather than returning the in-memory pair, so a
	// caller that logs an identity logs the identity of the bytes on disk.
	loaded, err := loadPairWithLeaf(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	return loaded, true, nil
}

func loadPairWithLeaf(certPath, keyPath string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: load certificate: %w", err)
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("tlsutil: parse certificate: %w", err)
		}
		cert.Leaf = leaf
	}
	return cert, nil
}

// regularFileExists distinguishes "absent" from "unreadable": a stat error
// that is not ErrNotExist (a permission problem, a broken mount) must not be
// read as "generate a fresh one over the top".
func regularFileExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	fi, err := os.Stat(path)
	switch {
	case err == nil:
		if fi.IsDir() {
			return false, fmt.Errorf("tlsutil: %s is a directory, want a PEM file", path)
		}
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("tlsutil: stat %s: %w", path, err)
	}
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
