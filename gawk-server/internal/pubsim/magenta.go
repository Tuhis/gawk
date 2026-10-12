package pubsim

import (
	"errors"
	"fmt"

	"github.com/Tuhis/gawk/gawk-server/internal/pubsim/engine"
)

// MagentaPhaseFrames is how many frames each phase of the `-fixture magenta`
// loop lasts: 1350, which is 45 s at gawk-pubsim's default 30 fps (other
// rates scale it, like the fixture's keyframe cadence). R40's end-to-end
// check (docs/71 CF8) needs ONE publisher session whose screening window
// trips and later clears; at the sampler's 5 s interval each phase is ~9
// samples, comfortably more than the default 6-sample window, and each phase
// stays long enough (≥ 40 s) for a reasonably stretched interval to see it.
// The dev stack and the e2e assertions rely on this length.
const MagentaPhaseFrames = 1350

// PhasedLoop builds the `-fixture magenta` sequence from the demuxed
// TSMagenta clip: its first half (solid magenta) repeated to phaseFrames,
// then its second half (solid grey) repeated to phaseFrames. Source loops the
// result, so the published stream alternates the two phases forever.
//
// Halves are repeated whole, and each half must start with a keyframe, so
// every repetition is a closed set of GOPs: a decoder joining anywhere — the
// relay's cached keyframe is all R40's sampler ever decodes — sees a valid
// stream. Access units are shared, not copied; the engine never writes them.
func PhasedLoop(aus []engine.AccessUnit, phaseFrames int) ([]engine.AccessUnit, error) {
	if len(aus) < 2 || len(aus)%2 != 0 {
		return nil, fmt.Errorf("pubsim: phased fixture needs an even number of access units, got %d", len(aus))
	}
	half := len(aus) / 2
	first, second := aus[:half], aus[half:]
	if !first[0].Keyframe || !second[0].Keyframe {
		return nil, errors.New("pubsim: each half of a phased fixture must start with a keyframe")
	}
	if phaseFrames < half || phaseFrames%half != 0 {
		return nil, fmt.Errorf("pubsim: phase of %d frames is not a whole number of %d-frame halves", phaseFrames, half)
	}
	out := make([]engine.AccessUnit, 0, 2*phaseFrames)
	for _, part := range [][]engine.AccessUnit{first, second} {
		for range phaseFrames / half {
			out = append(out, part...)
		}
	}
	return out, nil
}
