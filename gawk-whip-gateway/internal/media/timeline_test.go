package media

import (
	"math"
	"testing"
	"time"
)

func ntpAt(baseSec float64, t time.Duration) uint64 {
	v := baseSec + t.Seconds()
	sec := math.Floor(v)
	return uint64(sec)<<32 | uint64((v-sec)*(1<<32))
}

// WG6: "A/V offset within 20 ms of the source". Video and audio start at
// unrelated RTP offsets and reach the gateway with different path delays
// (video 60 ms later, as behind a reorder window); the sender reports map
// both onto one timeline. Without them the audio would sit 60 ms early.
func TestAVOffsetWithin20msOfSource(t *testing.T) {
	var tl Timeline
	const vOff, aOff = uint32(3_000_000_000), uint32(123_456)
	const ntpBase = 3_900_000_000.0
	videoDelay, audioDelay := 80*time.Millisecond, 20*time.Millisecond
	engineAt := func(src time.Duration, delay time.Duration) uint64 {
		return uint64((src + delay + 10*time.Second).Microseconds())
	}
	vRTP := func(src time.Duration) uint32 { return vOff + uint32(src.Seconds()*VideoClockRate) }
	aRTP := func(src time.Duration) uint32 { return aOff + uint32(src.Seconds()*AudioClockRate) }

	var maxSkewBefore, maxSkewAfter time.Duration
	for src := time.Duration(0); src < 5*time.Second; src += 20 * time.Millisecond {
		if src == time.Second {
			tl.VideoSenderReport(vRTP(src), ntpAt(ntpBase, src))
			tl.AudioSenderReport(aRTP(src), ntpAt(ntpBase, src))
		}
		v := tl.Video(vRTP(src), engineAt(src, videoDelay))
		a := tl.Audio(aRTP(src), engineAt(src, audioDelay))
		skew := time.Duration(int64(v)-int64(a)) * time.Microsecond
		if skew < 0 {
			skew = -skew
		}
		if src < time.Second {
			maxSkewBefore = max(maxSkewBefore, skew)
		} else {
			maxSkewAfter = max(maxSkewAfter, skew)
		}
	}
	if maxSkewAfter > 20*time.Millisecond {
		t.Fatalf("A/V skew %v after the sender reports, want ≤ 20 ms", maxSkewAfter)
	}
	if maxSkewBefore < 40*time.Millisecond {
		t.Fatalf("test isn't exercising the path-delay difference (skew before SRs %v)", maxSkewBefore)
	}
	if !tl.Synced() {
		t.Fatal("not synced")
	}
}

func TestVideoTimelineFollowsRTPClockAcrossWrap(t *testing.T) {
	var tl Timeline
	ts := uint32(math.MaxUint32 - 1500)
	first := tl.Video(ts, 1_000_000)
	got := tl.Video(ts+90000, 9_999_999) // arrival time ignored after the anchor
	if got-first != 1_000_000 {
		t.Fatalf("one second of RTP = %d µs", got-first)
	}
}
