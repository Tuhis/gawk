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

func relayRound(subs ...relayscrape.Subscriber) []relayscrape.Observation {
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

	// Three broadcasts: healthy, warn, bad.
	mk := func(key, session string, stats map[string]any, sub relayscrape.Subscriber) {
		a := batch(session, "viewer", stats)
		a.BroadcastKey = key
		p.ObserveClient(a, "Chrome 152", "Windows", "0.33.2")
		bc := relayscrape.Broadcast{
			PublisherActive: true, Role: "origin", Subscribers: 1,
			FramesRelayed: 10000, SubscriberDetails: []relayscrape.Subscriber{sub},
		}
		p.ObserveRelay([]relayscrape.Observation{
			{Kind: "broadcast", Pod: "pod-a", BroadcastKey: key, Broadcast: &bc},
			{Kind: "subscriber", Pod: "pod-a", BroadcastKey: key, SessionID: session, Subscriber: &sub},
		})
	}
	mk("111111111111", "111111111111111111111111", healthyViewer(), relayscrape.Subscriber{SessionID: "111111111111111111111111"})
	warn := healthyViewer()
	warn["decoderFps"] = 5.0
	mk("222222222222", "222222222222222222222222", warn, relayscrape.Subscriber{SessionID: "222222222222222222222222"})
	bad := healthyViewer()
	mk("333333333333", "333333333333333333333333", bad,
		relayscrape.Subscriber{SessionID: "333333333333333333333333", CarrierQueueOverflow: 20})

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
