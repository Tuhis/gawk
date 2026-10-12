package pubsim

import (
	"bytes"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/pubsim/engine"
	"github.com/Tuhis/gawk/gawk-server/internal/pubsim/fixture"
)

// The committed clip has the shape PhasedLoop and the docs/71 CF8 tier rely
// on: 60 access units, a keyframe every 15 (the default fixture's cadence),
// and two halves that each start on a keyframe and differ (magenta, grey).
func TestMagentaFixtureShape(t *testing.T) {
	aus, err := Demux(fixture.TSMagenta)
	if err != nil {
		t.Fatalf("Demux(TSMagenta): %v", err)
	}
	if len(aus) != 60 {
		t.Fatalf("magenta fixture has %d access units, want 60 (1 s magenta + 1 s grey @ 30 fps)", len(aus))
	}
	for i, au := range aus {
		if want := i%15 == 0; au.Keyframe != want {
			t.Fatalf("AU %d keyframe = %v, want %v (GOP 15)", i, au.Keyframe, want)
		}
	}
	if bytes.Equal(aus[0].Data, aus[30].Data) {
		t.Fatal("the two halves start with the same keyframe; the clip is not magenta then grey")
	}
}

// PhasedLoop stretches each half to MagentaPhaseFrames by repeating whole
// halves: 45 s of the first half's GOPs, then 45 s of the second's, with the
// keyframe cadence intact across every seam.
func TestPhasedLoopBuildsTwoLongPhases(t *testing.T) {
	aus, err := Demux(fixture.TSMagenta)
	if err != nil {
		t.Fatal(err)
	}
	out, err := PhasedLoop(aus, MagentaPhaseFrames)
	if err != nil {
		t.Fatalf("PhasedLoop: %v", err)
	}
	if len(out) != 2*MagentaPhaseFrames {
		t.Fatalf("len = %d, want %d", len(out), 2*MagentaPhaseFrames)
	}
	if MagentaPhaseFrames/30 < 40 {
		t.Fatalf("a phase is %d s at 30 fps; docs/71 CF8 needs at least 40", MagentaPhaseFrames/30)
	}
	for i, au := range out {
		var src engine.AccessUnit
		if i < MagentaPhaseFrames {
			src = aus[i%30]
		} else {
			src = aus[30+(i-MagentaPhaseFrames)%30]
		}
		if !bytes.Equal(au.Data, src.Data) || au.Keyframe != src.Keyframe {
			t.Fatalf("AU %d is not the expected half's AU", i)
		}
		if want := i%15 == 0; au.Keyframe != want {
			t.Fatalf("AU %d keyframe = %v, want %v: a seam broke the GOP cadence", i, au.Keyframe, want)
		}
	}
}

func TestPhasedLoopRejectsBadShapes(t *testing.T) {
	kf := engine.AccessUnit{Data: []byte{1}, Keyframe: true}
	delta := engine.AccessUnit{Data: []byte{2}}
	for name, tc := range map[string]struct {
		aus   []engine.AccessUnit
		phase int
	}{
		"odd length":                 {[]engine.AccessUnit{kf, delta, kf}, 4},
		"second half not a keyframe": {[]engine.AccessUnit{kf, delta, delta, delta}, 4},
		"phase not a whole multiple": {[]engine.AccessUnit{kf, delta, kf, delta}, 3},
		"phase shorter than a half":  {[]engine.AccessUnit{kf, delta, kf, delta}, 1},
	} {
		if _, err := PhasedLoop(tc.aus, tc.phase); err == nil {
			t.Errorf("%s: PhasedLoop accepted it", name)
		}
	}
}
