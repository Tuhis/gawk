package hub

// R18 live viewer count (docs/23): the count pump, the cache/prime/invalidate
// lifecycle, the publisher push, and the cluster aggregation — all driven
// through the exported PumpViewerCounts tick seam (Decision 4: the goroutine
// is never started by NewRegistry, so these tests stay deterministic).

import (
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// viewerCounts extracts the parsed counts of every ViewerCount datagram the
// fake sender received, in arrival order.
func viewerCounts(t *testing.T, f *fakeSender) []uint32 {
	t.Helper()
	var out []uint32
	for _, d := range f.received() {
		if len(d) >= 2 && d[1] == wire.TypeViewerCount {
			count, err := wire.ParseViewerCount(d)
			if err != nil {
				t.Fatalf("subscriber received malformed ViewerCount %x: %v", d, err)
			}
			out = append(out, count)
		}
	}
	return out
}

func waitViewerCounts(t *testing.T, f *fakeSender, want []uint32) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		got := viewerCounts(t, f)
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}, "viewer count datagrams")
}

// pubPush collects the count pump's pushes to the publisher (BindSend).
type pubPush struct {
	mu  sync.Mutex
	got []uint32
}

func (p *pubPush) bind(t *testing.T, pub *Publisher) {
	t.Helper()
	pub.BindSend(func(d []byte) {
		count, err := wire.ParseViewerCount(d)
		if err != nil {
			t.Errorf("publisher push is not a ViewerCount: %x: %v", d, err)
			return
		}
		p.mu.Lock()
		p.got = append(p.got, count)
		p.mu.Unlock()
	})
}

func (p *pubPush) counts() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint32(nil), p.got...)
}

func TestViewerCountReachesViewersAndPublisher(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	push := &pubPush{}
	push.bind(t, pub)

	f1, f2 := &fakeSender{}, &fakeSender{}
	if _, err := r.Subscribe(id, f1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	now := time.Now()
	r.PumpViewerCounts(now)

	waitViewerCounts(t, f1, []uint32{2})
	waitViewerCounts(t, f2, []uint32{2})
	if got := push.counts(); len(got) != 1 || got[0] != 2 {
		t.Errorf("publisher pushes = %v, want [2]", got)
	}

	// The origin's /statusz mirror of the same number (Y6).
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.ViewersGlobal != 2 {
		t.Errorf("Stats ViewersGlobal = %d, want 2", st.ViewersGlobal)
	}

	// A change is emitted on the next tick.
	f3 := &fakeSender{}
	if _, err := r.Subscribe(id, f3); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	r.PumpViewerCounts(now.Add(ViewerCountInterval))
	waitViewerCounts(t, f1, []uint32{2, 3})
	if got := push.counts(); len(got) != 2 || got[1] != 3 {
		t.Errorf("publisher pushes = %v, want [2 3]", got)
	}
}

func TestViewerCountZeroViewersPushedToPublisher(t *testing.T) {
	// The broadcaster's own preview structurally can't be counted: a
	// publisher is never a member of b.subs, so an audience of nobody is 0.
	r := NewRegistry(discardLog, Options{})
	_, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	push := &pubPush{}
	push.bind(t, pub)

	r.PumpViewerCounts(time.Now())
	if got := push.counts(); len(got) != 1 || got[0] != 0 {
		t.Errorf("publisher pushes = %v, want [0]", got)
	}
}

func TestViewerCountChangeDrivenWithKeepalive(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	push := &pubPush{}
	push.bind(t, pub)
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	t0 := time.Now()
	r.PumpViewerCounts(t0)
	waitViewerCounts(t, f, []uint32{1})

	// Unchanged count inside the keepalive window: no re-emit.
	for i := 1; i < 5; i++ {
		r.PumpViewerCounts(t0.Add(time.Duration(i) * ViewerCountInterval))
	}
	waitViewerCounts(t, f, []uint32{1})
	if got := push.counts(); len(got) != 1 {
		t.Errorf("publisher pushes = %v, want exactly one before keepalive", got)
	}

	// The keepalive re-emits the unchanged count (datagram-loss repair).
	r.PumpViewerCounts(t0.Add(ViewerCountKeepalive))
	waitViewerCounts(t, f, []uint32{1, 1})
	if got := push.counts(); len(got) != 2 {
		t.Errorf("publisher pushes = %v, want two after keepalive", got)
	}
}

func TestViewerCountJoinPrimedImmediately(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f1 := &fakeSender{}
	if _, err := r.Subscribe(id, f1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	r.PumpViewerCounts(time.Now())
	waitViewerCounts(t, f1, []uint32{1})

	// A late joiner sees the cached count at subscribe, without a tick. The
	// primed value is the last emitted one (1) — the next tick corrects it.
	f2 := &fakeSender{}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitViewerCounts(t, f2, []uint32{1})
}

func TestViewerCountStormEmitsOncePerTick(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	stable := &fakeSender{}
	if _, err := r.Subscribe(id, stable); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	now := time.Now()
	r.PumpViewerCounts(now)
	waitViewerCounts(t, stable, []uint32{1})

	// A reconnect storm between ticks: 20 subscribe/close cycles emit
	// nothing at all — emits happen only on the tick, so the storm cannot
	// spam clients (Decision 4 storm resistance).
	for range 20 {
		churn := &fakeSender{}
		s, err := r.Subscribe(id, churn)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		s.Close()
	}
	waitViewerCounts(t, stable, []uint32{1})

	// The next tick reflects the settled state in one emit... which is the
	// keepalive-free "unchanged" case here, so nothing new goes out.
	r.PumpViewerCounts(now.Add(ViewerCountInterval))
	waitViewerCounts(t, stable, []uint32{1})
}

func TestViewerCountDropsOnSubscriberClose(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f1, f2 := &fakeSender{}, &fakeSender{}
	s1, err := r.Subscribe(id, f1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	now := time.Now()
	r.PumpViewerCounts(now)
	waitViewerCounts(t, f2, []uint32{2})

	s1.Close()
	r.PumpViewerCounts(now.Add(ViewerCountInterval))
	waitViewerCounts(t, f2, []uint32{2, 1})
}

func TestViewerCountCacheClearedOnNewPublisherAndInvalidatePrimes(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f1 := &fakeSender{}
	if _, err := r.Subscribe(id, f1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	r.PumpViewerCounts(time.Now())
	waitViewerCounts(t, f1, []uint32{1})

	// InvalidatePrimes clears the cache: a joiner gets no stale prime.
	r.InvalidatePrimes(id)
	f2 := &fakeSender{}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := viewerCounts(t, f2); len(got) != 0 {
		t.Errorf("joiner after InvalidatePrimes primed with %v, want nothing", got)
	}

	// A new publisher session clears the cache too, and resets the emit
	// tracking so the fresh session gets an emit on its first tick even with
	// an unchanged count.
	prev := time.Now()
	r.PumpViewerCounts(prev) // re-cache for the current session
	pub.Close()
	if _, _, err := r.StartPublish(id); err != nil {
		t.Fatalf("StartPublish reclaim: %v", err)
	}
	f3 := &fakeSender{}
	if _, err := r.Subscribe(id, f3); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := viewerCounts(t, f3); len(got) != 0 {
		t.Errorf("joiner after publisher restart primed with %v, want nothing", got)
	}
	r.PumpViewerCounts(prev.Add(time.Millisecond)) // same wall-clock window: still emits
	waitViewerCounts(t, f3, []uint32{3})
}

// Cluster aggregation (docs/23 Decision 5, chunk Y3): the origin sums its own
// external subscribers with each edge's reported downstream count; edge
// sessions themselves are never counted; the emitted G reaches edge sessions
// (for forwarding) exactly like viewers.
func TestViewerCountClusterAggregation(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	push := &pubPush{}
	push.bind(t, pub)
	v1, v2 := &fakeSender{}, &fakeSender{}
	if _, err := r.Subscribe(id, v1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := r.Subscribe(id, v2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Attaching an edge session changes nothing: it is plumbing, not a
	// viewer, and it hasn't reported any downstream viewers yet.
	edge := &fakeSender{}
	edgeSub, err := r.SubscribeInternal(id, edge)
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	now := time.Now()
	r.PumpViewerCounts(now)
	waitViewerCounts(t, v1, []uint32{2})
	if got := push.counts(); len(got) != 1 || got[0] != 2 {
		t.Errorf("publisher pushes = %v, want [2]", got)
	}
	// The edge session received G too — that is how it forwards it down.
	waitViewerCounts(t, edge, []uint32{2})

	// A viewer joining *behind* the edge raises G by the report.
	edgeSub.RecordDownstreamViewers(3)
	r.PumpViewerCounts(now.Add(ViewerCountInterval))
	waitViewerCounts(t, v1, []uint32{2, 5})
	waitViewerCounts(t, edge, []uint32{2, 5})
	if got := push.counts(); len(got) != 2 || got[1] != 5 {
		t.Errorf("publisher pushes = %v, want [2 5]", got)
	}
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.ViewersGlobal != 5 || st.Subscribers != 2 || st.EdgeSessions != 1 {
		t.Errorf("Stats = viewersGlobal %d / subscribers %d / edgeSessions %d, want 5/2/1",
			st.ViewersGlobal, st.Subscribers, st.EdgeSessions)
	}

	// The edge detaching drops its whole subtree from G.
	edgeSub.Close()
	r.PumpViewerCounts(now.Add(2 * ViewerCountInterval))
	waitViewerCounts(t, v1, []uint32{2, 5, 2})
}

// An EDGE hub forwards an upstream G to its local viewers verbatim
// (Decision 5c) and primes late joiners with it; the pump itself skips edge
// hubs entirely.
func TestViewerCountEdgeHubForwardsAndPrimes(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.EdgePublish("K7M2QP")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	f1 := &fakeSender{}
	if _, err := r.Subscribe(id, f1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The pump never emits for an edge hub (no aggregation, no broadcaster).
	r.PumpViewerCounts(time.Now())
	time.Sleep(50 * time.Millisecond)
	if got := viewerCounts(t, f1); len(got) != 0 {
		t.Errorf("edge hub emitted %v from the pump, want nothing", got)
	}

	// An upstream G arrives through the edge's Publisher surface: forwarded.
	pub.HandleDatagram(wire.AppendViewerCount(nil, 7))
	waitViewerCounts(t, f1, []uint32{7})

	// Late joiners are primed from the forwarded cache.
	f2 := &fakeSender{}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitViewerCounts(t, f2, []uint32{7})
}

// A ViewerCount sent by a real broadcaster is a spoof: on an origin hub the
// TypeViewerCount dispatch is gated on b.edge, so it is dropped without
// reaching viewers or the cache (Decision 6).
func TestViewerCountFromBroadcasterIgnoredOnOrigin(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f1 := &fakeSender{}
	if _, err := r.Subscribe(id, f1); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	pub.HandleDatagram(wire.AppendViewerCount(nil, 9000))
	time.Sleep(50 * time.Millisecond)
	if got := viewerCounts(t, f1); len(got) != 0 {
		t.Errorf("spoofed ViewerCount reached viewers: %v", got)
	}
	// Not cached either: a joiner gets no prime.
	f2 := &fakeSender{}
	if _, err := r.Subscribe(id, f2); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := viewerCounts(t, f2); len(got) != 0 {
		t.Errorf("spoofed ViewerCount was cached and primed: %v", got)
	}

	// A malformed one still counts as a bad datagram.
	pub.HandleDatagram([]byte{wire.Version, wire.TypeViewerCount, 0x01})
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.BadDatagrams != 1 {
		t.Errorf("BadDatagrams = %d, want 1", st.BadDatagrams)
	}
}

// The count keepalive must keep flowing while the broadcaster is merely away.
// It is the only app-layer traffic a subscribed viewer is guaranteed to see
// when no media is flowing, which is what lets the viewer tell "my session is
// dead" from "the broadcaster stepped away" (BUGS.md, 2026-07-22 paired
// capture; docs/05 D1 keepalive keeps that session open on purpose). Skipping
// the pump for an away publisher made those two indistinguishable on the wire.
func TestViewerCountKeepaliveContinuesWhilePublisherAway(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	t0 := time.Now()
	r.PumpViewerCounts(t0)
	waitViewerCounts(t, f, []uint32{1})

	// The broadcaster goes away; the hub survives its grace period and so does
	// the viewer's session.
	pub.Close()

	// Inside the keepalive window nothing changes (the count is unchanged).
	r.PumpViewerCounts(t0.Add(ViewerCountInterval))
	waitViewerCounts(t, f, []uint32{1})

	// Past it, the keepalive re-emits — the viewer's proof it is still attached.
	r.PumpViewerCounts(t0.Add(ViewerCountKeepalive))
	waitViewerCounts(t, f, []uint32{1, 1})
	r.PumpViewerCounts(t0.Add(2 * ViewerCountKeepalive))
	waitViewerCounts(t, f, []uint32{1, 1, 1})
}
