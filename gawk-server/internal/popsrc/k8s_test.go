package popsrc

import (
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

const testNamespace = "gawk"

func popObject(t *testing.T, p *pops.Pop) *unstructured.Unstructured {
	t.Helper()
	p.TypeMeta = metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind}
	p.Namespace = testNamespace
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: raw}
}

func newPop(name, uid, display string) *pops.Pop {
	return &pops.Pop{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec:       pops.PopSpec{DisplayName: display, Pairing: pops.Pairing{TokenSHA256: "aa"}},
		Status:     pops.PopStatus{Credential: &pops.Credential{SHA256: credHash, PairingTokenSHA256: "aa"}},
	}
}

func TestK8sSourceFollowsTheCRs(t *testing.T) {
	// client-go defaults WatchListClient on; the fake tracker speaks plain
	// LIST+WATCH (the moderationsrc / roomcluster tests' reason).
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	// An empty scheme on purpose (roomcluster's newFakeDynamic explains).
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{pops.GroupVersionResource: pops.ListKind},
		popObject(t, newPop("assembly-2026", uidA, "Assembly 2026")))

	// Mutations before the informer's watch is registered would be lost by
	// the fake tracker; wait for it (roomcluster's roomWatchRegistered).
	registered := make(chan struct{})
	var once sync.Once
	client.PrependWatchReactor("pops", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(pops.GroupVersionResource, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(registered) })
		return true, w, nil
	})

	src, err := StartK8s(t.Context(), client, testNamespace, discard)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "sync", src.Synced)
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("the informer never registered its watch")
	}
	var ev events
	defer src.Subscribe(ev.add)()

	p, ok := src.Get("assembly-2026")
	if !ok || string(p.UID) != uidA || p.Spec.DisplayName != "Assembly 2026" {
		t.Fatalf("Get = %+v, %v", p, ok)
	}
	if err := p.CheckCredential("the-credential", time.Now()); err != nil {
		t.Errorf("credential from the CR status: %v", err)
	}
	p.Status.Credential.SHA256 = "mutated"
	if again, _ := src.Get("assembly-2026"); again.Status.Credential.SHA256 != credHash {
		t.Error("Get aliases the informer's copy")
	}

	ri := client.Resource(pops.GroupVersionResource).Namespace(testNamespace)
	// Add.
	if _, err := ri.Create(t.Context(), popObject(t, newPop("lan", uidB, "LAN")), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "add", func() bool { _, ok := src.Get("lan"); return ok && ev.seen("lan") == 1 })

	// Update: a Re-pair (spec.pairing) reaches every reader at once.
	repaired := newPop("assembly-2026", uidA, "Assembly 2026")
	repaired.Spec.Pairing.TokenSHA256 = "bb"
	if _, err := ri.Update(t.Context(), popObject(t, repaired), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "update", func() bool {
		p, _ := src.Get("assembly-2026")
		return p != nil && p.Spec.Pairing.TokenSHA256 == "bb" && ev.seen("assembly-2026") == 1
	})
	if p, _ := src.Get("assembly-2026"); p.CredentialCurrent() {
		t.Error("a re-paired Pop's old credential is still current")
	}

	// A CR whose name cannot be a label value is never served.
	bad := newPop("a.b", uidB, "dotted")
	if _, err := ri.Create(t.Context(), popObject(t, bad), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Delete.
	if err := ri.Delete(t.Context(), "lan", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "delete", func() bool { _, ok := src.Get("lan"); return !ok && ev.seen("lan") == 2 })
	if _, ok := src.Get("a.b"); ok {
		t.Error("a Pop with a non-label name is served")
	}
	if l := src.List(); len(l) != 1 || l[0].Name != "assembly-2026" {
		t.Errorf("List = %+v", l)
	}
}
