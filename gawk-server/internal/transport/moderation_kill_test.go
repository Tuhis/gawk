package transport

// R39 AP3 (docs/42 §4.3, §9): Server.HandleBanAdded — the actuation half of
// moderation. AP2's tests cover the publish-path GATE; these cover the KILL.

import (
	"context"
	"crypto/tls"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// killSession records the close a kill sends to a publisher session.
type killSession struct {
	mu     sync.Mutex
	code   webtransport.SessionErrorCode
	reason string
	closed bool
}

func (s *killSession) CloseWithError(code webtransport.SessionErrorCode, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code, s.reason, s.closed = code, reason, true
	return nil
}

func (s *killSession) info() (webtransport.SessionErrorCode, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.reason, s.closed
}

// newOutcomeServerRegistry is newOutcomeServer over a registry the caller
// already owns — the edge harness builds its own, and the Server under test
// has to share it.
func newOutcomeServerRegistry(t *testing.T, cfg config.Config, r *hub.Registry) (*Server, *metrics.ServerMetrics, *hub.Registry) {
	t.Helper()
	sm := metrics.NewServerMetrics(prometheus.NewRegistry())
	srv := New(cfg, r, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }, discardLog, sm)
	return srv, sm, r
}

// killFixture is one live broadcast on this pod: a hub with a viewer, and a
// tracked publisher session at a known source address.
type killFixture struct {
	srv    *Server
	id     string
	pub    *hub.Publisher
	sess   *killSession
	viewer *edgeConn
}

func newKillFixture(t *testing.T, srv *Server, r *hub.Registry, remote string) *killFixture {
	t.Helper()
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &killFixture{srv: srv, id: id, pub: pub, sess: &killSession{}, viewer: &edgeConn{}}
	if _, err := r.Subscribe(id, f.viewer); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	srv.trackPublisher(id, f.sess, netip.MustParseAddr(remote))
	return f
}

// A broadcastId ban kills that broadcast: the publisher session gets 4006,
// every viewer gets 4006, and the hub is gone.
func TestHandleBanAddedKillsTheBannedBroadcast(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	f := newKillFixture(t, srv, r, "203.0.113.7")

	srv.HandleBanAdded(idBan(f.id, "fraudulent stream"))

	code, reason, closed := f.sess.info()
	if !closed || code != webtransport.SessionErrorCode(wire.CloseCodeTerminatedByOperator) {
		t.Errorf("publisher close = %d/%v, want %d/true", code, closed, wire.CloseCodeTerminatedByOperator)
	}
	if reason != terminationReason {
		t.Errorf("close reason = %q, want %q", reason, terminationReason)
	}
	if code, closed := f.viewer.closeInfo(); !closed || code != uint32(wire.CloseCodeTerminatedByOperator) {
		t.Errorf("viewer close = %d/%v, want %d/true", code, closed, wire.CloseCodeTerminatedByOperator)
	}
	if err := r.CheckSubscribe(f.id); err == nil {
		t.Error("the killed broadcast is still subscribable")
	}
	if got := sm.TerminationCount(); got != 1 {
		t.Errorf("gawk_moderation_terminations_total = %v, want 1", got)
	}
	// The publisher bookkeeping is cleared, so a later IP ban cannot kill a
	// session that is already gone (and re-count it).
	if _, ok := srv.PublisherRemote(f.id); ok {
		t.Error("the killed publisher is still tracked")
	}
}

// THE ONE THAT MATTERS FOR IP BANS (docs/42 §4.3): exactly the matching live
// publishers die, and nothing else does. A CIDR ban that over-reached would
// be a fleet-wide outage switch.
func TestHandleBanAddedIPKillsOnlyMatchingPublishers(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{MaxBroadcasts: 10})

	inside1 := newKillFixture(t, srv, r, "203.0.113.7")
	inside2 := newKillFixture(t, srv, r, "203.0.113.200")
	outside := newKillFixture(t, srv, r, "198.51.100.9")
	// A v4-mapped v6 peer must be caught by the plain v4 ban: the relay sees
	// whichever form the stack hands it, and an operator writes only one.
	mapped := newKillFixture(t, srv, r, "::ffff:203.0.113.42")
	// A v6 publisher outside the banned v4 space is untouched.
	v6 := newKillFixture(t, srv, r, "2001:db8::1")

	srv.HandleBanAdded(ipBan("203.0.113.0/24", "abusive host"))

	for name, f := range map[string]*killFixture{
		"203.0.113.7": inside1, "203.0.113.200": inside2, "v4-mapped 203.0.113.42": mapped,
	} {
		if _, _, closed := f.sess.info(); !closed {
			t.Errorf("publisher at %s survived the CIDR ban", name)
		}
		if err := r.CheckSubscribe(f.id); err == nil {
			t.Errorf("the broadcast from %s is still subscribable", name)
		}
	}
	for name, f := range map[string]*killFixture{
		"198.51.100.9": outside, "2001:db8::1": v6,
	} {
		if _, _, closed := f.sess.info(); closed {
			t.Errorf("publisher at %s was killed by an unrelated CIDR ban", name)
		}
		if code, closed := f.viewer.closeInfo(); closed {
			t.Errorf("viewer of the broadcast at %s was closed (code %d)", name, code)
		}
		if err := r.CheckSubscribe(f.id); err != nil {
			t.Errorf("the broadcast from %s was removed: %v", name, err)
		}
	}
	if got := sm.TerminationCount(); got != 3 {
		t.Errorf("gawk_moderation_terminations_total = %v, want 3 (one per killed broadcast)", got)
	}
}

// An already-expired ban kills nothing. The relay evaluates expiry itself, so
// a CR the janitor has not cleaned up yet (docs/42 §6) is inert on the
// actuation path exactly as it is on the publish path.
func TestHandleBanAddedIgnoresAnExpiredRecord(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	f := newKillFixture(t, srv, r, "203.0.113.7")

	past := now.Add(-time.Second)
	rec := idBan(f.id, "cooldown that already lapsed")
	rec.ExpiresAt = &past
	srv.HandleBanAdded(rec)

	if _, _, closed := f.sess.info(); closed {
		t.Error("an expired ban killed a live publisher")
	}
	if err := r.CheckSubscribe(f.id); err != nil {
		t.Errorf("an expired ban removed the hub: %v", err)
	}
	if got := sm.TerminationCount(); got != 0 {
		t.Errorf("terminations = %v, want 0", got)
	}

	// The same record, one second before its expiry, does kill — which is
	// what proves the test above measured expiry and not a broken fixture.
	future := now.Add(time.Minute)
	rec.ExpiresAt = &future
	srv.HandleBanAdded(rec)
	if _, _, closed := f.sess.info(); !closed {
		t.Fatal("an unexpired ban did not kill the publisher")
	}
}

// Idempotence. The informer re-delivers on every resync, and a fleet-wide ban
// reaches pods that never had the broadcast. Neither may error, panic, or
// inflate the counter.
func TestHandleBanAddedIsIdempotentAndSafeOnUnknownBroadcasts(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	f := newKillFixture(t, srv, r, "203.0.113.7")

	// A pod that never had this broadcast: no hub, no publisher, no event.
	srv.HandleBanAdded(idBan("ZZZ23Z", "somebody else's broadcast"))
	if got := sm.TerminationCount(); got != 0 {
		t.Errorf("terminations after a ban for an unknown broadcast = %v, want 0", got)
	}
	// An IP nobody here is publishing from.
	srv.HandleBanAdded(ipBan("192.0.2.0/24", "unrelated"))
	if got := sm.TerminationCount(); got != 0 {
		t.Errorf("terminations after an unmatched IP ban = %v, want 0", got)
	}

	srv.HandleBanAdded(idBan(f.id, "kill"))
	srv.HandleBanAdded(idBan(f.id, "kill")) // the resync re-delivery
	if got := sm.TerminationCount(); got != 1 {
		t.Errorf("terminations after a re-delivered ban = %v, want 1", got)
	}

	// A malformed target is logged and dropped, never widened into a ban on
	// everybody.
	srv.HandleBanAdded(moderation.Record{
		Target: moderation.Target{Type: moderation.TargetIP, Value: "not-a-cidr"}})
	srv.HandleBanAdded(moderation.Record{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "!!!"}})
	if got := sm.TerminationCount(); got != 1 {
		t.Errorf("terminations after malformed targets = %v, want 1", got)
	}
}

// The kill works with -cluster-mode OFF: enforcement is not a federation
// feature (docs/42 §4.3), and this is the single-pod deployment's whole path.
func TestHandleBanAddedWorksWithoutClusterMode(t *testing.T) {
	srv, _, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	if srv.cluster != nil || srv.edges != nil {
		t.Fatal("the fixture is in cluster mode; this test is about the single-pod path")
	}
	f := newKillFixture(t, srv, r, "203.0.113.7")

	srv.HandleBanAdded(idBan(f.id, "kill"))
	if _, _, closed := f.sess.info(); !closed {
		t.Error("a single-pod relay did not kill the banned broadcast")
	}
}

// An EDGE pod stops its upstream pull before terminating the local hub
// (docs/42 §4.3), so the re-attach loop cannot rebuild the hub the kill just
// removed. Driven through the real EdgeManager against a fake upstream —
// a stub would prove only that the call is made, not that the pull dies.
func TestHandleBanAddedStopsTheEdgePull(t *testing.T) {
	h := newEdgeHarness(t)
	up := newFakeUpstream()
	h.queue(up)
	const id = "K7XQ2M"
	if err := h.manager.EnsureEdge(context.Background(), id); err != nil {
		t.Fatalf("EnsureEdge: %v", err)
	}

	srv, sm, _ := newOutcomeServerRegistry(t, config.Config{}, h.registry)
	srv.edges = h.manager

	viewer := &edgeConn{}
	if _, err := h.registry.Subscribe(id, viewer); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	srv.HandleBanAdded(idBan(id, "kill"))

	// The upstream pull is gone: its session was closed, and the manager no
	// longer tracks an edge for the broadcast.
	select {
	case <-up.closed:
	default:
		t.Error("the upstream session was not closed — the edge pull survived the kill")
	}
	h.manager.mu.Lock()
	_, stillTracked := h.manager.edges[id]
	h.manager.mu.Unlock()
	if stillTracked {
		t.Error("the EdgeManager still tracks an edge for the killed broadcast")
	}

	if code, closed := viewer.closeInfo(); !closed || code != uint32(wire.CloseCodeTerminatedByOperator) {
		t.Errorf("edge viewer close = %d/%v, want %d/true", code, closed, wire.CloseCodeTerminatedByOperator)
	}
	if err := h.registry.CheckSubscribe(id); err == nil {
		t.Error("the edge hub survived the kill")
	}
	// An edge hub has no broadcaster session of its own, but the kill is
	// still a real enforcement event on this pod and must be counted.
	if got := sm.TerminationCount(); got != 1 {
		t.Errorf("gawk_moderation_terminations_total = %v, want 1", got)
	}
}
