package mediaprobe

import (
	"encoding/hex"
	"testing"
)

// Fixtures are the leading bytes of single keyframes encoded by ffmpeg 7.0.2
// from lavfi testsrc (libx264, libvpx, libvpx-vp9). Only the headers matter;
// the probe never reads past them.
const (
	// libx264 -profile:v high, 1920x1080: coded 1088 high, cropped to 1080.
	h264HighAnnexB = "0000000167640028acd940780227e5c044000003000400000300f03c60c65800000001"
	h264HighSPS    = "67640028acd940780227e5c044000003000400000300f03c60c658"
	// libx264 -profile:v baseline, 1280x720.
	h264BaselineSPS = "6742c01fd9005005bb0110000003001000000303c0f1832480"
	// libvpx, 1280x720.
	vp8Keyframe720 = "3077019d012a0005d0020007088585888584880202224c71"
	// libvpx-vp9 profile 0 (4:2:0), 2560x1440.
	vp9Keyframe1440 = "a2498342e09ff059f60638241c184a1c02c65f7fdffbdf9b"
	// libvpx-vp9 profile 1 (4:4:4), 1280x720: exercises the subsampling bits.
	vp9P1Keyframe720 = "a24983420009fe059ec0c7048383094300bd805f7d47fcc7"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// avcc wraps one SPS in an AVCDecoderConfigurationRecord, the extradata shape
// WebCodecs' avc format produces.
func avcc(sps []byte) []byte {
	rec := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	rec = append(rec, sps...)
	return append(rec, 0) // zero PPS
}

func TestMediaInfoFromH264SPS(t *testing.T) {
	cases := []struct {
		name   string
		sps    string
		w, h   int
		height int
	}{
		{"high 1080p, cropped from 1088", h264HighSPS, 1920, 1080, 1080},
		{"baseline 720p", h264BaselineSPS, 1280, 720, 720},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sps := mustHex(t, c.sps)
			w, h, ok := h264SPSSize(sps)
			if !ok || w != c.w || h != c.h {
				t.Fatalf("h264SPSSize = %dx%d ok=%v, want %dx%d", w, h, ok, c.w, c.h)
			}
			if got := ExtradataHeight(CodecH264, avcc(sps)); got != c.height {
				t.Errorf("ExtradataHeight(AVCC) = %d, want %d", got, c.height)
			}
		})
	}
	// Annex B: the SPS rides in-band in the keyframe (the native broadcaster
	// shape, empty extradata).
	if got := KeyframeHeight(mustHex(t, h264HighAnnexB)); got != 1080 {
		t.Errorf("KeyframeHeight(Annex B) = %d, want 1080", got)
	}
}

func TestMediaInfoFromVP8(t *testing.T) {
	if got := KeyframeHeight(mustHex(t, vp8Keyframe720)); got != 720 {
		t.Errorf("KeyframeHeight(VP8) = %d, want 720", got)
	}
	// An interframe (tag bit 0 set) is not a keyframe and reports nothing.
	inter := mustHex(t, vp8Keyframe720)
	inter[0] |= 1
	if _, _, ok := vp8KeyframeSize(inter); ok {
		t.Error("vp8KeyframeSize accepted an interframe")
	}
}

func TestMediaInfoFromVP9(t *testing.T) {
	if w, h, ok := vp9KeyframeSize(mustHex(t, vp9Keyframe1440)); !ok || w != 2560 || h != 1440 {
		t.Errorf("profile 0 = %dx%d ok=%v, want 2560x1440", w, h, ok)
	}
	if w, h, ok := vp9KeyframeSize(mustHex(t, vp9P1Keyframe720)); !ok || w != 1280 || h != 720 {
		t.Errorf("profile 1 = %dx%d ok=%v, want 1280x720", w, h, ok)
	}
	if got := KeyframeHeight(mustHex(t, vp9Keyframe1440)); got != 1440 {
		t.Errorf("KeyframeHeight(VP9) = %d, want 1440", got)
	}
}

// Garbage, truncation and AVCC keyframes (no in-band SPS) all report 0 rather
// than a wrong height or a panic.
func TestMediaProbeUnknownInputsReportZero(t *testing.T) {
	sps := mustHex(t, h264HighSPS)
	inputs := map[string][]byte{
		"empty":              nil,
		"short":              {0x00, 0x00},
		"zeros":              make([]byte, 64),
		"AVCC keyframe":      {0, 0, 0, 5, 0x65, 0x88, 0x84, 0x00, 0x33},
		"truncated SPS":      append([]byte{0, 0, 1}, sps[:6]...),
		"truncated VP9":      mustHex(t, vp9Keyframe1440)[:5],
		"all ones":           {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"start code no SPS":  {0, 0, 1, 0x65, 0x88},
		"start code at end":  {0x12, 0, 0, 1},
		"ue with 32 zeros":   append([]byte{0, 0, 1, 0x67, 0x64, 0x00, 0x28}, make([]byte, 8)...),
		"SPS runs off end":   {0, 0, 1, 0x67, 0x42, 0xc0, 0x1f},
		"VP8 bad start code": {0x30, 0x77, 0x01, 0x9d, 0x01, 0x2b, 0, 5, 0xd0, 2},
	}
	// An "SPS" deep in a frame is a length prefix or slice data, not a
	// parameter set: only the front of the keyframe is searched.
	inputs["SPS past the scan window"] = append(make([]byte, annexBScanWindow+16), mustHex(t, h264HighAnnexB)...)
	for name, in := range inputs {
		if got := KeyframeHeight(in); got != 0 {
			t.Errorf("%s: KeyframeHeight = %d, want 0", name, got)
		}
	}
	if got := ExtradataHeight(CodecH264, []byte{1, 2, 3}); got != 0 {
		t.Errorf("short AVCC: ExtradataHeight = %d, want 0", got)
	}
	if got := ExtradataHeight(CodecVP9, avcc(sps)); got != 0 {
		t.Errorf("non-H.264 family: ExtradataHeight = %d, want 0", got)
	}
}

func TestCodecFamily(t *testing.T) {
	cases := map[string]string{
		"avc1.42E02A":   CodecH264,
		"avc3.640028":   CodecH264,
		"vp09.00.10.08": CodecVP9,
		"vp8":           CodecVP8,
		"av01.0.04M.08": CodecAV1,
		"hvc1.1.6.L93":  CodecOther,
		"":              CodecUnknown,
	}
	for in, want := range cases {
		if got := CodecFamily(in); got != want {
			t.Errorf("CodecFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolutionTier(t *testing.T) {
	cases := map[int]string{0: "unknown", 240: "sd", 480: "sd", 720: "720p", 1080: "1080p", 1200: "1440p", 1440: "1440p", 2160: "2160p"}
	for h, want := range cases {
		if got := ResolutionTier(h); got != want {
			t.Errorf("ResolutionTier(%d) = %q, want %q", h, got, want)
		}
	}
}
