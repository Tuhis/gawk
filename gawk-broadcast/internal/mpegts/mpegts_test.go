package mpegts

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// The fixture is a real H.264 MPEG-TS stream, not hand-rolled bytes:
// 320x240, 30 fps, 2 s (60 frames), GOP 15 (= the shipped 500 ms cadence at
// 30 fps), no B-frames, SPS/PPS repeated before every IDR. See
// testdata/README.md for the exact command.
const fixturePath = "testdata/sample.ts"

const (
	fixtureFrames    = 60
	fixtureGOPFrames = 15
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return b
}

// collect runs the fixture through the demuxer at a given write size and
// returns the access units.
func collect(t *testing.T, ts []byte, writeSize int) []AU {
	t.Helper()
	var aus []AU
	d := NewDemuxer(8<<20, func(au AU) error {
		aus = append(aus, AU{Data: bytes.Clone(au.Data), PTS: au.PTS, HasPTS: au.HasPTS})
		return nil
	})
	for off := 0; off < len(ts); off += writeSize {
		end := min(off+writeSize, len(ts))
		if _, err := d.Write(ts[off:end]); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return aus
}

// One PES = one access unit = one encoded frame. This is the property the
// whole framing decision rests on.
func TestOneAUPerFrame(t *testing.T) {
	aus := collect(t, loadFixture(t), 64*1024)
	if len(aus) != fixtureFrames {
		t.Fatalf("demuxed %d access units, want %d (one per encoded frame)", len(aus), fixtureFrames)
	}
	for i, au := range aus {
		if len(au.Data) == 0 {
			t.Errorf("AU %d is empty", i)
		}
		if !au.HasPTS {
			t.Errorf("AU %d carries no PTS", i)
		}
	}
}

// The child's stdout is a pipe: it delivers whatever sizes it feels like, and
// a TS packet will straddle reads. Feeding at adversarial sizes — including 1
// byte at a time and sizes coprime with 188 — must produce byte-identical
// output.
func TestAdversarialReadSizes(t *testing.T) {
	ts := loadFixture(t)
	want := collect(t, ts, 64*1024)

	for _, size := range []int{1, 2, 3, 7, 187, 188, 189, 190, 376, 377, 1000, 1188, 4096} {
		got := collect(t, ts, size)
		if len(got) != len(want) {
			t.Fatalf("write size %d: %d access units, want %d", size, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i].Data, want[i].Data) {
				t.Fatalf("write size %d: AU %d differs (%d vs %d bytes)", size, i, len(got[i].Data), len(want[i].Data))
			}
			if got[i].PTS != want[i].PTS {
				t.Fatalf("write size %d: AU %d PTS %d, want %d", size, i, got[i].PTS, want[i].PTS)
			}
		}
	}
}

// The engine bounds an AU by wire.MaxKeyframeBytes. Exceeding it must error
// rather than allocate: an untrusted length must never grow our heap.
func TestAUTooLargeErrors(t *testing.T) {
	ts := loadFixture(t)
	d := NewDemuxer(100, func(AU) error { return nil }) // absurdly small bound
	_, err := d.Write(ts)
	if err == nil {
		err = d.Close()
	}
	if !errors.Is(err, ErrAUTooLarge) {
		t.Fatalf("error = %v, want ErrAUTooLarge", err)
	}
}

// A callback error (the send path failing) must propagate rather than be
// swallowed into a silent stall.
func TestCallbackErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	d := NewDemuxer(8<<20, func(AU) error { return sentinel })
	_, err := d.Write(loadFixture(t))
	if err == nil {
		err = d.Close()
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the callback's error", err)
	}
}

// A stream that starts mid-packet (a child killed and restarted, a pipe
// picked up late) must resync rather than emit garbage.
func TestResyncsFromMidPacketStart(t *testing.T) {
	ts := loadFixture(t)
	full := collect(t, ts, 64*1024)
	// Drop the first 50 bytes: the grid now starts mid-packet.
	partial := collect(t, ts[50:], 64*1024)
	if len(partial) == 0 {
		t.Fatal("no access units recovered after a mid-packet start")
	}
	// The tail must agree with the corresponding tail of the clean run.
	if len(partial) > len(full) {
		t.Fatalf("recovered %d AUs from a truncated stream, want ≤ %d", len(partial), len(full))
	}
	lastGot, lastWant := partial[len(partial)-1], full[len(full)-1]
	if !bytes.Equal(lastGot.Data, lastWant.Data) {
		t.Error("final AU differs after resync")
	}
}

// PTS is monotonic and advances at the fixture's frame rate (90 kHz / 30 fps =
// 3000 ticks). It is unused today, but Decision 6's fallback depends on it
// being real, so pin it now rather than discover it later.
func TestPTSIsSaneAndMonotonic(t *testing.T) {
	aus := collect(t, loadFixture(t), 64*1024)
	for i := 1; i < len(aus); i++ {
		if aus[i].PTS <= aus[i-1].PTS {
			t.Fatalf("PTS not monotonic at AU %d: %d then %d", i, aus[i-1].PTS, aus[i].PTS)
		}
		if delta := aus[i].PTS - aus[i-1].PTS; delta != 3000 {
			t.Errorf("AU %d PTS delta = %d ticks, want 3000 (90kHz/30fps)", i, delta)
		}
	}
}
