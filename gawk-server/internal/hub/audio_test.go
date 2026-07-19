package hub

// R15 system audio (docs/20 Decision 4): the hub relays AudioFrame datagrams
// verbatim, caches + primes + invalidates AudioConfig on the ClockMapping
// lifecycle (both invalidation sites), never lets audio touch the video
// ingress-loss window or framesRelayed, re-ingests audio on edge hubs through
// the same dispatch, and delivers audio to reliable subscribers as carrier
// records (Decision 12).

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

func mustHexDgram(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex constant %q: %v", s, err)
	}
	return b
}

func audioDgram(t *testing.T, seq uint32, payload string) []byte {
	t.Helper()
	d, err := wire.AppendAudioFrame(nil, wire.AudioFrameHeader{Seq: seq, TimestampUs: uint64(seq) * 20_000}, []byte(payload))
	if err != nil {
		t.Fatalf("AppendAudioFrame: %v", err)
	}
	return d
}

func audioConfigDgram(t *testing.T, sampleRate uint32) []byte {
	t.Helper()
	d, err := wire.AppendAudioConfig(nil, wire.AudioConfig{Codec: "opus", SampleRate: sampleRate, Channels: 2})
	if err != nil {
		t.Fatalf("AppendAudioConfig: %v", err)
	}
	return d
}

func hasDgram(f *fakeSender, want []byte) func() bool {
	return func() bool {
		for _, d := range f.received() {
			if bytes.Equal(d, want) {
				return true
			}
		}
		return false
	}
}

func countType(f *fakeSender, msgType uint8) int {
	n := 0
	for _, d := range f.received() {
		if len(d) >= 2 && d[1] == msgType {
			n++
		}
	}
	return n
}

func TestAudioFrameFannedOutVerbatim(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	s1, s2 := &fakeSender{}, &fakeSender{}
	sub1, err := r.Subscribe(id, s1)
	if err != nil {
		t.Fatalf("Subscribe s1: %v", err)
	}
	defer sub1.Close()
	sub2, err := r.Subscribe(id, s2)
	if err != nil {
		t.Fatalf("Subscribe s2: %v", err)
	}
	defer sub2.Close()

	// Interleave audio between video frames: the audio seq space (100, 105 —
	// gappy on purpose) must never read as video loss.
	p.HandleDatagram(chunkDgram(t, false, 1, 0, 1, "v1"))
	a1 := audioDgram(t, 100, "opus-a")
	a2 := audioDgram(t, 105, "opus-b")
	p.HandleDatagram(a1)
	p.HandleDatagram(a2)
	p.HandleDatagram(chunkDgram(t, false, 2, 0, 1, "v2"))

	for _, f := range []*fakeSender{s1, s2} {
		waitFor(t, 5*time.Second, hasDgram(f, a1), "audio frame 1 fanned out")
		waitFor(t, 5*time.Second, hasDgram(f, a2), "audio frame 2 fanned out")
	}

	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.FramesRelayed != 2 {
		t.Errorf("FramesRelayed = %d, want 2 (video only, audio never counts)", stats.FramesRelayed)
	}
	if stats.IngressFramesLost != 0 || stats.IngressChunksLost != 0 {
		t.Errorf("ingress loss = %d/%d, want 0/0 (audio seqs must not perturb the window)",
			stats.IngressFramesLost, stats.IngressChunksLost)
	}
	if stats.DatagramsRelayed != 4 {
		t.Errorf("DatagramsRelayed = %d, want 4 (audio folds into the generic counter)", stats.DatagramsRelayed)
	}
}

func TestAudioConfigRelayedCachedAndPrimed(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	s1 := &fakeSender{}
	sub1, err := r.Subscribe(id, s1)
	if err != nil {
		t.Fatalf("Subscribe s1: %v", err)
	}
	defer sub1.Close()

	cfg := audioConfigDgram(t, 48000)
	p.HandleDatagram(cfg)
	waitFor(t, 5*time.Second, hasDgram(s1, cfg), "config fanned out to live subscriber")

	// Late joiner: primed from the cache without a broadcaster re-send.
	s2 := &fakeSender{}
	sub2, err := r.Subscribe(id, s2)
	if err != nil {
		t.Fatalf("Subscribe s2: %v", err)
	}
	defer sub2.Close()
	waitFor(t, 5*time.Second, hasDgram(s2, cfg), "late joiner primed with cached config")

	// Newest config wins the cache.
	cfg2 := audioConfigDgram(t, 44100)
	p.HandleDatagram(cfg2)
	s3 := &fakeSender{}
	sub3, err := r.Subscribe(id, s3)
	if err != nil {
		t.Fatalf("Subscribe s3: %v", err)
	}
	defer sub3.Close()
	waitFor(t, 5*time.Second, hasDgram(s3, cfg2), "joiner primed with newest config")

	// New publisher session: cache invalidated (the toggle may have flipped).
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
	if n := countType(s4, wire.TypeAudioConfig); n != 0 {
		t.Errorf("post-restart joiner received %d audio configs, want 0 (cache invalidated)", n)
	}
}

// R17 interop (docs/20 Decision 4 as amended 2026-07-19): InvalidatePrimes —
// the edge-upstream-loss site — must clear the audio config cache too, or a
// viewer joining between drop and re-attach would be served origin A's config
// beside origin B's packets.
func TestAudioConfigInvalidatedOnInvalidatePrimes(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	p.HandleDatagram(audioConfigDgram(t, 48000))

	r.InvalidatePrimes(id)

	s := &fakeSender{}
	sub, err := r.Subscribe(id, s)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()
	time.Sleep(50 * time.Millisecond)
	if n := countType(s, wire.TypeAudioConfig); n != 0 {
		t.Errorf("joiner after InvalidatePrimes received %d audio configs, want 0", n)
	}
}

func TestAudioMalformedDropped(t *testing.T) {
	valid := audioDgram(t, 1, "x")
	badVersion := append([]byte(nil), valid...)
	badVersion[0] = 0x7F

	cases := []struct {
		name  string
		dgram []byte
	}{
		{"truncated frame", valid[:wire.AudioFrameHeaderSize-1]},
		{"empty payload frame", valid[:wire.AudioFrameHeaderSize]},
		{"bad version", badVersion},
		{"truncated config", audioConfigDgram(t, 48000)[:8]},
		{"zero sample rate config", mustHexDgram(t, "010800046f707573"+"00000000"+"02")},
		{"zero channels config", mustHexDgram(t, "010800046f707573"+"0000bb80"+"00")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			p.HandleDatagram(tc.dgram)

			if got := r.Stats().Totals.BadDatagrams; got != 1 {
				t.Errorf("BadDatagrams = %d, want 1", got)
			}
			time.Sleep(20 * time.Millisecond)
			if len(s.received()) != 0 {
				t.Errorf("malformed audio datagram was fanned out: %d datagrams", len(s.received()))
			}
		})
	}
}

// Cluster mode needs no audio-specific edge code (docs/20 Decision 4): the
// edge pump re-ingests upstream datagrams verbatim through the edge hub's own
// Publisher.HandleDatagram, so the dispatch cases above ARE the cluster
// support — audio is cached, primed, and fanned on an edge exactly like on an
// origin.
func TestAudioEdgeHubReingestsAndPrimes(t *testing.T) {
	r := NewRegistry(discardLog, Options{})
	id, epub, err := r.EdgePublish("K7XQ2M")
	if err != nil {
		t.Fatalf("EdgePublish: %v", err)
	}

	s1 := &fakeSender{}
	sub1, err := r.Subscribe(id, s1)
	if err != nil {
		t.Fatalf("Subscribe s1: %v", err)
	}
	defer sub1.Close()

	frame := audioDgram(t, 7, "opus")
	cfg := audioConfigDgram(t, 48000)
	epub.HandleDatagram(frame)
	epub.HandleDatagram(cfg)
	waitFor(t, 5*time.Second, hasDgram(s1, frame), "audio frame re-ingested on edge hub")
	waitFor(t, 5*time.Second, hasDgram(s1, cfg), "audio config re-ingested on edge hub")

	// Edge hub primes late joiners from its own cache.
	s2 := &fakeSender{}
	sub2, err := r.Subscribe(id, s2)
	if err != nil {
		t.Fatalf("Subscribe s2: %v", err)
	}
	defer sub2.Close()
	waitFor(t, 5*time.Second, hasDgram(s2, cfg), "late joiner primed from edge cache")
}

// Decision 12: for a reliable subscriber the drain writes everything the
// fan-out queue carries as carrier records and never calls SendDatagram —
// audio rides the carrier with zero audio-specific relay code.
func TestAudioToReliableSubscriberRidesCarrier(t *testing.T) {
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

	ingestKeyframe(t, p, keyframeMsg(t, 0, "vp8", "KEY0"))
	a1 := audioDgram(t, 1, "opus-a")
	cfg := audioConfigDgram(t, 48000)
	p.HandleDatagram(a1)
	p.HandleDatagram(cfg)
	waitCarrierRecords(t, f, 2)

	records := f.carrierRecords(t)
	want := [][]byte{a1, cfg}
	if len(records) != len(want) {
		t.Fatalf("carrier records = %d, want %d", len(records), len(want))
	}
	for i := range want {
		if !bytes.Equal(records[i], want[i]) {
			t.Errorf("record %d = %x, want %x", i, records[i], want[i])
		}
	}
	if got := f.received(); len(got) != 0 {
		t.Errorf("SendDatagram was called %d times for a reliable subscriber", len(got))
	}
}
