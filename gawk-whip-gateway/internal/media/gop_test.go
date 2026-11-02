package media

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/testmedia"
)

// gopRun feeds a meter one packet per frame at fps for dur, with an IDR every
// gop; skipIDR(n) drops the n-th IDR's packets entirely. It returns the worst
// verdict seen and the final median.
func gopRun(t *testing.T, fps int, gop, dur time.Duration, skipIDR func(n int) bool) (GOPVerdict, []GOPVerdict, time.Duration) {
	t.Helper()
	g := NewGOPMeter(DefaultGOPConfig())
	frameTicks := uint32(VideoClockRate / fps)
	gopFrames := int(gop.Seconds() * float64(fps))
	frames := int(dur.Seconds() * float64(fps))
	ts := uint32(4_000_000_000) // close to the wrap, on purpose
	worst := GOPUnknown
	var seq []GOPVerdict
	idrN := 0
	for i := 0; i < frames; i++ {
		idr := i%gopFrames == 0
		if idr {
			idrN++
		}
		if !(idr && skipIDR != nil && skipIDR(idrN)) {
			g.Packet(ts, idr)
		}
		v := g.Verdict()
		seq = append(seq, v)
		if v > worst {
			worst = v
		}
		ts += frameTicks
	}
	return worst, seq, g.Median()
}

// WG6 / D7: the thresholds: ≤1.1 s fine, ≤2.2 s warn, >2.2 s end.
func TestD7GOPThresholds(t *testing.T) {
	cases := []struct {
		gop  time.Duration
		want GOPVerdict
	}{
		{500 * time.Millisecond, GOPOK},
		{time.Second, GOPOK},
		{1100 * time.Millisecond, GOPOK}, // ≤ 1.1 s is fine
		{1500 * time.Millisecond, GOPWarn},
		{2 * time.Second, GOPWarn},
		{2200 * time.Millisecond, GOPWarn}, // ≤ 2.2 s accepted with a warning
		{3 * time.Second, GOPEnd},
	}
	for _, c := range cases {
		worst, _, med := gopRun(t, 30, c.gop, c.gop*8, nil)
		if worst != c.want {
			t.Errorf("GOP %v: verdict %v, want %v (median %v)", c.gop, worst, c.want, med)
		}
		if diff := med - c.gop; diff > time.Millisecond || diff < -time.Millisecond {
			t.Errorf("GOP %v: median %v", c.gop, med)
		}
	}
}

// WG6: "a stream with a 2 s GOP that loses one whole IDR (and one IDR's
// first packet) is neither warned about more than it would be without the
// loss nor ended".
func TestD7TwoSecondGOPLosingOneIDRIsNotWarnedMoreNorEnded(t *testing.T) {
	_, clean, cleanMed := gopRun(t, 60, 2*time.Second, 30*time.Second, nil)
	_, lossy, lossyMed := gopRun(t, 60, 2*time.Second, 30*time.Second, func(n int) bool { return n == 4 })
	for i := range clean {
		if lossy[i] == GOPEnd {
			t.Fatalf("frame %d: ended after losing one IDR", i)
		}
		if lossy[i] > clean[i] && clean[i] != GOPUnknown {
			t.Fatalf("frame %d: verdict %v with the loss, %v without", i, lossy[i], clean[i])
		}
	}
	if lossyMed != cleanMed {
		t.Fatalf("median %v with the loss, %v without", lossyMed, cleanMed)
	}

	// Losing an IDR's first packet doesn't hide the IDR: every FU-A
	// fragment carries the type.
	sps := testmedia.SPS(testmedia.SPSOptions{})
	payloads := testmedia.Packetize(testmedia.IDR(sps, 6000), 1200)
	if PacketCarriesIDR(payloads[0]) {
		t.Fatal("the STAP-A of SPS/PPS isn't IDR data")
	}
	for i, p := range payloads[1:] {
		if !PacketCarriesIDR(p) {
			t.Fatalf("fragment %d of the IDR not recognised", i+1)
		}
	}
	g := NewGOPMeter(DefaultGOPConfig())
	g.Packet(0, PacketCarriesIDR(payloads[2])) // first two packets lost
	g.Packet(180000, true)
	if g.Median() != 2*time.Second {
		t.Fatalf("median %v", g.Median())
	}
}

// D7: before five spacings exist, two consecutive gaps over 4.5 s end the
// session; one doesn't. OBS's "auto" at ~50 fps is a 5 s GOP.
func TestD7EarlyEndAfterTwoLongGaps(t *testing.T) {
	g := NewGOPMeter(DefaultGOPConfig())
	g.Packet(0, true)
	g.Packet(5*90000, true) // one 5 s gap
	if v := g.Verdict(); v == GOPEnd {
		t.Fatal("one long gap must not end the session")
	}
	g.Packet(9*90000+45000, false) // 4.5 s open gap: not over
	if v := g.Verdict(); v == GOPEnd {
		t.Fatal("4.5 s is not more than 4.5 s")
	}
	g.Packet(9*90000+45001, false)
	if v := g.Verdict(); v != GOPEnd {
		t.Fatalf("second consecutive >4.5 s gap: verdict %v, want end", v)
	}

	// 60 fps "auto" (~4.2 s): never two >4.5 s gaps, so the median rule ends
	// it once five spacings exist (~21 s).
	worst, seq, _ := gopRun(t, 60, 4200*time.Millisecond, 30*time.Second, nil)
	if worst != GOPEnd {
		t.Fatalf("4.2 s GOP verdict %v, want end", worst)
	}
	for i, v := range seq {
		if v == GOPEnd {
			if at := time.Duration(i) * time.Second / 60; at < 21*time.Second {
				t.Fatalf("ended at %v, before five spacings existed", at)
			}
			break
		}
	}

	// A 2.2 s GOP that loses one IDR (a 4.4 s gap) must not trip it.
	worst, _, _ = gopRun(t, 60, 2200*time.Millisecond, 10*time.Second, func(n int) bool { return n == 2 })
	if worst == GOPEnd {
		t.Fatal("a lost IDR at the allowed bound ended the session")
	}
}

// A retransmitted IDR packet arriving after a later one lands in place.
func TestD7OutOfOrderIDRSorted(t *testing.T) {
	g := NewGOPMeter(DefaultGOPConfig())
	for _, s := range []uint32{0, 2, 1, 3, 5, 4, 6} {
		g.Packet(s*90000, true)
		g.Packet(s*90000, true) // duplicate packet of the same frame
	}
	if g.Median() != time.Second || g.Verdict() != GOPOK {
		t.Fatalf("median %v verdict %v", g.Median(), g.Verdict())
	}
}
