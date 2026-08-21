package transport

// R39 AP3 (docs/42 §4.3, §9): Server.HandleBanAdded — the actuation half of
// moderation. AP2's tests cover the publish-path GATE; these cover the KILL.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	if srv.clusterCoord() != nil || srv.edgeManager() != nil {
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
	srv.wiring.Store(&clusterWiring{edges: h.manager})

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

// The kill and the Lease deletion race, and the loser must not downgrade the
// message. The origin's kill deletes the cluster Lease, so an EDGE pod can see
// the lease deletion BEFORE its own Ban informer event — and the ordinary
// lease-deletion path expires the hub with 4000, "broadcast ended". A viewer
// would then be told the broadcast simply finished, which is exactly the
// outcome D6 spent a new close code to avoid.
//
// Nothing forces the arrival order (the Ban event has a head start of the
// origin's whole handler plus an API round trip, but that is a race, not a
// guarantee), so HandleLeaseDeleted consults the ban set instead of trusting
// the order. This test drives the LOSING order deliberately: the ban is in the
// set, and only the lease deletion is delivered.
func TestLeaseDeletionOfABannedBroadcastStillSays4006(t *testing.T) {
	srv, _, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	f := newKillFixture(t, srv, r, "203.0.113.7")

	// The pod knows about the ban, but HandleBanAdded has NOT run yet.
	bans := moderation.NewSet()
	if err := bans.Upsert(idBan(f.id, "kill")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	srv.SetModeration(bans)

	srv.HandleLeaseDeleted(f.id)

	code, closed := f.viewer.closeInfo()
	if !closed {
		t.Fatal("the viewer was not closed at all")
	}
	if code != uint32(wire.CloseCodeTerminatedByOperator) {
		t.Errorf("viewer close code = %d, want %d (4006): a lease deletion that lost the race to "+
			"the Ban event must not downgrade the kill to 'broadcast ended'",
			code, wire.CloseCodeTerminatedByOperator)
	}
}

// The control, and it is sharper than "4000 instead of 4006": with no ban in
// the set, a lease deletion for a hub whose publisher is still live changes
// nothing at all, because EndBroadcast honours the publisherActive guard
// (hub.go) that stops a racing janitor killing a live broadcast. So this
// asserts BOTH halves of the invariant at once — the ban path deliberately
// bypasses that guard, and the ordinary path still respects it. A ban lookup
// that turned every lease deletion into a termination would fail here.
func TestLeaseDeletionWithoutABanRespectsTheGCGuard(t *testing.T) {
	srv, _, r := newOutcomeServer(t, config.Config{}, hub.Options{})
	f := newKillFixture(t, srv, r, "203.0.113.7")
	srv.SetModeration(moderation.NewSet())

	srv.HandleLeaseDeleted(f.id)

	if _, closed := f.viewer.closeInfo(); closed {
		t.Error("an unbanned lease deletion killed a live broadcast: the publisherActive guard no longer holds")
	}
	if _, _, closed := f.sess.info(); closed {
		t.Error("an unbanned lease deletion closed the publisher session")
	}
}

// THE STARTUP-WINDOW HOLE (PR #280 review): the publish-path ban gate runs
// PRE-upgrade, but a session only becomes killable at trackPublisher, after
// the WebTransport upgrade round-trip. A ban landing in between updates the
// Set and then finds nothing to kill — no publisher entry, and on the mint
// path no hub either. With -moderation-source=k8s the 5-minute resync
// eventually heals it; with -moderation-source=file nothing re-fires at all,
// so the publisher who raced the ban streams until it disconnects.
//
// The hook lands the ban inside the window in the informer's own order (Set
// first, then actuate), and asserts that the kill really did find nothing —
// otherwise the test would be measuring the kill rather than the re-check.
func TestPublishBanLandingInsideTheUpgradeWindowStillCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim bool
		ban   func(id string) moderation.Record
	}{
		// The pure "nothing to kill" shape: no ID has been minted yet, so
		// the only handle that can catch this publisher is its address —
		// and publishersIn has nothing to walk.
		{name: "mint/ip", ban: func(string) moderation.Record { return ipBan("127.0.0.0/8", "abusive host") }},
		// Same hole on the claim path: the hub exists, but an IP ban only
		// ever walks tracked publisher sessions, so the kill is a no-op and
		// the broadcast carries on.
		{name: "claim/ip", claim: true, ban: func(string) moderation.Record { return ipBan("127.0.0.0/8", "abusive host") }},
		// The ID handle. Here the kill does find the hub and tears it down,
		// so the session dies either way — but with the wrong close code
		// (4004, "superseded", from the BindConn that follows a hub the kill
		// removed underneath it) instead of the 4006 D6 exists for.
		{name: "claim/broadcastId", claim: true, ban: func(id string) moderation.Record { return idBan(id, "fraudulent stream") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
				MaxSubscribers:  15,
				MaxIdleTimeout:  30 * time.Second,
				KeepAlivePeriod: 10 * time.Second,
				BroadcastGrace:  5 * time.Minute,
			}, discardLog)

			bans := moderation.NewSet()
			srv.SetModeration(bans)

			// A first, unbanned session: it proves the listener is up (so a
			// dial failure below can only be the kill) and, for the claim
			// subtest, provides the ID and resume token to come back with.
			warm, warmID, warmToken := dialPublisherHandshake(t, ctx, port, clientTLS)
			warm.CloseWithError(0, "")

			url := fmt.Sprintf("https://127.0.0.1:%d/publish", port)
			if tc.claim {
				url = fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, warmID, warmToken)
			}

			hook := func(id string) {
				if tc.claim && id != warmID {
					t.Errorf("the hook saw ID %q on the claim path, want %q", id, warmID)
				}
				rec := tc.ban(id)
				// The source's own order: close the gate, then actuate.
				if err := bans.Upsert(rec); err != nil {
					t.Errorf("Upsert: %v", err)
					return
				}
				srv.HandleBanAdded(rec)
				// The fixture is only interesting if the kill genuinely
				// missed. It does: trackPublisher has not run yet.
				if _, ok := srv.PublisherRemote(warmID); ok {
					t.Error("the publisher was already tracked; this is no longer the TOCTOU window")
				}
			}
			srv.testHookPostUpgradePublish.Store(&hook)

			_, sess, err := dialOnce(t, ctx, url, clientTLS)
			if err == nil {
				defer sess.CloseWithError(0, "")
				acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
				for {
					if _, err = sess.AcceptUniStream(acceptCtx); err != nil {
						break
					}
				}
				acceptCancel()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("the publisher that raced the ban is still connected: the kill missed it and nothing re-checked")
			}
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				t.Fatalf("publisher session ended with %v, want a WebTransport session close", err)
			}
			if se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeTerminatedByOperator) {
				t.Fatalf("close code = %d, want %d (4006)", se.ErrorCode, wire.CloseCodeTerminatedByOperator)
			}
		})
	}
}

// The other half of the startup-window finding (PR #280 review), and the half
// the wiring order alone does not express: SetCluster's fields must be
// SETTABLE-ONCE, not plain fields.
//
// The R39 ban informer calls HandleBanAdded -> terminate() from its own
// goroutine, and terminate() reads the edge manager. main now wires the
// cluster before starting the source, so the two no longer overlap in
// production — but a plain field write gives the race detector nothing to
// synchronise on, so any future caller that reintroduces the overlap would
// reintroduce silent memory unsafety rather than a visible bug. This pins the
// contract: the kill path may read the cluster wiring while SetCluster is
// still running, and see either the old value or the new one, never a torn
// one. Fails under -race with plain fields.
func TestSetClusterIsSafeAgainstAConcurrentBanActuation(t *testing.T) {
	srv, _, r := newOutcomeServer(t, config.Config{}, hub.Options{MaxBroadcasts: 10})
	fixtures := make([]*killFixture, 0, 8)
	for range 8 {
		fixtures = append(fixtures, newKillFixture(t, srv, r, "203.0.113.7"))
	}

	wired := make(chan struct{})
	go func() {
		defer close(wired)
		srv.SetCluster(&fakeCoordinator{}, "pod-self")
	}()

	// Actuates against the very fields SetCluster is publishing.
	for _, f := range fixtures {
		srv.HandleBanAdded(idBan(f.id, "kill"))
	}
	srv.HandleBanAdded(ipBan("203.0.113.0/24", "abusive host"))
	<-wired

	for i, f := range fixtures {
		if _, _, closed := f.sess.info(); !closed {
			t.Errorf("publisher %d survived the ban", i)
		}
	}
}
