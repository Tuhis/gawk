package relayscrape

import (
	"context"
	"strings"
	"testing"
)

// R87 (docs/91 §4, §8 SG3): a striped deep-buffer viewer as the relay
// publishes it — a DVR primary with its connection's QUIC counters, and a
// DVR stripe leg with its own — plus the broadcast's ingress bytes.
const pathStatusz = `{
  "broadcasts": {
    "1a2b3c4d5e6f": {
      "publisherActive": true, "role": "origin", "subscribers": 1,
      "ingressDatagramBytes": 600000, "keyframeBytesIn": 400000,
      "subscriberDetails": [
        {"key":"prim","sessionId":"555555555555555555555555","dvr":true,"dvrBufferMs":3000,
         "striped":true,"stripeN":2,"stripeOwner":"0a1b2c3d4e5f",
         "pathSrttMs":36.5,"pathMinRttMs":33.25,"pathBytesSent":91000000,"pathPacketsSent":76000,"pathPacketsLost":840},
        {"key":"leg1","dvr":true,"stripeLeg":true,"stripeN":2,"stripeMember":1,"stripeOwner":"0a1b2c3d4e5f",
         "pathSrttMs":38,"pathMinRttMs":34,"pathBytesSent":90000000,"pathPacketsSent":75000,"pathPacketsLost":800}
      ]
    }
  }
}`

func TestScrapeKeepsPathStatsAndIngressBytes(t *testing.T) {
	sink := newFakeSink()
	s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(podServing(t, pathStatusz))}), Sink: sink})
	s.ScrapeOnce(context.Background())

	var prim *Subscriber
	var bc *Broadcast
	for _, r := range sink.records(t) {
		switch r.Kind {
		case "subscriber":
			prim = r.Subscriber
		case "broadcast":
			bc = r.Broadcast
		}
	}
	if prim == nil || bc == nil {
		t.Fatalf("records missing: subscriber %v, broadcast %v", prim, bc)
	}
	want := Subscriber{PathSRTTMs: 36.5, PathMinRTTMs: 33.25, PathBytesSent: 91000000, PathPacketsSent: 76000, PathPacketsLost: 840}
	if prim.PathSRTTMs != want.PathSRTTMs || prim.PathMinRTTMs != want.PathMinRTTMs ||
		prim.PathBytesSent != want.PathBytesSent || prim.PathPacketsSent != want.PathPacketsSent ||
		prim.PathPacketsLost != want.PathPacketsLost {
		t.Errorf("primary path stats = %+v, want %+v", prim, want)
	}
	if !prim.DVR || prim.DVRBufferMs != 3000 || !prim.Striped || prim.StripeN != 2 {
		t.Errorf("primary DVR/stripe state = %+v", prim)
	}
	if bc.IngressBytes() != 1000000 {
		t.Errorf("ingress bytes = %d, want 1000000", bc.IngressBytes())
	}
	if len(bc.SubscriberDetails) != 2 {
		t.Fatalf("subscriberDetails = %d entries, want 2", len(bc.SubscriberDetails))
	}
	leg := bc.SubscriberDetails[1]
	if !leg.StripeLeg || !leg.DVR || leg.StripeMember != 1 || leg.PathPacketsLost != 800 || leg.PathSRTTMs != 38 {
		t.Errorf("DVR leg = %+v, want its own path stats and stripe filter", leg)
	}
	for _, lines := range sink.byPod {
		for _, ln := range lines {
			if strings.Contains(string(ln), `"kind":"subscriber"`) && !strings.Contains(string(ln), `"pathPacketsLost":840`) {
				t.Errorf("stored subscriber line lost the path counters: %s", ln)
			}
		}
	}
}

func TestPathLossRatioAndStreamBitrate(t *testing.T) {
	cases := []struct {
		name      string
		cur, prev Subscriber
		want      float64
		ok        bool
	}{
		{"lifetime", Subscriber{PathPacketsSent: 1000, PathPacketsLost: 10}, Subscriber{}, 0.01, true},
		{"window", Subscriber{PathPacketsSent: 3000, PathPacketsLost: 25}, Subscriber{PathPacketsSent: 1000, PathPacketsLost: 5}, 0.01, true},
		{"nothing sent", Subscriber{PathPacketsSent: 1000}, Subscriber{PathPacketsSent: 1000}, 0, false},
		{"not reported", Subscriber{}, Subscriber{}, 0, false},
		{"went backwards", Subscriber{PathPacketsSent: 500, PathPacketsLost: 1}, Subscriber{PathPacketsSent: 1000, PathPacketsLost: 5}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := c.cur.PathLossRatio(c.prev)
			if ok != c.ok || got != c.want {
				t.Errorf("PathLossRatio = %v, %v; want %v, %v", got, ok, c.want, c.ok)
			}
		})
	}

	prev := Broadcast{IngressDatagramBytes: 1_000_000, KeyframeBytesIn: 500_000}
	cur := Broadcast{IngressDatagramBytes: 5_000_000, KeyframeBytesIn: 3_000_000}
	if got, ok := StreamBitrateBps(prev, cur, 0, 5000); !ok || got != 10.4e6 {
		t.Errorf("StreamBitrateBps = %v, %v; want 10.4e6", got, ok)
	}
	if _, ok := StreamBitrateBps(cur, prev, 0, 5000); ok {
		t.Error("a counter that went backwards measured a bitrate")
	}
	if _, ok := StreamBitrateBps(Broadcast{}, Broadcast{}, 0, 5000); ok {
		t.Error("a relay that reports no ingress bytes measured a bitrate of 0")
	}
}
