package relayscan_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
)

// fakePod is one relay pod's ops listener: the two §4.5 routes, gated on the
// bearer token exactly as the relay gates them.
type fakePod struct {
	name       string
	version    string
	broadcasts []relayscan.Broadcast
	token      string
	// failBroadcasts / failConfig make one endpoint answer 500.
	failBroadcasts bool
	failConfig     bool
	hits           atomic.Int64
}

func (p *fakePod) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if p.token != "" && r.Header.Get("Authorization") != "Bearer "+p.token {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /internal/admin/broadcasts", func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		if !auth(w, r) {
			return
		}
		if p.failBroadcasts {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(relayscan.BroadcastsResponse{
			Schema: relayscan.SchemaBroadcasts, Pod: p.name, Broadcasts: p.broadcasts,
		})
	})
	mux.HandleFunc("GET /internal/admin/config", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if p.failConfig {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(relayscan.ConfigResponse{
			Schema: relayscan.SchemaConfig, Pod: p.name, Version: p.version,
			Config: map[string]any{"addr": ":4433", "publishSecret": "<set>"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func bc(id, key, role string, active bool, ip string, local, global int) relayscan.Broadcast {
	return relayscan.Broadcast{
		ID: id, Key: key, Role: role, PublisherActive: active, PublisherRemoteIP: ip,
		StartedAt:    time.Date(2026, 8, 20, 14, 0, 11, 0, time.UTC),
		ViewersLocal: local, ViewersGlobal: uint32(global),
	}
}

// The A-record fan-out: every pod is scraped and each broadcast is merged
// across the pods carrying it, with the origin's whole-broadcast facts winning.
func TestScanFansOutAndMergesAcrossPods(t *testing.T) {
	originPod := &fakePod{name: "gawk-server-0", version: "1.42.0", token: "tok",
		broadcasts: []relayscan.Broadcast{bc("ABC234", "3f9a1c2b4d5e", "origin", true, "203.0.113.7", 12, 340)}}
	edgePod := &fakePod{name: "gawk-server-1", version: "1.42.0", token: "tok",
		broadcasts: []relayscan.Broadcast{bc("ABC234", "3f9a1c2b4d5e", "edge", false, "", 328, 0)}}

	sc, err := relayscan.New(relayscan.Options{
		Resolve: relayscan.StaticResolver(edgePod.start(t), originPod.start(t)),
		Token:   "tok",
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.PodsResolved != 2 || snap.PodsAnswered != 2 {
		t.Fatalf("coverage = %d/%d pods", snap.PodsAnswered, snap.PodsResolved)
	}
	if len(snap.Broadcasts) != 1 {
		t.Fatalf("broadcasts = %+v", snap.Broadcasts)
	}
	got := snap.Broadcasts[0]
	if got.ID != "ABC234" || got.Key != "3f9a1c2b4d5e" {
		t.Fatalf("aggregate identity = %+v", got)
	}
	// The origin's facts win even though the edge pod was scraped first.
	if !got.PublisherActive || got.PublisherRemoteIP != "203.0.113.7" || got.ViewersGlobal != 340 {
		t.Fatalf("origin facts did not win: %+v", got)
	}
	if len(got.Pods) != 2 {
		t.Fatalf("placements = %+v", got.Pods)
	}
	for _, p := range snap.Pods {
		if !p.Reachable || p.Version != "1.42.0" || p.Config["addr"] != ":4433" {
			t.Fatalf("pod row = %+v", p)
		}
	}
	if _, ok := snap.Broadcast("abc234"); !ok {
		t.Fatalf("Snapshot.Broadcast is case-sensitive; it must find the raw ID however it is cased")
	}
}

// A pod that cannot answer degrades ITSELF and nothing else — the portal must
// stay usable exactly when a relay is in trouble.
func TestPodFailureDegradesOnlyThatPod(t *testing.T) {
	good := &fakePod{name: "good", version: "1.42.0",
		broadcasts: []relayscan.Broadcast{bc("BBB234", "k1", "origin", true, "198.51.100.9", 3, 3)}}
	bad := &fakePod{name: "bad", failBroadcasts: true, failConfig: true}

	sc, _ := relayscan.New(relayscan.Options{
		Resolve: relayscan.StaticResolver(good.start(t), bad.start(t)),
	})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("one failing pod failed the whole aggregate: %v", err)
	}
	if snap.PodsResolved != 2 || snap.PodsAnswered != 1 {
		t.Fatalf("coverage = %d/%d", snap.PodsAnswered, snap.PodsResolved)
	}
	if len(snap.Broadcasts) != 1 || snap.Broadcasts[0].ID != "BBB234" {
		t.Fatalf("healthy pod's broadcasts lost: %+v", snap.Broadcasts)
	}
	var sawUnreachable bool
	for _, p := range snap.Pods {
		if p.Reachable {
			continue
		}
		sawUnreachable = true
		if p.Err == "" {
			t.Fatalf("unreachable pod carries no error to render")
		}
	}
	if !sawUnreachable {
		t.Fatalf("the failing pod was not marked unreachable: %+v", snap.Pods)
	}
}

// A config-endpoint failure must not hide live broadcasts: the two endpoints
// are scraped independently on purpose.
func TestConfigFailureKeepsBroadcastsVisible(t *testing.T) {
	pod := &fakePod{name: "p", failConfig: true,
		broadcasts: []relayscan.Broadcast{bc("CCC234", "k", "origin", true, "203.0.113.1", 1, 1)}}
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(pod.start(t))})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Broadcasts) != 1 {
		t.Fatalf("broadcasts hidden by a config failure: %+v", snap.Broadcasts)
	}
	if !snap.Pods[0].Reachable || snap.Pods[0].ConfigErr == "" {
		t.Fatalf("pod row = %+v", snap.Pods[0])
	}
}

// A missing or wrong credential is a per-pod failure, not a panic and not an
// aggregate failure.
func TestWrongTokenDegradesPod(t *testing.T) {
	pod := &fakePod{name: "p", token: "right"}
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(pod.start(t)), Token: "wrong"})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Pods[0].Reachable || !strings.Contains(snap.Pods[0].Err, "401") {
		t.Fatalf("pod row = %+v", snap.Pods[0])
	}
}

// The ≤2 s cache: repeated calls inside the window must not re-scrape, and the
// window must actually expire.
func TestSnapshotIsCachedForAtMostTTL(t *testing.T) {
	pod := &fakePod{name: "p", broadcasts: []relayscan.Broadcast{bc("DDD234", "k", "origin", true, "203.0.113.2", 1, 1)}}
	now := time.Unix(1_700_000_000, 0)
	sc, _ := relayscan.New(relayscan.Options{
		Resolve: relayscan.StaticResolver(pod.start(t)),
		Now:     func() time.Time { return now },
	})

	for range 5 {
		if _, err := sc.Snapshot(context.Background()); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
	}
	if got := pod.hits.Load(); got != 1 {
		t.Fatalf("five calls inside the cache window hit the pod %d times, want 1", got)
	}

	now = now.Add(relayscan.DefaultCacheTTL - time.Millisecond)
	if _, err := sc.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := pod.hits.Load(); got != 1 {
		t.Fatalf("a call just inside the TTL re-scraped (%d hits)", got)
	}

	now = now.Add(2 * time.Millisecond) // now past the TTL
	if _, err := sc.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := pod.hits.Load(); got != 2 {
		t.Fatalf("a call past the TTL did not re-scrape (%d hits)", got)
	}

	// Invalidate is what a mutation calls so the operator sees their own
	// action immediately.
	sc.Invalidate()
	if _, err := sc.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := pod.hits.Load(); got != 3 {
		t.Fatalf("Invalidate did not force a re-scrape (%d hits)", got)
	}
}

// A relay speaking a schema this build does not know is a per-pod failure, not
// a silently misread pod.
func TestUnknownSchemaDegradesPod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schema": "gawk.admin.broadcasts.v99", "pod": "p"})
	}))
	t.Cleanup(srv.Close)
	sc, _ := relayscan.New(relayscan.Options{Resolve: relayscan.StaticResolver(strings.TrimPrefix(srv.URL, "http://"))})
	snap, err := sc.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Pods[0].Reachable {
		t.Fatalf("a pod answering an unknown schema was treated as reachable")
	}
}

func TestResolveFailureIsAnError(t *testing.T) {
	sc, _ := relayscan.New(relayscan.Options{
		Resolve: func(context.Context) ([]string, error) { return nil, context.DeadlineExceeded },
	})
	if _, err := sc.Snapshot(context.Background()); err == nil {
		t.Fatalf("a resolution failure must fail the call: the fleet is unknown, not empty")
	}
}

// The coalesced scan must not run on the first caller's request context.
//
// Snapshot collapses concurrent misses into one fleet scrape and hands every
// waiter the same result. If that scrape inherits the INITIATOR's context, one
// operator closing a tab mid-scan fails it for everybody parked on it: every
// concurrent /broadcasts answers 503 "the relay fleet could not be enumerated"
// and a kill's publisher resolution answers 400, with every relay pod up and
// the other requests perfectly healthy. Worse, nothing is cached, so the next
// caller pays for the whole fan-out again.
func TestAnAbortedInitiatorDoesNotPoisonTheCoalescedScan(t *testing.T) {
	pod := &fakePod{name: "gawk-server-0", version: "1.42.0",
		broadcasts: []relayscan.Broadcast{bc("ABC234", "3f9a1c2b4d5e", "origin", true, "203.0.113.7", 12, 340)}}
	addr := pod.start(t)

	var (
		entered  = make(chan struct{})
		once     sync.Once
		release  = make(chan struct{})
		resolves atomic.Int64
	)
	sc, err := relayscan.New(relayscan.Options{
		CacheTTL: time.Minute,
		Resolve: func(ctx context.Context) ([]string, error) {
			resolves.Add(1)
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return []string{addr}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type result struct {
		snap relayscan.Snapshot
		err  error
	}
	aborting, cancel := context.WithCancel(context.Background())
	initiator := make(chan result, 1)
	go func() {
		snap, err := sc.Snapshot(aborting)
		initiator <- result{snap, err}
	}()
	<-entered

	waiter := make(chan result, 1)
	go func() {
		snap, err := sc.Snapshot(context.Background())
		waiter <- result{snap, err}
	}()

	cancel()
	close(release)

	got := <-waiter
	if got.err != nil {
		t.Fatalf("a healthy request got %v because another caller walked away", got.err)
	}
	if got.snap.PodsResolved != 1 || got.snap.PodsAnswered != 1 {
		t.Fatalf("coverage = %d/%d, want the fleet the scan actually reached",
			got.snap.PodsAnswered, got.snap.PodsResolved)
	}
	if len(got.snap.Broadcasts) != 1 || got.snap.Broadcasts[0].ID != "ABC234" {
		t.Fatalf("broadcasts = %+v", got.snap.Broadcasts)
	}

	// The initiator's OWN request failed, which is correct: it asked and left.
	if first := <-initiator; !errors.Is(first.err, context.Canceled) {
		t.Fatalf("the aborted caller got %v, want context.Canceled", first.err)
	}

	// And the scan completed and cached, so the abort cost the fleet nothing.
	if _, err := sc.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot after the abort: %v", err)
	}
	if n := resolves.Load(); n != 1 {
		t.Fatalf("the fleet was resolved %d times: an aborted caller threw away the scrape "+
			"every other caller was waiting on", n)
	}
}
