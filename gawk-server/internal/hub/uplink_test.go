package hub

// R73 RU1 (docs/75 D1/D2): the relay half of the reliable uplink. A carrier
// record is byte for byte the datagram it replaces, and its ingest must be
// exactly that datagram's ingest.

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// carrierBytes frames records as a publisher's uplink carrier: the R19
// prologue, then uint16 length ‖ datagram per record.
func carrierBytes(t *testing.T, records ...[]byte) []byte {
	t.Helper()
	out := wire.AppendCarrierPrologue(nil)
	for _, r := range records {
		var err error
		out, err = wire.AppendCarrierRecord(out, r)
		if err != nil {
			t.Fatalf("AppendCarrierRecord: %v", err)
		}
	}
	return out
}

func uplinkParityDgram(t *testing.T, frameID uint32, index uint8, count uint16, payload []byte) []byte {
	t.Helper()
	d, err := wire.AppendParityChunk(nil, wire.ParityChunkHeader{
		FrameID: frameID, ParityIndex: index, ChunkCount: count,
		FrameBytes: uint32(count) * uint32(len(payload)),
	}, payload)
	if err != nil {
		t.Fatalf("AppendParityChunk: %v", err)
	}
	return d
}

// uplinkGop is one GOP as a broadcaster would emit it: a keyframe on its own
// stream, then the deltas' chunk and parity datagrams in send order.
type uplinkGop struct {
	keyframe []byte // nil: deltas sent before any keyframe
	dgrams   [][]byte
}

// randomUplink builds GOPs of random shape: random chunk counts, payloads
// and parity, frame IDs that may cross the uint32 wrap, and the occasional
// chunk lost before the relay (missing from both paths alike), so the
// ingress-loss window has something to count.
func randomUplink(t *testing.T, rng *rand.Rand) []uplinkGop {
	t.Helper()
	frameID := rng.Uint32()
	if rng.IntN(3) == 0 {
		frameID = math32Max - uint32(rng.IntN(40)) // cross the wrap
	}
	var gops []uplinkGop
	nGops := 1 + rng.IntN(4)
	for g := range nGops {
		var gop uplinkGop
		if g > 0 || rng.IntN(4) != 0 {
			gop.keyframe = keyframeMsg(t, frameID, "avc1.42E01F", string(randBytes(rng, 1+rng.IntN(3000))))
			frameID++
		}
		nDeltas := rng.IntN(25)
		for range nDeltas {
			count := 1 + rng.IntN(6)
			for i := range count {
				if rng.IntN(40) == 0 {
					continue // lost on the way to the relay, on both paths
				}
				payload := randBytes(rng, 1+rng.IntN(wire.MaxChunkPayload))
				d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
					FrameID: frameID, ChunkIndex: uint16(i), ChunkCount: uint16(count),
					TimestampUs: uint64(frameID) * 16_667,
				}, payload)
				if err != nil {
					t.Fatalf("AppendVideoChunk: %v", err)
				}
				gop.dgrams = append(gop.dgrams, d)
			}
			for p := range rng.IntN(wire.MaxParitySymbols + 1) {
				gop.dgrams = append(gop.dgrams, uplinkParityDgram(t, frameID, uint8(p), uint16(count), randBytes(rng, 1+rng.IntN(wire.MaxChunkPayload))))
			}
			frameID++
		}
		gops = append(gops, gop)
	}
	return gops
}

const math32Max = ^uint32(0)

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

// uplinkSide is one relay fed the same GOPs, by datagram or by carrier, with
// one subscriber of each delivery kind a record can reach.
type uplinkSide struct {
	r        *Registry
	id       string
	pub      *Publisher
	datagram *fakeSender // live-edge, served full parity
	reliable *fakeSender // R19 carrier delivery
	dvr      *fakeSender // allocates the DVR ring
	subs     []*Subscriber
}

func newUplinkSide(t *testing.T) *uplinkSide {
	t.Helper()
	r := NewRegistry(discardLog, Options{ParityDefault: 2, UplinkCarriers: true, QueueDepth: 1 << 16})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	u := &uplinkSide{r: r, id: id, pub: pub, datagram: &fakeSender{}, reliable: &fakeSender{}, dvr: &fakeSender{}}
	s1, err := r.SubscribeParity(id, u.datagram, 2, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	s2, err := r.SubscribeReliable(id, u.reliable, 2)
	if err != nil {
		t.Fatalf("SubscribeReliable: %v", err)
	}
	s3, err := r.SubscribeDVR(id, u.dvr, 30_000, 2)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	u.subs = []*Subscriber{s1, s2, s3}
	return u
}

// settle waits until every subscriber's queue is empty and the reliable
// viewer's carrier bytes have stopped moving: delivery is asynchronous, and
// closing a subscriber cancels whatever its drain has not written yet.
func (u *uplinkSide) settle(t *testing.T) {
	t.Helper()
	carrierBytesNow := func() int {
		n := 0
		for _, st := range u.reliable.carrierStreams() {
			n += len(st.bytes())
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	last, stable := -1, 0
	for stable < 3 {
		if time.Now().After(deadline) {
			t.Fatal("subscribers never settled")
		}
		time.Sleep(5 * time.Millisecond)
		idle := true
		for _, d := range uplinkStats(t, u.r, u.id).SubscriberDetails {
			if d.QueueDepth != 0 {
				idle = false
			}
		}
		if n := carrierBytesNow(); idle && n == last {
			stable++
		} else {
			last, stable = n, 0
		}
	}
}

// dvrContents snapshots the ring: each GOP's keyframe and records.
func (u *uplinkSide) dvrContents(t *testing.T) [][][]byte {
	t.Helper()
	u.r.mu.Lock()
	b := u.r.hubs[u.id]
	u.r.mu.Unlock()
	if b == nil || b.dvr == nil {
		t.Fatal("no DVR ring allocated")
	}
	b.dvr.mu.RLock()
	defer b.dvr.mu.RUnlock()
	var out [][][]byte
	for _, g := range b.dvr.gops {
		out = append(out, append([][]byte{g.keyframe}, g.records...))
	}
	return out
}

// comparable strips what legitimately differs between the two relays: the
// uplink counters (the point of the comparison is everything else), the
// per-subscriber random keys and timing-dependent details.
func comparableStats(s Stats) Stats {
	s.UplinkCarriers, s.UplinkRecords, s.UplinkRecordsStale, s.UplinkCarriersRejected = 0, 0, 0, 0
	s.SubscriberDetails = nil
	s.DVRRingBytes, s.DVRRingGops = 0, 0 // compared via the ring contents
	// How many R19 carriers the reliable viewer's drain opened (each a
	// 2-byte prologue) depends on its timing against the keyframe writer,
	// not on the ingest path; its records are compared byte for byte instead.
	s.CarrierStreams, s.EgressCarrierBytes = 0, 0
	return s
}

// Records are ingested exactly as the same bytes sent as datagrams: the same
// fan-out to datagram, R19-carrier and DVR subscribers, the same accounting,
// the same ring (docs/75 RU1, property over random frames).
func TestUplinkCarrierRecordsIngestExactlyAsDatagrams(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0x75))
		gops := randomUplink(t, rng)

		viaDgram, viaCarrier := newUplinkSide(t), newUplinkSide(t)
		records, keyframes := 0, 0
		for _, g := range gops {
			if g.keyframe != nil {
				ingestKeyframe(t, viaDgram.pub, g.keyframe)
				ingestKeyframe(t, viaCarrier.pub, g.keyframe)
				// Keyframe delivery is asynchronous, and a reliable viewer's
				// carrier rotates on it: let it land on both sides first, so
				// the two relays see the same interleaving.
				keyframes++
				for _, side := range []*uplinkSide{viaDgram, viaCarrier} {
					waitKeyframes(t, side.datagram, keyframes)
					waitKeyframes(t, side.reliable, keyframes)
				}
			}
			for _, d := range g.dgrams {
				viaDgram.pub.HandleDatagram(d)
			}
			// One carrier per GOP (D1), opened after its keyframe.
			if err := viaCarrier.pub.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, g.dgrams...))); err != nil {
				t.Fatalf("seed %d: IngestUplinkCarrier: %v", seed, err)
			}
			records += len(g.dgrams)
		}

		wantDVR, gotDVR := viaDgram.dvrContents(t), viaCarrier.dvrContents(t)
		if !reflect.DeepEqual(gotDVR, wantDVR) {
			t.Fatalf("seed %d: DVR ring differs: %d GOPs via carrier, %d via datagrams", seed, len(gotDVR), len(wantDVR))
		}
		for _, side := range []*uplinkSide{viaDgram, viaCarrier} {
			side.settle(t)
			for _, s := range side.subs {
				s.Close()
			}
		}

		if got, want := viaCarrier.datagram.received(), viaDgram.datagram.received(); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: datagram viewer got %d datagrams via carrier, %d via datagrams", seed, len(got), len(want))
		}
		if got, want := viaCarrier.reliable.carrierRecords(t), viaDgram.reliable.carrierRecords(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: reliable viewer got %d records via carrier, %d via datagrams", seed, len(got), len(want))
		}

		want := viaDgram.r.Stats().Broadcasts[viaDgram.r.ObfuscateID(viaDgram.id)]
		got := viaCarrier.r.Stats().Broadcasts[viaCarrier.r.ObfuscateID(viaCarrier.id)]
		if !reflect.DeepEqual(comparableStats(got), comparableStats(want)) {
			t.Fatalf("seed %d: stats differ:\n via carrier   %+v\n via datagrams %+v", seed, comparableStats(got), comparableStats(want))
		}
		if got.UplinkCarriers != uint64(len(gops)) || got.UplinkRecords != uint64(records) || got.UplinkRecordsStale != 0 {
			t.Fatalf("seed %d: uplink counters = carriers %d records %d stale %d, want %d / %d / 0",
				seed, got.UplinkCarriers, got.UplinkRecords, got.UplinkRecordsStale, len(gops), records)
		}
		if want.UplinkCarriers != 0 || want.UplinkRecords != 0 {
			t.Fatalf("seed %d: the datagram path moved uplink counters: %+v", seed, want)
		}
	}
}

// heldCarrier is a carrier whose publisher keeps it open: IngestUplinkCarrier
// runs on its own goroutine until the writer closes.
type heldCarrier struct {
	w    *io.PipeWriter
	done chan error
}

func holdCarrier(t *testing.T, p *Publisher) *heldCarrier {
	t.Helper()
	pr, pw := io.Pipe()
	h := &heldCarrier{w: pw, done: make(chan error, 1)}
	go func() { h.done <- p.IngestUplinkCarrier(pr) }()
	if _, err := pw.Write(wire.AppendCarrierPrologue(nil)); err != nil {
		t.Fatalf("prologue: %v", err)
	}
	return h
}

func (h *heldCarrier) finish(t *testing.T) {
	t.Helper()
	h.w.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("held carrier ended with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held carrier never returned")
	}
}

func uplinkStats(t *testing.T, r *Registry, id string) Stats {
	t.Helper()
	st, ok := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if !ok {
		t.Fatal("broadcast missing from stats")
	}
	return st
}

// At most MaxUplinkCarriers are open per publisher: a third is refused and
// counted, and a slot freed by a finished carrier is reusable.
func TestUplinkCarrierThirdConcurrentIsRejected(t *testing.T) {
	r := NewRegistry(discardLog, Options{UplinkCarriers: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	a, b := holdCarrier(t, p), holdCarrier(t, p)
	waitFor(t, 5*time.Second, func() bool { return uplinkStats(t, r, id).UplinkCarriers == 2 }, "two carriers open")

	if err := p.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, chunkDgram(t, false, 5, 0, 1, "x")))); !errors.Is(err, ErrUplinkCarrierLimit) {
		t.Fatalf("third carrier: err = %v, want ErrUplinkCarrierLimit", err)
	}
	st := uplinkStats(t, r, id)
	if st.UplinkCarriersRejected != 1 || st.UplinkCarriers != 2 || st.UplinkRecords != 0 {
		t.Fatalf("after the third: rejected %d carriers %d records %d, want 1 / 2 / 0",
			st.UplinkCarriersRejected, st.UplinkCarriers, st.UplinkRecords)
	}

	a.finish(t)
	if err := p.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, chunkDgram(t, false, 5, 0, 1, "x")))); err != nil {
		t.Fatalf("carrier after a slot freed: %v", err)
	}
	b.finish(t)
	st = uplinkStats(t, r, id)
	if st.UplinkCarriers != 3 || st.UplinkCarriersRejected != 1 || st.UplinkRecords != 1 {
		t.Fatalf("end: carriers %d rejected %d records %d, want 3 / 1 / 1", st.UplinkCarriers, st.UplinkCarriersRejected, st.UplinkRecords)
	}
}

// A malformed record ends its carrier; the records before it were forwarded
// and nothing after it is.
func TestUplinkCarrierMalformedRecordEndsCarrier(t *testing.T) {
	good1 := chunkDgram(t, false, 10, 0, 2, "aa")
	good2 := chunkDgram(t, false, 10, 1, 2, "bb")
	after := chunkDgram(t, false, 11, 0, 1, "cc")
	frame := func(recs ...[]byte) []byte { return carrierBytes(t, recs...) }
	rawRecord := func(n uint16, body []byte) []byte { return append([]byte{byte(n >> 8), byte(n)}, body...) }

	shortChunk := chunkDgram(t, false, 10, 0, 1, "z")[:wire.VideoChunkHeaderSize-1]
	badVersion := append([]byte(nil), good1...)
	badVersion[0] = 0x7f
	for name, tail := range map[string][]byte{
		"zero length":       rawRecord(0, nil),
		"oversize length":   rawRecord(wire.MaxDatagramSize+1, make([]byte, wire.MaxDatagramSize+1)),
		"decoder config":    frame(configDgram(t, "avc1.42E01F"))[wire.CarrierPrologueSize:],
		"time sync":         frame(wire.AppendTimeSync(nil, 1, 2))[wire.CarrierPrologueSize:],
		"bad version":       frame(badVersion)[wire.CarrierPrologueSize:],
		"truncated header":  frame(shortChunk)[wire.CarrierPrologueSize:],
		"unknown type 0x7e": frame([]byte{wire.Version, 0x7e, 0, 0})[wire.CarrierPrologueSize:],
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry(discardLog, Options{UplinkCarriers: true})
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			f := &fakeSender{}
			s, err := r.Subscribe(id, f)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			stream := append(frame(good1, good2), tail...)
			stream = append(stream, frame(after)[wire.CarrierPrologueSize:]...)
			if err := p.IngestUplinkCarrier(bytes.NewReader(stream)); !errors.Is(err, ErrBadUplinkRecord) {
				t.Fatalf("err = %v, want ErrBadUplinkRecord", err)
			}
			s.Close()
			wantDatagrams(t, f, [][]byte{good1, good2})
			st := uplinkStats(t, r, id)
			if st.UplinkRecords != 2 || st.BadDatagrams != 1 {
				t.Fatalf("records %d bad %d, want 2 / 1", st.UplinkRecords, st.BadDatagrams)
			}
		})
	}
}

// A record whose frame serially precedes the newest ingested keyframe is
// dropped and counted; the keyframe's own frame and later ones pass. Serial,
// across the wrap; and only this session's keyframe counts.
func TestUplinkCarrierDropsRecordsBehindNewestKeyframe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keyframe uint32
		stale    []uint32
		fresh    []uint32
	}{
		{"plain", 100, []uint32{99, 1, math32Max - 1<<20}, []uint32{100, 101, 5000}},
		{"across the wrap", 2, []uint32{0, math32Max, math32Max - 10}, []uint32{2, 3}},
		{"keyframe before the wrap", math32Max - 1, []uint32{math32Max - 2}, []uint32{math32Max, 0, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(discardLog, Options{UplinkCarriers: true})
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			f := &fakeSender{}
			s, err := r.Subscribe(id, f)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			ingestKeyframe(t, p, keyframeMsg(t, tc.keyframe, "avc1.42E01F", "kf"))
			waitKeyframes(t, f, 1)

			var recs, want [][]byte
			for _, id := range tc.stale {
				recs = append(recs, chunkDgram(t, false, id, 0, 1, "stale"))
			}
			for _, id := range tc.fresh {
				d := chunkDgram(t, false, id, 0, 1, "fresh")
				recs = append(recs, d)
				want = append(want, d)
			}
			if err := p.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, recs...))); err != nil {
				t.Fatalf("IngestUplinkCarrier: %v", err)
			}
			s.Close()
			wantDatagrams(t, f, want)
			st := uplinkStats(t, r, id)
			if st.UplinkRecordsStale != uint64(len(tc.stale)) || st.UplinkRecords != uint64(len(tc.fresh)) {
				t.Fatalf("stale %d records %d, want %d / %d", st.UplinkRecordsStale, st.UplinkRecords, len(tc.stale), len(tc.fresh))
			}
		})
	}

	// A reclaim restarts frame IDs: the previous session's keyframe must not
	// make the new session's records stale.
	t.Run("new session", func(t *testing.T) {
		r := NewRegistry(discardLog, Options{UplinkCarriers: true, BroadcastGrace: time.Minute})
		id, p, err := r.StartPublish("")
		if err != nil {
			t.Fatalf("StartPublish: %v", err)
		}
		ingestKeyframe(t, p, keyframeMsg(t, 900, "avc1.42E01F", "kf"))
		p.Close()
		_, p2, err := r.ResumePublish(id)
		if err != nil {
			t.Fatalf("ResumePublish: %v", err)
		}
		if err := p2.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, chunkDgram(t, false, 3, 0, 1, "new")))); err != nil {
			t.Fatalf("IngestUplinkCarrier: %v", err)
		}
		if st := uplinkStats(t, r, id); st.UplinkRecordsStale != 0 || st.UplinkRecords != 1 {
			t.Fatalf("stale %d records %d, want 0 / 1", st.UplinkRecordsStale, st.UplinkRecords)
		}
	})
}

// A carrier that ends mid-record (the sender's reset of an expired GOP)
// returns the read error for the transport to reset; it is not malformed.
func TestUplinkCarrierTruncatedIsNotBad(t *testing.T) {
	r := NewRegistry(discardLog, Options{UplinkCarriers: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	stream := carrierBytes(t, chunkDgram(t, false, 1, 0, 1, "a"), chunkDgram(t, false, 2, 0, 1, "b"))
	if err := p.IngestUplinkCarrier(bytes.NewReader(stream[:len(stream)-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if st := uplinkStats(t, r, id); st.UplinkRecords != 1 || st.BadDatagrams != 0 {
		t.Fatalf("records %d bad %d, want 1 / 0", st.UplinkRecords, st.BadDatagrams)
	}
}
