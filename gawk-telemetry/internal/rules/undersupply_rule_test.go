package rules

import (
	"strings"
	"testing"
)

// docs/91 §2's session as diagnose() saw it: a Most stable (DVR) viewer on a
// mobile link, 5.05 Mbit/s of goodput against a 10.7 Mbit/s stream, the
// playout offset pinned at the 3000 ms DVR floor, 622 resyncs and the frame
// rate under half its baseline for 58 % of the window.
func section2Session() *Facts {
	f := viewerFacts()
	f.SetText("deliveryMode", "dvr")
	f.SetClient("playoutOffsetMs", 3000)
	f.SetClient("dvrBufferMs", 3000)
	f.SetRelay("dvrBufferMs", 3000)
	f.SetClient("videoGoodputBps", 5.05e6)
	f.SetRelay("streamBitrateBps", 10.7e6)
	// 1080p60: the parity allowance is per relayed frame.
	f.SetRelay("framesRelayedPerSec", 60)
	f.SetClient("fpsDipShare", 0.58)
	f.SetRelay("dvrResyncs", 622)
	return f
}

// The same mode, healthy: the offset sits at the DVR floor by design (the
// deep profile's min IS its buffer), and the link carries the stream.
func healthyDVRSession() *Facts {
	f := viewerFacts()
	f.SetText("deliveryMode", "dvr")
	f.SetClient("playoutOffsetMs", 3000)
	f.SetClient("dvrBufferMs", 3000)
	f.SetRelay("dvrBufferMs", 3000)
	f.SetClient("videoGoodputBps", 11.2e6)
	f.SetRelay("streamBitrateBps", 11.3e6)
	f.SetRelay("framesRelayedPerSec", 60)
	f.SetClient("fpsDipShare", 0)
	f.SetRelay("dvrResyncs", 0)
	return f
}

// docs/91 §2's side finding: the rule compared every DVR viewer's offset
// against the RESILIENT clamp (2000 ms), below the deep profile's own floor,
// so it fired on every deep-buffer session whatever its health.
func TestResilientUndersupplySilentOnHealthyDVRSession(t *testing.T) {
	if got := findingByID(Evaluate(healthyDVRSession(), Playbook()), "resilient-undersupply"); got != nil {
		t.Fatalf("fired on a healthy DVR session: %+v", got)
	}
}

// And the §2 session still fires, for the reason that is true of it: the
// goodput, not the offset (which a DVR viewer always has pinned).
func TestResilientUndersupplyFiresOnSection2Goodput(t *testing.T) {
	got := findingByID(Evaluate(section2Session(), Playbook()), "resilient-undersupply")
	if got == nil {
		t.Fatal("the §2 session did not fire")
	}
	var goodput, stream bool
	for _, e := range got.Evidence {
		switch e.Signal {
		case "videoGoodputBps":
			goodput = e.From == FromClient && e.Value == 5.05e6
		case "streamBitrateBps":
			stream = e.From == FromRelay && e.Value == 10.7e6
		}
	}
	if !goodput || !stream {
		t.Errorf("evidence must cite goodput against the stream bitrate: %+v", got.Evidence)
	}
	if got.Confidence <= clientOnlyConfidenceCap {
		t.Errorf("confidence %v: the stream bitrate is relay-counted, so the finding is anchored", got.Confidence)
	}
}

func TestResilientUndersupplyConditions(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(f *Facts)
		fires bool
	}{
		{"§2 session", func(*Facts) {}, true},
		{"DVR, link carries the stream", func(f *Facts) { f.SetClient("videoGoodputBps", 10.6e6) }, false},
		{"DVR, a deeper buffer still judged on goodput", func(f *Facts) {
			f.SetRelay("dvrBufferMs", 8000)
			f.SetClient("dvrBufferMs", 8000)
			f.SetClient("playoutOffsetMs", 8000)
		}, true},
		{"DVR, offset under its own floor is not pinned", func(f *Facts) {
			f.SetRelay("dvrBufferMs", 8000)
			f.SetClient("playoutOffsetMs", 3000)
		}, false},
		{"DVR, no goodput: cannot judge supply", func(f *Facts) {
			g := viewerFacts()
			g.SetText("deliveryMode", "dvr")
			g.SetClient("playoutOffsetMs", 3000)
			g.SetRelay("streamBitrateBps", 10.7e6)
			*f = *g
		}, false},
		{"reliable, pinned at 2000 and undersupplied", func(f *Facts) {
			f.SetText("deliveryMode", "reliable")
			f.SetClient("playoutOffsetMs", 2000)
			f.SetClient("dvrBufferMs", 0)
			delete(f.relay, "dvrBufferMs")
		}, true},
		{"reliable, pinned at 2000 but the link carries the stream", func(f *Facts) {
			f.SetText("deliveryMode", "reliable")
			f.SetClient("playoutOffsetMs", 2000)
			f.SetClient("videoGoodputBps", 10.6e6)
			delete(f.relay, "dvrBufferMs")
		}, false},
		{"reliable, undersupplied but the buffer has room", func(f *Facts) {
			f.SetText("deliveryMode", "reliable")
			f.SetClient("playoutOffsetMs", 800)
			delete(f.relay, "dvrBufferMs")
		}, false},
		{"live edge never fires", func(f *Facts) { f.SetText("deliveryMode", "datagrams") }, false},
	}
	rule := ruleByID(t, "resilient-undersupply")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := section2Session()
			c.edit(f)
			if got := rule.Eval(f) != nil; got != c.fires {
				t.Errorf("fired = %v, want %v", got, c.fires)
			}
		})
	}
}

// When the relay's own path stats show a loss-limited sender, "the link
// cannot carry the stream" is the wrong conclusion: the verdict must name the
// connection instead, and point at striping.
func TestResilientUndersupplyDefersToAPerConnectionCeiling(t *testing.T) {
	f := section2Session()
	f.SetRelay("pathSrttMs", 36)
	f.SetRelay("pathMinRttMs", 33)
	f.SetRelay("pathLossRatio", 0.011)
	got := ruleByID(t, "resilient-undersupply").Eval(f)
	if got == nil {
		t.Fatal("did not fire")
	}
	if strings.Contains(got.Verdict, "the link cannot") || !strings.Contains(got.Verdict, "connection") {
		t.Errorf("verdict still blames the link: %q", got.Verdict)
	}
	if !strings.Contains(got.Action, "per-connection-ceiling") {
		t.Errorf("action should point at the per-connection rule: %q", got.Action)
	}
}

// Review round 1: the relay's ingress bitrate carries audio, config and every
// parity symbol, none of which a viewer's videoBytesReceived counts. Judged
// against it raw, a healthy viewer reads as undersupplied.
func TestUndersupplyIsJudgedAgainstVideoOnly(t *testing.T) {
	reliable := func(goodput, ingress, fps float64) *Facts {
		f := viewerFacts()
		f.SetText("deliveryMode", "reliable")
		f.SetClient("playoutOffsetMs", 2000)
		f.SetClient("videoGoodputBps", goodput)
		f.SetRelay("streamBitrateBps", ingress)
		if fps > 0 {
			f.SetRelay("framesRelayedPerSec", fps)
		}
		return f
	}
	cases := []struct {
		name  string
		f     *Facts
		fires bool
	}{
		// 360 kbps of video beside 128 kbps of Opus: 0.74x the ingress.
		{"low-rate stream with audio", reliable(360e3, 488e3, 30), false},
		// A viewer opted down to parity 0 still has the publisher's two
		// symbols a frame in the relay's ingress: 2 Mbit/s of video, 2.7 in.
		{"parity opted down", reliable(2.0e6, 2.0e6+2*1200*8*30+128e3, 30), false},
		// Without a frame rate the parity share cannot be bounded: no verdict.
		{"no frame rate, no client rate", reliable(2.0e6, 6e6, 0), false},
		// A real shortfall still fires through the overhead allowance.
		{"reliable, real shortfall", reliable(3e6, 8e6, 30), true},
	}
	// Deep mode: the client's own live video rate is the comparison.
	dvr := func(goodput, live, ingress float64) *Facts {
		f := healthyDVRSession()
		f.SetClient("videoGoodputBps", goodput)
		f.SetClient("dvrLiveRateBps", live)
		f.SetRelay("streamBitrateBps", ingress)
		return f
	}
	cases = append(cases,
		struct {
			name  string
			f     *Facts
			fires bool
		}{"DVR keeping up with its live rate, ingress far above", dvr(4e6, 4.1e6, 6e6), false},
		struct {
			name  string
			f     *Facts
			fires bool
		}{"DVR under its live rate", dvr(5.05e6, 10.2e6, 10.7e6), true},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, id := range []string{"resilient-undersupply", "per-connection-ceiling"} {
				f := c.f
				if id == "per-connection-ceiling" {
					f.SetRelay("pathSrttMs", 41)
					f.SetRelay("pathMinRttMs", 40)
					f.SetRelay("pathLossRatio", 0.01)
				}
				if got := findingByID(Evaluate(f, Playbook()), id) != nil; got != c.fires {
					t.Errorf("%s fired = %v, want %v", id, got, c.fires)
				}
			}
		})
	}
}
