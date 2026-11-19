package rules

import (
	"strings"
	"testing"
)

// R87 SG3 (docs/91 §8): per-connection-ceiling on synthetic facts shaped like
// §11.1's lab rows. Signature (a) is the relay's path stats, (b) the client's
// per-width goodput in a striped deep-buffer session.
func TestPerConnectionCeiling(t *testing.T) {
	short := func(f *Facts) {
		f.SetClient("videoGoodputBps", 6.1e6)
		f.SetRelay("streamBitrateBps", 11.3e6)
		f.SetRelay("streamFramesPerSec", 60)
	}
	path := func(srtt, minRTT, loss float64) func(*Facts) {
		return func(f *Facts) {
			f.SetRelay("pathSrttMs", srtt)
			f.SetRelay("pathMinRttMs", minRTT)
			f.SetRelay("pathLossRatio", loss)
		}
	}
	stripe := func(width, one, agg, slowest float64) func(*Facts) {
		return func(f *Facts) {
			f.SetClient("dvrStripeWidthMax", width)
			f.SetClient("stripeWidth1GoodputBps", one)
			f.SetClient("stripeMaxWidthGoodputBps", agg)
			f.SetClient("stripeMaxWidthConnMinBps", slowest)
		}
	}
	cases := []struct {
		name    string
		edits   []func(*Facts)
		fires   bool
		verdict string // substring, when it fires
		minConf float64
	}{
		// (a) — §11.1, 40 ms at 1 % loss: RTT at its minimum, loss present.
		{"loss-limited sender", []func(*Facts){short, path(41, 40, 0.0107)}, true, "not a full link", 0.8},
		// RTT within 25 % of the minimum still reads as unqueued.
		{"RTT at the inflation bound", []func(*Facts){short, path(50, 40, 0.005)}, true, "not a full link", 0.8},
		// §11.1's 8 Mbit/s link: 150-220 ms over 40, loss from the queue.
		{"full link: RTT inflated 4-5x", []func(*Facts){short, path(180, 40, 0.0195)}, false, "", 0},
		{"just past the inflation bound", []func(*Facts){short, path(51, 40, 0.005)}, false, "", 0},
		{"no loss: not loss-limited", []func(*Facts){short, path(41, 40, 0)}, false, "", 0},
		{"path stats absent", []func(*Facts){short}, false, "", 0},
		// The stream fits: nothing to explain, whatever the path shows.
		{"goodput carries the stream", []func(*Facts){path(41, 40, 0.002), func(f *Facts) {
			f.SetClient("videoGoodputBps", 11.2e6)
			f.SetRelay("streamBitrateBps", 11.3e6)
		}}, false, "", 0},

		// (b) — §11.1 at 1 % loss, K = 1 → 3: 6.1 → 18.6 total, each ~6.
		{"striping scaled per connection", []func(*Facts){short, stripe(3, 6.1e6, 18.6e6, 6.0e6)}, true, "striping added", 0.6},
		// The full link: the total stays at 7.6 and each connection falls to a share.
		{"striping split a fixed total", []func(*Facts){short, stripe(3, 7.6e6, 7.6e6, 2.4e6)}, false, "", 0},
		{"aggregate rose but one connection collapsed", []func(*Facts){short, stripe(2, 6e6, 9.5e6, 3.5e6)}, false, "", 0},
		{"never striped", []func(*Facts){short, stripe(1, 6e6, 6e6, 6e6)}, false, "", 0},
		// Both signatures together are the strongest case.
		{"both signatures", []func(*Facts){short, path(36, 33, 0.011), stripe(2, 5.05e6, 10.1e6, 5.0e6)}, true, "not a full link", 0.9},
	}
	rule := ruleByID(t, "per-connection-ceiling")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := viewerFacts()
			for _, e := range c.edits {
				e(f)
			}
			got := findingByID(Evaluate(f, []Rule{rule}), rule.ID)
			if (got != nil) != c.fires {
				t.Fatalf("fired = %v, want %v: %+v", got != nil, c.fires, got)
			}
			if got == nil {
				return
			}
			if !strings.Contains(got.Verdict, c.verdict) {
				t.Errorf("verdict %q, want it to contain %q", got.Verdict, c.verdict)
			}
			if got.Confidence < c.minConf {
				t.Errorf("confidence %v, want at least %v", got.Confidence, c.minConf)
			}
			if !strings.Contains(got.Action, "Striping") && !strings.Contains(got.Action, "rung") {
				t.Errorf("action must point at striping or a lower rung: %q", got.Action)
			}
		})
	}
}

// Signature (b) rests on client testimony alone, so D7 caps it however clean
// the numbers look; (a) is the relay's own counters and is not capped.
func TestPerConnectionCeilingProvenance(t *testing.T) {
	f := viewerFacts()
	f.SetClient("videoGoodputBps", 6.1e6)
	f.SetRelay("streamBitrateBps", 11.3e6)
	f.SetRelay("streamFramesPerSec", 60)
	f.SetClient("dvrStripeWidthMax", 3)
	f.SetClient("stripeWidth1GoodputBps", 6.1e6)
	f.SetClient("stripeMaxWidthGoodputBps", 18.6e6)
	f.SetClient("stripeMaxWidthConnMinBps", 6e6)
	got := findingByID(Evaluate(f, Playbook()), "per-connection-ceiling")
	if got == nil {
		t.Fatal("did not fire")
	}
	// The stream bitrate is relay-counted, so even (b) is anchored.
	if got.Confidence != 0.6 {
		t.Errorf("confidence %v, want the rule's own 0.6 for the client-side signature", got.Confidence)
	}
}

// The §2 session with the relay's path stats this branch now scrapes: the
// TimeSync RTT stayed 26-73 ms, which §11.1 reads as loss, not a full link.
func TestPerConnectionCeilingOnSection2(t *testing.T) {
	f := section2Session()
	f.SetRelay("pathSrttMs", 36)
	f.SetRelay("pathMinRttMs", 33)
	f.SetRelay("pathLossRatio", 0.011)
	rep := Evaluate(f, Playbook())
	if findingByID(rep, "per-connection-ceiling") == nil {
		t.Fatalf("per-connection-ceiling did not fire on §2 with a loss-limited path: %+v", rep.Findings)
	}
	if findingByID(Evaluate(healthyDVRSession(), Playbook()), "per-connection-ceiling") != nil {
		t.Error("fired on a healthy DVR session")
	}
}
