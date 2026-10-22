package kube_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// TestEnvtestPopClientAgainstARealAPIServer is PP6's portal half on a real
// API server with the chart's CRDs: the PopClient's writes are accepted by
// the schema, a spec edit and a Re-pair never clobber what the relays wrote
// in status, the tls patch lands through the status subresource without
// touching tls.csr, and a wall room with spec.pop, spec.maxParticipants and
// an ownerReference to the Pop is accepted with every field kept.
func TestEnvtestPopClientAgainstARealAPIServer(t *testing.T) {
	cfg, ns := startEnv(t, popCRDTemplate, roomCRDTemplate)
	ctx := t.Context()
	pc, err := kube.NewPopClient(cfg, ns)
	if err != nil {
		t.Fatal(err)
	}

	issued := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	expires := metav1.NewTime(issued.Add(72 * time.Hour))
	firstHash := pops.TokenSHA256("AAAQE-AYEAU-DAOCA-JBIFQ-YDIOB-4")
	created, err := pc.Create(ctx, kube.NewPop{Name: "assembly-2026", CreatedBy: "sub-1", Spec: pops.PopSpec{
		DisplayName: "Assembly 2026",
		Pairing:     pops.Pairing{TokenSHA256: firstHash, IssuedAt: &issued, ExpiresAt: &expires},
		Uplink:      pops.Budget{MaxMbps: 25, MaxStreams: 3},
		Wall:        &pops.Wall{RoomCode: "assembly-2026"},
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Pop.UID == "" {
		t.Fatal("the API server returned no UID")
	}

	// The relays' half: a redeemed credential and a live link.
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	relayStatus, _ := json.Marshal(map[string]any{"status": map[string]any{
		"credential": map[string]any{"sha256": strings.Repeat("c", 64), "pairingTokenSHA256": firstHash},
		"link":       map[string]any{"connected": true, "lanAddr": "192.168.1.20"},
		"tls":        map[string]any{"csr": "RELAY-CSR"},
	}})
	if _, err := dc.Resource(pops.GroupVersionResource).Namespace(ns).Patch(ctx, "assembly-2026",
		types.MergePatchType, relayStatus, metav1.PatchOptions{}, "status"); err != nil {
		t.Fatalf("relay status patch: %v", err)
	}

	_, after, err := pc.PatchSpec(ctx, "assembly-2026", func(s *pops.PopSpec) error {
		s.Uplink = pops.Budget{MaxStreams: 2}
		s.DisplayName = "Assembly"
		return nil
	})
	if err != nil {
		t.Fatalf("PatchSpec: %v", err)
	}
	if after.Pop.Spec.Uplink != (pops.Budget{MaxStreams: 2}) || after.Pop.Spec.DisplayName != "Assembly" {
		t.Fatalf("after the edit spec = %+v", after.Pop.Spec)
	}
	if !after.Pop.CredentialCurrent() || after.Pop.Status.Link == nil || after.Pop.Status.Link.LanAddr != "192.168.1.20" {
		t.Fatalf("a spec edit clobbered the relays' status: %+v", after.Pop.Status)
	}

	notAfter := metav1.NewTime(time.Now().UTC().Add(6 * 24 * time.Hour).Truncate(time.Second))
	if err := pc.PatchTLSStatus(ctx, "assembly-2026", pops.TLS{Host: "p-7k2m9x.pop.example", Certificate: "CERT",
		IssuedForCSR: strings.Repeat("d", 64), NotAfter: &notAfter, CSR: "PORTAL-MUST-NOT-WRITE"}); err != nil {
		t.Fatalf("PatchTLSStatus: %v", err)
	}

	newHash := pops.TokenSHA256("BBBQE-AYEAU-DAOCA-JBIFQ-YDIOB-4")
	repaired, err := pc.Repair(ctx, "assembly-2026", pops.Pairing{TokenSHA256: newHash, IssuedAt: &issued, ExpiresAt: &expires})
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if repaired.Pop.CredentialCurrent() {
		t.Fatal("the old credential is still current after a Re-pair")
	}
	got, err := pc.Get(ctx, "assembly-2026")
	if err != nil {
		t.Fatal(err)
	}
	tls := got.Pop.Status.TLS
	if tls == nil || tls.CSR != "RELAY-CSR" || tls.Host != "p-7k2m9x.pop.example" || tls.Certificate != "CERT" || tls.NotAfter == nil {
		t.Fatalf("status.tls = %+v, want the relay's csr and the portal's four paths", tls)
	}
	if got.Pop.Status.Credential == nil || got.Pop.Status.Credential.PairingTokenSHA256 != firstHash {
		t.Fatalf("Re-pair touched status.credential: %+v", got.Pop.Status.Credential)
	}

	// The wall.
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rc := kube.NewRoomClientFor(dc, cs, ns)
	owner, ok := kube.PopOwnerReference(got.Pop)
	if !ok {
		t.Fatal("no owner reference")
	}
	if _, err := rc.CreateStatic(ctx, kube.StaticRoom{Code: "assembly-2026", DisplayName: "Assembly 2026",
		MaxBroadcasts: 40, Pop: "assembly-2026", MaxParticipants: 300, Owner: &owner}); err != nil {
		t.Fatalf("the real API server rejected the wall room: %v", err)
	}
	wall, err := rc.Get(ctx, "assembly-2026")
	if err != nil {
		t.Fatal(err)
	}
	if wall.Room.Spec.Pop != "assembly-2026" || wall.Room.Spec.MaxParticipants != 300 || wall.Room.Spec.MaxBroadcasts != 40 {
		t.Fatalf("the schema pruned the wall's spec: %+v", wall.Room.Spec)
	}
	if len(wall.Room.OwnerReferences) != 1 || wall.Room.OwnerReferences[0].UID != got.Pop.UID {
		t.Fatalf("owner references = %+v", wall.Room.OwnerReferences)
	}
	if wall.Room.Spec.Kind != rooms.KindStatic || wall.Room.Spec.AttachSecretRef != nil {
		t.Fatalf("wall = %+v, want a static room with no attach secret", wall.Room.Spec)
	}

	if _, err := pc.Delete(ctx, "assembly-2026", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
