package media

import "errors"

var errBadSPS = errors.New("media: unparseable SPS")

// SPS is the part of a sequence parameter set the gateway reports or checks.
type SPS struct {
	ProfileIDC  uint8
	Constraints uint8
	LevelIDC    uint8
	// Width and Height are the cropped picture size (D9's resolution).
	Width, Height int
}

// Supported reports whether viewers can decode it (D5: Constrained
// Baseline, Main or High, up to level 5.2). Plain Baseline is accepted too:
// real encoders never use the FMO/ASO tools that separate it from
// Constrained Baseline, and libdatachannel-based senders label everything
// 42e01f anyway.
func (s SPS) Supported() bool {
	switch s.ProfileIDC {
	case 66, 77, 100:
	default:
		return false
	}
	return s.LevelIDC <= 52
}

// ParseSPS parses an SPS NAL (header byte included).
func ParseSPS(nal []byte) (SPS, error) {
	if len(nal) < 4 || nal[0]&0x1f != nalSPS {
		return SPS{}, errBadSPS
	}
	s := SPS{ProfileIDC: nal[1], Constraints: nal[2], LevelIDC: nal[3]}
	br := &bitReader{buf: unescapeRBSP(nal[4:])}
	ue := func() uint32 { v, _ := br.ue(); return v }
	se := func() { br.ue() }
	if _, ok := br.ue(); !ok { // seq_parameter_set_id
		return SPS{}, errBadSPS
	}
	chroma := uint32(1)
	separateColour := false
	switch s.ProfileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = ue()
		if chroma > 3 {
			return SPS{}, errBadSPS
		}
		if chroma == 3 {
			separateColour = br.bit() == 1
		}
		ue()               // bit_depth_luma_minus8
		ue()               // bit_depth_chroma_minus8
		br.bit()           // qpprime_y_zero_transform_bypass_flag
		if br.bit() == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if br.bit() == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(br, size)
				}
			}
		}
	}
	ue() // log2_max_frame_num_minus4
	switch poc := ue(); poc {
	case 0:
		ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		br.bit() // delta_pic_order_always_zero_flag
		se()     // offset_for_non_ref_pic
		se()     // offset_for_top_to_bottom_field
		n := ue()
		if n > 255 {
			return SPS{}, errBadSPS
		}
		for i := uint32(0); i < n; i++ {
			se()
		}
	case 2:
	default:
		return SPS{}, errBadSPS
	}
	ue()     // max_num_ref_frames
	br.bit() // gaps_in_frame_num_value_allowed_flag
	wMbs := ue() + 1
	hMapUnits := ue() + 1
	frameMbsOnly := br.bit()
	if frameMbsOnly == 0 {
		br.bit() // mb_adaptive_frame_field_flag
	}
	br.bit() // direct_8x8_inference_flag
	var cl, cr, ct, cb uint32
	if br.bit() == 1 { // frame_cropping_flag
		cl, cr, ct, cb = ue(), ue(), ue(), ue()
	}
	if br.err {
		return SPS{}, errBadSPS
	}
	// Crop units (H.264 §7.4.2.1.1, Table 6-1).
	subW, subH := uint32(2), uint32(2)
	switch chroma {
	case 2:
		subH = 1
	case 3:
		subW, subH = 1, 1
	}
	cropX, cropY := subW, subH*(2-frameMbsOnly)
	if chroma == 0 || separateColour {
		cropX, cropY = 1, 2-frameMbsOnly
	}
	w := wMbs * 16
	h := (2 - frameMbsOnly) * hMapUnits * 16
	if cropX*(cl+cr) >= w || cropY*(ct+cb) >= h || w > 16384 || h > 16384 {
		return SPS{}, errBadSPS
	}
	s.Width = int(w - cropX*(cl+cr))
	s.Height = int(h - cropY*(ct+cb))
	return s, nil
}

func skipScalingList(br *bitReader, size int) {
	last, next := int32(8), int32(8)
	for j := 0; j < size; j++ {
		if next != 0 {
			v, _ := br.ue()
			// se(v) from ue(v): k → (−1)^(k+1) ⌈k/2⌉
			delta := int32((v + 1) / 2)
			if v%2 == 0 {
				delta = -delta
			}
			next = (last + delta + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 → 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 0x03 {
			zeros = 0
			continue
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

// bitReader reads MSB-first bits and exp-Golomb codes; reading past the end
// sets err and returns zeros.
type bitReader struct {
	buf []byte
	pos int
	err bool
}

func (r *bitReader) bit() uint32 {
	if r.pos >= len(r.buf)*8 {
		r.err = true
		return 0
	}
	b := r.buf[r.pos/8] >> (7 - uint(r.pos%8)) & 1
	r.pos++
	return uint32(b)
}

func (r *bitReader) ue() (uint32, bool) {
	zeros := 0
	for r.bit() == 0 {
		if r.err || zeros > 31 {
			r.err = true
			return 0, false
		}
		zeros++
	}
	if r.err {
		return 0, false
	}
	var v uint32
	for i := 0; i < zeros; i++ {
		v = v<<1 | r.bit()
	}
	if r.err {
		return 0, false
	}
	return (1 << uint(zeros)) - 1 + v, true
}
