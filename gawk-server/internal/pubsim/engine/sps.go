package engine

import "fmt"

// H.264 NAL unit types we care about.
const (
	nalTypeNonIDR = 1
	nalTypeIDR    = 5
	nalTypeSPS    = 7
)

// ParseCodecString derives the WebCodecs codec string ("avc1.42E02A") from the
// first SPS in an Annex-B access unit.
//
// Parsed from the bitstream, never assumed (docs/19 Decision 8): the encoder
// may pick a different level than we asked for — level is a function of
// resolution, framerate and bitrate, and hardware encoders round it up — and
// on the Annex-B path this string is the *only* thing telling the viewer's
// decoder what it is about to get. The viewer's extradata-derived correction
// runs only for AVCC, which this path deliberately never produces.
//
// The three bytes are profile_idc, constraint_flags+reserved, level_idc, and
// they sit immediately after the 1-byte NAL header, before any position where
// an emulation-prevention byte can legally occur (an EPB requires two
// preceding zero bytes, and the earliest it can appear is the fourth byte of
// the RBSP). So reading them raw — without unescaping — is safe here, and only
// here; anything deeper into the SPS would need real EPB removal.
func ParseCodecString(au []byte) (string, bool) {
	for nal := range annexBNALs(au) {
		if len(nal) < 4 || nal[0]&0x1f != nalTypeSPS {
			continue
		}
		return fmt.Sprintf("avc1.%02X%02X%02X", nal[1], nal[2], nal[3]), true
	}
	return "", false
}

// HasIDR reports whether an access unit contains an IDR slice — the engine's
// definition of a keyframe (docs/19 Decision 7), and what routes it onto a
// reliable uni stream instead of datagrams.
func HasIDR(au []byte) bool {
	for nal := range annexBNALs(au) {
		if len(nal) > 0 && nal[0]&0x1f == nalTypeIDR {
			return true
		}
	}
	return false
}

// HasBSlices reports whether an access unit contains any B slice.
//
// This exists to be asserted, not consulted: no-B-frames is a hard encoder
// invariant (docs/19 Decision 13), because the entire viewer pipeline assumes
// decode order == presentation order — frameId ordering, the reorder buffer,
// and every piece of live-edge and pacing math. A B-frame would not fail
// loudly; it would produce subtly wrong playback. So the cascade pins
// b-frames=0 per candidate, and the fixture test proves the assertion can
// actually detect a violation.
func HasBSlices(au []byte) bool {
	for nal := range annexBNALs(au) {
		if len(nal) < 2 {
			continue
		}
		switch nal[0] & 0x1f {
		case nalTypeNonIDR, nalTypeIDR:
			if t, ok := sliceType(nal); ok && t%5 == 1 {
				return true
			}
		}
	}
	return false
}

// sliceType reads slice_type from a slice NAL's header. The header begins with
// first_mb_in_slice (ue), then slice_type (ue) — so two exp-Golomb reads. The
// values wrap at 5 (0..4 and 5..9 mean the same types), hence the %5 above:
// 0=P 1=B 2=I 3=SP 4=SI.
func sliceType(nal []byte) (uint32, bool) {
	br := &bitReader{buf: unescapeRBSP(nal[1:])}
	if _, ok := br.ue(); !ok { // first_mb_in_slice
		return 0, false
	}
	return br.ue()
}

// unescapeRBSP removes emulation-prevention bytes: the encoder inserts a 0x03
// after any 00 00 that would otherwise look like a start code, and a bit
// reader must not see it. ParseCodecString can skip this because the three
// bytes it reads sit before any position an EPB can legally occupy; a slice
// header cannot.
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 0x03 {
			zeros = 0
			continue // the emulation-prevention byte itself
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// bitReader reads unsigned exp-Golomb values, MSB first.
type bitReader struct {
	buf []byte
	pos int // bit position
}

func (r *bitReader) bit() (uint32, bool) {
	if r.pos >= len(r.buf)*8 {
		return 0, false
	}
	b := (r.buf[r.pos/8] >> (7 - uint(r.pos%8))) & 1
	r.pos++
	return uint32(b), true
}

// ue reads one ue(v): count leading zeros, then read that many bits.
func (r *bitReader) ue() (uint32, bool) {
	zeros := 0
	for {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, false // malformed; refuse rather than loop
		}
	}
	var v uint32
	for i := 0; i < zeros; i++ {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return v + (1 << uint(zeros)) - 1, true
}

// annexBNALs iterates the NAL units in an Annex-B buffer, yielding each NAL's
// bytes without its start code. It accepts both 3- and 4-byte start codes,
// which encoders mix freely within one AU.
func annexBNALs(buf []byte) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		start := -1
		i := 0
		for i+2 < len(buf) {
			if buf[i] != 0 || buf[i+1] != 0 {
				i++
				continue
			}
			scLen := 0
			switch {
			case buf[i+2] == 1:
				scLen = 3
			case i+3 < len(buf) && buf[i+2] == 0 && buf[i+3] == 1:
				scLen = 4
			default:
				i++
				continue
			}
			if start >= 0 && !yield(trimTrailingZeros(buf[start:i])) {
				return
			}
			i += scLen
			start = i
		}
		if start >= 0 && start < len(buf) {
			yield(trimTrailingZeros(buf[start:]))
		}
	}
}

// trimTrailingZeros drops trailing_zero_8bits some encoders pad NALs with;
// they are not part of the NAL and would confuse a length-sensitive reader.
func trimTrailingZeros(nal []byte) []byte {
	for len(nal) > 0 && nal[len(nal)-1] == 0 {
		nal = nal[:len(nal)-1]
	}
	return nal
}
