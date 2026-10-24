package rollup

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// Memory is the native broadcaster's footprint over its whole session
// (R76, docs/77 D10): the figure the OS's task manager shows, summarized for
// the two questions the playbook asks of it — is it over budget, and did it
// grow.
//
// Built from the minute `memory` events when the session has them, because
// those outlive the byte budget that stops a native session's samples
// (about 5.4 hours at 16 MiB, D9, D12); from the samples otherwise. A growth figure
// needs MemoryMinSpanMs of timeline and is absent below it, never zero.
type Memory struct {
	// Source is "events" or "samples".
	Source string `json:"source"`
	Points int    `json:"points"`
	// SpanMs is first point to last.
	SpanMs float64 `json:"spanMs"`
	// MedianBytes is the footprint's median over the whole timeline.
	MedianBytes float64 `json:"medianBytes"`
	// PeakBytes is the highest footprint or reported peak seen.
	PeakBytes float64 `json:"peakBytes"`
	// EarlyBytes and LateBytes are the footprint's medians over the window
	// after warm-up and the session's last window; GrowthBytes is late minus
	// early, negative when it shrank.
	EarlyBytes  *float64 `json:"earlyBytes,omitempty"`
	LateBytes   *float64 `json:"lateBytes,omitempty"`
	GrowthBytes *float64 `json:"growthBytes,omitempty"`
}

const (
	// MemoryWarmupMs is skipped before the early window: a pipeline
	// allocating its pools in its first minutes is what starting looks like.
	MemoryWarmupMs = 2 * 60_000
	// MemoryWindowMs is the width of the early and late windows: five minute
	// events, or 150 samples at the fleet's 2 s cadence.
	MemoryWindowMs = 5 * 60_000
	// MemoryMinSpanMs is the shortest timeline a growth figure is computed
	// over: warm-up, both windows and ten minutes between them.
	MemoryMinSpanMs = 20 * 60_000
	// memoryMinWindowPoints is what a window needs to stand as a median.
	memoryMinWindowPoints = 3
)

type memPoint struct{ t, footprint, peak float64 }

// computeMemory returns nil for a session with no readable footprint — and
// for every viewer, which reports none.
func computeMemory(in Input) *Memory {
	if in.Role != "broadcaster" {
		return nil
	}
	pts, source := memoryEventPoints(in.Events), "events"
	if len(pts) < 2 {
		pts, source = memorySamplePoints(in.Samples), "samples"
	}
	if len(pts) == 0 {
		return nil
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].t < pts[j].t })

	all := make([]float64, len(pts))
	m := &Memory{Source: source, Points: len(pts)}
	for i, p := range pts {
		all[i] = p.footprint
		m.PeakBytes = math.Max(m.PeakBytes, math.Max(p.footprint, p.peak))
	}
	m.MedianBytes = median(all)
	start, end := pts[0].t, pts[len(pts)-1].t
	m.SpanMs = end - start
	if m.SpanMs < MemoryMinSpanMs {
		return m
	}
	early := window(pts, start+MemoryWarmupMs, start+MemoryWarmupMs+MemoryWindowMs)
	late := window(pts, end-MemoryWindowMs, end)
	if len(early) < memoryMinWindowPoints || len(late) < memoryMinWindowPoints {
		return m
	}
	e, l := median(early), median(late)
	g := l - e
	m.EarlyBytes, m.LateBytes, m.GrowthBytes = &e, &l, &g
	return m
}

// memoryEventPoints parses the engine's `memory` event details:
// `footprint=<bytes>,peak=<bytes>[,heap=…][,gpu=…][,commit=…]`. A detail
// without a readable, non-negative footprint is skipped.
func memoryEventPoints(events []Event) []memPoint {
	var out []memPoint
	for _, e := range events {
		if e.Kind != "memory" {
			continue
		}
		p := memPoint{t: e.TMs, footprint: -1}
		for _, kv := range strings.Split(e.Detail, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
				continue
			}
			switch k {
			case "footprint":
				p.footprint = n
			case "peak":
				p.peak = n
			}
		}
		if p.footprint >= 0 {
			out = append(out, p)
		}
	}
	return out
}

func memorySamplePoints(samples []Sample) []memPoint {
	var out []memPoint
	for _, s := range samples {
		fp, ok := schema.Number(s.Stats, "memFootprintBytes")
		if !ok || fp < 0 {
			continue
		}
		peak, _ := schema.Number(s.Stats, "memFootprintPeakBytes")
		out = append(out, memPoint{t: s.TMs, footprint: fp, peak: peak})
	}
	return out
}

// window is the footprints with t in [from, to].
func window(pts []memPoint, from, to float64) []float64 {
	var out []float64
	for _, p := range pts {
		if p.t >= from && p.t <= to {
			out = append(out, p.footprint)
		}
	}
	return out
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
