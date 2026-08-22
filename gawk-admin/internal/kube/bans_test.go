package kube_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const testNamespace = "production"

// newFakeCRClient builds a CRClient over client-go's dynamic fake, which is a
// real client surface (create/get/update/delete/list, resourceVersions and
// all) rather than a hand-written stand-in.
func newFakeCRClient(t *testing.T, objs ...runtime.Object) (*kube.CRClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := moderation.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	// NewSimpleDynamicClient (not …WithCustomListKinds) because it derives an
	// unstructured-backed scheme from the typed one: the tracker must hold
	// Bans as unstructured, which is what a real dynamic client sees.
	// moderation.AddToScheme registers BanList too, so the GVR→list-kind
	// guess resolves to "bans" with no extra mapping.
	dc := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
	return kube.NewCRClientFor(dc, testNamespace), dc
}

func idRecord(v string) moderation.Record {
	return moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: v}}
}

// A re-ban must UPDATE the existing CR, never accumulate a second object: the
// deterministic name from moderation.CRName is what guarantees it (§4.2).
func TestCRClientUpsertIsIdempotentAndUpdatesInPlace(t *testing.T) {
	c, _ := newFakeCRClient(t)
	ctx := context.Background()

	rec := idRecord("abc234")
	rec.Reason = "first"
	rec.CreatedBy = "op@example.com"
	if err := c.Upsert(ctx, rec, "ban-1"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Same target, differently cased, different spec.
	rec2 := idRecord("ABC234")
	rec2.Reason = "second"
	rec2.CreatedBy = "op2@example.com"
	exp := time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)
	rec2.ExpiresAt = &exp
	if err := c.Upsert(ctx, rec2, "ban-2"); err != nil {
		t.Fatalf("Upsert (re-ban): %v", err)
	}

	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("re-banning the same target produced %d CRs, want 1: %+v", len(list), list)
	}
	got := list[0]
	if got.Name != "ban-id-abc234" {
		t.Fatalf("CR name = %q, want the deterministic moderation.CRName", got.Name)
	}
	if got.BanID != "ban-2" {
		t.Fatalf("ban-id annotation = %q, want the newest row's ID", got.BanID)
	}
	if got.Record.Reason != "second" || got.Record.CreatedBy != "op2@example.com" {
		t.Fatalf("spec was not updated: %+v", got.Record)
	}
	if got.Record.ExpiresAt == nil || !got.Record.ExpiresAt.Equal(exp) {
		t.Fatalf("expiresAt = %v, want %v", got.Record.ExpiresAt, exp)
	}
	if got.Record.Target.Value != "ABC234" {
		t.Fatalf("target was not normalized into the CR: %q", got.Record.Target.Value)
	}
}

func TestCRClientIPTargetRoundTrip(t *testing.T) {
	c, _ := newFakeCRClient(t)
	ctx := context.Background()

	// A bare address must land as its canonical /32 — the same value the relay
	// evaluates and the same value the store wrote.
	rec := moderation.Record{Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}}
	if err := c.Upsert(ctx, rec, "ban-ip"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d objects (err=%v)", len(list), err)
	}
	if list[0].Record.Target.Value != "203.0.113.7/32" {
		t.Fatalf("IP target = %q, want the canonical prefix", list[0].Record.Target.Value)
	}
	want, err := moderation.CRName(list[0].Record.Target)
	if err != nil {
		t.Fatalf("CRName: %v", err)
	}
	if list[0].Name != want {
		t.Fatalf("CR name = %q, want %q", list[0].Name, want)
	}
}

// Deleting an object that is already gone is success: the caller's intent is
// satisfied, and a janitor that errored on it would retry forever.
func TestCRClientDeleteIsIdempotent(t *testing.T) {
	c, _ := newFakeCRClient(t)
	ctx := context.Background()
	if err := c.Upsert(ctx, idRecord("bbb234"), "b"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := c.Delete(ctx, "ban-id-bbb234"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Delete(ctx, "ban-id-bbb234"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("List after delete = %d (err=%v)", len(list), err)
	}
}

// An operator-applied CR carries no ban-id annotation. That absence is the
// whole signal the reconciler adopts on, so List must report it faithfully —
// including for a CR with no labels at all (§4.2: no label selector).
func TestCRClientReportsUnannotatedOperatorBan(t *testing.T) {
	ban := &moderation.Ban{
		TypeMeta:   metav1.TypeMeta{APIVersion: moderation.SchemeGroupVersion.String(), Kind: moderation.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "ban-id-ccc234", Namespace: testNamespace},
		Spec: moderation.BanSpec{
			Target:    moderation.Target{Type: moderation.TargetBroadcastID, Value: "CCC234"},
			Reason:    "emergency",
			CreatedBy: "someone@example.com",
		},
	}
	c, _ := newFakeCRClient(t, ban)
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].BanID != "" {
		t.Fatalf("operator-applied CR reported as %+v", list)
	}
	if list[0].Record.Reason != "emergency" {
		t.Fatalf("spec lost in conversion: %+v", list[0].Record)
	}
}

// A CR whose target cannot be normalized is REPORTED, not enforced and not
// deleted: rejecting is safe, guessing is not.
func TestCRClientSurfacesUnparseableTarget(t *testing.T) {
	ban := &moderation.Ban{
		TypeMeta:   metav1.TypeMeta{APIVersion: moderation.SchemeGroupVersion.String(), Kind: moderation.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "ban-broken", Namespace: testNamespace},
		Spec:       moderation.BanSpec{Target: moderation.Target{Type: moderation.TargetIP, Value: "not-an-address"}},
	}
	c, _ := newFakeCRClient(t, ban)
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Err == nil {
		t.Fatalf("unparseable CR reported as %+v", list)
	}
}
