package relayscrape

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu       sync.Mutex
	byPod    map[string][][]byte
	observed []Observation
	rounds   []Round
}

func newFakeSink() *fakeSink { return &fakeSink{byPod: map[string][][]byte{}} }

func (f *fakeSink) StoreRelay(_ string, pod string, lines [][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPod[pod] = append(f.byPod[pod], lines...)
	return nil
}

func (f *fakeSink) ObserveRelay(r Round) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, r.Observations...)
	f.rounds = append(f.rounds, r)
}

func (f *fakeSink) records(t *testing.T) []Observation {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Observation
	for _, lines := range f.byPod {
		for _, ln := range lines {
			var o Observation
			if err := json.Unmarshal(ln, &o); err != nil {
				t.Fatalf("stored line is not an Observation: %v (%s)", err, ln)
			}
			out = append(out, o)
		}
	}
	return out
}

// Two pods with the R17 shapes: an origin holding the publisher, and an edge
// with its own viewers. The edge SESSION on the origin is plumbing and must
// never become a per-viewer record.
const originStatusz = `{
  "totals": {},
  "broadcasts": {
    "1a2b3c4d5e6f": {
      "publisherActive": true,
      "publisherSessionId": "000102030405060708090a0b",
      "role": "origin",
      "subscribers": 1,
      "edgeSessions": 1,
      "viewersGlobal": 3,
      "framesRelayed": 1200,
      "ingressFramesLost": 4,
      "subscriberDetails": [
        {"key":"aa11","sessionId":"111111111111111111111111","queueDepth":0,"dropped":2},
        {"key":"bb22","internal":true,"queueDepth":0,"dropped":0}
      ]
    }
  }
}`

const edgeStatusz = `{
  "totals": {},
  "broadcasts": {
    "1a2b3c4d5e6f": {
      "publisherActive": true,
      "role": "edge",
      "subscribers": 2,
      "framesRelayed": 1180,
      "subscriberDetails": [
        {"key":"cc33","sessionId":"222222222222222222222222","queueDepth":1,"dropped":0},
        {"key":"dd44","sessionId":"333333333333333333333333","queueDepth":0,"dropped":9,"reliable":true}
      ]
    }
  }
}`

func podServing(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/statusz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func addrOf(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestScrapeProducesOneRecordPerBroadcastAndSubscriber(t *testing.T) {
	origin := podServing(t, originStatusz)
	edge := podServing(t, edgeStatusz)
	sink := newFakeSink()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	s, err := New(Options{
		Resolve: StaticResolver([]string{addrOf(origin), addrOf(edge)}),
		Sink:    sink,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ScrapeOnce(context.Background())

	recs := sink.records(t)
	var broadcasts, subscribers int
	roles := map[string]string{}
	for _, r := range recs {
		switch r.Kind {
		case "broadcast":
			broadcasts++
			roles[r.Role] = r.SessionID
		case "subscriber":
			subscribers++
			if r.SessionID == "" {
				t.Error("a subscriber record has no sessionId; the join is impossible")
			}
		}
	}
	// One broadcast record per pod, and three real viewers across the fleet.
	if broadcasts != 2 {
		t.Errorf("broadcast records = %d, want 2 (one per pod)", broadcasts)
	}
	if subscribers != 3 {
		t.Errorf("subscriber records = %d, want 3 real viewers (the edge session is excluded)", subscribers)
	}
	// Only the ORIGIN's broadcast record carries a publisher session.
	if roles["origin"] != "000102030405060708090a0b" {
		t.Errorf("origin publisher session = %q", roles["origin"])
	}
	if roles["edge"] != "" {
		t.Errorf("edge broadcast record carries a publisher session %q; an edge hosts no publisher", roles["edge"])
	}
}

// An edge is plumbing, not an audience member (R17 W4 / docs/23). It must
// never appear as a viewer in the per-session store, or every hop would
// manufacture a viewer nobody is.
func TestScrapeExcludesEdgeSessions(t *testing.T) {
	origin := podServing(t, originStatusz)
	sink := newFakeSink()
	s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(origin)}), Sink: sink})
	s.ScrapeOnce(context.Background())

	for _, r := range sink.records(t) {
		if r.Subscriber != nil && r.Subscriber.Internal {
			t.Errorf("an internal edge session was stored as a viewer: %+v", r.Subscriber)
		}
	}
}

// The join TM1 made possible: one sessionId, two views.
func TestScrapeRecordsJoinBySessionID(t *testing.T) {
	origin := podServing(t, originStatusz)
	sink := newFakeSink()
	s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(origin)}), Sink: sink})
	s.ScrapeOnce(context.Background())

	var found *Observation
	for _, r := range sink.records(t) {
		if r.Kind == "subscriber" && r.SessionID == "111111111111111111111111" {
			rec := r
			found = &rec
		}
	}
	if found == nil {
		t.Fatal("the viewer's relay-side record is missing")
	}
	if found.Subscriber.Dropped != 2 || found.Subscriber.Key != "aa11" {
		t.Errorf("relay view = %+v", found.Subscriber)
	}
	// The display key and the join key stay independent (D2).
	if found.Subscriber.Key == found.SessionID {
		t.Error("statusz key and sessionId must remain separate handles")
	}
}

// A pod that disappears mid-round costs only its own records — one dead pod
// must never blind the fleet.
func TestScrapeSurvivesAPodDisappearing(t *testing.T) {
	origin := podServing(t, originStatusz)
	dead := podServing(t, edgeStatusz)
	dead.Close() // gone before the round starts

	sink := newFakeSink()
	s, _ := New(Options{
		Resolve: StaticResolver([]string{addrOf(origin), addrOf(dead)}),
		Sink:    sink,
	})
	s.ScrapeOnce(context.Background())

	recs := sink.records(t)
	if len(recs) == 0 {
		t.Fatal("a dead pod blinded the whole round")
	}
	for _, r := range recs {
		if r.Role == "edge" {
			t.Error("records from the dead pod appeared")
		}
	}
}

// Unknown /statusz fields must not break the scrape: the endpoint has grown
// additively through R9/R17/R19/R21 and will keep doing so.
func TestScrapeIgnoresUnknownStatuszFields(t *testing.T) {
	future := `{"broadcasts":{"1a2b3c4d5e6f":{
	  "role":"origin","publisherActive":true,"somethingFromR29":42,
	  "subscriberDetails":[{"key":"aa","sessionId":"111111111111111111111111","brandNew":true}]}},
	  "totals":{"newTotal":1}}`
	pod := podServing(t, future)
	sink := newFakeSink()
	s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(pod)}), Sink: sink})
	s.ScrapeOnce(context.Background())

	if len(sink.records(t)) != 2 {
		t.Errorf("records = %d, want 2 despite unknown fields", len(sink.records(t)))
	}
}

func TestScrapeIgnoresBadResponses(t *testing.T) {
	for _, body := range []string{"", "not json", "[1,2,3]"} {
		pod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		sink := newFakeSink()
		s, _ := New(Options{Resolve: StaticResolver([]string{addrOf(pod)}), Sink: sink})
		s.ScrapeOnce(context.Background())
		if n := len(sink.records(t)); n != 0 {
			t.Errorf("body %q produced %d records, want 0", body, n)
		}
		pod.Close()
	}
}

// A session shorter than the scrape interval is invisible to the relay side,
// and its rollup must SAY so rather than being silently treated as complete.
func TestCoverageNeverOverclaims(t *testing.T) {
	start := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Second

	cases := []struct {
		name string
		seen bool
		end  time.Time
		want string
	}{
		{"never observed", false, start.Add(time.Minute), "none"},
		{"shorter than the interval", true, start.Add(2 * time.Second), "partial"},
		{"exactly one interval", true, start.Add(interval), "partial"},
		{"two intervals", true, start.Add(2 * interval), "full"},
		{"a long session", true, start.Add(30 * time.Minute), "full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoverageFor(tc.seen, start, tc.end, interval); got != tc.want {
				t.Errorf("coverage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScraperCoverageTracksWhatItSaw(t *testing.T) {
	origin := podServing(t, originStatusz)
	sink := newFakeSink()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s, _ := New(Options{
		Resolve: StaticResolver([]string{addrOf(origin)}), Sink: sink,
		Now: func() time.Time { return now },
	})
	s.ScrapeOnce(context.Background())

	start := now.Add(-time.Minute)
	if got := s.Coverage("111111111111111111111111", start, now); got != "full" {
		t.Errorf("observed session coverage = %q, want full", got)
	}
	// A session the relay never saw — one that lived and died between scrapes.
	if got := s.Coverage("ffffffffffffffffffffffff", start, now); got != "none" {
		t.Errorf("unobserved session coverage = %q, want none", got)
	}
}

func TestPodNameIsFilenameSafe(t *testing.T) {
	for addr, want := range map[string]string{
		"10.42.0.7:2112": "10-42-0-7",
		"[::1]:2112":     "--1",
		"pod-a:2112":     "pod-a",
	} {
		if got := podName(addr); got != want {
			t.Errorf("podName(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A round's `Complete` flag is what licenses the live projection to read a
// broadcast's ABSENCE as its ending, so the accounting behind it has to be
// exact in both directions: a pod that answered with no broadcasts at all
// still answered (its empty answer IS the evidence that its broadcasts are
// over), and a pod that failed makes the whole round unusable for that
// inference.
func TestRoundCompletenessDistinguishesEmptyFromAbsent(t *testing.T) {
	empty := podServing(t, `{"totals":{},"broadcasts":{}}`)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(dead.Close)

	quiet := newFakeSink()
	s, err := New(Options{Resolve: StaticResolver([]string{addrOf(empty)}), Sink: quiet})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.ScrapeOnce(t.Context())
	if len(quiet.rounds) != 1 {
		t.Fatalf("rounds = %d, want 1", len(quiet.rounds))
	}
	if r := quiet.rounds[0]; !r.Complete || r.PodsAnswered != 1 {
		t.Errorf("a pod carrying no broadcasts read as unanswered: %+v — the fleet's quietest moment would be its least trustworthy", r)
	}

	partial := newFakeSink()
	s2, err := New(Options{Resolve: StaticResolver([]string{addrOf(empty), addrOf(dead)}), Sink: partial})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s2.ScrapeOnce(t.Context())
	if r := partial.rounds[0]; r.Complete || r.PodsAnswered != 1 || r.Pods != 2 {
		t.Errorf("a round with a dead pod claimed to be complete: %+v", r)
	}
}
