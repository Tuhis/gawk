package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSender records every datagram it is asked to send. If block is
// non-nil, SendDatagram waits on it first, simulating a stuck peer.
type fakeSender struct {
	mu    sync.Mutex
	got   [][]byte
	block chan struct{}
	err   error
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

func (f *fakeSender) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.got...)
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

// wantDatagrams asserts that the sender received exactly these datagrams in
// this order. Call only after the subscriber is closed (Close flushes the
// queue).
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
	h := New(discardLog, Options{})
	p, err := h.StartPublish()
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := h.Subscribe(f)
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

	st := h.Stats()
	if st.FramesRelayed != 1 || st.DatagramsRelayed != 3 || st.BadDatagrams != 0 {
		t.Errorf("stats = %+v, want 1 frame, 3 datagrams, 0 bad", st)
	}
}

func TestSecondPublisherRejected(t *testing.T) {
	h := New(discardLog, Options{})
	p, err := h.StartPublish()
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, err := h.StartPublish(); !errors.Is(err, ErrPublisherActive) {
		t.Fatalf("second StartPublish error = %v, want ErrPublisherActive", err)
	}
	p.Close()
	p.Close() // idempotent
	if _, err := h.StartPublish(); err != nil {
		t.Fatalf("StartPublish after Close: %v", err)
	}
}

func TestSubscribeFull(t *testing.T) {
	h := New(discardLog, Options{MaxSubscribers: 2})
	s1, _ := h.Subscribe(&fakeSender{})
	if h.Full() {
		t.Error("Full() = true with 1 of 2 slots used")
	}
	if _, err := h.Subscribe(&fakeSender{}); err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	if !h.Full() {
		t.Error("Full() = false with 2 of 2 slots used")
	}
	if _, err := h.Subscribe(&fakeSender{}); !errors.Is(err, ErrFull) {
		t.Fatalf("third Subscribe error = %v, want ErrFull", err)
	}
	s1.Close()
	if _, err := h.Subscribe(&fakeSender{}); err != nil {
		t.Fatalf("Subscribe after a slot freed: %v", err)
	}
}

func TestLateJoinerPrimed(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()

	cfg := configDgram(t, "avc1.42E02A")
	kf := [][]byte{
		chunkDgram(t, true, 10, 0, 3, "k0"),
		chunkDgram(t, true, 10, 1, 3, "k1"),
		chunkDgram(t, true, 10, 2, 3, "k2"),
	}
	p.HandleDatagram(cfg)
	for _, d := range kf {
		p.HandleDatagram(d)
	}

	// Joins after the keyframe was fully relayed.
	f := &fakeSender{}
	s, err := h.Subscribe(f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	live := chunkDgram(t, false, 11, 0, 1, "delta")
	p.HandleDatagram(live)
	s.Close()

	// Primed with [config, kf chunks in order] before any live data.
	wantDatagrams(t, f, [][]byte{cfg, kf[0], kf[1], kf[2], live})
}

func TestPrimingSurvivesPublisherClose(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()
	cfg := configDgram(t, "vp8")
	kf := chunkDgram(t, true, 0, 0, 1, "kf")
	p.HandleDatagram(cfg)
	p.HandleDatagram(kf)
	p.Close()

	f := &fakeSender{}
	s, err := h.Subscribe(f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	s.Close()
	wantDatagrams(t, f, [][]byte{cfg, kf})
}

func TestIncompleteKeyframeNeverCached(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()

	// Complete keyframe 5, then keyframe 6 missing chunk 1 of 3.
	kf5 := [][]byte{
		chunkDgram(t, true, 5, 0, 2, "a"),
		chunkDgram(t, true, 5, 1, 2, "b"),
	}
	for _, d := range kf5 {
		p.HandleDatagram(d)
	}
	p.HandleDatagram(chunkDgram(t, true, 6, 0, 3, "x"))
	p.HandleDatagram(chunkDgram(t, true, 6, 2, 3, "z"))

	if st := h.Stats(); st.CachedKeyframeID != 5 || st.CachedKeyframeChunks != 2 {
		t.Fatalf("cached keyframe = id %d (%d chunks), want id 5 (2 chunks)",
			st.CachedKeyframeID, st.CachedKeyframeChunks)
	}

	// A new joiner is primed with the last *complete* keyframe.
	f := &fakeSender{}
	s, _ := h.Subscribe(f)
	s.Close()
	wantDatagrams(t, f, kf5)

	// Keyframe 7 starts: 6's assembly is abandoned; completing 7 caches it.
	kf7 := chunkDgram(t, true, 7, 0, 1, "w")
	p.HandleDatagram(kf7)
	if st := h.Stats(); st.CachedKeyframeID != 7 || st.CachedKeyframeChunks != 1 {
		t.Fatalf("cached keyframe = id %d (%d chunks), want id 7 (1 chunk)",
			st.CachedKeyframeID, st.CachedKeyframeChunks)
	}
	// The straggler chunk of 6 must not resurrect the abandoned assembly.
	p.HandleDatagram(chunkDgram(t, true, 6, 1, 3, "y"))
	if st := h.Stats(); st.CachedKeyframeID != 7 {
		t.Fatalf("cached keyframe id = %d after straggler, want 7", st.CachedKeyframeID)
	}
}

func TestDuplicateAndReorderedKeyframeChunks(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()

	// Out of order and with a duplicate: must still complete exactly once.
	p.HandleDatagram(chunkDgram(t, true, 3, 2, 3, "c"))
	p.HandleDatagram(chunkDgram(t, true, 3, 0, 3, "a"))
	p.HandleDatagram(chunkDgram(t, true, 3, 0, 3, "a"))
	p.HandleDatagram(chunkDgram(t, true, 3, 1, 3, "b"))

	f := &fakeSender{}
	s, _ := h.Subscribe(f)
	s.Close()
	got := f.received()
	if len(got) != 3 {
		t.Fatalf("primed with %d chunks, want 3 (in order)", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		_, payload, err := wire.ParseVideoChunk(got[i])
		if err != nil || string(payload) != want {
			t.Errorf("primed chunk %d payload = %q (err %v), want %q", i, payload, err, want)
		}
	}
}

func TestConfigReEmittedBeforeKeyframe(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()
	cfg := configDgram(t, "avc1.42E02A")
	p.HandleDatagram(cfg)

	f := &fakeSender{}
	s, _ := h.Subscribe(f)

	kf0 := chunkDgram(t, true, 1, 0, 2, "k0")
	kf1 := chunkDgram(t, true, 1, 1, 2, "k1")
	p.HandleDatagram(kf0)
	p.HandleDatagram(kf1)
	s.Close()

	// Priming delivered cfg once; the keyframe's chunk 0 re-emits it.
	wantDatagrams(t, f, [][]byte{cfg, cfg, kf0, kf1})
}

func TestNewConfigReplacesCache(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()
	p.HandleDatagram(configDgram(t, "avc1.42E02A"))
	cfg2 := configDgram(t, "vp09.00.40.08")
	p.HandleDatagram(cfg2)

	f := &fakeSender{}
	s, _ := h.Subscribe(f)
	s.Close()
	wantDatagrams(t, f, [][]byte{cfg2})
}

func TestBadDatagramsDroppedAndCounted(t *testing.T) {
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()
	f := &fakeSender{}
	s, _ := h.Subscribe(f)

	bad := [][]byte{
		nil,                           // too short for prefix
		{0x02, 0x01},                  // unknown version
		{0x01, 0x7F, 0x00},            // unknown type
		{0x01, 0x01, 0x00, 0x00},      // video chunk too short
		{0x01, 0x02, 0x00, 0x09, 'v'}, // codecLen overrun
	}
	for _, d := range bad {
		p.HandleDatagram(d)
	}
	s.Close()
	wantDatagrams(t, f, nil)
	if st := h.Stats(); st.BadDatagrams != uint64(len(bad)) {
		t.Errorf("BadDatagrams = %d, want %d", st.BadDatagrams, len(bad))
	}
}

func TestSlowSubscriberDropsHealthyPeerUnaffected(t *testing.T) {
	const queueDepth = 8
	const n = 100

	h := New(discardLog, Options{QueueDepth: queueDepth})
	p, _ := h.StartPublish()

	healthy := &fakeSender{}
	blocked := &fakeSender{block: make(chan struct{})}
	sh, _ := h.Subscribe(healthy)
	sb, _ := h.Subscribe(blocked)

	var sent [][]byte
	for i := range n {
		d := chunkDgram(t, false, uint32(i), 0, 1, fmt.Sprintf("f%03d", i))
		sent = append(sent, d)
		p.HandleDatagram(d)
		// Let the healthy drain goroutine keep pace so its small queue
		// never overflows; the blocked peer stays stuck the whole time.
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

	// Healthy peer got 100%, in order.
	sh.Close()
	wantDatagrams(t, healthy, sent)

	// The blocked subscriber dropped everything beyond its stuck send plus
	// one queue's worth.
	if got := sb.Dropped(); got < n-queueDepth-1 {
		t.Errorf("blocked subscriber dropped %d datagrams, want >= %d", got, n-queueDepth-1)
	}
	close(blocked.block) // unstick so Close's drain flush can finish
	sb.Close()

	if st := h.Stats(); st.DatagramsDropped == 0 {
		t.Error("hub stats did not accumulate drops from the closed subscriber")
	}
}

func TestSubscriberCloseIdempotentAndConcurrent(t *testing.T) {
	h := New(discardLog, Options{})
	f := &fakeSender{}
	s, _ := h.Subscribe(f)

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
	h := New(discardLog, Options{})
	p, _ := h.StartPublish()
	f := &fakeSender{err: errors.New("session gone")}
	s, _ := h.Subscribe(f)

	for i := range 3 {
		p.HandleDatagram(chunkDgram(t, false, uint32(i), 0, 1, "x"))
	}
	s.Close() // must not hang even though every send failed
	if got := s.sendErrors.Load(); got != 3 {
		t.Errorf("sendErrors = %d, want 3", got)
	}
}

// TestConcurrentPublishSubscribeChurn exercises the locking under -race:
// a publisher streams while subscribers join and leave.
func TestConcurrentPublishSubscribeChurn(t *testing.T) {
	h := New(discardLog, Options{QueueDepth: 32})
	p, _ := h.StartPublish()

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
				s, err := h.Subscribe(&fakeSender{})
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
