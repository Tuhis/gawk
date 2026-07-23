package transport

// R21 DV2's headline criterion (docs/26), against a real relay over real QUIC:
// a DVR subscriber whose downlink goes COMPLETELY dark for a stall shorter
// than the ring loses nothing. Everything captured during the blackout is
// still delivered afterwards, in order and verbatim, because the relay kept
// it in the ring and the drain resumed from the cursor.
//
// This is the assertion the whole milestone exists for, and it cannot be made
// against a fake: the failure it guards against is a write parking on real
// flow control, which only a real QUIC stack produces. The lossy-link
// forwarder from resilient_loss_test.go is reused with the loss dialled to
// 100 % for a window — `tc netem` is unusable in CI (unprivileged containers,
// no NET_ADMIN) and unnecessary for a downlink-only model.
//
// The control is a plain DATAGRAM subscriber on the same link. It loses
// everything sent while the link is dark, because datagrams are never
// retransmitted — which proves the blackout was genuinely total, and that
// reliable delivery is what carries the DVR through it.
//
// Deliberately NOT an R19 carrier subscriber as the control, though that was
// tried first: at the data volume reachable in a CI-paced test (~60 small
// deltas) QUIC's own send buffer absorbs a 1.2 s blackout, so an R19 carrier
// write never parks long enough to hit CarrierWriteTimeout and that
// subscriber recovers by retransmit alone. R21's advantage over R19 shows at
// volumes that exhaust stream flow control — 3 Mbps for a couple of seconds is
// several hundred KB — which is not reproducible here without pacing fast
// enough to inject loopback ingress loss instead. Worth knowing on its own:
// short blackouts at low bitrate are already survivable without a ring.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDVRRingSurvivesACompleteBlackout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	// The publisher is wired straight to the relay: the blackout belongs on
	// the relay→viewer leg, and a clean ingress is what lets "the subscriber
	// is missing a delta" mean "the ring failed" and nothing else.
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	link := startLossyLink(t, port)
	// buffer= well under the default 3 s ring: the cursor must never fall off
	// the tail, so any loss here is a real failure rather than the mode's
	// designed one (docs/26 Decision 4).
	dvr := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s?delivery=reliable&buffer=2500", link.port(), id), clientTLS)
	defer dvr.CloseWithError(0, "")
	control := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", link.port(), id), clientTLS)
	defer control.CloseWithError(0, "")

	waitFor(t, 10*time.Second, func() bool { return r.Stats().Totals.Subscribers == 2 }, "both subscribers registered")
	stats := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if stats.DVRSubscribers != 1 {
		t.Fatalf("DVRSubscribers = %d, want 1 (the ?buffer= negotiation did not take)", stats.DVRSubscribers)
	}
	if stats.ReliableSubscribers != 1 {
		t.Fatalf("ReliableSubscribers = %d, want 1 (the DVR subscriber; the control is on datagrams)", stats.ReliableSubscribers)
	}

	// Four GOPs of single-chunk deltas. The blackout covers the middle two, so
	// there is healthy traffic on either side of it — a test that goes dark
	// from the first frame would not distinguish "recovered" from "started
	// late".
	const (
		gops = 4
		// Paced so one GOP lasts ~600 ms and the blackout ~1.2 s: comfortably
		// longer than the relay's 500 ms CarrierWriteTimeout, so the R19
		// control's carrier really does die mid-write (without that it
		// recovers by retransmit and proves nothing), and comfortably shorter
		// than the 3 s ring, so the DVR cursor never falls off the tail.
		deltasPerGOP = 15
		deltaPaceMs  = 40
		darkFromGOP  = 1
		darkToGOP    = 3 // exclusive
	)
	var sent [][]byte
	byBytes := make(map[string]int)
	frameID := uint32(1)
	var gopKeyframes []uint32
	for range gops {
		gopKeyframes = append(gopKeyframes, frameID)
		frameID++
		for range deltasPerGOP {
			dgram := encodeFrame(t, frameID, false, 1)[0]
			byBytes[string(dgram)] = len(sent)
			sent = append(sent, dgram)
			frameID++
		}
	}

	deadline := time.Now().Add(45 * time.Second)
	readCtx, readCancel := context.WithDeadline(ctx, deadline)
	defer readCancel()
	dvrSink := &carrierSink{}
	go readCarriers(readCtx, dvr, deadline, dvrSink)
	var controlMu sync.Mutex
	controlSeen := map[int]bool{}
	go func() {
		for {
			dgram, err := control.ReceiveDatagram(readCtx)
			if err != nil {
				return
			}
			if i, ok := byBytes[string(dgram)]; ok {
				controlMu.Lock()
				controlSeen[i] = true
				controlMu.Unlock()
			}
		}
	}()

	// Publish, going fully dark across the middle GOPs.
	next := 0
	for g := range gops {
		if g == darkFromGOP {
			link.setLoss(100) // the link drops dead
		}
		if g == darkToGOP {
			link.setLoss(0) // and comes back
		}
		sendKeyframeStream(t, pub, buildStreamKeyframe(t, gopKeyframes[g], "avc1.42E02A", 4096))
		// Let the keyframe's store-and-forward complete before its deltas. A
		// delta that beats its own keyframe into the ring is dropped there and
		// SHOULD be — it is undecodable for every possible cursor — but that
		// makes it a race this test does not want to measure.
		time.Sleep(150 * time.Millisecond)
		for range deltasPerGOP {
			if err := pub.SendDatagram(sent[next]); err != nil {
				t.Fatalf("SendDatagram %d: %v", next, err)
			}
			next++
			time.Sleep(deltaPaceMs * time.Millisecond)
		}
	}
	link.setLoss(0)

	if link.dropped.Load() == 0 {
		t.Fatal("the link dropped nothing — the blackout never happened and this test proves nothing")
	}

	// Recovery is a retransmit storm plus the catch-up burst, so give it real
	// time. Not fatal on timeout: "missing N of M" below names the failure far
	// better than "timed out waiting" would.
	if !waitUntil(15*time.Second, func() bool { return dvrSink.recordCount() >= len(sent) }) {
		t.Logf("gave up waiting for the ring to catch up: %d records for %d deltas sent",
			dvrSink.recordCount(), len(sent))
	}
	time.Sleep(500 * time.Millisecond)

	// The claim: every delta, verbatim, including the ones captured while the
	// link was dark.
	seen := make([]bool, len(sent))
	for _, run := range dvrSink.runs() {
		for _, rec := range run {
			if i, ok := byBytes[string(rec)]; ok {
				seen[i] = true
			}
		}
	}
	missing := 0
	firstMissing := -1
	for i, ok := range seen {
		if !ok {
			missing++
			if firstMissing < 0 {
				firstMissing = i
			}
		}
	}
	if missing > 0 {
		t.Errorf("DVR subscriber is missing %d of %d deltas (first: index %d) — the ring did not "+
			"carry the blackout; that is the milestone's headline claim",
			missing, len(sent), firstMissing)
	}

	final := r.Stats().Broadcasts[r.ObfuscateID(id)]
	if final.DVRResyncs != 0 {
		t.Errorf("dvrResyncs = %d, want 0 — the cursor fell off a ring that should have held "+
			"the whole blackout (buffer 2500 ms vs the default 3 s window)", final.DVRResyncs)
	}

	// The control proves the blackout was real: a datagram viewer cannot
	// recover what was dropped while the link was dark.
	controlMu.Lock()
	got := len(controlSeen)
	controlMu.Unlock()
	if got >= len(sent) {
		t.Errorf("the datagram control received all %d deltas through a total blackout — the "+
			"link was not actually dark, so the DVR assertion above proves nothing", len(sent))
	}
	t.Logf("blackout: DVR %d/%d deltas, datagram control %d/%d, %d packets dropped",
		len(sent)-missing, len(sent), got, len(sent), link.dropped.Load())
}
