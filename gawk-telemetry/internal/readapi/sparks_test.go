package readapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
)

// R36 TU7: GET /live/sparks?broadcast=<key> serves one live broadcast's fps
// tails in the shape the live table draws, and /live does not grow by them.
func TestLiveSparksRoute(t *testing.T) {
	f := newFixture(t)
	now := f.now
	proj := live.New(func() time.Time { return now })
	const sid = "a1a1a1a1a1a1a1a1a1a1a1a1"
	start := now
	for b := range 3 {
		samples := make([]ingest.Sample, 0, 5)
		for i := range 5 {
			samples = append(samples, ingest.Sample{TMs: float64((b*5 + i) * 2000), Stats: map[string]any{"decoderFps": 59.94}})
		}
		now = start.Add(time.Duration((b*5+4)*2000+500) * time.Millisecond)
		proj.ObserveClient(ingest.Accepted{
			SessionID: sid, BroadcastKey: bkey, Role: "viewer",
			StartedAtMs: start.UnixMilli(), Samples: samples,
		}, "Chrome 152", "Windows", "0.33.2")
	}
	api, err := New(Options{Store: f.store, Live: proj, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	reads := f.store.SessionReads()
	code, body := get("/live/sparks?broadcast=" + bkey)
	if code != http.StatusOK {
		t.Fatalf("GET /live/sparks = %d %s", code, body)
	}
	if f.store.SessionReads() != reads {
		t.Error("a poll of the tails read a session file; it must answer from memory")
	}
	var got struct {
		AtMs     int64 `json:"atMs"`
		StepMs   int64 `json:"stepMs"`
		WindowMs int64 `json:"windowMs"`
		Sessions map[string]struct {
			Field     string     `json:"field"`
			Values    []*float64 `json:"values"`
			ThroughMs int64      `json:"throughMs"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.AtMs != now.UnixMilli() || got.WindowMs != 120000 || got.StepMs != 4000 {
		t.Errorf("grid = %+v", got)
	}
	s := got.Sessions[sid]
	if s.Field != "decoderFps" || len(s.Values) != 30 {
		t.Fatalf("tail = %+v, want 30 decoderFps buckets", s)
	}
	// Oldest first: the session is 30 s old, so the head of the window is
	// null — literally null on the wire, not 0 and not absent.
	if s.Values[0] != nil || s.Values[29] == nil || *s.Values[29] != 59.9 {
		t.Errorf("values = head %v tail %v, want null … 59.9", s.Values[0], s.Values[29])
	}
	if !strings.Contains(string(body), `"values":[null,`) {
		t.Errorf("a bucket without a sample is not null on the wire: %s", body)
	}

	// The snapshot does not carry them.
	if _, live := get("/live"); strings.Contains(string(live), `"values"`) {
		t.Errorf("/live grew a tail: %s", live)
	}

	for path, want := range map[string]int{
		"/live/sparks":                        http.StatusBadRequest,
		"/live/sparks?broadcast=not-a-key":    http.StatusBadRequest,
		"/live/sparks?broadcast=ffffffffffff": http.StatusNotFound,
	} {
		if code, _ := get(path); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}

	// No projection wired: nothing is live, so nothing has a tail.
	if _, err := f.api.LiveSparks(bkey); err == nil {
		t.Error("tails served without a projection")
	}
}
