package hub

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// TestStripeLegsReapedWhenPrimaryEvicted reproduces the BUGS.md 2026-08-03
// leak ("Stripe legs outlive the primary that owns them, holding subscriber
// slots indefinitely"), observed live as subscribers: 4 / stripeLegs: 4 /
// viewersGlobal: 0 an hour after the primary's R10 eviction.
//
// A striping viewer holds one primary session plus stripe legs, and docs/35
// §5.6 assigned leg teardown to the viewer — which fails exactly when the
// relay kills the primary out from under a viewer that does not react (the
// observed trigger was the KeyframeOpenFailEvictThreshold eviction). A leg
// cannot age out on its own: it keeps receiving delta datagrams, the peer's
// QUIC stack keeps acking, so the idle timeout never fires. §14's answer is
// the ownership reap this test pins: the sessions share a viewer-minted
// ?owner= token, and a closing primary takes its legs with it.
//
// Test-first record (CODE-REVIEW.md): committed 2026-08-06 against the
// pre-fix relay (then without the owner plumbing, which did not exist), it
// failed at "timed out waiting for the evicted primary's stripe legs to be
// closed" with the hub still reporting Subscribers 5 (healthy viewer + 4
// orphaned legs) — the field signature, reproduced.
func TestStripeLegsReapedWhenPrimaryEvicted(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	// A healthy viewer beside the doomed one: it paces the keyframe ingest
	// (≤1 in-flight keyframe per subscriber — rapid ingests would supersede
	// instead of feeding the open-failure streak) and pins that the reap is
	// scoped to the dead primary's legs, not the broadcast's audience.
	healthy := &fakeSender{}
	sh, err := r.Subscribe(id, healthy)
	if err != nil {
		t.Fatalf("Subscribe(healthy): %v", err)
	}
	defer sh.Close()

	// The striping viewer: an owned primary whose keyframe stream opens
	// always fail (the exhausted-stream-credit zombie signature, R10) plus a
	// full set of legs sharing its owner token, engaged via StripeState
	// exactly as the transport applies it.
	primaryConn := &fakeSender{kfOpenErr: errors.New("too many open streams")}
	primary, err := r.SubscribeParity(id, primaryConn, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeParity(primary): %v", err)
	}
	const stripeN = wire.MaxStripeLegs
	legConns := make([]*fakeSender, stripeN)
	for j := range legConns {
		legConns[j] = &fakeSender{}
		if _, err := r.SubscribeStripeLeg(id, legConns[j], StripeLeg{N: stripeN, Member: j, Owner: testOwner}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(%d): %v", j, err)
		}
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: stripeN})

	// Trip the R10 eviction on the primary: threshold consecutive keyframes
	// whose stream open fails, paced on the healthy peer's deliveries.
	for i := range KeyframeOpenFailEvictThreshold {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(i+1), "vp8", fmt.Sprintf("kf%02d", i)))
		waitKeyframes(t, healthy, i+1)
	}
	waitFor(t, 5*time.Second, func() bool {
		code, closed := primaryConn.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}, "the striped primary to be evicted with the unresponsive code")

	// The invariant under test: the evicted primary's legs end too, with the
	// dedicated non-terminal code — a live viewer that merely lost its
	// primary re-engages striping through a fresh session set (docs/35
	// §5.6's leg-death fallback), which terminal 4000 would forbid.
	waitFor(t, 5*time.Second, func() bool {
		for _, lc := range legConns {
			if _, closed := lc.getCloseInfo(); !closed {
				return false
			}
		}
		return true
	}, "the evicted primary's stripe legs to be closed")
	for j, lc := range legConns {
		if code, _ := lc.getCloseInfo(); code != uint32(wire.CloseCodeStripeLegOrphaned) {
			t.Errorf("leg %d closed with code %d, want %d (stripe leg orphaned)", j, code, wire.CloseCodeStripeLegOrphaned)
		}
	}

	// And the slots come back: only the healthy viewer remains against
	// MaxSubscribers/MaxTotalSubscribers, the leg gauge returns to zero, and
	// the reap is visible to operators (the field capture's inverse:
	// subscribers 1 / stripeLegs 0 / viewersGlobal 1 / stripeLegsReaped 4).
	waitFor(t, 5*time.Second, func() bool {
		bs := singleBroadcastStats(t, r)
		return bs.Subscribers == 1 && bs.StripeLegs == 0
	}, "the orphaned legs' subscriber slots to be released")
	bs := singleBroadcastStats(t, r)
	if bs.ViewersGlobal != 1 {
		t.Errorf("ViewersGlobal = %d, want 1 (the healthy viewer)", bs.ViewersGlobal)
	}
	if bs.StripeLegsReaped != stripeN {
		t.Errorf("StripeLegsReaped = %d, want %d", bs.StripeLegsReaped, stripeN)
	}
	if got := r.Stats().Totals.StripeLegsReaped; got != stripeN {
		t.Errorf("Totals.StripeLegsReaped = %d, want %d", got, stripeN)
	}
}

// TestStripeLegReapScopedToOwner: the reap is precise, not broadcast-wide —
// two striping viewers share a broadcast, one's primary closes cleanly (the
// transport handler's deferred Close, i.e. any ordinary session end), and
// only that viewer's legs are reaped. Collateral reaping would force every
// striping viewer on a hot broadcast through the leg-death fallback each
// time any one of them left (docs/35 §14 Decision 1).
func TestStripeLegReapScopedToOwner(t *testing.T) {
	const ownerA = "aaaaaaaa11111111"
	const ownerB = "bbbbbbbb22222222"
	r := NewRegistry(discardLog, Options{StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}

	primaryA, err := r.SubscribeParity(id, &fakeSender{}, 0, ownerA)
	if err != nil {
		t.Fatalf("SubscribeParity(A): %v", err)
	}
	legsA := []*fakeSender{{}, {}}
	for j, lc := range legsA {
		if _, err := r.SubscribeStripeLeg(id, lc, StripeLeg{N: 2, Member: j, Owner: ownerA}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(A%d): %v", j, err)
		}
	}
	primaryBConn := &fakeSender{}
	primaryB, err := r.SubscribeParity(id, primaryBConn, 0, ownerB)
	if err != nil {
		t.Fatalf("SubscribeParity(B): %v", err)
	}
	defer primaryB.Close()
	legsB := []*fakeSender{{}, {}}
	for j, lc := range legsB {
		if _, err := r.SubscribeStripeLeg(id, lc, StripeLeg{N: 2, Member: j, Owner: ownerB}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(B%d): %v", j, err)
		}
	}

	primaryA.Close()

	for j, lc := range legsA {
		code, closed := lc.getCloseInfo()
		if !closed || code != uint32(wire.CloseCodeStripeLegOrphaned) {
			t.Errorf("A leg %d: closed=%v code=%d, want closed with %d", j, closed, code, wire.CloseCodeStripeLegOrphaned)
		}
	}
	for j, lc := range legsB {
		if _, closed := lc.getCloseInfo(); closed {
			t.Errorf("B leg %d was reaped by A's close — the reap must be owner-scoped", j)
		}
	}
	if _, closed := primaryBConn.getCloseInfo(); closed {
		t.Error("B's primary was closed by A's close")
	}
	bs := singleBroadcastStats(t, r)
	if bs.Subscribers != 3 {
		t.Errorf("Subscribers = %d, want 3 (B's primary + B's 2 legs)", bs.Subscribers)
	}
	if bs.StripeLegsReaped != 2 {
		t.Errorf("StripeLegsReaped = %d, want 2", bs.StripeLegsReaped)
	}
}

// TestUnownedPrimaryCannotStripe (docs/35 §14, owner decision 2026-08-13):
// the other half of the striped-surface enforcement. Leg dials without a
// token are rejected outright (TestNegotiateStripe), and an unowned primary
// ignores StripeState — so no session topology the registry cannot reap can
// be built, and the worst a tokenless client gets is duplicates.
func TestUnownedPrimaryCannotStripe(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	s, err := r.SubscribeParity(id, &fakeSender{}, 0, "")
	if err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	defer s.Close()
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	if s.stripeUntil.Load() != 0 {
		t.Error("an unowned primary armed stripe suppression")
	}
	if s.stripeTransitions.Load() != 0 {
		t.Error("an unowned primary counted a stripe transition")
	}
}

// TestUnownedLegRejectedAtHub: the hub-level guard behind NegotiateStripe's
// pre-upgrade rejection, so a future transport refactor cannot re-admit the
// unreapable-orphan class.
func TestUnownedLegRejectedAtHub(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	for name, owner := range map[string]string{
		"empty":     "",
		"short":     "aabbccdd",
		"uppercase": "AABBCCDD00112233",
	} {
		if _, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: 0, Owner: owner}, 0); !errors.Is(err, ErrStripeRejected) {
			t.Errorf("%s owner: err = %v, want ErrStripeRejected", name, err)
		}
	}
}

// TestBroadcastExpiryClosesLegsTerminally: when the whole broadcast ends,
// legs get the terminal 4000 from the expiry sweep like every subscriber —
// the ownership reap must not race them into a non-terminal code that would
// invite a reconnect into a dead broadcast (docs/35 §14 Decision 2).
func TestBroadcastExpiryClosesLegsTerminally(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true, BroadcastGrace: time.Minute})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	if _, err := r.SubscribeParity(id, &fakeSender{}, 0, testOwner); err != nil {
		t.Fatalf("SubscribeParity: %v", err)
	}
	legs := []*fakeSender{{}, {}}
	for j, lc := range legs {
		if _, err := r.SubscribeStripeLeg(id, lc, StripeLeg{N: 2, Member: j, Owner: testOwner}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(%d): %v", j, err)
		}
	}

	p.Close()
	r.EndBroadcast(id)

	for j, lc := range legs {
		code, closed := lc.getCloseInfo()
		if !closed || code != uint32(wire.CloseCodeBroadcastEnded) {
			t.Errorf("leg %d: closed=%v code=%d, want terminal %d from the expiry sweep", j, closed, code, wire.CloseCodeBroadcastEnded)
		}
	}
	if got := r.Stats().Totals.StripeLegsReaped; got != 0 {
		t.Errorf("StripeLegsReaped = %d, want 0 — broadcast expiry is not a reap", got)
	}
}

// TestStripeLegLeaseReapsSilentLeg (docs/35 §14 Decision 5): the cross-pod
// backstop. A leg whose primary this pod never saw — or whose viewer died
// without closing anything — sends nothing, and the lease ends it. Armed at
// subscribe: owner enforcement means every admitted leg has promised the
// 1 Hz heartbeat, so silence from the first second is already the failure.
func TestStripeLegLeaseReapsSilentLeg(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true, StripeLegLease: 50 * time.Millisecond})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	lc := &fakeSender{}
	if _, err := r.SubscribeStripeLeg(id, lc, StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0); err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		code, closed := lc.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeStripeLegOrphaned)
	}, "the silent leg to be lease-reaped with the orphaned code")
	waitFor(t, 5*time.Second, func() bool {
		bs := singleBroadcastStats(t, r)
		return bs.Subscribers == 0 && bs.StripeLegsReaped == 1
	}, "the lease-reaped leg's slot to be released and the reap counted")
}

// TestStripeLegLeaseRenewedByInbound: any inbound datagram renews the lease
// (the transport calls NoteLegAlive per datagram), so a heartbeating leg
// lives indefinitely — and dies one lease after the heartbeats stop.
func TestStripeLegLeaseRenewedByInbound(t *testing.T) {
	r := NewRegistry(discardLog, Options{StripedDelivery: true, StripeLegLease: 300 * time.Millisecond})
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	lc := &fakeSender{}
	leg, err := r.SubscribeStripeLeg(id, lc, StripeLeg{N: 2, Member: 0, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}

	// Renew well inside the lease for several lease-lengths: the leg must
	// survive the whole stretch.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		leg.NoteLegAlive()
		time.Sleep(50 * time.Millisecond)
	}
	if _, closed := lc.getCloseInfo(); closed {
		t.Fatal("a heartbeating leg was lease-reaped")
	}

	// Heartbeats stop — the orphan case — and the lease ends it.
	waitFor(t, 5*time.Second, func() bool {
		code, closed := lc.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeStripeLegOrphaned)
	}, "the leg to be lease-reaped once heartbeats stopped")
}
