package moderation

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The YAML in docs/42 §4.2, verbatim, must round-trip into BanSpec.
func TestBanUnmarshalsTheDocumentedShape(t *testing.T) {
	const doc = `{
	  "apiVersion": "gawk.ioio.fi/v1alpha1",
	  "kind": "Ban",
	  "metadata": {"name": "ban-id-abc23z", "namespace": "production"},
	  "spec": {
	    "target": {"type": "broadcastId", "value": "ABC23Z"},
	    "expiresAt": "2026-08-20T18:00:00Z",
	    "reason": "operator text",
	    "createdBy": "juho@example.com"
	  }
	}`
	var b Ban
	if err := json.Unmarshal([]byte(doc), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.APIVersion != "gawk.ioio.fi/v1alpha1" || b.Kind != "Ban" {
		t.Errorf("TypeMeta = %+v", b.TypeMeta)
	}
	if b.Name != "ban-id-abc23z" || b.Namespace != "production" {
		t.Errorf("ObjectMeta = %q/%q", b.Namespace, b.Name)
	}
	if b.Spec.Target.Type != TargetBroadcastID || b.Spec.Target.Value != "ABC23Z" {
		t.Errorf("target = %+v", b.Spec.Target)
	}
	if b.Spec.ExpiresAt == nil || !b.Spec.ExpiresAt.Time.Equal(time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("expiresAt = %v", b.Spec.ExpiresAt)
	}
	if b.Spec.Reason != "operator text" || b.Spec.CreatedBy != "juho@example.com" {
		t.Errorf("reason/createdBy = %q/%q", b.Spec.Reason, b.Spec.CreatedBy)
	}

	rec, err := RecordFromBan(&b)
	if err != nil {
		t.Fatalf("RecordFromBan: %v", err)
	}
	if rec.Target.Value != "ABC23Z" || rec.ExpiresAt == nil || rec.Reason != "operator text" {
		t.Errorf("record = %+v", rec)
	}
	// An absent expiresAt is permanent.
	b.Spec.ExpiresAt = nil
	rec, err = RecordFromBan(&b)
	if err != nil {
		t.Fatalf("RecordFromBan: %v", err)
	}
	if rec.ExpiresAt != nil {
		t.Errorf("absent expiresAt became %v, want permanent (nil)", rec.ExpiresAt)
	}
	if !rec.Active(time.Now().Add(100 * 365 * 24 * time.Hour)) {
		t.Error("a permanent ban expired")
	}
}

func TestRecordFromBanNormalizesAndRejects(t *testing.T) {
	b := &Ban{Spec: BanSpec{Target: Target{Type: TargetIP, Value: "203.0.113.7"}}}
	rec, err := RecordFromBan(b)
	if err != nil {
		t.Fatalf("RecordFromBan: %v", err)
	}
	if rec.Target.Value != "203.0.113.7/32" {
		t.Errorf("value = %q, want the canonical /32", rec.Target.Value)
	}
	if _, err := RecordFromBan(&Ban{Spec: BanSpec{Target: Target{Type: "nope", Value: "x"}}}); err == nil {
		t.Error("an unknown target type was accepted")
	}
	if _, err := RecordFromBan(nil); err == nil {
		t.Error("a nil Ban was accepted")
	}
}

func TestSchemeRegistration(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	gvk := schema.GroupVersionKind{Group: GroupName, Version: Version, Kind: Kind}
	obj, err := s.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v): %v", gvk, err)
	}
	if _, ok := obj.(*Ban); !ok {
		t.Fatalf("scheme.New(%v) = %T, want *Ban", gvk, obj)
	}
	if _, err := s.New(gvk.GroupVersion().WithKind(ListKind)); err != nil {
		t.Fatalf("scheme.New(%v): %v", ListKind, err)
	}
	if GroupVersionResource.Resource != "bans" || GroupVersionResource.Group != GroupName {
		t.Errorf("GroupVersionResource = %v", GroupVersionResource)
	}
}

// The deepcopy methods are hand-written (no code generator in this repo), so
// this test is what keeps them honest: mutating the copy must not touch the
// original through any pointer or slice field.
func TestDeepCopyDoesNotAlias(t *testing.T) {
	exp := metav1.NewTime(time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC))
	orig := &Ban{
		TypeMeta: metav1.TypeMeta{APIVersion: "gawk.ioio.fi/v1alpha1", Kind: "Ban"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ban-id-abc23z",
			Namespace: "production",
			Labels:    map[string]string{"a": "b"},
		},
		Spec: BanSpec{
			Target:    Target{Type: TargetBroadcastID, Value: "ABC23Z"},
			ExpiresAt: &exp,
			Reason:    "r",
			CreatedBy: "c",
		},
	}
	cp, ok := orig.DeepCopyObject().(*Ban)
	if !ok {
		t.Fatal("DeepCopyObject did not return a *Ban")
	}
	cp.Spec.ExpiresAt.Time = cp.Spec.ExpiresAt.Time.Add(time.Hour)
	cp.Spec.Target.Value = "ZZZ23Z"
	cp.Labels["a"] = "mutated"
	cp.Name = "other"

	if !orig.Spec.ExpiresAt.Time.Equal(exp.Time) {
		t.Error("expiresAt aliased between copies")
	}
	if orig.Spec.Target.Value != "ABC23Z" {
		t.Error("target aliased between copies")
	}
	if orig.Labels["a"] != "b" {
		t.Error("labels aliased between copies")
	}
	if orig.Name != "ban-id-abc23z" {
		t.Error("name aliased between copies")
	}

	list := &BanList{Items: []Ban{*orig}}
	lcp, ok := list.DeepCopyObject().(*BanList)
	if !ok {
		t.Fatal("BanList.DeepCopyObject did not return a *BanList")
	}
	lcp.Items[0].Spec.Reason = "mutated"
	if list.Items[0].Spec.Reason != "r" {
		t.Error("BanList items aliased between copies")
	}

	var nilBan *Ban
	if nilBan.DeepCopy() != nil {
		t.Error("nil Ban DeepCopy is not nil")
	}
	var nilList *BanList
	if nilList.DeepCopy() != nil {
		t.Error("nil BanList DeepCopy is not nil")
	}
}
