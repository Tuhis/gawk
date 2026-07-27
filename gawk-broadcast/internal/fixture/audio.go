package fixture

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
)

// Audio is the committed Opus fixture: 101 packets of 48 kHz stereo, 20 ms
// each (~2 s), 128 kbps — the format Decision 3 forces, so the bytes exercise
// the same wire path a real capture does.
//
// Framing on disk is a uint16 big-endian length prefix per packet, repeated.
// That is deliberately the same shape as R19's carrier records, so the repo
// gains no new framing concept for a test fixture; SplitAudio reads it.
//
// Committed rather than generated at run time, for the same reason sample.ts
// is: generating it needs an Opus encoder in the runner, and the bytes would
// differ between encoder versions. See README-audio.md for the recipe.
//
//go:embed sample-audio.opus
var Audio []byte

// ErrBadAudioFraming is returned when the fixture's length prefixes do not
// tile the file exactly.
var ErrBadAudioFraming = errors.New("fixture: malformed audio framing")

// SplitAudio splits the length-prefixed fixture into Opus packets. The
// returned slices alias b.
func SplitAudio(b []byte) ([][]byte, error) {
	var out [][]byte
	for off := 0; off < len(b); {
		if off+2 > len(b) {
			return nil, fmt.Errorf("%w: %d trailing bytes, need 2 for a length prefix", ErrBadAudioFraming, len(b)-off)
		}
		n := int(binary.BigEndian.Uint16(b[off:]))
		off += 2
		if n == 0 {
			return nil, fmt.Errorf("%w: zero-length packet at %d — there is no empty Opus packet", ErrBadAudioFraming, off-2)
		}
		if off+n > len(b) {
			return nil, fmt.Errorf("%w: packet at %d declares %d bytes, %d remain", ErrBadAudioFraming, off-2, n, len(b)-off)
		}
		out = append(out, b[off:off+n])
		off += n
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no packets", ErrBadAudioFraming)
	}
	return out, nil
}
