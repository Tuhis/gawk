package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// newFakePopClient builds a PopClient over client-go's dynamic fake, the
// same real client surface the room client's tests use. It hands back the
// fake for tests that need a reactor on the CR verbs.
func newFakePopClient(t *testing.T, objs ...runtime.Object) (*kube.PopClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := pops.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	dc := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
	return kube.NewPopClientFor(dc, testNamespace), dc
}

func storedPop(name, uid string) *pops.Pop {
	issued := metav1.NewTime(time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC))
	expires := metav1.NewTime(issued.Add(72 * time.Hour))
	return &pops.Pop{
		TypeMeta:   metav1.TypeMeta{APIVersion: pops.SchemeGroupVersion.String(), Kind: pops.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID(uid)},
		Spec: pops.PopSpec{
			DisplayName: "Assembly 2026",
			Pairing:     pops.Pairing{TokenSHA256: strings.Repeat("a", 64), IssuedAt: &issued, ExpiresAt: &expires},
			Uplink:      pops.Budget{MaxMbps: 25, MaxStreams: 3},
		},
		Status: pops.PopStatus{
			Credential: &pops.Credential{SHA256: strings.Repeat("c", 64), PairingTokenSHA256: strings.Repeat("a", 64)},
			Link:       &pops.Link{Connected: true, LanAddr: "192.168.1.20"},
		},
	}
}

// rawPop reads the stored object back as the API server holds it.
func rawPop(t *testing.T, dc *dynamicfake.FakeDynamicClient, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := dc.Resource(pops.GroupVersionResource).Namespace(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return u
}

func TestPopClientCreateWritesTheCRWithTheHashOnlyAndStampsTheCreator(t *testing.T) {
	c, dc := newFakePopClient(t)
	ctx := context.Background()
	issued := metav1.NewTime(time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC))
	spec := pops.PopSpec{
		DisplayName: "Assembly 2026",
		Pairing:     pops.Pairing{TokenSHA256: pops.TokenSHA256("AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-4"), IssuedAt: &issued},
		Uplink:      pops.Budget{MaxMbps: 30, MaxStreams: 4},
		Wall:        &pops.Wall{RoomCode: "assembly-2026"},
	}
	obj, err := c.Create(ctx, kube.NewPop{Name: "assembly-2026", Spec: spec, CreatedBy: "sub-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if obj.Name != "assembly-2026" || !obj.Managed || obj.CreatedBy != "sub-1" {
		t.Fatalf("created object = %+v, want the name, managed and the creator", obj)
	}
	u := rawPop(t, dc, "assembly-2026")
	if u.GetAnnotations()[kube.AnnotationPopManaged] != "true" || u.GetAnnotations()[kube.AnnotationPopCreatedBy] != "sub-1" {
		t.Fatalf("annotations = %v", u.GetAnnotations())
	}
	tok, _, _ := unstructured.NestedString(u.Object, "spec", "pairing", "tokenSHA256")
	if tok != spec.Pairing.TokenSHA256 {
		t.Fatalf("stored tokenSHA256 = %q, want %q", tok, spec.Pairing.TokenSHA256)
	}
	raw, _ := json.Marshal(u.Object)
	if strings.Contains(string(raw), "AAAQE") {
		t.Fatalf("the stored CR carries the pairing token itself: %s", raw)
	}
	if _, found, _ := unstructured.NestedMap(u.Object, "status"); found {
		t.Fatalf("Create wrote a status: %s", raw)
	}

	if _, err := c.Create(ctx, kube.NewPop{Name: "assembly-2026", Spec: spec}); !errors.Is(err, kube.ErrPopExists) {
		t.Fatalf("duplicate Create = %v, want ErrPopExists", err)
	}
	for _, bad := range []string{"", "Assembly", "a_b", strings.Repeat("a", 64)} {
		if _, err := c.Create(ctx, kube.NewPop{Name: bad, Spec: spec}); !errors.Is(err, pops.ErrBadName) {
			t.Errorf("Create(%q) = %v, want ErrBadName", bad, err)
		}
	}
}

func TestPopClientGetAndListDecodeAndReportMissing(t *testing.T) {
	c, _ := newFakePopClient(t, storedPop("lan-a", "uid-a"), storedPop("lan-b", "uid-b"))
	ctx := context.Background()
	got, err := c.Get(ctx, "lan-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pop.Spec.DisplayName != "Assembly 2026" || got.Pop.Status.Link == nil || !got.Pop.Status.Link.Connected {
		t.Fatalf("Get decoded %+v", got.Pop)
	}
	if _, err := c.Get(ctx, "nope"); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrPopNotFound", err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d, %v; want 2", len(list), err)
	}
}

// Edit goes through spec, never status, and never the pairing: the relays own
// the status paths and a Re-pair is its own verb (docs/76 §9, item 9).
func TestPopClientPatchSpecDiffsTheSpecAndNeverTouchesPairingOrStatus(t *testing.T) {
	p := storedPop("lan-a", "uid-checked")
	end := metav1.NewTime(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	p.Spec.NotAfter = &end
	c, dc := newFakePopClient(t, p)
	var patch string
	dc.PrependReactor("patch", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa := action.(k8stesting.PatchActionImpl)
		if pa.GetSubresource() != "" {
			t.Errorf("PatchSpec patched subresource %q", pa.GetSubresource())
		}
		patch = string(pa.GetPatch())
		return false, nil, nil
	})
	before, after, err := c.PatchSpec(context.Background(), "lan-a", func(s *pops.PopSpec) error {
		s.Uplink = pops.Budget{MaxStreams: 2} // maxMbps back to the default
		s.NotAfter = nil
		s.DisplayName = "Assembly"
		s.Pairing = pops.Pairing{TokenSHA256: "attacker"} // must be ignored
		return nil
	})
	if err != nil {
		t.Fatalf("PatchSpec: %v", err)
	}
	if before.Pop.Spec.DisplayName != "Assembly 2026" || after.Pop.Spec.DisplayName != "Assembly" {
		t.Fatalf("before/after = %q/%q", before.Pop.Spec.DisplayName, after.Pop.Spec.DisplayName)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		t.Fatalf("patch %q: %v", patch, err)
	}
	if _, ok := body["status"]; ok {
		t.Fatalf("PatchSpec wrote status: %s", patch)
	}
	if !strings.Contains(patch, `"uid":"uid-checked"`) {
		t.Fatalf("patch %s does not carry the read UID", patch)
	}
	spec := body["spec"].(map[string]any)
	if _, ok := spec["pairing"]; ok {
		t.Fatalf("PatchSpec touched spec.pairing: %s", patch)
	}
	if v, ok := spec["notAfter"]; !ok || v != nil {
		t.Fatalf("a cleared notAfter must be a merge-patch null: %s", patch)
	}
	if up := spec["uplink"].(map[string]any); up["maxMbps"] != nil || up["maxStreams"] != float64(2) {
		t.Fatalf("uplink patch = %v, want maxMbps null and maxStreams 2", up)
	}

	u := rawPop(t, dc, "lan-a")
	if tok, _, _ := unstructured.NestedString(u.Object, "spec", "pairing", "tokenSHA256"); tok != strings.Repeat("a", 64) {
		t.Fatalf("stored pairing = %q, want it untouched", tok)
	}
	if lan, _, _ := unstructured.NestedString(u.Object, "status", "link", "lanAddr"); lan != "192.168.1.20" {
		t.Fatalf("status.link.lanAddr = %q, want the relays' value kept", lan)
	}

	refuse := errors.New("refused")
	if _, _, err := c.PatchSpec(context.Background(), "lan-a", func(*pops.PopSpec) error { return refuse }); !errors.Is(err, refuse) {
		t.Fatalf("an edit that refuses = %v", err)
	}
	if _, _, err := c.PatchSpec(context.Background(), "nope", func(*pops.PopSpec) error { return nil }); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("PatchSpec(missing) = %v", err)
	}
}

// Re-pair writes a new spec.pairing and nothing else (docs/76 item 9).
func TestPopClientRepairWritesOnlySpecPairing(t *testing.T) {
	c, dc := newFakePopClient(t, storedPop("lan-a", "uid-checked"))
	var patch string
	dc.PrependReactor("patch", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch = string(action.(k8stesting.PatchActionImpl).GetPatch())
		return false, nil, nil
	})
	issued := metav1.NewTime(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC))
	expires := metav1.NewTime(issued.Add(time.Hour))
	newHash := strings.Repeat("b", 64)
	obj, err := c.Repair(context.Background(), "lan-a", pops.Pairing{TokenSHA256: newHash, IssuedAt: &issued, ExpiresAt: &expires})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if obj.Pop.Spec.Pairing.TokenSHA256 != newHash {
		t.Fatalf("returned pairing = %+v", obj.Pop.Spec.Pairing)
	}
	var body struct {
		Metadata map[string]any             `json:"metadata"`
		Spec     map[string]json.RawMessage `json:"spec"`
		Status   any                        `json:"status"`
	}
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		t.Fatal(err)
	}
	if body.Metadata["uid"] != "uid-checked" || body.Status != nil || len(body.Spec) != 1 || body.Spec["pairing"] == nil {
		t.Fatalf("Repair patch = %s, want exactly metadata.uid and spec.pairing", patch)
	}
	u := rawPop(t, dc, "lan-a")
	if name, _, _ := unstructured.NestedString(u.Object, "spec", "displayName"); name != "Assembly 2026" {
		t.Fatalf("Repair changed displayName to %q", name)
	}
	// The relays' record of the old credential stays: it is simply no longer
	// current, which is what makes the old credential stop working.
	if cred, _, _ := unstructured.NestedString(u.Object, "status", "credential", "pairingTokenSHA256"); cred != strings.Repeat("a", 64) {
		t.Fatalf("Repair touched status.credential: %q", cred)
	}
	if obj.Pop.CredentialCurrent() {
		t.Fatal("after a Re-pair the old credential is still current")
	}

	revoked := storedPop("gone", "uid-r")
	revoked.Spec.Revoked = true
	c2, _ := newFakePopClient(t, revoked)
	if _, err := c2.Repair(context.Background(), "gone", pops.Pairing{TokenSHA256: newHash}); !errors.Is(err, kube.ErrPopRevoked) {
		t.Fatalf("Repair of a revoked PoP = %v, want ErrPopRevoked", err)
	}
	if _, err := c.Repair(context.Background(), "nope", pops.Pairing{TokenSHA256: newHash}); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("Repair(missing) = %v", err)
	}
}

func TestPopClientRevokeSetsSpecRevokedIdempotently(t *testing.T) {
	c, dc := newFakePopClient(t, storedPop("lan-a", "uid-a"))
	for i := 0; i < 2; i++ {
		obj, err := c.Revoke(context.Background(), "lan-a")
		if err != nil {
			t.Fatalf("Revoke #%d: %v", i+1, err)
		}
		if !obj.Pop.Spec.Revoked {
			t.Fatalf("Revoke #%d returned an unrevoked PoP", i+1)
		}
	}
	if rev, _, _ := unstructured.NestedBool(rawPop(t, dc, "lan-a").Object, "spec", "revoked"); !rev {
		t.Fatal("spec.revoked not stored")
	}
	if _, err := c.Revoke(context.Background(), "nope"); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("Revoke(missing) = %v", err)
	}
}

// A delete is bound to the object read by a UID precondition, as a room's is,
// and never by resourceVersion: the link-home pod rewrites status every few
// seconds.
func TestPopClientDeleteIsBoundToTheReadUID(t *testing.T) {
	c, dc := newFakePopClient(t, storedPop("lan-a", "uid-checked"))
	var got *metav1.Preconditions
	dc.PrependReactor("delete", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		got = action.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions
		return false, nil, nil
	})
	refuse := errors.New("refused")
	if _, err := c.Delete(context.Background(), "lan-a", func(kube.PopObject) error { return refuse }); !errors.Is(err, refuse) {
		t.Fatalf("Delete with a refusing check = %v", err)
	}
	if got != nil {
		t.Fatal("a refused delete reached the API server")
	}
	obj, err := c.Delete(context.Background(), "lan-a", nil)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if obj.Name != "lan-a" {
		t.Fatalf("Delete returned %+v", obj)
	}
	if got == nil || got.UID == nil || *got.UID != "uid-checked" || got.ResourceVersion != nil {
		t.Fatalf("delete preconditions = %+v, want the read UID only", got)
	}
	if _, err := c.Delete(context.Background(), "lan-a", nil); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("second Delete = %v, want ErrPopNotFound", err)
	}
}

// gawk-admin's status writes go to the status subresource and carry only the
// four tls paths it owns (docs/76 §9) — never tls.csr, which is the relays'.
func TestPopClientPatchTLSStatusWritesOnlyTheAdminOwnedPaths(t *testing.T) {
	c, dc := newFakePopClient(t, storedPop("lan-a", "uid-a"))
	var patch, sub string
	dc.PrependReactor("patch", "pops", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa := action.(k8stesting.PatchActionImpl)
		patch, sub = string(pa.GetPatch()), pa.GetSubresource()
		return false, nil, nil
	})
	notAfter := metav1.NewTime(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	err := c.PatchTLSStatus(context.Background(), "lan-a", pops.TLS{
		Host: "p-7k2m9x.pop.gawk.ioio.fi", CSR: "MUST-NOT-BE-WRITTEN",
		Certificate: "-----BEGIN CERTIFICATE-----", IssuedForCSR: strings.Repeat("d", 64), NotAfter: &notAfter,
	})
	if err != nil {
		t.Fatalf("PatchTLSStatus: %v", err)
	}
	if sub != "status" {
		t.Fatalf("subresource = %q, want status", sub)
	}
	var body map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		t.Fatalf("patch %s: %v", patch, err)
	}
	if len(body) != 1 || len(body["status"]) != 1 {
		t.Fatalf("patch %s, want status.tls only", patch)
	}
	tls := body["status"]["tls"]
	for _, k := range []string{"host", "certificate", "issuedForCSR", "notAfter"} {
		if _, ok := tls[k]; !ok {
			t.Errorf("patch lacks tls.%s: %s", k, patch)
		}
	}
	if _, ok := tls["csr"]; ok || strings.Contains(patch, "MUST-NOT") {
		t.Fatalf("PatchTLSStatus wrote tls.csr: %s", patch)
	}
	if err := c.PatchTLSStatus(context.Background(), "nope", pops.TLS{Host: "x"}); !errors.Is(err, kube.ErrPopNotFound) {
		t.Fatalf("PatchTLSStatus(missing) = %v", err)
	}
}

func TestPopOwnerReferenceNamesThePopByUID(t *testing.T) {
	ref, ok := kube.PopOwnerReference(*storedPop("lan-a", "uid-a"))
	if !ok {
		t.Fatal("no owner reference for a Pop with a UID")
	}
	if ref.APIVersion != pops.SchemeGroupVersion.String() || ref.Kind != pops.Kind || ref.Name != "lan-a" || ref.UID != "uid-a" {
		t.Fatalf("owner reference = %+v", ref)
	}
	if ref.BlockOwnerDeletion != nil {
		// Setting it needs update on the owner's finalizers, which the admin
		// Role does not grant.
		t.Fatalf("blockOwnerDeletion set: %+v", ref)
	}
	if _, ok := kube.PopOwnerReference(*storedPop("lan-b", "")); ok {
		t.Fatal("an owner reference for a Pop with no UID")
	}
}

// The wall (docs/76 item 33): a static room with spec.pop, its caps, no attach
// secret (vouched attaches only) and an ownerReference to the Pop, so deleting
// the Pop garbage-collects the room.
func TestRoomClientCreateStaticWritesAWallRoom(t *testing.T) {
	c, cs, dc := newFakeRoomClientWithDynamic(t)
	ref, _ := kube.PopOwnerReference(*storedPop("assembly-2026", "uid-pop"))
	secret, err := c.CreateStatic(context.Background(), kube.StaticRoom{
		Code: "assembly-2026", DisplayName: "Assembly 2026", MaxBroadcasts: 40,
		Pop: "assembly-2026", MaxParticipants: 300, Owner: &ref,
	})
	if err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	if secret != "" {
		t.Fatalf("a wall room got an attach secret %q", secret)
	}
	if list, _ := cs.CoreV1().Secrets(testNamespace).List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("a wall room wrote %d Secrets", len(list.Items))
	}
	u, err := dc.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(context.Background(), "assembly-2026", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var room rooms.Room
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &room); err != nil {
		t.Fatal(err)
	}
	if room.Spec.Pop != "assembly-2026" || room.Spec.MaxParticipants != 300 || room.Spec.MaxBroadcasts != 40 || room.Spec.AttachSecretRef != nil {
		t.Fatalf("wall spec = %+v", room.Spec)
	}
	if len(room.OwnerReferences) != 1 || room.OwnerReferences[0].UID != "uid-pop" || room.OwnerReferences[0].Kind != pops.Kind {
		t.Fatalf("owner references = %+v", room.OwnerReferences)
	}

	if _, err := c.CreateStatic(context.Background(), kube.StaticRoom{Code: "other-wall", Pop: "assembly-2026", WithAttachSecret: true}); !errors.Is(err, kube.ErrWallAttachSecret) {
		t.Fatalf("a wall with an attach secret = %v, want ErrWallAttachSecret", err)
	}
}

// The named tier's reconciler runs inside OnLeading, whose context is
// cancelled when leadership is lost: Run must return then, or a demoted
// replica would keep issuing.
func TestPopReconcilerRunReturnsWhenItsContextEnds(t *testing.T) {
	c, _ := newFakePopClient(t)
	r := kube.NewPopReconciler(kube.PopReconcilerOptions{Pops: c, Namespace: testNamespace})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
