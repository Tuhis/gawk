package readapi

import (
	"context"
	"encoding/json"
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

// stripedStatusz is one origin pod holding ONE striped viewer: its primary
// session plus four R30 stripe legs (docs/35 §5.8, MaxStripeLegs = 4). The
// relay marks legs `stripeLeg: true`, never `internal: true` (that is R17's
// edge marker), and its own viewersGlobal already excludes them (docs/35
// ST3). Legs are dialled from the primary's URL, so they carry the viewer's
// sessionId too. `gen` scales the cumulative drop counters: the legs drop in
// every window, the primary only ever dropped 7 and then stopped.
func stripedStatusz(sid string, gen uint64) string {
	legs := make([]string, 0, 4)
	for j := 1; j <= 4; j++ {
		legs = append(legs, fmt.Sprintf(
			`{"key":"leg%d","sessionId":%q,"dropped":%d,"stripeLeg":true,"stripeN":5,"stripeMember":%d}`,
			j, sid, 3*gen, j))
	}
	return fmt.Sprintf(`{
  "totals": {},
  "broadcasts": {
    %q: {
      "publisherActive": true,
      "role": "origin",
      "subscribers": 1,
      "viewersGlobal": 1,
      "framesRelayed": %d,
      "subscriberDetails": [
        {"key":"prim","sessionId":%q,"dropped":7,"striped":true,"stripeN":5},
        %s
      ]
    }
  }
}`, bkey, 1000*gen, sid, strings.Join(legs, ",\n        "))
}

// stripeSink is the production wiring (cmd/gawk-telemetry's scrapeSink):
// every scrape lands in the store AND the live projection, so one fixture
// drives both read paths.
type stripeSink struct {
	store *store.Store
	proj  *live.Projection
}

func (s stripeSink) StoreRelay(date, pod string, lines [][]byte) error {
	return s.store.AppendRelay(date, pod, lines)
}
func (s stripeSink) ObserveRelay(r relayscrape.Round) { s.proj.ObserveRelay(r) }

// BUGS.md 2026-09-23: telemetry counted R30 stripe legs as audience, so every
// striped viewer read as five subscribers and viewer-count-gap fired on every
// broadcast with a striping-capable viewer.
func TestStripeLegsAreNotAudience(t *testing.T) {
	f := newFixture(t)
	const sid = "5e5e5e5e5e5e5e5e5e5e5e5e"
	// A minute-long viewer session that ends at f.now, so the scrapes below
	// fall inside its span (factsFor joins only observations within it).
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
	pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/statusz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(stripedStatusz(sid, gen.Load())))
	}))
	defer pod.Close()

	at := f.now.Add(-10 * time.Second)
	proj := live.New(func() time.Time { return at })
	sc, err := relayscrape.New(relayscrape.Options{
		Resolve: relayscrape.StaticResolver([]string{strings.TrimPrefix(pod.URL, "http://")}),
		Sink:    stripeSink{store: f.store, proj: proj},
		Now:     func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	for g := uint64(1); g <= 2; g++ {
		gen.Store(g)
		sc.ScrapeOnce(context.Background())
		at = at.Add(5 * time.Second)
	}

	// --- stored observations: legs are plumbing, like edges -----------------
	relayLines, err := f.store.ReadRelay(f.now.UTC().Format(store.DateLayout))
	if err != nil {
		t.Fatal(err)
	}
	subscriberObs := 0
	for _, ln := range relayLines {
		var o relayscrape.Observation
		if err := json.Unmarshal(ln, &o); err != nil {
			t.Fatal(err)
		}
		if o.Kind == "subscriber" {
			subscriberObs++
			if o.Subscriber.StripeLeg || strings.HasPrefix(o.Subscriber.Key, "leg") {
				t.Errorf("a stripe leg was stored as a subscriber observation: %s", ln)
			}
		}
	}
	if subscriberObs != 2 {
		t.Errorf("subscriber observations = %d, want 2 (the primary, once per scrape)", subscriberObs)
	}

	// --- live aggregate ------------------------------------------------------
	snap := proj.Snapshot()
	if len(snap.Live) != 1 {
		t.Fatalf("live broadcasts = %d, want 1", len(snap.Live))
	}
	bv := snap.Live[0]
	for name, want := range map[string]float64{
		"subscribersFleetTotal": 1, "subscribers": 1, "subscribersDropping": 0,
	} {
		if got := bv.Metrics[name]; got != want {
			t.Errorf("live %s = %v, want %v (stripe legs are not viewers)", name, got, want)
		}
	}
	for _, fd := range bv.Findings {
		if fd.ID == "viewer-count-gap" {
			t.Errorf("live viewer-count-gap fired on one striped viewer: %+v", fd)
		}
	}

	// --- /v1/broadcasts/{key}/diagnose ---------------------------------------
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()
	bc := getReport(t, srv.URL+"/v1/broadcasts/"+bkey+"/diagnose")
	assertNoViewerCountGap(t, "broadcast diagnose", bc)

	bf := rules.NewFacts(bkey, "broadcast", "")
	setBroadcastRelayFacts(bf, relayLines, bkey)
	assertRelayFacts(t, "broadcast diagnose", bf, map[string]float64{
		"subscribersFleetTotal": 1, "subscribers": 1, "subscribersDropping": 1, "peerMedianDropped": 7,
	})

	// --- /v1/sessions/{id}/diagnose -----------------------------------------
	sess := getReport(t, srv.URL+"/v1/sessions/"+sid+"/diagnose")
	assertNoViewerCountGap(t, "session diagnose", sess)

	// The facts the session diagnose evaluates, built exactly as Diagnose does.
	ref, err := f.store.FindSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := f.store.ReadSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
	sf := f.api.factsFor(rollup.Compute(in), in, relayLines)
	// subscriberDropped is the viewer's OWN relay-side figure: a leg sharing
	// its sessionId must not overwrite the primary's 7 with a leg's count.
	assertRelayFacts(t, "session diagnose", sf, map[string]float64{
		"subscribersFleetTotal": 1, "subscribers": 1, "subscribersDropping": 1, "subscriberDropped": 7,
	})
}

func getReport(t *testing.T, url string) rules.Report {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	var rep rules.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func assertNoViewerCountGap(t *testing.T, path string, rep rules.Report) {
	t.Helper()
	for _, fd := range rep.Findings {
		if fd.ID == "viewer-count-gap" {
			t.Errorf("%s: viewer-count-gap fired on one striped viewer: %+v", path, fd)
		}
	}
}

func assertRelayFacts(t *testing.T, path string, f *rules.Facts, want map[string]float64) {
	t.Helper()
	for name, w := range want {
		if got, ok := f.Relay(name); !ok || got != w {
			t.Errorf("%s: relay.%s = %v (ok=%v), want %v (stripe legs are not viewers)", path, name, got, ok, w)
		}
	}
}
