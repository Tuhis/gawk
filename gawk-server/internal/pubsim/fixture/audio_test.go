package fixture

import (
	"errors"
	"testing"
)

// The fixture's shape is what gawk-pubsim's audio pass depends on, and what a
// human listening to it should hear. Both are pinned here so a regenerated
// fixture cannot quietly become something else.
func TestAudioFixtureIsTwoSecondsOf20msStereo(t *testing.T) {
	packets, err := SplitAudio(Audio)
	if err != nil {
		t.Fatalf("SplitAudio: %v", err)
	}
	if len(packets) != 101 {
		t.Errorf("%d packets, want 101 (~2 s at 20 ms)", len(packets))
	}
	for i, p := range packets {
		if len(p) == 0 {
			t.Fatalf("packet %d is empty", i)
		}
		// TOC config 31 | stereo | one frame — CELT fullband, 20 ms, stereo.
		// Checked here rather than via internal/opus so this package keeps its
		// "imports nothing of ours" property, which is what lets white-box
		// test packages use it without an import cycle.
		if config := p[0] >> 3; config != 31 {
			t.Fatalf("packet %d TOC config = %d, want 31 (CELT fullband 20 ms)", i, config)
		}
		if p[0]&0x04 == 0 {
			t.Fatalf("packet %d is mono; the viewer is configured for stereo", i)
		}
		if p[0]&0x03 != 0 {
			t.Fatalf("packet %d carries several frames; one packet per datagram assumes one", i)
		}
	}
}

// The framing is parsed off a file, so it gets the same treatment as anything
// else parsed off bytes: a length that does not fit is an error, never a
// silently truncated packet.
func TestSplitAudioRejectsMalformedFraming(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"truncated prefix", []byte{0x00}},
		{"length past the end", []byte{0x00, 0x10, 0x01, 0x02}},
		{"zero-length packet", []byte{0x00, 0x00}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SplitAudio(tc.in); !errors.Is(err, ErrBadAudioFraming) {
				t.Errorf("SplitAudio = %v, want ErrBadAudioFraming", err)
			}
		})
	}
}
