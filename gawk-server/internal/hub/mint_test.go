package hub

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
)

// R33 (docs/83 D3): the mint-only draw creates nothing — no hub, no event,
// nothing counted against MaxBroadcasts — and follows StartPublish's
// collision rules: never a reserved code.
func TestMintUnusedIDCreatesNothing(t *testing.T) {
	rec := &busRecorder{}
	r := NewRegistry(discardLog, Options{MaxBroadcasts: 1, OnEvent: rec.hook})
	id, err := r.MintUnusedID()
	if err != nil {
		t.Fatalf("MintUnusedID: %v", err)
	}
	if norm, err := broadcastid.Normalize(id); err != nil || norm != id {
		t.Fatalf("minted %q, want a normalized broadcast ID (%v)", id, err)
	}
	if n := len(r.Stats().Broadcasts); n != 0 {
		t.Errorf("a mint-only draw created %d broadcast(s)", n)
	}
	if got := rec.types(); len(got) != 0 {
		t.Errorf("a mint-only draw emitted %v", got)
	}
	// Not counted: the one slot is still free for a real broadcast.
	if err := r.CheckPublishNew(); err != nil {
		t.Errorf("CheckPublishNew after a mint-only draw = %v, want nil", err)
	}
	// ...and the reclaim with that ID creates it, which is what D3 relies on.
	got, pub, err := r.ResumePublish(id)
	if err != nil || got != id {
		t.Fatalf("ResumePublish(%s) = %q, %v", id, got, err)
	}
	pub.Close()
}

func TestMintUnusedIDSkipsReservedIDs(t *testing.T) {
	r := NewRegistry(discardLog, Options{IDReserved: func(string) bool { return true }})
	if id, err := r.MintUnusedID(); err == nil {
		t.Fatalf("minted %s with every ID reserved", id)
	}
}

// A live hub's ID is never drawn: with every ID but one reserved and that
// one live, the draw gives up rather than hand out the live broadcast's ID.
func TestMintUnusedIDSkipsLiveIDs(t *testing.T) {
	var live string
	r := NewRegistry(discardLog, Options{IDReserved: func(id string) bool { return live != "" && id != live }})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()
	live = id
	if got, err := r.MintUnusedID(); err == nil {
		t.Fatalf("minted %s while the only unreserved ID is live", got)
	}
}
