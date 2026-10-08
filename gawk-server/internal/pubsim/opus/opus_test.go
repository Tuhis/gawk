package opus

import "testing"

// 0xfc is the TOC the NA1 spike actually recorded off GStreamer's opusenc with
// the Decision 3 property set (docs/28, and the mpegts testdata README): CELT
// fullband, 20 ms, stereo, one frame. If this ever reads differently, the
// pipeline stopped producing what the AudioConfig advertises.
func TestNA1TOC(t *testing.T) {
	got, ok := ParseTOC([]byte{0xfc, 0x00})
	if !ok {
		t.Fatal("ParseTOC rejected the TOC the NA1 capture contains")
	}
	if !got.Stereo || got.Channels() != 2 {
		t.Errorf("Stereo = %v (%d channels), want stereo", got.Stereo, got.Channels())
	}
	if got.FrameDurationUs != 20_000 || got.Frames != 1 || got.DurationUs != 20_000 {
		t.Errorf("TOC = %+v, want one 20 ms frame", got)
	}
}

func TestFrameSizeTable(t *testing.T) {
	// config << 3 | stereo << 2 | code. Every mode's boundaries, because the
	// three sub-tables are indexed differently (SILK and CELT by two bits,
	// Hybrid by one) and an off-by-one there is a silently wrong duration.
	for _, tc := range []struct {
		name   string
		toc    byte
		frame  uint64
		frames int
	}{
		{"silk nb 10ms", 0 << 3, 10_000, 1},
		{"silk nb 60ms", 3 << 3, 60_000, 1},
		{"silk wb 20ms", 9 << 3, 20_000, 1},
		{"hybrid swb 10ms", 12 << 3, 10_000, 1},
		{"hybrid fb 20ms", 15 << 3, 20_000, 1},
		{"celt nb 2.5ms", 16 << 3, 2_500, 1},
		{"celt fb 20ms", 31 << 3, 20_000, 1},
		{"two frames, code 1", 31<<3 | 1, 20_000, 2},
		{"two frames, code 2", 31<<3 | 2, 20_000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseTOC([]byte{tc.toc, 0x00})
			if !ok {
				t.Fatalf("ParseTOC(%#02x) rejected a valid TOC", tc.toc)
			}
			if got.FrameDurationUs != tc.frame {
				t.Errorf("FrameDurationUs = %d, want %d", got.FrameDurationUs, tc.frame)
			}
			if got.Frames != tc.frames {
				t.Errorf("Frames = %d, want %d", got.Frames, tc.frames)
			}
			if want := uint64(tc.frames) * tc.frame; got.DurationUs != want {
				t.Errorf("DurationUs = %d, want %d", got.DurationUs, want)
			}
		})
	}
}

// Code 3 counts its frames in the *second* byte. A packet that ends before it
// must be rejected rather than assumed to hold one frame: the caller derives
// timestamps from this duration, so a guess here walks every later stamp.
func TestCode3CountsFramesInTheSecondByte(t *testing.T) {
	got, ok := ParseTOC([]byte{31<<3 | 0x04 | 3, 0x83}) // 3 frames, VBR bit set
	if !ok {
		t.Fatal("ParseTOC rejected a code-3 packet")
	}
	if got.Frames != 3 || got.DurationUs != 60_000 {
		t.Errorf("TOC = %+v, want 3 frames of 20 ms", got)
	}

	if _, ok := ParseTOC([]byte{31<<3 | 3}); ok {
		t.Error("accepted a code-3 packet with no frame-count byte")
	}
	if _, ok := ParseTOC([]byte{31<<3 | 3, 0x00}); ok {
		t.Error("accepted a code-3 packet declaring zero frames")
	}
}

func TestEmptyPacketIsRejected(t *testing.T) {
	if _, ok := ParseTOC(nil); ok {
		t.Error("accepted an empty packet — there is no zero-byte Opus packet")
	}
}

func TestMonoIsReported(t *testing.T) {
	got, ok := ParseTOC([]byte{31 << 3}) // stereo bit clear
	if !ok {
		t.Fatal("ParseTOC rejected a mono TOC")
	}
	if got.Stereo || got.Channels() != 1 {
		t.Errorf("Stereo = %v (%d channels), want mono", got.Stereo, got.Channels())
	}
}
