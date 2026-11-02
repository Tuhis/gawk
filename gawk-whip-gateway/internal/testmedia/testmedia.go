// Package testmedia generates synthetic H.264 access units and their RTP
// packets for the gateway's tests.
//
// No pixel is encoded: the gateway never decodes, it only parses NAL headers,
// SPS fields and slice headers, so syntactically valid parameter sets and
// slice headers followed by filler are a faithful input. Generating them
// means the tests carry no copied fixture and no third-party bitstream
// (gawk-server's internal/pubsim fixture is internal and an x264 encode; the
// gateway needs neither). Imported by tests only.
//
// WG0 has not recorded real OBS RTP yet; the packetization here follows
// libdatachannel's (OBS's) shape as read from source: SPS and PPS in a STAP-A
// (or as single NALs) ahead of the IDR, FU-A for every NAL over the MTU,
// the marker on the last packet of each access unit, 90 kHz timestamps.
package testmedia

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// bitWriter writes MSB-first bits and exp-Golomb codes.
type bitWriter struct {
	buf  []byte
	nbit int
}

func (w *bitWriter) bit(b uint32) {
	if w.nbit%8 == 0 {
		w.buf = append(w.buf, 0)
	}
	if b != 0 {
		w.buf[len(w.buf)-1] |= 1 << (7 - uint(w.nbit%8))
	}
	w.nbit++
}

func (w *bitWriter) bits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(v >> uint(i) & 1)
	}
}

func (w *bitWriter) ue(v uint32) {
	v++
	n := 0
	for t := v; t > 1; t >>= 1 {
		n++
	}
	w.bits(0, n)
	w.bits(v, n+1)
}

// trailing writes rbsp_stop_one_bit and aligns.
func (w *bitWriter) trailing() {
	w.bit(1)
	for w.nbit%8 != 0 {
		w.bit(0)
	}
}

// escape inserts emulation-prevention bytes.
func escape(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp)+8)
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// SPSOptions shape the generated SPS.
type SPSOptions struct {
	Profile, Level uint8
	Width, Height  int // multiples of 2; cropped from macroblock multiples
}

// SPS builds an SPS NAL (header included). High profile writes the
// chroma/bit-depth fields too, so the parser's High branch is exercised.
func SPS(o SPSOptions) []byte {
	if o.Profile == 0 {
		o.Profile = 100
	}
	if o.Level == 0 {
		o.Level = 42
	}
	if o.Width == 0 {
		o.Width, o.Height = 1920, 1080
	}
	w := &bitWriter{}
	w.ue(0) // seq_parameter_set_id
	if o.Profile == 100 {
		w.ue(1)  // chroma_format_idc 4:2:0
		w.ue(0)  // bit_depth_luma_minus8
		w.ue(0)  // bit_depth_chroma_minus8
		w.bit(0) // qpprime
		w.bit(0) // seq_scaling_matrix_present_flag
	}
	w.ue(0) // log2_max_frame_num_minus4
	w.ue(2) // pic_order_cnt_type
	w.ue(1) // max_num_ref_frames
	w.bit(0)
	mbW := (o.Width + 15) / 16
	mbH := (o.Height + 15) / 16
	w.ue(uint32(mbW - 1))
	w.ue(uint32(mbH - 1))
	w.bit(1) // frame_mbs_only_flag
	w.bit(1) // direct_8x8_inference_flag
	cropR := (mbW*16 - o.Width) / 2
	cropB := (mbH*16 - o.Height) / 2
	if cropR != 0 || cropB != 0 {
		w.bit(1)
		w.ue(0)
		w.ue(uint32(cropR))
		w.ue(0)
		w.ue(uint32(cropB))
	} else {
		w.bit(0)
	}
	w.bit(0) // vui_parameters_present_flag
	w.trailing()
	return append([]byte{0x67, o.Profile, 0x00, o.Level}, escape(w.buf)...)
}

// PPS is a minimal PPS NAL.
func PPS() []byte { return []byte{0x68, 0xce, 0x3c, 0x80} }

// Slice types (slice_type values; +5 means "all slices of the picture").
const (
	SliceP = 0
	SliceB = 1
	SliceI = 2
)

// Slice builds a slice NAL of about size bytes. idr picks NAL type 5 (I
// slice) over 1; firstMB is first_mb_in_slice (0 opens the picture).
func Slice(idr bool, sliceType uint32, firstMB uint32, size int, seed byte) []byte {
	hdr := byte(0x41) // nal_ref_idc 2, type 1
	if idr {
		hdr = 0x65
	} else if sliceType == SliceB {
		hdr = 0x01 // non-reference B
	}
	w := &bitWriter{}
	w.ue(firstMB)
	w.ue(sliceType + 5)
	w.ue(0) // pic_parameter_set_id
	for len(w.buf) < size {
		// Filler that never forms a start code or an escape.
		w.bits(uint32(0xa0|seed&0x0f)|1, 8)
		seed++
	}
	w.trailing()
	return append([]byte{hdr}, escape(w.buf)...)
}

// AU is one synthetic access unit's NAL units.
type AU struct {
	NALs     [][]byte
	Keyframe bool
}

// IDR is SPS + PPS + one IDR slice of size bytes.
func IDR(sps []byte, size int) AU {
	return AU{NALs: [][]byte{sps, PPS(), Slice(true, SliceI, 0, size, 1)}, Keyframe: true}
}

// IDRNoParams is a bare IDR slice (a sender that doesn't repeat headers).
func IDRNoParams(size int) AU {
	return AU{NALs: [][]byte{Slice(true, SliceI, 0, size, 1)}, Keyframe: true}
}

// P is one P slice.
func P(size int, seed byte) AU { return AU{NALs: [][]byte{Slice(false, SliceP, 0, size, seed)}} }

// B is one B slice.
func B(size int) AU { return AU{NALs: [][]byte{Slice(false, SliceB, 0, size, 3)}} }

// AnnexB joins the NALs with start codes, as the gateway emits them.
func (a AU) AnnexB() []byte {
	var out []byte
	for _, n := range a.NALs {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// Packetize splits an AU into RTP payloads libdatachannel-style: SPS and
// PPS aggregated into one STAP-A, each other NAL single or FU-A at mtu.
func Packetize(a AU, mtu int) [][]byte {
	var out [][]byte
	var params [][]byte
	flush := func() {
		if len(params) == 0 {
			return
		}
		stap := []byte{0x78} // F=0, NRI=3, type 24
		for _, n := range params {
			stap = binary.BigEndian.AppendUint16(stap, uint16(len(n)))
			stap = append(stap, n...)
		}
		out = append(out, stap)
		params = nil
	}
	for _, n := range a.NALs {
		if t := n[0] & 0x1f; t == 7 || t == 8 {
			params = append(params, n)
			continue
		}
		flush()
		if len(n) <= mtu {
			out = append(out, n)
			continue
		}
		ind := n[0]&0xe0 | 28
		body := n[1:]
		first := true
		for len(body) > 0 {
			k := mtu - 2
			if k > len(body) {
				k = len(body)
			}
			fh := n[0] & 0x1f
			if first {
				fh |= 0x80
			}
			if k == len(body) {
				fh |= 0x40
			}
			out = append(out, append([]byte{ind, fh}, body[:k]...))
			body = body[k:]
			first = false
		}
	}
	flush()
	return out
}

// Stream generates RTP packets for a sequence of AUs at fps.
type Stream struct {
	SSRC    uint32
	Seq     uint16
	TS      uint32
	FPS     int
	MTU     int
	Payload uint8
}

// Packets returns one AU's RTP packets and advances the timestamp.
func (s *Stream) Packets(a AU) []*rtp.Packet {
	if s.MTU == 0 {
		s.MTU = 1200
	}
	if s.FPS == 0 {
		s.FPS = 60
	}
	if s.Payload == 0 {
		s.Payload = 102
	}
	payloads := Packetize(a, s.MTU)
	pkts := make([]*rtp.Packet, len(payloads))
	for i, p := range payloads {
		pkts[i] = &rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: s.Payload, SequenceNumber: s.Seq, Timestamp: s.TS,
			SSRC: s.SSRC, Marker: i == len(payloads)-1,
		}, Payload: p}
		s.Seq++
	}
	s.TS += uint32(90000 / s.FPS)
	return pkts
}
