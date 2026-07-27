// R29 forward parity under real packet loss (docs/34 FP8).
//
// Every other parity test proves a piece in isolation: the codec against
// exhaustive erasure pairs, the relay's prefix filter against in-memory fakes,
// the viewer's reassembler against hand-fed datagrams. None of them puts the
// three together on a socket that actually loses packets — which is exactly
// the hole docs/24 finding 10 found in R19, where the carrier path shipped
// with every test running against fakes or a zero-loss loopback and a
// regression degrading it would have shipped green.
//
// The loss goes where R29 says it lives (docs/34 §1: "the relay sent every
// byte; the frames died in flight"): the lossyLink from resilient_loss_test.go
// sits in front of the relay dropping relay → subscriber packets, while the
// publisher stays wired direct so ingress is clean and every absence
// downstream is attributable to the injected loss.
//
// Both subscribers are DATAGRAM subscribers on the same link — one served
// parity, one served none — so the no-parity side is a same-conditions control
// rather than a separate experiment. The recovery is run through the shared
// wire codec, which is the same code the browser reassembler calls, so what is
// proven end to end is: the relay forwarded the right symbols to the right
// subscriber under load, and the symbols that survived were sufficient.

package transport

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// frameSink collects the chunks and parity symbols that actually arrived for
// each frame, so recovery can be attempted exactly as a viewer would.
type frameSink struct {
	mu     sync.Mutex
	chunks map[uint32]map[uint16][]byte // frameID → chunkIndex → payload
	parity map[uint32]map[uint8][]byte  // frameID → symbol index → payload
	// parityDatagrams counts every parity chunk that reached this subscriber,
	// which is what proves the per-subscriber filter under load rather than in
	// a unit test.
	parityDatagrams int
}

func newFrameSink() *frameSink {
	return &frameSink{
		chunks: make(map[uint32]map[uint16][]byte),
		parity: make(map[uint32]map[uint8][]byte),
	}
}

func (s *frameSink) observe(dgram []byte) {
	_, typ, err := wire.PeekType(dgram)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch typ {
	case wire.TypeVideoChunk:
		h, payload, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			return
		}
		if s.chunks[h.FrameID] == nil {
			s.chunks[h.FrameID] = make(map[uint16][]byte)
		}
		s.chunks[h.FrameID][h.ChunkIndex] = append([]byte(nil), payload...)
	case wire.TypeParityChunk:
		h, payload, err := wire.ParseParityChunk(dgram)
		if err != nil {
			return
		}
		s.parityDatagrams++
		if s.parity[h.FrameID] == nil {
			s.parity[h.FrameID] = make(map[uint8][]byte)
		}
		s.parity[h.FrameID][h.ParityIndex] = append([]byte(nil), payload...)
	}
}

func (s *frameSink) parityCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parityDatagrams
}

// verdict counts, over the frames the publisher sent, how many arrived whole
// on their own and how many more became whole once parity was applied.
type verdict struct {
	completeOnArrival int
	recoveredByParity int
	stillMissing      int
	corrupt           int
}

// assess replays the viewer's decision for every sent frame: complete already,
// recoverable from the symbols that survived, or lost. `want` is the exact
// payload of each chunk, so recovery is checked for CORRECTNESS, not merely
// for not returning an error — a codec that reconstructed plausible garbage
// would otherwise pass.
func (s *frameSink) assess(t *testing.T, want map[uint32][][]byte, chunkCount int, frameBytes int) verdict {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	var v verdict
	for frameID, wantChunks := range want {
		got := s.chunks[frameID]
		missing := 0
		payloads := make([][]byte, chunkCount)
		for i := range chunkCount {
			if p, ok := got[uint16(i)]; ok {
				payloads[i] = p
			} else {
				missing++
			}
		}
		if missing == 0 {
			v.completeOnArrival++
			continue
		}
		symbols := make([][]byte, wire.MaxParitySymbols)
		held := 0
		for idx, p := range s.parity[frameID] {
			if int(idx) < len(symbols) {
				symbols[idx] = p
				held++
			}
		}
		if held == 0 || missing > held {
			v.stillMissing++
			continue
		}
		if err := wire.RecoverChunks(payloads, symbols, frameBytes); err != nil {
			v.stillMissing++
			continue
		}
		for i := range chunkCount {
			if string(payloads[i]) != string(wantChunks[i]) {
				v.corrupt++
				break
			}
		}
		v.recoveredByParity++
	}
	return v
}

// The headline R29 claim, under real loss: on a link dropping 3 % of the
// relay's packets — the rate measured on the session in docs/34 §1 — a
// subscriber served two parity symbols reconstructs nearly every frame that a
// no-parity subscriber behind the same link loses outright.
//
// The frame shape is the measured one too: 9 chunks per delta, which is what
// turns 3 % packet loss into ~24 % frame loss without parity (0.97⁹ = 0.76).
// That is not an incidental constant — it is why the feature exists, and
// picking a friendlier shape would prove something nobody is asking about.
func TestParityRecoversFramesLostOnALossyDownlink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  8,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		ParityDefault:   2,
	})

	// Publisher direct to the relay: R29's lossy leg is downstream, and clean
	// ingress is what lets "the subscriber is missing a chunk" mean "the link
	// dropped it" and nothing else.
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startLossyLink(t, port)
	protected := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?parity=2", link.port(), id), clientTLS)
	defer protected.CloseWithError(0, "")
	control := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?parity=0", link.port(), id), clientTLS)
	defer control.CloseWithError(0, "")

	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 2 }, "both subscribers registered")

	const (
		frames      = 120
		chunks      = 9
		payloadLen  = 600
		lossPercent = 3
	)
	frameBytes := chunks * payloadLen

	// Build every frame up front, with its parity, exactly as a producer does:
	// symbols over the chunk PAYLOADS, appended after the data chunks.
	want := make(map[uint32][][]byte, frames)
	type outbound struct{ dgrams [][]byte }
	var run []outbound
	for f := range frames {
		frameID := uint32(f + 1)
		payloads := make([][]byte, chunks)
		var dgrams [][]byte
		for i := range chunks {
			p := make([]byte, payloadLen)
			for b := range p {
				p[b] = byte(int(frameID)*7 + i*31 + b)
			}
			payloads[i] = p
			d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
				FrameID: frameID, ChunkIndex: uint16(i), ChunkCount: chunks,
				TimestampUs: uint64(frameID) * 16_667,
			}, p)
			if err != nil {
				t.Fatalf("AppendVideoChunk: %v", err)
			}
			dgrams = append(dgrams, d)
		}
		symbols, err := wire.ComputeParity(payloads, 2)
		if err != nil {
			t.Fatalf("ComputeParity: %v", err)
		}
		for i, sym := range symbols {
			d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
				FrameID: frameID, ParityIndex: uint8(i), ChunkCount: chunks, FrameBytes: uint32(frameBytes),
			}, sym)
			if err != nil {
				t.Fatalf("AppendParityChunk: %v", err)
			}
			dgrams = append(dgrams, d)
		}
		want[frameID] = payloads
		run = append(run, outbound{dgrams: dgrams})
	}

	readCtx, readCancel := context.WithTimeout(ctx, 60*time.Second)
	defer readCancel()
	protectedSink, controlSink := newFrameSink(), newFrameSink()
	for _, pair := range []struct {
		sess any
		sink *frameSink
	}{{protected, protectedSink}, {control, controlSink}} {
		go func(s any, sink *frameSink) {
			sess := s.(interface {
				ReceiveDatagram(context.Context) ([]byte, error)
			})
			for {
				d, err := sess.ReceiveDatagram(readCtx)
				if err != nil {
					return
				}
				sink.observe(d)
			}
		}(pair.sess, pair.sink)
	}

	// Arm the loss only now: dropping handshake packets costs a retransmit and
	// proves nothing. The point is to lose media.
	link.setLoss(lossPercent)

	for _, ob := range run {
		for _, d := range ob.dgrams {
			if err := pub.SendDatagram(d); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
		}
		// Paced so nothing overruns a receive buffer on the way in — an
		// ingress drop would be loss this test does not model.
		time.Sleep(2 * time.Millisecond)
	}

	// Let the tail drain.
	waitUntil(5*time.Second, func() bool {
		return len(controlSink.chunks) >= frames && len(protectedSink.chunks) >= frames
	})
	time.Sleep(500 * time.Millisecond)

	prot := protectedSink.assess(t, want, chunks, frameBytes)
	ctrl := controlSink.assess(t, want, chunks, frameBytes)
	t.Logf("protected: complete=%d recovered=%d missing=%d corrupt=%d (parity datagrams %d)",
		prot.completeOnArrival, prot.recoveredByParity, prot.stillMissing, prot.corrupt, protectedSink.parityCount())
	t.Logf("control:   complete=%d recovered=%d missing=%d (parity datagrams %d)",
		ctrl.completeOnArrival, ctrl.recoveredByParity, ctrl.stillMissing, controlSink.parityCount())

	// 1. The per-subscriber filter held under load: the control asked for 0 and
	//    must not have received a single symbol, and the protected subscriber
	//    must have received symbols for essentially every frame.
	if controlSink.parityCount() != 0 {
		t.Errorf("the ?parity=0 subscriber received %d parity datagrams, want 0", controlSink.parityCount())
	}
	if got := protectedSink.parityCount(); got < frames {
		t.Errorf("protected subscriber received %d parity datagrams over %d frames, want at least one per frame",
			got, frames)
	}

	// 2. The link genuinely lost frames, or the rest asserts nothing. At 3 %
	//    over 9 chunks the control is expected to lose ~24 % of 120 frames;
	//    requiring 5 leaves enormous headroom against a lucky run while still
	//    failing loudly if the loss injector silently stopped working.
	if ctrl.stillMissing < 5 {
		t.Fatalf("control lost only %d of %d frames — the injected loss is not reaching the media, so this test proves nothing",
			ctrl.stillMissing, frames)
	}

	// 3. Parity actually repaired them.
	//
	// The criterion is a 4x reduction in frame loss, not "recovers 95 % of
	// what the control lost". k=2 does NOT have a 100 % ceiling here, and the
	// gap is structural rather than statistical: a frame is unrecoverable as
	// soon as THREE of its eleven datagrams are lost, and a lost parity symbol
	// counts toward that three exactly as a lost data chunk does. The residue
	// is real, so a criterion written against a 100 % ceiling would be a flake
	// generator rather than a regression detector.
	//
	// Measured on this rig: control ~29 % frame loss, protected ~2.5 % — an
	// ~11x reduction, which leaves the 4x threshold a wide margin while still
	// failing unmistakably if parity stops being applied, since the two rates
	// would then converge on each other.
	protectedLossRate := float64(prot.stillMissing) / float64(frames)
	controlLossRate := float64(ctrl.stillMissing) / float64(frames)
	if protectedLossRate*4 > controlLossRate {
		t.Errorf("protected lost %.1f%% of frames against the control's %.1f%%: parity did not cut frame loss 4x",
			protectedLossRate*100, controlLossRate*100)
	}
	t.Logf("parity cut frame loss %.1fx (%.1f%% -> %.1f%%), recovering %d of the %d frames that arrived incomplete",
		controlLossRate/max(protectedLossRate, 1e-9), controlLossRate*100, protectedLossRate*100,
		prot.recoveredByParity, prot.recoveredByParity+prot.stillMissing)
	if prot.recoveredByParity == 0 {
		t.Error("no frame was recovered by parity — the symbols arrived but bought nothing")
	}

	// 4. Recovery is byte-correct. A codec reconstructing plausible garbage
	//    would satisfy every count above and still ruin the picture.
	if prot.corrupt != 0 {
		t.Errorf("%d frames recovered to the WRONG bytes", prot.corrupt)
	}

	// 5. The relay did not pay for any of it: parity is the producer's work,
	//    forwarded verbatim.
	bst := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if bst.ParityDatagramsForwarded == 0 {
		t.Error("relay reported no parity forwarded")
	}
	if bst.ParitySuppressed == 0 {
		t.Error("relay reported no parity suppressed, but the control asked for none")
	}
}

// The same rig with the fleet turned off: nothing is advertised, nothing is
// forwarded, and a subscriber asking for parity is served none — the
// byte-identical-to-pre-R29 claim, under loss rather than in a unit test.
func TestParityFleetOffForwardsNothingUnderLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  4,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
		ParityDefault:   0,
	})
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startLossyLink(t, port)
	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?parity=2", link.port(), id), clientTLS)
	defer sub.CloseWithError(0, "")
	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	readCtx, readCancel := context.WithTimeout(ctx, 20*time.Second)
	defer readCancel()
	sink := newFrameSink()
	go func() {
		for {
			d, err := sub.ReceiveDatagram(readCtx)
			if err != nil {
				return
			}
			sink.observe(d)
		}
	}()

	link.setLoss(3)
	// A producer against a parity-off fleet emits no symbols, so this sends
	// only data chunks — the shape the relay would actually see.
	for f := range 20 {
		for _, d := range encodeFrame(t, uint32(f+1), false, 2) {
			if err := pub.SendDatagram(d); err != nil {
				t.Fatalf("SendDatagram: %v", err)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	if got := sink.parityCount(); got != 0 {
		t.Errorf("subscriber received %d parity datagrams from a parity-off fleet, want 0", got)
	}
	bst := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if bst.ParityDatagramsForwarded != 0 || bst.ParitySuppressed != 0 {
		t.Errorf("parity counters = %d/%d on a parity-off fleet, want 0/0 (they are omitempty, so any value shows in /statusz)",
			bst.ParityDatagramsForwarded, bst.ParitySuppressed)
	}
}
