package hub

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// parityDgram builds a ParityChunk datagram at the given symbol index.
func parityDgram(t *testing.T, frameID uint32, index uint8) []byte {
	t.Helper()
	d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID:     frameID,
		ParityIndex: index,
		ChunkCount:  4,
		FrameBytes:  16,
	}, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	return d
}

// TestNegotiateParityClamps pins the rules that stop a viewer asking for
// protection the fleet is not producing. They are the same for every delivery
// mode (docs/75 D11): a chunk the broadcaster's uplink lost is missing at the
// relay, so carrier retransmission cannot bring it back, and a reliable or DVR
// viewer needs the same prefix a live-edge viewer gets.
func TestNegotiateParityClamps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		param       string
		fleet       int
		wantRequest int
		wantServed  int
	}{
		{"absent means the fleet default", "", 2, 2, 2},
		{"explicit down", "1", 2, 1, 1},
		{"explicit off", "0", 2, 0, 0},
		// A viewer cannot conjure parity the producer never emitted, so a
		// request above the fleet level is recorded but served lower — which
		// is what makes parityActive < parityRequested legible on the overlay.
		{"above the fleet level is clamped", "2", 1, 2, 1},
		{"garbage falls back to the fleet default", "banana", 2, 2, 2},
		{"negative is treated as garbage", "-1", 2, 2, 2},
		{"out of range is treated as garbage", "9", 2, 2, 2},
		{"fleet off means nobody gets it", "2", 0, 2, 0},
	} {
		for _, reliable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reliable=%v", tc.name, reliable), func(t *testing.T) {
				req, served := NegotiateParity(tc.param, tc.fleet, reliable)
				if req != tc.wantRequest || served != tc.wantServed {
					t.Fatalf("NegotiateParity(%q, fleet=%d, reliable=%v) = (%d, %d), want (%d, %d)",
						tc.param, tc.fleet, reliable, req, served, tc.wantRequest, tc.wantServed)
				}
			})
		}
	}
}

// TestParityFanOutIsPerSubscriberPrefix is the core of the design: the
// producer computes once, and each subscriber receives a PREFIX of the symbols
// matching its own k — from a single fan-out, with no relay-side computation.
func TestParityFanOutIsPerSubscriberPrefix(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f2, f1, f0 := &fakeSender{}, &fakeSender{}, &fakeSender{}
	s2, err := r.SubscribeParity(id, f2, 2, "")
	if err != nil {
		t.Fatalf("SubscribeParity(2): %v", err)
	}
	s1, err := r.SubscribeParity(id, f1, 1, "")
	if err != nil {
		t.Fatalf("SubscribeParity(1): %v", err)
	}
	s0, err := r.SubscribeParity(id, f0, 0, "")
	if err != nil {
		t.Fatalf("SubscribeParity(0): %v", err)
	}

	p.HandleDatagram(chunkDgram(t, false, 7, 0, 1, "aa"))
	p.HandleDatagram(parityDgram(t, 7, 0))
	p.HandleDatagram(parityDgram(t, 7, 1))
	s2.Close()
	s1.Close()
	s0.Close()

	// The data chunk reaches everyone regardless of parity level.
	for name, f := range map[string]*fakeSender{"k=2": f2, "k=1": f1, "k=0": f0} {
		if got := countType(f, wire.TypeVideoChunk); got != 1 {
			t.Errorf("%s received %d video chunks, want 1", name, got)
		}
	}
	if got := countType(f2, wire.TypeParityChunk); got != 2 {
		t.Errorf("k=2 subscriber received %d parity chunks, want 2", got)
	}
	if got := countType(f1, wire.TypeParityChunk); got != 1 {
		t.Errorf("k=1 subscriber received %d parity chunks, want 1", got)
	}
	if got := countType(f0, wire.TypeParityChunk); got != 0 {
		t.Errorf("k=0 subscriber received %d parity chunks, want 0", got)
	}
}

// The k=1 subscriber must receive symbol 0 (P), never symbol 1 (Q). P alone is
// the k=1 code; serving Q instead would be undecodable on its own.
func TestParityPrefixServesPNotQ(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.SubscribeParity(id, f, 1, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	// Deliberately send Q first, so passing cannot be an artefact of order.
	p.HandleDatagram(parityDgram(t, 7, 1))
	p.HandleDatagram(parityDgram(t, 7, 0))
	s.Close()

	var indices []uint8
	for _, d := range f.received() {
		if len(d) >= 2 && d[1] == wire.TypeParityChunk {
			h, _, err := wire.ParseParityChunk(d)
			if err != nil {
				t.Fatalf("ParseParityChunk: %v", err)
			}
			indices = append(indices, h.ParityIndex)
		}
	}
	if len(indices) != 1 || indices[0] != 0 {
		t.Fatalf("k=1 subscriber got parity indices %v, want exactly [0]", indices)
	}
}

// subscribeCarrier subscribes a reliable (dvr=false) or DVR (dvr=true)
// viewer served k parity symbols.
func subscribeCarrier(t *testing.T, r *Registry, id string, f *fakeSender, dvr bool, k int) *Subscriber {
	t.Helper()
	so := subscribeOpts{reliable: true, parityK: k, parityRequested: k}
	if dvr {
		so.dvr, so.bufferMs = true, 3000
	}
	s, err := r.subscribeOpts(id, f, so)
	if err != nil {
		t.Fatalf("subscribe (dvr=%v, k=%d): %v", dvr, k, err)
	}
	return s
}

// carrierTypes is the message type of every carrier record, in order.
func carrierTypes(t *testing.T, f *fakeSender) []uint8 {
	t.Helper()
	var types []uint8
	for _, rec := range f.carrierRecords(t) {
		types = append(types, rec[1])
	}
	return types
}

// Reliable and DVR delivery get parity too (docs/75 D11). Carrier
// retransmission repairs loss between the relay and the viewer, but a chunk the
// broadcaster's uplink lost never reached the relay: only parity brings it
// back. Each carrier subscriber gets its own PREFIX, as a datagram viewer does,
// as carrier records in fan-out order — and nothing on the datagram path.
func TestParityRidesTheCarrier(t *testing.T) {
	for _, dvr := range []bool{false, true} {
		t.Run(fmt.Sprintf("dvr=%v", dvr), func(t *testing.T) {
			r := NewRegistry(discardLog, Options{
				ParityDefault: 2,
				DVR:           DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
			})
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			f2, f1, f0 := &fakeSender{}, &fakeSender{}, &fakeSender{}
			subs := []*Subscriber{
				subscribeCarrier(t, r, id, f2, dvr, 2),
				subscribeCarrier(t, r, id, f1, dvr, 1),
				subscribeCarrier(t, r, id, f0, dvr, 0),
			}
			ingestKeyframe(t, p, keyframeMsg(t, 6, "vp8", "KEY6"))
			chunk := chunkDgram(t, false, 7, 0, 1, "aa")
			p0, p1 := parityDgram(t, 7, 0), parityDgram(t, 7, 1)
			p.HandleDatagram(chunk)
			p.HandleDatagram(p0)
			p.HandleDatagram(p1)
			// A marker after the parity, so a subscriber served fewer symbols is
			// known to have caught up rather than to be still on its way.
			marker := chunkDgram(t, false, 8, 0, 1, "bb")
			p.HandleDatagram(marker)

			for _, tc := range []struct {
				name string
				f    *fakeSender
				want [][]byte
			}{
				{"k=2", f2, [][]byte{chunk, p0, p1, marker}},
				{"k=1", f1, [][]byte{chunk, p0, marker}},
				{"k=0", f0, [][]byte{chunk, marker}},
			} {
				waitCarrierRecords(t, tc.f, len(tc.want))
				got := tc.f.carrierRecords(t)
				if len(got) != len(tc.want) {
					t.Fatalf("%s: carrier record types %x, want %d records", tc.name, carrierTypes(t, tc.f), len(tc.want))
				}
				for i := range tc.want {
					if !bytes.Equal(got[i], tc.want[i]) {
						t.Errorf("%s: record %d = %x, want %x", tc.name, i, got[i], tc.want[i])
					}
				}
				if n := countType(tc.f, wire.TypeParityChunk); n != 0 {
					t.Errorf("%s: %d parity chunks on the datagram path, want 0 — carrier viewers get them as records", tc.name, n)
				}
			}
			for _, s := range subs {
				s.Close()
			}
		})
	}
}

// The DVR ring keeps parity beside the video records it protects (docs/75
// D11), so a replayed GOP carries its symbols, and its byte bound counts them:
// the ring is shared by every DVR viewer and holds every symbol the producer
// emitted, whatever each cursor is served.
func TestDVRRingRetainsParity(t *testing.T) {
	r := NewRegistry(discardLog, Options{
		ParityDefault: 2,
		DVR:           DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
	})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// A k=0 cursor still makes the ring keep every symbol: the ring is the
	// broadcast's, and the next joiner may want them.
	s := subscribeCarrier(t, r, id, &fakeSender{}, true, 0)
	defer s.Close()

	kf := keyframeMsg(t, 6, "vp8", "KEY6")
	ingestKeyframe(t, p, kf)
	chunk := chunkDgram(t, false, 7, 0, 1, "aa")
	p0, p1 := parityDgram(t, 7, 0), parityDgram(t, 7, 1)
	for _, d := range [][]byte{chunk, p0, p1} {
		p.HandleDatagram(d)
	}

	ring := s.dvr
	c := ring.NewCursor().AtFirstRecord()
	var got [][]byte
	for {
		rec, ok := ring.Record(c)
		if !ok {
			break
		}
		got = append(got, rec)
		c = c.Next()
	}
	want := [][]byte{chunk, p0, p1}
	if len(got) != len(want) {
		t.Fatalf("ring holds %d records, want %d (the video chunk and both parity symbols)", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("ring record %d = %x, want %x", i, got[i], want[i])
		}
	}
	if wantBytes := len(kf) + len(chunk) + len(p0) + len(p1); ring.Bytes() != wantBytes {
		t.Errorf("ring.Bytes() = %d, want %d: the byte bound must count parity", ring.Bytes(), wantBytes)
	}
}

// egress_parity_bytes_total counts parity wherever it leaves (docs/75 D11):
// on a carrier, the framed record that was written, so the fleet cost of
// serving reliable and DVR viewers parity is measured, not modelled.
func TestParityEgressCountedOnTheCarrier(t *testing.T) {
	for _, dvr := range []bool{false, true} {
		t.Run(fmt.Sprintf("dvr=%v", dvr), func(t *testing.T) {
			r := NewRegistry(discardLog, Options{
				ParityDefault: 2,
				DVR:           DVROptions{Window: 30 * time.Second, MaxBytes: 1 << 20},
			})
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			f := &fakeSender{}
			s := subscribeCarrier(t, r, id, f, dvr, 2)
			ingestKeyframe(t, p, keyframeMsg(t, 6, "vp8", "KEY6"))
			p0, p1 := parityDgram(t, 7, 0), parityDgram(t, 7, 1)
			for _, d := range [][]byte{chunkDgram(t, false, 7, 0, 1, "aa"), p0, p1} {
				p.HandleDatagram(d)
			}
			waitCarrierRecords(t, f, 3)

			// Each record is the datagram behind a two-byte length prefix.
			want := uint64(len(p0) + 2 + len(p1) + 2)
			key := r.ObfuscateID(id)
			if got := r.Stats().Broadcasts[key].EgressParityBytes; got != want {
				t.Errorf("live EgressParityBytes = %d, want %d", got, want)
			}
			// And it survives its subscriber (CODE-REVIEW: counters survive
			// their owner's deletion).
			s.Close()
			if got := r.Stats().Broadcasts[key].EgressParityBytes; got != want {
				t.Errorf("EgressParityBytes after the subscriber left = %d, want %d", got, want)
			}
		})
	}
}

// The relay must compute no parity: it forwards what the producer emitted.
// A parity datagram is relayed verbatim, byte for byte.
func TestParityForwardedVerbatim(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.SubscribeParity(id, f, 2, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	want := parityDgram(t, 99, 1)
	p.HandleDatagram(want)
	s.Close()

	for _, d := range f.received() {
		if len(d) >= 2 && d[1] == wire.TypeParityChunk {
			if string(d) != string(want) {
				t.Fatalf("parity relayed as %x, want the producer's bytes %x", d, want)
			}
			return
		}
	}
	t.Fatal("no parity chunk relayed")
}

// Parity is video-adjacent but must not be counted as a relayed FRAME: the R9
// ingress-loss window keys on chunkIndex == 0 of a VideoChunk, and a parity
// datagram entering that accounting would corrupt both framesRelayed and the
// loss estimate.
func TestParityDoesNotCountAsFrame(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	s, err := r.SubscribeParity(id, f, 2, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	p.HandleDatagram(chunkDgram(t, false, 7, 0, 1, "aa"))
	p.HandleDatagram(parityDgram(t, 7, 0))
	p.HandleDatagram(parityDgram(t, 7, 1))
	s.Close()

	bst := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if bst.FramesRelayed != 1 {
		t.Errorf("FramesRelayed = %d, want 1 (parity must not count as a frame)", bst.FramesRelayed)
	}
	if bst.BadDatagrams != 0 {
		t.Errorf("BadDatagrams = %d, want 0 (parity is a known type)", bst.BadDatagrams)
	}
	if bst.ParityDatagramsForwarded != 2 {
		t.Errorf("ParityDatagramsForwarded = %d, want 2", bst.ParityDatagramsForwarded)
	}
}

// An EDGE session is not a viewer, and the per-subscriber prefix must not be
// applied to it: it is the downstream pod's plumbing, and the origin cannot
// know that pod's viewers' k. Clamping here would strip symbols the edge can
// never get back — which is precisely how the cascade lost them (docs/34 §5.1,
// the R19 "convert at the serving pod" rule).
func TestParityReachesEdgeSessionsWhole(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	edge := &fakeSender{}
	s, err := r.SubscribeInternal(id, edge)
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}
	p.HandleDatagram(chunkDgram(t, false, 7, 0, 1, "aa"))
	p.HandleDatagram(parityDgram(t, 7, 0))
	p.HandleDatagram(parityDgram(t, 7, 1))
	s.Close()

	if got := countType(edge, wire.TypeParityChunk); got != 2 {
		t.Fatalf("edge session received %d parity chunks, want 2 (every symbol the producer emitted)", got)
	}
	// Bytes that went out on the wire are forwarded, not suppressed — an
	// origin whose only subscriber is an edge must not read as "forwarded
	// none", which is what the cluster assertion checks.
	bst := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if bst.ParityDatagramsForwarded != 2 || bst.ParitySuppressed != 0 {
		t.Fatalf("ParityDatagramsForwarded/Suppressed = %d/%d, want 2/0",
			bst.ParityDatagramsForwarded, bst.ParitySuppressed)
	}
}

// The cascade end to end, in one process: whatever the origin hands its edge
// session must be enough for the SECOND hop to serve its own viewers their own
// prefixes. This is the assertion the two-pod e2e makes, at unit speed.
func TestParitySurvivesTheCascade(t *testing.T) {
	origin := NewRegistry(discardLog, Options{ParityDefault: 2})
	id, pub, err := origin.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	upstream := &fakeSender{}
	edgeSess, err := origin.SubscribeInternal(id, upstream)
	if err != nil {
		t.Fatalf("SubscribeInternal: %v", err)
	}

	// The edge pod: its own registry, whose publisher is fed by what the
	// origin sent downstream (transport/edge.go's read loop, in miniature).
	edge := NewRegistry(discardLog, Options{ParityDefault: 2})
	edgeID, edgePub, err := edge.EdgePublish(id)
	if err != nil {
		t.Fatalf("edge EdgePublish: %v", err)
	}
	f2, f1 := &fakeSender{}, &fakeSender{}
	v2, err := edge.SubscribeParity(edgeID, f2, 2, "")
	if err != nil {
		t.Fatalf("edge SubscribeParity(2): %v", err)
	}
	v1, err := edge.SubscribeParity(edgeID, f1, 1, "")
	if err != nil {
		t.Fatalf("edge SubscribeParity(1): %v", err)
	}

	pub.HandleDatagram(chunkDgram(t, false, 7, 0, 1, "aa"))
	pub.HandleDatagram(parityDgram(t, 7, 0))
	pub.HandleDatagram(parityDgram(t, 7, 1))
	edgeSess.Close()
	for _, d := range upstream.received() {
		edgePub.HandleDatagram(d)
	}
	v2.Close()
	v1.Close()

	if got := countType(f2, wire.TypeParityChunk); got != 2 {
		t.Errorf("edge-pod k=2 viewer received %d parity chunks, want 2", got)
	}
	if got := countType(f1, wire.TypeParityChunk); got != 1 {
		t.Errorf("edge-pod k=1 viewer received %d parity chunks, want 1", got)
	}
}

// With the fleet default at 0 the feature is entirely inert: no capability is
// advertised, so producers emit nothing, and any parity that somehow arrives
// is suppressed for every subscriber.
func TestParityFleetOffSuppressesEverything(t *testing.T) {
	r := NewRegistry(discardLog, Options{ParityDefault: 0})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	req, served := NegotiateParity("2", 0, false)
	if served != 0 {
		t.Fatalf("served = %d with the fleet off, want 0 (requested %d)", served, req)
	}
	s, err := r.SubscribeParity(id, f, served, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	p.HandleDatagram(parityDgram(t, 7, 0))
	s.Close()
	if got := countType(f, wire.TypeParityChunk); got != 0 {
		t.Errorf("subscriber received %d parity chunks with the fleet off, want 0", got)
	}
}
