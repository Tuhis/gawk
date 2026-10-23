package popsrc

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// installPopCAS gives the fake tracker resourceVersion semantics for Pops
// (roomcluster's installCAS): an update must carry the stored version or it
// is a Conflict, and a successful write bumps it.
func installPopCAS(client *dynamicfake.FakeDynamicClient) {
	var mu sync.Mutex
	client.PrependReactor("update", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		cur, err := client.Tracker().Get(pops.GroupVersionResource, action.GetNamespace(), obj.GetName())
		if err != nil {
			return true, nil, err
		}
		rv := cur.(*unstructured.Unstructured).GetResourceVersion()
		if obj.GetResourceVersion() != rv {
			return true, nil, apierrors.NewConflict(pops.GroupVersionResource.GroupResource(), obj.GetName(), errors.New("resourceVersion mismatch"))
		}
		n, _ := strconv.Atoi(rv)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		return false, nil, nil
	})
}

func newStatusFake(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{pops.GroupVersionResource: pops.ListKind}, objs...)
	installPopCAS(client)
	return client
}

func pairablePop(t *testing.T, name, tokenSHA string, expires time.Time) *unstructured.Unstructured {
	p := &pops.Pop{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uidA, ResourceVersion: "1"}}
	exp := metav1.NewTime(expires)
	p.Spec.Pairing = pops.Pairing{TokenSHA256: tokenSHA, ExpiresAt: &exp}
	return popObject(t, p)
}

func getPop(t *testing.T, client *dynamicfake.FakeDynamicClient, name string) *pops.Pop {
	t.Helper()
	u, err := client.Resource(pops.GroupVersionResource).Namespace(testNamespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var p pops.Pop
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// A pairing string pairs exactly one PoP: the redeem writes the credential
// hash with the string it was paired under, and a second redeem of the same
// string is ErrPairingUsed, even when it raced the first.
func TestK8sRedeemPairingPairsOnce(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tok := pops.TokenSHA256("K7M2Q-9XDA4")
	client := newStatusFake(t, pairablePop(t, "assembly-2026", tok, now.Add(time.Hour)))
	st := NewK8sStatus(client, testNamespace)

	if err := st.RedeemPairing(t.Context(), "assembly-2026", tok, credHash, now); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	p := getPop(t, client, "assembly-2026")
	if p.Status.Credential == nil || p.Status.Credential.SHA256 != credHash ||
		p.Status.Credential.PairingTokenSHA256 != tok || p.Status.Credential.PairedAt == nil {
		t.Fatalf("status.credential = %+v", p.Status.Credential)
	}
	if err := p.CheckCredential("the-credential", now); err != nil {
		t.Errorf("the paired credential does not check: %v", err)
	}
	if err := st.RedeemPairing(t.Context(), "assembly-2026", tok, pops.CredentialSHA256("other"), now); !errors.Is(err, pops.ErrPairingUsed) {
		t.Errorf("second redeem = %v, want ErrPairingUsed", err)
	}
	if got := getPop(t, client, "assembly-2026"); got.Status.Credential.SHA256 != credHash {
		t.Error("a second redeem replaced the credential")
	}

	// Concurrent redeems: exactly one wins.
	tok2 := pops.TokenSHA256("ABCDE-FGHIJ")
	client2 := newStatusFake(t, pairablePop(t, "lan", tok2, now.Add(time.Hour)))
	st2 := NewK8sStatus(client2, testNamespace)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = st2.RedeemPairing(t.Context(), "lan", tok2, pops.CredentialSHA256(strconv.Itoa(i)), now)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, pops.ErrPairingUsed):
			t.Errorf("concurrent redeem: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d concurrent redeems succeeded, want exactly 1", ok)
	}
}

func TestK8sRedeemPairingRefusals(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tok := pops.TokenSHA256("K7M2Q-9XDA4")
	client := newStatusFake(t, pairablePop(t, "expired", tok, now.Add(-time.Second)))
	st := NewK8sStatus(client, testNamespace)
	if err := st.RedeemPairing(t.Context(), "expired", tok, credHash, now); !errors.Is(err, pops.ErrPairingExpired) {
		t.Errorf("expired = %v", err)
	}
	if err := st.RedeemPairing(t.Context(), "expired", pops.TokenSHA256("other"), credHash, now); !errors.Is(err, pops.ErrPairingUnknown) {
		t.Errorf("wrong token = %v", err)
	}
	if err := st.RedeemPairing(t.Context(), "missing", tok, credHash, now); !errors.Is(err, pops.ErrPairingUnknown) {
		t.Errorf("missing Pop = %v", err)
	}
}

// The link lease: a force-take bumps the generation whatever it was, a
// renewal is a CAS on the generation, and a release clears the holder and
// the link's connected flag in one write.
func TestK8sLease(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	client := newStatusFake(t, pairablePop(t, "assembly-2026", "aa", now))
	st := NewK8sStatus(client, testNamespace)
	ctx := t.Context()

	gen, err := st.TakeLease(ctx, "assembly-2026", "pod-a", "10.0.0.1:4433", now)
	if err != nil || gen != 1 {
		t.Fatalf("take = %d, %v", gen, err)
	}
	if err := st.PatchLink(ctx, "assembly-2026", pops.Link{Connected: true, Version: "0.44.0", Proto: 1, LanAddr: "192.168.1.20"}); err != nil {
		t.Fatal(err)
	}
	// pod-b force-takes on a fresh link.
	gen2, err := st.TakeLease(ctx, "assembly-2026", "pod-b", "10.0.0.2:4433", now)
	if err != nil || gen2 != 2 {
		t.Fatalf("force-take = %d, %v", gen2, err)
	}
	// pod-a's renewal sees its generation superseded.
	renew := pops.Lease{Holder: "pod-a", Addr: "10.0.0.1:4433", Generation: 1, RenewedAt: &metav1.Time{Time: now}}
	if err := st.PatchLease(ctx, "assembly-2026", renew, 1); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale renew = %v, want ErrLeaseLost", err)
	}
	// So does its release: it must not clear pod-b's link.
	if err := st.PatchLease(ctx, "assembly-2026", pops.Lease{}, 1); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale release = %v, want ErrLeaseLost", err)
	}
	p := getPop(t, client, "assembly-2026")
	if p.Status.Lease.Holder != "pod-b" || p.Status.Lease.Generation != 2 || !p.Status.Link.Connected {
		t.Fatalf("after the stale writes: lease %+v link %+v", p.Status.Lease, p.Status.Link)
	}
	later := metav1.NewTime(now.Add(5 * time.Second))
	if err := st.PatchLease(ctx, "assembly-2026", pops.Lease{Holder: "pod-b", Addr: "10.0.0.2:4433", Generation: 2, RenewedAt: &later}, 2); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := st.AddPublicAddr(ctx, "assembly-2026", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPublicAddr(ctx, "assembly-2026", "2001:db8:1:2::/64"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPublicAddr(ctx, "assembly-2026", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	if err := st.PatchCSR(ctx, "assembly-2026", "csr-pem"); err != nil {
		t.Fatal(err)
	}
	p = getPop(t, client, "assembly-2026")
	if got := p.Status.Link.PublicAddrs; len(got) != 2 || got[0] != "203.0.113.7" || got[1] != "2001:db8:1:2::/64" {
		t.Errorf("publicAddrs = %v", got)
	}
	if p.Status.TLS == nil || p.Status.TLS.CSR != "csr-pem" || !p.Status.Lease.RenewedAt.Equal(&later) {
		t.Errorf("tls %+v lease %+v", p.Status.TLS, p.Status.Lease)
	}
	if err := st.PatchLease(ctx, "assembly-2026", pops.Lease{}, 2); err != nil {
		t.Fatalf("release: %v", err)
	}
	p = getPop(t, client, "assembly-2026")
	if p.Status.Lease.Holder != "" || p.Status.Lease.Generation != 2 || p.Status.Link.Connected {
		t.Errorf("after release: lease %+v link %+v", p.Status.Lease, p.Status.Link)
	}
	// gawk-admin's half of status.tls survives the relay's writes.
	if p.Status.Credential != nil {
		t.Errorf("a lease write invented a credential: %+v", p.Status.Credential)
	}
}

func TestMemStatus(t *testing.T) {
	now := time.Now()
	st := NewMemStatus(nil)
	ctx := t.Context()
	if err := st.RedeemPairing(ctx, "lan", "aa", credHash, now); !errors.Is(err, pops.ErrPairingUnknown) {
		t.Errorf("-pops-file has no pairing: %v", err)
	}
	g1, err := st.TakeLease(ctx, "lan", "pod", "", now)
	if err != nil || g1 != 1 {
		t.Fatalf("take = %d, %v", g1, err)
	}
	g2, _ := st.TakeLease(ctx, "lan", "pod", "", now)
	if g2 != 2 {
		t.Fatalf("second take = %d", g2)
	}
	if err := st.PatchLease(ctx, "lan", pops.Lease{Holder: "pod", Generation: 1}, 1); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale renew = %v", err)
	}
	if err := st.PatchLease(ctx, "lan", pops.Lease{Holder: "pod", Generation: 2}, 2); err != nil {
		t.Errorf("renew = %v", err)
	}
	for _, err := range []error{
		st.PatchLink(ctx, "lan", pops.Link{Connected: true}),
		st.AddPublicAddr(ctx, "lan", "203.0.113.7"),
		st.PatchCSR(ctx, "lan", "pem"),
	} {
		if err != nil {
			t.Error(err)
		}
	}
}

// Get is the fresh read Authenticate falls back to when the informer cache
// has not caught up with a redeem: k8s reads the API, memory merges its
// status over the file source's Pop.
func TestStatusGet(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	client := newStatusFake(t, pairablePop(t, "assembly-2026", "aa", now.Add(time.Hour)))
	st := NewK8sStatus(client, testNamespace)
	if err := st.RedeemPairing(t.Context(), "assembly-2026", "aa", credHash, now); err != nil {
		t.Fatal(err)
	}
	p, err := st.Get(t.Context(), "assembly-2026")
	if err != nil || p.Status.Credential == nil || p.Status.Credential.SHA256 != credHash {
		t.Fatalf("k8s Get = %+v, %v", p, err)
	}
	if _, err := st.Get(t.Context(), "nope"); err == nil {
		t.Error("k8s Get of a missing Pop succeeded")
	}

	src := newStore()
	fp := pops.FilePop{Name: "lan", UID: uidA, CredentialSHA256: credHash}.ToPop()
	src.put(&fp)
	mem := NewMemStatus(src)
	if _, err := mem.TakeLease(t.Context(), "lan", "pod", "10.0.0.1:4433", now); err != nil {
		t.Fatal(err)
	}
	p, err = mem.Get(t.Context(), "lan")
	if err != nil || p.Status.Credential == nil || p.Status.Credential.SHA256 != credHash ||
		p.Status.Lease == nil || p.Status.Lease.Holder != "pod" {
		t.Fatalf("mem Get = %+v, %v", p, err)
	}
	if _, err := mem.Get(t.Context(), "nope"); err == nil {
		t.Error("mem Get of a missing Pop succeeded")
	}
}
