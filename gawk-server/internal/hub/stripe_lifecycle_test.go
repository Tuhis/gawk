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
// §5.6 assigns leg teardown to the viewer — which fails exactly when the
// relay kills the primary out from under a viewer that does not react (the
// observed trigger was the KeyframeOpenFailEvictThreshold eviction). A leg
// cannot age out on its own: it keeps receiving delta datagrams, the peer's
// QUIC stack keeps acking, so the idle timeout never fires. The registry
// must therefore reap a primary's legs when the primary's session ends for
// any reason — this test drives the observed path (eviction) and asserts
// the invariant docs/35 §14 establishes.
//
// Test-first record (CODE-REVIEW.md): run un-skipped, this fails today at
// "timed out waiting for the evicted primary's stripe legs to be closed",
// with the hub still reporting Subscribers 5 (healthy viewer + 4 orphaned
// legs), StripeLegs 4 — the field signature, reproduced. Remove the skip as
// the first step of implementing ST8 (docs/35 §14), watch it fail again,
// then fix.
func TestStripeLegsReapedWhenPrimaryEvicted(t *testing.T) {
	t.Skip("known defect — BUGS.md 2026-08-03 (stripe legs outlive their primary); un-skip with the ST8 fix, docs/35 §14")

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

	// The striping viewer: a primary whose keyframe stream opens always fail
	// (the exhausted-stream-credit zombie signature, R10) plus a full set of
	// legs, engaged via StripeState exactly as the transport applies it.
	primaryConn := &fakeSender{kfOpenErr: errors.New("too many open streams")}
	primary, err := r.Subscribe(id, primaryConn)
	if err != nil {
		t.Fatalf("Subscribe(primary): %v", err)
	}
	const stripeN = wire.MaxStripeLegs
	legConns := make([]*fakeSender, stripeN)
	for j := range legConns {
		legConns[j] = &fakeSender{}
		if _, err := r.SubscribeStripeLeg(id, legConns[j], StripeLeg{N: stripeN, Member: j}, 0); err != nil {
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

	// The invariant under test: the evicted primary's legs end too. Today
	// nothing closes them — no session end, no removal, forever.
	waitFor(t, 5*time.Second, func() bool {
		for _, lc := range legConns {
			if _, closed := lc.getCloseInfo(); !closed {
				return false
			}
		}
		return true
	}, "the evicted primary's stripe legs to be closed")

	// The reap must be non-terminal: a live viewer that merely lost its
	// primary re-engages striping through a fresh session set (docs/35
	// §5.6's leg-death fallback), which terminal 4000 would forbid. ST8
	// pins the exact code (a dedicated one, so the reap is diagnosable).
	for j, lc := range legConns {
		if code, _ := lc.getCloseInfo(); code == uint32(wire.CloseCodeBroadcastEnded) {
			t.Errorf("leg %d closed with terminal code %d — a reaped leg must stay reconnectable", j, code)
		}
	}

	// And the slots come back: only the healthy viewer remains against
	// MaxSubscribers/MaxTotalSubscribers, and the leg gauge returns to zero
	// (the field capture's inverse: subscribers 1 / stripeLegs 0 /
	// viewersGlobal 1).
	waitFor(t, 5*time.Second, func() bool {
		bs := singleBroadcastStats(t, r)
		return bs.Subscribers == 1 && bs.StripeLegs == 0
	}, "the orphaned legs' subscriber slots to be released")
	if got := singleBroadcastStats(t, r).ViewersGlobal; got != 1 {
		t.Errorf("ViewersGlobal = %d, want 1 (the healthy viewer)", got)
	}
}
