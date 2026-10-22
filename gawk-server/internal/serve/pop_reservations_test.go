package serve

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// R74 PP8 (docs/76 item 14): a home mint never draws an ID a PoP has
// reserved. The check chains onto whatever IDReserved rooms installed, and
// the reservation set it reads is late-bound (cluster mode builds it with
// the coordinator, after the hub).
func TestPopIDReservedChainsOntoThePreviousCheck(t *testing.T) {
	var res popsrv.Reservations
	prev := func(id string) bool { return id == "ROOMAA" }
	check := popIDReserved(prev, func() popsrv.Reservations { return res })

	if check("K7XQ2M") || !check("ROOMAA") {
		t.Fatal("before the set exists only the previous check may answer")
	}
	res = popsrv.NewMemReservations()
	if got, err := res.Reserve(context.Background(), "K7XQ2M", "lan-party", 0); err != nil || got != popsrv.ReserveOK {
		t.Fatalf("Reserve = %q, %v", got, err)
	}
	if !check("K7XQ2M") || !check("ROOMAA") || check("ABC234") {
		t.Errorf("chained check: K7XQ2M=%v ROOMAA=%v ABC234=%v, want true true false",
			check("K7XQ2M"), check("ROOMAA"), check("ABC234"))
	}
	if !popIDReserved(nil, func() popsrv.Reservations { return res })("K7XQ2M") {
		t.Error("with no previous check the reservation still answers")
	}
}

// The hub's mint actually consults it: with every draw reserved, the mint
// gives up rather than handing out a PoP's ID.
func TestHubMintSkipsReservedIDs(t *testing.T) {
	everything := reserveAll{}
	r := hub.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), hub.Options{
		IDReserved: popIDReserved(nil, func() popsrv.Reservations { return everything }),
	})
	if id, _, err := r.StartPublish(""); err == nil {
		t.Fatalf("minted %s with every ID reserved", id)
	}
}

// reserveAll owns every ID.
type reserveAll struct{ popsrv.Reservations }

func (reserveAll) Owner(string) (string, bool) { return "lan-party", true }

// popReservationsFor picks the set a home runs: none with -pops off, the
// in-memory one single-pod. Cluster mode's is built with the coordinator.
func TestPopReservationsFor(t *testing.T) {
	if res := popReservationsFor(config.Config{}, nil); res != nil {
		t.Errorf("-pops off: %T, want nil", res)
	}
	mem := popReservationsFor(config.Config{Pops: true}, nil)
	if reflect.TypeOf(mem) != reflect.TypeOf(popsrv.NewMemReservations()) {
		t.Errorf("single-pod -pops: %T, want the in-memory set", mem)
	}
	if res := popReservationsFor(config.Config{Pops: true, ClusterMode: true}, nil); res != nil {
		t.Errorf("cluster mode with no coordinator yet: %T, want nil", res)
	}
	coord, err := cluster.New(cluster.Options{
		Client: fake.NewClientset(), Namespace: "gawk", PodName: "pod-a", AdvertiseAddr: "pod-a:4433",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	res := popReservationsFor(config.Config{Pops: true, ClusterMode: true}, coord)
	if reflect.TypeOf(res) != reflect.TypeOf(popsrv.NewClusterReservations(coord)) {
		t.Errorf("cluster -pops: %T, want the cluster set", res)
	}
	if res := popReservationsFor(config.Config{ClusterMode: true}, coord); res != nil {
		t.Errorf("cluster mode, -pops off: %T, want nil", res)
	}
}

// Every coordinator hook Run hands over reaches cluster.Options — the
// registryOptions rule for the coordinator: OnOriginClaimed is what wakes
// a pull waiting on a PoP reservation (docs/76 item 17).
func TestClusterOptionsCarryEveryHook(t *testing.T) {
	var deleted, lost, claimed string
	hooks := coordinatorHooks{
		OnLeaseDeleted:  func(id string) { deleted = id },
		OnLeaseLost:     func(id string, _ cluster.Origin) { lost = id },
		OnOriginClaimed: func(id string, _ cluster.Origin) { claimed = id },
	}
	cfg := config.Config{BroadcastGrace: 42, MaxBroadcasts: 7}
	o := clusterOptions(cfg, hooks, nil, "gawk", "pod-a", "10.0.0.1:4433", nil)
	if o.Namespace != "gawk" || o.PodName != "pod-a" || o.AdvertiseAddr != "10.0.0.1:4433" ||
		o.BroadcastGrace != 42 || o.MaxBroadcasts != 7 {
		t.Errorf("options = %+v", o)
	}
	if o.OnLeaseDeleted == nil || o.OnLeaseLost == nil || o.OnOriginClaimed == nil {
		t.Fatal("a hook did not reach cluster.Options")
	}
	o.OnLeaseDeleted("A")
	o.OnLeaseLost("B", cluster.Origin{})
	o.OnOriginClaimed("C", cluster.Origin{})
	if deleted != "A" || lost != "B" || claimed != "C" {
		t.Errorf("hooks ran as deleted=%q lost=%q claimed=%q", deleted, lost, claimed)
	}
}
