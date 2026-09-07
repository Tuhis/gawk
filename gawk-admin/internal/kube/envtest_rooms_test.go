package kube_test

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// TestEnvtestRoomCRDAcceptsTheRoomClientsPayload is RM7's schema check: the
// RoomClient's real static-room payload — annotation, displayCode, the
// attachSecretRef, the merge patch a rotation applies — against the relay
// chart's Room CRD schema on a real API server, the gap the Ban twin in
// envtest_test.go closed for bans. A payload the schema pruned or rejected
// would make every portal-created room silently un-joinable while the portal
// reported success.
//
// Skips if the relay chart's crd-room.yaml is absent (a checkout predating
// RM3), and without KUBEBUILDER_ASSETS like every envtest.
func TestEnvtestRoomCRDAcceptsTheRoomClientsPayload(t *testing.T) {
	if _, err := os.Stat(roomCRDTemplate); err != nil {
		t.Skipf("the relay chart's Room CRD template (%s) is absent; skipping", roomCRDTemplate)
	}
	cfg, ns := startEnv(t, roomCRDTemplate)
	ctx := t.Context()

	rc, err := kube.NewRoomClient(cfg, ns)
	if err != nil {
		t.Fatalf("NewRoomClient: %v", err)
	}
	secret, err := rc.CreateStatic(ctx, kube.StaticRoom{
		Code: "TuhisRoom", DisplayName: "Tuhis' room", MaxBroadcasts: 4, WithAttachSecret: true,
	})
	if err != nil {
		t.Fatalf("the real API server rejected the room client's create payload: %v", err)
	}
	if secret == "" {
		t.Fatal("no attach secret returned")
	}
	// The rotation path patches the CR: the merge patch must survive the
	// schema too, and the annotation must not be pruned as unknown metadata.
	if _, err := rc.RotateSecret(ctx, "tuhisroom"); err != nil {
		t.Fatalf("the rotation path was rejected: %v", err)
	}
	obj, err := rc.Get(ctx, "tuhisroom")
	if err != nil || obj.Err != nil {
		t.Fatalf("Get: %v / %v", err, obj.Err)
	}
	if !obj.Managed {
		t.Fatal("the managed annotation did not round-trip")
	}
	if obj.Room.Spec.DisplayCode != "TuhisRoom" || obj.Room.Spec.AttachSecretRef == nil || obj.Room.Spec.MaxBroadcasts != 4 {
		t.Fatalf("the schema pruned part of the spec: %+v", obj.Room.Spec)
	}
	// A secret-less room, then an explicit delete of each — the Secret goes
	// with the managed one.
	if _, err := rc.CreateStatic(ctx, kube.StaticRoom{Code: "open"}); err != nil {
		t.Fatalf("secret-less create rejected: %v", err)
	}
	for _, name := range []string{"tuhisroom", "open"} {
		if _, err := rc.DeleteExisting(ctx, name); err != nil {
			t.Fatalf("DeleteExisting(%s): %v", name, err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, kube.RoomSecretName("tuhisroom"), metav1.GetOptions{}); err == nil {
		t.Fatal("the portal's attach Secret outlived its room")
	}

	// And the schema is ON: an unknown spec.kind is enum-rejected.
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": rooms.SchemeGroupVersion.String(),
		"kind":       rooms.Kind,
		"metadata":   map[string]any{"name": "badkind"},
		"spec":       map[string]any{"kind": "ephemeral"},
	}}
	if _, err := dc.Resource(rooms.GroupVersionResource).Namespace(ns).Create(ctx, bad, metav1.CreateOptions{}); err == nil {
		t.Fatal("the schema accepted an unknown spec.kind — OpenAPI validation is not in force")
	}
}
