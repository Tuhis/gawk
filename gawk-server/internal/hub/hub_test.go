package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSender records every datagram and close code, and every keyframe stream
// fully written to it. It implements hub.Conn.
type fakeSender struct {
	mu         sync.Mutex
	got        [][]byte // datagrams delivered
	keyframes  [][]byte // full keyframe messages delivered (Write+Close)
	block      chan struct{}
	kfBlock    chan struct{} // if non-nil, every keyframe Write blocks on it
	err        error
	kfOpenErr  error // if non-nil, OpenKeyframeStream fails
	kfWriteErr error // if non-nil, every keyframe Write fails (uncancelled: the "slow peer" shape)
	closeCode  uint32
	closed     bool
	kfOpens    int

	// R19 carrier support: every opened carrier stream is retained so tests
	// can inspect its bytes/state at any point in its life (a carrier is
	// long-lived, unlike a keyframe stream).
	carriers   []*fakeCarrierStream
	carOpenErr error         // if non-nil, OpenCarrierStream fails
	carBlock   chan struct{} // if non-nil, every carrier Write blocks on it (until deadline/cancel)
	carOpens   int
}

func (f *fakeSender) SendDatagram(d []byte) error {
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.got = append(f.got, d)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) OpenKeyframeStream() (KeyframeStream, error) {
	f.mu.Lock()
	f.kfOpens++
	oe := f.kfOpenErr
	f.mu.Unlock()
	if oe != nil {
		return nil, oe
	}
	return &fakeKeyframeStream{parent: f, cancel: make(chan struct{})}, nil
}

func (f *fakeSender) OpenCarrierStream() (KeyframeStream, error) {
	f.mu.Lock()
	f.carOpens++
	oe := f.carOpenErr
	f.mu.Unlock()
	if oe != nil {
		return nil, oe
	}
	st := &fakeCarrierStream{parent: f, cancel: make(chan struct{})}
	f.mu.Lock()
	f.carriers = append(f.carriers, st)
	f.mu.Unlock()
	return st, nil
}

func (f *fakeSender) CloseWithError(code uint32, reason string) error {
	f.mu.Lock()
	f.closeCode = code
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.got...)
}

func (f *fakeSender) receivedKeyframes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.keyframes...)
}

func (f *fakeSender) getCloseInfo() (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode, f.closed
}

func (f *fakeSender) setKfOpenErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kfOpenErr = err
}

func (f *fakeSender) setKfWriteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kfWriteErr = err
}

func (f *fakeSender) getKfWriteErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kfWriteErr
}

func (f *fakeSender) keyframeCount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.keyframes))
}

// fakeKeyframeStream is one keyframe delivery to a fakeSender. Write optionally
// blocks on the parent's kfBlock channel and is unblocked by CancelWrite (its
// per-stream cancel channel), mirroring how a real SendStream.CancelWrite
// aborts an in-flight Write.
type fakeKeyframeStream struct {
	parent *fakeSender
	buf    []byte
	cancel chan struct{}
	once   sync.Once
}

func (k *fakeKeyframeStream) SetWriteDeadline(time.Time) error { return nil }

func (k *fakeKeyframeStream) Write(p []byte) (int, error) {
	if err := k.parent.getKfWriteErr(); err != nil {
		return 0, err
	}
	if k.parent.kfBlock != nil {
		select {
		case <-k.parent.kfBlock:
		case <-k.cancel:
			return 0, errors.New("keyframe stream cancelled")
		}
	}
	select {
	case <-k.cancel:
		return 0, errors.New("keyframe stream cancelled")
	default:
	}
	k.buf = append(k.buf, p...)
	return len(p), nil
}

func (k *fakeKeyframeStream) Close() error {
	select {
	case <-k.cancel:
		return errors.New("keyframe stream cancelled")
	default:
	}
	k.parent.mu.Lock()
	k.parent.keyframes = append(k.parent.keyframes, append([]byte(nil), k.buf...))
	k.parent.mu.Unlock()
	return nil
}

func (k *fakeKeyframeStream) CancelWrite() {
	k.once.Do(func() { close(k.cancel) })
}

// fakeCarrierStream is one R19 carrier stream: long-lived, written to
// incrementally by the subscriber drain, closed on rotation. Write honors the
// deadline set by SetWriteDeadline while blocked on the parent's carBlock —
// mirroring how a real SendStream write unblocks with an error when its
// deadline passes.
type fakeCarrierStream struct {
	parent    *fakeSender
	mu        sync.Mutex
	buf       []byte
	deadline  time.Time
	closed    bool
	cancelled bool
	cancel    chan struct{}
	once      sync.Once
}

func (c *fakeCarrierStream) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *fakeCarrierStream) Write(p []byte) (int, error) {
	c.parent.mu.Lock()
	block := c.parent.carBlock
	c.parent.mu.Unlock()
	if block != nil {
		c.mu.Lock()
		deadline := c.deadline
		c.mu.Unlock()
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			t := time.NewTimer(time.Until(deadline))
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-c.parent.carBlock:
		case <-c.cancel:
			return 0, errors.New("carrier stream cancelled")
		case <-timeout:
			return 0, errors.New("carrier write deadline exceeded")
		}
	}
	select {
	case <-c.cancel:
		return 0, errors.New("carrier stream cancelled")
	default:
	}
	c.mu.Lock()
	c.buf = append(c.buf, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (f *fakeSender) setCarBlock(ch chan struct{}) {
	f.mu.Lock()
	f.carBlock = ch
	f.mu.Unlock()
}

func (f *fakeSender) carrierOpens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.carOpens
}

func (c *fakeCarrierStream) Close() error {
	select {
	case <-c.cancel:
		return errors.New("carrier stream cancelled")
	default:
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeCarrierStream) CancelWrite() {
	c.once.Do(func() {
		c.mu.Lock()
		c.cancelled = true
		c.mu.Unlock()
		close(c.cancel)
	})
}

func (c *fakeCarrierStream) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

func (c *fakeCarrierStream) state() (closed, cancelled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.cancelled
}

func (f *fakeSender) carrierStreams() []*fakeCarrierStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeCarrierStream(nil), f.carriers...)
}

// carrierRecords decodes every complete record across all carrier streams, in
// open order — the byte-identical datagrams a resilient viewer would feed its
// datagram path.
func (f *fakeSender) carrierRecords(t *testing.T) [][]byte {
	t.Helper()
	var records [][]byte
	for _, st := range f.carrierStreams() {
		buf := st.bytes()
		if len(buf) == 0 {
			continue
		}
		if err := wire.ParseCarrierPrologue(buf); err != nil {
			t.Fatalf("carrier stream prologue: %v", err)
		}
		rest := buf[wire.CarrierPrologueSize:]
		for len(rest) > 0 {
			record, remaining, err := wire.ParseCarrierRecord(rest)
			if err != nil {
				t.Fatalf("carrier record: %v", err)
			}
			records = append(records, append([]byte(nil), record...))
			rest = remaining
		}
	}
	return records
}

// keyframeMsg builds a full StreamFrame message (header + optional config +
// payload) as the broadcaster would write it to a uni stream. An empty codec
// omits the embedded config.
func keyframeMsg(t *testing.T, frameID uint32, codec, payload string) []byte {
	t.Helper()
	var config []byte
	if codec != "" {
		var err error
		config, err = wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: codec, Extradata: []byte{0x01, 0x02}})
		if err != nil {
			t.Fatalf("AppendDecoderConfig: %v", err)
		}
	}
	hdr := wire.StreamFrameHeader{
		Keyframe:    true,
		FrameID:     frameID,
		TimestampUs: uint64(frameID) * 16_667,
		ConfigLen:   uint32(len(config)),
		PayloadLen:  uint32(len(payload)),
	}
	msg, err := wire.AppendStreamFrameHeader(nil, hdr)
	if err != nil {
		t.Fatalf("AppendStreamFrameHeader: %v", err)
	}
	msg = append(msg, config...)
	msg = append(msg, payload...)
	return msg
}

// ingestKeyframe feeds a keyframe message to the publisher exactly as the
// transport accept loop would (one stream, read to EOF).
func ingestKeyframe(t *testing.T, p *Publisher, msg []byte) {
	t.Helper()
	if err := p.IngestKeyframeStream(bytes.NewReader(msg)); err != nil {
		t.Fatalf("IngestKeyframeStream: %v", err)
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitKeyframes blocks until the sender has received at least n keyframes.
// Keyframe delivery is asynchronous (a per-subscriber writer goroutine), and a
// closing subscriber cancels an in-flight keyframe — so tests wait for delivery
// before asserting or closing.
func waitKeyframes(t *testing.T, f *fakeSender, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.receivedKeyframes()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("keyframes delivered = %d, want >= %d", len(f.receivedKeyframes()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func chunkDgram(t *testing.T, keyframe bool, frameID uint32, index, count uint16, payload string) []byte {
	t.Helper()
	d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
		Keyframe:    keyframe,
		FrameID:     frameID,
		ChunkIndex:  index,
		ChunkCount:  count,
		TimestampUs: uint64(frameID) * 16_667,
	}, []byte(payload))
	if err != nil {
		t.Fatalf("AppendVideoChunk: %v", err)
	}
	return d
}

func configDgram(t *testing.T, codec string) []byte {
	t.Helper()
	d, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: codec, Extradata: []byte{0x01, 0x02}})
	if err != nil {
		t.Fatalf("AppendDecoderConfig: %v", err)
	}
	return d
}

func wantDatagrams(t *testing.T, f *fakeSender, want [][]byte) {
	t.Helper()
	got := f.received()
	if len(got) != len(want) {
		t.Fatalf("received %d datagrams, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("datagram %d = %x, want %x", i, got[i], want[i])
		}
	}
}

func TestVerbatimForwarding(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sent := [][]byte{
		configDgram(t, "avc1.42E02A"),
		chunkDgram(t, false, 7, 0, 2, "aa"),
		chunkDgram(t, false, 7, 1, 2, "bb"),
	}
	for _, d := range sent {
		p.HandleDatagram(d)
	}
	s.Close()
	wantDatagrams(t, f, sent)

	st := r.Stats()
	bst := st.Broadcasts[r.ObfuscateID(id)]
	if bst.FramesRelayed != 1 || bst.DatagramsRelayed != 3 || bst.BadDatagrams != 0 {
		t.Errorf("stats = %+v, want 1 frame, 3 datagrams, 0 bad", bst)
	}
}

func TestSecondPublisherRejected(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, _, err := r.StartPublish(id); !errors.Is(err, ErrPublisherActive) {
		t.Fatalf("second StartPublish error = %v, want ErrPublisherActive", err)
	}
	p.Close()
	p.Close() // idempotent
	if _, _, err := r.StartPublish(id); err != nil {
		t.Fatalf("StartPublish after Close: %v", err)
	}
}

func TestSubscribeFull(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxSubscribers: 2})
	id, _, _ := r.StartPublish("")

	s1, err := r.Subscribe(id, &fakeSender{})
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("CheckSubscribe should not fail: %v", err)
	}

	_, err = r.Subscribe(id, &fakeSender{})
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrFull) {
		t.Errorf("CheckSubscribe error = %v, want ErrFull", err)
	}

	if _, err := r.Subscribe(id, &fakeSender{}); !errors.Is(err, ErrFull) {
		t.Fatalf("third Subscribe error = %v, want ErrFull", err)
	}
	s1.Close()
	if _, err := r.Subscribe(id, &fakeSender{}); err != nil {
		t.Fatalf("Subscribe after a slot freed: %v", err)
	}
}

func TestLateJoinerPrimedWithStreamKeyframe(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	// A keyframe (config + payload) arrives over a stream and is cached.
	kf := keyframeMsg(t, 10, "avc1.42E02A", "keyframe-bytes")
	ingestKeyframe(t, p, kf)

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Deltas after joining still flow over datagrams.
	live := chunkDgram(t, false, 11, 0, 1, "delta")
	p.HandleDatagram(live)

	waitKeyframes(t, f, 1)
	s.Close()

	if kfs := f.receivedKeyframes(); len(kfs) != 1 || !bytes.Equal(kfs[0], kf) {
		t.Fatalf("primed keyframes = %d, want 1 matching the cached keyframe", len(kfs))
	}
	wantDatagrams(t, f, [][]byte{live})
}

func TestPrimingSurvivesPublisherClose(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p, _ := r.StartPublish("")
	kf := keyframeMsg(t, 0, "vp8", "kf")
	ingestKeyframe(t, p, kf)
	p.Close()

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitKeyframes(t, f, 1)
	s.Close()
	if kfs := f.receivedKeyframes(); len(kfs) != 1 || !bytes.Equal(kfs[0], kf) {
		t.Fatalf("primed keyframes = %d, want 1 (cache must survive publisher away)", len(kfs))
	}
}

// A keyframe stream that overruns MaxKeyframeBytes is rejected and never
// cached; the previous cache is left intact.
func TestOversizeKeyframeNotCached(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxKeyframeBytes: wire.StreamFrameHeaderSize + 8})
	id, p, _ := r.StartPublish("")

	good := keyframeMsg(t, 5, "", "12345678") // exactly 8 payload bytes: fits
	ingestKeyframe(t, p, good)

	toobig := keyframeMsg(t, 6, "", "123456789") // 9 payload bytes: over the cap
	if err := p.IngestKeyframeStream(bytes.NewReader(toobig)); err == nil {
		t.Fatal("IngestKeyframeStream accepted an oversize keyframe, want error")
	}

	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.CachedKeyframeID != 5 {
		t.Errorf("cached keyframe id = %d after oversize reject, want 5 (unchanged)", st.CachedKeyframeID)
	}
	if st.KeyframeStreamsOversize != 1 {
		t.Errorf("KeyframeStreamsOversize = %d, want 1", st.KeyframeStreamsOversize)
	}
}

// A newer keyframe supersedes a stale in-flight one to the same subscriber
// (blocked writer), and only the newest is ultimately delivered.
func TestNewKeyframeSupersedesStale(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	f := &fakeSender{kfBlock: make(chan struct{})}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First keyframe: its write blocks (subscriber slow).
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "first"))
	waitFor(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.kfOpens == 1
	}, "first keyframe stream opened")

	// Second keyframe supersedes the first (CancelWrite) and opens a new stream.
	ingestKeyframe(t, p, keyframeMsg(t, 1, "vp8", "second"))
	waitFor(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.kfOpens == 2
	}, "second keyframe stream opened")

	// Release the block: the second keyframe completes, the first was dropped.
	close(f.kfBlock)
	waitKeyframes(t, f, 1)
	s.Close()

	if got := s.KeyframesDropped(); got < 1 {
		t.Errorf("keyframesDropped = %d, want >= 1 (superseded first)", got)
	}
	kfs := f.receivedKeyframes()
	if len(kfs) != 1 {
		t.Fatalf("delivered %d keyframes, want exactly 1 (the newest)", len(kfs))
	}
	hdr, _ := wire.ParseStreamFrameHeader(kfs[0])
	if hdr.FrameID != 1 {
		t.Errorf("delivered keyframe frameID = %d, want 1 (newest)", hdr.FrameID)
	}
}

// A new publisher session invalidates the keyframe cache (frameIDs reset, codec
// may differ); a joiner during the away window is primed with the old keyframe,
// a joiner after restart with the new one.
func TestPublisherRestartResetsKeyframeCache(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	oldKf := keyframeMsg(t, 41, "avc1.42E02A", "old")
	ingestKeyframe(t, p1, oldKf)
	p1.Close()

	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if !st.HasConfig || st.CachedKeyframeID != 41 {
		t.Fatalf("caches after publisher close = %+v, want config + keyframe 41", st)
	}
	away := &fakeSender{}
	sa, _ := r.Subscribe(id, away)
	waitKeyframes(t, away, 1)
	sa.Close()
	if kfs := away.receivedKeyframes(); len(kfs) != 1 || !bytes.Equal(kfs[0], oldKf) {
		t.Fatalf("away joiner primed with %d keyframes, want the old cached one", len(kfs))
	}

	_, p2, err := r.StartPublish(id)
	if err != nil {
		t.Fatalf("StartPublish after restart: %v", err)
	}
	st = r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.HasConfig || st.CachedKeyframeID != 0 || st.CachedKeyframeBytes != 0 {
		t.Fatalf("keyframe cache survived publisher restart: %+v", st)
	}

	newKf := keyframeMsg(t, 0, "vp09.00.40.08", "new")
	ingestKeyframe(t, p2, newKf)

	late := &fakeSender{}
	sl, _ := r.Subscribe(id, late)
	waitKeyframes(t, late, 1)
	sl.Close()
	if kfs := late.receivedKeyframes(); len(kfs) != 1 || !bytes.Equal(kfs[0], newKf) {
		t.Fatalf("post-restart joiner primed with %d keyframes, want the new one", len(kfs))
	}
}

// The crux (docs/12 Decision 3/4): a subscriber whose keyframe stream stalls is
// superseded/dropped and recovers at the next keyframe, while a healthy peer
// receives every keyframe and the publisher ingest is never blocked.
func TestSlowKeyframeStreamDropsHealthyPeerUnaffected(t *testing.T) {
	const n = 20
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	healthy := &fakeSender{}
	blocked := &fakeSender{kfBlock: make(chan struct{})}
	sh, _ := r.Subscribe(id, healthy)
	sb, _ := r.Subscribe(id, blocked)

	for i := range n {
		// Ingest returns promptly regardless of the blocked subscriber — the
		// publisher is never coupled to a slow peer.
		ingestKeyframe(t, p, keyframeMsg(t, uint32(i), "vp8", fmt.Sprintf("kf%02d", i)))
		waitKeyframes(t, healthy, i+1)
	}

	sh.Close()
	if got := len(healthy.receivedKeyframes()); got != n {
		t.Errorf("healthy subscriber received %d keyframes, want %d", got, n)
	}

	close(blocked.kfBlock)
	sb.Close()
	if got := sb.KeyframesDropped(); got == 0 {
		t.Error("blocked subscriber recorded no keyframe drops")
	}
	if st := r.Stats().Totals.KeyframeStreamsDropped; st == 0 {
		t.Error("hub stats did not accumulate keyframe drops from the blocked subscriber")
	}
}

func TestUnreachableSubscriberEvictedAfterConsecutiveOpenFailures(t *testing.T) {
	// R10 field finding (docs/14): a session whose client stopped reading uni
	// streams exhausts its stream credit — every OpenKeyframeStream fails,
	// forever. Without eviction the relay burns fan-out work on the zombie
	// indefinitely. After KeyframeOpenFailEvictThreshold consecutive open
	// failures the subscriber must be closed (with the non-terminal
	// "unresponsive" code so a live client reconnects) and removed, while a
	// healthy peer keeps receiving every keyframe.
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	healthy := &fakeSender{}
	zombie := &fakeSender{kfOpenErr: errors.New("too many open streams")}
	sh, _ := r.Subscribe(id, healthy)
	defer sh.Close()
	if _, err := r.Subscribe(id, zombie); err != nil {
		t.Fatalf("Subscribe(zombie): %v", err)
	}

	for i := range KeyframeOpenFailEvictThreshold {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(i+1), "vp8", fmt.Sprintf("kf%02d", i)))
		// Pace on the healthy peer's delivery so rapid ingests don't supersede
		// its in-flight writes (≤1 in flight per subscriber, by design).
		waitKeyframes(t, healthy, i+1)
	}

	waitFor(t, 5*time.Second, func() bool {
		code, closed := zombie.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "zombie subscriber to be closed with the unresponsive code")
	waitFor(t, 5*time.Second, func() bool {
		return singleBroadcastStats(t, r).Subscribers == 1
	}, "evicted subscriber to be removed from the broadcast")

	// The healthy peer got every keyframe; the drops were all counted.
	waitKeyframes(t, healthy, KeyframeOpenFailEvictThreshold)
	if st := singleBroadcastStats(t, r); st.KeyframeDrops.OpenFailed != KeyframeOpenFailEvictThreshold {
		t.Errorf("KeyframeDrops.OpenFailed = %d, want %d", st.KeyframeDrops.OpenFailed, KeyframeOpenFailEvictThreshold)
	}
}

func TestOpenFailureStreakResetsOnSuccess(t *testing.T) {
	// Eviction is for *persistent* unreachability: a single successful stream
	// open between two sub-threshold failure streaks must reset the count.
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	flaky := &fakeSender{kfOpenErr: errors.New("transient")}
	if _, err := r.Subscribe(id, flaky); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	seq := uint32(0)
	ingest := func(n int) {
		for range n {
			seq++
			ingestKeyframe(t, p, keyframeMsg(t, seq, "vp8", fmt.Sprintf("kf%03d", seq)))
		}
	}

	ingest(KeyframeOpenFailEvictThreshold - 1)
	flaky.setKfOpenErr(nil)
	ingest(1) // success resets the streak
	flaky.setKfOpenErr(errors.New("transient again"))
	ingest(KeyframeOpenFailEvictThreshold - 1)

	if _, closed := flaky.getCloseInfo(); closed {
		t.Fatal("subscriber was evicted despite the streak resetting on success")
	}
	if got := singleBroadcastStats(t, r).Subscribers; got != 1 {
		t.Errorf("Subscribers = %d, want 1 (no eviction)", got)
	}
}

func TestStalledSubscriberEvictedAfterConsecutiveSlowKeyframes(t *testing.T) {
	// Safari field finding (2026-07-21): a viewer whose stream path wedges while
	// datagrams keep flowing (QUIC datagrams are not flow-controlled; streams
	// are) leaves every keyframe *open* succeeding and every *write* timing out.
	// That never feeds the open-failure streak, so the pre-fix relay kept the
	// subscriber attached forever: it received deltas it could not decode and
	// froze permanently on "awaiting keyframe". A subscriber dropping
	// KeyframeSlowEvictThreshold consecutive keyframes as "slow" is as
	// unreachable as one that cannot open streams — evict it with the same
	// non-terminal code so a live client reconnects into fresh stream credit.
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	healthy := &fakeSender{}
	// Opens succeed, writes fail: the stalled-stream shape, not the zombie one.
	stalled := &fakeSender{kfWriteErr: errors.New("write deadline exceeded")}
	sh, _ := r.Subscribe(id, healthy)
	defer sh.Close()
	if _, err := r.Subscribe(id, stalled); err != nil {
		t.Fatalf("Subscribe(stalled): %v", err)
	}

	for i := range KeyframeSlowEvictThreshold {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(i+1), "vp8", fmt.Sprintf("kf%02d", i)))
		// Pace on the healthy peer so a rapid next ingest can't supersede the
		// stalled peer's in-flight write — a superseded drop is not a slow one.
		waitKeyframes(t, healthy, i+1)
	}

	waitFor(t, 5*time.Second, func() bool {
		code, closed := stalled.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "stalled subscriber to be closed with the unresponsive code")
	waitFor(t, 5*time.Second, func() bool {
		return singleBroadcastStats(t, r).Subscribers == 1
	}, "evicted subscriber to be removed from the broadcast")

	// Every drop was attributed to the stall, not miscounted as superseded.
	if st := singleBroadcastStats(t, r); st.KeyframeDrops.Slow != KeyframeSlowEvictThreshold {
		t.Errorf("KeyframeDrops.Slow = %d, want %d", st.KeyframeDrops.Slow, KeyframeSlowEvictThreshold)
	}
}

func TestSlowStreakResetsOnDeliveredKeyframe(t *testing.T) {
	// Eviction is for *persistent* stalls. A transiently slow subscriber that
	// gets one keyframe through must not accumulate toward eviction — otherwise
	// a congested-but-live viewer is disconnected for a bad few seconds.
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	flaky := &fakeSender{kfWriteErr: errors.New("transient stall")}
	if _, err := r.Subscribe(id, flaky); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	seq := uint32(0)
	ingest := func(n int) {
		for range n {
			seq++
			ingestKeyframe(t, p, keyframeMsg(t, seq, "vp8", fmt.Sprintf("kf%03d", seq)))
			waitFor(t, 5*time.Second, func() bool {
				return singleBroadcastStats(t, r).KeyframeDrops.Total()+flaky.keyframeCount() >= uint64(seq)
			}, "keyframe to be accounted before the next ingest")
		}
	}

	ingest(KeyframeSlowEvictThreshold - 1)
	flaky.setKfWriteErr(nil)
	ingest(1) // a delivered keyframe resets the streak
	flaky.setKfWriteErr(errors.New("stalling again"))
	ingest(KeyframeSlowEvictThreshold - 1)

	if _, closed := flaky.getCloseInfo(); closed {
		t.Fatal("subscriber was evicted despite the streak resetting on a delivered keyframe")
	}
	if got := singleBroadcastStats(t, r).Subscribers; got != 1 {
		t.Errorf("Subscribers = %d, want 1 (no eviction)", got)
	}
}

func TestBadDatagramsDroppedAndCounted(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")
	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)

	bad := [][]byte{
		nil,
		{0x02, 0x01},
		{0x01, 0x7F, 0x00},
		{0x01, 0x01, 0x00, 0x00},
		{0x01, 0x02, 0x00, 0x09, 'v'},
	}
	for _, d := range bad {
		p.HandleDatagram(d)
	}
	s.Close()
	wantDatagrams(t, f, nil)
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.BadDatagrams != uint64(len(bad)) {
		t.Errorf("BadDatagrams = %d, want %d", st.BadDatagrams, len(bad))
	}
}

func TestSlowSubscriberDropsHealthyPeerUnaffected(t *testing.T) {
	const queueDepth = 8
	const n = 100

	r := NewRegistry(discardLog, Options{QueueDepth: queueDepth})
	id, p, _ := r.StartPublish("")

	healthy := &fakeSender{}
	blocked := &fakeSender{block: make(chan struct{})}
	sh, _ := r.Subscribe(id, healthy)
	sb, _ := r.Subscribe(id, blocked)

	var sent [][]byte
	for i := range n {
		d := chunkDgram(t, false, uint32(i), 0, 1, fmt.Sprintf("f%03d", i))
		sent = append(sent, d)
		p.HandleDatagram(d)
		deadline := time.Now().Add(5 * time.Second)
		for {
			healthy.mu.Lock()
			caughtUp := len(healthy.got) == i+1
			healthy.mu.Unlock()
			if caughtUp {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("healthy subscriber stuck at datagram %d", i)
			}
			time.Sleep(time.Millisecond)
		}
	}

	sh.Close()
	wantDatagrams(t, healthy, sent)

	if got := sb.Dropped(); got < n-queueDepth-1 {
		t.Errorf("blocked subscriber dropped %d datagrams, want >= %d", got, n-queueDepth-1)
	}
	close(blocked.block)
	sb.Close()

	if st := r.Stats().Totals.DatagramsDropped; st == 0 {
		t.Error("hub stats did not accumulate drops from the closed subscriber")
	}
}

func TestSixteenthSubscriberRejectedAtDefaultCap(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, _, _ := r.StartPublish("")
	for i := range 15 {
		if _, err := r.Subscribe(id, &fakeSender{}); err != nil {
			t.Fatalf("Subscribe %d: %v", i+1, err)
		}
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrFull) {
		t.Error("CheckSubscribe did not return ErrFull")
	}
	if _, err := r.Subscribe(id, &fakeSender{}); !errors.Is(err, ErrFull) {
		t.Fatalf("16th Subscribe error = %v, want ErrFull", err)
	}
}

func TestFifteenSubscribersOneBlocked(t *testing.T) {
	duration := 5 * time.Second
	if testing.Short() {
		duration = 300 * time.Millisecond
	}

	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	const healthyCount = 14
	healthy := make([]*fakeSender, healthyCount)
	healthySubs := make([]*Subscriber, healthyCount)
	for i := range healthy {
		healthy[i] = &fakeSender{}
		if healthySubs[i], err = r.Subscribe(id, healthy[i]); err != nil {
			t.Fatalf("Subscribe healthy %d: %v", i, err)
		}
	}
	blocked := &fakeSender{block: make(chan struct{})}
	blockedSub, err := r.Subscribe(id, blocked)
	if err != nil {
		t.Fatalf("Subscribe blocked: %v", err)
	}

	cfg := configDgram(t, "avc1.42E02A")
	want := [][]byte{cfg}
	p.HandleDatagram(cfg)

	const chunksPerFrame = 3
	deadline := time.Now().Add(duration)
	for frameID := uint32(0); time.Now().Before(deadline); frameID++ {
		// Keyframe-flagged chunks are still forwarded verbatim as datagrams
		// here; the hub no longer re-emits config before them (config rides the
		// keyframe stream in production, R8). This test exercises datagram
		// fan-out backpressure, so the flag only affects the payload.
		keyframe := frameID%60 == 0
		for ci := range uint16(chunksPerFrame) {
			d := chunkDgram(t, keyframe, frameID, ci, chunksPerFrame,
				fmt.Sprintf("f%d/%d", frameID, ci))
			want = append(want, d)
			p.HandleDatagram(d)
		}
		time.Sleep(16 * time.Millisecond)
	}

	catchup := time.Now().Add(5 * time.Second)
	for i, f := range healthy {
		for len(f.received()) < len(want) {
			if time.Now().After(catchup) {
				t.Fatalf("healthy subscriber %d received %d of %d datagrams",
					i, len(f.received()), len(want))
			}
			time.Sleep(time.Millisecond)
		}
	}

	for i, s := range healthySubs {
		s.Close()
		if got := s.Dropped(); got != 0 {
			t.Errorf("healthy subscriber %d dropped %d datagrams, want 0", i, got)
		}
		wantDatagrams(t, healthy[i], want)
	}

	minDropped := uint64(max(0, len(want)-r.opts.QueueDepth-1))
	if got := blockedSub.Dropped(); got < minDropped {
		t.Errorf("blocked subscriber dropped %d datagrams, want >= %d", got, minDropped)
	}
	close(blocked.block)
	blockedSub.Close()

	if st := r.Stats().Totals.DatagramsDropped; st < minDropped {
		t.Errorf("Stats().Totals.DatagramsDropped = %d, want >= %d", st, minDropped)
	}
}

func TestSubscriberCloseIdempotentAndConcurrent(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, _, _ := r.StartPublish("")
	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Close()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close did not finish")
	}
}

func TestSendErrorsDoNotStopDrain(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")
	f := &fakeSender{err: errors.New("session gone")}
	s, _ := r.Subscribe(id, f)

	for i := range 3 {
		p.HandleDatagram(chunkDgram(t, false, uint32(i), 0, 1, "x"))
	}
	s.Close()
	if got := s.sendErrors.Load(); got != 3 {
		t.Errorf("sendErrors = %d, want 3", got)
	}
}

func TestConcurrentPublishSubscribeChurn(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxSubscribers: 2, QueueDepth: 32})
	id, p, _ := r.StartPublish("")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := uint32(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			keyframe := i%10 == 0
			if keyframe {
				p.HandleDatagram(configDgram(t, "vp8"))
			}
			p.HandleDatagram(chunkDgram(t, keyframe, i, 0, 2, "p0"))
			p.HandleDatagram(chunkDgram(t, keyframe, i, 1, 2, "p1"))
			i++
		}
	}()

	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s, err := r.Subscribe(id, &fakeSender{})
				if err != nil {
					continue
				}
				s.Close()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	p.Close()
}

func TestBroadcastIsolation(t *testing.T) {
	r := NewRegistry(discardLog, Options{})

	id1, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish 1: %v", err)
	}
	id2, p2, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish 2: %v", err)
	}

	if id1 == id2 {
		t.Fatalf("expected different IDs, got both as %q", id1)
	}

	f1 := &fakeSender{}
	f2 := &fakeSender{}

	s1, err := r.Subscribe(id1, f1)
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	s2, err := r.Subscribe(id2, f2)
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}

	dgram1 := chunkDgram(t, false, 1, 0, 1, "data1")
	dgram2 := chunkDgram(t, false, 2, 0, 1, "data2")

	p1.HandleDatagram(dgram1)
	p2.HandleDatagram(dgram2)

	s1.Close()
	s2.Close()

	wantDatagrams(t, f1, [][]byte{dgram1})
	wantDatagrams(t, f2, [][]byte{dgram2})

	st := r.Stats()
	if st.Totals.Broadcasts != 2 {
		t.Errorf("expected 2 active broadcasts in totals, got %d", st.Totals.Broadcasts)
	}
	if bst1 := st.Broadcasts[r.ObfuscateID(id1)]; bst1.DatagramsRelayed != 1 {
		t.Errorf("broadcast 1 expected 1 relayed, got %d", bst1.DatagramsRelayed)
	}
	if bst2 := st.Broadcasts[r.ObfuscateID(id2)]; bst2.DatagramsRelayed != 1 {
		t.Errorf("broadcast 2 expected 1 relayed, got %d", bst2.DatagramsRelayed)
	}
}

func TestGraceLifecycle(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 50 * time.Millisecond})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Drops from a subscriber still live at GC time must survive into the
	// registry totals.
	s.dropped.Add(5)

	p.Close()

	// Wait for grace timeout to trigger GC
	time.Sleep(100 * time.Millisecond)

	closeCode, closed := f.getCloseInfo()
	if !closed {
		t.Error("expected subscriber connection to be closed")
	}
	if closeCode != uint32(wire.CloseCodeBroadcastEnded) {
		t.Errorf("expected close code %d, got %d", wire.CloseCodeBroadcastEnded, closeCode)
	}
	if got := r.Stats().Totals.DatagramsDropped; got != 5 {
		t.Errorf("Totals.DatagramsDropped after GC = %d, want 5", got)
	}

	// Entry must be deleted
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for expired ID, got %v", err)
	}

	// Reclaim expired ID should fail
	if _, _, err := r.StartPublish(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for expired ID reclaim, got %v", err)
	}
}

func TestGraceReclaim(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 100 * time.Millisecond})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	p1.Close()

	// Reclaim within grace
	time.Sleep(20 * time.Millisecond)
	_, p2, err := r.StartPublish(id)
	if err != nil {
		t.Fatalf("Reclaim failed: %v", err)
	}

	// Wait past the original 100ms grace period to verify timer was canceled
	time.Sleep(120 * time.Millisecond)

	closeCode, closed := f.getCloseInfo()
	if closed {
		t.Fatalf("subscriber was closed early with code %d; reclaim did not cancel GC", closeCode)
	}

	p2.Close()
	s.Close()
}

// fakePublisherConn records the takeover kick sent to a deposed publisher's
// session. It implements hub.SessionCloser.
type fakePublisherConn struct {
	mu        sync.Mutex
	closeCode uint32
	closed    bool
}

func (f *fakePublisherConn) CloseWithError(code uint32, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCode = code
	f.closed = true
	return nil
}

func (f *fakePublisherConn) getCloseInfo() (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode, f.closed
}

// The zombie-publisher lockout (docs/06 revision 2026-07-18): a publisher
// whose session dies silently keeps the slot until the QUIC idle timeout,
// and rejecting its reclaim in that window forced clients into a mint
// fallback that orphaned every viewer — the old broadcast was GC'd while
// the broadcaster streamed on under a fresh ID. TakeOverPublish must depose
// the incumbent instead: newest publisher wins.
func TestTakeOverSupersedesActivePublisher(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 50 * time.Millisecond})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	conn1 := &fakePublisherConn{}
	if !p1.BindConn(conn1) {
		t.Fatal("BindConn on the live publisher = false, want true")
	}
	// Prime the caches so the takeover's session reset is observable.
	ingestKeyframe(t, p1, keyframeMsg(t, 7, "avc1.42E02A", "kf"))

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitKeyframes(t, f, 1) // the join prime, from p1's session

	gotID, p2, err := r.TakeOverPublish(id)
	if err != nil {
		t.Fatalf("TakeOverPublish: %v", err)
	}
	if gotID != id {
		t.Fatalf("TakeOverPublish id = %q, want %q", gotID, id)
	}

	// The deposed session is kicked with the superseded close code.
	if code, closed := conn1.getCloseInfo(); !closed || code != uint32(wire.CloseCodePublisherSuperseded) {
		t.Fatalf("deposed publisher close = (%d, %v), want (%d, true)", code, closed, wire.CloseCodePublisherSuperseded)
	}

	// A new publisher session means new frameIDs and possibly a new config:
	// the keyframe cache must have been invalidated, like any reclaim.
	if st := r.Stats().Broadcasts[r.ObfuscateID(id)]; st.CachedKeyframeBytes != 0 {
		t.Errorf("cached keyframe survived takeover (%d bytes), want invalidated", st.CachedKeyframeBytes)
	}

	// Late datagrams from the deposed handle must not reach subscribers;
	// the new publisher's must.
	p1.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "stale"))
	live := chunkDgram(t, false, 1, 0, 1, "live")
	p2.HandleDatagram(live)
	waitFor(t, 5*time.Second, func() bool { return len(f.received()) >= 1 }, "live datagram delivered")
	wantDatagrams(t, f, [][]byte{live})

	// The deposed handler's deferred Close must not free the slot or arm the
	// GC grace timer — that is exactly what killed live broadcasts.
	p1.Close()
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if !st.PublisherActive {
		t.Fatal("publisher slot freed by the deposed publisher's Close")
	}
	if st.GraceRemainingSeconds != 0 {
		t.Fatalf("grace timer armed by the deposed publisher's Close (%ds remaining)", st.GraceRemainingSeconds)
	}
	time.Sleep(100 * time.Millisecond) // past BroadcastGrace
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast GC'd while the new publisher is active: %v", err)
	}
	if code, closed := f.getCloseInfo(); closed {
		t.Fatalf("subscriber closed (code %d) while the new publisher is active", code)
	}

	p2.Close()
	s.Close()
}

// TakeOverPublish on a broadcast whose publisher already closed behaves like
// a plain reclaim: it claims the slot and cancels the pending grace timer.
func TestTakeOverOnInactiveBehavesLikeReclaim(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 50 * time.Millisecond})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	p1.Close() // arms the grace timer

	_, p2, err := r.TakeOverPublish(id)
	if err != nil {
		t.Fatalf("TakeOverPublish: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // past the original grace deadline
	if err := r.CheckSubscribe(id); err != nil {
		t.Fatalf("broadcast GC'd despite takeover cancelling the grace timer: %v", err)
	}
	p2.Close()
}

func TestTakeOverPublishNotFound(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	if _, _, err := r.TakeOverPublish("AAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TakeOverPublish on unknown id = %v, want ErrNotFound", err)
	}
}

// A publisher deposed between its pre-upgrade claim and its post-upgrade
// BindConn learns about it from BindConn, so the transport layer can end the
// session instead of continuing a deposed broadcast.
func TestBindConnAfterTakeOverReportsDeposed(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, p2, err := r.TakeOverPublish(id); err != nil {
		t.Fatalf("TakeOverPublish: %v", err)
	} else {
		defer p2.Close()
	}
	if p1.BindConn(&fakePublisherConn{}) {
		t.Fatal("BindConn on a deposed publisher = true, want false")
	}
}

// The AfterFunc/reclaim race (design doc 06, E3): a grace callback armed for
// an older publisher generation can fire concurrently with — or after — a
// successful reclaim (Timer.Stop does not guarantee the callback hasn't
// started). The generation check must make the stale callback a no-op.
func TestGraceExpiryStaleGenerationIsNoOp(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: time.Hour})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	p1.Close() // arms the (1h) grace timer for the current generation

	r.mu.Lock()
	staleGen := r.hubs[id].generation
	r.mu.Unlock()

	_, p2, err := r.StartPublish(id) // reclaim: cancels timer, bumps generation
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	// Simulate the armed callback firing anyway with its stale generation.
	r.handleGraceExpiry(id, staleGen)

	if err := r.CheckSubscribe(id); err != nil {
		t.Errorf("broadcast deleted by stale grace callback: %v", err)
	}
	if closeCode, closed := f.getCloseInfo(); closed {
		t.Errorf("subscriber closed by stale grace callback (code %d)", closeCode)
	}

	p2.Close()
	s.Close()
}

// R2 review finding F2: broadcast IDs are only ~31^6 strong, so an unkeyed
// hash of them (the original truncated SHA-256) is brute-forceable offline
// from a /statusz scrape. The stats key must depend on per-registry secret
// state — two registries must key the same ID differently.
func TestObfuscatedStatsKeysArePerRegistry(t *testing.T) {
	r1 := NewRegistry(discardLog, Options{})
	r2 := NewRegistry(discardLog, Options{})
	const id = "ABCDEF"
	if r1.ObfuscateID(id) == r2.ObfuscateID(id) {
		t.Errorf("ObfuscateID(%q) identical across registries (%q): keying is offline-computable", id, r1.ObfuscateID(id))
	}
	if r1.ObfuscateID(id) != r1.ObfuscateID(id) {
		t.Error("ObfuscateID not stable within a registry")
	}

	mintedID, p, err := r1.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer p.Close()
	st := r1.Stats()
	if _, ok := st.Broadcasts[r1.ObfuscateID(mintedID)]; !ok {
		t.Errorf("Stats not keyed by this registry's ObfuscateID; keys = %v", st.Broadcasts)
	}
	if _, ok := st.Broadcasts[mintedID]; ok {
		t.Error("Stats leaked the raw broadcast ID as a key")
	}
}

func TestMaxBroadcastsLimit(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBroadcasts: 2})
	id1, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish 1: %v", err)
	}
	id2, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish 2: %v", err)
	}
	if _, _, err := r.StartPublish(""); !errors.Is(err, ErrMaxBroadcasts) {
		t.Fatalf("StartPublish 3 error = %v, want ErrMaxBroadcasts", err)
	}
	// Reclaims should not count towards max broadcasts
	_, _, err = r.StartPublish(id1)
	if !errors.Is(err, ErrPublisherActive) { // active, but not ErrMaxBroadcasts
		t.Fatalf("reclaim error = %v, want ErrPublisherActive", err)
	}
	// Clean up one to free slot
	r.mu.Lock()
	delete(r.hubs, id2)
	r.mu.Unlock()

	_, _, err = r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish 4 after delete: %v", err)
	}
}

func TestMaxTotalSubscribersLimit(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBroadcasts: 2, MaxSubscribers: 10, MaxTotalSubscribers: 3})
	id1, _, _ := r.StartPublish("")
	id2, _, _ := r.StartPublish("")

	s1, err := r.Subscribe(id1, &fakeSender{})
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	defer s1.Close()

	s2, err := r.Subscribe(id1, &fakeSender{})
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	defer s2.Close()

	s3, err := r.Subscribe(id2, &fakeSender{})
	if err != nil {
		t.Fatalf("Subscribe 3: %v", err)
	}
	defer s3.Close()

	// 4th subscriber exceeds total limit (3)
	if err := r.CheckSubscribe(id2); !errors.Is(err, ErrTotalSubscribers) {
		t.Fatalf("CheckSubscribe 4 error = %v, want ErrTotalSubscribers", err)
	}
	if _, err := r.Subscribe(id2, &fakeSender{}); !errors.Is(err, ErrTotalSubscribers) {
		t.Fatalf("Subscribe 4 error = %v, want ErrTotalSubscribers", err)
	}
}

// Design-doc verification item (R2): datagrams larger than
// wire.MaxDatagramSize are dropped and counted as bad — never relayed,
// never cached — even when they start with a valid VideoChunk header.
func TestOversizedDatagramDroppedAndCounted(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	oversized := make([]byte, wire.MaxDatagramSize+1)
	copy(oversized, chunkDgram(t, true, 1, 0, 1, "x")) // valid header: only the size check can reject it
	p.HandleDatagram(oversized)

	s.Close()
	wantDatagrams(t, f, nil)
	st := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if st.BadDatagrams != 1 {
		t.Errorf("BadDatagrams = %d, want 1", st.BadDatagrams)
	}
	if st.DatagramsRelayed != 0 || st.CachedKeyframeBytes != 0 {
		t.Errorf("oversized datagram leaked into relay/cache: %+v", st)
	}
}

// R2 review finding F3: drain() keeps consuming — and bandwidth-dropping —
// a closed subscriber's queued backlog after Close folded its drop count
// into the hub, so those drops vanished from the totals. The fold must
// happen only once drain has finished.
func TestBandwidthDropsSurviveSubscriberClose(t *testing.T) {
	const backlog = 5
	payload := strings.Repeat("x", 1000)
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 1100})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{block: make(chan struct{})}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The first datagram fits the bandwidth burst and parks drain inside
	// SendDatagram; the backlog queues up behind it with the token bucket
	// exhausted (refilling far too slowly to pass another ~1KB datagram).
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, payload))
	for i := range backlog {
		p.HandleDatagram(chunkDgram(t, false, uint32(2+i), 0, 1, payload))
	}

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	// Wait until Close has marked the subscriber closed (and closed the
	// queue), then release drain to bandwidth-drop the backlog.
	for {
		if s.closed.Load() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(f.block)
	<-closed
	p.Close()

	st := r.Stats().Totals
	if st.DatagramsDropped != backlog {
		t.Errorf("Totals.DatagramsDropped = %d, want %d (post-close bandwidth drops lost)", st.DatagramsDropped, backlog)
	}
	if st.BandwidthDroppedDatagrams != backlog {
		t.Errorf("Totals.BandwidthDroppedDatagrams = %d, want %d", st.BandwidthDroppedDatagrams, backlog)
	}
}

// R2 review finding F3: a subscriber still draining when its broadcast is
// garbage collected recorded bandwidth drops onto the orphaned hub struct —
// after handleGraceExpiry had already folded the hub's counters into the
// registry totals — losing them. Late drops must fall back to the totals.
func TestBandwidthDropsSurviveHubGC(t *testing.T) {
	const backlog = 5
	payload := strings.Repeat("x", 1000)
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 1100, BroadcastGrace: 30 * time.Millisecond})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{block: make(chan struct{})}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, payload))
	for i := range backlog {
		p.HandleDatagram(chunkDgram(t, false, uint32(2+i), 0, 1, payload))
	}
	p.Close() // grace timer starts; drain is parked in SendDatagram

	deadline := time.Now().Add(5 * time.Second)
	for r.Stats().Totals.Broadcasts != 0 {
		if time.Now().After(deadline) {
			t.Fatal("broadcast was not garbage collected")
		}
		time.Sleep(time.Millisecond)
	}

	close(f.block) // drain now bandwidth-drops the backlog on the GC'd hub
	s.Close()      // idempotent; waits for drain teardown

	deadline = time.Now().Add(5 * time.Second)
	for {
		st := r.Stats().Totals
		if st.BandwidthDroppedDatagrams == backlog && st.DatagramsDropped == backlog {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Totals = %+v, want %d bandwidth drops and %d dropped datagrams surviving GC", st, backlog, backlog)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBandwidthLimiting(t *testing.T) {
	// Limiter with 100 bytes/sec limit
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 100})
	if r.limiter == nil {
		t.Fatal("limiter not initialized")
	}

	// Consume 100 bytes (burst cap)
	if !r.consumeBandwidth(100) {
		t.Error("consumeBandwidth(100) expected true, got false")
	}
	// Instantly consuming more should fail
	if r.consumeBandwidth(1) {
		t.Error("consumeBandwidth(1) expected false, got true")
	}

	// Refill check
	r.limiter.mu.Lock()
	r.limiter.tokens = 50
	r.limiter.mu.Unlock()

	if !r.consumeBandwidth(40) {
		t.Error("consumeBandwidth(40) expected true, got false")
	}
	if r.consumeBandwidth(20) {
		t.Error("consumeBandwidth(20) expected false, got true")
	}
}

// R5 Q2 (docs/15): the hub relays ClockMapping datagrams to live subscribers,
// caches the latest, primes late joiners with it, and invalidates the cache on
// a new publisher session — the same lifecycle as the cached keyframe.
func TestClockMappingRelayedCachedAndPrimed(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	hasMapping := func(f *fakeSender, want []byte) func() bool {
		return func() bool {
			for _, d := range f.received() {
				if bytes.Equal(d, want) {
					return true
				}
			}
			return false
		}
	}
	countMappings := func(f *fakeSender) int {
		n := 0
		for _, d := range f.received() {
			if len(d) >= 2 && d[1] == wire.TypeClockMapping {
				n++
			}
		}
		return n
	}

	s1 := &fakeSender{}
	sub1, err := r.Subscribe(id, s1)
	if err != nil {
		t.Fatalf("Subscribe s1: %v", err)
	}
	defer sub1.Close()

	mapping := wire.AppendClockMapping(nil, 123_456)
	p.HandleDatagram(mapping)
	waitFor(t, 5*time.Second, hasMapping(s1, mapping), "mapping fanned out to live subscriber")

	// Late joiner: primed with the cached mapping without the broadcaster
	// re-sending anything.
	s2 := &fakeSender{}
	sub2, err := r.Subscribe(id, s2)
	if err != nil {
		t.Fatalf("Subscribe s2: %v", err)
	}
	defer sub2.Close()
	waitFor(t, 5*time.Second, hasMapping(s2, mapping), "late joiner primed with cached mapping")

	// A newer mapping supersedes the cache: the next joiner gets it, not the old one.
	mapping2 := wire.AppendClockMapping(nil, -42)
	p.HandleDatagram(mapping2)
	s3 := &fakeSender{}
	sub3, err := r.Subscribe(id, s3)
	if err != nil {
		t.Fatalf("Subscribe s3: %v", err)
	}
	defer sub3.Close()
	waitFor(t, 5*time.Second, hasMapping(s3, mapping2), "joiner primed with newest mapping")

	// New publisher session (frame timestamps on a new timeline): cache gone.
	p.Close()
	if _, _, err := r.StartPublish(id); err != nil {
		t.Fatalf("StartPublish reclaim: %v", err)
	}
	s4 := &fakeSender{}
	sub4, err := r.Subscribe(id, s4)
	if err != nil {
		t.Fatalf("Subscribe s4: %v", err)
	}
	defer sub4.Close()
	time.Sleep(50 * time.Millisecond) // priming is immediate; give the drain a beat
	if n := countMappings(s4); n != 0 {
		t.Errorf("post-restart joiner received %d clock mappings, want 0 (cache invalidated)", n)
	}
}

func TestClockMappingMalformedDropped(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	s := &fakeSender{}
	sub, err := r.Subscribe(id, s)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	bad := wire.AppendClockMapping(nil, 7)[:5] // truncated
	p.HandleDatagram(bad)

	if got := r.Stats().Totals.BadDatagrams; got != 1 {
		t.Errorf("BadDatagrams = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)
	if len(s.received()) != 0 {
		t.Errorf("malformed mapping was fanned out: %d datagrams", len(s.received()))
	}
}

// R17 W2: ResumePublish creates the hub for an unknown ID (the caller has
// verified a resume token), counting the create against MaxBroadcasts;
// existing hubs behave exactly like StartPublish reclaim.
func TestResumePublishCreatesAndLimits(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBroadcasts: 2})

	id, pub, err := r.ResumePublish("K7XQ2M")
	if err != nil {
		t.Fatalf("ResumePublish unknown ID: %v", err)
	}
	if id != "K7XQ2M" {
		t.Fatalf("ResumePublish id = %q, want K7XQ2M", id)
	}
	// The created hub is live: a second claim conflicts.
	if _, _, err := r.ResumePublish("K7XQ2M"); !errors.Is(err, ErrPublisherActive) {
		t.Fatalf("second claim err = %v, want ErrPublisherActive", err)
	}
	// Graced hub reclaims fine.
	pub.Close()
	if _, _, err := r.ResumePublish("K7XQ2M"); err != nil {
		t.Fatalf("reclaim of graced hub: %v", err)
	}

	// Creates count against MaxBroadcasts.
	if _, _, err := r.ResumePublish("ABC234"); err != nil {
		t.Fatalf("second broadcast create: %v", err)
	}
	if _, _, err := r.ResumePublish("DEF567"); !errors.Is(err, ErrMaxBroadcasts) {
		t.Fatalf("third create err = %v, want ErrMaxBroadcasts", err)
	}

	// Malformed IDs never create anything.
	if _, _, err := r.ResumePublish("!!!!"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed ID err = %v, want ErrNotFound", err)
	}
}

// R17 W3: EndBroadcast (the lease-deletion callback) force-expires a
// publisher-less broadcast — subscribers get the terminal 4000 exactly like
// a local grace expiry — but never touches one with a live publisher (we
// are its origin; a racing janitor must not kill a live broadcast). The
// lifecycle hooks fire outside the registry lock.
func TestEndBroadcastAndClusterHooks(t *testing.T) {
	var closedIDs []string
	var expiredIDs []string
	var mu sync.Mutex
	r := NewRegistry(discardLog, Options{
		OnPublisherClosed: func(id string) {
			mu.Lock()
			closedIDs = append(closedIDs, id)
			mu.Unlock()
		},
		OnBroadcastExpired: func(id string) {
			mu.Lock()
			expiredIDs = append(expiredIDs, id)
			mu.Unlock()
		},
	})

	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	if _, err := r.Subscribe(id, f); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Live publisher: EndBroadcast is a no-op (we host the origin).
	r.EndBroadcast(id)
	if _, closed := f.getCloseInfo(); closed {
		t.Fatal("EndBroadcast closed subscribers of a live broadcast")
	}

	pub.Close() // grace begins; the OnPublisherClosed hook fires
	mu.Lock()
	gotClosed := append([]string(nil), closedIDs...)
	mu.Unlock()
	if len(gotClosed) != 1 || gotClosed[0] != id {
		t.Fatalf("OnPublisherClosed calls = %v, want [%s]", gotClosed, id)
	}

	// The lease vanished cluster-wide: local viewers get the terminal 4000.
	r.EndBroadcast(id)
	code, closed := f.getCloseInfo()
	if !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
		t.Fatalf("subscriber close = (%d, %v), want (4000, true)", code, closed)
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("CheckSubscribe after EndBroadcast = %v, want ErrNotFound", err)
	}
	mu.Lock()
	gotExpired := append([]string(nil), expiredIDs...)
	mu.Unlock()
	if len(gotExpired) != 1 || gotExpired[0] != id {
		t.Errorf("OnBroadcastExpired calls = %v, want [%s]", gotExpired, id)
	}

	// Unknown / malformed IDs are silently ignored.
	r.EndBroadcast("AAAAAA")
	r.EndBroadcast("!!!")
}

// R17 post-review fix (PR #47): ExpireEdgeIfViewerless is the linger-out
// deletion — atomic with Subscribe under the registry lock, so a viewer that
// raced the linger window keeps the hub (the caller re-attaches for it)
// instead of being stranded on a pull-less hub or 4000'd mid-broadcast.
func TestExpireEdgeIfViewerless(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxSubscribers: 4})

	// Unknown / malformed IDs: nothing to keep — report the hub gone.
	if !r.ExpireEdgeIfViewerless("AAAAAA") || !r.ExpireEdgeIfViewerless("!!!") {
		t.Error("unknown/malformed ID should report the hub gone")
	}

	// An edge hub with an ACTIVE publisher (upstream pull mid-claim, or a
	// come-home racing the linger-out) is kept.
	id, epub, err := r.EdgePublish("K7XQ2M")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}
	if r.ExpireEdgeIfViewerless(id) {
		t.Error("edge hub with an active publisher reported gone")
	}

	// A viewer that raced the linger window keeps the hub, untouched.
	epub.Close()
	f := &fakeSender{}
	sub, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if r.ExpireEdgeIfViewerless(id) {
		t.Error("edge hub with a live viewer reported gone")
	}
	if _, closed := f.getCloseInfo(); closed {
		t.Error("viewer was closed by an expire that must keep the hub")
	}

	// Viewer-less and publisher-less: deleted, so the next viewer's
	// CheckSubscribe 404s into a fresh EnsureEdge.
	sub.Close()
	if !r.ExpireEdgeIfViewerless(id) {
		t.Error("viewerless edge hub not expired")
	}
	if err := r.CheckSubscribe(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("CheckSubscribe after linger expire = %v, want ErrNotFound", err)
	}

	// Origin hubs are NEVER expired here — their lifecycle belongs to the
	// grace timer.
	originID, opub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	opub.Close() // graced, viewerless — still not this path's business
	if r.ExpireEdgeIfViewerless(originID) {
		t.Error("origin hub expired by the edge linger path")
	}
	if err := r.CheckSubscribe(originID); err != nil {
		t.Errorf("origin hub gone after ExpireEdgeIfViewerless: %v", err)
	}
}

// R17 post-review fix (PR #47): a failed EdgePublish must not relabel the
// hub. The old order set edge=true before the claim, so racing a live origin
// publisher mislabeled it — flipping role metrics and loss attribution, and
// skipping the origin's lease lifecycle hooks at close/expiry.
func TestEdgePublishFailureKeepsOriginRole(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()

	if _, _, err := r.EdgePublish(id); !errors.Is(err, ErrPublisherActive) {
		t.Fatalf("EdgePublish on a live origin = %v, want ErrPublisherActive", err)
	}
	if role := r.Stats().Broadcasts[r.ObfuscateID(id)].Role; role != "origin" {
		t.Errorf("role after failed EdgePublish = %q, want origin (hub must not be relabeled)", role)
	}
}

// --- R19 reliable delivery (docs/24) ---------------------------------------

// waitCarrierRecords blocks until the sender's carriers hold at least n
// complete records (record writes happen on the subscriber's drain goroutine).
func waitCarrierRecords(t *testing.T, f *fakeSender, n int) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		return len(f.carrierRecords(t)) >= n
	}, fmt.Sprintf("%d carrier records", n))
}

// The core X2 criterion: a resilient subscriber receives every enqueued delta
// byte-identically and in order across a per-GOP carrier rotation, its
// datagram path stays silent, and keyframes keep their existing stream path.
func TestReliableSubscriberCarrierDelivery(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// GOP 1: keyframe (rotation) + config + two deltas.
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	cfg := configDgram(t, "vp8")
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	p.HandleDatagram(cfg)
	p.HandleDatagram(d1)
	p.HandleDatagram(d2)
	waitCarrierRecords(t, f, 3)

	// GOP 2: next keyframe rotates the carrier; two more deltas.
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY3"))
	d4 := chunkDgram(t, false, 4, 0, 1, "d4")
	d5 := chunkDgram(t, false, 5, 0, 1, "d5")
	p.HandleDatagram(d4)
	p.HandleDatagram(d5)
	waitCarrierRecords(t, f, 5)

	// Every datagram arrived as a record, byte-identical and in order.
	records := f.carrierRecords(t)
	want := [][]byte{cfg, d1, d2, d4, d5}
	if len(records) != len(want) {
		t.Fatalf("carrier records = %d, want %d", len(records), len(want))
	}
	for i := range want {
		if !bytes.Equal(records[i], want[i]) {
			t.Errorf("record %d = %x, want %x", i, records[i], want[i])
		}
	}

	// The datagram path is silent for a resilient subscriber.
	if got := f.received(); len(got) != 0 {
		t.Errorf("SendDatagram was called %d times for a reliable subscriber", len(got))
	}

	// Keyframes still travel their own streams, untouched.
	waitKeyframes(t, f, 2)

	// The rotation happened: two carriers, the first gracefully closed.
	carriers := f.carrierStreams()
	if len(carriers) != 2 {
		t.Fatalf("carrier streams = %d, want 2", len(carriers))
	}
	waitFor(t, time.Second, func() bool {
		closed, _ := carriers[0].state()
		return closed
	}, "first carrier closed on rotation")
	if closed, cancelled := carriers[1].state(); closed || cancelled {
		t.Errorf("second carrier closed=%v cancelled=%v, want live", closed, cancelled)
	}

	// Stats (docs/24 Decision 10): live view.
	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.ReliableSubscribers != 1 {
		t.Errorf("ReliableSubscribers = %d, want 1", stats.ReliableSubscribers)
	}
	if stats.CarrierStreams != 2 {
		t.Errorf("CarrierStreams = %d, want 2", stats.CarrierStreams)
	}
	if stats.CarrierRecords != 5 {
		t.Errorf("CarrierRecords = %d, want 5", stats.CarrierRecords)
	}
	if stats.EgressCarrierBytes == 0 {
		t.Error("EgressCarrierBytes = 0, want > 0")
	}
	if len(stats.SubscriberDetails) != 1 || !stats.SubscriberDetails[0].Reliable {
		t.Errorf("SubscriberDetails = %+v, want one reliable entry", stats.SubscriberDetails)
	}

	// Fold on close: the counters survive at the hub level.
	sub.Close()
	stats = r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.ReliableSubscribers != 0 {
		t.Errorf("ReliableSubscribers after close = %d, want 0", stats.ReliableSubscribers)
	}
	if stats.CarrierRecords != 5 {
		t.Errorf("CarrierRecords after close = %d, want 5 (folded)", stats.CarrierRecords)
	}
	if total := r.Stats().Totals.CarrierRecords; total != 5 {
		t.Errorf("Totals.CarrierRecords = %d, want 5", total)
	}
}

// A mixed audience: the resilient subscriber gets records, the normal one
// gets datagrams — neither delivery leaks into the other (X2 criterion).
func TestMixedAudienceIndependentDelivery(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	reliable := &fakeSender{}
	normal := &fakeSender{}
	subR, err := r.SubscribeReliable(id, reliable)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer subR.Close()
	subN, err := r.Subscribe(id, normal)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer subN.Close()

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	p.HandleDatagram(d1)
	p.HandleDatagram(d2)

	waitCarrierRecords(t, reliable, 2)
	waitFor(t, 5*time.Second, func() bool { return len(normal.received()) == 2 }, "normal subscriber datagrams")

	wantDatagrams(t, normal, [][]byte{d1, d2})
	if len(normal.carrierStreams()) != 0 {
		t.Errorf("normal subscriber got %d carrier streams, want 0", len(normal.carrierStreams()))
	}
	if got := reliable.received(); len(got) != 0 {
		t.Errorf("reliable subscriber got %d datagrams, want 0", len(got))
	}
	records := reliable.carrierRecords(t)
	if len(records) != 2 || !bytes.Equal(records[0], d1) || !bytes.Equal(records[1], d2) {
		t.Errorf("reliable records = %d, want [d1 d2]", len(records))
	}
	waitKeyframes(t, reliable, 1)
	waitKeyframes(t, normal, 1)

	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.Subscribers != 2 || stats.ReliableSubscribers != 1 {
		t.Errorf("Subscribers = %d / ReliableSubscribers = %d, want 2 / 1",
			stats.Subscribers, stats.ReliableSubscribers)
	}
}

// A carrier whose write stalls past KeyframeWriteTimeout is cancelled; the
// GOP's remaining records are dropped and delivery resumes on the next
// rotation's fresh carrier (docs/24 Decision 5: drops-over-stalls at GOP
// granularity).
func TestStalledCarrierCancelledAfterDeadline(t *testing.T) {
	r := NewRegistry(discardLog, Options{KeyframeWriteTimeout: 50 * time.Millisecond})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// Healthy GOP 1.
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	p.HandleDatagram(d1)
	waitCarrierRecords(t, f, 1)

	// Stall the peer: the next record write blocks until its deadline.
	f.setCarBlock(make(chan struct{}))
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	p.HandleDatagram(d2)
	waitFor(t, 5*time.Second, func() bool {
		_, cancelled := f.carrierStreams()[0].state()
		return cancelled
	}, "stalled carrier cancelled after write deadline")
	waitFor(t, time.Second, func() bool { return sub.carrierRecordsDropped.Load() >= 1 }, "stalled record dropped")

	// The link recovers; the next GOP delivers on a fresh carrier.
	f.setCarBlock(nil)
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY3"))
	d4 := chunkDgram(t, false, 4, 0, 1, "d4")
	p.HandleDatagram(d4)
	waitCarrierRecords(t, f, 2)

	records := f.carrierRecords(t)
	if !bytes.Equal(records[len(records)-1], d4) {
		t.Errorf("last record = %x, want d4 %x", records[len(records)-1], d4)
	}
	if n := len(f.carrierStreams()); n != 2 {
		t.Errorf("carrier streams = %d, want 2 (stalled + fresh)", n)
	}
}

// A stalled carrier record must be abandoned on the carrier's own GOP-scale
// deadline, never on the keyframe stall tolerance (docs/24 finding 12, review
// finding BACKPRESSURE-2). The two writes are not comparable: a keyframe is
// ~236 KB on a stream of its own, written by a goroutine that blocks nobody,
// so an operator can rationally be patient with it — while one ~20-byte delta
// record is written by the drain goroutine that owns the subscriber's ENTIRE
// delta path, so its deadline is the length of the freeze every later delta
// inherits. Inheriting a patient keyframe timeout makes the mode built to hide
// stalls produce one lasting several GOPs.
func TestStalledCarrierAbandonedOnItsOwnDeadline(t *testing.T) {
	// A fleet tuned to be patient with keyframes on slow links.
	const keyframeTimeout = 5 * time.Second
	// Post-fix the drain gives up after CarrierWriteTimeout (~one GOP); this
	// bound is far above that and far below keyframeTimeout, so neither a slow
	// CI runner nor a lucky scheduling win can blur the two.
	const stallBound = 2 * time.Second

	r := NewRegistry(discardLog, Options{KeyframeWriteTimeout: keyframeTimeout})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// A healthy GOP first: the carrier is open and delivering.
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "d1"))
	waitCarrierRecords(t, f, 1)

	// The peer's stream flow-control window closes: the next record write parks.
	f.setCarBlock(make(chan struct{}))
	p.HandleDatagram(chunkDgram(t, false, 2, 0, 1, "d2"))
	waitFor(t, stallBound, func() bool {
		_, cancelled := f.carrierStreams()[0].state()
		return cancelled
	}, fmt.Sprintf("the stalled carrier to be abandoned within %s (KeyframeWriteTimeout is %s — a delta record must not inherit it)",
		stallBound, keyframeTimeout))

	// And the drain is free again: the next GOP delivers on a fresh carrier
	// without waiting out the keyframe timeout.
	f.setCarBlock(nil)
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY3"))
	d4 := chunkDgram(t, false, 4, 0, 1, "d4")
	p.HandleDatagram(d4)
	waitFor(t, stallBound, func() bool { return len(f.carrierRecords(t)) >= 2 }, "delivery resumed on the next rotation")
	records := f.carrierRecords(t)
	if !bytes.Equal(records[len(records)-1], d4) {
		t.Errorf("last record = %x, want d4 %x", records[len(records)-1], d4)
	}
}

// The carrier's deadline is its own bound — except where an operator has
// configured a keyframe stall tolerance *tighter* than it. Patience on the
// drain that owns the whole delta path may shrink with the fleet's setting,
// never grow past a GOP (docs/24 finding 12).
func TestCarrierWriteTimeoutTakesTheTighterBound(t *testing.T) {
	if CarrierWriteTimeout >= time.Second {
		t.Fatalf("CarrierWriteTimeout = %s, want < the 1s default KeyframeWriteTimeout — "+
			"a carrier record must never wait as long as a keyframe", CarrierWriteTimeout)
	}
	for _, tc := range []struct {
		name     string
		keyframe time.Duration
		want     time.Duration
	}{
		{"default keyframe timeout", time.Second, CarrierWriteTimeout},
		{"patient fleet", 5 * time.Second, CarrierWriteTimeout},
		{"operator tighter than a GOP", 200 * time.Millisecond, 200 * time.Millisecond},
		{"unset", 0, CarrierWriteTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{KeyframeWriteTimeout: tc.keyframe}
			if got := opts.carrierWriteTimeout(); got != tc.want {
				t.Errorf("carrierWriteTimeout() = %s, want %s (KeyframeWriteTimeout %s)", got, tc.want, tc.keyframe)
			}
		})
	}
}

// Carrier stream-open failures feed the same consecutive-failure eviction
// streak as keyframe opens: a zombie subscriber (all opens failing — the
// exhausted-stream-credit signature) is evicted with 4001 (docs/24
// Decision 5). One carrier open is attempted per rotation, so with both kinds
// failing each GOP contributes two to the streak.
func TestCarrierOpenFailuresFeedEvictionStreak(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	openErr := errors.New("stream credit exhausted")
	f := &fakeSender{kfOpenErr: openErr, carOpenErr: openErr}
	if _, err := r.SubscribeReliable(id, f); err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}

	for i := 0; i < KeyframeOpenFailEvictThreshold/2; i++ {
		frameID := uint32(i * 2)
		ingestKeyframe(t, p, keyframeMsg(t, frameID, "vp8", "KEY"))
		p.HandleDatagram(chunkDgram(t, false, frameID+1, 0, 1, "d"))
		// Each rotation permits exactly one carrier open attempt; wait for it
		// so the next cycle's failure lands on a fresh rotation.
		wantOpens := i + 1
		waitFor(t, 5*time.Second, func() bool { return f.carrierOpens() >= wantOpens }, "carrier open attempt")
	}

	waitFor(t, 5*time.Second, func() bool {
		code, closed := f.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "zombie reliable subscriber evicted with 4001")
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].Subscribers == 0
	}, "evicted subscriber removed from hub")
}

// A resilient subscriber whose carrier drain falls behind overflows the same
// bounded queue a slow datagram viewer does — but it is a different failure
// (docs/24 finding 11, review finding PRODUCT-3): the hole lands in a stream
// the viewer treats as reliable and in-order, so it freezes to the next
// keyframe. The overflow is counted apart from the datagram viewer's drops,
// while still counting in the shared drop total, and survives the fold on
// close.
func TestReliableQueueOverflowCountedApartFromDatagramDrops(t *testing.T) {
	const queueDepth = 4
	const deltas = 20

	// A write deadline far past the test: the parked carrier write never times
	// out on its own, so the backlog behind it is deterministic.
	r := NewRegistry(discardLog, Options{QueueDepth: queueDepth, KeyframeWriteTimeout: time.Minute})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	reliable := &fakeSender{}
	reliable.setCarBlock(make(chan struct{})) // the carrier write parks
	subR, err := r.SubscribeReliable(id, reliable)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	// The control: a normal viewer, equally stuck, whose drops must stay out of
	// the carrier bucket.
	normal := &fakeSender{block: make(chan struct{})}
	subN, err := r.Subscribe(id, normal)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Fan-out is synchronous with HandleDatagram, so everything past the queue's
	// capacity (plus the one datagram each parked drain holds) overflows.
	for i := range deltas {
		p.HandleDatagram(chunkDgram(t, false, uint32(i+1), 0, 1, fmt.Sprintf("d%03d", i)))
	}
	minDrops := uint64(deltas - queueDepth - 1)

	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if subR.Dropped() < minDrops {
		t.Fatalf("reliable subscriber dropped %d, want >= %d — the queue did not overflow", subR.Dropped(), minDrops)
	}
	if subN.Dropped() < minDrops {
		t.Fatalf("datagram subscriber dropped %d, want >= %d — the control queue did not overflow", subN.Dropped(), minDrops)
	}
	if stats.CarrierQueueOverflow != subR.Dropped() {
		t.Errorf("CarrierQueueOverflow = %d, want %d (the reliable subscriber's drops)",
			stats.CarrierQueueOverflow, subR.Dropped())
	}
	if stats.DatagramsDropped != subR.Dropped()+subN.Dropped() {
		t.Errorf("DatagramsDropped = %d, want %d — the carrier bucket is a slice of the drop total, not a second budget",
			stats.DatagramsDropped, subR.Dropped()+subN.Dropped())
	}
	// Nothing reached a carrier: these deltas were dropped before they ever
	// became records, which is why carrierRecordsDropped can't cover them.
	if stats.CarrierRecordsDropped != 0 {
		t.Errorf("CarrierRecordsDropped = %d, want 0 (drops happened at the queue)", stats.CarrierRecordsDropped)
	}

	// Per-subscriber detail: only the reliable entry carries the overflow.
	var seenReliable, seenNormal bool
	for _, d := range stats.SubscriberDetails {
		if d.Reliable {
			seenReliable = true
			if d.CarrierQueueOverflow != subR.Dropped() {
				t.Errorf("reliable detail CarrierQueueOverflow = %d, want %d", d.CarrierQueueOverflow, subR.Dropped())
			}
			continue
		}
		seenNormal = true
		if d.CarrierQueueOverflow != 0 {
			t.Errorf("datagram detail CarrierQueueOverflow = %d, want 0", d.CarrierQueueOverflow)
		}
		if d.Dropped == 0 {
			t.Error("datagram detail Dropped = 0, want the control's drops")
		}
	}
	if !seenReliable || !seenNormal {
		t.Errorf("SubscriberDetails = %+v, want one reliable and one datagram entry", stats.SubscriberDetails)
	}

	// Fold on close: the counter survives its subscriber (CODE-REVIEW.md).
	wantOverflow := subR.Dropped()
	close(normal.block)
	subN.Close()
	subR.Close()
	stats = r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.CarrierQueueOverflow != wantOverflow {
		t.Errorf("CarrierQueueOverflow after close = %d, want %d (folded)", stats.CarrierQueueOverflow, wantOverflow)
	}
	if total := r.Stats().Totals.CarrierQueueOverflow; total != wantOverflow {
		t.Errorf("Totals.CarrierQueueOverflow = %d, want %d", total, wantOverflow)
	}
}

// The egress bandwidth cap charges carrier records like datagrams — reliable
// delivery is not a cap bypass (docs/24 Decision 5). Over-cap records are
// dropped at the drain and recorded under the bandwidth reason.
func TestCarrierBandwidthCapDropsRecords(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 100})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// Exhaust the bucket so the next record can't pass.
	r.limiter.mu.Lock()
	r.limiter.tokens = 0
	r.limiter.last = time.Now().Add(time.Hour) // no refill during the test
	r.limiter.mu.Unlock()

	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	p.HandleDatagram(d1)
	waitFor(t, 5*time.Second, func() bool { return sub.Dropped() == 1 }, "over-cap record dropped")

	if n := len(f.carrierStreams()); n != 0 {
		t.Errorf("carrier streams = %d, want 0 (drop happens before any open)", n)
	}
	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.BandwidthDroppedDatagrams != 1 {
		t.Errorf("BandwidthDroppedDatagrams = %d, want 1", stats.BandwidthDroppedDatagrams)
	}
	if want := uint64(wire.CarrierPrologueSize + wire.CarrierRecordHeaderSize + len(d1)); stats.BandwidthDroppedBytes != want {
		t.Errorf("BandwidthDroppedBytes = %d, want %d (record incl. prefix, plus the prologue this record would have opened with)",
			stats.BandwidthDroppedBytes, want)
	}
}

// freezeBandwidthBudget re-tunes the registry's egress limiter into a large,
// effectively static budget: a rate this low refills a thousandth of a byte per
// second, so what the drain debited over a test is exactly what it charged —
// assertable as a number rather than as an inequality. The rate must stay above
// zero; consume() reads a non-positive rate as "no limit" and never touches the
// balance at all.
func freezeBandwidthBudget(t *testing.T, r *Registry) {
	t.Helper()
	if r.limiter == nil {
		t.Fatal("registry has no bandwidth limiter (set Options.MaxBandwidthBytes)")
	}
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()
	r.limiter.rate = 0.001
	r.limiter.burst = 1 << 20
	r.limiter.tokens = 1 << 20
	r.limiter.last = time.Now()
}

// bandwidthBalance reads the frozen budget's remaining tokens.
func bandwidthBalance(r *Registry) float64 {
	r.limiter.mu.Lock()
	defer r.limiter.mu.Unlock()
	return r.limiter.tokens
}

// A record the drain has already decided not to write must not debit the egress
// cap (docs/24 finding 13, review finding BW-CHARGE). That cap is a single
// process-wide token bucket shared by every broadcast on the pod, so bytes
// charged for records that never reach a stream throttle *other* broadcasts'
// viewers — a phantom debit. The shape it takes is a whole GOP tail: once a
// carrier open fails the rest of the GOP is dropped at the top of the loop, and
// each of those drops used to pay full freight on the way out.
func TestDeadCarrierRecordsDoNotDebitEgressCap(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 1 << 20})
	freezeBandwidthBudget(t, r)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// Every carrier open fails, so the GOP dies on its first record and the
	// whole tail behind it is dropped without a stream ever existing.
	f := &fakeSender{carOpenErr: errors.New("stream credit exhausted")}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// sendKeyframe charges inline on the ingest goroutine (only the write is
	// deferred), so once this returns the keyframe's own debit has landed and
	// the balance below is a clean baseline for the records.
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	before := bandwidthBalance(r)

	const tail = 5
	deltas := make([][]byte, tail)
	for i := range deltas {
		deltas[i] = chunkDgram(t, false, uint32(i+1), 0, 1, "d")
		p.HandleDatagram(deltas[i])
	}
	waitFor(t, 5*time.Second, func() bool {
		return sub.carrierRecordsDropped.Load() == tail
	}, "the whole GOP tail dropped for a dead carrier")

	// One open is attempted per rotation, so exactly one record was ever
	// destined for the wire: its own bytes plus the prologue that open would
	// have written. The four behind it died at the top of the loop and are
	// worth nothing.
	want := wire.CarrierPrologueSize + wire.CarrierRecordHeaderSize + len(deltas[0])
	if got := int(math.Round(before - bandwidthBalance(r))); got != want {
		t.Errorf("egress budget debited %d bytes for %d records the drain refused to write, want %d "+
			"(only the record that reached an open attempt) — a dropped record must not spend a budget shared with every other broadcast",
			got, tail, want)
	}
	// And a dead carrier is not a bandwidth failure: the cap was never the
	// reason anything here was dropped, and must not read as though it were.
	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.BandwidthDroppedDatagrams != 0 {
		t.Errorf("BandwidthDroppedDatagrams = %d, want 0 (these records died with the carrier, not against the cap)",
			stats.BandwidthDroppedDatagrams)
	}
	if stats.EgressCarrierBytes != 0 {
		t.Errorf("EgressCarrierBytes = %d, want 0 (nothing reached a stream)", stats.EgressCarrierBytes)
	}
}

// The carrier prologue rides the same budget as the records behind it (docs/24
// finding 13, review finding BACKPRESSURE-4). Two bytes per GOP is nothing in
// itself, but a cap with a hole in it is not a cap, and the invariant that pays
// for the rest of the accounting being reviewable is that every carrier byte
// the relay puts on the wire is charged exactly once — once per GOP for the
// prologue, once per record for a record.
func TestCarrierPrologueChargedOncePerGOP(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxBandwidthBytes: 1 << 20})
	freezeBandwidthBudget(t, r)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	sub, err := r.SubscribeReliable(id, f)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	defer sub.Close()

	// spend charges one delta through and reports what the budget lost. The
	// record's bytes are visible to the fake only after its write, which the
	// charge precedes — so by the time the record shows up the debit is final.
	records := 0
	spend := func(dgram []byte) int {
		t.Helper()
		before := bandwidthBalance(r)
		p.HandleDatagram(dgram)
		records++
		waitCarrierRecords(t, f, records)
		return int(math.Round(before - bandwidthBalance(r)))
	}
	record := func(dgram []byte) int { return wire.CarrierRecordHeaderSize + len(dgram) }

	// GOP 1. The first record lazily opens the carrier and so pays for the
	// prologue too; the second rides a carrier that already exists.
	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	if got, want := spend(d1), wire.CarrierPrologueSize+record(d1); got != want {
		t.Errorf("first record of a GOP debited %d bytes, want %d (record + the prologue its open writes)", got, want)
	}
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	if got, want := spend(d2), record(d2); got != want {
		t.Errorf("second record of a GOP debited %d bytes, want %d (no second prologue on one carrier)", got, want)
	}

	// GOP 2. The keyframe rotates the carrier, so the next record opens a
	// fresh one and pays for a fresh prologue.
	ingestKeyframe(t, p, keyframeMsg(t, 3, "vp8", "KEY3"))
	d4 := chunkDgram(t, false, 4, 0, 1, "d4")
	if got, want := spend(d4), wire.CarrierPrologueSize+record(d4); got != want {
		t.Errorf("first record after a rotation debited %d bytes, want %d (the new carrier's prologue is charged too)", got, want)
	}

	// Closing the loop: what the cap was charged is what the operator is told
	// went out. If these two ever disagree, one of them is lying.
	wantEgress := uint64(2*wire.CarrierPrologueSize + record(d1) + record(d2) + record(d4))
	if got := r.Stats().Broadcasts[r.ObfuscateID(id)].EgressCarrierBytes; got != wantEgress {
		t.Errorf("EgressCarrierBytes = %d, want %d (the bytes charged to the cap)", got, wantEgress)
	}
}
