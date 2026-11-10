package hub

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// UplinkSenderCarriers is the most uplink carriers a broadcaster keeps live
// at once (R73, docs/75 D2): the current GOP's and one still draining. The
// broadcaster resets the oldest before it opens a third.
const UplinkSenderCarriers = 2

// uplinkResetSlack is room for a carrier the broadcaster has already reset
// but whose RESET_STREAM has not been processed here yet. The reset and the
// next carrier's first STREAM frame travel independently, and this side frees
// a slot only when the old carrier's reader sees the reset, so without the
// slack a conforming sender's current GOP could be refused (review of #541).
const uplinkResetSlack = 1

// MaxUplinkCarriers bounds a publisher's concurrently open uplink carriers:
// the sender's limit plus one for a reset in flight. One more is refused
// (ErrUplinkCarrierLimit) and counted; the transport resets it. The memory a
// publisher can hold behind its carriers stays bounded by the connection
// receive window, however many there are.
const MaxUplinkCarriers = UplinkSenderCarriers + uplinkResetSlack

// uplinkCarrierReadBuffer sizes the carrier reader's buffer: a dozen records,
// so a GOP's records are not read two bytes at a time from the stream. It is
// application memory on top of what QUIC holds behind a hole, and bounded.
const uplinkCarrierReadBuffer = 16 << 10

var (
	// ErrUplinkCarrierLimit refuses a carrier beyond MaxUplinkCarriers.
	ErrUplinkCarrierLimit = errors.New("hub: too many concurrent uplink carriers")
	// ErrBadUplinkRecord ends a carrier whose record is not a VideoChunk or
	// ParityChunk datagram (docs/75 D1); the records before it were forwarded.
	ErrBadUplinkRecord = errors.New("hub: invalid uplink carrier record")
	// ErrBadKeyframeCarrier ends a keyframe carrier whose head is not a
	// keyframe StreamFrame (docs/88 D2); nothing of it was cached.
	ErrBadKeyframeCarrier = errors.New("hub: invalid keyframe carrier")
)

// keyframeCarrierReadStep is how far the keyframe part of a keyframe carrier
// grows its buffer ahead of the bytes actually read. The StreamFrame's length
// comes from the publisher; allocating it whole up front would let a stream
// that declares MaxKeyframeBytes and then holds a hole pin that much per
// carrier, past the connection receive window (docs/88 BR1).
const keyframeCarrierReadStep = 64 << 10

// IngestUplinkCarrier reads one uplink carrier from a publisher-initiated
// unidirectional stream (R73, docs/75 D1/D2): the R19 prologue
// Version‖TypeReliableCarrier, then records of uint16 length (BE) ‖ datagram,
// until the stream ends. Each record is byte for byte the VideoChunk or
// ParityChunk datagram it replaces and is ingested through HandleDatagram, so
// accounting, the ingress window, fan-out and the DVR see exactly what a
// datagram would have produced.
//
// A record whose frameID serially precedes the newest keyframe this session
// has ingested is dropped and counted (uplinkRecordsStale): viewers jump to
// that keyframe and discard the delta anyway. A malformed record ends the
// carrier with ErrBadUplinkRecord; the records before it were already
// forwarded. A clean end of stream at a record boundary returns nil; any other
// error is returned and the caller resets the stream.
func (p *Publisher) IngestUplinkCarrier(stream io.Reader) error {
	if !p.acquireUplinkCarrier() {
		return ErrUplinkCarrierLimit
	}
	defer p.releaseUplinkCarrier()
	p.NoteSeen()

	rd := bufio.NewReaderSize(stream, uplinkCarrierReadBuffer)
	var prologue [wire.CarrierPrologueSize]byte
	if _, err := io.ReadFull(rd, prologue[:]); err != nil {
		return err
	}
	if err := wire.ParseCarrierPrologue(prologue[:]); err != nil {
		p.hub.countBad()
		return err
	}
	return p.ingestUplinkRecords(rd)
}

// IngestKeyframeCarrier reads one keyframe carrier from a publisher-initiated
// unidirectional stream (R84, docs/88 D2, K3): the prologue
// Version‖TypeKeyframeCarrier, one StreamFrame message exactly as a keyframe
// stream carries it, then RU1's records until the stream ends.
//
// The StreamFrame goes to the keyframe path (cache, priming, DVR, R8 fan-out)
// as soon as its declared length has been read, not at the FIN: the stream
// lives for the GOP. Its records are then ingested as IngestUplinkCarrier's.
// The stream holds one of the publisher's MaxUplinkCarriers slots for its
// whole life, never a keyframe-stream slot (the transport gives that back
// once it has read the prologue).
//
// A head that is not a keyframe StreamFrame (another type, a bad version, the
// keyframe flag clear) ends the stream, counted as bad input; one over
// MaxKeyframeBytes ends it counted as oversize. Either way nothing is cached.
// A stream cut short before its keyframe is complete (the sender's 2 s abort)
// returns the read error, cached nothing and is not counted as bad. The
// keyframe's buffer grows with what has been read, so a publisher holding a
// hole inside it pins no more than its receive window (docs/88 BR1).
func (p *Publisher) IngestKeyframeCarrier(stream io.Reader) error {
	if !p.acquireUplinkCarrier() {
		return ErrUplinkCarrierLimit
	}
	defer p.releaseUplinkCarrier()
	p.NoteSeen()
	b := p.hub

	rd := bufio.NewReaderSize(stream, uplinkCarrierReadBuffer)
	var prologue [wire.CarrierPrologueSize]byte
	if _, err := io.ReadFull(rd, prologue[:]); err != nil {
		return err
	}
	if err := wire.ParseKeyframeCarrierPrologue(prologue[:]); err != nil {
		b.countBad()
		return err
	}

	header := make([]byte, wire.StreamFrameHeaderSize)
	if _, err := io.ReadFull(rd, header); err != nil {
		return err
	}
	hdr, err := wire.ParseStreamFrameHeader(header)
	if err != nil {
		if errors.Is(err, wire.ErrKeyframeTooLarge) {
			b.countKeyframeOversize()
		} else {
			b.countBad()
		}
		return fmt.Errorf("%w: %v", ErrBadKeyframeCarrier, err)
	}
	if !hdr.Keyframe {
		b.countBad()
		return fmt.Errorf("%w: keyframe flag clear on frame %d", ErrBadKeyframeCarrier, hdr.FrameID)
	}
	total := wire.StreamFrameHeaderSize + int(hdr.ConfigLen) + int(hdr.PayloadLen)
	if total > b.registry.opts.MaxKeyframeBytes {
		b.countKeyframeOversize()
		return fmt.Errorf("%w: keyframe %d bytes exceeds MaxKeyframeBytes %d", ErrBadKeyframeCarrier, total, b.registry.opts.MaxKeyframeBytes)
	}
	msg, err := readGrowing(rd, header, total)
	if err != nil {
		return err
	}
	p.onKeyframe(msg, hdr)
	return p.ingestUplinkRecords(rd)
}

// readGrowing returns a slice of exactly total bytes: head, then what rd
// yields after it. Its buffer grows by at most keyframeCarrierReadStep (or
// doubling) ahead of the bytes read, so memory follows the data that arrived
// rather than the length a publisher declared. The result's capacity is
// exactly total: it is retained as the cached keyframe.
func readGrowing(rd io.Reader, head []byte, total int) ([]byte, error) {
	buf := make([]byte, len(head), min(total, len(head)+keyframeCarrierReadStep))
	copy(buf, head)
	for len(buf) < total {
		if len(buf) == cap(buf) {
			grown := make([]byte, len(buf), min(total, max(2*len(buf), len(buf)+keyframeCarrierReadStep)))
			copy(grown, buf)
			buf = grown
		}
		n, err := io.ReadFull(rd, buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return buf, nil
}

// ingestUplinkRecords reads RU1's records (uint16 length ‖ datagram) from rd
// until the stream ends, ingesting each through HandleDatagram.
func (p *Publisher) ingestUplinkRecords(rd *bufio.Reader) error {
	b := p.hub
	var lenBuf [wire.CarrierRecordHeaderSize]byte
	for {
		if _, err := io.ReadFull(rd, lenBuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // FIN at a record boundary: the carrier is done
			}
			return err
		}
		n := int(binary.BigEndian.Uint16(lenBuf[:]))
		if n == 0 || n > wire.MaxDatagramSize {
			b.countBad()
			return fmt.Errorf("%w: declared %d bytes, want 1-%d", ErrBadUplinkRecord, n, wire.MaxDatagramSize)
		}
		// A fresh slice per record: fan-out queues and the DVR ring retain it.
		rec := make([]byte, n)
		if _, err := io.ReadFull(rd, rec); err != nil {
			return err
		}
		frameID, err := uplinkRecordFrameID(rec)
		if err != nil {
			b.countBad()
			return err
		}
		if !p.admitUplinkRecord(frameID) {
			continue
		}
		p.HandleDatagram(rec)
	}
}

// uplinkRecordFrameID validates a record exactly as HandleDatagram validates
// the same datagram, restricted to the two types a carrier may carry, and
// returns its frameID.
func uplinkRecordFrameID(rec []byte) (uint32, error) {
	ver, typ, err := wire.PeekType(rec)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBadUplinkRecord, err)
	}
	if ver != wire.Version {
		return 0, fmt.Errorf("%w: version 0x%02x", ErrBadUplinkRecord, ver)
	}
	switch typ {
	case wire.TypeVideoChunk:
		hdr, _, err := wire.ParseVideoChunk(rec)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrBadUplinkRecord, err)
		}
		return hdr.FrameID, nil
	case wire.TypeParityChunk:
		hdr, _, err := wire.ParseParityChunk(rec)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrBadUplinkRecord, err)
		}
		return hdr.FrameID, nil
	default:
		return 0, fmt.Errorf("%w: type 0x%02x", ErrBadUplinkRecord, typ)
	}
}

// acquireUplinkCarrier claims one of the publisher's MaxUplinkCarriers slots,
// counting the carrier as opened, or as rejected when none is free.
func (p *Publisher) acquireUplinkCarrier() bool {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.uplinkCarriersOpen >= MaxUplinkCarriers {
		b.uplinkCarriersRejected++
		return false
	}
	p.uplinkCarriersOpen++
	b.uplinkCarriers++
	return true
}

func (p *Publisher) releaseUplinkCarrier() {
	r := p.hub.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	p.uplinkCarriersOpen--
}

// admitUplinkRecord counts one well-formed record and reports whether it is
// still worth forwarding: false when its frame serially precedes the newest
// keyframe this publisher session has ingested (the cache is cleared on every
// claim, so an earlier session's keyframe never makes a record stale).
func (p *Publisher) admitUplinkRecord(frameID uint32) bool {
	b := p.hub
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.closed {
		return false
	}
	if b.cachedKeyframe != nil && int32(frameID-b.cachedKeyframeID) < 0 {
		b.uplinkRecordsStale++
		return false
	}
	b.uplinkRecords++
	return true
}
