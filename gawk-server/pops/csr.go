package pops

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// The named tier's certificate request rules (docs/76 §6.11 item 38),
// shared by the three parties that read a CSR: the PoP that writes it, the
// link-home pod that stores it in status.tls.csr, and the gawk-admin
// leader that turns it into a cert-manager CertificateRequest. One
// definition, so "the CSR's hash" and "a valid CSR" never drift between
// them.

// ErrBadCSR wraps every reason ValidateCSR refuses a request.
var ErrBadCSR = errors.New("pops: bad certificate request")

// ErrBadChain wraps every reason CheckChain refuses an issued chain.
var ErrBadChain = errors.New("pops: bad certificate chain")

// MaxCSRBytes bounds a CSR's PEM: a P-256 request with one SAN is under
// 600 bytes.
const MaxCSRBytes = 4 << 10

// CSRSHA256 is the hex SHA-256 of the request's DER — the value
// status.tls.issuedForCSR holds. Hashing the DER rather than the text makes
// PEM framing (line ends, whitespace) irrelevant. "" for anything that is
// not one PEM CERTIFICATE REQUEST.
func CSRSHA256(csrPEM string) string {
	der, ok := csrDER(csrPEM)
	if !ok {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func csrDER(csrPEM string) ([]byte, bool) {
	if len(csrPEM) > MaxCSRBytes {
		return nil, false
	}
	b := bytes.ReplaceAll([]byte(strings.TrimSpace(csrPEM)), []byte("\r\n"), []byte("\n"))
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, false
	}
	return block.Bytes, true
}

// ValidateCSR parses csrPEM and checks it is what a PoP may ask for: a
// valid self-signature, a P-256 key, exactly one SAN — the DNS name host,
// which gawk-admin assigned — and no other name of any kind (a subject CN,
// if present, must be the same host). Anything else is ErrBadCSR.
func ValidateCSR(csrPEM, host string) (*x509.CertificateRequest, error) {
	host = normHost(host)
	if host == "" {
		return nil, fmt.Errorf("%w: no host is assigned", ErrBadCSR)
	}
	der, ok := csrDER(csrPEM)
	if !ok {
		return nil, fmt.Errorf("%w: not one PEM CERTIFICATE REQUEST", ErrBadCSR)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrBadCSR, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the key must be ECDSA P-256", ErrBadCSR)
	}
	if len(csr.DNSNames) != 1 || normHost(csr.DNSNames[0]) != host {
		return nil, fmt.Errorf("%w: want exactly one SAN %q, got %q", ErrBadCSR, host, csr.DNSNames)
	}
	if len(csr.IPAddresses) != 0 || len(csr.EmailAddresses) != 0 || len(csr.URIs) != 0 {
		return nil, fmt.Errorf("%w: only the DNS name %q may be named", ErrBadCSR, host)
	}
	if cn := csr.Subject.CommonName; cn != "" && normHost(cn) != host {
		return nil, fmt.Errorf("%w: subject CN %q is not %q", ErrBadCSR, cn, host)
	}
	return csr, nil
}

// CheckChain parses an issued PEM chain and checks its leaf answers csr:
// the same public key, valid for host. It returns the parsed chain, leaf
// first. It does not verify the issuer: the PoP trusts what its link
// delivers, and a browser verifies the rest.
func CheckChain(chainPEM string, csr *x509.CertificateRequest, host string) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := []byte(chainPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadChain, err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: no certificate", ErrBadChain)
	}
	leaf := chain[0]
	want, ok := csr.PublicKey.(*ecdsa.PublicKey)
	got, ok2 := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ok2 || !want.Equal(got) {
		return nil, fmt.Errorf("%w: the leaf's key is not the request's", ErrBadChain)
	}
	if err := leaf.VerifyHostname(normHost(host)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadChain, err)
	}
	return chain, nil
}

func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}
