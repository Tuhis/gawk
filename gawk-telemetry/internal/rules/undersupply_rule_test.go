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
