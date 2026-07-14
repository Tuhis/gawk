package transport

// R9 M4: connection-outcome counters. The pre-upgrade rejection paths run
// before any WebTransport upgrade, so they can be driven with plain
// httptest requests — no QUIC needed. The accepted outcomes are asserted in
// the e2e relay test (TestRelayPublishToSubscribe).

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

func newOutcomeServer(t *testing.T, cfg config.Config, opts hub.Options) (*Server, *metrics.ServerMetrics, *hub.Registry) {
	t.Helper()
	r := hub.NewRegistry(discardLog, opts)
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	srv := New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, discardLog, sm)
	return srv, sm, r
}

func connectReq(target string, pathValues map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodConnect, target, nil)
	req.RemoteAddr = "203.0.113.20:40000" // non-loopback so no limiter bypass
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	return req
}

func TestPublishOutcomeUnauthorized(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{PublishSecret: "hunter2"}, hub.Options{})
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish?secret=wrong", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeUnauthorized); got != 1 {
		t.Errorf("publish/unauthorized = %v, want 1", got)
	}
}

func TestPublishOutcomeNotFoundOnBadReclaim(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish/zzzzzz", map[string]string{"id": "zzzzzz"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeNotFound); got != 1 {
		t.Errorf("publish/not_found = %v, want 1", got)
	}
}

func TestPublishOutcomeConflictOnActiveReclaim(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish/"+id, map[string]string{"id": id}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeConflict); got != 1 {
		t.Errorf("publish/conflict = %v, want 1", got)
	}
}

func TestPublishOutcomeLimitRejectedAtCapacity(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{MaxBroadcasts: 1})
	if _, pub, err := r.StartPublish(""); err != nil {
		t.Fatalf("StartPublish: %v", err)
	} else {
		defer pub.Close()
	}

	w := httptest.NewRecorder()
	srv.handlePublish(w, connectReq("https://relay/publish", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := sm.ConnectionCount("publish", metrics.OutcomeLimitRejected); got != 1 {
		t.Errorf("publish/limit_rejected = %v, want 1", got)
	}
}

func TestSubscribeOutcomeNotFound(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{}, hub.Options{})
	w := httptest.NewRecorder()
	srv.handleSubscribe(w, connectReq("https://relay/subscribe/zzzzzz", map[string]string{"id": "zzzzzz"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := sm.ConnectionCount("subscribe", metrics.OutcomeNotFound); got != 1 {
		t.Errorf("subscribe/not_found = %v, want 1", got)
	}
}

func TestSubscribeOutcomeLimitRejected(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{MaxSubscribers: 1})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()
	if _, err := r.Subscribe(id, &fakeMinimalConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	w := httptest.NewRecorder()
	srv.handleSubscribe(w, connectReq("https://relay/subscribe/"+id, map[string]string{"id": id}))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := sm.ConnectionCount("subscribe", metrics.OutcomeLimitRejected); got != 1 {
		t.Errorf("subscribe/limit_rejected = %v, want 1", got)
	}
}

func TestRateLimitedNotDoubleCountedAsOutcome(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t,
		config.Config{ConnRateLimit: 1, ConnBurstLimit: 1}, hub.Options{})

	req := connectReq("https://relay/subscribe/abc", map[string]string{"id": "abc"})
	w := httptest.NewRecorder()
	srv.handleSubscribe(w, req) // burst=1: allowed (404s later, that's fine)
	w = httptest.NewRecorder()
	srv.handleSubscribe(w, req) // limited
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := sm.RateLimitedCount(); got != 1 {
		t.Errorf("rate_limited = %v, want 1", got)
	}
	// Rate-limited attempts are counted only in gawk_rate_limited_total.
	if got := sm.ConnectionCount("subscribe", metrics.OutcomeLimitRejected); got != 0 {
		t.Errorf("subscribe/limit_rejected = %v, want 0 for rate-limited attempts", got)
	}
}

// fakeMinimalConn satisfies hub.Conn for occupancy-only subscriptions.
type fakeMinimalConn struct{}

func (fakeMinimalConn) SendDatagram([]byte) error { return nil }
func (fakeMinimalConn) OpenKeyframeStream() (hub.KeyframeStream, error) {
	return nil, http.ErrNotSupported
}
func (fakeMinimalConn) CloseWithError(uint32, string) error { return nil }
