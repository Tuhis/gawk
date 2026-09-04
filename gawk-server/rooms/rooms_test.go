package rooms

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNormalizeCode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"TuhisRoom", "tuhisroom", true},
		{"  tuhis-room ", "tuhis-room", true},
		{"5UP4XW", "5up4xw", true},
		{"abc", "abc", true},
		{"ab", "", false},
		{strings.Repeat("a", 33), "", false},
		{strings.Repeat("a", 32), strings.Repeat("a", 32), true},
		{"-abc", "", false},
		{"abc-", "", false},
		{"a b c", "", false},
		{"ab_c", "", false},
		{"tuhis.room", "", false},
		{"", "", false},
	} {
		got, err := NormalizeCode(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("NormalizeCode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidCode) {
			t.Errorf("NormalizeCode(%q) = %q, %v; want ErrInvalidCode", tc.in, got, err)
		}
	}
}

func TestIsDynamicShape(t *testing.T) {
	if !IsDynamicShape("5up4xw") || !IsDynamicShape("5UP4XW") {
		t.Error("a six-character broadcast-alphabet code is dynamic-shaped")
	}
	// 0, O, 1, I, L are outside the broadcast alphabet: a static slug may
	// contain them, a typed code never can.
	for _, s := range []string{"tuhisroom", "abc", "0oil11", "abcdefg"} {
		if IsDynamicShape(s) {
			t.Errorf("IsDynamicShape(%q) = true", s)
		}
	}
}

func TestDisplayCode(t *testing.T) {
	if got := DisplayCode(&Room{ObjectMeta: metav1.ObjectMeta{Name: "5up4xw"}, Spec: RoomSpec{Kind: KindDynamic}}); got != "5UP4XW" {
		t.Errorf("dynamic display = %q", got)
	}
	if got := DisplayCode(&Room{ObjectMeta: metav1.ObjectMeta{Name: "tuhisroom"}, Spec: RoomSpec{Kind: KindStatic, DisplayCode: "TuhisRoom"}}); got != "TuhisRoom" {
		t.Errorf("static display = %q", got)
	}
	if got := DisplayCode(&Room{ObjectMeta: metav1.ObjectMeta{Name: "tuhisroom"}, Spec: RoomSpec{Kind: KindStatic}}); got != "tuhisroom" {
		t.Errorf("static without displayCode = %q", got)
	}
	if DisplayCode(nil) != "" {
		t.Error("nil room display")
	}
}

func TestFingerprint(t *testing.T) {
	fp := Fingerprint([]byte("token"))
	if len(fp) != FingerprintSize*2 {
		t.Fatalf("fingerprint %q is %d hex chars, want %d", fp, len(fp), FingerprintSize*2)
	}
	if Fingerprint([]byte("other")) == fp {
		t.Error("fingerprints collide")
	}
	if strings.Contains(fp, "token") {
		t.Error("fingerprint leaks the token")
	}
}

// The YAML in docs/44 §4.3, as JSON, must round-trip into Room.
func TestRoomUnmarshalsTheDocumentedShape(t *testing.T) {
	const doc = `{
	  "apiVersion": "gawk.ioio.fi/v1alpha1",
	  "kind": "Room",
	  "metadata": {"name": "tuhisroom", "namespace": "production"},
	  "spec": {
	    "kind": "static",
	    "displayCode": "TuhisRoom",
	    "displayName": "Tuhis' room",
	    "attachSecretRef": {"name": "room-tuhisroom", "key": "attachSecret"},
	    "maxBroadcasts": 4,
	    "integrations": {}
	  },
	  "status": {
	    "creatorTokenFingerprint": "",
	    "createdAt": "2026-09-03T18:00:00Z",
	    "attachments": [{"broadcastID": "5UP4XW", "label": "tuhis", "attachedAt": "2026-09-03T18:01:00Z"}],
	    "lease": {"holder": "gawk-server-7c9f", "addr": "10.42.0.17:4433", "generation": 3, "renewedAt": "2026-09-03T18:05:10Z"},
	    "emptySince": null
	  }
	}`
	var r Room
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Kind != "Room" || r.Name != "tuhisroom" || r.Namespace != "production" {
		t.Errorf("meta = %+v / %+v", r.TypeMeta, r.ObjectMeta)
	}
	if r.Spec.Kind != KindStatic || r.Spec.DisplayCode != "TuhisRoom" || r.Spec.DisplayName != "Tuhis' room" {
		t.Errorf("spec = %+v", r.Spec)
	}
	if r.Spec.AttachSecretRef == nil || r.Spec.AttachSecretRef.Name != "room-tuhisroom" || r.Spec.AttachSecretRef.Key != "attachSecret" {
		t.Errorf("attachSecretRef = %+v", r.Spec.AttachSecretRef)
	}
	if r.Spec.MaxBroadcasts != 4 {
		t.Errorf("maxBroadcasts = %d", r.Spec.MaxBroadcasts)
	}
	if len(r.Status.Attachments) != 1 || r.Status.Attachments[0].BroadcastID != "5UP4XW" || r.Status.Attachments[0].Label != "tuhis" {
		t.Errorf("attachments = %+v", r.Status.Attachments)
	}
	if r.Status.Lease == nil || r.Status.Lease.Holder != "gawk-server-7c9f" || r.Status.Lease.Generation != 3 || r.Status.Lease.Addr != "10.42.0.17:4433" {
		t.Errorf("lease = %+v", r.Status.Lease)
	}
	if r.Status.EmptySince != nil {
		t.Errorf("emptySince = %v, want nil", r.Status.EmptySince)
	}
	if r.Status.CreatedAt == nil || !r.Status.CreatedAt.Time.Equal(time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("createdAt = %v", r.Status.CreatedAt)
	}
	// Re-marshal omits the empties so a status write never carries junk.
	out, err := json.Marshal(r.Status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "emptySince") || strings.Contains(string(out), "creatorTokenFingerprint") {
		t.Errorf("status marshals empties: %s", out)
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
	if _, ok := obj.(*Room); !ok {
		t.Fatalf("scheme.New(%v) = %T, want *Room", gvk, obj)
	}
	if _, err := s.New(gvk.GroupVersion().WithKind(ListKind)); err != nil {
		t.Fatalf("scheme.New(%v): %v", ListKind, err)
	}
	if GroupVersionResource.Resource != "rooms" || GroupVersionResource.Group != GroupName {
		t.Errorf("GroupVersionResource = %v", GroupVersionResource)
	}
}

// Hand-written deepcopy: mutating the copy must not touch the original
// through any pointer or slice field.
func TestDeepCopyDoesNotAlias(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC))
	orig := &Room{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gawk.ioio.fi/v1alpha1", Kind: "Room"},
		ObjectMeta: metav1.ObjectMeta{Name: "tuhisroom", Labels: map[string]string{"a": "b"}},
		Spec: RoomSpec{
			Kind:            KindStatic,
			AttachSecretRef: &SecretKeyRef{Name: "s", Key: "k"},
			Integrations:    Integrations{Mumble: &MumbleIntegration{Server: "m"}},
		},
		Status: RoomStatus{
			CreatedAt:   now.DeepCopy(),
			Attachments: []Attachment{{BroadcastID: "ABCDEF", AttachedAt: now.DeepCopy()}},
			Lease:       &Lease{Holder: "h", Generation: 1, RenewedAt: now.DeepCopy()},
			EmptySince:  now.DeepCopy(),
		},
	}
	cp, ok := orig.DeepCopyObject().(*Room)
	if !ok {
		t.Fatal("DeepCopyObject did not return a *Room")
	}
	cp.Labels["a"] = "mutated"
	cp.Spec.AttachSecretRef.Name = "mutated"
	cp.Spec.Integrations.Mumble.Server = "mutated"
	cp.Status.CreatedAt.Time = cp.Status.CreatedAt.Add(time.Hour)
	cp.Status.Attachments[0].BroadcastID = "ZZZZZZ"
	cp.Status.Attachments[0].AttachedAt.Time = cp.Status.Attachments[0].AttachedAt.Add(time.Hour)
	cp.Status.Lease.Holder = "mutated"
	cp.Status.Lease.RenewedAt.Time = cp.Status.Lease.RenewedAt.Add(time.Hour)
	cp.Status.EmptySince.Time = cp.Status.EmptySince.Add(time.Hour)

	if orig.Labels["a"] != "b" {
		t.Error("labels aliased")
	}
	if orig.Spec.AttachSecretRef.Name != "s" {
		t.Error("attachSecretRef aliased")
	}
	if orig.Spec.Integrations.Mumble.Server != "m" {
		t.Error("integrations aliased")
	}
	if !orig.Status.CreatedAt.Time.Equal(now.Time) {
		t.Error("createdAt aliased")
	}
	if orig.Status.Attachments[0].BroadcastID != "ABCDEF" || !orig.Status.Attachments[0].AttachedAt.Time.Equal(now.Time) {
		t.Error("attachments aliased")
	}
	if orig.Status.Lease.Holder != "h" || !orig.Status.Lease.RenewedAt.Time.Equal(now.Time) {
		t.Error("lease aliased")
	}
	if !orig.Status.EmptySince.Time.Equal(now.Time) {
		t.Error("emptySince aliased")
	}

	list := &RoomList{Items: []Room{*orig}}
	lcp, ok := list.DeepCopyObject().(*RoomList)
	if !ok {
		t.Fatal("RoomList.DeepCopyObject did not return a *RoomList")
	}
	lcp.Items[0].Spec.DisplayName = "mutated"
	if list.Items[0].Spec.DisplayName != "" {
		t.Error("RoomList items aliased")
	}
	var nilRoom *Room
	if nilRoom.DeepCopy() != nil {
		t.Error("nil Room DeepCopy is not nil")
	}
	var nilList *RoomList
	if nilList.DeepCopy() != nil {
		t.Error("nil RoomList DeepCopy is not nil")
	}
}
