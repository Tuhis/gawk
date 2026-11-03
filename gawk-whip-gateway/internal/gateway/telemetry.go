package gateway

import (
	"regexp"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-server/publisher"
)

// whipStats is what only the gateway sees: the OBS → gateway leg (WG11).
// The engine's Stats cover the gateway → relay leg like any native
// broadcaster's; these say what reached the engine and what did not, and
// why. Every name is lowerCamelCase with a `whip` prefix, so it can never
// collide with a browser or native field of another meaning (gawk-telemetry
// matches on the names, docs/33 D15).
type whipStats struct {
	// Sender is the coarse class of the WHIP client ("OBS 32.0",
	// "FFmpeg", "GStreamer", "WHIP client"), never its user-agent string.
	Sender string `json:"whipSender,omitempty"`

	// Video RTP sequence numbers seen (first to newest) and given up on.
	PacketsExpected uint64 `json:"whipPacketsExpected"`
	PacketsLost     uint64 `json:"whipPacketsLost"`
	BytesReceived   uint64 `json:"whipBytesReceived"`

	// D6's retransmit accounting: NACKed sequence numbers, and of those the
	// ones that filled their hole in time and the ones that came back after
	// the window had dropped their frame. NACKs in neither were never
	// answered.
	NACKsSent            uint64 `json:"whipNacksSent"`
	RetransmitsRecovered uint64 `json:"whipRetransmitsRecovered"`
	RetransmitsLate      uint64 `json:"whipRetransmitsLate"`
	// RetransmitMaxMs is the slowest NACK → arrival since the previous
	// sample (0: none): the window this interval would have needed.
	RetransmitMaxMs float64 `json:"whipRetransmitMaxMs"`

	// The window D6 is applying now, and the smoothed RTT it is sized from
	// (meaningful once RttAvailable: before the first sample it is the
	// configured initial RTT).
	RTTAvailable bool    `json:"whipRttAvailable"`
	RTTMs        float64 `json:"whipRttMs"`
	WindowMs     float64 `json:"whipWindowMs"`

	// Frames not republished, by reason: the window closed on a hole; a
	// delta after a loss, before the next IDR (this is the freeze viewers
	// see); the engine fell behind.
	FramesWindowDropped       uint64 `json:"whipFramesWindowDropped"`
	FramesIDRWaitDropped      uint64 `json:"whipFramesIdrWaitDropped"`
	FramesBackpressureDropped uint64 `json:"whipFramesBackpressureDropped"`

	// GOPMs is D7's measured keyframe interval (median), 0 until measured.
	GOPMs float64 `json:"whipGopMs"`
}

// telemetrySample is one R28 sample: the engine's stats and the gateway's,
// flat in one object (encoding/json flattens embedded structs).
type telemetrySample struct {
	publisher.Stats
	whipStats
}

// dropCounts are one session's frames not republished, by reason. Loop-owned.
type dropCounts struct {
	window, idrWait, backpressure uint64
}

// reportTelemetry hands the reporter one sample. Loop-owned: it reads the
// pipeline. The reporter decimates to the relay's cadence, so the slowest
// retransmit is carried until a sample is actually taken rather than lost
// with a skipped one.
func (ss *session) reportTelemetry() {
	if ss.rep == nil {
		return
	}
	ro := ss.pipe.Reorder()
	if d := ro.TakeRetransmitMax(); d > ss.retransmitMax {
		ss.retransmitMax = d
	}
	c := ss.pipe.Counters()
	w := whipStats{
		Sender:                    ss.sender,
		PacketsExpected:           c.Expected,
		PacketsLost:               c.Lost,
		BytesReceived:             c.Bytes,
		NACKsSent:                 ro.NACKsSent(),
		RetransmitsRecovered:      ro.RetransmitsRecovered(),
		RetransmitsLate:           ro.RetransmitsLate(),
		RetransmitMaxMs:           msOf(ss.retransmitMax),
		RTTAvailable:              ro.RTT().Samples() > 0,
		RTTMs:                     msOf(ro.RTT().RTT()),
		WindowMs:                  msOf(ro.Window()),
		FramesWindowDropped:       ss.drops.window,
		FramesIDRWaitDropped:      ss.drops.idrWait,
		FramesBackpressureDropped: ss.drops.backpressure,
		GOPMs:                     msOf(ss.pipe.GOP().Median()),
	}
	ss.mu.Lock()
	eng := ss.engStats
	ss.mu.Unlock()
	if ss.rep.Report(telemetrySample{Stats: eng, whipStats: w}) {
		ss.retransmitMax = 0
	}
}

func msOf(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

var obsUA = regexp.MustCompile(`OBS-Studio/(\d+)\.(\d+)`)

// senderClass reduces a WHIP client's User-Agent to a coarse client class
// and OS class (docs/33 D8: never the raw string). OBS sends
// "Mozilla/5.0 (OBS-Studio/<version>; <OS> <arch>; <locale>)"; FFmpeg's
// whip muxer "Lavf/<version>"; GStreamer's whipsink "GStreamer/<version>".
func senderClass(ua string) (client, os string) {
	switch m := obsUA.FindStringSubmatch(ua); {
	case m != nil:
		client = "OBS " + m[1] + "." + m[2]
	case strings.HasPrefix(ua, "Lavf/") || strings.Contains(ua, "FFmpeg"):
		client = "FFmpeg"
	case strings.Contains(ua, "GStreamer"):
		client = "GStreamer"
	default:
		client = "WHIP client"
	}
	switch {
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS") || strings.Contains(ua, "macOS"):
		os = "macOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	return client, os
}
