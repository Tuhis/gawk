// Package opus reads the one byte of an Opus packet that describes the packet:
// its TOC (RFC 6716 §3.1).
//
// Two callers, two questions, one table. internal/mpegts needs the packet
// *duration*, because only a PES carries a PTS and a PES may carry several
// access units — the timestamps for the second onward have to be derived
// (docs/28 NA1 finding 2). internal/engine needs the *channel count and frame
// duration* to check the AudioConfig it is about to advertise against what the
// encoder actually produced (docs/28 Decision 10) — the audio analogue of
// parsing the codec string out of the SPS rather than assuming it.
//
// It is a package rather than a helper in either of them because both need it
// and neither should import the other. Nothing here decodes audio; this is a
// header read, and it deliberately stops at the TOC.
package opus

// TOC is what the first byte (plus, for code 3, the second) of an Opus packet
// says about the packet.
type TOC struct {
	// Stereo is the TOC's stereo flag: two channels when set, one when not.
	Stereo bool
	// Frames is how many Opus frames the packet carries (1, 2, or 1-48 for
	// the arbitrary-count code).
	Frames int
	// FrameDurationUs is one frame's duration in microseconds. 2500 is a
	// legal value, which is why this is not milliseconds.
	FrameDurationUs uint64
	// DurationUs is the whole packet's duration: Frames × FrameDurationUs.
	DurationUs uint64
}

// Channels reports the channel count the TOC declares.
func (t TOC) Channels() int {
	if t.Stereo {
		return 2
	}
	return 1
}

// silkFrameUs, hybridFrameUs and celtFrameUs are RFC 6716 Table 2's frame
// sizes, indexed by the low bits of the config number. The mode is the config
// number's range: 0-11 SILK, 12-15 Hybrid, 16-31 CELT.
var (
	silkFrameUs   = [4]uint64{10_000, 20_000, 40_000, 60_000}
	hybridFrameUs = [2]uint64{10_000, 20_000}
	celtFrameUs   = [4]uint64{2_500, 5_000, 10_000, 20_000}
)

// ParseTOC reads a packet's table of contents. It returns false for an empty
// packet, or for a code-3 packet whose frame-count byte is missing or declares
// zero frames — a caller must treat that as an unusable packet rather than
// guess, because every timestamp behind it would inherit the guess.
func ParseTOC(pkt []byte) (TOC, bool) {
	if len(pkt) == 0 {
		return TOC{}, false
	}
	b := pkt[0]
	config := int(b >> 3)
	t := TOC{Stereo: b&0x04 != 0}

	switch {
	case config < 12:
		t.FrameDurationUs = silkFrameUs[config&0x03]
	case config < 16:
		t.FrameDurationUs = hybridFrameUs[config&0x01]
	default:
		t.FrameDurationUs = celtFrameUs[config&0x03]
	}

	switch b & 0x03 {
	case 0:
		t.Frames = 1
	case 1, 2:
		t.Frames = 2
	default:
		// Code 3: an arbitrary number of frames, counted in the next byte's
		// low six bits.
		if len(pkt) < 2 {
			return TOC{}, false
		}
		t.Frames = int(pkt[1] & 0x3f)
		if t.Frames == 0 {
			return TOC{}, false
		}
	}
	t.DurationUs = uint64(t.Frames) * t.FrameDurationUs
	return t, true
}
