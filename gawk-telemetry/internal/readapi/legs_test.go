package readapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

const legOwner = "0a1b2c3d4e5f"

// A striped viewer as two relay pods publish it (R36 TU5). The origin holds
// the primary (sessionId + owner key), the member-0 leg and a leg from a relay
// predating the owner key; the edge holds the member-1 leg and a leg whose
// owner no primary carries. Legs carry no sessionId: they are never sent the
// telemetry hello. gen scales the cumulative drop counters.
func legOriginStatusz(sid string, gen uint64) string {
	return fmt.Sprintf(`{"broadcasts":{%q:{"publisherActive":true,"role":"origin","subscribers":1,"viewersGlobal":1,"framesRelayed":%d,
  "subscriberDetails":[
    {"key":"prim","sessionId":%q,"dropped":%d,"keyframesSent":4,"striped":true,"stripeN":2,"stripeOwner":%q},
    {"key":"leg0","dropped":%d,"stripeLeg":true,"stripeN":2,"stripeOwner":%q},
    {"key":"old","dropped":%d,"stripeLeg":true,"stripeN":2,"stripeMember":1}
  ]}}}`, bkey, 1000*gen, sid, 7, legOwner, 10*gen, legOwner, 50*gen)
}

func legEdgeStatusz(gen uint64) string {
	return fmt.Sprintf(`{"broadcasts":{%q:{"publisherActive":true,"role":"edge","framesRelayed":%d,
  "subscriberDetails":[
    {"key":"leg1","dropped":%d,"stripeLeg":true,"stripeN":2,"stripeMember":1,"stripeOwner":%q},
    {"key":"stray","dropped":%d,"stripeLeg":true,"stripeN":4,"stripeMember":3,"stripeOwner":"ffffffffffff"}
  ]}}}`, bkey, 990*gen, 20*gen, legOwner, 70*gen)
}

func TestBroadcastTimelineAttributesStripeLegsToTheirViewer(t *testing.T) {
	f := newFixture(t)
	const sid = "5c5c5c5c5c5c5c5c5c5c5c5c"
	stats := healthyViewerStats(30)
	samples := make([]ingest.Sample, 0, len(stats))
	for i, s := range stats {
		samples = append(samples, ingest.Sample{TMs: float64(i) * 2000, Stats: s})
	}
	if err := f.writer.Accept(ingest.Accepted{
		SessionID: sid, BroadcastKey: bkey, Role: "viewer", Final: true,
		App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer", Browser: "Chrome 152", OS: "Windows"},
		StartedAtMs: f.now.Add(-time.Minute).UnixMilli(), ReceivedAt: f.now,
		Samples: samples, Events: []ingest.Event{{TMs: 1000, Kind: "watching"}},
	}); err != nil {
		t.Fatal(err)
	}

	var gen atomic.Uint64
	serve := func(body func() string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/statusz" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body()))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	origin := serve(func() string { return legOriginStatusz(sid, gen.Load()) })
	edge := serve(func() string { return legEdgeStatusz(gen.Load()) })
	at := f.now.Add(-30 * time.Second)
	proj := live.New(func() time.Time { return at })
	sc, err := relayscrape.New(relayscrape.Options{
		Resolve: relayscrape.StaticResolver([]string{
			strings.TrimPrefix(origin.URL, "http://"), strings.TrimPrefix(edge.URL, "http://"),
		}),
		Sink: stripeSink{store: f.store, proj: proj},
		Now:  func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	for g := uint64(1); g <= 3; g++ {
		gen.Store(g)
		sc.ScrapeOnce(context.Background())
		at = at.Add(5 * time.Second)
	}

	d, err := f.api.GetBroadcast(bkey, BroadcastQuery{Since: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var viewer *Lane
	for i := range d.Lanes {
		switch d.Lanes[i].Kind {
		case "viewer":
			viewer = &d.Lanes[i]
		case "relay":
			if len(d.Lanes[i].Legs) != 0 {
				t.Error("the relay lane carries legs")
			}
		}
	}
	if len(d.Lanes) != 2 || viewer == nil {
		t.Fatalf("lanes = %d (viewer %v), want the viewer and the relay: a leg never takes a lane", len(d.Lanes), viewer != nil)
	}
	if len(viewer.Legs) != 2 {
		t.Fatalf("viewer legs = %+v, want its two attributed legs", viewer.Legs)
	}
	originPod := relayPodOf(origin)
	edgePod := relayPodOf(edge)
	for i, want := range []struct {
		member  int
		pod     string
		role    string
		rate    float64
		dropped float64
	}{
		{0, originPod, "origin", 2, 30},
		{1, edgePod, "edge", 4, 60},
	} {
		l := viewer.Legs[i]
		if l.Member != want.member || l.N != 2 || l.Pod != want.pod || l.Role != want.role {
			t.Errorf("leg %d = member %d n %d on %s/%s, want member %d on %s/%s",
				i, l.Member, l.N, l.Pod, l.Role, want.member, want.pod, want.role)
		}
		if l.Unit != LegUnit || l.DatagramsDropped != want.dropped {
			t.Errorf("leg %d unit %q dropped %v, want %q and the last cumulative %v", i, l.Unit, l.DatagramsDropped, LegUnit, want.dropped)
		}
		if len(l.Points) != 2 || l.Points[0].Value != want.rate || l.Points[1].Value != want.rate {
			t.Errorf("leg %d points = %+v, want two intervals at %v dropped/s", i, l.Points, want.rate)
		}
	}

	// And nothing that counts reads a leg: the stored broadcast and session
	// verdicts see one viewer, the primary's own drops and baseline.
	relayLines, err := f.store.ReadRelay(f.now.UTC().Format(store.DateLayout))
	if err != nil {
		t.Fatal(err)
	}
	bf := rules.NewFacts(bkey, "broadcast", "")
	setBroadcastRelayFacts(bf, relayLines, bkey)
	assertRelayFacts(t, "broadcast facts", bf, map[string]float64{
		"subscribers": 1, "subscribersFleetTotal": 1, "subscribersDropping": 1, "peerMedianDropped": 7,
	})
	ref, err := f.store.FindSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := f.store.ReadSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
	row := rollup.Compute(in)
	assertRelayFacts(t, "session facts", f.api.factsFor(row, in, relayLines), map[string]float64{
		"subscribers": 1, "subscriberDropped": 7,
	})
	JoinRelay(&row, relayLines, 5*time.Second)
	if row.RelayPod != originPod || row.Relay["dropped"] != 7 {
		t.Errorf("joined row pod %q dropped %v, want the primary's %s and 7", row.RelayPod, row.Relay["dropped"], originPod)
	}

	// The live card agrees: one viewer, its two legs, four legs in all.
	snap := proj.Snapshot()
	if len(snap.Live) != 1 || snap.Live[0].Viewers != 1 || snap.Live[0].StripeLegs != 4 {
		t.Fatalf("live card = %+v, want one viewer and four stripe legs", snap.Live)
	}
	for _, s := range snap.Live[0].Sessions {
		if s.SessionID == sid && len(s.Legs) != 2 {
			t.Errorf("live viewer legs = %+v, want two", s.Legs)
		}
	}
}

// relayPodOf is the pod name the scraper files a test server's records under:
// its host, which for two loopback test servers is the same name — the roles
// tell their legs apart.
func relayPodOf(srv *httptest.Server) string {
	host, _, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	return strings.ReplaceAll(host, ".", "-")
}

// legObs is one stored broadcast record holding the given entries.
func legObs(atMs int64, pod string, subs ...relaySubscriber) relayObservation {
	return relayObservation{
		Kind: "broadcast", AtMs: atMs, Pod: pod, Role: "origin", BroadcastKey: bkey,
		Broadcast: &relayBroadcast{SubscriberDetails: subs},
	}
}

func primaryEntry(sid string) relaySubscriber {
	return relaySubscriber{Key: "prim", SessionID: sid, StripeOwner: legOwner}
}

func legEntry(key string, member int, dropped uint64) relaySubscriber {
	return relaySubscriber{Key: key, StripeLeg: true, StripeN: 4, StripeMember: member, StripeOwner: legOwner, Dropped: dropped}
}

// Every re-dial is a new leg session, so a long session can collect many.
// The newest stay under the cap, the count of the rest is stated.
func TestALaneCarriesAtMostMaxLegsPerLane(t *testing.T) {
	const sid = "5d5d5d5d5d5d5d5d5d5d5d5d"
	var obs []relayObservation
	for i := range MaxLegsPerLane + 4 {
		at := int64(1_000_000 + i*5000)
		obs = append(obs, legObs(at, "pod-a", primaryEntry(sid), legEntry(fmt.Sprintf("leg%02d", i), i%4, 0)))
	}
	lanes := []Lane{{Kind: "broadcaster", SessionID: "5e5e5e5e5e5e5e5e5e5e5e5e"}, {Kind: "viewer", SessionID: sid}}
	attachLegs(lanes, obs, 0, 0)
	if len(lanes[0].Legs) != 0 {
		t.Error("a broadcaster lane got legs")
	}
	v := lanes[1]
	if len(v.Legs) != MaxLegsPerLane || v.LegsOmitted != 4 {
		t.Fatalf("legs = %d, omitted %d; want %d and 4", len(v.Legs), v.LegsOmitted, MaxLegsPerLane)
	}
	// The four oldest (leg00..leg03, members 0..3) are the ones left out, so
	// the first kept is leg04, member 0, dialled fifth.
	if v.Legs[0].Member != 0 || len(v.Legs[0].Points) != 0 {
		t.Errorf("first kept leg = %+v, want leg04 with a single observation", v.Legs[0])
	}
}

// A long leg's series is reduced worst-of, so a burst survives it; a counter
// that went backwards is a break, never negative traffic; and the timeline's
// window bounds the legs like it bounds every lane's samples.
func TestALegSeriesIsBoundedAndHonest(t *testing.T) {
	const sid = "5f5f5f5f5f5f5f5f5f5f5f5f"
	var obs []relayObservation
	var dropped uint64
	for i := range 1000 {
		switch i {
		case 500:
			dropped += 5000 // one burst: 1000 dropped/s over its 5 s interval
		case 700:
			dropped = 0 // the relay restarted under the leg
		default:
			dropped++
		}
		obs = append(obs, legObs(int64(1_000_000+i*5000), "pod-a", primaryEntry(sid), legEntry("leg", 1, dropped)))
	}
	lanes := []Lane{{Kind: "viewer", SessionID: sid}}
	attachLegs(lanes, obs, 0, 0)
	l := lanes[0].Legs[0]
	if len(l.Points) > LegPoints || !l.Downsampled {
		t.Fatalf("points = %d (downsampled %v), want at most %d and marked", len(l.Points), l.Downsampled, LegPoints)
	}
	var peak float64
	for _, p := range l.Points {
		if p.Value < 0 {
			t.Errorf("negative drop rate %v at %d", p.Value, p.AtMs)
		}
		peak = max(peak, p.Value)
	}
	if peak != 1000 {
		t.Errorf("peak = %v dropped/s, want the burst's 1000 kept", peak)
	}

	// The window: only the observations inside it.
	lanes = []Lane{{Kind: "viewer", SessionID: sid}}
	attachLegs(lanes, obs, 1_000_000+10*5000, 1_000_000+20*5000)
	if got := len(lanes[0].Legs[0].Points); got != 10 {
		t.Errorf("windowed points = %d, want the 10 intervals inside it", got)
	}
}

// The owner token is minted per connection attempt. A viewer that reconnected
// owns each attempt's key, so both attempts' legs sit under its one lane; a
// primary scraped before its telemetry hello landed has no sessionId yet and
// names nobody — its legs wait for an entry that does.
func TestAReconnectedViewerOwnsEachAttemptsLegs(t *testing.T) {
	const sid = "7b7b7b7b7b7b7b7b7b7b7b7b"
	first := relaySubscriber{Key: "p1", SessionID: sid, StripeOwner: "111111111111"}
	second := relaySubscriber{Key: "p2", SessionID: sid, StripeOwner: "222222222222"}
	early := relaySubscriber{Key: "p3", StripeOwner: "333333333333"} // before its hello
	leg := func(key, owner string) relaySubscriber {
		return relaySubscriber{Key: key, StripeLeg: true, StripeN: 2, StripeMember: 1, StripeOwner: owner}
	}
	obs := []relayObservation{
		legObs(1_000_000, "pod-a", first, leg("l1", "111111111111")),
		legObs(1_005_000, "pod-b", second, leg("l2", "222222222222"), early, leg("l3", "333333333333")),
	}
	lanes := []Lane{{Kind: "viewer", SessionID: sid}}
	attachLegs(lanes, obs, 0, 0)
	got := lanes[0].Legs
	if len(got) != 2 || got[0].Pod != "pod-a" || got[1].Pod != "pod-b" {
		t.Errorf("legs = %+v, want the first attempt's on pod-a and the second's on pod-b, and not the hello-less primary's", got)
	}
}

// No owner match, no attribution: a leg from a relay predating the owner key
// or whose primary was never scraped is attached to nobody.
func TestALegIsNeverGuessedOntoAViewer(t *testing.T) {
	const sid = "6c6c6c6c6c6c6c6c6c6c6c6c"
	old := relaySubscriber{Key: "old", StripeLeg: true, StripeN: 2, StripeMember: 1}
	orphan := relaySubscriber{Key: "orph", StripeLeg: true, StripeN: 2, StripeOwner: "ffffffffffff"}
	unowned := relaySubscriber{Key: "prim", SessionID: sid} // a primary with no owner key
	lanes := []Lane{{Kind: "viewer", SessionID: sid}}
	attachLegs(lanes, []relayObservation{legObs(1_000_000, "pod-a", unowned, old, orphan)}, 0, 0)
	if len(lanes[0].Legs) != 0 {
		t.Errorf("legs = %+v, want none: nothing joins them to this viewer", lanes[0].Legs)
	}
}
