package adminapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The gawk.admin.pops.v1 field names are the contract gawk-admin's relayscan
// parses (docs/76 §9); pin them so a rename breaks here first.
func TestPopsSchemaFieldNames(t *testing.T) {
	if SchemaPops != "gawk.admin.pops.v1" {
		t.Errorf("SchemaPops = %q", SchemaPops)
	}
	since := time.Date(2026, 10, 21, 18, 4, 12, 0, time.UTC)
	full := PopState{
		Name: "assembly-2026", UID: "u", Linked: true, Since: &since, Version: "0.44.0", Proto: 1,
		PublicAddrs: []string{"203.0.113.7"}, LanAddr: "192.168.1.20", NamedHost: "p-7k2m9x.pop.gawk.ioio.fi",
		Live: 3, Forwarded: 1, OutsideViewers: 7, UplinkMbps: 12.5, UplinkStreams: 1,
		Uplink: BudgetUse{MaxMbps: 25, MaxStreams: 3}, Downlink: BudgetUse{MaxMbps: 100, MaxStreams: 20},
		DownlinkStreams: 2, GuestsConnected: 40, OffersSent: 41,
		Broadcasts: []PopBroadcast{{ID: "5UP4XW", Label: "tuhis", PublisherLanAddr: "192.168.1.31", LocalViewers: 4, OutsideViewers: 7, Forwarded: true, Wall: "on"}},
		Health:     []HealthLine{{Key: "firewall", OK: true, Detail: "rule present"}},
	}
	want := []string{"broadcasts", "downlink", "downlinkStreams", "forwarded", "guestsConnected", "health", "lanAddr",
		"linked", "live", "name", "namedHost", "offersSent", "outsideViewers", "proto", "publicAddrs", "since", "uid",
		"uplink", "uplinkMbps", "uplinkStreams", "version"}
	if got := jsonKeys(t, full); !reflect.DeepEqual(got, want) {
		t.Errorf("PopState keys = %v\nwant %v", got, want)
	}
	if got := jsonKeys(t, full.Broadcasts[0]); !reflect.DeepEqual(got, []string{"forwarded", "id", "label", "localViewers", "outsideViewers", "publisherLanAddr", "wall"}) {
		t.Errorf("PopBroadcast keys = %v", got)
	}
	if got := jsonKeys(t, full.Health[0]); !reflect.DeepEqual(got, []string{"detail", "key", "ok"}) {
		t.Errorf("HealthLine keys = %v", got)
	}
	if got := jsonKeys(t, full.Uplink); !reflect.DeepEqual(got, []string{"maxMbps", "maxStreams"}) {
		t.Errorf("BudgetUse keys = %v", got)
	}
	if got := jsonKeys(t, PopsResponse{Schema: SchemaPops, Pod: "p"}); !reflect.DeepEqual(got, []string{"pod", "pops", "schema"}) {
		t.Errorf("PopsResponse keys = %v", got)
	}
	// An unlinked PoP's row omits what it has not reported.
	got := jsonKeys(t, PopState{Name: "lan"})
	for _, k := range got {
		if k != "name" && k != "uplink" && k != "downlink" {
			t.Errorf("an empty PopState marshals %q", k)
		}
	}
}
