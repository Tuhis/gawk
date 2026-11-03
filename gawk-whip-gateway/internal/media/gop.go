package media

import (
	"sort"
	"time"
)

// VideoClockRate is H.264's RTP clock (RFC 6184).
const VideoClockRate = 90000

// GOPConfig holds D7's bounds (owner decision OD5: as knobs). Provisional
// until WG0.
type GOPConfig struct {
	// WarnAbove: a median spacing above it warns (gop_long) (1.1 s).
	WarnAbove time.Duration
	// EndAbove: a median spacing above it ends the session (gop_too_long)
	// (2.2 s).
	EndAbove time.Duration
	// EarlyGap: before Samples spacings exist, two consecutive gaps longer
	// than it end the session (4.5 s). It can't come down without letting
	// one lost IDR start end a stream at an allowed GOP.
	EarlyGap time.Duration
	// Samples: the median is over the last this many spacings (5).
	Samples int
}

// DefaultGOPConfig is D7's provisional default (until WG0).
func DefaultGOPConfig() GOPConfig {
	return GOPConfig{
		WarnAbove: 1100 * time.Millisecond,
		EndAbove:  2200 * time.Millisecond,
		EarlyGap:  4500 * time.Millisecond,
		Samples:   5,
	}
}

// GOPVerdict is the meter's judgement.
type GOPVerdict int

const (
	// GOPUnknown: not enough spacings to judge, and no early end.
	GOPUnknown GOPVerdict = iota
	// GOPOK: the median spacing is within WarnAbove.
	GOPOK
	// GOPWarn: within EndAbove; gop_long.
	GOPWarn
	// GOPEnd: end the session with gop_too_long.
	GOPEnd
)

// GOPMeter measures the keyframe interval from the RTP timestamps of IDR
// starts, recorded on arrival (PacketCarriesIDR), before D6 can drop the
// frame (D7). Timestamps are kept sorted, so a retransmitted IDR packet
// arriving after a later one lands in place, and deduplicated per frame.
type GOPMeter struct {
	cfg GOPConfig

	started bool
	maxTS   int64 // extended RTP timestamp of the newest packet
	firstTS int64 // the session's first packet: the early rule's first edge
	idrs    []int64
	// full is set once Samples spacings have existed: the median rule
	// governs from then on.
	full bool
}

// NewGOPMeter builds a meter.
func NewGOPMeter(cfg GOPConfig) *GOPMeter { return &GOPMeter{cfg: cfg} }

func (g *GOPMeter) extend(ts uint32) int64 {
	return g.maxTS + int64(int32(ts-uint32(g.maxTS)))
}

// Packet records one arriving video packet's timestamp, and whether it
// carries IDR data.
func (g *GOPMeter) Packet(ts uint32, idr bool) {
	if !g.started {
		g.started = true
		g.maxTS, g.firstTS = int64(ts), int64(ts)
	}
	ext := g.extend(ts)
	if ext > g.maxTS {
		g.maxTS = ext
	}
	if !idr {
		return
	}
	i := sort.Search(len(g.idrs), func(i int) bool { return g.idrs[i] >= ext })
	if i < len(g.idrs) && g.idrs[i] == ext {
		return
	}
	if i == 0 && len(g.idrs) > g.cfg.Samples {
		return // older than everything kept: too late to matter
	}
	g.idrs = append(g.idrs, 0)
	copy(g.idrs[i+1:], g.idrs[i:])
	g.idrs[i] = ext
	if keep := g.cfg.Samples + 1; len(g.idrs) > keep && g.full {
		g.idrs = g.idrs[len(g.idrs)-keep:]
	}
	if len(g.idrs)-1 >= g.cfg.Samples {
		g.full = true
		if keep := g.cfg.Samples + 1; len(g.idrs) > keep {
			g.idrs = g.idrs[len(g.idrs)-keep:]
		}
	}
}

func ticks(d time.Duration) int64 { return int64(d) * VideoClockRate / int64(time.Second) }

func toDuration(t int64) time.Duration { return time.Duration(t * int64(time.Second) / VideoClockRate) }

// spacings returns the last (up to Samples) IDR spacings.
func (g *GOPMeter) spacings() []int64 {
	var sp []int64
	for i := 1; i < len(g.idrs); i++ {
		sp = append(sp, g.idrs[i]-g.idrs[i-1])
	}
	if len(sp) > g.cfg.Samples {
		sp = sp[len(sp)-g.cfg.Samples:]
	}
	return sp
}

// Median is the lower median of the last spacings (zero before two IDRs).
// The lower median, so with two or four samples one lost keyframe (one
// doubled spacing) doesn't move it.
func (g *GOPMeter) Median() time.Duration {
	sp := g.spacings()
	if len(sp) == 0 {
		return 0
	}
	sorted := append([]int64(nil), sp...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return toDuration(sorted[(len(sorted)-1)/2])
}

// Verdict judges the stream as of the newest packet.
func (g *GOPMeter) Verdict() GOPVerdict {
	if g.full {
		switch m := g.Median(); {
		case m > g.cfg.EndAbove:
			return GOPEnd
		case m > g.cfg.WarnAbove:
			return GOPWarn
		default:
			return GOPOK
		}
	}
	// The early rule: gaps between the session's first packet, each IDR
	// start, and the newest packet; two consecutive ones over EarlyGap end
	// it (OBS's "auto" GOP below ~55 fps).
	edges := append([]int64{g.firstTS}, g.idrs...)
	edges = append(edges, g.maxTS)
	limit := ticks(g.cfg.EarlyGap)
	prevLong := false
	for i := 1; i < len(edges); i++ {
		long := edges[i]-edges[i-1] > limit
		if long && prevLong {
			return GOPEnd
		}
		prevLong = long
	}
	return GOPUnknown
}
