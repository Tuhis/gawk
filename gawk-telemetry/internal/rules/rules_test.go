package rules

import (
	"strings"
	"testing"
)

func viewerFacts() *Facts { return NewFacts("sess", "session", "viewer") }
func castFacts() *Facts   { return NewFacts("sess", "session", "broadcaster") }
func bcastFacts() *Facts  { return NewFacts("1a2b3c4d5e6f", "broadcast", "") }

func findingByID(rep Report, id string) *Finding {
	for i := range rep.Findings {
		if rep.Findings[i].ID == id {
			return &rep.Findings[i]
		}
	}
	return nil
}

// TM6's criterion: EVERY transcribed playbook row has a synthetic session that
// fires it and one that does not. The table below is that pairing, one entry
// per rule — so a rule added to Playbook() without a case here shows up in the
// coverage check at the bottom.
func TestEveryPlaybookRowFiresAndDoesNot(t *testing.T) {
	cases := []struct {
		id    string
		fires func() *Facts
		quiet func() *Facts
	}{
		{
			id: "leg-b-single-viewer",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetRelay("subscriberDropped", 900)
				f.SetRelay("peerMedianDropped", 10)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
			quiet: func() *Facts {
				// Everyone is dropping equally: that is row 3, not row 1.
				f := viewerFacts()
				f.SetRelay("subscriberDropped", 900)
				f.SetRelay("peerMedianDropped", 850)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
		},
		{
			id: "configured-bandwidth-cap",
			fires: func() *Facts {
				f := bcastFacts()
				f.SetRelay("bandwidthDroppedDatagrams", 4200)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
			quiet: func() *Facts {
				f := bcastFacts()
				f.SetRelay("bandwidthDroppedDatagrams", 0)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
		},
		{
			id: "relay-egress-saturation",
			fires: func() *Facts {
				f := bcastFacts()
				f.SetRelay("subscribersDropping", 6)
				f.SetRelay("subscribers", 6)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
			quiet: func() *Facts {
				f := bcastFacts()
				f.SetRelay("subscribersDropping", 1)
				f.SetRelay("subscribers", 6)
				f.SetRelay("ingressLossRatio", 0)
				return f
			},
		},
		{
			id: "leg-a-broadcaster-uplink",
			fires: func() *Facts {
				f := bcastFacts()
				f.SetRelay("ingressLossRatio", 0.04)
				return f
			},
			quiet: func() *Facts {
				f := bcastFacts()
				f.SetRelay("ingressLossRatio", 0.0001)
				return f
			},
		},
		{
			id: "encoder-overload",
			fires: func() *Facts {
				f := castFacts()
				f.SetClient("captureFps", 60)
				f.SetClient("encoderFps", 22)
				return f
			},
			quiet: func() *Facts {
				f := castFacts()
				f.SetClient("captureFps", 60)
				f.SetClient("encoderFps", 59)
				return f
			},
		},
		{
			id: "send-path-gap",
			fires: func() *Facts {
				f := castFacts()
				f.SetClient("encoderFps", 60)
				f.SetClient("sentFps", 20)
				return f
			},
			quiet: func() *Facts {
				f := castFacts()
				f.SetClient("encoderFps", 60)
				f.SetClient("sentFps", 58)
				return f
			},
		},
		{
			id: "decoder-choking",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetClient("receivedFps", 60)
				f.SetClient("decoderFps", 18)
				f.SetClient("isHardwareAccelerated", 0)
				return f
			},
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("receivedFps", 60)
				f.SetClient("decoderFps", 59)
				return f
			},
		},
		{
			id: "stall-attribution",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetClient("timeSinceLastFrameMs", 6000)
				f.SetRelay("publisherActive", 1)
				return f
			},
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("timeSinceLastFrameMs", 40)
				return f
			},
		},
		{
			id: "keyframe-gap-churn",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetClient("keyframeStreamsReceived", 40)
				f.SetClient("reorderGapResyncs", 38)
				return f
			},
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("keyframeStreamsReceived", 40)
				f.SetClient("reorderGapResyncs", 1)
				return f
			},
		},
		{
			id: "parity-ineffective",
			fires: func() *Facts {
				// Parity is being served in volume and frames are still dying
				// at a rate comparable to what it repairs.
				f := viewerFacts()
				f.SetClient("parityChunksReceived", 400)
				f.SetClient("framesRecoveredByParity", 20)
				f.SetClient("framesDroppedIncomplete", 30)
				return f
			},
			quiet: func() *Facts {
				// The healthy shape: parity is working, and the residue is the
				// structural one every k has (docs/34 §11).
				f := viewerFacts()
				f.SetClient("parityChunksReceived", 400)
				f.SetClient("framesRecoveredByParity", 200)
				f.SetClient("framesDroppedIncomplete", 12)
				return f
			},
		},
		{
			id: "config-or-limits",
			fires: func() *Facts {
				f := bcastFacts()
				f.SetRelay("publisherActive", 1)
				f.SetRelay("framesRelayedPerSec", 0)
				return f
			},
			quiet: func() *Facts {
				f := bcastFacts()
				f.SetRelay("publisherActive", 1)
				f.SetRelay("framesRelayedPerSec", 30)
				return f
			},
		},
		{
			id: "resilient-undersupply",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetText("deliveryMode", "reliable")
				f.SetClient("playoutOffsetMs", 2000)
				return f
			},
			quiet: func() *Facts {
				// A live-edge viewer can never fire this row.
				f := viewerFacts()
				f.SetText("deliveryMode", "datagrams")
				f.SetClient("playoutOffsetMs", 2000)
				return f
			},
		},
		{
			id: "carrier-queue-overflow",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetRelay("carrierQueueOverflow", 12)
				return f
			},
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetRelay("carrierQueueOverflow", 0)
				return f
			},
		},
		{
			id: "viewer-count-gap",
			fires: func() *Facts {
				f := bcastFacts()
				f.SetRelay("viewersGlobal", 3)
				f.SetRelay("subscribersFleetTotal", 7)
				return f
			},
			quiet: func() *Facts {
				f := bcastFacts()
				f.SetRelay("viewersGlobal", 7)
				f.SetRelay("subscribersFleetTotal", 7)
				return f
			},
		},
		{
			id: "dvr-ring-outlived",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetRelay("dvrResyncs", 4)
				f.SetRelay("dvrLagMs", 2800)
				return f
			},
			quiet: func() *Facts {
				// The playbook's own caveat: large lag with resyncs FLAT is a
				// viewer riding out a bad link exactly as designed.
				f := viewerFacts()
				f.SetRelay("dvrResyncs", 0)
				f.SetRelay("dvrLagMs", 2900)
				return f
			},
		},
		{
			id: "audio-overflow-latch",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetClient("audioPacketsReceived", 1000)
				f.SetClient("audioOverflowDrops", 740)
				f.SetClient("audioGapsConcealed", 700)
				return f
			},
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("audioPacketsReceived", 1000)
				f.SetClient("audioOverflowDrops", 3)
				return f
			},
		},
		{
			id: "intermittent-fps-dips",
			fires: func() *Facts {
				f := viewerFacts()
				f.SetClient("fpsDipEpisodes", 4)
				f.SetClient("fpsDipShare", 0.2)
				f.SetClient("fpsDipWorstFps", 2)
				f.SetClient("fpsDipLongestMs", 6000)
				return f
			},
			// A measured zero is the healthy case, and it must read as a
			// PASSED check rather than a missing signal (D16).
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("fpsDipEpisodes", 0)
				f.SetClient("fpsDipShare", 0)
				return f
			},
		},
		{
			id: "keyframe-only-delivery",
			fires: func() *Facts {
				f := viewerFacts()
				// One gap resync per keyframe inside the dips: every GOP
				// broken, only keyframes surviving.
				f.SetClient("fpsDipResyncs", 24)
				f.SetClient("fpsDipKeyframes", 24)
				return f
			},
			// Dips with no resync activity are a different problem — a source
			// stutter, or the decoder — and this rule must not claim them.
			quiet: func() *Facts {
				f := viewerFacts()
				f.SetClient("fpsDipResyncs", 0)
				f.SetClient("fpsDipKeyframes", 24)
				return f
			},
		},
		{
			id: "delivered-below-target",
			fires: func() *Facts {
				f := castFacts()
				// 60 asked for, 30 delivered, all session long — a flat
				// baseline the dip rules cannot see and a perfect funnel.
				f.SetClient("targetFps", 60)
				f.SetClient("sentFps", 30)
				f.SetClient("captureFps", 30)
				return f
			},
			quiet: func() *Facts {
				f := castFacts()
				f.SetClient("targetFps", 30)
				f.SetClient("sentFps", 30)
				f.SetClient("captureFps", 30)
				return f
			},
		},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.id] = true
		t.Run(tc.id, func(t *testing.T) {
			rep := Evaluate(tc.fires(), Playbook())
			if findingByID(rep, tc.id) == nil {
				t.Errorf("rule did not fire on its own signature; findings=%+v unavailable=%+v",
					rep.Findings, rep.Unavailable)
			}
			rep = Evaluate(tc.quiet(), Playbook())
			if f := findingByID(rep, tc.id); f != nil {
				t.Errorf("rule fired on a healthy session: %+v", f)
			}
		})
	}

	for _, r := range Playbook() {
		if !covered[r.ID] {
			t.Errorf("rule %q has no fires/quiet pair — every playbook row needs both", r.ID)
		}
	}
}

// D7: a finding resting only on client testimony cannot claim high
// confidence. A wedged client's own accounting is the least reliable evidence
// in the system, and it is exactly what a wedged client sends.
func TestClientOnlyEvidenceCapsConfidence(t *testing.T) {
	f := viewerFacts()
	f.SetClient("receivedFps", 60)
	f.SetClient("decoderFps", 10)
	f.SetClient("isHardwareAccelerated", 0)
	rep := Evaluate(f, Playbook())
	fd := findingByID(rep, "decoder-choking")
	if fd == nil {
		t.Fatal("decoder-choking did not fire")
	}
	if fd.Confidence > clientOnlyConfidenceCap {
		t.Errorf("confidence = %v with client-only evidence, want <= %v", fd.Confidence, clientOnlyConfidenceCap)
	}
	for _, e := range fd.Evidence {
		if e.From == FromRelay {
			t.Fatal("fixture leaked relay evidence; the cap would not be exercised")
		}
	}
}

// The same rule anchored by a relay counter may exceed the cap — which is the
// point of the distinction, not a loophole.
func TestRelayAnchoredEvidenceMayExceedTheCap(t *testing.T) {
	f := castFacts()
	f.SetClient("encoderFps", 60)
	f.SetClient("sentFps", 10)
	f.SetRelay("framesRelayedPerSec", 9)
	rep := Evaluate(f, Playbook())
	fd := findingByID(rep, "send-path-gap")
	if fd == nil {
		t.Fatal("send-path-gap did not fire")
	}
	if fd.Confidence <= clientOnlyConfidenceCap {
		t.Errorf("confidence = %v with a relay anchor, want above the client-only cap", fd.Confidence)
	}
}

// A rule whose signals are absent must land in `unavailable` — never change
// the verdict by silently voting, and never be mistaken for a passed check.
func TestMissingSignalsAppearAsUnavailable(t *testing.T) {
	f := viewerFacts()
	f.SetClient("timeSinceLastFrameMs", 20) // only one rule can run

	rep := Evaluate(f, Playbook())
	if len(rep.Unavailable) == 0 {
		t.Fatal("no rules reported as unavailable despite almost no signals")
	}
	for _, m := range rep.Unavailable {
		if len(m.Signals) == 0 {
			t.Errorf("rule %q is unavailable but names no missing signal", m.ID)
		}
		// The signal names are QUALIFIED, so "the relay is not scraped" is
		// distinguishable from "this client stopped reporting".
		for _, s := range m.Signals {
			if side, _, ok := splitSignal(s); !ok || (side != "relay" && side != "client" && side != "fleet" && side != "text") {
				t.Errorf("missing signal %q is not side-qualified", s)
			}
		}
		if findingByID(rep, m.ID) != nil {
			t.Errorf("rule %q is both unavailable and firing", m.ID)
		}
		for _, p := range rep.Passed {
			if p == m.ID {
				t.Errorf("rule %q counted as passed despite missing signals", m.ID)
			}
		}
	}
}

// Owner decision (§8): a healthy session gets a POSITIVE verdict with the
// checks that support it — so "no issues" is distinguishable from "the
// analysis never ran".
func TestHealthySessionGetsAPositiveVerdictWithItsBasis(t *testing.T) {
	f := viewerFacts()
	f.SetClient("receivedFps", 60)
	f.SetClient("decoderFps", 60)
	f.SetClient("timeSinceLastFrameMs", 20)
	f.SetClient("keyframeStreamsReceived", 40)
	f.SetClient("reorderGapResyncs", 0)
	f.SetRelay("carrierQueueOverflow", 0)
	f.SetRelay("dvrResyncs", 0)

	rep := Evaluate(f, Playbook())
	if !rep.Healthy {
		t.Fatalf("healthy session produced findings: %+v", rep.Findings)
	}
	if len(rep.Passed) == 0 {
		t.Error("a healthy verdict must name the checks that passed")
	}
	if rep.Severity() != SeverityOK {
		t.Errorf("severity = %q, want ok", rep.Severity())
	}
	if rep.Summary() == "" {
		t.Error("no summary for a healthy session")
	}
}

// The one thing an ops view must never do: paint an absence of evidence as
// green. A session with NO signals at all is unknown, not ok.
func TestNoEvidenceIsUnknownNotOK(t *testing.T) {
	rep := Evaluate(viewerFacts(), Playbook())
	if rep.Severity() != SeverityUnknown {
		t.Errorf("severity with no signals = %q, want unknown", rep.Severity())
	}
	if len(rep.Passed) != 0 {
		t.Errorf("passed = %v with no signals; nothing was actually checked", rep.Passed)
	}
	// Healthy is still true (nothing fired), which is exactly why Severity()
	// and not Healthy is what a UI colours by.
	if rep.Severity() == SeverityOK {
		t.Error("an unevaluated session rendered as ok")
	}
}

// Findings are ranked worst-first: an operator reads top-down and must hit the
// thing most worth acting on.
func TestFindingsRankWorstFirst(t *testing.T) {
	f := viewerFacts()
	// A bad one (carrier overflow) and a warn one (decoder gap).
	f.SetRelay("carrierQueueOverflow", 9)
	f.SetClient("receivedFps", 60)
	f.SetClient("decoderFps", 10)

	rep := Evaluate(f, Playbook())
	if len(rep.Findings) < 2 {
		t.Fatalf("expected at least two findings, got %+v", rep.Findings)
	}
	if rep.Findings[0].Severity != SeverityBad {
		t.Errorf("first finding severity = %q, want bad", rep.Findings[0].Severity)
	}
	for i := 1; i < len(rep.Findings); i++ {
		if rep.Findings[i].Severity.Rank() > rep.Findings[i-1].Severity.Rank() {
			t.Errorf("findings are not worst-first at index %d", i)
		}
	}
	if rep.Severity() != SeverityBad {
		t.Errorf("report severity = %q, want the worst finding's", rep.Severity())
	}
}

// A rule scoped to one role must never fire on the other — the funnel gaps
// look superficially alike and would otherwise cross-contaminate.
func TestRuleScopesAreRespected(t *testing.T) {
	f := viewerFacts()
	f.SetClient("captureFps", 60)
	f.SetClient("encoderFps", 5)
	rep := Evaluate(f, Playbook())
	if findingByID(rep, "encoder-overload") != nil {
		t.Error("a broadcaster rule fired on a viewer session")
	}
	// And it is not reported as unavailable either — it simply does not apply.
	for _, m := range rep.Unavailable {
		if m.ID == "encoder-overload" {
			t.Error("an out-of-scope rule was reported as unavailable rather than skipped")
		}
	}
}

// Relay and client disagreeing is itself a finding (D7) — the stall rule is
// where that shows: the client says frames stopped, the relay says the
// publisher is live, and the verdict names which side that implicates.
func TestRelayAnchorChangesTheStallVerdict(t *testing.T) {
	away := viewerFacts()
	away.SetClient("timeSinceLastFrameMs", 5000)
	away.SetRelay("publisherActive", 0)
	got := findingByID(Evaluate(away, Playbook()), "stall-attribution")
	if got == nil || !contains(got.Verdict, "broadcaster stopped sending") {
		t.Errorf("publisher-away verdict = %q", verdictOf(got))
	}

	legB := viewerFacts()
	legB.SetClient("timeSinceLastFrameMs", 5000)
	legB.SetRelay("publisherActive", 1)
	got = findingByID(Evaluate(legB, Playbook()), "stall-attribution")
	if got == nil || !contains(got.Verdict, "leg B") {
		t.Errorf("publisher-live verdict = %q", verdictOf(got))
	}

	// Without the relay anchor the rule still fires, but says it cannot
	// attribute — and caps its confidence accordingly.
	blind := viewerFacts()
	blind.SetClient("timeSinceLastFrameMs", 5000)
	got = findingByID(Evaluate(blind, Playbook()), "stall-attribution")
	if got == nil || !contains(got.Verdict, "not yet attributed") {
		t.Errorf("unanchored verdict = %q", verdictOf(got))
	}
	if got.Confidence > clientOnlyConfidenceCap {
		t.Errorf("unanchored confidence = %v, want capped", got.Confidence)
	}
}

func verdictOf(f *Finding) string {
	if f == nil {
		return "<no finding>"
	}
	return f.Verdict
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// D17: the acceleration discriminator. A pipeline shortfall while the encoder
// is in SOFTWARE has an obvious first suspect, and saying so is the difference
// between a verdict and a shrug.
func TestSoftwareEncodeNarrowsAShortfallVerdict(t *testing.T) {
	base := func(accel string) *Facts {
		f := castFacts()
		f.SetClient("targetFps", 60)
		f.SetClient("sentFps", 30)
		f.SetClient("captureFps", 60) // capture keeps up: the gap is downstream
		if accel != "" {
			f.SetText("acceleration", accel)
		}
		return f
	}

	soft := findingByID(Evaluate(base("software"), Playbook()), "delivered-below-target")
	if soft == nil {
		t.Fatal("delivered-below-target did not fire")
	}
	if !strings.Contains(soft.Verdict, "SOFTWARE") {
		t.Errorf("software encode not named in the verdict: %q", soft.Verdict)
	}
	var sawText bool
	for _, e := range soft.Evidence {
		if e.Signal == "acceleration" && e.Text == "software" {
			sawText = true
		}
	}
	if !sawText {
		t.Error("acceleration missing from the evidence as text — the claim is not checkable")
	}

	// Hardware: still a shortfall, but the encoder is not the suspect.
	hard := findingByID(Evaluate(base("hardware"), Playbook()), "delivered-below-target")
	if hard == nil {
		t.Fatal("delivered-below-target did not fire on the hardware case")
	}
	if strings.Contains(hard.Verdict, "SOFTWARE") {
		t.Errorf("hardware encode was described as software: %q", hard.Verdict)
	}

	// Absent (the native engine reports no acceleration string) — the rule must
	// still fire, or it would be dead for every native broadcaster.
	if findingByID(Evaluate(base(""), Playbook()), "delivered-below-target") == nil {
		t.Error("the rule died when acceleration was absent")
	}
}

// And it must NOT point at the encoder when the source is the limit: there the
// encoder is keeping up with everything it is given.
func TestSourceLimitedShortfallDoesNotBlameTheEncoder(t *testing.T) {
	f := castFacts()
	f.SetClient("targetFps", 60)
	f.SetClient("sentFps", 4)
	f.SetClient("captureFps", 4) // a static screen
	f.SetText("acceleration", "software")

	fd := findingByID(Evaluate(f, Playbook()), "delivered-below-target")
	if fd == nil {
		t.Fatal("delivered-below-target did not fire")
	}
	if strings.Contains(fd.Verdict, "SOFTWARE") {
		t.Errorf("a source-limited stream was blamed on the encoder: %q", fd.Verdict)
	}
	if !strings.Contains(fd.Verdict, "source-limited") {
		t.Errorf("verdict does not name the source as the limit: %q", fd.Verdict)
	}
}
