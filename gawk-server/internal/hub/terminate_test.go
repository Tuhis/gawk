package hub

// R39 AP3 (docs/42 §4.3, §9): Registry.TerminateBroadcast — the only entry
// point that may kill a live broadcast.

import (
	"reflect"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

const terminateCode = uint32(wire.CloseCodeTerminatedByOperator)

// liveBroadcast builds a broadcast with a bound publisher and one subscriber
// of every kind AP3 must reach: a plain viewer, an internal edge session, and
// an R30 stripe leg. It returns everything a test needs to assert on.
type liveBroadcast struct {
	r        *Registry
	id       string
	pub      *Publisher
	pubConn  *fakePublisherConn
	viewer   *fakeSender
	internal *fakeSender
	leg      *fakeSender
}

func newLiveBroadcast(t *testing.T, opts Options) *liveBroadcast {
	t.Helper()
	r := NewRegistry(discardLog, opts)
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	conn := &fakePublisherConn{}
	if !pub.BindConn(conn) {
		t.Fatal("BindConn = false on a fresh publisher")
	}

	lb := &liveBroadcast{
		r: r, id: id, pub: pub, pubConn: conn,
		viewer: &fakeSender{}, internal: &fakeSender{}, leg: &fakeSender{},
	}
	if _, err := r.Subscribe(id, lb.viewer); err != nil {
		t.Fatalf("Subscribe(viewer): %v", err)
	}
	if _, err := r.SubscribeInternal(id, lb.internal); err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	if _, err := r.SubscribeStripeLeg(id, lb.leg,
		StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0); err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	return lb
}

// hubFor reaches the (unexported) hub object so a test can assert on state
// that is unreachable once the hub is unregistered — which is exactly the
// state TerminateBroadcast has to purge.
func (lb *liveBroadcast) hubFor(t *testing.T) *broadcastHub {
	t.Helper()
	lb.r.mu.Lock()
	defer lb.r.mu.Unlock()
	b, ok := lb.r.hubs[lb.id]
	if !ok {
		t.Fatalf("hub %q is not registered", lb.id)
	}
	return b
}

// Every session the pod holds for the broadcast is closed with the code the
// caller passed — publisher, viewer, internal edge session and stripe leg
// alike. A kill that reached only viewers would leave the broadcaster
// publishing into a dead hub and the downstream edge pods serving stale
// media (docs/42 §4.1 step 3).
func TestTerminateBroadcastClosesEverySessionKind(t *testing.T) {
	lb := newLiveBroadcast(t, Options{})

	if !lb.r.TerminateBroadcast(lb.id, terminateCode, "terminated by operator") {
		t.Fatal("TerminateBroadcast = false on a live broadcast")
	}

	if code, closed := lb.pubConn.getCloseInfo(); !closed || code != terminateCode {
		t.Errorf("publisher close = %d/%v, want %d/true", code, closed, terminateCode)
	}
	for name, f := range map[string]*fakeSender{
		"viewer": lb.viewer, "internal edge session": lb.internal, "stripe leg": lb.leg,
	} {
		if code, closed := f.getCloseInfo(); !closed || code != terminateCode {
			t.Errorf("%s close = %d/%v, want %d/true", name, code, closed, terminateCode)
		}
	}
	if err := lb.r.CheckSubscribe(lb.id); err == nil {
		t.Error("the broadcast is still subscribable after termination")
	}
}

// The code is a PARAMETER, not a second hardcoded constant: the ordinary
// expiry path still sends 4000, and the two share one loop (docs/42 §4.3 —
// "hub.go:1918-1921 becomes parameterized").
func TestExpiryStillSends4000AfterParameterization(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: time.Hour})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	pub.Close()
	r.EndBroadcast(id)

	if code, closed := f.getCloseInfo(); !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
		t.Errorf("expiry close = %d/%v, want %d/true", code, closed, wire.CloseCodeBroadcastEnded)
	}
}

// docs/26 D11: a surviving DVR ring replays banned content to DVR cursors,
// and a surviving prime cache serves the killed broadcast's keyframe to
// anything still holding the hub. Both are purged under the lock, before any
// session is closed.
func TestTerminateBroadcastPurgesPrimesRingsAndGraceTimer(t *testing.T) {
	lb := newLiveBroadcast(t, Options{DVRAudio: true, BroadcastGrace: time.Hour})

	// Fill every cache the kill has to clear: a keyframe prime (+ embedded
	// config), a clock mapping, an audio config, and a DVR ring with content.
	dvr := &fakeSender{}
	if _, err := lb.r.SubscribeDVR(lb.id, dvr, 1000); err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	ingestKeyframe(t, lb.pub, keyframeMsg(t, 1, "avc1.42E01F", "keyframe-payload"))
	lb.pub.HandleDatagram(wire.AppendClockMapping(nil, 1234))
	lb.pub.HandleDatagram(chunkDgram(t, false, 2, 0, 1, "delta"))

	b := lb.hubFor(t)
	if b.cachedKeyframe == nil || b.dvr == nil {
		t.Fatalf("test setup did not populate the caches: keyframe=%v dvr=%v",
			b.cachedKeyframe != nil, b.dvr != nil)
	}

	// Arm a grace timer the way a publisher-away broadcast has one, so the
	// kill has something to stop.
	lb.pub.Close()
	if b.graceTimer == nil {
		t.Fatal("test setup did not arm a grace timer")
	}

	if !lb.r.TerminateBroadcast(lb.id, terminateCode, "terminated by operator") {
		t.Fatal("TerminateBroadcast = false")
	}

	// Read the detached hub directly: nothing else can still see it, which is
	// the point — anything left here is reachable only by whoever already
	// holds a pointer, and that is exactly the leak class.
	lb.r.mu.Lock()
	defer lb.r.mu.Unlock()
	for name, got := range map[string]any{
		"cachedKeyframe":     b.cachedKeyframe,
		"cachedClockMapping": b.cachedClockMapping,
		"cachedAudioConfig":  b.cachedAudioConfig,
		"cachedViewerCount":  b.cachedViewerCount,
	} {
		if v := reflect.ValueOf(got); v.IsValid() && !v.IsNil() {
			t.Errorf("%s survived the termination", name)
		}
	}
	if b.cachedKeyframeHasConfig || b.cachedKeyframeID != 0 {
		t.Errorf("keyframe prime metadata survived: hasConfig=%v id=%d",
			b.cachedKeyframeHasConfig, b.cachedKeyframeID)
	}
	if b.dvr != nil || b.dvrAudio != nil {
		t.Errorf("DVR rings survived the termination: video=%v audio=%v",
			b.dvr != nil, b.dvrAudio != nil)
	}
	if b.graceTimer != nil || !b.graceStart.IsZero() {
		t.Errorf("grace timer survived the termination: timer=%v start=%v",
			b.graceTimer != nil, b.graceStart)
	}
}

// docs/35 finding 3, as a regression test rather than a checklist: the expiry
// fold predated R29 and R30, so a hub taking its parity and stripe counters
// with it made a fleet total — and the Prometheus counter built on it — go
// BACKWARDS. Reflection over TotalStats, not a hand-written field list: a
// counter added in some later milestone and forgotten in the fold fails this
// test the day it lands, which a hand-written list would not.
func TestTerminateBroadcastFoldsEveryCounter(t *testing.T) {
	lb := newLiveBroadcast(t, Options{DVRAudio: true})

	// Move as many counters off zero as the hub API allows: keyframes in and
	// out, delta datagrams, a bad datagram, and an oversize keyframe.
	ingestKeyframe(t, lb.pub, keyframeMsg(t, 1, "avc1.42E01F", "kf"))
	waitKeyframes(t, lb.viewer, 1)
	for i := range 5 {
		lb.pub.HandleDatagram(chunkDgram(t, false, uint32(2+i), 0, 1, "delta"))
	}
	lb.pub.HandleDatagram([]byte{0xff, 0xff, 0xff})

	before := lb.r.Stats().Totals
	nonZero := 0
	for _, name := range counterFields(before) {
		if numeric(before, name) > 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatal("test setup produced no non-zero counters; the fold assertion would be vacuous")
	}

	if !lb.r.TerminateBroadcast(lb.id, terminateCode, "terminated by operator") {
		t.Fatal("TerminateBroadcast = false")
	}

	after := lb.r.Stats().Totals
	for _, name := range counterFields(before) {
		b, a := numeric(before, name), numeric(after, name)
		if a < b {
			t.Errorf("Totals.%s went BACKWARDS across the kill: %v -> %v — the fold is missing this counter (docs/35 finding 3)",
				name, b, a)
		}
	}
	// Broadcasts is a GAUGE, not a counter, and must fall — the assertion
	// above deliberately excludes it, and this pins that it really is a
	// gauge rather than a counter someone forgot to fold.
	if after.Broadcasts != 0 {
		t.Errorf("Totals.Broadcasts = %d after the kill, want 0", after.Broadcasts)
	}
}

// counterFields lists the monotonic (never-decreasing) fields of TotalStats.
// The two live gauges are named explicitly so a new field is treated as a
// counter by default — the safe direction, since a gauge wrongly asserted
// monotonic shows up as an immediate test failure while a counter wrongly
// excluded is the silent bug docs/35 finding 3 was.
func counterFields(t TotalStats) []string {
	gauges := map[string]bool{
		"Broadcasts": true, "Subscribers": true, "ReliableSubscribers": true,
		"StripeLegs": true, "StripedPrimaries": true,
	}
	var out []string
	rt := reflect.TypeOf(t)
	for i := range rt.NumField() {
		f := rt.Field(i)
		if gauges[f.Name] {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Uint64, reflect.Int, reflect.Int64:
			out = append(out, f.Name)
		case reflect.Struct:
			// KeyframeDrops: every cause counts as its own counter.
			for j := range f.Type.NumField() {
				out = append(out, f.Name+"."+f.Type.Field(j).Name)
			}
		}
	}
	return out
}

func numeric(t TotalStats, path string) float64 {
	v := reflect.ValueOf(t)
	for _, seg := range splitDot(path) {
		v = v.FieldByName(seg)
	}
	switch v.Kind() {
	case reflect.Uint64:
		return float64(v.Uint())
	default:
		return float64(v.Int())
	}
}

func splitDot(s string) []string {
	for i := range len(s) {
		if s[i] == '.' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

// Idempotence: the same informer event reaches every pod in the fleet, and
// all but one of them have never heard of the broadcast.
func TestTerminateBroadcastNoOpsOnAbsentHub(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	if r.TerminateBroadcast("ZZZ23Z", terminateCode, "terminated by operator") {
		t.Error("TerminateBroadcast = true for a broadcast this pod does not have")
	}
	// A malformed ID is the same answer, not a panic.
	if r.TerminateBroadcast("!!!", terminateCode, "terminated by operator") {
		t.Error("TerminateBroadcast = true for a malformed ID")
	}
	// And terminating twice is a no-op the second time.
	lb := newLiveBroadcast(t, Options{})
	if !lb.r.TerminateBroadcast(lb.id, terminateCode, "x") {
		t.Fatal("first TerminateBroadcast = false")
	}
	if lb.r.TerminateBroadcast(lb.id, terminateCode, "x") {
		t.Error("second TerminateBroadcast = true, want a no-op")
	}
}

// An ORIGIN kill fires OnBroadcastExpired, which is what deletes the cluster
// Lease and makes the kill fleet-wide (docs/42 §4.1 step 3). An EDGE hub must
// NOT: an edge is derived state that never owns the lease, and deleting the
// origin's lease from an edge would kill the broadcast everywhere for the
// wrong reason (R17 W4).
func TestTerminateBroadcastFiresExpiredHookForOriginOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		edge     bool
		wantFire bool
	}{
		{"origin", false, true},
		{"edge", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var expired []string
			r := NewRegistry(discardLog, Options{
				OnBroadcastExpired: func(id string) { expired = append(expired, id) },
			})
			var id string
			var pub *Publisher
			var err error
			if tc.edge {
				id = "ABC23Z"
				if _, pub, err = r.EdgePublish(id); err != nil {
					t.Fatalf("EdgePublish: %v", err)
				}
			} else if id, pub, err = r.StartPublish(""); err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			defer pub.Close()

			if !r.TerminateBroadcast(id, terminateCode, "terminated by operator") {
				t.Fatal("TerminateBroadcast = false")
			}
			if got := len(expired) == 1; got != tc.wantFire {
				t.Errorf("OnBroadcastExpired fired = %v (%v), want %v", got, expired, tc.wantFire)
			}
		})
	}
}

// THE GUARD STAYS. hub.go's publisherActive check is what stops a racing GC
// janitor from killing a live broadcast, and TerminateBroadcast bypassing it
// must not have relaxed it for anybody else (docs/42 §3).
func TestGCGuardStillHoldsForEndBroadcast(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: time.Hour})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	r.EndBroadcast(id)
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("EndBroadcast killed a broadcast with a LIVE publisher: %v", err)
	}
	if _, closed := f.getCloseInfo(); closed {
		t.Error("EndBroadcast closed a live broadcast's subscriber")
	}

	// ExpireEdgeIfViewerless and the grace timer share the same guard.
	if r.ExpireEdgeIfViewerless(id) {
		t.Error("ExpireEdgeIfViewerless removed a hub with a live publisher")
	}
	r.handleGraceExpiry(id, 1)
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("a stale grace callback killed a live broadcast: %v", err)
	}

	// ...and the kill still goes through, on the very same hub.
	if !r.TerminateBroadcast(id, terminateCode, "terminated by operator") {
		t.Fatal("TerminateBroadcast = false on the live broadcast the guard protected")
	}
}

// A killed publisher's deferred Close must not arm a grace timer: the hub is
// gone, and a timer keyed on the ID could expire a DIFFERENT hub registered
// under it later (an edge pull, or a re-mint after the cooldown).
func TestTerminatedPublisherCloseArmsNoGraceTimer(t *testing.T) {
	var graced []string
	r := NewRegistry(discardLog, Options{
		BroadcastGrace:    time.Hour,
		OnPublisherClosed: func(id string) { graced = append(graced, id) },
	})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if !r.TerminateBroadcast(id, terminateCode, "terminated by operator") {
		t.Fatal("TerminateBroadcast = false")
	}
	pub.Close() // what the session handler does when its session dies

	// The publisher was deposed by the kill, so its Close is a no-op: no
	// grace timer, and no lease grace stamp racing the lease DELETE the kill
	// already issued.
	if len(graced) != 0 {
		t.Errorf("OnPublisherClosed fired for a terminated publisher: %v", graced)
	}
	r.mu.Lock()
	_, stillRegistered := r.hubs[id]
	r.mu.Unlock()
	if stillRegistered {
		t.Error("the terminated hub came back")
	}
}
