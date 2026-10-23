package relayscan_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
)

// R74 (docs/76 §9): /internal/admin/pops is the fourth endpoint. Each PoP is
// reported by the pod holding its link; the merge keeps one row per PoP,
// tagged with that pod.
func TestScanCollectsPopsFromTheirLinkHomePods(t *testing.T) {
	a := &fakePod{name: "gawk-server-0", pops: []relayscan.PopState{{Name: "assembly-2026", Linked: true, Live: 7, Forwarded: 2,
		PublicAddrs: []string{"203.0.113.7"}, LanAddr: "192.168.1.20"}}}
	b := &fakePod{name: "gawk-server-1", pops: []relayscan.PopState{{Name: "lanparty", Linked: true, Live: 1}}}
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(a.start(t), b.start(t))})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Pops) != 2 || snap.Pops[0].Name != "assembly-2026" || snap.Pops[1].Name != "lanparty" {
		t.Fatalf("pops = %+v", snap.Pops)
	}
	p, ok := snap.Pop("assembly-2026")
	if !ok || p.Pod != "gawk-server-0" || p.Live != 7 || p.Forwarded != 2 || p.LanAddr != "192.168.1.20" {
		t.Fatalf("Pop(assembly-2026) = %+v, %v", p, ok)
	}
	if _, ok := snap.Pop("nosuchpop"); ok {
		t.Fatal("found a PoP no pod reported")
	}
}

// Two pods can briefly both report a PoP across a force-take (docs/76 item
// 11): the one that says it is linked wins, whatever the scrape order.
func TestALinkedReportBeatsAStaleOne(t *testing.T) {
	stale := &fakePod{name: "gawk-server-0", pops: []relayscan.PopState{{Name: "assembly-2026", Linked: false, Live: 1}}}
	fresh := &fakePod{name: "gawk-server-1", pops: []relayscan.PopState{{Name: "assembly-2026", Linked: true, Live: 5}}}
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(stale.start(t), fresh.start(t))})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p, ok := snap.Pop("assembly-2026")
	if !ok || !p.Linked || p.Pod != "gawk-server-1" || p.Live != 5 || len(snap.Pops) != 1 {
		t.Fatalf("Pop = %+v, pops = %+v", p, snap.Pops)
	}
}

// A relay predating R74 answers 404: the pod stays reachable, its
// broadcasts and rooms stay visible, and PopsErr says why it reports no PoPs.
func TestPopsFailureKeepsThePodReachable(t *testing.T) {
	pod := &fakePod{name: "old", noPops: true,
		broadcasts: []relayscan.Broadcast{bc("EEE234", "k", "origin", true, "203.0.113.3", 1, 1)},
		rooms:      []relayscan.Room{{Code: "tuhisroom", Kind: "static"}}}
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(pod.start(t))})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	p := snap.Pods[0]
	if !p.Reachable || len(snap.Broadcasts) != 1 || len(snap.Rooms) != 1 || p.RoomsErr != "" {
		t.Fatalf("a pops failure hid the pod: %+v", p)
	}
	if !strings.Contains(p.PopsErr, "predates R74") || len(snap.Pops) != 0 {
		t.Fatalf("popsErr = %q, pops = %+v", p.PopsErr, snap.Pops)
	}
}
