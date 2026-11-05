package live

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
)

const (
	stripedSID   = "5a5a5a5a5a5a5a5a5a5a5a5a"
	plainSID     = "5b5b5b5b5b5b5b5b5b5b5b5b"
	stripedOwner = "0a1b2c3d4e5f"
)

// stripedObs is one scrape round over two pods carrying one broadcast:
//
//   - pod-a (origin): a striped viewer's primary (owner key, striped), an
//     unstriped viewer, the striped viewer's member-0 leg, and a leg from a
//     relay predating the owner key (unattributable);
//   - pod-b (edge, unless edgeAnswered is false): the striped viewer's
//     member-1 leg, and a leg whose owner no primary carries.
//
// gen scales every cumulative drop counter. Legs carry no sessionId: they are
// never sent the telemetry hello.
func stripedObs(gen uint64, edgeAnswered bool) []relayscrape.Observation {
	prim := relayscrape.Subscriber{Key: "prim", SessionID: stripedSID, Dropped: 1 * gen, Striped: true, StripeN: 2, StripeOwner: stripedOwner}
	plain := relayscrape.Subscriber{Key: "plain", SessionID: plainSID, Dropped: 3 * gen}
	leg0 := relayscrape.Subscriber{Key: "leg0", Dropped: 10 * gen, QueueDepth: 2, StripeLeg: true, StripeN: 2, StripeOwner: stripedOwner}
	oldLeg := relayscrape.Subscriber{Key: "old", Dropped: 50 * gen, StripeLeg: true, StripeN: 2, StripeMember: 1}
	leg1 := relayscrape.Subscriber{Key: "leg1", Dropped: 20 * gen, StripeLeg: true, StripeN: 2, StripeMember: 1, StripeOwner: stripedOwner}
	stray := relayscrape.Subscriber{Key: "stray", Dropped: 70 * gen, StripeLeg: true, StripeN: 4, StripeMember: 3, StripeOwner: "ffffffffffff"}

	origin := relayscrape.Broadcast{
		PublisherActive: true, Role: "origin", Subscribers: 2, ViewersGlobal: 2, FramesRelayed: 1000 * gen,
		SubscriberDetails: []relayscrape.Subscriber{prim, plain, leg0, oldLeg},
	}
	edge := relayscrape.Broadcast{
		PublisherActive: true, Role: "edge", FramesRelayed: 990 * gen,
		SubscriberDetails: []relayscrape.Subscriber{leg1, stray},
	}
	out := []relayscrape.Observation{
		{Kind: "broadcast", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &origin},
		{Kind: "subscriber", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", SessionID: stripedSID, Subscriber: &prim},
		{Kind: "subscriber", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", SessionID: plainSID, Subscriber: &plain},
	}
	if edgeAnswered {
		out = append(out, relayscrape.Observation{Kind: "broadcast", Pod: "pod-b", Role: "edge", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &edge})
	}
	return out
}

// A striped viewer is ONE participant with its legs attributed to it, each
// with the pod it landed on — and the legs are in no count and no baseline.
func TestStripeLegsAttachToTheirViewerAndCountNowhere(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(round(stripedObs(1, true)))
	c.add(5 * time.Second)
	p.ObserveRelay(round(stripedObs(2, true)))

	snap := p.Snapshot()
	if len(snap.Live) != 1 {
		t.Fatalf("live broadcasts = %d, want 1", len(snap.Live))
	}
	bv := snap.Live[0]
	if bv.Viewers != 2 {
		t.Errorf("viewers = %d, want 2 (legs are not viewers)", bv.Viewers)
	}
	if bv.StripeLegs != 4 {
		t.Errorf("stripeLegs = %d, want all 4 live legs, attributed or not", bv.StripeLegs)
	}
	for name, want := range map[string]float64{"subscribers": 2, "subscribersFleetTotal": 2, "subscribersDropping": 2} {
		if got := bv.Metrics[name]; got != want {
			t.Errorf("%s = %v, want %v (legs are not audience)", name, got, want)
		}
	}
	if len(bv.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2: a leg never becomes a row", len(bv.Sessions))
	}

	v := findSession(snap, stripedSID)
	if v == nil {
		t.Fatal("the striped viewer has no row")
	}
	want := []LegView{
		{Member: 0, N: 2, Pod: "pod-a", Role: "origin", DatagramsDropped: ptr(10), QueueDepth: 2, RelayAgeMs: 0},
		{Member: 1, N: 2, Pod: "pod-b", Role: "edge", DatagramsDropped: ptr(20), RelayAgeMs: 0},
	}
	if got, w := mustJSON(t, v.Legs), mustJSON(t, want); got != w {
		t.Errorf("legs =\n %s\nwant\n %s", got, w)
	}
	if plain := findSession(snap, plainSID); plain == nil || len(plain.Legs) != 0 {
		t.Errorf("the unstriped viewer = %+v, want a row with no legs", plain)
	}

	// The R28 baseline is the viewers' own windowed drops (1 and 3), never a
	// leg's 10, 20, 50 or 70.
	if got, _ := relayFact(p, stripedSID, "peerMedianDropped"); got != 3 {
		t.Errorf("peerMedianDropped = %v, want 3 from the two viewers alone", got)
	}
	if got, _ := relayFact(p, stripedSID, "subscriberDropped"); got != 1 {
		t.Errorf("subscriberDropped = %v, want the primary's own 1", got)
	}
}

// The first observation of a leg has no window: its drops are absent, not
// zero and not a lifetime total.
func TestALegsFirstScrapeReportsNoDrops(t *testing.T) {
	p, _ := newProj()
	p.ObserveRelay(round(stripedObs(1, true)))
	v := findSession(p.Snapshot(), stripedSID)
	if v == nil || len(v.Legs) != 2 {
		t.Fatalf("striped viewer = %+v, want two legs", v)
	}
	for _, l := range v.Legs {
		if l.DatagramsDropped != nil {
			t.Errorf("leg %d reports %v dropped on its first scrape", l.Member, *l.DatagramsDropped)
		}
	}
}

// A complete round that lists a home leg nowhere is the relay saying the leg
// is gone; a partial round says nothing about the pods that did not answer.
func TestALegEndsOnlyWhenACompleteRoundOmitsIt(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(round(stripedObs(1, true)))
	c.add(5 * time.Second)

	partial := relayscrape.Round{Observations: stripedObs(2, false), Pods: 2, PodsAnswered: 1}
	p.ObserveRelay(partial)
	snap := p.Snapshot()
	if got := snap.Live[0].StripeLegs; got != 4 {
		t.Errorf("after a partial round stripeLegs = %d, want 4 (pod-b's legs are unknown, not gone)", got)
	}
	if v := findSession(snap, stripedSID); len(v.Legs) != 2 || v.Legs[1].RelayAgeMs != 5000 {
		t.Errorf("legs after a partial round = %+v, want pod-b's leg kept and aging", v.Legs)
	}

	c.add(5 * time.Second)
	p.ObserveRelay(round(stripedObs(3, false)))
	snap = p.Snapshot()
	if got := snap.Live[0].StripeLegs; got != 2 {
		t.Errorf("after a complete round without pod-b stripeLegs = %d, want 2", got)
	}
	if v := findSession(snap, stripedSID); len(v.Legs) != 1 || v.Legs[0].Pod != "pod-a" {
		t.Errorf("legs = %+v, want pod-a's alone", v.Legs)
	}
}

// With no round at all (scraping broken), a leg is not shown as live past
// the relay staleness bound.
func TestAQuietLegAgesOut(t *testing.T) {
	p, c := newProj()
	p.ObserveRelay(round(stripedObs(1, true)))
	c.add(RelayStaleAfter + time.Second)
	snap := p.Snapshot()
	if got := snap.Live[0].StripeLegs; got != 0 {
		t.Errorf("stripeLegs = %d after %s of silence, want 0", got, RelayStaleAfter+time.Second)
	}
	if v := findSession(snap, stripedSID); v != nil && len(v.Legs) != 0 {
		t.Errorf("legs = %+v, want none", v.Legs)
	}
}

// A PoP's observations carry no owner key (the PoP's telemetry route does
// not publish it), so its legs are counted and attached to nobody. Even a
// PoP leg that did carry home's viewer's key is never home's: a leg is joined
// only within the relay that served it (docs/78 D8).
func TestPopLegsAreCountedAndNeverAttachedAcrossRelays(t *testing.T) {
	p, _ := newProj()
	const issuer = "a1b2c3"
	obs := stripedObs(1, false)
	popSID := "80bbbbbbbbbbbbbbbbbbbbbb"
	popPrim := relayscrape.Subscriber{Key: "pp", SessionID: popSID}
	popLeg := relayscrape.Subscriber{Key: "pl", StripeLeg: true, StripeN: 2, StripeMember: 1}
	forged := relayscrape.Subscriber{Key: "pf", StripeLeg: true, StripeN: 2, StripeOwner: stripedOwner}
	popBC := relayscrape.Broadcast{PublisherActive: true, Role: "origin", SubscriberDetails: []relayscrape.Subscriber{popPrim, popLeg, forged}}
	obs = append(obs,
		relayscrape.Observation{Kind: "broadcast", Pod: "pop:" + issuer, Role: "pop-origin", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &popBC},
		relayscrape.Observation{Kind: "subscriber", Pod: "pop:" + issuer, Role: "pop-origin", BroadcastKey: "1a2b3c4d5e6f", SessionID: popSID, Subscriber: &popPrim},
	)
	p.ObserveRelay(round(obs))
	snap := p.Snapshot()

	home := findSession(snap, stripedSID)
	if len(home.Legs) != 1 || home.Legs[0].Pod != "pod-a" {
		t.Errorf("home viewer's legs = %+v, want its pod-a leg alone", home.Legs)
	}
	if pop := findSession(snap, popSID); pop == nil || len(pop.Legs) != 0 {
		t.Errorf("PoP viewer = %+v, want a row with no legs: nothing joins them", pop)
	}
	if got := snap.Live[0].StripeLegs; got != 4 {
		t.Errorf("stripeLegs = %d, want 4 (pod-a's two and the PoP's two)", got)
	}
}

// The owner token is minted per connection attempt, so a viewer that
// reconnects carries a NEW owner key — or none, on a delivery mode that
// cannot stripe. The join is the primary's CURRENT key: a leg of the dead
// attempt still listed for a round is attributed to nobody.
func TestAReconnectedPrimaryOwnsOnlyItsCurrentAttemptsLegs(t *testing.T) {
	const sid = "7a7a7a7a7a7a7a7a7a7a7a7a"
	obs := func(primOwner string, legs ...relayscrape.Subscriber) []relayscrape.Observation {
		prim := relayscrape.Subscriber{Key: "p-" + primOwner, SessionID: sid, StripeOwner: primOwner}
		bc := relayscrape.Broadcast{PublisherActive: true, Role: "origin",
			SubscriberDetails: append([]relayscrape.Subscriber{prim}, legs...)}
		return []relayscrape.Observation{
			{Kind: "broadcast", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", Broadcast: &bc},
			{Kind: "subscriber", Pod: "pod-a", Role: "origin", BroadcastKey: "1a2b3c4d5e6f", SessionID: sid, Subscriber: &prim},
		}
	}
	oldLeg := relayscrape.Subscriber{Key: "l-old", StripeLeg: true, StripeN: 2, StripeMember: 1, StripeOwner: "111111111111"}
	newLeg := relayscrape.Subscriber{Key: "l-new", StripeLeg: true, StripeN: 2, StripeMember: 1, StripeOwner: "222222222222"}

	p, c := newProj()
	p.ObserveRelay(round(obs("111111111111", oldLeg)))
	if v := findSession(p.Snapshot(), sid); len(v.Legs) != 1 {
		t.Fatalf("legs = %+v, want the first attempt's leg", v.Legs)
	}

	c.add(5 * time.Second)
	p.ObserveRelay(round(obs("222222222222", oldLeg, newLeg)))
	snap := p.Snapshot()
	if v := findSession(snap, sid); len(v.Legs) != 1 || snap.Live[0].StripeLegs != 2 {
		t.Errorf("after a reconnect legs = %+v (of %d), want the new attempt's one of two", v.Legs, snap.Live[0].StripeLegs)
	}

	// Reconnected in a mode that sends no owner token: nothing is its leg.
	c.add(5 * time.Second)
	p.ObserveRelay(round(obs("", newLeg)))
	if v := findSession(p.Snapshot(), sid); len(v.Legs) != 0 {
		t.Errorf("a primary with no owner key kept legs %+v", v.Legs)
	}
}

// The legs a card shows must not depend on which pod answered first.
func TestLegsDoNotDependOnPodAnswerOrder(t *testing.T) {
	render := func(reverse bool) string {
		p, c := newProj()
		for g := uint64(1); g <= 2; g++ {
			obs := stripedObs(g, true)
			if reverse {
				for i, j := 0, len(obs)-1; i < j; i, j = i+1, j-1 {
					obs[i], obs[j] = obs[j], obs[i]
				}
			}
			p.ObserveRelay(round(obs))
			c.add(5 * time.Second)
		}
		return mustJSON(t, p.Snapshot())
	}
	if a, b := render(false), render(true); a != b {
		t.Errorf("the card depends on pod answer order\n%s\n%s", a, b)
	}
}

func ptr(v float64) *float64 { return &v }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
