package rollup

import "github.com/Tuhis/gawk/gawk-telemetry/internal/schema"

// Goodput derivation (R87, docs/91 §8 SG3): what a viewer actually received
// per second, overall and per connection by stripe width. One function for
// both producers — the read path runs it over the session's samples, the
// live projection over its rolling window — so the two cannot disagree about
// what goodput means.

// GoodputFields are the stats fields the derivation reads. The live window
// keeps only these.
var GoodputFields = []string{"videoBytesReceived", "dvrStripeWidth", "connectionBytesReceived"}

// GoodputFactNames are every fact Goodput.Facts can emit, so a producer can
// clear the ones a later window cannot support.
var GoodputFactNames = []string{
	"videoGoodputBps",
	"dvrStripeWidthMax", "stripeWidth1GoodputBps", "stripeMaxWidthGoodputBps", "stripeMaxWidthConnMinBps",
}

// MinGoodputSpanMs is the least measured time a rate is reported over. A
// 0.5 s GOP's keyframe is about half a GOP's bytes, so a rate over a couple
// of seconds is mostly a statement about where the keyframes fell.
const MinGoodputSpanMs = 10_000

// Goodput is one window's derived rates. Zero-valued parts are absent: each
// is reported only when its span reached MinGoodputSpanMs.
type Goodput struct {
	// VideoBps is videoBytesReceived's rate over every interval in which the
	// counter did not go backwards (a reconnect restarts it).
	VideoBps float64
	VideoOK  bool
	// The striped deep-buffer view: intervals bucketed by the width in force
	// at both ends, with the same number of connections reporting.
	WidthMax int
	// Width1Bps is the aggregate while unstriped; MaxWidthBps the aggregate
	// at WidthMax, and MaxWidthConnMinBps its slowest connection's rate.
	Width1Bps, MaxWidthBps, MaxWidthConnMinBps float64
	StripeOK                                   bool
}

type widthBucket struct {
	ms      float64
	perConn []float64 // bytes per connection index
}

// DeriveGoodput computes the rates over samples, in time order.
func DeriveGoodput(samples []Sample) Goodput {
	var g Goodput
	var videoBytes, videoMs float64
	buckets := map[int]*widthBucket{}
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		dt := b.TMs - a.TMs
		if dt <= 0 {
			continue
		}
		va, okA := schema.Number(a.Stats, "videoBytesReceived")
		vb, okB := schema.Number(b.Stats, "videoBytesReceived")
		if okA && okB && vb >= va {
			videoBytes += vb - va
			videoMs += dt
		}

		wa, okWA := schema.Number(a.Stats, "dvrStripeWidth")
		wb, okWB := schema.Number(b.Stats, "dvrStripeWidth")
		ca, okCA := schema.Numbers(a.Stats, "connectionBytesReceived")
		cb, okCB := schema.Numbers(b.Stats, "connectionBytesReceived")
		w := int(wb)
		if !okWA || !okWB || wa != wb || w < 1 || !okCA || !okCB || len(ca) != w || len(cb) != w {
			continue
		}
		deltas := make([]float64, w)
		valid := true
		for j := range w {
			if cb[j] < ca[j] {
				valid = false
				break
			}
			deltas[j] = cb[j] - ca[j]
		}
		if !valid {
			continue
		}
		bk := buckets[w]
		if bk == nil {
			bk = &widthBucket{perConn: make([]float64, w)}
			buckets[w] = bk
		}
		bk.ms += dt
		for j, d := range deltas {
			bk.perConn[j] += d
		}
	}
	if videoMs >= MinGoodputSpanMs {
		g.VideoBps, g.VideoOK = videoBytes*8/(videoMs/1000), true
	}

	one := buckets[1]
	if one == nil || one.ms < MinGoodputSpanMs {
		return g
	}
	g.WidthMax = 1
	for w, bk := range buckets {
		if bk.ms >= MinGoodputSpanMs && w > g.WidthMax {
			g.WidthMax = w
		}
	}
	rate := func(bytes, ms float64) float64 { return bytes * 8 / (ms / 1000) }
	g.Width1Bps = rate(one.perConn[0], one.ms)
	top := buckets[g.WidthMax]
	var total float64
	for j, bytes := range top.perConn {
		total += bytes
		if r := rate(bytes, top.ms); j == 0 || r < g.MaxWidthConnMinBps {
			g.MaxWidthConnMinBps = r
		}
	}
	g.MaxWidthBps = rate(total, top.ms)
	g.StripeOK = true
	return g
}

// Facts renders the rates under their fact names, absent parts omitted.
func (g Goodput) Facts() map[string]float64 {
	out := map[string]float64{}
	if g.VideoOK {
		out["videoGoodputBps"] = g.VideoBps
	}
	if g.StripeOK {
		out["dvrStripeWidthMax"] = float64(g.WidthMax)
		out["stripeWidth1GoodputBps"] = g.Width1Bps
		out["stripeMaxWidthGoodputBps"] = g.MaxWidthBps
		out["stripeMaxWidthConnMinBps"] = g.MaxWidthConnMinBps
	}
	return out
}

// TrimGoodputSample keeps only what DeriveGoodput reads, for a window that
// holds samples rather than streaming past them.
func TrimGoodputSample(s Sample) Sample {
	keep := make(map[string]any, len(GoodputFields))
	for _, f := range GoodputFields {
		if v, ok := s.Stats[f]; ok {
			keep[f] = v
		}
	}
	return Sample{TMs: s.TMs, Stats: keep}
}
