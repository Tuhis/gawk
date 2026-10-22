package popsrv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/netip"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// signAs plays the ACME issuer for a CSR: a 6-day leaf for its name.
func signAs(t *testing.T, csrPEM string) string {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ACME"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(6 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The named tier end to end over a real link: home assigns the host, the
// PoP's CSR names it, gawk-admin (played here) issues it, the PoP installs
// it and announces the name, and home's offer view then lists it.
func TestTheNamedTierOverTheLink(t *testing.T) {
	f, pop := StartHomeAndPop(t, 2)
	for _, pod := range f.Pods {
		t.Cleanup(pod.Manager.EnableNamed())
	}
	lan := netip.MustParseAddr("192.168.1.20")
	named := popclient.NewNamed(pop.Client, popclient.NamedOptions{
		StateDir: pop.StateDir,
		LAN:      func() netip.Addr { return lan },
		System:   staticResolver{namedHost: lan},
		Public:   staticResolver{namedHost: lan},
		Log:      slog.New(slog.DiscardHandler),
	})
	pairLinked(t, f, pop)
	enableNamed(t, f)
	adminTLS(t, f, popName, map[string]any{"host": namedHost}, func(x *pops.TLS) bool { return x.Host == namedHost })

	// docs/76 §11 PP12: the PoP's CSR names the host it got in config.
	WaitFor(t, "the PoP's csr in status", func() bool { return storedCSR(f, t) != "" })
	csr := storedCSR(f, t)
	if _, err := pops.ValidateCSR(csr, namedHost); err != nil {
		t.Fatalf("stored CSR: %v", err)
	}
	if named.Host() != "" {
		t.Fatal("a host before issuance")
	}

	sha := pops.CSRSHA256(csr)
	adminTLS(t, f, popName, map[string]any{"certificate": signAs(t, csr), "issuedForCSR": sha},
		func(x *pops.TLS) bool { return x.IssuedForCSR == sha })
	WaitFor(t, "the PoP installs its certificate", func() bool { return named.Host() == namedHost })
	if named.Certificate(&tls.ClientHelloInfo{ServerName: namedHost}) == nil {
		t.Fatal("the relay would not answer its name")
	}
	WaitFor(t, "home lists the named host", func() bool {
		holder := f.LinkHolder(popName)
		if holder == nil {
			return false
		}
		for _, lp := range holder.Manager.Linked() {
			if lp.Name == popName {
				return lp.NamedHost == namedHost
			}
		}
		return false
	})
	if got := named.CheckDNS(t.Context()); got != popclient.DNSOK || named.GuestHost() != namedHost {
		t.Fatalf("self-check = %s, guest host %q", got, named.GuestHost())
	}
}

type staticResolver map[string]netip.Addr

func (s staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return []netip.Addr{a}, nil
	}
	return nil, nil
}
