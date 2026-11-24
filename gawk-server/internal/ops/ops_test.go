package ops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func testHandler(t *testing.T) (http.Handler, *hub.Registry) {
	t.Helper()
	return testHandlerReady(t, nil)
}

func testHandlerReady(t *testing.T, ready func() bool) (http.Handler, *hub.Registry) {
	t.Helper()
	r := hub.NewRegistry(discardLog, hub.Options{})
	promReg := metrics.NewBaseRegistry("test-version")
	promReg.MustRegister(metrics.NewRegistryCollector(r))
	return Handler(r, nil, promReg, discardLog, ready, nil, nil), r
}

func TestHandlerServesAllRoutes(t *testing.T) {
	h, r := testHandler(t)
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	if w := get("/healthz"); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("/healthz = %d %q, want 200 ok", w.Code, w.Body.String())
	}

	// A nil ready func means always ready.
	if w := get("/readyz"); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("/readyz = %d %q, want 200 ok", w.Code, w.Body.String())
	}

	w := get("/statusz")
	if w.Code != http.StatusOK {
		t.Fatalf("/statusz = %d, want 200", w.Code)
	}
	var rs hub.RegistryStats
	if err := json.NewDecoder(w.Body).Decode(&rs); err != nil {
		t.Fatalf("/statusz JSON: %v", err)
	}
	if rs.Totals.Broadcasts != 1 {
		t.Errorf("/statusz totals.broadcasts = %d, want 1", rs.Totals.Broadcasts)
	}

	w = get("/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`gawk_build_info{version="test-version"} 1`,
		"go_goroutines",                   // Go collector wired
		"gawk_broadcasts_active 1",        // registry collector wired
		"gawk_broadcast_publisher_active", // per-broadcast series present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// /readyz follows the injected readiness (R17 W1): 200 while ready, 503 once
// the drain has begun — the kubelet readiness contract for rolling updates.
func TestReadyzFollowsDrainState(t *testing.T) {
	ready := true
	h, _ := testHandlerReady(t, func() bool { return ready })

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return w
	}

	if w := get(); w.Code != http.StatusOK {
		t.Errorf("/readyz while ready = %d, want 200", w.Code)
	}
	ready = false
	if w := get(); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while draining = %d, want 503", w.Code)
	}
	if w := get(); !strings.Contains(w.Body.String(), "draining") {
		t.Errorf("/readyz draining body = %q, want to mention draining", w.Body.String())
	}
}

func TestRunDisabledWithEmptyAddr(t *testing.T) {
	h, _ := testHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, "", h, discardLog) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run with empty addr = %v, want nil (disabled, immediate return)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run with empty addr did not return immediately")
	}
}

func TestServeOverTCPAndGracefulShutdown(t *testing.T) {
	h, _ := testHandler(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, h, discardLog) }()

	resp, err := http.Get("http://" + l.Addr().String() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics over TCP: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "gawk_build_info") {
		t.Errorf("GET /metrics = %d, body missing gawk_build_info", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel = %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down after ctx cancel")
	}
}

// countingRooms counts snapshot builds: the handler asks its rooms source
// once per document, right after the registry snapshot.
type countingRooms struct{ builds atomic.Int64 }

func (c *countingRooms) Stats() map[string]roomsrv.RoomStats {
	c.builds.Add(1)
	return nil
}

func statuszBroadcasts(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/statusz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/statusz = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := w.Body.String()
	var rs hub.RegistryStats
	if err := json.Unmarshal([]byte(body), &rs); err != nil {
		t.Fatalf("/statusz JSON: %v", err)
	}
	return rs.Totals.Broadcasts, body
}

// The public route must not build a snapshot per request: each one walks
// every broadcast under Registry.mu, the lock fan-out takes per datagram.
func TestCachedStatuszBuildsOneSnapshotPerWindow(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	rooms := &countingRooms{}
	clock := time.Unix(1_800_000_000, 0)
	h := cachedStatuszHandler(r, rooms, discardLog, time.Second, func() time.Time { return clock })

	if n, _ := statuszBroadcasts(t, h); n != 0 {
		t.Fatalf("first snapshot lists %d broadcasts, want 0", n)
	}
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	for i := 0; i < 50; i++ {
		clock = clock.Add(10 * time.Millisecond) // 500 ms in total, inside the window
		if n, _ := statuszBroadcasts(t, h); n != 0 {
			t.Fatalf("request %d inside the window lists %d broadcasts, want the stored 0", i, n)
		}
	}
	if got := rooms.builds.Load(); got != 1 {
		t.Fatalf("51 requests inside one window built %d snapshots, want 1", got)
	}

	clock = clock.Add(time.Second)
	if n, _ := statuszBroadcasts(t, h); n != 1 {
		t.Fatalf("after the window the snapshot lists %d broadcasts, want 1", n)
	}
	if got := rooms.builds.Load(); got != 2 {
		t.Fatalf("builds after the window = %d, want 2", got)
	}
}

// A flood that lands on an expired snapshot must share one rebuild.
func TestCachedStatuszConcurrentRequestsShareOneBuild(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	rooms := &countingRooms{}
	h := cachedStatuszHandler(r, rooms, discardLog, time.Hour, time.Now)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/statusz", nil))
			if w.Code != http.StatusOK {
				t.Errorf("/statusz = %d, want 200", w.Code)
			}
		}()
	}
	wg.Wait()
	if got := rooms.builds.Load(); got != 1 {
		t.Fatalf("64 concurrent requests built %d snapshots, want 1", got)
	}
}

// The cached document is the uncached one, byte for byte.
func TestCachedStatuszMatchesUncachedBytes(t *testing.T) {
	r := hub.NewRegistry(discardLog, hub.Options{})
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	_, fresh := statuszBroadcasts(t, StatuszHandler(r, nil, discardLog))
	_, cached := statuszBroadcasts(t, CachedStatuszHandler(r, nil, discardLog, PublicStatuszMaxAge))
	if fresh != cached {
		t.Fatalf("cached /statusz differs from the uncached document:\n fresh  %s\n cached %s", fresh, cached)
	}
}

// The ops listener is internal and its readers (the telemetry scrape, the
// e2e tiers, an operator pairing a capture) expect the current state.
func TestOpsListenerStatuszIsNotCached(t *testing.T) {
	h, r := testHandler(t)
	if n, _ := statuszBroadcasts(t, h); n != 0 {
		t.Fatalf("first read lists %d broadcasts, want 0", n)
	}
	if _, _, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if n, _ := statuszBroadcasts(t, h); n != 1 {
		t.Fatalf("read right after a publish lists %d broadcasts, want 1", n)
	}
}
