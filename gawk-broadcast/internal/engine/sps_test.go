package engine

import (
	"bytes"
	"os"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
)

// The same real fixture the demuxer tests use (see
// internal/mpegts/testdata/README.md, including why it is ffmpeg-generated
// rather than captured from the live pipeline).
const (
	fixturePath      = "../mpegts/testdata/sample.ts"
	fixtureFrames    = 60
	fixtureGOPFrames = 15
	// What ffprobe reports for the fixture: Constrained Baseline, level 1.3.
	// profile_idc 66 = 0x42, level_idc 13 = 0x0D.
	fixtureCodec = "avc1.42C00D"
)

func fixtureAUs(t *testing.T) [][]byte {
	t.Helper()
	ts, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var aus [][]byte
	d := mpegts.NewDemuxer(8<<20, func(au mpegts.AU) error {
		aus = append(aus, bytes.Clone(au.Data))
		return nil
	})
	if _, err := d.Write(ts); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return aus
}

func TestParseCodecStringFromSyntheticSPS(t *testing.T) {
	// profile_idc 0x42, constraint flags 0xE0, level_idc 0x2A → the codec
	// string the browser broadcaster negotiates first (avc1.42E02A).
	au := []byte{0, 0, 0, 1, 0x67, 0x42, 0xE0, 0x2A, 0x99}
	got, ok := ParseCodecString(au)
	if !ok {
		t.Fatal("no codec string parsed")
	}
	if got != "avc1.42E02A" {
		t.Errorf("codec = %q, want avc1.42E02A", got)
	}
}

func TestParseCodecStringHandlesThreeByteStartCodes(t *testing.T) {
	au := []byte{0, 0, 1, 0x67, 0x64, 0x00, 0x28, 0xAC}
	got, ok := ParseCodecString(au)
	if !ok {
		t.Fatal("no codec string parsed from a 3-byte start code")
	}
	if got != "avc1.640028" {
		t.Errorf("codec = %q, want avc1.640028", got)
	}
}

func TestParseCodecStringWithoutSPS(t *testing.T) {
	if _, ok := ParseCodecString(testP); ok {
		t.Error("parsed a codec string from an AU with no SPS")
	}
}

// Decision 8: the codec string is parsed, never assumed — the encoder may pick
// a different level than requested. Cross-checked against what ffprobe says
// about the same fixture.
func TestFixtureCodecStringMatchesEncoder(t *testing.T) {
	aus := fixtureAUs(t)
	got, ok := ParseCodecString(aus[0])
	if !ok {
		t.Fatal("first AU carries no SPS")
	}
	if got != fixtureCodec {
		t.Errorf("codec = %q, want %q (ffprobe: Constrained Baseline, level 13)", got, fixtureCodec)
	}
}

// Decision 7: an AU containing an IDR slice is a keyframe → reliable uni
// stream. The fixture's keyframes are every 15th frame by construction.
func TestFixtureIDRDetectionMatchesKnownPositions(t *testing.T) {
	aus := fixtureAUs(t)
	if len(aus) != fixtureFrames {
		t.Fatalf("%d AUs, want %d", len(aus), fixtureFrames)
	}
	for i, au := range aus {
		want := i%fixtureGOPFrames == 0
		if got := HasIDR(au); got != want {
			t.Errorf("AU %d: HasIDR = %v, want %v", i, got, want)
		}
	}
}

// Decision 13, and the one that matters most for late joiners: the
// DecoderConfig extradata is empty on this path, so the relay's cached
// keyframe can only prime a viewer if the parameter sets travel inside the
// keyframe AU itself. h264parse config-interval=-1 is what guarantees it;
// this proves the guarantee is observable.
func TestFixtureKeyframesCarryParameterSetsBeforeTheIDR(t *testing.T) {
	for i, au := range fixtureAUs(t) {
		if !HasIDR(au) {
			continue
		}
		var sawSPS, sawPPS bool
		for nal := range annexBNALs(au) {
			if len(nal) == 0 {
				continue
			}
			switch nal[0] & 0x1f {
			case 7:
				sawSPS = true
			case 8:
				sawPPS = true
			case nalTypeIDR:
				if !sawSPS || !sawPPS {
					t.Errorf("AU %d: IDR slice precedes its SPS/PPS (sps=%v pps=%v) — a primed late joiner could not decode it",
						i, sawSPS, sawPPS)
				}
			}
		}
		if !sawSPS || !sawPPS {
			t.Errorf("AU %d is a keyframe without SPS/PPS in band", i)
		}
	}
}

// Decision 13: no B-frames. The viewer assumes decode order == presentation
// order throughout, and a violation would not fail loudly — it would just play
// subtly wrong.
func TestFixtureHasNoBSlices(t *testing.T) {
	for i, au := range fixtureAUs(t) {
		if HasBSlices(au) {
			t.Errorf("AU %d contains a B slice; decode order == presentation order is a protocol assumption", i)
		}
	}
}

// A detector that never fires is not a detector. Prove HasBSlices can actually
// say yes, so TestFixtureHasNoBSlices means something.
func TestHasBSlicesDetectsABSlice(t *testing.T) {
	// slice header: first_mb_in_slice = 0 → ue "1"; slice_type = 1 (B) → ue
	// "010". Bits: 1 010 0000 → 0xA0.
	bSlice := []byte{0, 0, 0, 1, 0x41, 0xA0}
	if !HasBSlices(bSlice) {
		t.Error("failed to detect a B slice; the no-B-frames assertion would be vacuous")
	}
	// slice_type = 0 (P) → ue "1". Bits: 1 1 000000 → 0xC0.
	pSlice := []byte{0, 0, 0, 1, 0x41, 0xC0}
	if HasBSlices(pSlice) {
		t.Error("reported a B slice for a P slice")
	}
	// slice_type = 2 (I) → ue "011". Bits: 1 011 0000 → 0xB0.
	iSlice := []byte{0, 0, 0, 1, 0x65, 0xB0}
	if HasBSlices(iSlice) {
		t.Error("reported a B slice for an I slice")
	}
}

// The keyframe cadence is the self-healing bound: a lost frame recovers within
// one GOP. The fixture pins 500 ms at its own frame rate.
func TestFixtureKeyframeCadence(t *testing.T) {
	aus := fixtureAUs(t)
	var lastIDR = -1
	for i, au := range aus {
		if !HasIDR(au) {
			continue
		}
		if lastIDR >= 0 {
			if gap := i - lastIDR; gap != fixtureGOPFrames {
				t.Errorf("keyframe gap %d frames at AU %d, want %d (500ms at 30fps)", gap, i, fixtureGOPFrames)
			}
		}
		lastIDR = i
	}
}

func TestUnescapeRBSP(t *testing.T) {
	// 00 00 03 01 → the 0x03 is an emulation-prevention byte, not data.
	got := unescapeRBSP([]byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x02})
	want := []byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x02}
	if !bytes.Equal(got, want) {
		t.Errorf("unescapeRBSP = %x, want %x", got, want)
	}
	// A 0x03 not preceded by two zeros is ordinary data.
	if got := unescapeRBSP([]byte{0x01, 0x03, 0x04}); !bytes.Equal(got, []byte{0x01, 0x03, 0x04}) {
		t.Errorf("unescapeRBSP stripped a real 0x03: %x", got)
	}
}
