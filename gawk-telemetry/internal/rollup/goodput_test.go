package rollup

import (
	"math"
	"testing"
)

// samplesAt builds a 2 s-cadence timeline from per-interval rates: each step
// is (width, per-connection bytes per second). Counters are cumulative, as
// the client sends them.
func goodputTimeline(steps []struct {
	width int
	bps   []float64
}) []Sample {
	var out []Sample
	video := 0.0
	conns := []float64{}
	tMs := 0.0
	emit := func(width int) {
		arr := make([]any, len(conns))
		for i, c := range conns {
			arr[i] = c
		}
		out = append(out, Sample{TMs: tMs, Stats: map[string]any{
			"videoBytesReceived": video, "dvrStripeWidth": float64(width), "connectionBytesReceived": arr,
		}})
	}
	for i, st := range steps {
		for len(conns) < st.width {
			conns = append(conns, 0)
		}
		conns = conns[:st.width]
		if i == 0 {
			emit(st.width)
		}
		tMs += 2000
		for j, r := range st.bps {
			b := r / 8 * 2
			conns[j] += b
			video += b
		}
		emit(st.width)
	}
	return out
}

func repeat(n, width int, bps ...float64) []struct {
	width int
	bps   []float64
} {
	out := make([]struct {
		width int
		bps   []float64
	}, n)
	for i := range out {
		out[i].width, out[i].bps = width, bps
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

func TestDeriveGoodput(t *testing.T) {
	t.Run("per-connection ceiling: each connection keeps its share", func(t *testing.T) {
		steps := append(repeat(10, 1, 6e6), repeat(10, 3, 6.1e6, 6e6, 6.2e6)...)
		g := DeriveGoodput(goodputTimeline(steps))
		if !g.VideoOK || !g.StripeOK || g.WidthMax != 3 {
			t.Fatalf("goodput = %+v", g)
		}
		if !near(g.Width1Bps, 6e6) || !near(g.MaxWidthBps, 18.3e6) || !near(g.MaxWidthConnMinBps, 6e6) {
			t.Errorf("rates = %+v", g)
		}
		if !near(g.VideoBps, (6e6*20+18.3e6*20)/40) {
			t.Errorf("video goodput = %v", g.VideoBps)
		}
	})
	t.Run("full link: connections split a fixed total", func(t *testing.T) {
		steps := append(repeat(10, 1, 7.6e6), repeat(10, 2, 3.8e6, 3.8e6)...)
		g := DeriveGoodput(goodputTimeline(steps))
		if !near(g.MaxWidthBps, 7.6e6) || !near(g.MaxWidthConnMinBps, 3.8e6) {
			t.Errorf("rates = %+v", g)
		}
	})
	t.Run("a width held under the minimum span is not the widest", func(t *testing.T) {
		steps := append(repeat(10, 1, 6e6), repeat(2, 2, 6e6, 6e6)...)
		g := DeriveGoodput(goodputTimeline(steps))
		if g.WidthMax != 1 {
			t.Errorf("widthMax = %d after 4 s at width 2, want 1", g.WidthMax)
		}
	})
	t.Run("never unstriped: no baseline", func(t *testing.T) {
		g := DeriveGoodput(goodputTimeline(repeat(10, 2, 6e6, 6e6)))
		if g.StripeOK || !g.VideoOK {
			t.Errorf("goodput = %+v, want overall goodput only", g)
		}
	})
	t.Run("too short to measure", func(t *testing.T) {
		g := DeriveGoodput(goodputTimeline(repeat(3, 1, 6e6)))
		if g.VideoOK || len(g.Facts()) != 0 {
			t.Errorf("goodput over 6 s = %+v", g)
		}
	})
	t.Run("a counter reset costs its interval only", func(t *testing.T) {
		// A reconnect: the counters restart from zero at sample 5.
		s := goodputTimeline(repeat(10, 1, 8e6))
		base := s[5].Stats["videoBytesReceived"].(float64)
		for _, smp := range s[5:] {
			v := smp.Stats["videoBytesReceived"].(float64) - base
			smp.Stats["videoBytesReceived"] = v
			smp.Stats["connectionBytesReceived"] = []any{v}
		}
		g := DeriveGoodput(s)
		if !g.VideoOK || !near(g.VideoBps, 8e6) || !near(g.Width1Bps, 8e6) {
			t.Errorf("goodput across a reset = %+v", g)
		}
	})
	t.Run("not in deep mode", func(t *testing.T) {
		s := goodputTimeline(repeat(10, 1, 8e6))
		for _, smp := range s {
			delete(smp.Stats, "dvrStripeWidth")
			delete(smp.Stats, "connectionBytesReceived")
		}
		g := DeriveGoodput(s)
		if !g.VideoOK || g.StripeOK {
			t.Errorf("goodput = %+v, want overall only", g)
		}
	})
	t.Run("trim keeps what it reads", func(t *testing.T) {
		s := goodputTimeline(repeat(10, 1, 8e6))
		for i := range s {
			s[i].Stats["receivedFps"] = 60.0
			s[i] = TrimGoodputSample(s[i])
			if _, ok := s[i].Stats["receivedFps"]; ok {
				t.Fatal("trim kept a field goodput does not read")
			}
		}
		if g := DeriveGoodput(s); !g.StripeOK || !near(g.Width1Bps, 8e6) {
			t.Errorf("goodput over trimmed samples = %+v", g)
		}
	})
}

// Review round 2: the client's (dvrGoodputBps, dvrLiveRateBps) pair, compared
// sample by sample over the samples where frames were flowing.
func TestDeriveSupplyRatio(t *testing.T) {
	type smp struct{ goodput, live float64 } // live < 0: omitted
	build := func(ss []smp) []Sample {
		out := make([]Sample, len(ss))
		for i, s := range ss {
			st := map[string]any{"dvrGoodputBps": s.goodput}
			if s.live >= 0 {
				st["dvrLiveRateBps"] = s.live
			}
			out[i] = Sample{TMs: float64(i * 2000), Stats: st}
		}
		return out
	}
	rep := func(n int, s smp) []smp {
		out := make([]smp, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	cases := []struct {
		name  string
		ss    []smp
		ratio float64 // 0: absent
	}{
		{"keeping up", rep(10, smp{9.8e6, 10e6}), 0.98},
		// The broadcaster paused: goodput reads 0, the live rate holds its
		// last value. Those samples say nothing about supply.
		{"a pause is not undersupply", append(rep(10, smp{9.8e6, 10e6}), rep(30, smp{0, 10e6})...), 0.98},
		// After a restart the rate is absent until re-measured.
		{"restart: no rate, no comparison", append(rep(6, smp{5e6, 10e6}), rep(10, smp{9e6, -1})...), 0.5},
		{"§2", rep(20, smp{5.05e6, 10.7e6}), 5.05 / 10.7},
		{"too few flowing samples", append(rep(4, smp{5e6, 10e6}), rep(20, smp{0, 10e6})...), 0},
		{"not in deep mode", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := DeriveGoodput(build(c.ss)).Facts()
			got, ok := facts["dvrSupplyRatio"]
			if c.ratio == 0 {
				if ok {
					t.Errorf("dvrSupplyRatio = %v, want absent", got)
				}
				return
			}
			if !ok || !near(got, c.ratio) {
				t.Errorf("dvrSupplyRatio = %v (%v), want %v", got, ok, c.ratio)
			}
			if _, ok := facts["dvrGoodputBps"]; !ok {
				t.Error("dvrGoodputBps (flowing median) absent beside the ratio")
			}
			if _, ok := facts["dvrLiveRateBps"]; !ok {
				t.Error("dvrLiveRateBps (flowing median) absent beside the ratio")
			}
		})
	}
}

// Review round 3: a starved window (bytes arriving, no complete frame) reads
// goodput 0 and is the viewer's real supply; only a PAUSE (dvrIdle) says
// nothing. An older client without dvrIdle keeps the old exclusion.
func TestDeriveSupplyRatioCountsStarvedWindows(t *testing.T) {
	series := func(n int, idle func(i int) any, goodput func(i int) float64) []Sample {
		out := make([]Sample, n)
		for i := range out {
			st := map[string]any{"dvrGoodputBps": goodput(i), "dvrLiveRateBps": 10e6}
			if v := idle(i); v != nil {
				st["dvrIdle"] = v
			}
			out[i] = Sample{TMs: float64(i * 2000), Stats: st}
		}
		return out
	}
	alternating := func(i int) float64 {
		if i%2 == 0 {
			return 0 // a keyframe still crawling in
		}
		return 8e6
	}
	cases := []struct {
		name    string
		samples []Sample
		ratio   float64 // 0: absent
	}{
		{"starved windows count", series(20, func(int) any { return false }, alternating), 0.4},
		// Odd count: a median would land on 0 or 0.8; the share does not.
		{"starved windows count, odd count", series(21, func(int) any { return false }, alternating), 80e6 / 210e6},
		{"a pause does not", series(40, func(i int) any { return i >= 10 }, func(i int) float64 {
			if i >= 10 {
				return 0
			}
			return 9.8e6
		}), 0.98},
		{"older client: zero windows excluded as before", series(20, func(int) any { return nil }, alternating), 0.8},
		{"all paused", series(20, func(int) any { return true }, func(int) float64 { return 0 }), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := DeriveGoodput(c.samples).Facts()["dvrSupplyRatio"]
			if c.ratio == 0 {
				if ok {
					t.Errorf("dvrSupplyRatio = %v, want absent", got)
				}
				return
			}
			if !ok || !near(got, c.ratio) {
				t.Errorf("dvrSupplyRatio = %v (%v), want %v", got, ok, c.ratio)
			}
		})
	}
}
