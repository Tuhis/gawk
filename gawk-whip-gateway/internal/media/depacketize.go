// Package media turns OBS's RTP into what the publisher engine takes: whole
// Annex-B H.264 access units and Opus packets on the engine's clock (docs/83
// D6, D7, WG6).
//
// Everything here is pure and deterministic: no goroutines, no pion, no
// wall clock. The session loop feeds packets with the time they arrived and
// asks for the next deadline, so every rule (the reorder window, the drop
// until the next IDR, the GOP bounds, the A/V alignment) is a unit test.
package media

import "errors"

// H.264 NAL unit types (RFC 6184 §5.2 for the RTP-only ones).
const (
	nalSlice  = 1
	nalIDR    = 5
	nalSEI    = 6
	nalSPS    = 7
	nalPPS    = 8
	nalAUD    = 9
	nalSTAPA  = 24
	nalSTAPB  = 25
	nalMTAP16 = 26
	nalMTAP24 = 27
	nalFUA    = 28
	nalFUB    = 29
)

// errMalformed is a payload that isn't valid packetization-mode 1 H.264.
var errMalformed = errors.New("media: malformed H.264 RTP payload")

// PacketCarriesIDR reports whether an RTP payload carries any part of an IDR
// slice: a single NAL of type 5, a STAP-A holding one, or any FU-A fragment
// of one (every fragment's FU header repeats the type). D7 records IDR starts
// from this, on arrival and before D6 can drop the frame, so a lost first
// packet of a keyframe doesn't make the GOP look longer.
func PacketCarriesIDR(p []byte) bool {
	if len(p) < 1 {
		return false
	}
	switch t := p[0] & 0x1f; t {
	case nalIDR:
		return true
	case nalFUA:
		return len(p) >= 2 && p[1]&0x1f == nalIDR
	case nalSTAPA:
		found := false
		_ = walkSTAPA(p, func(n []byte) {
			if n[0]&0x1f == nalIDR {
				found = true
			}
		})
		return found
	}
	return false
}

// PacketStartsFrame reports whether an RTP payload can be the first packet of
// an access unit: one starting with a NAL that only appears at the start of
// one (AUD, SEI, SPS, PPS), or with a slice whose first_mb_in_slice is 0 (the
// slice's first RBSP bit is 1, ue(0)). D6 uses it after a drop, where the
// packet that ended the previous frame was lost and the boundary is unknown:
// a frame whose first packet isn't a start is missing its head.
func PacketStartsFrame(p []byte) bool {
	if len(p) < 2 {
		return false
	}
	switch t := p[0] & 0x1f; t {
	case nalAUD, nalSEI, nalSPS, nalPPS:
		return true
	case nalSlice, nalIDR:
		return p[1]&0x80 != 0
	case nalFUA:
		if p[1]&0x80 == 0 { // not the S fragment
			return false
		}
		switch p[1] & 0x1f {
		case nalAUD, nalSEI, nalSPS, nalPPS:
			return true
		case nalSlice, nalIDR:
			return len(p) >= 3 && p[2]&0x80 != 0
		}
		return false
	case nalSTAPA:
		first := true
		ok := false
		_ = walkSTAPA(p, func(n []byte) {
			if first {
				first = false
				ok = PacketStartsFrame(n)
			}
		})
		return ok
	}
	return false
}

// walkSTAPA calls fn for each aggregated NAL of a STAP-A payload.
func walkSTAPA(p []byte, fn func([]byte)) error {
	i := 1
	n := 0
	for i < len(p) {
		if i+2 > len(p) {
			return errMalformed
		}
		size := int(p[i])<<8 | int(p[i+1])
		i += 2
		if size == 0 || i+size > len(p) {
			return errMalformed
		}
		fn(p[i : i+size])
		i += size
		n++
	}
	if n == 0 {
		return errMalformed
	}
	return nil
}

// depacketize rebuilds the NAL units of one frame from its RTP payloads, in
// sequence order. FU-A fragments must run start…end without interruption;
// STAP-B, MTAP and FU-B belong to the interleaved mode the SDP answer never
// accepts, so they are malformed here.
func depacketize(payloads [][]byte) ([][]byte, error) {
	var nals [][]byte
	var fu []byte // the NAL being rebuilt from FU-A fragments, nil when none
	for _, p := range payloads {
		if len(p) == 0 {
			// A padding-only packet: it holds a sequence number, no NAL.
			continue
		}
		if p[0]&0x80 != 0 { // forbidden_zero_bit
			return nil, errMalformed
		}
		t := p[0] & 0x1f
		if t != nalFUA && fu != nil {
			return nil, errMalformed // an FU-A interrupted before its end
		}
		switch {
		case t >= 1 && t <= 23:
			nals = append(nals, p)
		case t == nalSTAPA:
			if err := walkSTAPA(p, func(n []byte) { nals = append(nals, n) }); err != nil {
				return nil, err
			}
		case t == nalFUA:
			if len(p) < 3 {
				return nil, errMalformed
			}
			start, end := p[1]&0x80 != 0, p[1]&0x40 != 0
			switch {
			case start && fu != nil, !start && fu == nil:
				return nil, errMalformed
			case start:
				fu = append([]byte{p[0]&0xe0 | p[1]&0x1f}, p[2:]...)
			default:
				fu = append(fu, p[2:]...)
			}
			if end {
				nals = append(nals, fu)
				fu = nil
			}
		default: // 0, STAP-B, MTAP16/24, FU-B, 30, 31
			return nil, errMalformed
		}
	}
	if fu != nil {
		return nil, errMalformed // the frame ended inside an FU-A
	}
	for _, n := range nals {
		if len(n) < 1 || n[0]&0x80 != 0 {
			return nil, errMalformed
		}
	}
	return nals, nil
}

// annexB joins NAL units with 4-byte start codes.
func annexB(nals [][]byte) []byte {
	size := 0
	for _, n := range nals {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	for _, n := range nals {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}
