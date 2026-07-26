package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newProj() (*Projection, *clock) {
	c := &clock{t: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	return New(c.now), c
}

func batch(sessionID, role string, stats map[string]any) ingest.Accepted {
	return ingest.Accepted{
		SessionID: sessionID, BroadcastKey: "1a2b3c4d5e6f", Role: role,
		App:     ingest.AppInfo{Version: "0.33.2", Surface: role, Browser: "Chrome 152", OS: "Windows"},
		Samples: []ingest.Sample{{TMs: 0, Stats: stats}},
	}
}

func healthyViewer() map[string]any {
	return map[string]any{
		"receivedFps": 60.0, "decoderFps": 60.0, "timeSinceLastFrameMs": 16.0,
		"deliveryMode": "datagrams", "keyframeStreamsReceived": 10.0, "reorderGapResyncs": 0.0,
	}
}

// round wraps observations as one COMPLETE scrape pass — every configured pod
// answered. That is the shape the lifecycle rules are allowed to draw
// conclusions from, so it is the default in tests; a partial round is spelled
// out explicitly where it is the point.
func round(obs []relayscrape.Observation) relayscrape.Round {
	return relayscrape.Round{Observations: obs, Complete: true, Pods: 1, PodsAnswered: 1}
}

func relayRound(subs ...relayscrape.Subscriber) relayscrape.Round {
	return round(relayObs(subs...))
}

func relayObs(subs ...relayscrape.Subscriber) []relayscrape.Observation {
	bc := relayscrape.Broadcast{
		PublisherActive: true, Role: "origin", Subscribers: len(subs),
		FramesRelayed: 10000, SubscriberDetails: subs,
	}
	out := []relayscrape.Observation{{
		Kind: "broadcast", Pod: "pod-a", Role: "origin",
		BroadcastKey: "1a2b3c4d5e6f", Broadcast: &bc,
	}}
	for i := range subs {
		s := subs[i]
		out = append(out, relayscrape.Observation{
			Kind: "subscriber", Pod: "pod-a", Role: "origin",
			BroadcastKey: "1a2b3c4d5e6f", SessionID: s.SessionID, Subscriber: &s,
		})
	}
	return out
}

func findSession(snap Snapshot, id string) *SessionView {
	for _, b := range snap.Live {
		for i := range b.Sessions {
			if b.Sessions[i].SessionID == id {
				return &b.Sessions[i]
			}
		}
	}
	return nil
}

// The headline honesty rule: a session that NEVER reported reads unknown, and
// one that has gone quiet reads stale. Neither is ever ok. Painting an absence
// of evidence as green is the one thing an ops dashboard must not do.
func TestAbsenceOfEvidenceIsNeverGreen(t *testing.T) {
	p, c := newProj()

	// A viewer the relay sees but which never sent telemetry (an old client,
	// a blocked endpoint).
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "aaaaaaaaaaaaaaaaaaaaaaaa"}))
	snap := p.Snapshot()
	silent := findSession(snap, "aaaaaaaaaaaaaaaaaaaaaaaa")
	if silent == nil {
		t.Fatal("a relay-visible subscriber that never reported is missing from the view entirely")
	}
	if silent.ClientState != "unknown" {
		t.Errorf("clientState = %q, want unknown", silent.ClientState)
	}
	if silent.Severity == rules.SeverityOK {
		t.Error("a viewer that never reported rendered as ok")
	}
	if silent.Severity != rules.SeverityUnknown {
		t.Errorf("severity = %q, want unknown", silent.Severity)
	}

	// A viewer that reported healthily and then went quiet.
	p.ObserveClient(batch("bbbbbbbbbbbbbbbbbbbbbbbb", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "bbbbbbbbbbbbbbbbbbbbbbbb"}))
	if v := findSession(p.Snapshot(), "bbbbbbbbbbbbbbbbbbbbbbbb"); v.ClientState != "reporting" {
		t.Fatalf("clientState = %q while reporting", v.ClientState)
	}

	c.add(ClientStaleAfter + time.Second)
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "bbbbbbbbbbbbbbbbbbbbbbbb"}))
	stale := findSession(p.Snapshot(), "bbbbbbbbbbbbbbbbbbbbbbbb")
	if stale.ClientState != "stale" {
		t.Errorf("clientState = %q after going quiet, want stale", stale.ClientState)
	}
	if stale.Severity == rules.SeverityOK {
		t.Error("a viewer whose telemetry stopped rendered as ok")
	}
}

// The two halves of a row have different freshness and the view says so
// separately, rather than painting them as one instant.
func TestPerSideFreshnessIsReportedSeparately(t *testing.T) {
	p, c := newProj()
	p.ObserveClient(batch("cccccccccccccccccccccccc", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "cccccccccccccccccccccccc"}))

	c.add(25 * time.Second) // past the relay's stale bound, inside the client's
	p.ObserveClient(batch("cccccccccccccccccccccccc", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")

	v := findSession(p.Snapshot(), "cccccccccccccccccccccccc")
	if v.ClientState != "reporting" {
		t.Errorf("clientState = %q, want reporting", v.ClientState)
	}
	if v.RelayState != "stale" {
		t.Errorf("relayState = %q, want stale — the two sides age independently", v.RelayState)
	}
	if v.ClientAgeMs < 0 || v.RelayAgeMs < v.ClientAgeMs {
		t.Errorf("ages = client %d / relay %d; the relay side is older", v.ClientAgeMs, v.RelayAgeMs)
	}
}

// Severity, not recency, orders the live group: problems float to the top and
// a healthy fleet is a short quiet list.
func TestSeverityOrdersTheLiveGroup(t *testing.T) {
	c := &clock{t: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	p := New(c.now)

	// Three broadcasts: healthy, warn, bad. Each round is ONE scrape pass over
	// the whole fleet, which is the production shape — a pass lists every
	// broadcast, and a broadcast missing from a complete pass has ended.
	// Two rounds, because relay counters are read as a WINDOW: the bad
	// broadcast is bad because its viewer is overflowing NOW, not because it
	// once did.
	feed := func(gen uint64, overflow uint64) {
		var obs []relayscrape.Observation
		mk := func(key, session string, stats map[string]any, sub relayscrape.Subscriber) {
			a := batch(session, "viewer", stats)
			a.BroadcastKey = key
			p.ObserveClient(a, "Chrome 152", "Windows", "0.33.2")
			bc := relayscrape.Broadcast{
				PublisherActive: true, Role: "origin", Subscribers: 1,
				// Frames keep flowing, or row 10 would (correctly) call every
				// one of these broadcasts dead.
				FramesRelayed: 10000 * gen, SubscriberDetails: []relayscrape.Subscriber{sub},
			}
			obs = append(obs,
				relayscrape.Observation{Kind: "broadcast", Pod: "pod-a", BroadcastKey: key, Broadcast: &bc},
				relayscrape.Observation{Kind: "subscriber", Pod: "pod-a", BroadcastKey: key, SessionID: session, Subscriber: &sub},
			)
		}
		mk("111111111111", "111111111111111111111111", healthyViewer(), relayscrape.Subscriber{SessionID: "111111111111111111111111"})
		warn := healthyViewer()
		warn["decoderFps"] = 5.0
		mk("222222222222", "222222222222222222222222", warn, relayscrape.Subscriber{SessionID: "222222222222222222222222"})
		mk("333333333333", "333333333333333333333333", healthyViewer(),
			relayscrape.Subscriber{SessionID: "333333333333333333333333", CarrierQueueOverflow: overflow})
		p.ObserveRelay(round(obs))
	}
	feed(1, 0)
	c.add(5 * time.Second)
	feed(2, 20)

	// Hysteresis: escalation takes two consecutive evaluations, so a single
	// blip cannot light the page up.
	p.Snapshot()
	snap := p.Snapshot()

	if len(snap.Live) != 3 {
		t.Fatalf("live broadcasts = %d, want 3", len(snap.Live))
	}
	prev := 9
	for i, b := range snap.Live {
		r := maxRank(b.Severity, b.WorstViewer)
		if r > prev {
			t.Errorf("broadcast %d (%s) is worse than the one above it", i, b.BroadcastKey)
		}
		prev = r
	}
	if snap.Live[0].BroadcastKey != "333333333333" {
		t.Errorf("first broadcast = %s, want the bad one", snap.Live[0].BroadcastKey)
	}
}

// A live `warn` outranks an ended `bad`: the GROUPING is the precedence,
// because only the live one can still be acted on. The two never interleave.
func TestLiveAndEndedNeverInterleave(t *testing.T) {
	p, _ := newProj()

	warn := healthyViewer()
	warn["decoderFps"] = 4.0
	p.ObserveClient(batch("dddddddddddddddddddddddd", "viewer", warn), "Chrome 152", "Windows", "0.33.2")
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "dddddddddddddddddddddddd"}))

	p.NoteEnded(BroadcastView{
		BroadcastKey: "999999999999", Severity: rules.SeverityBad,
		WorstViewer: rules.SeverityBad, EndedAgoMs: 60000, Viewers: 2,
	})

	p.Snapshot()
	snap := p.Snapshot()

	if len(snap.Live) == 0 {
		t.Fatal("the live broadcast vanished")
	}
	if len(snap.Ended) != 1 {
		t.Fatalf("ended = %d, want 1", len(snap.Ended))
	}
	// Separate arrays: no ordering comparison can put the ended `bad` above the
	// live `warn`, because they are not in the same list at all.
	for _, b := range snap.Live {
		if b.Lifecycle == "ended" {
			t.Error("an ended broadcast appeared in the live group")
		}
	}
	if snap.Ended[0].Lifecycle != "ended" {
		t.Errorf("ended row lifecycle = %q", snap.Ended[0].Lifecycle)
	}
	// The ended row KEEPS its severity — an ended `bad` still shows red; it is
	// recession, not hue suppression, that stops it out-shouting the live row.
	if snap.Ended[0].Severity != rules.SeverityBad {
		t.Errorf("ended severity = %q, want bad preserved", snap.Ended[0].Severity)
	}
}

// The ended group is served from stored verdicts, not recomputed — it shows
// the FINAL verdict, which is what "how did last night go?" means.
func TestEndedGroupIsBoundedAndNewestFirst(t *testing.T) {
	p, _ := newProj()
	for i := range MaxRecentEnded + 5 {
		p.NoteEnded(BroadcastView{
			BroadcastKey: string(rune('a'+i)) + "00000000000",
			Severity:     rules.SeverityOK, EndedAgoMs: int64(i) * 1000,
		})
	}
	snap := p.Snapshot()
	if len(snap.Ended) != MaxRecentEnded {
		t.Errorf("ended = %d, want the %d cap", len(snap.Ended), MaxRecentEnded)
	}
}

// Hysteresis follows the project's dwell instinct (R4, R27). Deliberately
// asymmetric for an ops view: problems appear promptly and must not vanish
// before the human finishes looking at them.
func TestHysteresis(t *testing.T) {
	t.Run("a single-sample blip does not escalate", func(t *testing.T) {
		p, _ := newProj()
		p.ObserveClient(batch("eeeeeeeeeeeeeeeeeeeeeeee", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
		p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "eeeeeeeeeeeeeeeeeeeeeeee"}))
		p.Snapshot()

		blip := healthyViewer()
		blip["decoderFps"] = 2.0
		p.ObserveClient(batch("eeeeeeeeeeeeeeeeeeeeeeee", "viewer", blip), "Chrome 152", "Windows", "0.33.2")
		v := findSession(p.Snapshot(), "eeeeeeeeeeeeeeeeeeeeeeee")
		if v.Severity == rules.SeverityWarn {
			t.Error("one bad sample escalated immediately")
		}

		// Held a second time ⇒ it escalates.
		p.ObserveClient(batch("eeeeeeeeeeeeeeeeeeeeeeee", "viewer", blip), "Chrome 152", "Windows", "0.33.2")
		v = findSession(p.Snapshot(), "eeeeeeeeeeeeeeeeeeeeeeee")
		if v.Severity != rules.SeverityWarn {
			t.Errorf("severity = %q after two bad evaluations, want warn", v.Severity)
		}
	})

	t.Run("a cleared fault persists through the dwell", func(t *testing.T) {
		p, c := newProj()
		bad := healthyViewer()
		bad["decoderFps"] = 2.0
		for range 3 {
			p.ObserveClient(batch("ffffffffffffffffffffffff", "viewer", bad), "Chrome 152", "Windows", "0.33.2")
			p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "ffffffffffffffffffffffff"}))
			p.Snapshot()
		}
		if v := findSession(p.Snapshot(), "ffffffffffffffffffffffff"); v.Severity != rules.SeverityWarn {
			t.Fatalf("severity = %q, want warn before the clear", v.Severity)
		}

		// Recovered, but only just: the state must hold so the operator can
		// still see what they came to look at.
		p.ObserveClient(batch("ffffffffffffffffffffffff", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
		if v := findSession(p.Snapshot(), "ffffffffffffffffffffffff"); v.Severity != rules.SeverityWarn {
			t.Errorf("severity = %q immediately after recovery, want the fault held", v.Severity)
		}

		c.add(ClearDwell + time.Second)
		p.ObserveClient(batch("ffffffffffffffffffffffff", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
		p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "ffffffffffffffffffffffff"}))
		if v := findSession(p.Snapshot(), "ffffffffffffffffffffffff"); v.Severity == rules.SeverityWarn {
			t.Error("the fault never cleared after the dwell")
		}
	})
}

// The broadcaster and its viewers are ONE table with the broadcaster pinned
// first: it is the same kind of row, but the one whose problems explain
// everyone else's.
func TestBroadcasterIsPinnedFirst(t *testing.T) {
	p, _ := newProj()
	p.ObserveClient(batch("111111111111111111111111", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	p.ObserveClient(batch("222222222222222222222222", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	p.ObserveClient(batch("333333333333333333333333", "broadcaster", map[string]any{
		"captureFps": 60.0, "encoderFps": 60.0, "sentFps": 60.0,
	}), "Chrome 152", "Windows", "0.33.2")

	snap := p.Snapshot()
	if len(snap.Live) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(snap.Live))
	}
	sessions := snap.Live[0].Sessions
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d, want 3 in ONE table", len(sessions))
	}
	if sessions[0].Role != "broadcaster" {
		t.Errorf("first row role = %q, want broadcaster pinned first", sessions[0].Role)
	}
	// Viewers counted as viewers; the broadcaster is not one.
	if snap.Live[0].Viewers != 2 {
		t.Errorf("viewers = %d, want 2 (the broadcaster is not an audience member)", snap.Live[0].Viewers)
	}
}

// The live view and diagnose() use the SAME rules (§4.8.3). Two disagreeing
// truths about one stream would be worse than no dashboard.
func TestLiveSeverityMatchesTheRuleEngine(t *testing.T) {
	p, _ := newProj()
	bad := healthyViewer()
	bad["decoderFps"] = 3.0
	bad["isHardwareAccelerated"] = false
	for range 3 {
		p.ObserveClient(batch("abcabcabcabcabcabcabcabc", "viewer", bad), "Chrome 152", "Windows", "0.33.2")
		p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "abcabcabcabcabcabcabcabc"}))
		p.Snapshot()
	}
	v := findSession(p.Snapshot(), "abcabcabcabcabcabcabcabc")

	// The same facts through the rule engine directly.
	f := rules.NewFacts(v.SessionID, "session", "viewer")
	for k, val := range v.Metrics {
		f.SetClient(k, val)
	}
	for k, val := range v.Config {
		f.SetText(k, val)
	}
	f.SetRelay("carrierQueueOverflow", 0)
	f.SetRelay("dvrResyncs", 0)
	direct := rules.Evaluate(f, rules.Playbook())

	if v.Severity != direct.Severity() {
		t.Errorf("dashboard severity %q != diagnose severity %q — one stream, two truths",
			v.Severity, direct.Severity())
	}
}

// The snapshot must always serialize: it is what /live returns.
func TestSnapshotSerializes(t *testing.T) {
	p, _ := newProj()
	p.ObserveClient(batch("aaaabbbbccccddddeeeeffff", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	b, err := json.Marshal(p.Snapshot())
	if err != nil {
		t.Fatalf("snapshot does not serialize: %v", err)
	}
	if len(b) == 0 {
		t.Error("empty snapshot")
	}
}

func TestEndSessionRemovesItFromTheView(t *testing.T) {
	p, _ := newProj()
	p.ObserveClient(batch("1a1a1a1a1a1a1a1a1a1a1a1a", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	if findSession(p.Snapshot(), "1a1a1a1a1a1a1a1a1a1a1a1a") == nil {
		t.Fatal("session missing before end")
	}
	p.EndSession("1a2b3c4d5e6f", "1a1a1a1a1a1a1a1a1a1a1a1a")
	if findSession(p.Snapshot(), "1a1a1a1a1a1a1a1a1a1a1a1a") != nil {
		t.Error("session still live after EndSession")
	}
}

// --- lifecycle (review finding 1) -----------------------------------------
//
// Before this, `EndSession`/`NoteEnded` had no production caller at all: the
// Ended group could never appear, and nothing ever left `bcasts`/`sessions`.
// The tests below drive the lifecycle the way production does — through scrape
// rounds and the clock — rather than by calling the end methods directly,
// which is precisely why the defect survived the original suite.

func TestABroadcastEndsWhenCompleteRoundsStopListingIt(t *testing.T) {
	p, c := newProj()
	p.ObserveClient(batch("1b1b1b1b1b1b1b1b1b1b1b1b", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "1b1b1b1b1b1b1b1b1b1b1b1b"}))
	if len(p.Snapshot().Live) != 1 {
		t.Fatal("broadcast missing while it is being scraped")
	}

	// The hub is gone from /statusz — the relay's own statement that the
	// broadcast is over. One omission is tolerated; the second is the verdict.
	c.add(5 * time.Second)
	p.ObserveRelay(round(nil))
	if len(p.Snapshot().Live) != 1 {
		t.Fatal("a single missed round ended the broadcast; it must take EndedAfterMissedRounds")
	}
	c.add(5 * time.Second)
	p.ObserveRelay(round(nil))

	c.add(2 * time.Second)
	snap := p.Snapshot()
	if len(snap.Live) != 0 {
		t.Errorf("live broadcasts = %d, want 0 after the relay stopped listing it", len(snap.Live))
	}
	if len(snap.Ended) != 1 {
		t.Fatalf("ended broadcasts = %d, want 1 — the recessed group can never appear otherwise", len(snap.Ended))
	}
	if snap.Ended[0].Lifecycle != "ended" {
		t.Errorf("ended lifecycle = %q", snap.Ended[0].Lifecycle)
	}
	// The age is measured from the end INSTANT, so it grows with the clock
	// rather than being frozen at whatever the ending render happened to see.
	if snap.Ended[0].EndedAgoMs != 2000 {
		t.Errorf("EndedAgoMs = %d, want 2000; an ended row that cannot say when it ended cannot be aged out either",
			snap.Ended[0].EndedAgoMs)
	}
	// The leak half of the finding: the projection must actually shrink.
	if len(p.bcasts) != 0 {
		t.Errorf("bcasts still holds %d entries after the broadcast ended", len(p.bcasts))
	}
}

func TestAPartialRoundNeverEndsABroadcast(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "2b2b2b2b2b2b2b2b2b2b2b2b"}))

	// A pod timing out mid-rollout: the fleet's answer is incomplete, so an
	// absence proves nothing. Ten of these must change nothing at all.
	for range 10 {
		c.add(5 * time.Second)
		p.ObserveRelay(relayscrape.Round{Complete: false, Pods: 2, PodsAnswered: 1})
	}
	if len(p.Snapshot().Live) != 1 {
		t.Error("an incomplete scrape round ended a broadcast; one dead pod would sweep the whole dashboard")
	}
}

func TestAClientOnlyBroadcastSurvivesRoundsAndEndsOnQuiet(t *testing.T) {
	p, c := newProj()
	// No relay configured (or a session shorter than a scrape interval): the
	// relay never observed this broadcast, so its absence from a round is not
	// evidence of anything.
	p.ObserveClient(batch("3b3b3b3b3b3b3b3b3b3b3b3b", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	for range 5 {
		c.add(5 * time.Second)
		p.ObserveRelay(round(nil))
		p.ObserveClient(batch("3b3b3b3b3b3b3b3b3b3b3b3b", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	}
	if len(p.Snapshot().Live) != 1 {
		t.Fatal("a reporting client-only broadcast was ended by rounds that never covered it")
	}

	// It goes quiet on both sides: the backstop, not the mechanism.
	c.add(EndedAfterQuiet + time.Second)
	snap := p.Snapshot()
	if len(snap.Live) != 0 || len(snap.Ended) != 1 {
		t.Errorf("live = %d, ended = %d; a broadcast nothing has said anything about must not live forever",
			len(snap.Live), len(snap.Ended))
	}
}

func TestAQuietSessionIsEvictedFromItsBroadcast(t *testing.T) {
	p, c := newProj()
	// A viewer the relay saw but which never reported: it must render as
	// `unknown` while it is there, and it must not be there forever.
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "4b4b4b4b4b4b4b4b4b4b4b4b"}))
	if findSession(p.Snapshot(), "4b4b4b4b4b4b4b4b4b4b4b4b") == nil {
		t.Fatal("session missing while the relay still saw it")
	}
	// The relay stops listing that subscriber, but the broadcast lives on.
	for range 40 {
		c.add(5 * time.Second)
		p.ObserveRelay(relayRound())
	}
	if len(p.Snapshot().Live) != 1 {
		t.Fatal("the broadcast itself was ended; only the session should have been")
	}
	if findSession(p.Snapshot(), "4b4b4b4b4b4b4b4b4b4b4b4b") != nil {
		t.Error("a session neither side has mentioned for minutes is still a row")
	}
}

func TestEndedRowsAgeOut(t *testing.T) {
	p, c := newProj()
	p.NoteEnded(BroadcastView{BroadcastKey: "5b5b5b5b5b5b"})
	if len(p.Snapshot().Ended) != 1 {
		t.Fatal("ended row missing immediately after it ended")
	}
	c.add(EndedRetention + time.Minute)
	if n := len(p.Snapshot().Ended); n != 0 {
		t.Errorf("ended rows = %d after the retention window; the group must not grow forever", n)
	}
}

// --- cluster aggregation (review finding 6) --------------------------------

// One broadcast, two pods (R17's origin/edge cascade), reported under the SAME
// obfuscated key because the fleet shares one statsKey. The scraper answers
// pods concurrently and appends in completion order, so if broadcast-level
// facts are last-writer-wins the whole card — role, counts, rule outcomes —
// flaps at scrape cadence depending on which pod was quicker.
// `gen` scales the cumulative counters, so feeding gen 1 then gen 2 makes each
// window's delta exactly the per-gen figure — the shape the live path reads.
func clusterObs(originFirst bool, gen uint64) []relayscrape.Observation {
	originSub := relayscrape.Subscriber{SessionID: "aa11aa11aa11aa11aa11aa11", Dropped: 2 * gen}
	edgeSub1 := relayscrape.Subscriber{SessionID: "bb22bb22bb22bb22bb22bb22"}
	edgeSub2 := relayscrape.Subscriber{SessionID: "cc33cc33cc33cc33cc33cc33"}
	origin := relayscrape.Broadcast{
		PublisherActive: true, Role: "origin", Subscribers: 2, EdgeSessions: 1,
		ViewersGlobal: 3, FramesRelayed: 1200 * gen, IngressFramesLost: 4 * gen,
		DatagramsDropped: 10 * gen,
		SubscriberDetails: []relayscrape.Subscriber{
			originSub, {Key: "edge", Internal: true},
		},
	}
	edge := relayscrape.Broadcast{
		PublisherActive: true, Role: "edge", Subscribers: 2,
		FramesRelayed: 1180 * gen, DatagramsDropped: 5 * gen,
		SubscriberDetails: []relayscrape.Subscriber{edgeSub1, edgeSub2},
	}
	a := []relayscrape.Observation{
		{Kind: "broadcast", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &origin},
		{Kind: "subscriber", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", SessionID: originSub.SessionID, Subscriber: &originSub},
	}
	b := []relayscrape.Observation{
		{Kind: "broadcast", Pod: "pod-b", Role: "edge", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &edge},
		{Kind: "subscriber", Pod: "pod-b", Role: "edge", BroadcastKey: "1a2b3c4d5e6f", SessionID: edgeSub1.SessionID, Subscriber: &edgeSub1},
		{Kind: "subscriber", Pod: "pod-b", Role: "edge", BroadcastKey: "1a2b3c4d5e6f", SessionID: edgeSub2.SessionID, Subscriber: &edgeSub2},
	}
	if originFirst {
		return append(a, b...)
	}
	return append(b, a...)
}

func TestClusterFactsDoNotDependOnPodAnswerOrder(t *testing.T) {
	render := func(originFirst bool) Snapshot {
		p, c := newProj()
		p.ObserveRelay(round(clusterObs(originFirst, 1)))
		c.add(5 * time.Second)
		p.ObserveRelay(round(clusterObs(originFirst, 2)))
		return p.Snapshot()
	}
	a, err := json.Marshal(render(true))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(render(false))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("the card depends on which pod answered last\norigin first: %s\nedge first:   %s", a, b)
	}
}

func TestFleetSubscriberTotalSumsRealViewersAcrossPods(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(round(clusterObs(true, 1)))
	c.add(5 * time.Second)
	p.ObserveRelay(round(clusterObs(true, 2)))
	snap := p.Snapshot()
	if len(snap.Live) != 1 {
		t.Fatalf("live broadcasts = %d, want 1 — both pods carry ONE broadcast", len(snap.Live))
	}
	bv := snap.Live[0]
	// One real viewer on the origin plus two on the edge. The origin's own
	// internal edge session is plumbing and must never be counted as audience.
	if got := bv.Metrics["subscribersFleetTotal"]; got != 3 {
		t.Errorf("subscribersFleetTotal = %v, want 3 (one pod's count is not a fleet total)", got)
	}
	// R18 already computes the authoritative audience count at the origin.
	if got := bv.Metrics["viewersGlobal"]; got != 3 {
		t.Errorf("viewersGlobal = %v, want the origin's aggregate 3", got)
	}
	// Ingress is measured on the publisher's leg, which only the origin has.
	if got := bv.Metrics["ingressFramesLost"]; got != 4 {
		t.Errorf("ingressFramesLost = %v, want the origin's 4", got)
	}
	// Egress is per pod, so it adds up.
	if got := bv.Metrics["datagramsDropped"]; got != 15 {
		t.Errorf("datagramsDropped = %v, want 15 (10 + 5 across pods)", got)
	}
	if bv.Role != "origin" || bv.Pod != "pod-a" {
		t.Errorf("card reports role %q on pod %q; the origin is the broadcast's home", bv.Role, bv.Pod)
	}
}

// --- windowed evaluation (review finding 4) --------------------------------
//
// /statusz counters are cumulative and the playbook's counter rules are
// written for a session rollup, where the total IS the session. Fed lifetime
// totals, the same rules can only ratchet — and hysteresis can never clear a
// number that never comes down.

// twoPodlessRounds feeds `n` rounds for one viewer whose relay counters are
// whatever `at` returns for that round.
func feedRounds(p *Projection, c *clock, n int, at func(i int) relayscrape.Subscriber) {
	for i := range n {
		if i > 0 {
			c.add(5 * time.Second)
		}
		p.ObserveRelay(relayRound(at(i)))
		p.Snapshot()
	}
}

func TestASingleCounterEventDoesNotMarkAViewerBadForever(t *testing.T) {
	p, c := newProj()
	// One carrier overflow burst early on, then a clean stream for minutes.
	feedRounds(p, c, 12, func(i int) relayscrape.Subscriber {
		var overflow uint64
		if i >= 2 {
			overflow = 20 // happened once, at round 2, and never again
		}
		return relayscrape.Subscriber{SessionID: "d0d0d0d0d0d0d0d0d0d0d0d0", CarrierQueueOverflow: overflow}
	})

	v := findSession(p.Snapshot(), "d0d0d0d0d0d0d0d0d0d0d0d0")
	if v == nil {
		t.Fatal("session missing")
	}
	for _, f := range v.Findings {
		if f.ID == "carrier-queue-overflow" {
			t.Errorf("a single overflow event at round 2 is still accusing the viewer %d rounds later", 12)
		}
	}
	if v.Severity == rules.SeverityBad {
		t.Error("severity is still bad long after the event that caused it stopped happening")
	}
}

func TestAnOngoingCounterEventStillFires(t *testing.T) {
	p, c := newProj()
	// The delta must not simply disable the rule: overflow every round.
	feedRounds(p, c, 6, func(i int) relayscrape.Subscriber {
		return relayscrape.Subscriber{
			SessionID: "d1d1d1d1d1d1d1d1d1d1d1d1", CarrierQueueOverflow: uint64(10 * i),
		}
	})

	v := findSession(p.Snapshot(), "d1d1d1d1d1d1d1d1d1d1d1d1")
	found := false
	for _, f := range v.Findings {
		if f.ID == "carrier-queue-overflow" {
			found = true
		}
	}
	if !found {
		t.Error("a viewer overflowing every single window is not being reported")
	}
}

func TestTheFirstObservationReportsNoCounterFacts(t *testing.T) {
	p, _ := newProj()
	// A session already three hours old when telemetry first sees it: its
	// lifetime totals say nothing about now, so they must not be presented as
	// if they did. `unavailable` is the honest state for one scrape interval.
	p.ObserveRelay(relayRound(relayscrape.Subscriber{
		SessionID: "d2d2d2d2d2d2d2d2d2d2d2d2", CarrierQueueOverflow: 9000, Dropped: 100000,
	}))
	v := findSession(p.Snapshot(), "d2d2d2d2d2d2d2d2d2d2d2d2")
	for _, f := range v.Findings {
		if f.ID == "carrier-queue-overflow" {
			t.Error("a lifetime total was read as if it had happened in the last five seconds")
		}
	}
}

func TestACounterGoingBackwardsIsNotNegativeTraffic(t *testing.T) {
	p, c := newProj()
	// A relay restart (or a re-created hub) resets the counters. The window is
	// void; it must not produce a negative delta or a spurious clean bill.
	feedRounds(p, c, 3, func(i int) relayscrape.Subscriber {
		return relayscrape.Subscriber{SessionID: "d3d3d3d3d3d3d3d3d3d3d3d3", Dropped: uint64(1000 * (i + 1))}
	})
	c.add(5 * time.Second)
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "d3d3d3d3d3d3d3d3d3d3d3d3", Dropped: 0}))
	if got, ok := relayFact(p, "d3d3d3d3d3d3d3d3d3d3d3d3", "subscriberDropped"); ok && got < 0 {
		t.Errorf("subscriberDropped = %v after a counter reset", got)
	}

	// And the window after the reset measures from the new baseline.
	c.add(5 * time.Second)
	p.ObserveRelay(relayRound(relayscrape.Subscriber{SessionID: "d3d3d3d3d3d3d3d3d3d3d3d3", Dropped: 7}))
	if got, _ := relayFact(p, "d3d3d3d3d3d3d3d3d3d3d3d3", "subscriberDropped"); got != 7 {
		t.Errorf("subscriberDropped = %v after the reset, want the 7 measured since", got)
	}
}

// relayFact reads a session's relay-side fact directly. Relay facts are inputs
// to the rules rather than row metrics, so there is no rendered surface to
// assert delta arithmetic against.
func relayFact(p *Projection, sessionID, name string) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.bcasts {
		if s, ok := b.sessions[sessionID]; ok {
			v, ok := s.relayFacts[name]
			return v, ok
		}
	}
	return 0, false
}

// --- producer coverage (review finding 5) ----------------------------------

// What this producer emits must be exactly its share of the inventory a rule
// is allowed to require. Both directions matter: a fact emitted but unlisted
// weakens the guard, and a fact listed but never emitted is precisely what
// playbook row 10 was — a rule that reads `unavailable` forever because its
// input does not exist.
func TestLiveEmitsExactlyItsShareOfTheInventory(t *testing.T) {
	p, c := newProj()
	full := relayscrape.Subscriber{
		SessionID: "e0e0e0e0e0e0e0e0e0e0e0e0", QueueDepth: 3, Dropped: 5, SendErrors: 1,
		KeyframesSent: 9, KeyframesDropped: 2, Reliable: true, CarrierStreams: 4,
		CarrierRecords: 100, CarrierRecordsDropped: 3, CarrierQueueOverflow: 1,
		DVR: true, DVRBufferMs: 2000, DVRLagMs: 900, DVRGopSeq: 12, DVRResyncs: 1,
	}
	stats := map[string]any{"isHardwareAccelerated": true}
	for _, f := range liveNumericFields {
		stats[f] = 1.0
	}
	for _, f := range liveTextFields {
		stats[f] = "x"
	}
	stats["audioBuffer"] = map[string]any{"overflowDrops": 1.0, "gapsConcealed": 2.0}

	// Two rounds, so every counter has a window and every delta fact exists.
	for i := range 2 {
		if i > 0 {
			c.add(5 * time.Second)
			full.Dropped += 3
			full.CarrierQueueOverflow++
		}
		p.ObserveClient(batch("e0e0e0e0e0e0e0e0e0e0e0e0", "viewer", stats), "Chrome 152", "Windows", "0.33.2")
		p.ObserveRelay(relayRound(full))
	}

	emitted := map[string]bool{}
	p.mu.Lock()
	for _, b := range p.bcasts {
		bf := rules.NewFacts(b.key, "broadcast", "")
		for k, v := range b.relayFacts {
			bf.SetRelay(k, v)
		}
		for _, n := range bf.Names() {
			emitted[n] = true
		}
		for _, s := range b.sessions {
			for _, n := range s.facts(s.view).Names() {
				emitted[n] = true
			}
		}
	}
	p.mu.Unlock()

	want := rules.ProducedBy("live")
	for n := range emitted {
		if !want[n] {
			t.Errorf("live emits %q, which rules.ProducibleFacts does not list", n)
		}
	}
	for n := range want {
		if !emitted[n] {
			t.Errorf("rules.ProducibleFacts claims live emits %q, but a maximal round produced no such fact", n)
		}
	}
}
