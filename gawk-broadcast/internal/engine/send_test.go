package engine

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A minimal Annex-B access unit. sps/pps/idr mirror what h264parse
// config-interval=-1 puts in front of every IDR.
var (
	testSPS = []byte{0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x2a, 0x99, 0x11}
	testPPS = []byte{0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80}
	testIDR = []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00}
	testP   = []byte{0, 0, 0, 1, 0x41, 0x9a, 0x22, 0x11}
)

func keyframeAU() []byte {
	return bytes.Join([][]byte{testSPS, testPPS, testIDR}, nil)
}

func newTestSender(sess RelaySession) *sender {
	return newSender(sess, &FakeClock{}, testLog)
}

// The keyframe cadence is measured, not assumed: key-int-max is a *frame*
// count derived from the nominal fps, so when damage-driven capture runs
// under the nominal rate the wall-clock GOP stretches proportionally
// (observed 2026-07-17: ~650 ms at ~40 fps against the 500 ms target). The
// stat is what lets a broadcaster see that without instrumenting a viewer.
func TestKeyframeCadenceIsMeasured(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)

	if st := s.stats(); st.KeyframeIntervalAvailable {
		t.Fatal("cadence available before any keyframe interval exists")
	}
	// Three keyframes, 700 ms apart on the arrival clock.
	for i, ts := range []uint64{1_000_000, 1_700_000, 2_400_000} {
		s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: ts})
		if i == 0 {
			if st := s.stats(); st.KeyframeIntervalAvailable {
				t.Fatal("one keyframe is not an interval")
			}
		}
	}
	st := s.stats()
	if !st.KeyframeIntervalAvailable {
		t.Fatal("cadence not available after three keyframes")
	}
	if st.KeyframeIntervalMs < 699 || st.KeyframeIntervalMs > 701 {
		t.Errorf("KeyframeIntervalMs = %.1f, want ≈700", st.KeyframeIntervalMs)
	}
}

// Deltas must round-trip: what the viewer's reassembler puts back together has
// to be exactly what the encoder produced.
func TestDeltaChunkingRoundTrips(t *testing.T) {
	for _, size := range []int{0, 1, wire.MaxChunkPayload - 1, wire.MaxChunkPayload, wire.MaxChunkPayload + 1, 100_000} {
		sess := newFakeSession()
		s := newTestSender(sess)
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		s.send(AccessUnit{Data: payload, TimestampUs: 4242})

		dgrams := sess.sentDatagrams()
		if len(dgrams) == 0 {
			t.Fatalf("size %d: no datagrams sent", size)
		}
		var got []byte
		for i, d := range dgrams {
			h, chunk, err := wire.ParseVideoChunk(d)
			if err != nil {
				t.Fatalf("size %d: chunk %d does not parse: %v", size, i, err)
			}
			if h.Keyframe {
				t.Errorf("size %d: delta chunk marked keyframe", size)
			}
			if int(h.ChunkIndex) != i || int(h.ChunkCount) != len(dgrams) {
				t.Errorf("size %d: chunk %d/%d, want %d/%d", size, h.ChunkIndex, h.ChunkCount, i, len(dgrams))
			}
			if h.TimestampUs != 4242 {
				t.Errorf("size %d: timestamp = %d, want 4242", size, h.TimestampUs)
			}
			got = append(got, chunk...)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("size %d: reassembled %d bytes, want %d", size, len(got), len(payload))
		}
		// A zero-length frame still exists on the wire (mirrors packetizer.ts).
		if size == 0 && len(dgrams) != 1 {
			t.Errorf("empty frame produced %d datagrams, want 1", len(dgrams))
		}
	}
}

// Decision 12: a partial frame is dead weight the viewer's reassembler
// discards anyway, so the remaining chunks are not worth uplink.
func TestMidFrameSendFailureDropsRemainder(t *testing.T) {
	sess := newFakeSession()
	sess.sendErrAfter = 3 // three chunks land, then the queue is full
	s := newTestSender(sess)

	payload := make([]byte, wire.MaxChunkPayload*10) // 10 chunks
	s.send(AccessUnit{Data: payload, TimestampUs: 1})

	if n := len(sess.sentDatagrams()); n != 3 {
		t.Errorf("sent %d chunks after a mid-frame failure, want 3 (the rest are dead weight)", n)
	}
	st := s.stats()
	if st.FramesDroppedAtSend != 1 {
		t.Errorf("FramesDroppedAtSend = %d, want 1", st.FramesDroppedAtSend)
	}
	if st.SentFrames != 0 {
		t.Errorf("SentFrames = %d, want 0 — the frame did not actually leave", st.SentFrames)
	}
}

// Decision 12: DatagramTooLargeError is not this frame's fault — the path MTU
// is smaller than assumed. Shrink and re-chunk rather than drop (the Go-side
// analogue of docs/11's Firefox path-MTU fix).
func TestDatagramTooLargeRechunksRatherThanDrops(t *testing.T) {
	const pathMax = 600
	sess := newFakeSession()
	s := newTestSender(sess)

	// Fail anything above the real path MTU, like quic-go does.
	rejected := 0
	sess.sendFunc = func(b []byte) error {
		if len(b) > pathMax {
			rejected++
			return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: pathMax}
		}
		return nil
	}

	payload := make([]byte, 2000)
	s.send(AccessUnit{Data: payload, TimestampUs: 7})

	if rejected == 0 {
		t.Fatal("test did not exercise the too-large path")
	}
	st := s.stats()
	if st.FramesDroppedAtSend != 0 {
		t.Errorf("FramesDroppedAtSend = %d, want 0 — the frame should have been re-chunked, not dropped", st.FramesDroppedAtSend)
	}
	if st.SentFrames != 1 {
		t.Errorf("SentFrames = %d, want 1", st.SentFrames)
	}
	// Every delivered datagram now fits the discovered MTU...
	var got []byte
	for _, d := range sess.sentDatagrams() {
		if len(d) > pathMax {
			t.Fatalf("datagram of %d bytes exceeds the discovered path max %d", len(d), pathMax)
		}
		_, chunk, err := wire.ParseVideoChunk(d)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk...)
	}
	// ...and the frame is still whole.
	if len(got) != len(payload) {
		t.Errorf("reassembled %d bytes, want %d", len(got), len(payload))
	}
	// The smaller size sticks: the next frame must not rediscover it.
	before := rejected
	s.send(AccessUnit{Data: make([]byte, 2000), TimestampUs: 8})
	if rejected != before {
		t.Errorf("path MTU was rediscovered on the next frame (%d new rejections); the shrink must stick", rejected-before)
	}
}

// The relay's R9 ingress-loss window compares frame IDs serially, so the
// counter must wrap at uint32 exactly like the browser's nextFrameId.
func TestFrameIDsStartAtZeroAndWrap(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)

	s.send(AccessUnit{Data: testP, TimestampUs: 1})
	h, _, err := wire.ParseVideoChunk(sess.sentDatagrams()[0])
	if err != nil {
		t.Fatal(err)
	}
	if h.FrameID != 0 {
		t.Errorf("first frame ID = %d, want 0", h.FrameID)
	}

	s.nextFrameID = 0xffffffff
	s.send(AccessUnit{Data: testP, TimestampUs: 2})
	s.send(AccessUnit{Data: testP, TimestampUs: 3})
	dgrams := sess.sentDatagrams()
	last := dgrams[len(dgrams)-2:]
	ids := make([]uint32, 0, 2)
	for _, d := range last {
		h, _, err := wire.ParseVideoChunk(d)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h.FrameID)
	}
	if ids[0] != 0xffffffff || ids[1] != 0 {
		t.Errorf("frame IDs around the wrap = %v, want [4294967295 0]", ids)
	}
}

// Keyframes go whole on a reliable stream, carrying their config so a
// delivered keyframe is self-sufficient for the relay's late-joiner priming.
func TestKeyframeGoesOnStreamWithEmbeddedConfig(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)

	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 99})
	waitForCond(t, func() bool { return len(sess.sendStreams()) == 1 }, "one keyframe stream")

	// Nothing on datagrams: a keyframe never rides them.
	if n := len(sess.sentDatagrams()); n != 0 {
		t.Errorf("%d datagrams sent for a keyframe, want 0", n)
	}

	msg := waitForStreamBytes(t, sess.sendStreams()[0])
	hdr, err := wire.ParseStreamFrameHeader(msg)
	if err != nil {
		t.Fatalf("stream frame header: %v", err)
	}
	if !hdr.Keyframe || hdr.FrameID != 0 || hdr.TimestampUs != 99 {
		t.Errorf("header = %+v, want keyframe frame 0 at ts 99", hdr)
	}
	if hdr.ConfigLen == 0 {
		t.Fatal("keyframe carries no config; a primed late joiner could not decode it")
	}
	cfgStart := wire.StreamFrameHeaderSize
	cfg, err := wire.ParseDecoderConfig(msg[cfgStart : cfgStart+int(hdr.ConfigLen)])
	if err != nil {
		t.Fatalf("embedded config: %v", err)
	}
	if cfg.Codec != "avc1.42E02A" {
		t.Errorf("codec = %q, want avc1.42E02A (parsed from the SPS, not assumed)", cfg.Codec)
	}
	// The Annex-B path never builds an avcC record.
	if len(cfg.Extradata) != 0 {
		t.Errorf("extradata = %x, want empty on the Annex-B path", cfg.Extradata)
	}
	payload := msg[cfgStart+int(hdr.ConfigLen):]
	if !bytes.Equal(payload, keyframeAU()) {
		t.Error("keyframe payload does not match the access unit")
	}
}

// highProfileKeyframeAU is a keyframe from a *different* encoder lineage: the
// same shape, a different SPS profile/level, so the codec string parsed out of
// it differs from keyframeAU's.
func highProfileKeyframeAU() []byte {
	sps := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x28, 0x99, 0x11}
	return bytes.Join([][]byte{sps, testPPS, testIDR}, nil)
}

// configCodec reads the codec string out of the config embedded in a keyframe
// stream — what a late joiner primed from the relay's cache actually gets.
func configCodec(t *testing.T, str *fakeSendStream) string {
	t.Helper()
	msg := waitForStreamBytes(t, str)
	hdr, err := wire.ParseStreamFrameHeader(msg)
	if err != nil {
		t.Fatalf("stream frame header: %v", err)
	}
	if hdr.ConfigLen == 0 {
		t.Fatal("keyframe carries no config")
	}
	cfg, err := wire.ParseDecoderConfig(msg[wire.StreamFrameHeaderSize : wire.StreamFrameHeaderSize+int(hdr.ConfigLen)])
	if err != nil {
		t.Fatalf("embedded config: %v", err)
	}
	return cfg.Codec
}

// The DecoderConfig is derived once and cached for the whole broadcast, which
// is right while one child owns the encode — and wrong the moment capture is
// rebuilt under a live session (gst's mid-session restart). A rebuild can land
// on a different rung, or a different encoder entirely, and the codec string is
// the *only* thing telling a viewer's decoder what it is about to receive on
// this Annex-B path. So the first frame of a rebuilt pipeline carries the
// epoch, and the epoch is what re-derives.
func TestARebuiltPipelineReAdvertisesItsDecoderConfig(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)

	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 1})
	waitForCond(t, func() bool { return len(sess.sendStreams()) == 1 }, "the first keyframe stream")
	if got := configCodec(t, sess.sendStreams()[0]); got != "avc1.42E02A" {
		t.Fatalf("codec = %q, want avc1.42E02A", got)
	}

	// An unmarked keyframe never re-derives: within one pipeline the config is
	// settled, and re-parsing every keyframe would be work with no question
	// behind it.
	s.send(AccessUnit{Data: highProfileKeyframeAU(), Keyframe: true, TimestampUs: 2})
	waitForCond(t, func() bool { return len(sess.sendStreams()) == 2 }, "the second keyframe stream")
	if got := configCodec(t, sess.sendStreams()[1]); got != "avc1.42E02A" {
		t.Errorf("codec = %q on an unmarked keyframe, want the cached avc1.42E02A", got)
	}

	// The first frame of a rebuilt pipeline does.
	s.send(AccessUnit{Data: highProfileKeyframeAU(), Keyframe: true, EncoderRestarted: true, TimestampUs: 3})
	waitForCond(t, func() bool { return len(sess.sendStreams()) == 3 }, "the rebuilt pipeline's first keyframe stream")
	if got := configCodec(t, sess.sendStreams()[2]); got != "avc1.640028" {
		t.Errorf("codec = %q after a rebuild, want avc1.640028 parsed from the new SPS — a viewer configured for the dead pipeline's codec decodes nothing",
			got)
	}
}

// Decision 12: ≤1 keyframe stream in flight. With a 500 ms GOP a stalled
// uplink would otherwise accumulate open streams toward stream-credit
// exhaustion — the publisher-side mirror of R10's zombie finding. The browser
// fire-and-forgets with no bound; fresh code does not reproduce that.
func TestStalledKeyframeIsSupersededByTheNext(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)

	// First keyframe stalls mid-write (a saturated uplink).
	sess.nextStreamStalls = true
	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 1})
	waitForCond(t, func() bool { return len(sess.sendStreams()) == 1 }, "the first keyframe stream to open")
	first := sess.sendStreams()[0]

	// The next keyframe arrives while it is still writing.
	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 2})
	waitForCond(t, func() bool { return first.wasCancelled() }, "the stalled keyframe to be cancelled")

	if n := len(sess.sendStreams()); n != 2 {
		t.Fatalf("%d streams opened, want 2", n)
	}
	waitForCond(t, func() bool { return s.stats().KeyframeStreamsSuperseded == 1 }, "a superseded count of 1")

	// The newest keyframe is the one that survives.
	waitForCond(t, func() bool { return s.stats().KeyframeStreamsSent == 1 }, "the newest keyframe to complete")
	s.wait()
}

// teardown() calls wait() while the pump goroutine may still be inside
// send(): cancelling the session context does not synchronously stop the
// pump. A keyframe write spawned in that window must either be waited for or
// not started at all — never started *behind* a Wait that has already
// returned, which is what "let in-flight writes settle before closing the
// session out from under them" exists to prevent.
//
// Concretely this is the sync.WaitGroup contract: a positive Add that starts
// from a zero counter must happen-before Wait. CI's race detector caught the
// violation (Add at send.go's keyframe spawn vs. Wait in sender.wait()).
func TestWaitDoesNotRaceAKeyframeArrivingDuringTeardown(t *testing.T) {
	for i := 0; i < 20; i++ {
		sess := newFakeSession()
		// The write stalls, so the keyframe is genuinely in flight when
		// teardown arrives: with instant writes the counter is never above
		// zero, wait() never parks, and the interleaving can't arise.
		sess.nextStreamStalls = true
		s := newTestSender(sess)

		// The pump delivers a keyframe. Nothing below synchronises with this
		// goroutine before wait() — that is the point: a channel receive or a
		// second spawn here would order the two and hide the bug.
		go s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 1})
		waitForCond(t, func() bool { return len(sess.sendStreams()) == 1 }, "the keyframe stream to open")
		first := sess.sendStreams()[0]
		time.Sleep(time.Millisecond) // let the write goroutine register

		// The uplink comes unstuck a moment into teardown, so wait() returns.
		go func() {
			time.Sleep(2 * time.Millisecond)
			first.CancelWrite(keyframeSupersededCode)
		}()

		s.wait()

		// Whatever wait() decided, no write may still be running once it has
		// returned and the caller has closed the session.
		s.wait()
		s.kfMu.Lock()
		inflight := s.inflight
		s.kfMu.Unlock()
		if inflight != nil {
			t.Fatalf("iteration %d: a keyframe stream is still in flight after wait()", i)
		}
	}
}

func TestKeyframeStreamOpenFailureIsCountedNotFatal(t *testing.T) {
	sess := newFakeSession()
	sess.openErr = errors.New("stream credit exhausted")
	s := newTestSender(sess)

	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 1})
	if st := s.stats(); st.KeyframeStreamsFailed != 1 {
		t.Errorf("KeyframeStreamsFailed = %d, want 1", st.KeyframeStreamsFailed)
	}
	// A later keyframe still gets tried: one stream failure is not the end.
	sess.mu.Lock()
	sess.openErr = nil
	sess.mu.Unlock()
	s.send(AccessUnit{Data: keyframeAU(), Keyframe: true, TimestampUs: 2})
	waitForCond(t, func() bool { return s.stats().KeyframeStreamsSent == 1 }, "the next keyframe to recover")
	s.wait()
}

// An oversized AU must be refused rather than allocated: the relay would
// reject it anyway, and MaxKeyframeBytes exists to bound exactly this.
func TestOversizedKeyframeIsRefused(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)
	s.send(AccessUnit{Data: make([]byte, wire.MaxKeyframeBytes+1), Keyframe: true, TimestampUs: 1})
	if st := s.stats(); st.KeyframeStreamsFailed != 1 {
		t.Errorf("KeyframeStreamsFailed = %d, want 1", st.KeyframeStreamsFailed)
	}
	if n := len(sess.sendStreams()); n != 0 {
		t.Errorf("opened %d streams for an oversized keyframe, want 0", n)
	}
}

// A frame needing more chunks than the wire permits is dropped, not truncated.
func TestFrameExceedingMaxChunkCountIsDropped(t *testing.T) {
	sess := newFakeSession()
	s := newTestSender(sess)
	s.send(AccessUnit{Data: make([]byte, wire.MaxChunkPayload*(wire.MaxChunkCount+1)), TimestampUs: 1})
	if st := s.stats(); st.FramesDroppedAtSend != 1 {
		t.Errorf("FramesDroppedAtSend = %d, want 1", st.FramesDroppedAtSend)
	}
	if n := len(sess.sentDatagrams()); n != 0 {
		t.Errorf("sent %d datagrams for an over-long frame, want 0", n)
	}
}

func waitForCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func waitForStreamBytes(t *testing.T, str *fakeSendStream) []byte {
	t.Helper()
	var msg []byte
	waitForCond(t, func() bool {
		msg = str.bytesWritten()
		return len(msg) > 0
	}, "bytes written to the keyframe stream")
	return msg
}
