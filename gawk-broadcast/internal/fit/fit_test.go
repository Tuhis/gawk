package fit

import "testing"

// The cases below are the Rust module's golden cases
// (gawk-broadcast-windows/crates/capture/src/fit.rs), restated verbatim. Two
// implementations of one rule drift silently unless the *answers* are pinned
// on both sides; docs/39 D2 asks for exactly that, the same way the wire
// mirrors keep byte-identical vectors.
func TestMatchingAspectFillsTheBoxExactly(t *testing.T) {
	cases := [][4]int{
		{2560, 1440, 1920, 1080},
		{1920, 1080, 1920, 1080},
		// Upscaling a small source still pins one side to the box.
		{640, 360, 1920, 1080},
	}
	for _, c := range cases {
		if w, h := Within(c[0], c[1], c[2], c[3]); w != 1920 || h != 1080 {
			t.Errorf("Within(%d,%d,%d,%d) = %dx%d, want 1920x1080", c[0], c[1], c[2], c[3], w, h)
		}
	}
}

func TestNarrowerSourcesPinHeightAndShrinkWidth(t *testing.T) {
	// 4:3 into a 16:9 box: height matches, width shrinks.
	if w, h := Within(1600, 1200, 1920, 1080); w != 1440 || h != 1080 {
		t.Errorf("4:3 fit = %dx%d, want 1440x1080", w, h)
	}
	// Portrait window into the same box.
	if w, h := Within(1080, 1920, 1920, 1080); w != 608 || h != 1080 {
		t.Errorf("portrait fit = %dx%d, want 608x1080", w, h)
	}
}

func TestWiderSourcesPinWidthAndShrinkHeight(t *testing.T) {
	// 21:9 ultrawide into a 16:9 box: width matches, height shrinks. This is
	// the monitor-source case D2 calls out — today's exact caps stretch it.
	if w, h := Within(3440, 1440, 1920, 1080); w != 1920 || h != 804 {
		t.Errorf("ultrawide fit = %dx%d, want 1920x804", w, h)
	}
}

func TestResultsAreEven(t *testing.T) {
	for _, c := range [][2]int{{1366, 768}, {1279, 721}, {1000, 700}} {
		w, h := Within(c[0], c[1], 1920, 1080)
		if w%2 != 0 || h%2 != 0 {
			t.Errorf("Within(%d,%d,1920,1080) = %dx%d, want both even", c[0], c[1], w, h)
		}
		if w != 1920 && h != 1080 {
			t.Errorf("Within(%d,%d,1920,1080) = %dx%d: one side must pin to the box", c[0], c[1], w, h)
		}
	}
}

// The worked example from docs/39 AS1's acceptance table, so the number in the
// design doc and the number the code produces cannot part company.
func TestDesignDocWorkedExample(t *testing.T) {
	if w, h := Within(1000, 700, 1920, 1080); w != 1542 || h != 1080 {
		t.Errorf("Within(1000,700,1920,1080) = %dx%d, want 1542x1080 (docs/39 AS1)", w, h)
	}
}

func TestDegenerateInputsFallBackToTheBox(t *testing.T) {
	// A portal that omitted `size` parses to 0x0, and the fallback is exactly
	// the pre-R35 exact-caps behavior (D2).
	cases := []struct {
		sw, sh, bw, bh int
		wantW, wantH   int
	}{
		{0, 0, 1920, 1080, 1920, 1080},
		{100, 0, 1280, 720, 1280, 720},
		{0, 100, 1280, 720, 1280, 720},
		{-4, 9, 1280, 720, 1280, 720},
	}
	for _, c := range cases {
		if w, h := Within(c.sw, c.sh, c.bw, c.bh); w != c.wantW || h != c.wantH {
			t.Errorf("Within(%d,%d,%d,%d) = %dx%d, want %dx%d",
				c.sw, c.sh, c.bw, c.bh, w, h, c.wantW, c.wantH)
		}
	}
}

// An odd *box* is floored too: the encoder caps have always applied &^1 to the
// configured resolution (encoderCaps), and moving that job here must not lose
// it. A 1921x1081 box is what a hand-typed -resolution can produce.
func TestOddBoxIsFlooredToEven(t *testing.T) {
	if w, h := Within(0, 0, 1921, 1081); w != 1920 || h != 1080 {
		t.Errorf("degenerate fit into an odd box = %dx%d, want 1920x1080", w, h)
	}
	if w, h := Within(1920, 1080, 1921, 1081); w%2 != 0 || h%2 != 0 {
		t.Errorf("fit into an odd box = %dx%d, want both even", w, h)
	}
}

// Never zero, never negative: a 1x1 window must still produce a codec-legal
// size rather than caps the encoder rejects.
func TestTinySourcesFloorAtTwo(t *testing.T) {
	if w, h := Within(1, 4000, 1920, 1080); w != 2 || h != 1080 {
		t.Errorf("sliver fit = %dx%d, want 2x1080", w, h)
	}
	if w, h := Within(4000, 1, 1920, 1080); w != 1920 || h != 2 {
		t.Errorf("sliver fit = %dx%d, want 1920x2", w, h)
	}
}
