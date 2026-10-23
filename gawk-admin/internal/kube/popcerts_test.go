package kube_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

const testZone = "pop.gawk.test"

// newNamedFake is a dynamic fake that knows the three kinds the named tier
// touches: Pops, cert-manager CertificateRequests and external-dns
// DNSEndpoints (no Go types for the last two: docs/76 §9).
func newNamedFake(t *testing.T, objs ...runtime.Object) (*kube.PopClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		pops.GroupVersionResource:       pops.ListKind,
		kube.CertificateRequestResource: "CertificateRequestList",
		kube.DNSEndpointResource:        "DNSEndpointList",
	})
	for _, o := range objs {
		p := o.(*pops.Pop)
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: raw}
		u.SetAPIVersion(pops.SchemeGroupVersion.String())
		u.SetKind(pops.Kind)
		if _, err := dc.Resource(pops.GroupVersionResource).Namespace(testNamespace).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return kube.NewPopClientFor(dc, testNamespace), dc
}

func namedPop(name, uid, lan string) *pops.Pop {
	p := storedPop(name, uid)
	p.Spec.NamedAddress.Enabled = true
	p.Status.Link.LanAddr = lan
	return p
}

func newNamedReconciler(c *kube.PopClient, dc *dynamicfake.FakeDynamicClient, mod ...func(*kube.PopReconcilerOptions)) *kube.PopReconciler {
	o := kube.PopReconcilerOptions{
		Pops: c, Dynamic: dc, Namespace: testNamespace,
		Zone: testZone, Issuer: "ClusterIssuer/letsencrypt-shortlived", DNSTTL: 60 * time.Second,
	}
	for _, m := range mod {
		m(&o)
	}
	return kube.NewPopReconciler(o)
}

func reconcile(t *testing.T, r *kube.PopReconciler) {
	t.Helper()
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
}

func getPop(t *testing.T, c *kube.PopClient, name string) pops.Pop {
	t.Helper()
	o, err := c.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return o.Pop
}

// setCSR writes status.tls.csr as the link-home pod would.
func setCSR(t *testing.T, dc *dynamicfake.FakeDynamicClient, name, csr string) {
	t.Helper()
	ri := dc.Resource(pops.GroupVersionResource).Namespace(testNamespace)
	u, err := ri.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(u.Object, csr, "status", "tls", "csr"); err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func setLan(t *testing.T, dc *dynamicfake.FakeDynamicClient, name, lan string) {
	t.Helper()
	ri := dc.Resource(pops.GroupVersionResource).Namespace(testNamespace)
	u, err := ri.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(u.Object, lan, "status", "link", "lanAddr"); err != nil {
		t.Fatal(err)
	}
	if _, err := ri.Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

type testCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ACME"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{key: key, cert: cert}
}

// sign issues a 6-day leaf for the request, as the shortlived profile would.
func (ca *testCA) sign(t *testing.T, csrPEM string, serial int64) string {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: csr.DNSNames,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(6 * 24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
}

func newCSR(t *testing.T, names ...string) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func csrSHA(csrPEM string) string {
	block, _ := pem.Decode([]byte(csrPEM))
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func listCRs(t *testing.T, dc *dynamicfake.FakeDynamicClient) []unstructured.Unstructured {
	t.Helper()
	l, err := dc.Resource(kube.CertificateRequestResource).Namespace(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// markReady plays cert-manager: the request's chain and a Ready condition.
func markReady(t *testing.T, dc *dynamicfake.FakeDynamicClient, cr *unstructured.Unstructured, chain string) {
	t.Helper()
	u := cr.DeepCopy()
	_ = unstructured.SetNestedField(u.Object, base64.StdEncoding.EncodeToString([]byte(chain)), "status", "certificate")
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Issued"}}, "status", "conditions")
	if _, err := dc.Resource(kube.CertificateRequestResource).Namespace(testNamespace).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func requestCSR(t *testing.T, cr unstructured.Unstructured) string {
	t.Helper()
	s, _, _ := unstructured.NestedString(cr.Object, "spec", "request")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// countCreates counts CertificateRequest creates: "one issuance per CSR".
func countCreates(dc *dynamicfake.FakeDynamicClient) func() int {
	var mu sync.Mutex
	n := 0
	dc.PrependReactor("create", "certificaterequests", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		n++
		mu.Unlock()
		return false, nil, nil
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

var labelShape = regexp.MustCompile(`^p-[a-z0-9]{6}$`)

// The host is a random label chosen once, never the PoP's name: CT logs
// publish every issued name, and the PoP name is the default wall code
// (docs/76 item 38).
func TestPopReconcilerAssignsARandomHostThatIsNotTheName(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("p-abcdef", "uid-1", "192.168.1.20"), namedPop("assembly-2026", "uid-2", "192.168.1.21"))
	labels := []string{"p-abcdef", "p-k2m9xq"}
	r := newNamedReconciler(c, dc, func(o *kube.PopReconcilerOptions) {
		o.NewLabel = func() (string, error) {
			if len(labels) == 0 {
				return kube.NewNamedLabel()
			}
			l := labels[0]
			labels = labels[1:]
			return l, nil
		}
	})
	reconcile(t, r)
	p := getPop(t, c, "p-abcdef")
	if p.Status.TLS == nil || p.Status.TLS.Host != "p-k2m9xq."+testZone {
		t.Fatalf("host = %+v, want p-k2m9xq.%s (the label equal to the name was redrawn)", p.Status.TLS, testZone)
	}
	other := getPop(t, c, "assembly-2026")
	if other.Status.TLS == nil || !strings.HasSuffix(other.Status.TLS.Host, "."+testZone) ||
		!labelShape.MatchString(strings.TrimSuffix(other.Status.TLS.Host, "."+testZone)) ||
		strings.Contains(other.Status.TLS.Host, "assembly") {
		t.Fatalf("host = %+v, want p-<6 random>.%s", other.Status.TLS, testZone)
	}
	// Chosen once: a second pass keeps it.
	reconcile(t, r)
	if again := getPop(t, c, "assembly-2026"); again.Status.TLS.Host != other.Status.TLS.Host {
		t.Fatalf("host changed %q -> %q", other.Status.TLS.Host, again.Status.TLS.Host)
	}
}

func TestNewNamedLabelShape(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		l, err := kube.NewNamedLabel()
		if err != nil {
			t.Fatal(err)
		}
		if !labelShape.MatchString(l) {
			t.Fatalf("label %q", l)
		}
		seen[l] = true
	}
	if len(seen) < 195 {
		t.Fatalf("only %d distinct labels in 200", len(seen))
	}
}

// A PoP that is not enabled, or a fleet with no zone and issuer, gets
// neither a host nor a record.
func TestPopReconcilerLeavesPinnedOnlyPopsAlone(t *testing.T) {
	plain := storedPop("plain", "uid-1")
	c, dc := newNamedFake(t, plain, namedPop("named", "uid-2", "192.168.1.20"))
	reconcile(t, newNamedReconciler(c, dc))
	if p := getPop(t, c, "plain"); p.Status.TLS != nil {
		t.Fatalf("pinned-only pop got tls %+v", p.Status.TLS)
	}
	c2, dc2 := newNamedFake(t, namedPop("named", "uid-2", "192.168.1.20"))
	reconcile(t, newNamedReconciler(c2, dc2, func(o *kube.PopReconcilerOptions) { o.Zone, o.Issuer = "", "" }))
	if p := getPop(t, c2, "named"); p.Status.TLS != nil {
		t.Fatalf("unconfigured fleet assigned %+v", p.Status.TLS)
	}
	if l, _ := dc2.Resource(kube.DNSEndpointResource).Namespace(testNamespace).List(context.Background(), metav1.ListOptions{}); len(l.Items) != 0 {
		t.Fatalf("unconfigured fleet wrote %d DNSEndpoints", len(l.Items))
	}
}

func dnsEndpoint(t *testing.T, dc *dynamicfake.FakeDynamicClient, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u, err := dc.Resource(kube.DNSEndpointResource).Namespace(testNamespace).Get(context.Background(), "pop-"+name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return u, true
}

// One A record, the PoP's LAN address, TTL -pop-dns-ttl; it follows the
// address, and a revoke (or turning the name off, or deleting the PoP)
// deletes it.
func TestPopReconcilerDNSEndpointLifecycle(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	host := getPop(t, c, "lan-a").Status.TLS.Host
	u, ok := dnsEndpoint(t, dc, "lan-a")
	if !ok {
		t.Fatal("no DNSEndpoint")
	}
	check := func(u *unstructured.Unstructured, target string) {
		t.Helper()
		eps, _, _ := unstructured.NestedSlice(u.Object, "spec", "endpoints")
		if len(eps) != 1 {
			t.Fatalf("endpoints = %v", eps)
		}
		ep := eps[0].(map[string]any)
		if ep["dnsName"] != host || ep["recordType"] != "A" || ep["recordTTL"] != int64(60) {
			t.Fatalf("endpoint = %v", ep)
		}
		if ts, _ := ep["targets"].([]any); len(ts) != 1 || ts[0] != target {
			t.Fatalf("targets = %v, want [%s]", ep["targets"], target)
		}
		if u.GetLabels()[pops.LabelKey] != "lan-a" {
			t.Fatalf("labels = %v", u.GetLabels())
		}
		if refs := u.GetOwnerReferences(); len(refs) != 1 || refs[0].UID != types.UID("uid-a") || refs[0].Kind != pops.Kind {
			t.Fatalf("ownerReferences = %v", refs)
		}
	}
	check(u, "192.168.1.20")

	setLan(t, dc, "lan-a", "192.168.1.33")
	reconcile(t, r)
	u, _ = dnsEndpoint(t, dc, "lan-a")
	check(u, "192.168.1.33")

	if _, err := c.Revoke(context.Background(), "lan-a"); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if _, ok := dnsEndpoint(t, dc, "lan-a"); ok {
		t.Fatal("DNSEndpoint survived the revoke")
	}

	// Turning the name off, and an orphan whose Pop is gone, are swept too.
	c2, dc2 := newNamedFake(t, namedPop("lan-b", "uid-b", "192.168.1.20"), namedPop("lan-c", "uid-c", "192.168.1.21"))
	r2 := newNamedReconciler(c2, dc2)
	reconcile(t, r2)
	if _, _, err := c2.PatchSpec(context.Background(), "lan-b", func(s *pops.PopSpec) error { s.NamedAddress.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := dc2.Resource(pops.GroupVersionResource).Namespace(testNamespace).Delete(context.Background(), "lan-c", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r2)
	for _, n := range []string{"lan-b", "lan-c"} {
		if _, ok := dnsEndpoint(t, dc2, n); ok {
			t.Fatalf("DNSEndpoint for %s survived", n)
		}
	}
}

// The record is harmless only because it names a private address (docs/76
// §8): a PoP reporting a public LAN address gets no record under the
// official zone, and one that moves to a public address loses its record.
func TestPopReconcilerPublishesOnlyAPrivateLanAddress(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("pub", "uid-p", "203.0.113.7"), namedPop("ula", "uid-u", "fd00::20"))
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	if _, ok := dnsEndpoint(t, dc, "pub"); ok {
		t.Fatal("a DNSEndpoint for a public LAN address")
	}
	u, ok := dnsEndpoint(t, dc, "ula")
	if !ok {
		t.Fatal("no DNSEndpoint for a ULA address")
	}
	if eps, _, _ := unstructured.NestedSlice(u.Object, "spec", "endpoints"); len(eps) != 1 || eps[0].(map[string]any)["recordType"] != "AAAA" {
		t.Fatalf("endpoints = %v", eps)
	}
	setLan(t, dc, "ula", "2001:db8::20")
	reconcile(t, r)
	if _, ok := dnsEndpoint(t, dc, "ula"); ok {
		t.Fatal("the DNSEndpoint survived a move to a public address")
	}
}

// A relay on a port other than 4433 reports ip:port; the record names
// the address.
func TestPopReconcilerPublishesALanAddressThatCarriesAPort(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20:4434"))
	reconcile(t, newNamedReconciler(c, dc))
	u, ok := dnsEndpoint(t, dc, "lan-a")
	if !ok {
		t.Fatal("no DNSEndpoint")
	}
	eps, _, _ := unstructured.NestedSlice(u.Object, "spec", "endpoints")
	if ts, _ := eps[0].(map[string]any)["targets"].([]any); len(ts) != 1 || ts[0] != "192.168.1.20" {
		t.Fatalf("targets = %v, want [192.168.1.20]", ts)
	}
}

// A PoP named "p" (a valid name) still gets a host: only the random part
// of the label is kept free of the name, not the fixed "p-" prefix.
func TestPopReconcilerAssignsAHostToAPopNamedP(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("p", "uid-1", "192.168.1.20"))
	labels := []string{"p-k2m9xq"}
	r := newNamedReconciler(c, dc, func(o *kube.PopReconcilerOptions) {
		o.NewLabel = func() (string, error) {
			if len(labels) == 0 {
				return "p-pppppp", nil
			}
			l := labels[0]
			labels = labels[1:]
			return l, nil
		}
	})
	reconcile(t, r)
	if p := getPop(t, c, "p"); p.Status.TLS == nil || p.Status.TLS.Host != "p-k2m9xq."+testZone {
		t.Fatalf("host = %+v, want p-k2m9xq.%s", p.Status.TLS, testZone)
	}
}

// docs/76 §11 PP12: a CSR with a wrong SAN is refused — no
// CertificateRequest, nothing written.
func TestPopReconcilerRefusesACSRWithAWrongSAN(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	host := getPop(t, c, "lan-a").Status.TLS.Host
	for _, bad := range []string{
		newCSR(t, "p-zzzzzz."+testZone),
		newCSR(t, host, "lan-a."+testZone),
		newCSR(t, "lan-a"),
	} {
		setCSR(t, dc, "lan-a", bad)
		reconcile(t, r)
		if n := len(listCRs(t, dc)); n != 0 {
			t.Fatalf("a CertificateRequest was created for a bad CSR (%d)", n)
		}
		if p := getPop(t, c, "lan-a"); p.Status.TLS.IssuedForCSR != "" || p.Status.TLS.Certificate != "" {
			t.Fatalf("tls = %+v", p.Status.TLS)
		}
	}
}

// docs/76 §11 PP12: a renewal CSR is issued exactly once (issuedForCSR),
// the previous chain stays labelled with the previous CSR until the new one
// is ready, and the CertificateRequest names the issuer.
func TestPopReconcilerIssuesEachCSRExactlyOnce(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	creates := countCreates(dc)
	ca := newTestCA(t)
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	host := getPop(t, c, "lan-a").Status.TLS.Host

	csr1 := newCSR(t, host)
	setCSR(t, dc, "lan-a", csr1)
	reconcile(t, r)
	reconcile(t, r) // not ready yet: no second request
	crs := listCRs(t, dc)
	if creates() != 1 || len(crs) != 1 {
		t.Fatalf("creates = %d, requests = %d, want 1", creates(), len(crs))
	}
	cr := crs[0]
	if got := requestCSR(t, cr); got != csr1 {
		t.Fatalf("spec.request is not the PoP's CSR")
	}
	if k, _, _ := unstructured.NestedString(cr.Object, "spec", "issuerRef", "kind"); k != "ClusterIssuer" {
		t.Fatalf("issuerRef.kind = %q", k)
	}
	if n, _, _ := unstructured.NestedString(cr.Object, "spec", "issuerRef", "name"); n != "letsencrypt-shortlived" {
		t.Fatalf("issuerRef.name = %q", n)
	}
	if g, _, _ := unstructured.NestedString(cr.Object, "spec", "issuerRef", "group"); g != "cert-manager.io" {
		t.Fatalf("issuerRef.group = %q", g)
	}
	if cr.GetLabels()[pops.LabelKey] != "lan-a" {
		t.Fatalf("labels = %v", cr.GetLabels())
	}
	if p := getPop(t, c, "lan-a"); p.Status.TLS.Certificate != "" {
		t.Fatal("a chain was written before the request was ready")
	}

	chain1 := ca.sign(t, csr1, 2)
	markReady(t, dc, &cr, chain1)
	reconcile(t, r)
	p := getPop(t, c, "lan-a")
	if p.Status.TLS.Certificate != chain1 || p.Status.TLS.IssuedForCSR != csrSHA(csr1) || p.Status.TLS.NotAfter == nil {
		t.Fatalf("tls after issuance = %+v", p.Status.TLS)
	}
	reconcile(t, r)
	if creates() != 1 {
		t.Fatalf("an issued CSR was requested again (%d creates)", creates())
	}

	// Renewal: the PoP writes a new CSR over the old one.
	csr2 := newCSR(t, host)
	setCSR(t, dc, "lan-a", csr2)
	reconcile(t, r)
	reconcile(t, r)
	if creates() != 2 {
		t.Fatalf("renewal creates = %d, want exactly 2 in total", creates())
	}
	if p := getPop(t, c, "lan-a"); p.Status.TLS.IssuedForCSR != csrSHA(csr1) || p.Status.TLS.Certificate != chain1 {
		t.Fatalf("before the renewal is ready, tls must still describe CSR 1: %+v", p.Status.TLS)
	}
	var cr2 *unstructured.Unstructured
	for _, x := range listCRs(t, dc) {
		if requestCSR(t, x) == csr2 {
			cr2 = x.DeepCopy()
		}
	}
	if cr2 == nil {
		t.Fatal("no request for the renewal CSR")
	}
	chain2 := ca.sign(t, csr2, 3)
	markReady(t, dc, cr2, chain2)
	reconcile(t, r)
	if p := getPop(t, c, "lan-a"); p.Status.TLS.Certificate != chain2 || p.Status.TLS.IssuedForCSR != csrSHA(csr2) {
		t.Fatalf("tls after renewal = %+v", p.Status.TLS)
	}
	// The superseded request is cleaned up; the current one stays.
	if crs := listCRs(t, dc); len(crs) != 1 || requestCSR(t, crs[0]) != csr2 {
		t.Fatalf("requests after renewal = %d", len(crs))
	}
	reconcile(t, r)
	if creates() != 2 {
		t.Fatalf("creates = %d after renewal settled, want 2", creates())
	}
}

// An issued chain whose leaf is not for the request's key (a confused
// issuer, a swapped object) is never written.
func TestPopReconcilerRefusesAChainThatDoesNotAnswerTheCSR(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	ca := newTestCA(t)
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	host := getPop(t, c, "lan-a").Status.TLS.Host
	setCSR(t, dc, "lan-a", newCSR(t, host))
	reconcile(t, r)
	cr := listCRs(t, dc)[0]
	markReady(t, dc, &cr, ca.sign(t, newCSR(t, host), 9)) // another key
	if err := r.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("ReconcileOnce accepted a foreign chain silently")
	}
	if p := getPop(t, c, "lan-a"); p.Status.TLS.Certificate != "" || p.Status.TLS.IssuedForCSR != "" {
		t.Fatalf("a foreign chain was written: %+v", p.Status.TLS)
	}
}

// cert-manager never retries a CertificateRequest: a failed or denied one
// is deleted after FailedRetry so the next pass asks again, and not before.
func TestPopReconcilerRetriesAFailedRequestAfterFailedRetry(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	creates := countCreates(dc)
	now := time.Now()
	r := newNamedReconciler(c, dc, func(o *kube.PopReconcilerOptions) {
		o.Now = func() time.Time { return now }
		o.FailedRetry = time.Hour
	})
	reconcile(t, r)
	setCSR(t, dc, "lan-a", newCSR(t, getPop(t, c, "lan-a").Status.TLS.Host))
	reconcile(t, r)
	cr := listCRs(t, dc)[0]
	cr.SetCreationTimestamp(metav1.NewTime(now.Add(-10 * time.Minute)))
	_ = unstructured.SetNestedSlice(cr.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "Failed"}}, "status", "conditions")
	if _, err := dc.Resource(kube.CertificateRequestResource).Namespace(testNamespace).Update(context.Background(), &cr, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if len(listCRs(t, dc)) != 1 || creates() != 1 {
		t.Fatal("a failed request was retried before FailedRetry")
	}
	now = now.Add(time.Hour)
	reconcile(t, r) // deletes the failed one
	reconcile(t, r) // asks again
	if len(listCRs(t, dc)) != 1 || creates() != 2 {
		t.Fatalf("after FailedRetry: requests = %d, creates = %d, want 1 and 2", len(listCRs(t, dc)), creates())
	}
}

// A revoked PoP gets no new issuance: the shortlived certificate it holds
// dies on its own (docs/76 item 38).
func TestPopReconcilerDoesNotIssueForARevokedPop(t *testing.T) {
	c, dc := newNamedFake(t, namedPop("lan-a", "uid-a", "192.168.1.20"))
	r := newNamedReconciler(c, dc)
	reconcile(t, r)
	host := getPop(t, c, "lan-a").Status.TLS.Host
	if _, err := c.Revoke(context.Background(), "lan-a"); err != nil {
		t.Fatal(err)
	}
	setCSR(t, dc, "lan-a", newCSR(t, host))
	reconcile(t, r)
	if n := len(listCRs(t, dc)); n != 0 {
		t.Fatalf("%d requests for a revoked pop", n)
	}
}
