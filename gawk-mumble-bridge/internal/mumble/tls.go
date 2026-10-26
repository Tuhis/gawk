package mumble

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

// TLSOptions says how to trust murmur's certificate. Murmur generates a
// self-signed certificate on first start, so an operator usually pins it
// (CertSHA256) or names the CA that issued a real one (CAFile); skipping
// verification is the lab's last resort.
type TLSOptions struct {
	// ServerName is the name verified against the certificate (and sent
	// as SNI). Empty: the host of the dialled address.
	ServerName string
	// CAFile is a PEM bundle of extra roots; empty: the system roots.
	CAFile string
	// CertSHA256 pins the server's leaf certificate by the SHA-256 of its
	// DER bytes (hex, colons allowed). It replaces chain and name
	// verification: the pin IS the identity.
	CertSHA256 string
	// InsecureSkipVerify accepts any certificate.
	InsecureSkipVerify bool
}

// ParseCertPin decodes a -mumble-cert-sha256 value.
func ParseCertPin(s string) ([]byte, error) {
	clean := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	pin, err := hex.DecodeString(clean)
	if err != nil || len(pin) != sha256.Size {
		return nil, fmt.Errorf("mumble: certificate pin %q is not a SHA-256 in hex", s)
	}
	return pin, nil
}

// Config builds the client TLS configuration.
func (o TLSOptions) Config(host string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: o.ServerName, MinVersion: tls.VersionTLS12}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	switch {
	case o.CertSHA256 != "":
		pin, err := ParseCertPin(o.CertSHA256)
		if err != nil {
			return nil, err
		}
		// Chain and name checks are replaced by the pin, which
		// VerifyPeerCertificate enforces on every handshake.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("mumble: server sent no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if !bytes.Equal(sum[:], pin) {
				return fmt.Errorf("mumble: server certificate SHA-256 %s does not match the pin", hex.EncodeToString(sum[:]))
			}
			return nil
		}
	case o.InsecureSkipVerify:
		cfg.InsecureSkipVerify = true
	}
	if o.CAFile != "" && o.CertSHA256 == "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("mumble: CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mumble: CA file %s holds no PEM certificate", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// NewClientCertificate returns a fresh self-signed client certificate.
//
// Murmur refuses to create a temporary channel for a user without one
// (PermissionDenied MissingCertificate), so every bridged connection
// presents its own: a fresh key per connection, never stored, so a Mumble
// ban of one bridged user (which bans the certificate's hash) never reaches
// another. The bridged users stay unregistered (D10).
func NewClientCertificate(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
