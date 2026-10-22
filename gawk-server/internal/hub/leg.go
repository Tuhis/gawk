package hub

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The R74 leg writer (docs/76 item 18; PP8, PP9). A PoP leg (the uplink's
// /pop/publish, or home's /pop/subscribe) is an internal subscriber, so it is
// exempt from the viewer caps, out of the local viewer count and served every
// parity symbol. With Carrier set its deltas and parity ride R73 uplink
// carriers (docs/75 D1), read at the far end by Publisher.IngestUplinkCarrier,
// under R73's sender rules (docs/75 D2, D3):
//
//   - only VideoChunk and ParityChunk go on a carrier: R73's reader ends a
//     carrier on any other record, so a ViewerCount from the count pump, a
//     live DecoderConfig, audio and the clock go as datagrams;
//   - at most two carriers are live: the current GOP's and one finishing.
//     The superseded one is finished (FIN) at rotation and reset ResetDelay
//     later, and if a third would open, the oldest is reset first;
//   - no reset within a GOP: records carry no write deadline, so a slow write
//     waits, and only the next keyframe ends it;
//   - a write still blocked on the older GOP's carrier is abandoned when the
//     next keyframe fans out (its deadline set to now) and that carrier is
//     reset at once (F10);
//   - nothing queued for an older GOP is written once a newer keyframe has
//     gone, and a GOP superseded before its carrier opened never gets one.
//
// The viewer reliable drain (drainReliable) is untouched: its rules (a
// 500 ms record deadline, close-only retirement) are right for a viewer.
//
// quic-go gives no signal that a finished stream was fully acknowledged, so
// the reset after ResetDelay is a plain timer (docs/76 item 18).

// LegResetDelay is how long a superseded leg carrier may finish before it is
// reset: long enough for its tail to land, short enough that it never holds
// one of the far end's MaxUplinkCarriers slots into the next rotation.
const LegResetDelay = 150 * time.Millisecond

// LegOptions configures SubscribeLeg.
type LegOptions struct {
	// Carrier selects carrier delivery for deltas and parity. False is a
	// datagram leg, byte-identical to SubscribeInternal (home's
	// -uplink-carriers=false, or -pop-leg-delivery=datagram).
	Carrier bool
	// ResetDelay is how long a finished carrier lives before its reset.
	// Zero means LegResetDelay.
	ResetDelay time.Duration
	// Now and AfterFunc are the clock, replaced in tests. Nil means the
	// time package's.
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) (stop func() bool)
}

func (o LegOptions) withDefaults() LegOptions {
	if o.ResetDelay <= 0 {
		o.ResetDelay = LegResetDelay
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	return o
}

// SubscribeLeg registers a PoP leg on broadcast id (docs/76 item 18): an
// internal subscriber (as SubscribeInternal: exempt from the caps, out of the
// viewer count, a downstream count it reports honoured, all parity served,
// never an audio lane) whose keyframe streams never evict it for being slow,
// since a leg crosses a WAN. With o.Carrier its deltas and parity are written
// as carrier records under the rules above; without, it is a datagram leg.
func (r *Registry) SubscribeLeg(id string, conn Conn, o LegOptions) (*Subscriber, error) {
	so := subscribeOpts{internal: true, leg: true}
	if o.Carrier {
		so.reliable = true
		so.legCarrier = true
		so.legOpts = o.withDefaults()
	}
	return r.subscribeOpts(id, conn, so)
}

// legKfValid marks legKfID as holding a keyframe's frameID.
const legKfValid = uint64(1) << 32

// legNoteKeyframe is the carrier leg's half of sendKeyframe, called under
// kfMu for an accepted keyframe: it records the keyframe's frameID, rotates
// the carrier and, when the drain is inside a write on the old carrier,
// abandons that write by moving its deadline to now. The three happen under
// carMu, where the drain also decides to start a write, so a write is either
// already in flight (and abandoned here) or sees the rotation and is never
// started on the old carrier.
func (s *Subscriber) legNoteKeyframe(msg []byte) {
	kf := uint64(0)
	if hdr, err := wire.ParseStreamFrameHeader(msg); err == nil {
		kf = legKfValid | uint64(hdr.FrameID)
	}
	s.carMu.Lock()
	s.legKfID.Store(kf)
	s.carRotations.Add(1)
	if s.legWriting && s.carCurrent != nil {
		// Any time in the past ends a blocked quic-go Write at once.
		_ = s.carCurrent.SetWriteDeadline(time.Now())
	}
	s.carMu.Unlock()
}

// legStale reports whether a record's frame serially precedes the newest
// keyframe fanned out to this leg: the far end would discard it
// (admitUplinkRecord), and writing it holds up the current GOP.
func (s *Subscriber) legStale(frameID uint32) bool {
	kf := s.legKfID.Load()
	if kf&legKfValid == 0 {
		return false
	}
	return int32(frameID-uint32(kf)) < 0
}

type legResult uint8

const (
	legOK legResult = iota
	// legAbandoned: a rotation overtook the write. The record is re-judged
	// against the new GOP.
	legAbandoned
	// legFailed: the carrier is gone (a peer reset, a dead session, Close).
	// The rest of the GOP is dropped.
	legFailed
)

// drainLeg is the carrier leg's drain.
func (s *Subscriber) drainLeg() {
	defer s.legCancelAll()
	var carSeen uint64
	carDead := false
	var scratch []byte
	for dgram := range s.queue {
		if !isCarrierMediaDatagram(dgram) {
			s.sendSidebandDatagram(dgram)
			continue
		}
		frameID, err := uplinkRecordFrameID(dgram)
		if err != nil {
			s.carrierRecordsDropped.Add(1)
			continue
		}
		// At most two passes: a write abandoned by a rotation is retried once
		// against the new GOP, where it is either stale (an old GOP's delta)
		// or the new GOP's first delta, which raced its keyframe.
		for range 2 {
			if rot := s.carRotations.Load(); rot != carSeen {
				s.legRetire()
				carSeen = rot
				carDead = false
			}
			if carDead || s.closed.Load() || s.legStale(frameID) {
				s.carrierRecordsDropped.Add(1)
				break
			}
			scratch, err = wire.AppendCarrierRecord(scratch[:0], dgram)
			if err != nil {
				s.carrierRecordsDropped.Add(1)
				break
			}
			needsOpen := s.currentCarrier() == nil
			n := len(scratch)
			if needsOpen {
				n += wire.CarrierPrologueSize
			}
			if !s.hub.registry.consumeBandwidth(n) {
				s.dropped.Add(1)
				s.hub.countBandwidthDrop(n)
				break
			}
			res := legOK
			if needsOpen {
				res = s.legOpenCarrier(carSeen)
			}
			if res == legOK {
				res = s.legWrite(scratch, carSeen)
			}
			if res == legAbandoned {
				continue
			}
			if res == legFailed {
				carDead = true
				s.carrierRecordsDropped.Add(1)
				break
			}
			s.carrierRecords.Add(1)
			s.egressCarrierBytes.Add(uint64(len(scratch)))
			s.noteCarrierParity(dgram, len(scratch))
			break
		}
	}
}

// legOpenCarrier opens the current GOP's carrier and writes its prologue.
// Open failures feed the same eviction streak as a viewer's carrier opens.
func (s *Subscriber) legOpenCarrier(carSeen uint64) legResult {
	st, err := s.sender.OpenCarrierStream()
	if err != nil {
		s.noteCarrierOpenFailed()
		return legFailed
	}
	s.noteCarrierOpenSucceeded()
	s.carMu.Lock()
	if s.closed.Load() {
		s.carMu.Unlock()
		st.CancelWrite()
		return legFailed
	}
	s.carCurrent = st
	s.carMu.Unlock()
	s.carrierStreams.Add(1)
	// No deadline on a leg's records: only a rotation ends a write.
	_ = st.SetWriteDeadline(time.Time{})

	res := s.legWrite(wire.AppendCarrierPrologue(nil), carSeen)
	if res != legOK {
		// A carrier without its prologue is no carrier: reset, never finish.
		s.carMu.Lock()
		if s.carCurrent == st {
			s.carCurrent = nil
		}
		s.carMu.Unlock()
		st.CancelWrite()
		return res
	}
	s.egressCarrierBytes.Add(wire.CarrierPrologueSize)
	return legOK
}

// legWrite writes buf to the current carrier, with no deadline. A rotation
// that lands first means nothing is written (legAbandoned). One that lands
// during the write abandons it, and the carrier, holding a half-written
// record, is reset at once (F10).
func (s *Subscriber) legWrite(buf []byte, carSeen uint64) legResult {
	s.carMu.Lock()
	st := s.carCurrent
	if st == nil {
		s.carMu.Unlock()
		return legFailed // reset by Close
	}
	if s.carRotations.Load() != carSeen {
		s.carMu.Unlock()
		return legAbandoned
	}
	s.legWriting = true
	s.carMu.Unlock()

	_, err := st.Write(buf)

	s.carMu.Lock()
	s.legWriting = false
	abandoned := s.carRotations.Load() != carSeen
	if err != nil && s.carCurrent == st {
		s.carCurrent = nil
	}
	s.carMu.Unlock()
	if err == nil {
		return legOK
	}
	st.CancelWrite()
	if abandoned {
		return legAbandoned
	}
	return legFailed
}

// legRetire ends the current carrier at a rotation: it is finished (FIN) and
// reset ResetDelay later. A carrier still finishing from the rotation before
// is reset first, so no more than two are ever live.
func (s *Subscriber) legRetire() {
	s.carMu.Lock()
	old := s.carCurrent
	s.carCurrent = nil
	var prev KeyframeStream
	var prevStop func() bool
	if old != nil && s.legFinishing != nil {
		prev, prevStop = s.legFinishing, s.legFinishStop
		s.legFinishing, s.legFinishStop = nil, nil
	}
	s.carMu.Unlock()
	if prev != nil {
		prevStop()
		prev.CancelWrite()
	}
	if old == nil {
		return
	}
	if err := old.Close(); err != nil {
		old.CancelWrite()
		return
	}
	s.carMu.Lock()
	defer s.carMu.Unlock()
	if s.closed.Load() {
		old.CancelWrite()
		return
	}
	s.legFinishing = old
	s.legFinishStop = s.legOpts.AfterFunc(s.legOpts.ResetDelay, func() { s.legResetFinished(old) })
}

// legResetFinished is the ResetDelay timer: it resets st if st is still the
// finishing carrier (a later rotation or Close may have reset it already).
func (s *Subscriber) legResetFinished(st KeyframeStream) {
	s.carMu.Lock()
	if s.legFinishing != st {
		s.carMu.Unlock()
		return
	}
	s.legFinishing, s.legFinishStop = nil, nil
	s.carMu.Unlock()
	st.CancelWrite()
}

// legCancelFinishingLocked resets the finishing carrier and stops its timer.
// Caller holds carMu. A no-op on every subscriber but a carrier leg.
func (s *Subscriber) legCancelFinishingLocked() {
	if s.legFinishing == nil {
		return
	}
	if s.legFinishStop != nil {
		s.legFinishStop()
	}
	s.legFinishing.CancelWrite()
	s.legFinishing, s.legFinishStop = nil, nil
}

// legCancelAll resets both carriers when the drain ends.
func (s *Subscriber) legCancelAll() {
	s.carMu.Lock()
	defer s.carMu.Unlock()
	if s.carCurrent != nil {
		s.carCurrent.CancelWrite()
		s.carCurrent = nil
	}
	s.legCancelFinishingLocked()
}
