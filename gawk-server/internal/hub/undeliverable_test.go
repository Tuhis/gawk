package hub

import (
	"errors"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The relay's undeliverable-video notice (docs/24 finding 19). The relay tells a
// viewer, on datagrams, that it holds video the viewer's session will not
// take: consecutive keyframe hand-offs that moved zero bytes, spanning
// UndeliverableAfter. These tests pin when it fires and, as importantly, the
// cases that must never fire it — a static screen, a slow link that is still
// moving bytes, an edge session.

// undeliverableAfterForTest keeps the suite fast; production uses
// UndeliverableVideoAfter (3 s).
const undeliverableAfterForTest = 150 * time.Millisecond

func undeliverableNotices(t *testing.T, f *fakeSender) []wire.VideoUndeliverable {
	t.Helper()
	var out []wire.VideoUndeliverable
	for _, d := range f.received() {
		if len(d) < 2 || d[1] != wire.TypeVideoUndeliverable {
			continue
		}
		m, err := wire.ParseVideoUndeliverable(d)
		if err != nil {
			t.Fatalf("relay sent a malformed VideoUndeliverable %x: %v", d, err)
		}
		out = append(out, m)
	}
	return out
}

// feedKeyframes ingests one keyframe (plus a delta) every gap for total,
// numbering frames from first, and returns the last frameID it ingested.
func feedKeyframes(t *testing.T, p *Publisher, first uint32, gap, total time.Duration) uint32 {
	t.Helper()
	id := first
	for end := time.Now().Add(total); time.Now().Before(end); id += 10 {
		ingestKeyframe(t, p, keyframeMsg(t, id, "vp8", "KEYFRAME"))
		p.HandleDatagram(chunk(t, id+1))
		time.Sleep(gap)
	}
	return id - 10
}

func subscriberDetail(t *testing.T, r *Registry, internal bool) SubscriberStats {
	t.Helper()
	for _, d := range singleBroadcastStats(t, r).SubscriberDetails {
		if d.Internal == internal {
			return d
		}
	}
	t.Fatalf("no subscriber with internal=%v in /statusz", internal)
	return SubscriberStats{}
}

// The headline case: a live-edge viewer whose stream path is wedged — every
// keyframe write parks until the next keyframe supersedes it, having moved
// nothing — is told, and a healthy viewer on the same broadcast is not.
// Notices are rate-limited to one per UndeliverableNoticeInterval and name the
// newest keyframe the relay could not deliver.
func TestUndeliverableNoticeWhileKeyframesMoveNoBytes(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	wedged := &fakeSender{kfBlock: make(chan struct{})} // never released
	healthy := &fakeSender{}
	sw, err := r.Subscribe(id, wedged)
	if err != nil {
		t.Fatalf("Subscribe(wedged): %v", err)
	}
	defer sw.Close()
	sh, _ := r.Subscribe(id, healthy)
	defer sh.Close()

	const first = 1000
	last := feedKeyframes(t, p, first, 30*time.Millisecond, UndeliverableNoticeInterval+300*time.Millisecond)

	got := undeliverableNotices(t, wedged)
	if len(got) == 0 {
		t.Fatal("wedged subscriber was never told its video is undeliverable")
	}
	// ~1.3 s of wedge at a 1 s interval: the first notice after 150 ms, at
	// most one more. More means the rate limit is not holding.
	if len(got) > 2 {
		t.Errorf("%d notices in ~1.3 s, want at most 2 (one per %v)", len(got), UndeliverableNoticeInterval)
	}
	for _, m := range got {
		if m.KeyframeID < first || m.KeyframeID > last {
			t.Errorf("notice names keyframe %d, want one of the ingested %d..%d", m.KeyframeID, first, last)
		}
		if time.Duration(m.StalledMs)*time.Millisecond < undeliverableAfterForTest {
			t.Errorf("notice reports %d ms stalled, below the %v threshold", m.StalledMs, undeliverableAfterForTest)
		}
	}
	if n := undeliverableNotices(t, healthy); len(n) != 0 {
		t.Errorf("healthy subscriber got %d undeliverable notices, want 0", len(n))
	}
	var total uint64
	for _, d := range singleBroadcastStats(t, r).SubscriberDetails {
		total += d.UndeliverableNotices
	}
	if total < uint64(len(got)) {
		t.Errorf("/statusz undeliverableNotices sums to %d, want at least the %d received", total, len(got))
	}
}

// A failed keyframe open is the other zero-byte hand-off (exhausted stream
// credit). The notice must reach the viewer before the open-failure eviction
// closes a session that WebKit may never report closed.
func TestUndeliverableNoticeOnKeyframeOpenFailuresPrecedesEviction(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfOpenErr: errors.New("no stream credit")}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for i := range KeyframeOpenFailEvictThreshold {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(i+1), "vp8", "KEY"))
		time.Sleep(40 * time.Millisecond)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, closed := f.getCloseInfo()
		return closed
	}, "the open-failure eviction")
	if len(undeliverableNotices(t, f)) == 0 {
		t.Fatal("no undeliverable notice before the open-failure eviction")
	}
}

// The static-screen guard. Screen capture is damage-driven: a paused game
// produces no frames, hence no keyframes, hence no hand-offs. A viewer whose
// last keyframe failed must not be told anything while nothing new exists —
// "has video it cannot deliver" is false when there is no video.
func TestNoUndeliverableNoticeOnStaticScreen(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfWriteErr: errors.New("write deadline exceeded")}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ingestKeyframe(t, p, keyframeMsg(t, 1, "vp8", "KEY"))
	waitFor(t, 5*time.Second, func() bool {
		return singleBroadcastStats(t, r).KeyframeDrops.Slow == 1
	}, "the one keyframe to fail")

	// The screen goes static: the relay's own keepalive still flows, nothing
	// else does, for many thresholds.
	for range 6 {
		r.PumpViewerCounts(time.Now().Add(time.Hour))
		time.Sleep(undeliverableAfterForTest)
	}
	if n := undeliverableNotices(t, f); len(n) != 0 {
		t.Fatalf("static screen produced %d undeliverable notices, want 0", len(n))
	}
}

// Slow is not undeliverable. A viewer on a thin link whose keyframe writes
// time out after moving part of the keyframe is congested, not wedged, and a
// reconnect would not help it: no notice, however long it lasts.
func TestNoUndeliverableNoticeWhileBytesStillMove(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfWriteErr: errors.New("write deadline exceeded"), kfWriteN: 3}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Fewer keyframes than KeyframeSlowEvictThreshold, spanning six notice
	// thresholds: the session must still be there for "no notice" to mean
	// anything.
	feedKeyframes(t, p, 1, 6*undeliverableAfterForTest/(KeyframeSlowEvictThreshold-2),
		6*undeliverableAfterForTest)
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("slow subscriber was evicted; the test no longer observes the notice path")
	}
	if n := undeliverableNotices(t, f); len(n) != 0 {
		t.Fatalf("slow-but-moving subscriber got %d undeliverable notices, want 0", len(n))
	}
}

// A delivered keyframe ends the episode: the zero-byte streak and its start
// time reset, so two failures straddling a success never add up to a notice.
func TestUndeliverableStreakResetsOnDeliveredKeyframe(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfWriteErr: errors.New("stall")}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	accounted := func(n uint64) {
		waitFor(t, 5*time.Second, func() bool {
			return singleBroadcastStats(t, r).KeyframeDrops.Total()+f.keyframeCount() >= n
		}, "keyframe to be accounted")
	}
	ingestKeyframe(t, p, keyframeMsg(t, 1, "vp8", "KEY"))
	accounted(1)
	time.Sleep(2 * undeliverableAfterForTest)
	f.setKfWriteFailure(0, nil)
	ingestKeyframe(t, p, keyframeMsg(t, 2, "vp8", "KEY"))
	accounted(2)
	f.setKfWriteFailure(0, errors.New("stall again"))
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY"))
	accounted(3)
	ingestKeyframe(t, p, keyframeMsg(t, 4, "vp8", "KEY"))
	accounted(4)
	if n := undeliverableNotices(t, f); len(n) != 0 {
		t.Fatalf("got %d notices across a delivered keyframe, want 0", len(n))
	}
}

// Resilient mode: video, audio and the control sideband all ride carrier
// streams, so the notice is the one thing that must NOT go there — it is
// written as a datagram, the path a stream wedge leaves open.
func TestUndeliverableNoticeReachesReliableViewerOnDatagrams(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfBlock: make(chan struct{})} // every keyframe write parks
	sub, err := r.SubscribeReliable(id, f, 0)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	feedKeyframes(t, p, 1, 30*time.Millisecond, 3*undeliverableAfterForTest)
	waitFor(t, 5*time.Second, func() bool { return len(undeliverableNotices(t, f)) > 0 },
		"a resilient subscriber to get its notice as a datagram")
	sub.Close()
	for _, rec := range f.carrierRecords(t) {
		if len(rec) >= 2 && rec[1] == wire.TypeVideoUndeliverable {
			t.Fatal("undeliverable notice was written to a carrier stream")
		}
	}
}

// Deep buffer: drainDVR opens its keyframe streams itself, so it must feed
// the same accounting.
func TestUndeliverableNoticeReachesDVRViewer(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		UndeliverableAfter: undeliverableAfterForTest,
		DVR:                DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
		DVRProgressTimeout: time.Minute,
	})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	// A failing write, not a parked one: drainDVR writes keyframes inline,
	// and the fake ignores write deadlines.
	f := &fakeSender{kfWriteErr: errors.New("write deadline exceeded")}
	if _, err := r.SubscribeDVR(id, f, 30000, 0); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	feedKeyframes(t, p, 1, 30*time.Millisecond, 3*undeliverableAfterForTest)
	waitFor(t, 5*time.Second, func() bool { return len(undeliverableNotices(t, f)) > 0 },
		"a Deep-buffer subscriber to get its notice")
}

// An edge (or PoP leg) session is plumbing: its own pod judges its own
// viewers, and an origin's notice would arrive at the edge as a datagram from
// its publisher. Never sent there.
func TestNoUndeliverableNoticeToInternalSubscriber(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfBlock: make(chan struct{})} // parked, superseded: never evicted
	sub, err := r.SubscribeInternal(id, f)
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	defer sub.Close()
	feedKeyframes(t, p, 1, 30*time.Millisecond, 4*undeliverableAfterForTest)
	if n := undeliverableNotices(t, f); len(n) != 0 {
		t.Fatalf("edge session got %d undeliverable notices, want 0", len(n))
	}
	if d := subscriberDetail(t, r, true); d.UndeliverableNotices != 0 {
		t.Errorf("/statusz counts %d notices on the edge session, want 0", d.UndeliverableNotices)
	}
}

// Version skew: a viewer that predates the notice drops it as a bad datagram
// but still counts it as inbound, which is what its dead-session watchdog
// reads. In resilient mode the control sideband rides the (wedged) carrier,
// so an unbounded notice would be the only thing keeping that watchdog from
// ever firing. The episode is therefore capped: after UndeliverableMaxNotices
// the relay goes quiet until a hand-off moves a byte again.
func TestUndeliverableNoticesStopAfterTheEpisodeCap(t *testing.T) {
	r := NewRegistry(discardLog, Options{UndeliverableAfter: undeliverableAfterForTest})
	id, p, _ := r.StartPublish("")
	defer p.Close()

	f := &fakeSender{kfBlock: make(chan struct{})}
	sub, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	feedKeyframes(t, p, 1, 30*time.Millisecond,
		undeliverableAfterForTest+time.Duration(UndeliverableMaxNotices)*UndeliverableNoticeInterval+300*time.Millisecond)
	if got := len(undeliverableNotices(t, f)); got != UndeliverableMaxNotices {
		t.Fatalf("%d notices over a long wedge, want exactly the cap of %d", got, UndeliverableMaxNotices)
	}
}
