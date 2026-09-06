package hub

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// chunk is one delta video chunk datagram for frameID.
func chunk(t *testing.T, frameID uint32) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{FrameID: frameID, ChunkIndex: 0, ChunkCount: 1, TimestampUs: uint64(frameID)}, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	return d
}

// The publisher stall state (docs/06 revision 2026-09-06): a connected
// publisher from which NO datagram at all arrives — not a video chunk, not a
// keyframe stream, not the TimeSync/ClockMapping pings every broadcaster's
// own loop sends while its page runs — is reported as not live after
// PublisherStallTimeout. Ending it after BroadcastGrace of silence is the
// opt-in PublisherStallEnds. The fix for a background broadcaster tab that
// held its slot and a "live" room tile for five hours on zero datagrams.

func stallRegistry(stall, grace time.Duration, opt ...func(*Options)) *Registry {
	o := Options{PublisherStallTimeout: stall, BroadcastGrace: grace}
	for _, f := range opt {
		f(&o)
	}
	return NewRegistry(discardLog, o)
}

func TestStalledPublisherIsNotLive(t *testing.T) {
	r := stallRegistry(30*time.Millisecond, time.Hour)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()

	// Fresh publisher: live, the stall clock started at the claim.
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("fresh publisher: live=%v known=%v, want live", live, known)
	}
	// A datagram keeps it live past the timeout.
	time.Sleep(20 * time.Millisecond)
	p.HandleDatagram(chunk(t, 1))
	time.Sleep(20 * time.Millisecond)
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("publisher that sent a chunk 20ms ago reads as not live")
	}
	// Silence past the timeout: stalled — not live, still active, still known.
	time.Sleep(40 * time.Millisecond)
	live, _, known := r.BroadcastState(id)
	if !known || live {
		t.Fatalf("silent publisher: live=%v known=%v, want known and not live", live, known)
	}
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if !st.PublisherActive || !st.PublisherStalled {
		t.Fatalf("stats: active=%v stalled=%v, want active and stalled", st.PublisherActive, st.PublisherStalled)
	}
	// A datagram returning un-stalls it.
	p.HandleDatagram(chunk(t, 2))
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("publisher that resumed sending still reads as not live")
	}
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.PublisherStalled {
		t.Fatal("stats still stalled after a datagram returned")
	}
}

// Any publisher datagram counts, media or not: a paused game / static screen
// sends no video (capture is damage-driven) and possibly no audio, but its
// page's loop still sends TimeSync every 2 s and ClockMapping every 5 s —
// that is exactly what separates it from a frozen page, whose QUIC
// keepalives are answered by the browser's network process and nothing
// else. TimeSync never reaches the hub (the transport answers it inline), so
// the transport stamps it through NoteSeen.
func TestAnyPublisherDatagramCountsAsSeen(t *testing.T) {
	r := stallRegistry(30*time.Millisecond, time.Hour)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	stalled := func() bool {
		time.Sleep(40 * time.Millisecond)
		live, _, _ := r.BroadcastState(id)
		return !live
	}
	if !stalled() {
		t.Fatal("precondition: should be stalled")
	}
	ingestKeyframe(t, p, keyframeMsg(t, 1, "avc1.42E02A", "kf"))
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("a keyframe stream did not clear the stall")
	}
	if !stalled() {
		t.Fatal("should have stalled again")
	}
	audio, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{Seq: 1, TimestampUs: 1}, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	p.HandleDatagram(audio)
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("an audio frame did not clear the stall")
	}
	if !stalled() {
		t.Fatal("should have stalled again")
	}
	p.HandleDatagram(wire.AppendClockMapping(nil, 1234))
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("a ClockMapping did not clear the stall: a static screen keeps sending those")
	}
	if !stalled() {
		t.Fatal("should have stalled again")
	}
	p.NoteSeen() // the transport's TimeSync path
	if live, _, _ := r.BroadcastState(id); !live {
		t.Fatal("NoteSeen did not clear the stall")
	}
}

func TestStallSweepEndsTheBroadcastAfterGraceWhenOptedIn(t *testing.T) {
	r := stallRegistry(20*time.Millisecond, 60*time.Millisecond, func(o *Options) { o.PublisherStallEnds = true })
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pc := &fakePublisherConn{}
	if !p.BindConn(pc) {
		t.Fatal("BindConn = false on a fresh publisher")
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Stalled but inside the grace: the sweep only notes it.
	time.Sleep(30 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("viewer closed before the grace elapsed")
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast gone before the grace elapsed: %v", err)
	}

	// Silent for the whole grace: ended for everyone with the terminal code.
	time.Sleep(40 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	code, closed := f.getCloseInfo()
	if !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("viewer: closed=%v code=%d, want closed with %d", closed, code, wire.CloseCodeBroadcastEnded)
	}
	if pcode, pclosed := pc.getCloseInfo(); !pclosed || pcode != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("publisher session: closed=%v code=%d, want closed with %d", pclosed, pcode, wire.CloseCodeBroadcastEnded)
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("broadcast still known after the stall ended it: %v", err)
	}
	// The slot is free again.
	if err := r.CheckPublishNew(); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

// The default (PR #302 review, option (a)): stall → away only. Past the
// grace the broadcast is still held — away, not ended — and the slot stays
// taken; the operator sees it on /statusz and has /internal/admin.
func TestStallPastTheGraceWithoutTheKnobStaysAwayAndHeld(t *testing.T) {
	r := stallRegistry(20*time.Millisecond, 40*time.Millisecond, func(o *Options) { o.MaxBroadcasts = 1 })
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	pc := &fakePublisherConn{}
	if !p.BindConn(pc) {
		t.Fatal("BindConn = false on a fresh publisher")
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	time.Sleep(70 * time.Millisecond) // well past stall + grace
	r.SweepStalledPublishers(time.Now())
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("viewer closed: the stall ended the broadcast without -publisher-stall-ends")
	}
	if _, closed := pc.getCloseInfo(); closed {
		t.Fatal("publisher session closed: the stall ended the broadcast without -publisher-stall-ends")
	}
	live, _, known := r.BroadcastState(id)
	if !known || live {
		t.Fatalf("live=%v known=%v, want known and away", live, known)
	}
	if err := r.CheckPublishNew(); !errors.Is(err, ErrMaxBroadcasts) {
		t.Fatalf("slot released without the knob: CheckPublishNew = %v, want %v", err, ErrMaxBroadcasts)
	}
}

// OnPublisherStalled is the cluster hook (docs/44 §4.9): fired by the sweep
// on the onset and on recovery, outside the lock, once per transition — so
// the origin can stamp its lease and other pods' rooms show the tile away.
func TestStallSweepReportsTransitionsOnce(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	r := stallRegistry(20*time.Millisecond, time.Hour, func(o *Options) {
		o.OnPublisherStalled = func(_ string, stalled bool) {
			mu.Lock()
			calls = append(calls, stalled)
			mu.Unlock()
		}
	})
	_, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	got := func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), calls...)
	}
	r.SweepStalledPublishers(time.Now())
	if len(got()) != 0 {
		t.Fatalf("hook fired on a fresh publisher: %v", got())
	}
	time.Sleep(30 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	r.SweepStalledPublishers(time.Now())
	if c := got(); len(c) != 1 || !c[0] {
		t.Fatalf("after the onset, two sweeps: calls = %v, want [true]", c)
	}
	p.HandleDatagram(chunk(t, 1))
	r.SweepStalledPublishers(time.Now())
	r.SweepStalledPublishers(time.Now())
	if c := got(); len(c) != 2 || c[1] {
		t.Fatalf("after recovery, two sweeps: calls = %v, want [true false]", c)
	}
}

func TestStallDisabledByZeroTimeout(t *testing.T) {
	r := stallRegistry(0, 30*time.Millisecond, func(o *Options) { o.PublisherStallEnds = true })
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	time.Sleep(50 * time.Millisecond)
	r.SweepStalledPublishers(time.Now())
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("with the timeout off a silent publisher must stay live: live=%v known=%v", live, known)
	}
}

func TestEdgeHubNeverStalls(t *testing.T) {
	r := stallRegistry(10*time.Millisecond, time.Hour)
	id, p, err := r.EdgePublish("ABCDEF")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	defer p.Close()
	time.Sleep(30 * time.Millisecond)
	if live, _, known := r.BroadcastState(id); !known || !live {
		t.Fatalf("edge hub: live=%v known=%v, want live (the Lease is its liveness)", live, known)
	}
}
