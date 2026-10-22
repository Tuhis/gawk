package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R74 (docs/76 item 18): the edge pump's uni-stream loop classifies each
// stream by its prologue. An R73 carrier (the PoP downlink's /pop/subscribe
// leg writer, docs/75 D1) goes to Publisher.IngestUplinkCarrier and gives
// its keyframe slot back; anything else is the keyframe reader, byte for
// byte; and a stream refused for want of a slot is reset, not left
// unread.

// edgeTestStream is an upstream uni stream: a pipe the test writes, with the
// CancelRead a *webtransport.ReceiveStream has. Writes and the FIN run in
// order on one goroutine, since a pipe write blocks until it is read.
type edgeTestStream struct {
	*io.PipeReader
	ops       chan func(*io.PipeWriter)
	once      sync.Once
	cancelled chan struct{}
}

func newEdgeTestStream() *edgeTestStream {
	r, w := io.Pipe()
	s := &edgeTestStream{PipeReader: r, ops: make(chan func(*io.PipeWriter), 8), cancelled: make(chan struct{})}
	go func() {
		for op := range s.ops {
			op(w)
		}
	}()
	return s
}

func (s *edgeTestStream) CancelRead(webtransport.StreamErrorCode) {
	s.once.Do(func() {
		close(s.cancelled)
		_ = s.PipeReader.CloseWithError(errors.New("stream reset by reader"))
	})
}

func (s *edgeTestStream) isCancelled() bool {
	select {
	case <-s.cancelled:
		return true
	default:
		return false
	}
}

func (s *edgeTestStream) write(p []byte) {
	s.ops <- func(w *io.PipeWriter) { _, _ = w.Write(p) }
}

// finish sends the FIN after every queued write.
func (s *edgeTestStream) finish() {
	s.ops <- func(w *io.PipeWriter) { _ = w.Close() }
	close(s.ops)
}

type edgeStreamsRig struct {
	registry *hub.Registry
	id       string
	pub      *hub.Publisher
	up       *fakeUpstream
	viewer   *edgeConn
	done     chan error
}

// newEdgeStreamsRig runs acceptStreams on a fresh edge hub, with one viewer.
func newEdgeStreamsRig(t *testing.T) *edgeStreamsRig {
	t.Helper()
	h := newEdgeHarness(t)
	id, pub, err := h.registry.EdgePublish("ABCDEF")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	rig := &edgeStreamsRig{
		registry: h.registry,
		id:       id,
		pub:      pub,
		up:       newFakeUpstream(),
		viewer:   &edgeConn{},
		done:     make(chan error, 1),
	}
	sub, err := h.registry.Subscribe(id, rig.viewer)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	es := newEdgeSession(h.manager, id)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { rig.done <- es.acceptStreams(ctx, rig.up, pub) }()
	t.Cleanup(func() {
		cancel()
		_ = rig.up.Close()
		select {
		case <-rig.done:
		case <-time.After(5 * time.Second):
			t.Error("acceptStreams did not return")
		}
		sub.Close()
	})
	return rig
}

func (rig *edgeStreamsRig) offer(st *edgeTestStream) { rig.up.streams <- st }

func (rig *edgeStreamsRig) stats(t *testing.T) hub.Stats {
	t.Helper()
	st, ok := rig.registry.Stats().Broadcasts[rig.registry.ObfuscateID(rig.id)]
	if !ok {
		t.Fatal("broadcast missing from stats")
	}
	return st
}

func (rig *edgeStreamsRig) hasDgram(want []byte) bool {
	rig.viewer.mu.Lock()
	defer rig.viewer.mu.Unlock()
	for _, d := range rig.viewer.dgrams {
		if bytes.Equal(d, want) {
			return true
		}
	}
	return false
}

func edgeCarrier(t *testing.T, records ...[]byte) []byte {
	t.Helper()
	b := wire.AppendCarrierPrologue(nil)
	for _, rec := range records {
		var err error
		if b, err = wire.AppendCarrierRecord(b, rec); err != nil {
			t.Fatalf("AppendCarrierRecord: %v", err)
		}
	}
	return b
}

func edgeDelta(t *testing.T, frameID uint32) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
		FrameID: frameID, ChunkIndex: 0, ChunkCount: 1, TimestampUs: uint64(frameID) * 16_667,
	}, []byte("delta"))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	return d
}

// A keyframe stream still goes to the keyframe reader, byte for byte: the
// classification peeks the prologue and replays it.
func TestEdgeStreamsKeyframeStreamIngestedByteIdentical(t *testing.T) {
	rig := newEdgeStreamsRig(t)
	msg := buildStreamKeyframe(t, 7, "avc1.42E02A", 2000)
	st := newEdgeTestStream()
	rig.offer(st)
	st.write(msg)
	st.finish()
	waitFor(t, 5*time.Second, func() bool { return rig.viewer.keyframeCount() == 1 }, "keyframe fanned out")
	rig.viewer.mu.Lock()
	got := rig.viewer.keyframes[0]
	rig.viewer.mu.Unlock()
	if !bytes.Equal(got, msg) {
		t.Fatalf("fanned-out keyframe differs from the upstream bytes (%d vs %d bytes)", len(got), len(msg))
	}
	if st.isCancelled() {
		t.Error("a good keyframe stream was reset")
	}
}

// An R73 carrier is read by IngestUplinkCarrier: its records reach the
// viewer as the datagrams they replace.
func TestEdgeStreamsCarrierIngested(t *testing.T) {
	rig := newEdgeStreamsRig(t)
	kf := newEdgeTestStream()
	rig.offer(kf)
	kf.write(buildStreamKeyframe(t, 10, "avc1.42E02A", 100))
	kf.finish()
	waitFor(t, 5*time.Second, func() bool { return rig.viewer.keyframeCount() == 1 }, "keyframe")

	d11, d12 := edgeDelta(t, 11), edgeDelta(t, 12)
	car := newEdgeTestStream()
	rig.offer(car)
	car.write(edgeCarrier(t, d11, d12))
	car.finish()
	waitFor(t, 5*time.Second, func() bool { return rig.hasDgram(d11) && rig.hasDgram(d12) }, "carrier records fanned out as datagrams")
	if st := rig.stats(t); st.UplinkCarriers != 1 || st.UplinkRecords != 2 {
		t.Errorf("uplink carriers=%d records=%d, want 1 and 2", st.UplinkCarriers, st.UplinkRecords)
	}
	if car.isCancelled() {
		t.Error("a carrier that ended with FIN was reset")
	}
}

// A carrier lives for a GOP, so it gives back the keyframe slot it was
// classified in: with a carrier still open, maxConcurrentKeyframeStreams
// keyframe streams are all admitted.
func TestEdgeStreamsCarrierFreesItsSlot(t *testing.T) {
	rig := newEdgeStreamsRig(t)
	car := newEdgeTestStream()
	rig.offer(car)
	car.write(wire.AppendCarrierPrologue(nil)) // and nothing more: still open
	waitFor(t, 5*time.Second, func() bool { return rig.stats(t).UplinkCarriers == 1 }, "carrier classified")

	var kfs []*edgeTestStream
	for range maxConcurrentKeyframeStreams {
		st := newEdgeTestStream()
		kfs = append(kfs, st)
		rig.offer(st) // never written: each holds its slot
	}
	// One more, to prove the loop has taken every stream above.
	probe := newEdgeTestStream()
	rig.offer(probe)
	waitFor(t, 5*time.Second, probe.isCancelled, "the stream past the limit to be refused")
	for i, st := range kfs {
		if st.isCancelled() {
			t.Errorf("keyframe stream %d refused: the open carrier kept its slot", i)
		}
	}
	if car.isCancelled() {
		t.Error("the open carrier was reset")
	}
	for _, st := range kfs {
		st.finish()
	}
	car.finish()
}

// A stream refused for want of a slot is reset (CancelRead), not left
// unread holding flow-control credit.
func TestEdgeStreamsRefusedStreamIsReset(t *testing.T) {
	rig := newEdgeStreamsRig(t)
	var held []*edgeTestStream
	for range maxConcurrentKeyframeStreams {
		st := newEdgeTestStream()
		held = append(held, st)
		rig.offer(st)
	}
	extra := newEdgeTestStream()
	rig.offer(extra)
	waitFor(t, 5*time.Second, extra.isCancelled, "the refused stream to be reset")
	for i, st := range held {
		if st.isCancelled() {
			t.Errorf("admitted stream %d was reset", i)
		}
		st.finish()
	}
}

// A carrier the hub refuses (over MaxUplinkCarriers) or ends (a malformed
// record) is reset; so is a keyframe stream the keyframe reader rejects.
func TestEdgeStreamsFailedIngestIsReset(t *testing.T) {
	rig := newEdgeStreamsRig(t)
	var open []*edgeTestStream
	for range hub.MaxUplinkCarriers {
		st := newEdgeTestStream()
		open = append(open, st)
		rig.offer(st)
		st.write(wire.AppendCarrierPrologue(nil))
	}
	waitFor(t, 5*time.Second, func() bool { return rig.stats(t).UplinkCarriers == hub.MaxUplinkCarriers }, "carriers open")
	over := newEdgeTestStream()
	rig.offer(over)
	over.write(wire.AppendCarrierPrologue(nil))
	waitFor(t, 5*time.Second, over.isCancelled, "the carrier over the limit to be reset")

	bad := newEdgeTestStream()
	rig.offer(bad)
	// A ViewerCount is never a carrier record (docs/75 D1).
	bad.write(edgeCarrier(t, wire.AppendViewerCount(nil, 3)))
	waitFor(t, 5*time.Second, bad.isCancelled, "the carrier with a malformed record to be reset")

	junk := newEdgeTestStream()
	rig.offer(junk)
	junk.write([]byte{0xff, 0xff, 0xff, 0xff})
	junk.finish()
	waitFor(t, 5*time.Second, junk.isCancelled, "the unreadable keyframe stream to be reset")

	for _, st := range open {
		st.finish()
	}
}
