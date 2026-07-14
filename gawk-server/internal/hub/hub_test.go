package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/wire"
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
	if k.parent.kfWriteErr != nil {
		return 0, k.parent.kfWriteErr
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
