package popsrv

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

type fakeWallHost struct {
	mu  sync.Mutex
	ops []string
}

func (h *fakeWallHost) VouchWall(_ context.Context, pop, op, code, id, label string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops = append(h.ops, op+" "+id)
	return nil
}

func (h *fakeWallHost) ServeRoomStream(context.Context, string, string, string, io.ReadWriteCloser) {}

func (h *fakeWallHost) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ops...)
}

type mapOwner struct {
	mu sync.Mutex
	m  map[string]string
}

func (o *mapOwner) Owner(id string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.m[id]
	return p, ok
}

func (o *mapOwner) set(id, pop string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[id] = pop
}

func newUnitWall(t *testing.T, wait time.Duration) (*Wall, *fakeWallHost, *mapOwner) {
	t.Helper()
	src := &fakeSource{pops: map[string]*pops.Pop{popName: {
		ObjectMeta: metav1.ObjectMeta{Name: popName, UID: "u"},
		Spec:       pops.PopSpec{Wall: &pops.Wall{RoomCode: "assembly-2026"}},
	}}}
	m := New(Options{Source: src, Log: harnessLog(t)})
	host := &fakeWallHost{}
	owner := &mapOwner{m: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := StartWall(ctx, m, WallOptions{Host: host, Owner: owner, OwnerWait: wait, OwnerPoll: 10 * time.Millisecond})
	return w, host, owner
}

// One broadcast whose reservation never shows (a local-only collision, or
// a relink's re-reserve still in flight) must not hold up the others: a
// relink resyncs every broadcast at once.
func TestWallAttachWaitingForItsReservationDoesNotBlockOthers(t *testing.T) {
	w, host, owner := newUnitWall(t, 3*time.Second)
	owner.set("BBBBBB", popName)
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallAttach, ID: "AAAAAA"})
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallAttach, ID: "BBBBBB"})
	start := time.Now()
	WaitFor(t, "the owned attach", func() bool { return len(host.got()) == 1 })
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the owned attach waited %v behind an unowned one", d)
	}
	if got := host.got(); got[0] != "attach BBBBBB" {
		t.Fatalf("ops = %v", got)
	}
	// Another PoP's reservation is never vouched for.
	owner.set("AAAAAA", "another-pop")
	time.Sleep(200 * time.Millisecond)
	if got := host.got(); len(got) != 1 {
		t.Fatalf("an attach for another PoP's reservation was vouched: %v", got)
	}
}

// A Hide that arrives while the attach still waits for its reservation
// wins: the attach is abandoned, so the tile never flashes.
func TestWallHideWhileTheAttachWaitsAbandonsIt(t *testing.T) {
	w, host, owner := newUnitWall(t, 3*time.Second)
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallAttach, ID: "AAAAAA"})
	time.Sleep(50 * time.Millisecond)
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallDetach, ID: "AAAAAA"})
	owner.set("AAAAAA", popName)
	WaitFor(t, "the detach", func() bool { return len(host.got()) >= 1 })
	time.Sleep(200 * time.Millisecond)
	if got := host.got(); len(got) != 1 || got[0] != "detach AAAAAA" {
		t.Fatalf("ops = %v, want the detach alone", got)
	}
	// Ops on one broadcast stay in order.
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallAttach, ID: "AAAAAA"})
	w.onRecord(popName, &poplink.Wall{Op: poplink.WallDetach, ID: "AAAAAA"})
	WaitFor(t, "the last op", func() bool {
		got := host.got()
		return len(got) >= 2 && got[len(got)-1] == "detach AAAAAA"
	})
}
