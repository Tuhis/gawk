package hub

// R9 observability tests (docs/13 M2+M3): ingress-loss window semantics,
// keyframe drop reasons, sendErrors/egress-bytes exposure, and the
// per-subscriber /statusz detail.

import (
	"errors"
	"testing"
	"time"
)

// singleBroadcastStats returns the Stats of the only broadcast in the registry.
func singleBroadcastStats(t *testing.T, r *Registry) Stats {
	t.Helper()
	rs := r.Stats()
	if len(rs.Broadcasts) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(rs.Broadcasts))
	}
	for _, s := range rs.Broadcasts {
		return s
	}
	panic("unreachable")
}

func TestIngressWindowSequentialNoLoss(t *testing.T) {
	var w ingressWindow
	var fl, cl uint64
	for id := uint32(1); id <= 3000; id++ {
		f, c := w.observeChunk(id, 0, 1)
		fl += f
		cl += c
	}
	if fl != 0 || cl != 0 {
		t.Errorf("losses = %d frames / %d chunks, want 0/0", fl, cl)
	}
}

func TestIngressWindowReorderWithinWindowNotLost(t *testing.T) {
	var w ingressWindow
	var fl, cl uint64
	observe := func(id uint32) {
		f, c := w.observeChunk(id, 0, 1)
		fl += f
		cl += c
	}
	// Frame 4 arrives late (after 5..8) but well inside the window.
	for _, id := range []uint32{1, 2, 3, 5, 6, 7, 8, 4} {
		observe(id)
	}
	// Slide the whole window past frame 4: nothing may count as lost.
	for id := uint32(9); id <= 8+2*ingressWindowFrames; id++ {
		observe(id)
	}
	if fl != 0 || cl != 0 {
		t.Errorf("losses = %d frames / %d chunks, want 0/0 (reorder is not loss)", fl, cl)
	}
}

func TestIngressWindowGapAgesOutAsLost(t *testing.T) {
	var w ingressWindow
	var fl uint64
	observe := func(id uint32) {
		f, _ := w.observeChunk(id, 0, 1)
		fl += f
	}
	observe(1)
	observe(2)
	observe(3)
	// Skip 4 forever.
	for id := uint32(5); id < 4+ingressWindowFrames; id++ {
		observe(id)
	}
	if fl != 0 {
		t.Fatalf("frame 4 counted lost before leaving the window (framesLost=%d)", fl)
	}
	observe(4 + ingressWindowFrames) // evicts slot 4
	if fl != 1 {
		t.Errorf("framesLost = %d, want 1 after the gap aged out", fl)
	}
}

func TestIngressWindowMissingAndDuplicateChunks(t *testing.T) {
	var w ingressWindow
	var fl, cl uint64
	observe := func(id uint32, idx, count int) {
		f, c := w.observeChunk(id, idx, count)
		fl += f
		cl += c
	}
	// Frame 1: chunks 0 and 2 of 3, chunk 0 duplicated (dupes must not
	// masquerade as coverage). Frame 2 complete.
	observe(1, 0, 3)
	observe(1, 0, 3)
	observe(1, 2, 3)
	observe(2, 0, 1)
	for id := uint32(3); id <= 2+ingressWindowFrames; id++ {
		observe(id, 0, 1)
	}
	if fl != 0 {
		t.Errorf("framesLost = %d, want 0 (frame 1 arrived, just incomplete)", fl)
	}
	if cl != 1 {
		t.Errorf("chunksLost = %d, want 1 (chunk 1 of frame 1)", cl)
	}
}

func TestIngressWindowWraparound(t *testing.T) {
	var w ingressWindow
	var fl uint64
	observe := func(id uint32) {
		f, _ := w.observeChunk(id, 0, 1)
		fl += f
	}
	var start uint32 = ^uint32(0) - 99 // 100 frames before the uint32 wrap
	id := start
	for i := 0; i < 50; i++ {
		observe(id)
		id++
	}
	skipped := id // one frame skipped across the wrap region
	id++
	for i := 0; i < 2*ingressWindowFrames; i++ {
		observe(id)
		id++
	}
	if fl != 1 {
		t.Errorf("framesLost = %d, want exactly the one frame (%d) skipped across the wrap", fl, skipped)
	}
}

func TestIngressWindowJumpBeyondWindowDoesNotInventLosses(t *testing.T) {
	var w ingressWindow
	var fl uint64
	observe := func(id uint32) {
		f, _ := w.observeChunk(id, 0, 1)
		fl += f
	}
	observe(1)
	observe(2)
	// A jump far beyond the window (corrupt header or a huge stall): the
	// untracked span must not be scored as millions of lost frames.
	observe(1_000_000)
	if fl != 0 {
		t.Errorf("framesLost = %d, want 0 after a beyond-window jump", fl)
	}
	// And tracking resumes normally at the new position.
	for id := uint32(1_000_001); id <= 1_000_000+ingressWindowFrames+10; id++ {
		observe(id)
	}
	if fl != 0 {
		t.Errorf("framesLost = %d, want 0 for the contiguous run after the jump", fl)
	}
}

func TestIngressLossViaPublisherAndRestartReset(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	// Frames 1..3 as delta datagrams, frame 4 as a keyframe stream (both
	// ingest paths feed the window), frame 5 skipped, then slide past it.
	for fid := uint32(1); fid <= 3; fid++ {
		pub.HandleDatagram(chunkDgram(t, false, fid, 0, 1, "d"))
	}
	ingestKeyframe(t, pub, keyframeMsg(t, 4, "vp8", "kf"))
	for fid := uint32(6); fid <= 5+ingressWindowFrames; fid++ {
		pub.HandleDatagram(chunkDgram(t, false, fid, 0, 1, "d"))
	}

	st := singleBroadcastStats(t, r)
	if st.IngressFramesLost != 1 {
		t.Errorf("IngressFramesLost = %d, want 1 (frame 5)", st.IngressFramesLost)
	}
	if st.IngressChunksLost != 0 {
		t.Errorf("IngressChunksLost = %d, want 0", st.IngressChunksLost)
	}
	if st.IngressDatagramBytes == 0 {
		t.Errorf("IngressDatagramBytes = 0, want > 0")
	}

	// Publisher restart: frameIDs reset to 1. The window must reset with it
	// (no phantom losses from the old sequence) while the cumulative counter
	// survives.
	pub.Close()
	if _, pub, err = r.StartPublish(id); err != nil {
		t.Fatalf("StartPublish reclaim: %v", err)
	}
	for fid := uint32(1); fid <= 2*ingressWindowFrames; fid++ {
		pub.HandleDatagram(chunkDgram(t, false, fid, 0, 1, "d"))
	}
	st = singleBroadcastStats(t, r)
	if st.IngressFramesLost != 1 {
		t.Errorf("IngressFramesLost after restart = %d, want still 1", st.IngressFramesLost)
	}
}

func TestKeyframeDropReasons(t *testing.T) {
	t.Run("bandwidth", func(t *testing.T) {
		// A 1-byte/s budget rejects any keyframe; the drop must land under the
		// keyframe bandwidth reason, NOT the datagram bandwidth counter.
		r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 1})
		id, pub, _ := r.StartPublish("")
		f := &fakeSender{}
		sub, err := r.Subscribe(id, f)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		ingestKeyframe(t, pub, keyframeMsg(t, 1, "vp8", "kf-payload"))

		st := singleBroadcastStats(t, r)
		if st.KeyframeDrops.Bandwidth != 1 {
			t.Errorf("KeyframeDrops.Bandwidth = %d, want 1", st.KeyframeDrops.Bandwidth)
		}
		if st.BandwidthDroppedDatagrams != 0 {
			t.Errorf("BandwidthDroppedDatagrams = %d, want 0 (keyframe drops are not datagram drops)", st.BandwidthDroppedDatagrams)
		}
		if st.BandwidthDroppedBytes == 0 {
			t.Errorf("BandwidthDroppedBytes = 0, want the dropped keyframe's bytes")
		}
	})

	t.Run("open_failed", func(t *testing.T) {
		r := NewRegistry(discardLog, Options{})
		id, pub, _ := r.StartPublish("")
		f := &fakeSender{kfOpenErr: errors.New("stream limit reached")}
		sub, err := r.Subscribe(id, f)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()
		ingestKeyframe(t, pub, keyframeMsg(t, 1, "vp8", "kf"))

		st := singleBroadcastStats(t, r)
		if st.KeyframeDrops.OpenFailed != 1 {
			t.Errorf("KeyframeDrops.OpenFailed = %d, want 1", st.KeyframeDrops.OpenFailed)
		}
	})

	t.Run("slow", func(t *testing.T) {
		// An uncancelled write failure (deadline exceeded / peer error) is the
		// subscriber's fault: reason "slow".
		r := NewRegistry(discardLog, Options{KeyframeWriteTimeout: 50 * time.Millisecond})
		id, pub, _ := r.StartPublish("")
		f := &fakeSender{kfWriteErr: errors.New("deadline exceeded")}
		sub, err := r.Subscribe(id, f)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		ingestKeyframe(t, pub, keyframeMsg(t, 1, "vp8", "kf"))
		waitFor(t, 5*time.Second, func() bool {
			return singleBroadcastStats(t, r).KeyframeDrops.Slow == 1
		}, "slow keyframe drop to be counted")
		sub.Close()
	})

	t.Run("superseded", func(t *testing.T) {
		r := NewRegistry(discardLog, Options{})
		id, pub, _ := r.StartPublish("")
		f := &fakeSender{kfBlock: make(chan struct{})}
		sub, err := r.Subscribe(id, f)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		// Keyframe 1 blocks in flight; keyframe 2 supersedes it.
		ingestKeyframe(t, pub, keyframeMsg(t, 1, "vp8", "kf1"))
		ingestKeyframe(t, pub, keyframeMsg(t, 2, "vp8", "kf2"))
		close(f.kfBlock)
		waitKeyframes(t, f, 1)

		waitFor(t, 5*time.Second, func() bool {
			st := singleBroadcastStats(t, r)
			return st.KeyframeDrops.Superseded == 1 && st.KeyframeStreamsSent == 1
		}, "superseded drop + delivered keyframe to be counted")
		st := singleBroadcastStats(t, r)
		if st.KeyframeStreamsDropped != st.KeyframeDrops.Total() {
			t.Errorf("aggregate KeyframeStreamsDropped = %d, want sum of reasons %d",
				st.KeyframeStreamsDropped, st.KeyframeDrops.Total())
		}
		sub.Close()
	})
}

func TestSendErrorsAndEgressBytesExposed(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, _ := r.StartPublish("")

	ok := &fakeSender{}
	failing := &fakeSender{err: errors.New("send failed")}
	subOK, err := r.Subscribe(id, ok)
	if err != nil {
		t.Fatalf("Subscribe ok: %v", err)
	}
	subFail, err := r.Subscribe(id, failing)
	if err != nil {
		t.Fatalf("Subscribe failing: %v", err)
	}

	d := chunkDgram(t, false, 1, 0, 1, "delta")
	pub.HandleDatagram(d)
	kf := keyframeMsg(t, 2, "vp8", "kf")
	ingestKeyframe(t, pub, kf)
	waitKeyframes(t, ok, 1)
	waitFor(t, 5*time.Second, func() bool { return len(ok.received()) == 1 }, "delta delivered")
	waitFor(t, 5*time.Second, func() bool {
		return singleBroadcastStats(t, r).SendErrors >= 1
	}, "send error to be counted")

	st := singleBroadcastStats(t, r)
	if st.EgressDatagramBytes != uint64(len(d)) {
		t.Errorf("EgressDatagramBytes = %d, want %d (only the successful send)", st.EgressDatagramBytes, len(d))
	}
	// The failing subscriber also received the keyframe attempt: its fake
	// stream write succeeds (kfWriteErr unset), so both got the keyframe.
	waitKeyframes(t, failing, 1)
	waitFor(t, 5*time.Second, func() bool {
		return singleBroadcastStats(t, r).EgressKeyframeBytes == uint64(2*len(kf))
	}, "keyframe egress bytes to be counted")

	// Folding: closing subscribers must not lose any of it.
	subOK.Close()
	subFail.Close()
	st = singleBroadcastStats(t, r)
	if st.SendErrors != 1 {
		t.Errorf("SendErrors after close = %d, want 1", st.SendErrors)
	}
	if st.EgressDatagramBytes != uint64(len(d)) || st.EgressKeyframeBytes != uint64(2*len(kf)) {
		t.Errorf("egress bytes after close = %d/%d, want %d/%d",
			st.EgressDatagramBytes, st.EgressKeyframeBytes, len(d), 2*len(kf))
	}
	rs := r.Stats()
	if rs.Totals.SendErrors != 1 || rs.Totals.EgressKeyframeBytes != uint64(2*len(kf)) {
		t.Errorf("totals = %+v, want sendErrors 1 and egressKeyframeBytes %d", rs.Totals, 2*len(kf))
	}
	pub.Close()
}

func TestSubscriberDetailsInStats(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, _ := r.StartPublish("")
	defer pub.Close()

	f1, f2 := &fakeSender{}, &fakeSender{}
	s1, err := r.Subscribe(id, f1)
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	defer s1.Close()
	s2, err := r.Subscribe(id, f2)
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	defer s2.Close()

	st := singleBroadcastStats(t, r)
	if len(st.SubscriberDetails) != 2 {
		t.Fatalf("SubscriberDetails = %d entries, want 2", len(st.SubscriberDetails))
	}
	k1, k2 := st.SubscriberDetails[0].Key, st.SubscriberDetails[1].Key
	if k1 == "" || k2 == "" || k1 == k2 {
		t.Errorf("subscriber keys = %q, %q: want non-empty and distinct", k1, k2)
	}

	// Keys are stable across polls (that's what makes a slow viewer
	// watchable across /statusz refreshes).
	again := singleBroadcastStats(t, r)
	got := map[string]bool{}
	for _, d := range again.SubscriberDetails {
		got[d.Key] = true
	}
	if !got[k1] || !got[k2] {
		t.Errorf("keys changed across polls: first %q/%q, then %v", k1, k2, got)
	}
}
