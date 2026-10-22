package popsrv

// The named tier's home half (docs/76 §11 PP12) in-process: CSR records
// reach status.tls.csr, and a chain reaches the PoP only for the CSR it
// answers.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

const namedHost = "p-7k2m9x.pop.gawk.test"

// adminTLS writes status.tls paths as gawk-admin does (a merge patch on the
// status subresource) and waits until every pod sees them.
func adminTLS(t *testing.T, f *HomeFleet, name string, tls map[string]any, seen func(*pops.TLS) bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"status": map[string]any{"tls": tls}})
	if _, err := f.Pops.Resource(pops.GroupVersionResource).Namespace(HarnessNamespace).Patch(
		context.Background(), name, types.MergePatchType, b, metav1.PatchOptions{}, "status"); err != nil {
		t.Fatalf("patch tls: %v", err)
	}
	f.waitAllSee(t, name, func(p *pops.Pop) bool { return p.Status.TLS != nil && seen(p.Status.TLS) })
}

// mergeSpec merge-patches the Pop's spec as gawk-admin's PatchSpec does
// (no resourceVersion precondition, so it cannot lose a race with the
// link's own status writes) and waits until every pod sees it.
func mergeSpec(t *testing.T, f *HomeFleet, spec map[string]any, seen func(*pops.Pop) bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"spec": spec})
	if _, err := f.Pops.Resource(pops.GroupVersionResource).Namespace(HarnessNamespace).Patch(
		context.Background(), popName, types.MergePatchType, b, metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch spec: %v", err)
	}
	f.waitAllSee(t, popName, seen)
}

func enableNamed(t *testing.T, f *HomeFleet) {
	t.Helper()
	mergeSpec(t, f, map[string]any{"namedAddress": map[string]any{"enabled": true}},
		func(p *pops.Pop) bool { return p.Spec.NamedAddress.Enabled })
}

func testCSR(t *testing.T, names ...string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// namedFleet is a one-pod fleet with the named tier on and a paired PoP
// whose Pop has the named address enabled and a host assigned.
func namedFleet(t *testing.T) (*HomeFleet, *PoP, chan poplink.Certificate) {
	t.Helper()
	f, pop := StartHomeAndPop(t, 1)
	for _, pod := range f.Pods {
		t.Cleanup(pod.Manager.EnableNamed())
	}
	certs := make(chan poplink.Certificate, 8)
	pop.Client.OnRecord(poplink.TCertificate, func(rec any) { certs <- *rec.(*poplink.Certificate) })
	pairLinked(t, f, pop)
	enableNamed(t, f)
	adminTLS(t, f, popName, map[string]any{"host": namedHost}, func(x *pops.TLS) bool { return x.Host == namedHost })
	WaitFor(t, "config carries the named host", func() bool {
		c := pop.Client.State().Config
		return c != nil && c.NamedHost == namedHost
	})
	return f, pop, certs
}

func storedCSR(f *HomeFleet, t *testing.T) string {
	p := f.GetPop(t, popName)
	if p.Status.TLS == nil {
		return ""
	}
	return p.Status.TLS.CSR
}

func noCertificate(t *testing.T, certs chan poplink.Certificate, within time.Duration) {
	t.Helper()
	select {
	case c := <-certs:
		t.Fatalf("unexpected certificate for CSR %s", c.ForCSRSHA256)
	case <-time.After(within):
	}
}

func nextCertificate(t *testing.T, certs chan poplink.Certificate) poplink.Certificate {
	t.Helper()
	select {
	case c := <-certs:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no certificate reached the PoP")
	}
	return poplink.Certificate{}
}

// docs/76 §11 PP12: the PoP never receives the previous chain for a
// renewal CSR, and gets each chain once.
func TestTheLinkDeliversAChainOnlyForTheCSRItAnswers(t *testing.T) {
	f, pop, certs := namedFleet(t)

	csr1 := testCSR(t, namedHost)
	if err := pop.Client.Send(poplink.CSR{PEM: csr1}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "csr 1 stored", func() bool { return storedCSR(f, t) == csr1 })
	noCertificate(t, certs, 200*time.Millisecond)

	sha1 := pops.CSRSHA256(csr1)
	adminTLS(t, f, popName, map[string]any{"certificate": "CHAIN-1", "issuedForCSR": sha1},
		func(x *pops.TLS) bool { return x.IssuedForCSR == sha1 })
	if c := nextCertificate(t, certs); c.ChainPEM != "CHAIN-1" || c.ForCSRSHA256 != sha1 {
		t.Fatalf("certificate = %+v, want CHAIN-1 for csr 1", c)
	}
	// Another Pop update does not send it again.
	mergeSpec(t, f, map[string]any{"displayName": "Assembly 2026 (day 2)"},
		func(p *pops.Pop) bool { return p.Spec.DisplayName == "Assembly 2026 (day 2)" })
	noCertificate(t, certs, 300*time.Millisecond)

	// Renewal at half-life: the PoP sends CSR 2. Until gawk-admin issues
	// it, status still holds chain 1 issued for CSR 1 — which must never
	// be sent as an answer, whatever updates arrive meanwhile.
	csr2 := testCSR(t, namedHost)
	if err := pop.Client.Send(poplink.CSR{PEM: csr2}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "csr 2 stored", func() bool { return storedCSR(f, t) == csr2 })
	mergeSpec(t, f, map[string]any{"displayName": "Assembly 2026 (day 3)"},
		func(p *pops.Pop) bool { return p.Spec.DisplayName == "Assembly 2026 (day 3)" })
	noCertificate(t, certs, 300*time.Millisecond)

	sha2 := pops.CSRSHA256(csr2)
	adminTLS(t, f, popName, map[string]any{"certificate": "CHAIN-2", "issuedForCSR": sha2},
		func(x *pops.TLS) bool { return x.IssuedForCSR == sha2 })
	if c := nextCertificate(t, certs); c.ChainPEM != "CHAIN-2" || c.ForCSRSHA256 != sha2 {
		t.Fatalf("certificate = %+v, want CHAIN-2 for csr 2", c)
	}
}

// The PoP asked for CSR 2 on this link, but the informer still shows CSR 1
// issued (the relay's own csr write has not come back yet): no chain.
func TestARenewalCSRIsNotAnsweredFromAStaleStatus(t *testing.T) {
	f, pop, certs := namedFleet(t)
	csr1 := testCSR(t, namedHost)
	if err := pop.Client.Send(poplink.CSR{PEM: csr1}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "csr 1 stored", func() bool { return storedCSR(f, t) == csr1 })
	sha1 := pops.CSRSHA256(csr1)
	adminTLS(t, f, popName, map[string]any{"certificate": "CHAIN-1", "issuedForCSR": sha1},
		func(x *pops.TLS) bool { return x.IssuedForCSR == sha1 })
	nextCertificate(t, certs)

	// Stop CSR writes from landing, so status keeps CSR 1 issued.
	l := f.Pods[0].Manager.local(popName)
	n := &namedCerts{m: f.Pods[0].Manager, asked: map[string]linkSHA{}, sent: map[string]linkSHA{}}
	n.asked[popName] = linkSHA{l, pops.CSRSHA256(testCSR(t, namedHost))}
	n.deliver(popName)
	noCertificate(t, certs, 200*time.Millisecond)
}

// A relink (or a restart of the PoP) gets the issued chain again on the
// new link: the PoP may have lost it.
func TestARelinkGetsTheIssuedChainAgain(t *testing.T) {
	f, pop, certs := namedFleet(t)
	csr := testCSR(t, namedHost)
	if err := pop.Client.Send(poplink.CSR{PEM: csr}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "csr stored", func() bool { return storedCSR(f, t) == csr })
	sha := pops.CSRSHA256(csr)
	adminTLS(t, f, popName, map[string]any{"certificate": "CHAIN", "issuedForCSR": sha},
		func(x *pops.TLS) bool { return x.IssuedForCSR == sha })
	nextCertificate(t, certs)

	old := f.Pods[0].Manager.local(popName)
	old.close(closeSuperseded, "test relink")
	WaitFor(t, "relinked", func() bool {
		l := f.Pods[0].Manager.local(popName)
		return l != nil && l != old && l.ready.Load()
	})
	if c := nextCertificate(t, certs); c.ForCSRSHA256 != sha {
		t.Fatalf("relink certificate = %+v", c)
	}
}

// docs/76 §11 PP12: a CSR naming anything but the assigned host is never
// stored, so gawk-admin never sees it.
func TestACSRForAnotherNameIsNotStored(t *testing.T) {
	f, pop, _ := namedFleet(t)
	for _, bad := range []string{testCSR(t, "p-zzzzzz.pop.gawk.test"), testCSR(t, namedHost, popName+".pop.gawk.test"), "junk"} {
		if err := pop.Client.Send(poplink.CSR{PEM: bad}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := storedCSR(f, t); got != "" {
		t.Fatalf("a bad CSR was stored: %q", got)
	}
	good := testCSR(t, namedHost)
	if err := pop.Client.Send(poplink.CSR{PEM: good}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "the good csr stored", func() bool { return storedCSR(f, t) == good })
}
