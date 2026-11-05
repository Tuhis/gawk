package readapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// R36 TU6: the broadcast timeline reads the home fleet's relay records only
// (docs/78 D8), so a lane for a session a PoP served has no relay-side
// figures. The lane says which PoP served it rather than leaving that to be
// inferred from an absence.
func TestATimelineLaneNamesThePopThatServedIt(t *testing.T) {
	f := newFixture(t)
	const issuer = "a1b2c3"
	const popSID = "80cccccccccccccccccccccc"
	const homeSID = "000c0c0c0c0c0c0c0c0c0c0c"
	seed := func(sid, iss string) {
		t.Helper()
		if err := f.writer.Accept(ingest.Accepted{
			SessionID: sid, BroadcastKey: bkey, Role: "viewer", Issuer: iss, Final: true,
			App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer"},
			StartedAtMs: f.now.Add(-time.Minute).UnixMilli(), ReceivedAt: f.now,
			Samples: samplesOf(healthyViewerStats(10)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(popSID, issuer)
	seed(homeSID, "")

	// The link-home pod's file holds home's record and the PoP's, as the
	// scraper stores them. This row was never joined (the test finalizer runs
	// no JoinRelay), so its PoP's name has to come from the PoP's records.
	at := f.now.Add(-30 * time.Second).UnixMilli()
	line := func(v map[string]any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := f.store.AppendRelay(f.now.UTC().Format(store.DateLayout), "pod-a", [][]byte{
		line(map[string]any{"kind": "broadcast", "atMs": at, "pod": "pod-a", "role": "origin", "broadcastKey": bkey,
			"broadcast": map[string]any{"publisherActive": true, "framesRelayed": 400}}),
		line(map[string]any{"kind": "broadcast", "atMs": at, "pod": relayscrape.PopPodPrefix + issuer, "role": "pop-origin",
			"popName": "Assembly 2026", "broadcastKey": bkey,
			"broadcast": map[string]any{"publisherActive": true, "framesRelayed": 900}}),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := f.api.GetBroadcast(bkey, BroadcastQuery{Since: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	lanes := map[string]Lane{}
	for _, l := range d.Lanes {
		lanes[l.SessionID] = l
	}
	if l := lanes[popSID]; l.Issuer != issuer || l.Pop != "Assembly 2026" {
		t.Errorf("PoP session's lane issuer %q pop %q, want %s and its display name", l.Issuer, l.Pop, issuer)
	}
	if l := lanes[homeSID]; l.Issuer != "" || l.Pop != "" {
		t.Errorf("home session's lane issuer %q pop %q, want neither", l.Issuer, l.Pop)
	}
	// What is READ does not change: the relay lane is still home's alone. A
	// PoP record leaking into it would read as a re-home to the PoP.
	if len(d.Rehomes) != 0 {
		t.Errorf("rehomes = %+v, want none: the PoP's records are not home's relay", d.Rehomes)
	}
	for _, l := range d.Lanes {
		if l.Kind == "relay" && l.Note != "" {
			t.Errorf("relay lane note %q, want home's observation found", l.Note)
		}
	}
}

// A live session's lane carries the projection's issuer and PoP name.
func TestALiveLaneNamesItsPop(t *testing.T) {
	f := newFixture(t)
	const issuer = "d4e5f6"
	const sid = "80dddddddddddddddddddddd"
	proj := live.New(func() time.Time { return f.now })
	popSub := relayscrape.Subscriber{SessionID: sid}
	popBC := relayscrape.Broadcast{PublisherActive: true, Role: "origin"}
	proj.ObserveRelay(relayscrape.Round{Complete: true, Pods: 1, PodsAnswered: 1, Observations: []relayscrape.Observation{
		{Kind: "broadcast", Pod: relayscrape.PopPodPrefix + issuer, Role: "pop-origin", PopName: "LAN party",
			BroadcastKey: bkey, Broadcast: &popBC},
		{Kind: "subscriber", Pod: relayscrape.PopPodPrefix + issuer, Role: "pop-origin", PopName: "LAN party",
			BroadcastKey: bkey, SessionID: sid, Subscriber: &popSub},
	}})
	snap := proj.Snapshot()
	if len(snap.Live) != 1 || len(snap.Live[0].Sessions) != 1 {
		t.Fatalf("live = %+v, want the PoP's one viewer", snap.Live)
	}
	if v := snap.Live[0].Sessions[0]; v.Issuer != issuer || v.Pop != "LAN party" {
		t.Errorf("live session issuer %q pop %q, want %s and LAN party", v.Issuer, v.Pop, issuer)
	}

	api, err := New(Options{Store: f.store, Live: proj, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	rows := api.liveRowsFor(bkey)
	if len(rows) != 1 || rows[0].Issuer != issuer || rows[0].Config["pop"] != "LAN party" {
		t.Fatalf("live rows = %+v, want the PoP's issuer and name", rows)
	}
	if l := api.sessionLane(rows[0], BroadcastQuery{}, 0); l.Issuer != issuer || l.Pop != "LAN party" {
		t.Errorf("live lane issuer %q pop %q, want %s and LAN party", l.Issuer, l.Pop, issuer)
	}
}
