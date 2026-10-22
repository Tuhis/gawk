package hub

import "testing"

// R74 PP9 (docs/76 items 23, 25): a PoP's downlink leg can land on an edge
// pod. The leg is demand there (the edge must not linger out under it, nor
// expire the hub it reads), and the party viewers it reports belong in what
// the edge reports upstream, or home's G misses them.
func TestEdgeDemandCountsPopLegs(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxSubscribers: 4})
	id, epub, err := r.EdgePublish("K7XQ2M")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer epub.Close()
	if serving, viewers := r.EdgeDemand(id); serving != 0 || viewers != 0 {
		t.Fatalf("empty edge hub: EdgeDemand = %d, %d; want 0, 0", serving, viewers)
	}

	viewer, err := r.Subscribe(id, &fakeSender{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	leg, err := r.SubscribeLeg(id, &fakeSender{}, LegOptions{})
	if err != nil {
		t.Fatalf("SubscribeLeg: %v", err)
	}
	leg.RecordDownstreamViewers(10)
	if serving, viewers := r.EdgeDemand(id); serving != 2 || viewers != 11 {
		t.Fatalf("a viewer and a leg reporting 10: EdgeDemand = %d, %d; want 2, 11", serving, viewers)
	}
	if got := r.ViewerSubscribers(id); got != 1 {
		t.Fatalf("ViewerSubscribers = %d, want 1 (a leg is never a viewer itself)", got)
	}

	// With only the leg left, the hub is still in use: the linger-out
	// expiry must keep it.
	viewer.Close()
	epub.Close()
	if r.ExpireEdgeIfViewerless(id) {
		t.Fatal("an edge hub serving a PoP leg was expired")
	}
	leg.Close()
	if !r.ExpireEdgeIfViewerless(id) {
		t.Fatal("an edge hub serving nothing was kept")
	}
	if serving, viewers := r.EdgeDemand("!!!"); serving != 0 || viewers != 0 {
		t.Fatalf("malformed ID: EdgeDemand = %d, %d", serving, viewers)
	}
}
