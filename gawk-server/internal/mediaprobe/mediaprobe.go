// Package mediaprobe reads a broadcast's codec family and coded resolution
// out of the media the relay already carries (R59, docs/61 D2): the codec
// string of a DecoderConfig, the SPS of an H.264 stream (AVCC extradata or an
// in-band Annex B NAL), and the keyframe headers of VP8 and VP9.
//
// It exists for metric labels only. Every function is header-only, never
// allocates proportionally to the frame, and reports "don't know" rather than
// an error: a probe that fails costs a label, never the media path.
package mediaprobe

import "strings"

// Codec families, the closed vocabulary of the codec label.
const (
	CodecH264    = "h264"
	CodecVP8     = "vp8"
	CodecVP9     = "vp9"
	CodecAV1     = "av1"
	CodecOther   = "other"
	CodecUnknown = "unknown"
)

// CodecFamily maps a WebCodecs codec string ("avc1.42E02A", "vp09.00.10.08",
// "vp8") onto the codec label vocabulary. Empty is CodecUnknown.
func CodecFamily(codec string) string {
	switch {
	case codec == "":
		return CodecUnknown
	case strings.HasPrefix(codec, "avc1") || strings.HasPrefix(codec, "avc3"):
		return CodecH264
	case strings.HasPrefix(codec, "vp09"):
		return CodecVP9
	case codec == "vp8":
		return CodecVP8
	case strings.HasPrefix(codec, "av01"):
		return CodecAV1
	}
	return CodecOther
}

// ResolutionTier buckets a coded height into the resolution label
// vocabulary: sd, 720p, 1080p, 1440p, 2160p. Zero (not probed) is "unknown".
func ResolutionTier(height int) string {
	switch {
	case height <= 0:
		return "unknown"
	case height <= 480:
		return "sd"
	case height <= 720:
		return "720p"
	case height <= 1080:
		return "1080p"
	case height <= 1440:
		return "1440p"
	}
	return "2160p"
}

// ExtradataHeight returns the coded height carried by a DecoderConfig's
// extradata, or 0. Only H.264 carries one there (AVCC); VP8/VP9 configs have
// none and are probed from their keyframes instead.
func ExtradataHeight(family string, extradata []byte) int {
	if family != CodecH264 {
		return 0
	}
	sps := avccFirstSPS(extradata)
	if sps == nil {
		return 0
	}
	_, h, ok := h264SPSSize(sps)
	if !ok {
		return 0
	}
	return h
}

// KeyframeHeight returns the coded height of an encoded keyframe, or 0. The
// format is recognised from the bytes themselves (an Annex B start code with
// an SPS, the VP8 keyframe start code, the VP9 frame marker and sync code),
// so it needs no codec hint. AVCC H.264 keyframes carry no SPS and return 0;
// their height comes from ExtradataHeight.
func KeyframeHeight(payload []byte) int {
	if _, h, ok := vp8KeyframeSize(payload); ok {
		return h
	}
	if _, h, ok := vp9KeyframeSize(payload); ok {
		return h
	}
	if sps := annexBFirstSPS(payload); sps != nil {
		if _, h, ok := h264SPSSize(sps); ok {
			return h
		}
	}
	return 0
}

// vp8KeyframeSize parses the VP8 keyframe header (RFC 6386 §9.1): a 3-byte
// frame tag whose bit 0 is 0 on a keyframe, the start code 9d 01 2a, then
// 14-bit little-endian width and height.
func vp8KeyframeSize(p []byte) (w, h int, ok bool) {
	if len(p) < 10 || p[0]&1 != 0 || p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
		return 0, 0, false
	}
	w = int(p[6]) | int(p[7]&0x3f)<<8
	h = int(p[8]) | int(p[9]&0x3f)<<8
	return w, h, w > 0 && h > 0
}

// vp9KeyframeSize parses the start of a VP9 uncompressed header (VP9
// bitstream spec §6.2) up to frame_size() on a keyframe.
func vp9KeyframeSize(p []byte) (w, h int, ok bool) {
	br := bitReader{buf: p}
	if br.u(2) != 2 { // frame_marker
		return 0, 0, false
	}
	low := br.u(1)
	high := br.u(1)
	profile := high<<1 | low
	if profile == 3 {
		br.u(1) // reserved_zero
	}
	if br.u(1) == 1 { // show_existing_frame
		return 0, 0, false
	}
	if br.u(1) != 0 { // frame_type: 0 = KEY_FRAME
		return 0, 0, false
	}
	br.u(1) // show_frame
	br.u(1) // error_resilient_mode
	if br.u(8) != 0x49 || br.u(8) != 0x83 || br.u(8) != 0x42 {
		return 0, 0, false
	}
	// color_config()
	if profile >= 2 {
		br.u(1) // ten_or_twelve_bit
	}
	const csRGB = 7
	if br.u(3) != csRGB { // color_space
		br.u(1) // color_range
		if profile == 1 || profile == 3 {
			br.u(1) // subsampling_x
			br.u(1) // subsampling_y
			br.u(1) // reserved_zero
		}
	} else if profile == 1 || profile == 3 {
		br.u(1) // reserved_zero
	}
	w = int(br.u(16)) + 1
	h = int(br.u(16)) + 1
	if br.overrun {
		return 0, 0, false
	}
	return w, h, true
}

// avccFirstSPS returns the first SPS NAL of an AVCDecoderConfigurationRecord
// (ISO/IEC 14496-15 §5.3.3.1), or nil.
func avccFirstSPS(rec []byte) []byte {
	if len(rec) < 8 || rec[0] != 1 || rec[5]&0x1f == 0 {
		return nil
	}
	n := int(rec[6])<<8 | int(rec[7])
	if n == 0 || len(rec) < 8+n {
		return nil
	}
	return rec[8 : 8+n]
}

// annexBScanWindow bounds the start-code search. An Annex B keyframe opens
// with its parameter sets (AUD, SPS, PPS, then SEI and slices), so the SPS is
// always near the front; scanning the whole frame would cost a pass over
// megabytes per keyframe, and deep into an AVCC frame a length prefix can
// read as a start code.
const annexBScanWindow = 512

// annexBFirstSPS returns the first SPS NAL (type 7) starting within the
// first annexBScanWindow bytes of an Annex B byte stream, from its header
// byte up to the next start code, or nil.
func annexBFirstSPS(p []byte) []byte {
	for i := 0; i+3 < len(p) && i < annexBScanWindow; i++ {
		if p[i] != 0 || p[i+1] != 0 || p[i+2] != 1 {
			continue
		}
		start := i + 3
		if p[start]&0x1f != 7 {
			continue
		}
		end := len(p)
		for j := start; j+2 < len(p); j++ {
			if p[j] == 0 && p[j+1] == 0 && (p[j+2] == 1 || p[j+2] == 0) {
				end = j
				break
			}
		}
		return p[start:end]
	}
	return nil
}

// h264SPSSize parses an SPS NAL (header byte included; H.264 §7.3.2.1.1)
// far enough to compute the cropped frame size.
func h264SPSSize(nal []byte) (w, h int, ok bool) {
	if len(nal) < 4 || nal[0]&0x1f != 7 {
		return 0, 0, false
	}
	br := bitReader{buf: unescapeRBSP(nal[1:])}
	profile := br.u(8)
	br.u(8) // constraint flags + reserved
	br.u(8) // level_idc
	br.ue() // seq_parameter_set_id
	chromaFormat := uint32(1)
	separateColourPlanes := uint32(0)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chromaFormat = br.ue()
		if chromaFormat == 3 {
			separateColourPlanes = br.u(1)
		}
		br.ue()           // bit_depth_luma_minus8
		br.ue()           // bit_depth_chroma_minus8
		br.u(1)           // qpprime_y_zero_transform_bypass_flag
		if br.u(1) == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if chromaFormat == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if br.u(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(&br, size)
				}
			}
		}
	}
	br.ue()          // log2_max_frame_num_minus4
	switch br.ue() { // pic_order_cnt_type
	case 0:
		br.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		br.u(1) // delta_pic_order_always_zero_flag
		br.se() // offset_for_non_ref_pic
		br.se() // offset_for_top_to_bottom_field
		n := br.ue()
		if n > 255 {
			return 0, 0, false
		}
		for i := uint32(0); i < n; i++ {
			br.se()
		}
	}
	br.ue() // max_num_ref_frames
	br.u(1) // gaps_in_frame_num_value_allowed_flag
	widthMbs := int(br.ue()) + 1
	heightMapUnits := int(br.ue()) + 1
	frameMbsOnly := int(br.u(1))
	if frameMbsOnly == 0 {
		br.u(1) // mb_adaptive_frame_field_flag
	}
	br.u(1) // direct_8x8_inference_flag
	w = widthMbs * 16
	h = (2 - frameMbsOnly) * heightMapUnits * 16
	if br.u(1) == 1 { // frame_cropping_flag
		left, right, top, bottom := int(br.ue()), int(br.ue()), int(br.ue()), int(br.ue())
		cropX, cropY := 1, 2-frameMbsOnly
		if separateColourPlanes == 0 && chromaFormat != 0 {
			if chromaFormat == 1 || chromaFormat == 2 {
				cropX = 2
			}
			if chromaFormat == 1 {
				cropY *= 2
			}
		}
		w -= (left + right) * cropX
		h -= (top + bottom) * cropY
	}
	if br.overrun || w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return 0, 0, false
	}
	return w, h, true
}

func skipScalingList(br *bitReader, size int) {
	last, next := int32(8), int32(8)
	for j := 0; j < size; j++ {
		if next != 0 {
			next = (last + br.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

// unescapeRBSP drops the emulation-prevention bytes (00 00 03 → 00 00). Only
// the SPS is unescaped, and an SPS is tens of bytes.
func unescapeRBSP(p []byte) []byte {
	out := make([]byte, 0, len(p))
	zeros := 0
	for _, b := range p {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads MSB-first. Reading past the end yields zeros and sets
// overrun, so a truncated header parses to garbage the caller then rejects.
type bitReader struct {
	buf     []byte
	pos     int
	overrun bool
}

func (b *bitReader) u(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v <<= 1
		if b.pos >= len(b.buf)*8 {
			b.overrun = true
			continue
		}
		v |= uint32(b.buf[b.pos/8]>>(7-b.pos%8)) & 1
		b.pos++
	}
	return v
}

// ue reads an unsigned Exp-Golomb code. More than 31 leading zeros is
// malformed; it marks overrun rather than overflowing.
func (b *bitReader) ue() uint32 {
	zeros := 0
	for b.u(1) == 0 {
		if b.overrun || zeros == 31 {
			b.overrun = true
			return 0
		}
		zeros++
	}
	return (1<<zeros - 1) + b.u(zeros)
}

func (b *bitReader) se() int32 {
	k := b.ue()
	if k&1 == 1 {
		return int32((k + 1) / 2)
	}
	return -int32(k / 2)
}
