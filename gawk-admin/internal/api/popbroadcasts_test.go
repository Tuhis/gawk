package api_test

import (
	"net/http"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

type wirePopBroadcast struct {
	wireBroadcast
	Pop *struct {
		Name             string `json:"name"`
		Forwarded        bool   `json:"forwarded"`
		Label            string `json:"label"`
		PublisherLanAddr string `json:"publisherLanAddr"`
		LocalViewers     int    `json:"localViewers"`
		OutsideViewers   int    `json:"outsideViewers"`
	} `json:"pop"`
}

// Party broadcasts are in the Broadcasts view (docs/76 item 29, §7.2): a
// forwarded one is the fleet's row with the PoP's badge; a party-only one
// home does not carry gets a row of its own from the PoP's report, with
// its ban state, so it can be killed before anyone outside watches.
func TestListBroadcastsMergesPartyBroadcasts(t *testing.T) {
	h := newHarness(t)
	snap := liveSnapshot("ABC234", "3f9a1c2b4d5e", "203.0.113.7")
	snap.Pops = []relayscan.PopAggregate{{Pod: "gawk-server-0", PopState: adminapi.PopState{
		Name: "assembly-2026", Linked: true,
		Broadcasts: []adminapi.PopBroadcast{
			{ID: "ABC234", Label: "tuhis", PublisherLanAddr: "192.168.1.31", LocalViewers: 4, OutsideViewers: 7, Forwarded: true},
			{ID: "xyz789", Label: "demo", PublisherLanAddr: "192.168.1.40", LocalViewers: 3},
		},
	}}}
	h.fleet.set(snap)
	if _, err := h.store.CreateBan(t.Context(), store.Ban{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "XYZ789"}, CreatedBy: "op"}); err != nil {
		t.Fatal(err)
	}

	var body struct {
		Broadcasts []wirePopBroadcast `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	if len(body.Broadcasts) != 2 {
		t.Fatalf("broadcasts = %+v, want the fleet row and the party-only row", body.Broadcasts)
	}
	fwd, party := body.Broadcasts[0], body.Broadcasts[1]
	if fwd.ID != "ABC234" || fwd.Key != "3f9a1c2b4d5e" || fwd.Pop == nil || fwd.Pop.Name != "assembly-2026" ||
		!fwd.Pop.Forwarded || fwd.Pop.PublisherLanAddr != "192.168.1.31" || fwd.Pop.OutsideViewers != 7 {
		t.Fatalf("forwarded row = %+v pop %+v", fwd, fwd.Pop)
	}
	if fwd.PublisherRemoteIP != "203.0.113.7" || fwd.ViewersGlobal != 340 {
		t.Fatalf("the forwarded row lost its fleet data: %+v", fwd)
	}
	if party.ID != "XYZ789" || party.Key != "" || !party.PublisherActive || party.Pop == nil ||
		party.Pop.Forwarded || party.Pop.Label != "demo" || party.Pop.LocalViewers != 3 || party.ViewersGlobal != 3 {
		t.Fatalf("party-only row = %+v pop %+v", party, party.Pop)
	}
	if party.PublisherRemoteIP != "" || len(party.Pods) != 0 {
		t.Fatalf("a party-only row claims fleet placement: %+v", party)
	}
	if party.BanState == nil || !party.BanState.Banned {
		t.Fatalf("party-only banState = %+v, want the ID ban", party.BanState)
	}
	if party.Links == nil || party.Links.Watch != "https://gawk.example/#/view/XYZ789" || party.Links.Telemetry != "" {
		t.Fatalf("party-only links = %+v", party.Links)
	}
}

// A fleet without PoPs is R39's view: no badge on any row.
func TestListBroadcastsWithoutPopsHasNoBadge(t *testing.T) {
	h := newHarness(t)
	h.fleet.set(liveSnapshot("ABC234", "k", "203.0.113.7"))
	var body struct {
		Broadcasts []wirePopBroadcast `json:"broadcasts"`
	}
	h.decode(http.MethodGet, "/api/v1/broadcasts", nil, http.StatusOK, &body)
	if len(body.Broadcasts) != 1 || body.Broadcasts[0].Pop != nil {
		t.Fatalf("broadcasts = %+v", body.Broadcasts)
	}
}
