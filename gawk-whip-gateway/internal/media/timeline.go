package media

// AudioClockRate is Opus's RTP clock (RFC 7587: always 48 kHz).
const AudioClockRate = 48000

// Timeline maps RTP timestamps onto the engine's clock (microseconds), so
// the engine's TimestampUs carries the sender's capture cadence rather than
// network arrival jitter, and audio lines up with video (WG6: within 20 ms
// of the source).
//
// Video is anchored once, at its first frame: engine time = anchor time +
// (RTP − anchor RTP) / 90 kHz. Audio is mapped onto that same line through
// the sender's RTCP sender reports, which pair each stream's RTP clock with
// one shared NTP clock: audio RTP → sender NTP (audio SR) → video RTP (video
// SR) → engine time. Until both streams have reported, audio is anchored at
// its own first packet's arrival, which is off by the difference in the two
// streams' path delay; the first pair of SRs (OBS sends them about once a
// second) corrects it.
type Timeline struct {
	vExt, aExt unwrap32

	videoAnchored bool
	videoRTP      int64
	videoUs       uint64

	audioAnchored bool
	audioRTP      int64
	audioUs       uint64

	srV, srA *senderReport
}

type senderReport struct {
	rtp   int64 // extended, in that stream's clock
	ntpUs int64
}

type unwrap32 struct {
	started bool
	max     int64
}

func (u *unwrap32) ext(ts uint32) int64 {
	if !u.started {
		u.started, u.max = true, int64(ts)
		return u.max
	}
	e := u.max + int64(int32(ts-uint32(u.max)))
	if e > u.max {
		u.max = e
	}
	return e
}

// NTPToUs converts a 64-bit NTP timestamp (32.32 fixed point) to
// microseconds.
func NTPToUs(ntp uint64) int64 {
	sec := int64(ntp >> 32)
	frac := ntp & 0xffffffff
	return sec*1_000_000 + int64(frac*1_000_000>>32)
}

// VideoSenderReport records a video SR.
func (t *Timeline) VideoSenderReport(rtp uint32, ntp uint64) {
	t.srV = &senderReport{rtp: t.vExt.ext(rtp), ntpUs: NTPToUs(ntp)}
}

// AudioSenderReport records an audio SR.
func (t *Timeline) AudioSenderReport(rtp uint32, ntp uint64) {
	t.srA = &senderReport{rtp: t.aExt.ext(rtp), ntpUs: NTPToUs(ntp)}
}

// Video maps a video RTP timestamp; nowUs anchors the first.
func (t *Timeline) Video(ts uint32, nowUs uint64) uint64 {
	e := t.vExt.ext(ts)
	if !t.videoAnchored {
		t.videoAnchored, t.videoRTP, t.videoUs = true, e, nowUs
	}
	return addUs(t.videoUs, (e-t.videoRTP)*1_000_000/VideoClockRate)
}

// Synced reports whether audio is mapped through sender reports.
func (t *Timeline) Synced() bool { return t.videoAnchored && t.srV != nil && t.srA != nil }

// Audio maps an audio RTP timestamp; nowUs anchors it until it is synced.
func (t *Timeline) Audio(ts uint32, nowUs uint64) uint64 {
	e := t.aExt.ext(ts)
	if t.Synced() {
		ntpUs := t.srA.ntpUs + (e-t.srA.rtp)*1_000_000/AudioClockRate
		vRTP := t.srV.rtp + (ntpUs-t.srV.ntpUs)*VideoClockRate/1_000_000
		return addUs(t.videoUs, (vRTP-t.videoRTP)*1_000_000/VideoClockRate)
	}
	if !t.audioAnchored {
		t.audioAnchored, t.audioRTP, t.audioUs = true, e, nowUs
	}
	return addUs(t.audioUs, (e-t.audioRTP)*1_000_000/AudioClockRate)
}

func addUs(base uint64, d int64) uint64 {
	if d < 0 && uint64(-d) > base {
		return 0
	}
	return uint64(int64(base) + d)
}
