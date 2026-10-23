package pops

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func csrPEM(t *testing.T, key any, tmpl x509.CertificateRequest) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &tmpl, key)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func p256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const host = "p-7k2m9x.pop.gawk.ioio.fi"

func TestValidateCSRAcceptsOneSANForTheAssignedHost(t *testing.T) {
	pemStr := csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host}})
	csr, err := ValidateCSR(pemStr, host)
	if err != nil {
		t.Fatalf("ValidateCSR: %v", err)
	}
	if csr.DNSNames[0] != host {
		t.Fatalf("parsed SAN %v", csr.DNSNames)
	}
	// A CN equal to the host is what some tools add; still one name.
	withCN := csrPEM(t, p256(t), x509.CertificateRequest{Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}})
	if _, err := ValidateCSR(withCN, host); err != nil {
		t.Fatalf("CN == host refused: %v", err)
	}
	// Case and a trailing dot on the assigned host do not matter.
	if _, err := ValidateCSR(pemStr, strings.ToUpper(host)+"."); err != nil {
		t.Fatalf("host case refused: %v", err)
	}
}

func TestValidateCSRRefusesAnythingElse(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good := csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host}})
	tamper := []byte(good)
	block, _ := pem.Decode(tamper)
	block.Bytes[len(block.Bytes)-3] ^= 0xff
	tampered := string(pem.EncodeToMemory(block))

	cases := map[string]struct {
		pem, host string
	}{
		"wrong SAN":        {csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{"p-aaaaaa.pop.gawk.ioio.fi"}}), host},
		"two SANs":         {csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host, "evil.example"}}), host},
		"no SAN":           {csrPEM(t, p256(t), x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}), host},
		"IP SAN":           {csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host}, IPAddresses: []net.IP{net.IPv4(192, 168, 1, 20)}}), host},
		"email SAN":        {csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host}, EmailAddresses: []string{"a@b.c"}}), host},
		"other CN":         {csrPEM(t, p256(t), x509.CertificateRequest{Subject: pkix.Name{CommonName: "assembly-2026"}, DNSNames: []string{host}}), host},
		"RSA key":          {csrPEM(t, rsaKey, x509.CertificateRequest{DNSNames: []string{host}}), host},
		"P-384 key":        {csrPEM(t, p384, x509.CertificateRequest{DNSNames: []string{host}}), host},
		"bad signature":    {tampered, host},
		"not PEM":          {"hello", host},
		"no assigned host": {good, ""},
		"a certificate":    {strings.Replace(good, "CERTIFICATE REQUEST", "CERTIFICATE", -1), host},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateCSR(c.pem, c.host); !errors.Is(err, ErrBadCSR) {
				t.Fatalf("ValidateCSR = %v, want ErrBadCSR", err)
			}
		})
	}
}

func TestCSRSHA256IsTheDERHashAndIgnoresPEMFraming(t *testing.T) {
	pemStr := csrPEM(t, p256(t), x509.CertificateRequest{DNSNames: []string{host}})
	block, _ := pem.Decode([]byte(pemStr))
	sum := sha256.Sum256(block.Bytes)
	want := hex.EncodeToString(sum[:])
	if got := CSRSHA256(pemStr); got != want {
		t.Fatalf("CSRSHA256 = %s, want %s", got, want)
	}
	// CRLF line ends and surrounding whitespace are the same request.
	if got := CSRSHA256("\n\n" + strings.ReplaceAll(pemStr, "\n", "\r\n") + "  "); got != want {
		t.Fatalf("CSRSHA256 with CRLF = %s, want %s", got, want)
	}
	if CSRSHA256("") != "" || CSRSHA256("not pem") != "" {
		t.Fatal("CSRSHA256 of a non-CSR must be empty")
	}
}

func TestCheckChainWantsTheRequestsKeyAndHost(t *testing.T) {
	key := p256(t)
	csr, err := ValidateCSR(csrPEM(t, key, x509.CertificateRequest{DNSNames: []string{host}}), host)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(pub any, names ...string) string {
		ca := p256(t)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, ca)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	if chain, err := CheckChain(issue(&key.PublicKey, host), csr, host); err != nil || len(chain) != 1 {
		t.Fatalf("CheckChain(good) = %d, %v", len(chain), err)
	}
	for name, pemStr := range map[string]string{
		"other key":  issue(&p256(t).PublicKey, host),
		"other host": issue(&key.PublicKey, "p-aaaaaa.pop.gawk.ioio.fi"),
		"empty":      "",
	} {
		if _, err := CheckChain(pemStr, csr, host); !errors.Is(err, ErrBadChain) {
			t.Fatalf("%s: CheckChain = %v, want ErrBadChain", name, err)
		}
	}
}
