package hub

// R84 BR1 (docs/88 D2, §5.2, K3): the keyframe carrier. A GOP stream whose
// prologue is Version‖TypeKeyframeCarrier, its keyframe's StreamFrame at the
// head and RU1's records after it. The keyframe goes to the keyframe path as
// soon as its declared length is read; the rest is RU1's carrier.

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

// keyframeCarrierBytes frames a GOP as a keyframe carrier: the prologue, the
// keyframe's StreamFrame message, then the records.
func keyframeCarrierBytes(t *testing.T, keyframe []byte, records ...[]byte) []byte {
	t.Helper()
	out := wire.AppendKeyframeCarrierPrologue(nil)
	out = append(out, keyframe...)
	for _, r := range records {
		var err error
		if out, err = wire.AppendCarrierRecord(out, r); err != nil {
			t.Fatalf("AppendCarrierRecord: %v", err)
		}
	}
	return out
}

// cachedKeyframe is the hub's join-priming keyframe, nil when none.
func cachedKeyframe(t *testing.T, r *Registry, id string) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.hubs[id]
	if b == nil {
		t.Fatal("no hub")
	}
	return b.cachedKeyframe
}

// The keyframe reaches the cache and the subscribers' keyframe streams once
// its declared length has been read, while the publisher still holds the
// stream open: a GOP's carrier lives until the next keyframe, and a viewer
// joining meanwhile must be primed with it (docs/88 BR1, red first).
func TestKeyframeCarrierKeyframeLandsBeforeFIN(t *testing.T) {
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
	defer s.Close()

	kf := keyframeMsg(t, 40, "avc1.42E01F", "keyframe-payload")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- p.IngestKeyframeCarrier(pr) }()
	if _, err := pw.Write(keyframeCarrierBytes(t, kf)); err != nil {
		t.Fatalf("write head: %v", err)
	}

	// The FIN is held back: the keyframe must already be cached and fanned out.
	waitFor(t, 5*time.Second, func() bool { return bytes.Equal(cachedKeyframe(t, r, id), kf) }, "keyframe cached before the FIN")
	waitKeyframes(t, f, 1)
	if got := f.receivedKeyframes()[0]; !bytes.Equal(got, kf) {
		t.Fatalf("subscriber's keyframe = %x, want %x", got, kf)
	}

	// The records after it are the GOP's deltas, ingested as RU1's.
	d1, d2 := chunkDgram(t, false, 41, 0, 1, "d1"), chunkDgram(t, false, 42, 0, 1, "d2")
	for _, d := range [][]byte{d1, d2} {
		rec, err := wire.AppendCarrierRecord(nil, d)
		if err != nil {
			t.Fatalf("AppendCarrierRecord: %v", err)
		}
		if _, err := pw.Write(rec); err != nil {
			t.Fatalf("write record: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool { return len(f.received()) == 2 }, "records forwarded before the FIN")
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IngestKeyframeCarrier = %v, want nil at a clean FIN", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IngestKeyframeCarrier never returned")
	}
	wantDatagrams(t, f, [][]byte{d1, d2})
	st := uplinkStats(t, r, id)
	if st.UplinkCarriers != 1 || st.UplinkRecords != 2 || st.KeyframeStreamsIn != 1 || st.BadDatagrams != 0 {
		t.Fatalf("carriers %d records %d keyframes %d bad %d, want 1 / 2 / 1 / 0",
			st.UplinkCarriers, st.UplinkRecords, st.KeyframeStreamsIn, st.BadDatagrams)
	}
}

// A keyframe carrier is, to everything downstream, a keyframe stream followed
// by RU1's carrier: the same fan-out to datagram, R19-carrier and DVR
// subscribers, the same accounting and the same ring as the keyframe stream
// and datagrams it replaces (docs/88 G7; RU1's property over random frames).
func TestKeyframeCarrierIngestsExactlyAsKeyframeStreamAndDatagrams(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0x84))
		gops := randomUplink(t, rng)

		viaDgram, viaCarrier := newUplinkSide(t), newUplinkSide(t)
		records, keyframes := 0, 0
		for _, g := range gops {
			if g.keyframe != nil {
				ingestKeyframe(t, viaDgram.pub, g.keyframe)
			}
			// The carrier side hands its keyframe over inside the GOP stream; a
			// GOP with no keyframe (deltas before any) is RU1's plain carrier.
			if g.keyframe != nil {
				pr, pw := io.Pipe()
				done := make(chan error, 1)
				go func() { done <- viaCarrier.pub.IngestKeyframeCarrier(pr) }()
				if _, err := pw.Write(keyframeCarrierBytes(t, g.keyframe)); err != nil {
					t.Fatalf("write head: %v", err)
				}
				keyframes++
				for _, side := range []*uplinkSide{viaDgram, viaCarrier} {
					waitKeyframes(t, side.datagram, keyframes)
					waitKeyframes(t, side.reliable, keyframes)
				}
				var tail []byte
				for _, d := range g.dgrams {
					var err error
					if tail, err = wire.AppendCarrierRecord(tail, d); err != nil {
						t.Fatalf("AppendCarrierRecord: %v", err)
					}
				}
				go func() { _, _ = pw.Write(tail); pw.Close() }()
				if err := <-done; err != nil {
					t.Fatalf("seed %d: IngestKeyframeCarrier: %v", seed, err)
				}
			} else if err := viaCarrier.pub.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, g.dgrams...))); err != nil {
				t.Fatalf("seed %d: IngestUplinkCarrier: %v", seed, err)
			}
			for _, d := range g.dgrams {
				viaDgram.pub.HandleDatagram(d)
			}
			records += len(g.dgrams)
		}

		wantDVR, gotDVR := viaDgram.dvrContents(t), viaCarrier.dvrContents(t)
		if !reflect.DeepEqual(gotDVR, wantDVR) {
			t.Fatalf("seed %d: DVR ring differs: %d GOPs via keyframe carriers, %d via streams and datagrams", seed, len(gotDVR), len(wantDVR))
		}
		for _, side := range []*uplinkSide{viaDgram, viaCarrier} {
			side.settle(t)
			for _, s := range side.subs {
				s.Close()
			}
		}
		if got, want := viaCarrier.datagram.received(), viaDgram.datagram.received(); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: datagram viewer got %d datagrams via keyframe carriers, %d via datagrams", seed, len(got), len(want))
		}
		if got, want := viaCarrier.datagram.receivedKeyframes(), viaDgram.datagram.receivedKeyframes(); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: datagram viewer got %d keyframes via keyframe carriers, %d via keyframe streams", seed, len(got), len(want))
		}
		if got, want := viaCarrier.reliable.carrierRecords(t), viaDgram.reliable.carrierRecords(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: reliable viewer got %d records via keyframe carriers, %d via datagrams", seed, len(got), len(want))
		}
		want := viaDgram.r.Stats().Broadcasts[viaDgram.r.ObfuscateID(viaDgram.id)]
		got := viaCarrier.r.Stats().Broadcasts[viaCarrier.r.ObfuscateID(viaCarrier.id)]
		if !reflect.DeepEqual(comparableStats(got), comparableStats(want)) {
			t.Fatalf("seed %d: stats differ:\n via keyframe carriers %+v\n via datagrams         %+v", seed, comparableStats(got), comparableStats(want))
		}
		if got.UplinkCarriers != uint64(len(gops)) || got.UplinkRecords != uint64(records) || got.UplinkRecordsStale != 0 {
			t.Fatalf("seed %d: uplink counters = carriers %d records %d stale %d, want %d / %d / 0",
				seed, got.UplinkCarriers, got.UplinkRecords, got.UplinkRecordsStale, len(gops), records)
		}
	}
}

// A keyframe carrier holds one of the publisher's MaxUplinkCarriers slots,
// shared with RU1's carriers, for its whole life.
func TestKeyframeCarrierCountsAgainstMaxUplinkCarriers(t *testing.T) {
	r := NewRegistry(discardLog, Options{UplinkCarriers: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	// Two keyframe carriers held open past their keyframes, and one of RU1's.
	var held []*io.PipeWriter
	var dones []chan error
	for i := range 2 {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- p.IngestKeyframeCarrier(pr) }()
		if _, err := pw.Write(keyframeCarrierBytes(t, keyframeMsg(t, uint32(10*i), "", "kf"))); err != nil {
			t.Fatalf("write: %v", err)
		}
		held, dones = append(held, pw), append(dones, done)
	}
	c := holdCarrier(t, p)
	waitFor(t, 5*time.Second, func() bool {
		st := uplinkStats(t, r, id)
		return st.UplinkCarriers == 3 && st.KeyframeStreamsIn == 2
	}, "three carriers open, both keyframes in")

	// A fourth, of either kind, is refused and counted.
	if err := p.IngestKeyframeCarrier(bytes.NewReader(keyframeCarrierBytes(t, keyframeMsg(t, 30, "", "kf")))); !errors.Is(err, ErrUplinkCarrierLimit) {
		t.Fatalf("fourth (keyframe carrier): err = %v, want ErrUplinkCarrierLimit", err)
	}
	if err := p.IngestUplinkCarrier(bytes.NewReader(carrierBytes(t, chunkDgram(t, false, 31, 0, 1, "x")))); !errors.Is(err, ErrUplinkCarrierLimit) {
		t.Fatalf("fourth (carrier): err = %v, want ErrUplinkCarrierLimit", err)
	}
	st := uplinkStats(t, r, id)
	if st.UplinkCarriersRejected != 2 || st.UplinkCarriers != 3 || st.KeyframeStreamsIn != 2 {
		t.Fatalf("rejected %d carriers %d keyframes %d, want 2 / 3 / 2", st.UplinkCarriersRejected, st.UplinkCarriers, st.KeyframeStreamsIn)
	}

	// A slot freed by a finished keyframe carrier is reusable.
	held[0].Close()
	if err := <-dones[0]; err != nil {
		t.Fatalf("first keyframe carrier: %v", err)
	}
	if err := p.IngestKeyframeCarrier(bytes.NewReader(keyframeCarrierBytes(t, keyframeMsg(t, 40, "", "kf")))); err != nil {
		t.Fatalf("keyframe carrier after a slot freed: %v", err)
	}
	held[1].Close()
	<-dones[1]
	c.finish(t)
}

// A keyframe part that is malformed, oversized, has its keyframe flag clear,
// or is cut short ends the stream with nothing cached and nothing fanned out.
// Malformed input is counted as bad and an oversize keyframe as oversize, as
// on a keyframe stream; a stream cut short (the sender's 2 s abort, D3) is
// neither.
func TestKeyframeCarrierRejectsBadKeyframes(t *testing.T) {
	good := keyframeMsg(t, 7, "avc1.42E01F", "payload")
	flagClear := append([]byte(nil), good...)
	flagClear[2] = 0 // StreamFrame flags: keyframe bit clear
	badType := append([]byte(nil), good...)
	badType[1] = wire.TypeVideoChunk
	badVersion := append([]byte(nil), good...)
	badVersion[0] = 0x7f
	big := keyframeMsg(t, 7, "", string(make([]byte, 4096)))
	rec := chunkDgram(t, false, 8, 0, 1, "d")

	for name, tc := range map[string]struct {
		stream       []byte
		bad, oversz  uint64
		wantSentinel error
	}{
		"keyframe flag clear":      {stream: keyframeCarrierBytes(t, flagClear, rec), bad: 1, wantSentinel: ErrBadKeyframeCarrier},
		"not a stream frame":       {stream: keyframeCarrierBytes(t, badType, rec), bad: 1},
		"bad stream frame version": {stream: keyframeCarrierBytes(t, badVersion, rec), bad: 1},
		"oversize":                 {stream: keyframeCarrierBytes(t, big, rec), oversz: 1},
		"bad prologue":             {stream: append(wire.AppendCarrierPrologue(nil), good...), bad: 1},
		"cut short in the header":  {stream: keyframeCarrierBytes(t, good)[:wire.CarrierPrologueSize+10]},
		"cut short in the payload": {stream: keyframeCarrierBytes(t, good)[:wire.CarrierPrologueSize+len(good)-1]},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRegistry(discardLog, Options{UplinkCarriers: true, MaxKeyframeBytes: 1024})
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			f := &fakeSender{}
			s, err := r.Subscribe(id, f)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			err = p.IngestKeyframeCarrier(bytes.NewReader(tc.stream))
			if err == nil {
				t.Fatal("err = nil, want the stream ended")
			}
			if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("err = %v, want %v", err, tc.wantSentinel)
			}
			s.Close()
			if kf := cachedKeyframe(t, r, id); kf != nil {
				t.Fatalf("cached %x, want nothing", kf)
			}
			if n := len(f.receivedKeyframes()); n != 0 {
				t.Fatalf("subscriber got %d keyframes, want 0", n)
			}
			wantDatagrams(t, f, nil)
			st := uplinkStats(t, r, id)
			if st.BadDatagrams != tc.bad || st.KeyframeStreamsOversize != tc.oversz || st.KeyframeStreamsIn != 0 || st.UplinkRecords != 0 {
				t.Fatalf("bad %d oversize %d keyframes %d records %d, want %d / %d / 0 / 0",
					st.BadDatagrams, st.KeyframeStreamsOversize, st.KeyframeStreamsIn, st.UplinkRecords, tc.bad, tc.oversz)
			}
		})
	}
}

// A malformed record after the keyframe ends the carrier as RU1's does: the
// keyframe and the records before it were handed on, nothing after it is.
func TestKeyframeCarrierMalformedRecordAfterKeyframe(t *testing.T) {
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
	kf := keyframeMsg(t, 20, "", "kf")
	good := chunkDgram(t, false, 21, 0, 1, "ok")
	stream := keyframeCarrierBytes(t, kf, good)
	stream = append(stream, 0, 0) // a zero-length record
	stream = append(stream, keyframeCarrierBytes(t, nil, chunkDgram(t, false, 22, 0, 1, "after"))[wire.CarrierPrologueSize:]...)
	if err := p.IngestKeyframeCarrier(bytes.NewReader(stream)); !errors.Is(err, ErrBadUplinkRecord) {
		t.Fatalf("err = %v, want ErrBadUplinkRecord", err)
	}
	waitKeyframes(t, f, 1)
	s.Close()
	wantDatagrams(t, f, [][]byte{good})
	if !bytes.Equal(cachedKeyframe(t, r, id), kf) {
		t.Fatal("the keyframe before the bad record was not kept")
	}
}
