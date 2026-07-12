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

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSender records every datagram and close code.
type fakeSender struct {
	mu        sync.Mutex
	got       [][]byte
	block     chan struct{}
	err       error
	closeCode uint32
	closed    bool
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

func (f *fakeSender) getCloseInfo() (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode, f.closed
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
	bst := st.Broadcasts[broadcastid.Obfuscate(id)]
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

func TestLateJoinerPrimed(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

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

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	live := chunkDgram(t, false, 11, 0, 1, "delta")
	p.HandleDatagram(live)
	s.Close()

	wantDatagrams(t, f, [][]byte{cfg, kf[0], kf[1], kf[2], live})
}

func TestPrimingSurvivesPublisherClose(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p, _ := r.StartPublish("")
	cfg := configDgram(t, "vp8")
	kf := chunkDgram(t, true, 0, 0, 1, "kf")
	p.HandleDatagram(cfg)
	p.HandleDatagram(kf)
	p.Close()

	f := &fakeSender{}
	s, err := r.Subscribe(id, f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	s.Close()
	wantDatagrams(t, f, [][]byte{cfg, kf})
}

func TestIncompleteKeyframeNeverCached(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	kf5 := [][]byte{
		chunkDgram(t, true, 5, 0, 2, "a"),
		chunkDgram(t, true, 5, 1, 2, "b"),
	}
	for _, d := range kf5 {
		p.HandleDatagram(d)
	}
	p.HandleDatagram(chunkDgram(t, true, 6, 0, 3, "x"))
	p.HandleDatagram(chunkDgram(t, true, 6, 2, 3, "z"))

	st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
	if st.CachedKeyframeID != 5 || st.CachedKeyframeChunks != 2 {
		t.Fatalf("cached keyframe = id %d (%d chunks), want id 5 (2 chunks)",
			st.CachedKeyframeID, st.CachedKeyframeChunks)
	}

	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)
	s.Close()
	wantDatagrams(t, f, kf5)

	kf7 := chunkDgram(t, true, 7, 0, 1, "w")
	p.HandleDatagram(kf7)
	st = r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
	if st.CachedKeyframeID != 7 || st.CachedKeyframeChunks != 1 {
		t.Fatalf("cached keyframe = id %d (%d chunks), want id 7 (1 chunk)",
			st.CachedKeyframeID, st.CachedKeyframeChunks)
	}
	p.HandleDatagram(chunkDgram(t, true, 6, 1, 3, "y"))
	st = r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
	if st.CachedKeyframeID != 7 {
		t.Fatalf("cached keyframe id = %d after straggler, want 7", st.CachedKeyframeID)
	}
}

func TestDuplicateAndReorderedKeyframeChunks(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")

	p.HandleDatagram(chunkDgram(t, true, 3, 2, 3, "c"))
	p.HandleDatagram(chunkDgram(t, true, 3, 0, 3, "a"))
	p.HandleDatagram(chunkDgram(t, true, 3, 0, 3, "a"))
	p.HandleDatagram(chunkDgram(t, true, 3, 1, 3, "b"))

	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)
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
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")
	cfg := configDgram(t, "avc1.42E02A")
	p.HandleDatagram(cfg)

	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)

	kf0 := chunkDgram(t, true, 1, 0, 2, "k0")
	kf1 := chunkDgram(t, true, 1, 1, 2, "k1")
	p.HandleDatagram(kf0)
	p.HandleDatagram(kf1)
	s.Close()

	wantDatagrams(t, f, [][]byte{cfg, cfg, kf0, kf1})
}

func TestNewConfigReplacesCache(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")
	p.HandleDatagram(configDgram(t, "avc1.42E02A"))
	cfg2 := configDgram(t, "vp09.00.40.08")
	p.HandleDatagram(cfg2)

	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)
	s.Close()
	wantDatagrams(t, f, [][]byte{cfg2})
}

func TestPublisherRestartResetsCaches(t *testing.T) {
	r := NewRegistry(discardLog, Options{BroadcastGrace: 5 * time.Minute})
	id, p1, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	cfg1 := configDgram(t, "avc1.42E02A")
	p1.HandleDatagram(cfg1)
	kf1 := [][]byte{
		chunkDgram(t, true, 41, 0, 2, "old0"),
		chunkDgram(t, true, 41, 1, 2, "old1"),
	}
	for _, d := range kf1 {
		p1.HandleDatagram(d)
	}
	p1.Close()

	st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
	if !st.HasConfig || st.CachedKeyframeID != 41 || st.CachedKeyframeChunks != 2 {
		t.Fatalf("caches after publisher close = %+v, want config + keyframe 41 (2 chunks)", st)
	}
	away := &fakeSender{}
	sa, _ := r.Subscribe(id, away)
	sa.Close()
	wantDatagrams(t, away, [][]byte{cfg1, kf1[0], kf1[1]})

	_, p2, err := r.StartPublish(id)
	if err != nil {
		t.Fatalf("StartPublish after restart: %v", err)
	}
	st = r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
	if st.HasConfig || st.CachedKeyframeID != 0 || st.CachedKeyframeChunks != 0 || st.CachedKeyframeBytes != 0 {
		t.Fatalf("caches survived publisher restart: %+v", st)
	}

	early := &fakeSender{}
	se, _ := r.Subscribe(id, early)

	cfg2 := configDgram(t, "vp09.00.40.08")
	kf2 := chunkDgram(t, true, 0, 0, 1, "new")
	p2.HandleDatagram(cfg2)
	p2.HandleDatagram(kf2)
	se.Close()
	wantDatagrams(t, early, [][]byte{cfg2, cfg2, kf2})

	late := &fakeSender{}
	sl, _ := r.Subscribe(id, late)
	sl.Close()
	wantDatagrams(t, late, [][]byte{cfg2, kf2})
}

func TestConfigPrecedesEveryKeyframe(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, _ := r.StartPublish("")
	f := &fakeSender{}
	s, _ := r.Subscribe(id, f)

	cfg1 := configDgram(t, "avc1.42E02A")
	cfg2 := configDgram(t, "vp09.00.40.08")
	k0 := [][]byte{chunkDgram(t, true, 0, 0, 2, "k0a"), chunkDgram(t, true, 0, 1, 2, "k0b")}
	d1 := chunkDgram(t, false, 1, 0, 1, "d1")
	d2 := chunkDgram(t, false, 2, 0, 1, "d2")
	k3 := [][]byte{chunkDgram(t, true, 3, 0, 2, "k3a"), chunkDgram(t, true, 3, 1, 2, "k3b")}
	d4 := chunkDgram(t, false, 4, 0, 1, "d4")

	for _, d := range [][]byte{cfg1, k0[0], k0[1], d1, d2, cfg2, k3[0], k3[1], d4} {
		p.HandleDatagram(d)
	}
	s.Close()

	wantDatagrams(t, f, [][]byte{
		cfg1, cfg1, k0[0], k0[1], d1, d2, cfg2, cfg2, k3[0], k3[1], d4,
	})
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
	st := r.Stats().Broadcasts[broadcastid.Obfuscate(id)]
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
		keyframe := frameID%60 == 0
		if keyframe {
			want = append(want, cfg)
		}
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
	if bst1 := st.Broadcasts[broadcastid.Obfuscate(id1)]; bst1.DatagramsRelayed != 1 {
		t.Errorf("broadcast 1 expected 1 relayed, got %d", bst1.DatagramsRelayed)
	}
	if bst2 := st.Broadcasts[broadcastid.Obfuscate(id2)]; bst2.DatagramsRelayed != 1 {
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

