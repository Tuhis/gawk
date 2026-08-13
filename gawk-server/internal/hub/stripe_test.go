package hub

import (
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// testOwner is a well-formed ?owner= token for tests: legs require one
// (docs/35 §14), and a primary needs one before ApplyStripeState will arm.
const testOwner = "aabbccdd00112233"

// TestNegotiateStripe pins the dial-validation rules: strict, because a
// mis-striped leg cannot degrade to anything useful (docs/35 §5.3).
func TestNegotiateStripe(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stripe, leg string
		owner       string
		enabled     bool
		reliable    bool
		wantLeg     StripeLeg
		wantIsLeg   bool
		wantErr     bool
	}{
		{name: "absent params are the ordinary viewer path", enabled: true},
		{name: "absent params ignore a stray owner", owner: testOwner, enabled: true},
		{name: "valid leg", stripe: "3", leg: "1", owner: testOwner, enabled: true, wantLeg: StripeLeg{N: 3, Member: 1, Owner: testOwner}, wantIsLeg: true},
		{name: "member zero", stripe: "1", leg: "0", owner: testOwner, enabled: true, wantLeg: StripeLeg{N: 1, Member: 0, Owner: testOwner}, wantIsLeg: true},
		{name: "max width", stripe: "4", leg: "3", owner: testOwner, enabled: true, wantLeg: StripeLeg{N: 4, Member: 3, Owner: testOwner}, wantIsLeg: true},
		{name: "stripe without leg", stripe: "3", owner: testOwner, enabled: true, wantErr: true},
		{name: "leg without stripe", leg: "1", owner: testOwner, enabled: true, wantErr: true},
		// §14 (owner decision 2026-08-13): a leg without a valid owner token
		// is an unreapable orphan waiting to happen — rejected like any other
		// unusable leg, so a pre-token viewer simply stays unstriped.
		{name: "leg without owner rejects", stripe: "3", leg: "1", enabled: true, wantErr: true},
		{name: "short owner rejects", stripe: "3", leg: "1", owner: "aabbccdd", enabled: true, wantErr: true},
		{name: "long owner rejects", stripe: "3", leg: "1", owner: testOwner + "00", enabled: true, wantErr: true},
		{name: "uppercase owner rejects", stripe: "3", leg: "1", owner: "AABBCCDD00112233", enabled: true, wantErr: true},
		{name: "non-hex owner rejects", stripe: "3", leg: "1", owner: "zzbbccdd00112233", enabled: true, wantErr: true},
		{name: "disabled fleet rejects", stripe: "3", leg: "1", owner: testOwner, enabled: false, wantErr: true},
		{name: "reliable delivery rejects", stripe: "3", leg: "1", owner: testOwner, enabled: true, reliable: true, wantErr: true},
		{name: "width over max", stripe: "5", leg: "0", owner: testOwner, enabled: true, wantErr: true},
		{name: "width zero", stripe: "0", leg: "0", owner: testOwner, enabled: true, wantErr: true},
		{name: "member out of range", stripe: "2", leg: "2", owner: testOwner, enabled: true, wantErr: true},
		{name: "member negative", stripe: "2", leg: "-1", owner: testOwner, enabled: true, wantErr: true},
		{name: "garbage width", stripe: "banana", leg: "0", owner: testOwner, enabled: true, wantErr: true},
		{name: "garbage member", stripe: "2", leg: "x", owner: testOwner, enabled: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leg, isLeg, err := NegotiateStripe(tc.stripe, tc.leg, tc.owner, tc.enabled, tc.reliable)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NegotiateStripe: %v", err)
			}
			if leg != tc.wantLeg || isLeg != tc.wantIsLeg {
				t.Fatalf("got (%+v, %v), want (%+v, %v)", leg, isLeg, tc.wantLeg, tc.wantIsLeg)
			}
		})
	}
}

// receivedOrdinals extracts the stripe ordinals of every delta datagram a
// fake sender received, so the partition can be asserted by content.
func receivedOrdinals(t *testing.T, f *fakeSender) []uint32 {
	t.Helper()
	var ords []uint32
	for _, d := range f.received() {
		if len(d) < 2 {
			continue
		}
		switch d[1] {
		case wire.TypeVideoChunk:
			h, _, err := wire.ParseVideoChunk(d)
			if err != nil {
				t.Fatalf("ParseVideoChunk: %v", err)
			}
			ords = append(ords, wire.StripeOrdinal(h.ChunkIndex, h.ChunkCount, -1))
		case wire.TypeParityChunk:
			h, _, err := wire.ParseParityChunk(d)
			if err != nil {
				t.Fatalf("ParseParityChunk: %v", err)
			}
			ords = append(ords, wire.StripeOrdinal(0, h.ChunkCount, int(h.ParityIndex)))
		}
	}
	return ords
}

// TestStripeLegPartitionIsExactCover is the core routing property (docs/35
// §5.2): for every frame size and stripe width, the legs' shares partition
// the frame's delta datagrams — every ordinal delivered exactly once across
// the leg set, chunks and parity alike — while a plain viewer beside them
// still receives everything.
func TestStripeLegPartitionIsExactCover(t *testing.T) {
	for n := 1; n <= 30; n++ {
		for stripeN := 1; stripeN <= wire.MaxStripeLegs; stripeN++ {
			t.Run(fmt.Sprintf("chunks=%d/N=%d", n, stripeN), func(t *testing.T) {
				r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
				id, p, err := r.StartPublish("")
				if err != nil {
					t.Fatalf("StartPublish: %v", err)
				}
				control := &fakeSender{}
				cs, err := r.SubscribeParity(id, control, 2, "")
				if err != nil {
					t.Fatalf("SubscribeParity: %v", err)
				}
				legs := make([]*fakeSender, stripeN)
				legSubs := make([]*Subscriber, stripeN)
				for j := range legs {
					legs[j] = &fakeSender{}
					legSubs[j], err = r.SubscribeStripeLeg(id, legs[j], StripeLeg{N: stripeN, Member: j, Owner: testOwner}, 2)
					if err != nil {
						t.Fatalf("SubscribeStripeLeg(%d): %v", j, err)
					}
				}

				for i := 0; i < n; i++ {
					p.HandleDatagram(chunkDgram(t, false, 7, uint16(i), uint16(n), "aa"))
				}
				if n > 1 { // parity needs n data chunks; use n>=2 to send both symbols
					p.HandleDatagram(parityDgramN(t, 7, 0, uint16(n)))
					p.HandleDatagram(parityDgramN(t, 7, 1, uint16(n)))
				}
				cs.Close()
				for _, s := range legSubs {
					s.Close()
				}

				wantTotal := n
				if n > 1 {
					wantTotal += 2
				}
				// The control viewer receives every delta datagram.
				if got := len(receivedOrdinals(t, control)); got != wantTotal {
					t.Fatalf("control received %d delta datagrams, want %d", got, wantTotal)
				}
				// The legs partition them: exactly once, on the right leg.
				seen := make(map[uint32]int)
				for j, f := range legs {
					for _, ord := range receivedOrdinals(t, f) {
						if int(ord%uint32(stripeN)) != j {
							t.Fatalf("leg %d received ordinal %d (mismapped)", j, ord)
						}
						seen[ord]++
					}
				}
				if len(seen) != wantTotal {
					t.Fatalf("legs covered %d ordinals, want %d", len(seen), wantTotal)
				}
				for ord, count := range seen {
					if count != 1 {
						t.Fatalf("ordinal %d delivered %d times across legs, want exactly 1", ord, count)
					}
				}
			})
		}
	}
}

// parityDgramN is parityDgram with a caller-chosen chunk count, so partition
// tests can place parity ordinals correctly for any frame size.
func parityDgramN(t *testing.T, frameID uint32, index uint8, chunkCount uint16) []byte {
	t.Helper()
	d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID:     frameID,
		ParityIndex: index,
		ChunkCount:  chunkCount,
		FrameBytes:  uint32(chunkCount) * 4,
	}, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	return d
}

// TestStripeLegParityPrefixComposes: a leg's parity share respects its own
// negotiated k — the R29 prefix and the R30 mod filter are both applied.
func TestStripeLegParityPrefixComposes(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// One leg covering ALL ordinals (N=1) but with k=0: it must see chunks
	// and never parity.
	f := &fakeSender{}
	s, err := r.SubscribeStripeLeg(id, f, StripeLeg{N: 1, Member: 0, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	p.HandleDatagram(chunkDgram(t, false, 7, 0, 4, "aa"))
	p.HandleDatagram(parityDgramN(t, 7, 0, 4))
	p.HandleDatagram(parityDgramN(t, 7, 1, 4))
	s.Close()
	if got := countType(f, wire.TypeVideoChunk); got != 1 {
		t.Errorf("leg received %d video chunks, want 1", got)
	}
	if got := countType(f, wire.TypeParityChunk); got != 0 {
		t.Errorf("k=0 leg received %d parity chunks, want 0", got)
	}
}

// TestStripeLegReceivesNoControlOrAudio: a leg carries delta datagrams only —
// config, clock mapping, viewer count and audio all stay on the primary, and
// the join primes are skipped entirely (docs/35 §5.1).
func TestStripeLegReceivesNoControlOrAudio(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// Populate every cache a joiner would be primed with.
	p.HandleDatagram(configDgram(t, "avc1.42E02A"))
	p.HandleDatagram(wire.AppendClockMapping(nil, 1500))
	p.HandleDatagram(audioConfigDgram(t, 48000))
	r.PumpViewerCounts(time.Now())
	ingestKeyframe(t, p, keyframeMsg(t, 9, "avc1.42E02A", "kf"))

	f := &fakeSender{}
	s, err := r.SubscribeStripeLeg(id, f, StripeLeg{N: 1, Member: 0, Owner: testOwner}, 2)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	// Live traffic of every non-delta kind, plus one delta.
	p.HandleDatagram(configDgram(t, "avc1.42E02A"))
	p.HandleDatagram(wire.AppendClockMapping(nil, 1500))
	p.HandleDatagram(audioConfigDgram(t, 48000))
	p.HandleDatagram(audioDgram(t, 1, "op"))
	r.PumpViewerCounts(time.Now().Add(10 * time.Second))
	p.HandleDatagram(chunkDgram(t, false, 10, 0, 1, "aa"))
	ingestKeyframe(t, p, keyframeMsg(t, 12, "avc1.42E02A", "kf2"))
	s.Close()

	for _, tc := range []struct {
		name string
		typ  uint8
		want int
	}{
		{"decoder config", wire.TypeDecoderConfig, 0},
		{"clock mapping", wire.TypeClockMapping, 0},
		{"viewer count", wire.TypeViewerCount, 0},
		{"audio config", wire.TypeAudioConfig, 0},
		{"audio frame", wire.TypeAudioFrame, 0},
		{"video chunk", wire.TypeVideoChunk, 1},
	} {
		if got := countType(f, tc.typ); got != tc.want {
			t.Errorf("leg received %d %s datagrams, want %d", got, tc.name, tc.want)
		}
	}
	if got := len(f.receivedKeyframes()); got != 0 {
		t.Errorf("leg received %d keyframe streams, want 0 (primary carries them)", got)
	}
	if f.kfOpens != 0 {
		t.Errorf("leg saw %d keyframe stream opens, want 0", f.kfOpens)
	}
}

// TestStripeSuppressionOnPrimary: StripeState suppresses delta datagrams on
// the primary (legs carry them) while control keeps flowing; unstripe — and
// TTL expiry — restore them (docs/35 §5.3).
func TestStripeSuppressionOnPrimary(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.SubscribeParity(id, f, 2, testOwner)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}

	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	p.HandleDatagram(chunkDgram(t, false, 7, 0, 2, "aa"))
	p.HandleDatagram(parityDgramN(t, 7, 0, 2))
	p.HandleDatagram(configDgram(t, "avc1.42E02A"))

	// Unstripe restores the delta flow immediately.
	s.ApplyStripeState(wire.StripeState{})
	p.HandleDatagram(chunkDgram(t, false, 8, 0, 2, "bb"))

	// Re-stripe, then let the TTL lapse: deltas resume by expiry alone —
	// the fail-open half of the design.
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	s.stripeUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())
	p.HandleDatagram(chunkDgram(t, false, 9, 0, 2, "cc"))
	s.Close()

	if got := countType(f, wire.TypeVideoChunk); got != 2 {
		t.Errorf("primary received %d video chunks, want 2 (unstriped + TTL-expired)", got)
	}
	if got := countType(f, wire.TypeParityChunk); got != 0 {
		t.Errorf("striped primary received %d parity chunks, want 0", got)
	}
	if got := countType(f, wire.TypeDecoderConfig); got != 1 {
		t.Errorf("striped primary received %d configs, want 1 — control must keep flowing", got)
	}

	stats := r.Stats()
	var bs Stats
	for _, b := range stats.Broadcasts {
		bs = b
	}
	if bs.StripeSuppressedDatagrams != 2 {
		t.Errorf("StripeSuppressedDatagrams = %d, want 2 (one chunk + one parity)", bs.StripeSuppressedDatagrams)
	}
	// stripe → unstripe → stripe = 3 level flips; the fold after Close keeps
	// them (counters survive their owner — CODE-REVIEW).
	if bs.StripeTransitions != 3 {
		t.Errorf("StripeTransitions = %d, want 3", bs.StripeTransitions)
	}
}

// TestStripeRefreshDoesNotInflateTransitions: the 1 Hz level refresh re-arms
// the TTL without counting as a new transition.
func TestStripeRefreshDoesNotInflateTransitions(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.SubscribeParity(id, f, 2, testOwner)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	defer s.Close()
	for i := 0; i < 5; i++ {
		s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	}
	if got := s.stripeTransitions.Load(); got != 1 {
		t.Fatalf("stripeTransitions = %d after 5 refreshes, want 1", got)
	}
	if !s.stripeSuppressed(time.Now().UnixNano()) {
		t.Fatal("refresh did not keep the suppression armed")
	}
}

// TestApplyStripeStateInertOnWrongSessions: reliable, DVR, internal and leg
// sessions can never be suppressed — the message is discarded exactly like
// any other unrecognized datagram.
func TestApplyStripeStateInertOnWrongSessions(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true, DVR: DVROptions{Window: time.Second, MaxBytes: 1 << 20}})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	rel, err := r.SubscribeReliable(id, &fakeSender{})
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	dvr, err := r.SubscribeDVR(id, &fakeSender{}, 3000)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	internal, err := r.SubscribeInternal(id, &fakeSender{})
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	leg, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	for name, s := range map[string]*Subscriber{"reliable": rel, "dvr": dvr, "internal": internal, "leg": leg} {
		s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
		if s.stripeUntil.Load() != 0 {
			t.Errorf("%s session accepted a stripe suppression", name)
		}
		if s.stripeTransitions.Load() != 0 {
			t.Errorf("%s session counted a stripe transition", name)
		}
		s.Close()
	}
}

// TestStripeLegCountsAgainstCapsNotViewers (docs/35 §5.8): a leg consumes a
// subscriber slot — the bound against a counterfeit leg flood — but never
// inflates the audience number.
func TestStripeLegCountsAgainstCapsNotViewers(t *testing.T) {
	r := NewRegistry(discardLog, Options{MaxSubscribers: 2, StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	viewer, err := r.Subscribe(id, &fakeSender{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer viewer.Close()
	leg, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	defer leg.Close()
	if _, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 1, Owner: testOwner}, 0); err == nil {
		t.Fatal("third session admitted past MaxSubscribers=2 — legs must count against the cap")
	}

	stats := r.Stats()
	var bs Stats
	for _, b := range stats.Broadcasts {
		bs = b
	}
	if bs.Subscribers != 2 {
		t.Errorf("Subscribers = %d, want 2 (viewer + leg)", bs.Subscribers)
	}
	if bs.ViewersGlobal != 1 {
		t.Errorf("ViewersGlobal = %d, want 1 — a leg is not a viewer", bs.ViewersGlobal)
	}
	if bs.StripeLegs != 1 {
		t.Errorf("StripeLegs = %d, want 1", bs.StripeLegs)
	}
	if r.ViewerSubscribers(id) != 1 {
		t.Errorf("ViewerSubscribers = %d, want 1", r.ViewerSubscribers(id))
	}
	if r.ExternalSubscribers(id) != 2 {
		t.Errorf("ExternalSubscribers = %d, want 2 — legs keep an edge alive", r.ExternalSubscribers(id))
	}
}

// TestStripeSubscriberDetails: the /statusz join of one viewer's sessions.
func TestStripeSubscriberDetails(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, err := r.SubscribeParity(id, &fakeSender{}, 2, testOwner)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	defer primary.Close()
	leg, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 3, Member: 2, Owner: testOwner}, 2)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	defer leg.Close()
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})

	stats := r.Stats()
	var bs Stats
	for _, b := range stats.Broadcasts {
		bs = b
	}
	if bs.StripedPrimaries != 1 {
		t.Errorf("StripedPrimaries = %d, want 1", bs.StripedPrimaries)
	}
	var sawLeg, sawPrimary bool
	for _, d := range bs.SubscriberDetails {
		if d.StripeLeg {
			sawLeg = true
			if d.StripeN != 3 || d.StripeMember != 2 {
				t.Errorf("leg detail = n%d/member%d, want 3/2", d.StripeN, d.StripeMember)
			}
		} else {
			sawPrimary = true
			if !d.Striped || d.StripeN != 3 {
				t.Errorf("primary detail = striped=%v n=%d, want true/3", d.Striped, d.StripeN)
			}
		}
	}
	if !sawLeg || !sawPrimary {
		t.Fatalf("details missing a row: leg=%v primary=%v", sawLeg, sawPrimary)
	}
}

// BenchmarkFanOutStripeInactive pins the no-striping hot path: with no legs
// and no suppression, the stripe check must be two cheap field reads per
// subscriber (docs/35 §6 — the "no measurable Registry.mu regression"
// criterion is judged against this baseline).
func BenchmarkFanOutStripeInactive(b *testing.B) {
	benchmarkFanOut(b, false)
}

func BenchmarkFanOutStriped(b *testing.B) {
	benchmarkFanOut(b, true)
}

func benchmarkFanOut(b *testing.B, striped bool) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true, MaxSubscribers: 32})
	id, p, err := r.StartPublish("")
	if err != nil {
		b.Fatalf("StartPublish: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := r.SubscribeParity(id, &fakeSender{}, 2, ""); err != nil {
			b.Fatalf("SubscribeParity: %v", err)
		}
	}
	if striped {
		for j := 0; j < 3; j++ {
			if _, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 3, Member: j, Owner: testOwner}, 2); err != nil {
				b.Fatalf("SubscribeStripeLeg: %v", err)
			}
		}
	}
	dgram, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{FrameID: 1, ChunkIndex: 3, ChunkCount: 18, TimestampUs: 1}, make([]byte, 1100))
	if err != nil {
		b.Fatalf("AppendVideoChunk: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.HandleDatagram(dgram)
	}
}

// docs/35 §12 finding 3: counters must survive their owner's deletion
// (CODE-REVIEW), and the hub-expiry fold predated both R29 and R30 — a
// lingered-out edge hub took its parity AND stripe counters with it, which
// is exactly how the first e2e-cluster dispatch showed zero stripe
// engagement on a fleet where the striped pass had demonstrably run (the
// striped viewer's serving hub was a viewerless edge that lingered out
// before cluster-assert read /statusz). A fleet total that DROPS on hub
// expiry is also a Prometheus counter going backwards.
func TestExpiryKeepsStripeAndParityTotals(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2, StripedDelivery: true, BroadcastGrace: time.Minute})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, err := r.SubscribeParity(id, &fakeSender{}, 2, testOwner)
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	leg, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	// Stripe activity first: engage, fan a delta + parity (both withheld
	// from the striped primary; the parity is also k-suppressed for the k=0
	// leg), release. Then parity activity the primary actually RECEIVES, so
	// forwarded and egress-byte counters are non-zero too. Closing the
	// subscribers flushes their drains and folds their egress into the hub
	// while it is still alive — the lingered-edge shape.
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	p.HandleDatagram(chunkDgram(t, false, 7, 0, 2, "aa"))
	p.HandleDatagram(parityDgramN(t, 7, 0, 2))
	primary.ApplyStripeState(wire.StripeState{})
	p.HandleDatagram(chunkDgram(t, false, 8, 0, 2, "bb"))
	p.HandleDatagram(parityDgramN(t, 8, 0, 2))
	primary.Close()
	leg.Close()

	before := r.Stats().Totals
	if before.StripeTransitions == 0 || before.StripeSuppressedDatagrams == 0 ||
		before.ParityDatagramsForwarded == 0 || before.ParitySuppressed == 0 ||
		before.EgressParityBytes == 0 {
		t.Fatalf("test setup produced no counters: %+v", before)
	}

	// The publisher goes away and the hub is force-expired (EndBroadcast is
	// the janitor's path — same effect as the edge linger-out).
	p.Close()
	r.EndBroadcast(id)

	after := r.Stats().Totals
	if after.StripeTransitions < before.StripeTransitions {
		t.Errorf("StripeTransitions dropped across expiry: %d -> %d", before.StripeTransitions, after.StripeTransitions)
	}
	if after.StripeSuppressedDatagrams < before.StripeSuppressedDatagrams {
		t.Errorf("StripeSuppressedDatagrams dropped across expiry: %d -> %d", before.StripeSuppressedDatagrams, after.StripeSuppressedDatagrams)
	}
	if after.ParityDatagramsForwarded < before.ParityDatagramsForwarded {
		t.Errorf("ParityDatagramsForwarded dropped across expiry: %d -> %d", before.ParityDatagramsForwarded, after.ParityDatagramsForwarded)
	}
	if after.ParitySuppressed < before.ParitySuppressed {
		t.Errorf("ParitySuppressed dropped across expiry: %d -> %d", before.ParitySuppressed, after.ParitySuppressed)
	}
	if after.EgressParityBytes < before.EgressParityBytes {
		t.Errorf("EgressParityBytes dropped across expiry: %d -> %d", before.EgressParityBytes, after.EgressParityBytes)
	}
}
