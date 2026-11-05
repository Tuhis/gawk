package relayscrape

import (
	"context"
	"strings"
	"testing"
)

// A striped viewer as the relay publishes it (R36 TU5): the primary carries
// the viewer's sessionId and the owner key; its legs carry the same owner
// key, their (n, member) and NO sessionId — a leg is never sent the telemetry
// hello. Member 0 is a real member and arrives without the field.
const stripedStatusz = `{
  "broadcasts": {
    "1a2b3c4d5e6f": {
      "publisherActive": true, "role": "origin", "subscribers": 1, "viewersGlobal": 1,
      "subscriberDetails": [
        {"key":"prim","sessionId":"444444444444444444444444","dropped":1,"striped":true,"stripeN":3,"stripeOwner":"0a1b2c3d4e5f"},
        {"key":"leg0","dropped":2,"stripeLeg":true,"stripeN":3,"stripeOwner":"0a1b2c3d4e5f"},
        {"key":"leg2","dropped":5,"stripeLeg":true,"stripeN":3,"stripeMember":2,"stripeOwner":"0a1b2c3d4e5f"}
      ]
    }
  }
}`

// Legs are kept — on the broadcast record, with every stripe field and the
// owner key — and never become a per-subscriber record, whose join key is
// the sessionId a leg does not have.
func TestScrapeKeepsStripeLegsOnTheBroadcastRecordOnly(t *testing.T) {
	sink := newFakeSink()
	s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(podServing(t, stripedStatusz))}), Sink: sink})
	s.ScrapeOnce(context.Background())

	var subs []Observation
	var details []Subscriber
	for _, r := range sink.records(t) {
		switch r.Kind {
		case "subscriber":
			subs = append(subs, r)
		case "broadcast":
			details = append(details, r.Broadcast.SubscriberDetails...)
		default:
			t.Errorf("unexpected record kind %q", r.Kind)
		}
	}
	if len(subs) != 1 || subs[0].Subscriber.Key != "prim" {
		t.Fatalf("subscriber records = %+v, want the primary alone", subs)
	}
	if got := subs[0].Subscriber; got.StripeOwner != "0a1b2c3d4e5f" || !got.Striped || got.StripeN != 3 {
		t.Errorf("stored primary = %+v, want owner, striped and stripeN kept", got)
	}
	if len(details) != 3 {
		t.Fatalf("subscriberDetails = %d entries, want all 3 (legs are stored on the broadcast record)", len(details))
	}
	for _, d := range details[1:] {
		if !d.StripeLeg || d.StripeOwner != "0a1b2c3d4e5f" || d.StripeN != 3 || d.SessionID != "" {
			t.Errorf("stored leg = %+v, want stripeLeg, owner, n and no sessionId", d)
		}
	}
	if details[1].StripeMember != 0 || details[2].StripeMember != 2 {
		t.Errorf("leg members = %d, %d, want 0 and 2", details[1].StripeMember, details[2].StripeMember)
	}
	// And on disk, not only after a round trip through this type.
	for _, lines := range sink.byPod {
		for _, ln := range lines {
			if strings.Contains(string(ln), `"kind":"broadcast"`) && strings.Count(string(ln), `"stripeOwner":"0a1b2c3d4e5f"`) != 3 {
				t.Errorf("stored broadcast line does not carry every owner key: %s", ln)
			}
		}
	}
}
