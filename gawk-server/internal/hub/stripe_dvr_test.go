package hub

// R87 SG1 (docs/91 §6): striped DVR delivery in the hub. One ring, one cursor
// per connection of a viewer's stripe; each connection serves exactly the
// GOPs of its residue, whole and in order.

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// stripedDVROpts is a registry that stripes DVR delivery, with a ring deep
// enough that nothing falls off it unless a test means it to.
func stripedDVROpts() Options {
	return Options{
		StripedDelivery: true,
		StripedDVR:      true,
		DVR:             DVROptions{Window: 10 * time.Minute, MaxBytes: 256 << 20},
	}
}

// stripeGOPLen is the publisher's fixed GOP length in frames: the shape
// that makes an unhashed frameID mod N collapse (docs/91 D2).
const stripeGOPLen = 30

// gopKeyframeID is the keyframe frameID of GOP k.
func gopKeyframeID(k int) uint32 { return uint32(1 + stripeGOPLen*k) }

// ingestStripeGOP publishes GOP k: its keyframe, then deltas records.
func ingestStripeGOP(t *testing.T, p *Publisher, k, deltas int) {
	t.Helper()
	kf := gopKeyframeID(k)
	ingestKeyframe(t, p, keyframeMsg(t, kf, "vp8", "KEY"))
	for i := range deltas {
		p.HandleDatagram(chunkDgram(t, false, kf+uint32(i)+1, 0, 1, fmt.Sprintf("g%d-d%d", k, i)))
	}
}

// servedGOP is one GOP as one connection delivered it: its keyframe's
// frameID and the frameIDs of the records on its carrier, in order.
type servedGOP struct {
	kf      uint32
	records []uint32
}

// servedGOPs pairs a fake connection's keyframe streams with its carriers.
// A DVR drain sends each GOP's keyframe, then opens that GOP's carrier, so
// the i-th keyframe and the i-th carrier are one GOP.
func servedGOPs(t *testing.T, f *fakeSender) []servedGOP {
	t.Helper()
	kfs := f.receivedKeyframes()
	cars := f.carrierStreams()
	if len(cars) > len(kfs) {
		t.Fatalf("%d carriers for %d keyframes", len(cars), len(kfs))
	}
	out := make([]servedGOP, len(kfs))
	for i, msg := range kfs {
		hdr, err := wire.ParseStreamFrameHeader(msg)
		if err != nil {
			t.Fatalf("keyframe %d: %v", i, err)
		}
		out[i].kf = hdr.FrameID
		if i >= len(cars) {
			continue
		}
		for _, rec := range cars[i].records(t) {
			h, _, err := wire.ParseVideoChunk(rec)
			if err != nil {
				t.Fatalf("record on carrier %d: %v", i, err)
			}
			out[i].records = append(out[i].records, h.FrameID)
		}
	}
	return out
}

// subscribeStripe dials a DVR primary that owns testOwner and its legs
// j = 1…n−1, then arms the primary's StripeState for width n.
func subscribeStripe(t *testing.T, r *Registry, id string, n, bufferMs int) (*Subscriber, []*Subscriber, []*fakeSender) {
	t.Helper()
	conns := make([]*fakeSender, n)
	for j := range conns {
		conns[j] = &fakeSender{}
	}
	primary, err := r.SubscribeDVROwned(id, conns[0], bufferMs, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVROwned: %v", err)
	}
	var legs []*Subscriber
	for j := 1; j < n; j++ {
		leg, err := r.SubscribeStripeLeg(id, conns[j], StripeLeg{N: n, Member: j, Owner: testOwner, BufferMs: bufferMs}, 0)
		if err != nil {
			t.Fatalf("SubscribeStripeLeg(%d): %v", j, err)
		}
		legs = append(legs, leg)
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: uint8(n)})
	return primary, legs, conns
}

// TestStripedDVRPartitionIsExactCover is SG1's core criterion: with
// StripeState fresh, N cursors on one ring serve every GOP exactly once
// between them, each on the connection its residue names, keyframe first
// and then its records in order.
func TestStripedDVRPartitionIsExactCover(t *testing.T) {
	const gops, deltas = 600, 3
	for n := 2; n <= wire.MaxStripeLegs; n++ {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			r := NewRegistry(discardLog, stripedDVROpts())
			id, p, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			primary, legs, conns := subscribeStripe(t, r, id, n, 30000)
			// The arm must predate every GOP, or the primary serves the older
			// ones in full (dvrServesGop) and this is no longer a partition.
			time.Sleep(2 * time.Millisecond)
			for k := range gops {
				if k%50 == 0 {
					primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: uint8(n)})
				}
				ingestStripeGOP(t, p, k, deltas)
			}
			// The last GOP completes only when a next keyframe arrives; a
			// trailing one makes every GOP's records final.
			ingestStripeGOP(t, p, gops, 0)

			waitFor(t, 20*time.Second, func() bool {
				total := 0
				for _, c := range conns {
					total += len(c.receivedKeyframes())
				}
				return total >= gops+1
			}, "every GOP to be served")
			time.Sleep(20 * time.Millisecond) // let the last records land

			servedBy := make(map[uint32]int)
			for j, c := range conns {
				for _, g := range servedGOPs(t, c) {
					if prev, dup := servedBy[g.kf]; dup {
						t.Errorf("GOP %d served by connection %d and %d", g.kf, prev, j)
					}
					servedBy[g.kf] = j
					if want := wire.StripeGOPResidue(g.kf, n); want != j {
						t.Errorf("GOP %d served by connection %d, its residue is %d", g.kf, j, want)
					}
					if g.kf == gopKeyframeID(gops) {
						continue // the trailing GOP's records were never sent
					}
					want := []uint32{g.kf + 1, g.kf + 2, g.kf + 3}
					if !slices.Equal(g.records, want) {
						t.Errorf("GOP %d records on connection %d = %v, want %v", g.kf, j, g.records, want)
					}
				}
			}
			for k := range gops + 1 {
				if _, ok := servedBy[gopKeyframeID(k)]; !ok {
					t.Errorf("GOP %d (k=%d) served by nobody", gopKeyframeID(k), k)
				}
			}
			// Each connection must actually carry a share: an assignment
			// that piles everything on one connection would pass the cover.
			for j, c := range conns {
				if got := len(c.receivedKeyframes()); got < (gops/n)*8/10 {
					t.Errorf("connection %d served %d GOPs, want about %d", j, got, gops/n)
				}
			}
			// Legs carry video only (D5): no datagrams, no audio lane.
			for j, leg := range legs {
				if got := len(conns[j+1].received()); got != 0 {
					t.Errorf("leg %d received %d datagrams, want none", j+1, got)
				}
				if leg.audioQueue != nil || leg.dvrAudio != nil {
					t.Errorf("leg %d has an audio lane", j+1)
				}
			}
			for _, s := range append([]*Subscriber{primary}, legs...) {
				if s.DVRResyncs() != 0 {
					t.Errorf("a connection resynced on a ring that held everything")
				}
			}
		})
	}
}

// TestStripedDVRExpirySendsPrimaryBackToEveryGOP (D7): when the viewer's
// StripeState refresh stops, the primary serves every GOP again. No GOP may
// go unserved across the expiry; duplicates are the accepted cost.
func TestStripedDVRExpirySendsPrimaryBackToEveryGOP(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, _, conns := subscribeStripe(t, r, id, 2, 30000)
	// A stopped refresh, compressed: the state lapses 300 ms from now
	// instead of StripeStateTTL.
	expiry := time.Now().Add(300 * time.Millisecond)
	primary.stripeUntil.Store(expiry.UnixNano())
	time.Sleep(2 * time.Millisecond)

	const gops = 60
	for k := range gops {
		ingestStripeGOP(t, p, k, 2)
		time.Sleep(10 * time.Millisecond)
	}
	ingestStripeGOP(t, p, gops, 0)
	if time.Now().Before(expiry.Add(100 * time.Millisecond)) {
		t.Fatal("the GOPs did not outlast the expiry; the test proves nothing")
	}
	waitFor(t, 10*time.Second, func() bool {
		gs := servedGOPs(t, conns[0])
		return len(gs) > 0 && gs[len(gs)-1].kf == gopKeyframeID(gops)
	}, "the primary to reach the newest GOP")

	served := make(map[uint32]bool)
	before, after := 0, 0
	for _, c := range conns {
		for _, g := range servedGOPs(t, c) {
			served[g.kf] = true
		}
	}
	for k := range gops + 1 {
		if !served[gopKeyframeID(k)] {
			t.Errorf("GOP k=%d served by nobody across the expiry", k)
		}
	}
	primaryGOPs := servedGOPs(t, conns[0])
	for _, g := range primaryGOPs {
		if wire.StripeGOPResidue(g.kf, 2) != 0 {
			after++
		} else {
			before++
		}
	}
	if after == 0 {
		t.Fatal("the primary never served a residue-1 GOP: the expiry did not send it back to every GOP")
	}
	// The newest third of the GOPs arrived well after the expiry: the
	// primary must have every one of them.
	have := make(map[uint32]bool)
	for _, g := range primaryGOPs {
		have[g.kf] = true
	}
	for k := gops * 2 / 3; k <= gops; k++ {
		if !have[gopKeyframeID(k)] {
			t.Errorf("GOP k=%d arrived after the expiry and the primary did not serve it", k)
		}
	}
	// Before the expiry it served only its residue; the leg had the rest.
	if len(servedGOPs(t, conns[1])) == 0 {
		t.Error("the leg served nothing while the state was fresh")
	}
	t.Logf("primary served %d residue-0 and %d residue-1 GOPs", before, after)
}

// TestStripedDVRLegFallsBehindAlone (D3): a leg whose writes park past the
// viewer's buffer resyncs on its own; the primary and the other leg keep
// their cursors and serve every GOP of their residues.
func TestStripedDVRLegFallsBehindAlone(t *testing.T) {
	const n, bufferMs = 3, MinDVRBufferMs
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, legs, conns := subscribeStripe(t, r, id, n, bufferMs)
	block := make(chan struct{})
	conns[1].setCarBlock(block) // leg 1's link is down
	time.Sleep(2 * time.Millisecond)

	// 3 s of GOPs at 100 ms: three times the buffer.
	const gops = 30
	for k := range gops {
		if k%10 == 0 {
			primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: n})
		}
		ingestStripeGOP(t, p, k, 2)
		time.Sleep(100 * time.Millisecond)
	}
	ingestStripeGOP(t, p, gops, 0)

	waitFor(t, 10*time.Second, func() bool { return legs[0].DVRResyncs() > 0 }, "the blocked leg to resync")
	want := func(member int) []uint32 {
		var ids []uint32
		for k := range gops + 1 {
			if kf := gopKeyframeID(k); wire.StripeGOPResidue(kf, n) == member {
				ids = append(ids, kf)
			}
		}
		return ids
	}
	for _, j := range []int{0, 2} {
		waitFor(t, 10*time.Second, func() bool { return len(conns[j].receivedKeyframes()) >= len(want(j)) },
			fmt.Sprintf("connection %d to serve its residue", j))
		var got []uint32
		for _, g := range servedGOPs(t, conns[j]) {
			got = append(got, g.kf)
		}
		if !slices.Equal(got, want(j)) {
			t.Errorf("connection %d served %v, want its residue %v", j, got, want(j))
		}
	}
	if primary.DVRResyncs() != 0 || legs[1].DVRResyncs() != 0 {
		t.Errorf("resyncs: primary %d, leg 2 %d — only the blocked leg may resync",
			primary.DVRResyncs(), legs[1].DVRResyncs())
	}
	close(block)
}

// TestStripedDVRPacerShare (D4): a striped connection's catch-up ceiling is
// max-catchup × live / N, so the whole stripe never catches up faster than
// one unstriped viewer.
func TestStripedDVRPacerShare(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	plain, err := r.SubscribeDVR(id, &fakeSender{}, 3000, 0)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	primary, legs, _ := subscribeStripe(t, r, id, 3, 3000)
	ring := plain.dvr
	t0 := time.Now()
	ring.AppendKeyframe(make([]byte, 100_000), t0)
	ring.AppendKeyframe(make([]byte, 100_000), t0.Add(time.Second))
	live := ring.LiveRateBps()
	if live <= 0 {
		t.Fatal("no live rate to share")
	}
	for name, tc := range map[string]struct {
		s    *Subscriber
		want int
	}{
		"unstriped":       {plain, live},
		"striped primary": {primary, live / 3},
		"leg":             {legs[0], live / 3},
	} {
		if got := tc.s.dvrPaceBps(); got != tc.want {
			t.Errorf("%s: pace rate %d, want %d", name, got, tc.want)
		}
	}
	primary.ApplyStripeState(wire.StripeState{})
	if got := primary.dvrPaceBps(); got != live {
		t.Errorf("unstriped primary: pace rate %d, want the full %d", got, live)
	}

	// The group bound: N pacers at live/N hand out no more in a second than
	// one pacer at live.
	for n := 2; n <= wire.MaxStripeLegs; n++ {
		now := time.Now()
		clock := func() time.Time { return now }
		single := newDVRPacer(DefaultDVRMaxCatchup, clock)
		group := make([]*dvrPacer, n)
		for j := range group {
			group[j] = newDVRPacer(DefaultDVRMaxCatchup, clock)
		}
		var singleBytes, groupBytes int
		for range 1000 {
			if single.allow(1000, live) {
				singleBytes += 1000
			}
			for _, g := range group {
				if g.allow(1000, live/n) {
					groupBytes += 1000
				}
			}
			now = now.Add(time.Millisecond)
		}
		if groupBytes > singleBytes+n*1000 {
			t.Errorf("N=%d: the stripe caught up %d bytes in 1 s, one unstriped viewer %d", n, groupBytes, singleBytes)
		}
	}
}

// TestApplyStripeStateOnDVRPrimary (D7): a DVR primary honours StripeState
// only when it is owned and the relay stripes DVR.
func TestApplyStripeStateOnDVRPrimary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stripedDVR bool
		owner      string
		wantArmed  bool
	}{
		{"owned, striped DVR on", true, testOwner, true},
		{"owned, striped DVR off", false, testOwner, false},
		{"unowned, striped DVR on", true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := stripedDVROpts()
			opts.StripedDVR = tc.stripedDVR
			r := NewRegistry(discardLog, opts)
			id, _, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			s, err := r.SubscribeDVROwned(id, &fakeSender{}, 3000, 0, tc.owner)
			if err != nil {
				t.Fatalf("SubscribeDVROwned: %v", err)
			}
			defer s.Close()
			s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
			if armed := s.stripeUntil.Load() != 0; armed != tc.wantArmed {
				t.Fatalf("armed = %v, want %v", armed, tc.wantArmed)
			}
			if _, n := s.dvrStripeShare(time.Now().UnixNano()); tc.wantArmed != (n == 2) {
				t.Fatalf("stripe width %d with armed = %v", n, tc.wantArmed)
			}
		})
	}
}

// TestStripedDVRLegNeedsTheRelayFlag: the hub's own guard behind
// NegotiateStripe. A DVR leg is refused on a relay that does not stripe
// DVR, and leg 0 is the primary's in DVR mode.
func TestStripedDVRLegNeedsTheRelayFlag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stripedDVR bool
		member     int
		wantErr    bool
	}{
		{"striped DVR on", true, 1, false},
		{"striped DVR off", false, 1, true},
		{"leg zero", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := stripedDVROpts()
			opts.StripedDVR = tc.stripedDVR
			r := NewRegistry(discardLog, opts)
			id, _, err := r.StartPublish("")
			if err != nil {
				t.Fatalf("StartPublish: %v", err)
			}
			s, err := r.SubscribeStripeLeg(id, &fakeSender{}, StripeLeg{N: 2, Member: tc.member, Owner: testOwner, BufferMs: 3000}, 0)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if s != nil {
				s.Close()
			}
		})
	}
}

// TestStripedDVRStatsAndReap: /statusz shows a DVR leg as a stripe leg with
// its DVR fields and a striped DVR primary as striped; the R30 counters
// count them; and the primary's close reaps its DVR legs with 4005, as it
// does datagram legs.
func TestStripedDVRStatsAndReap(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, _, conns := subscribeStripe(t, r, id, 3, 3000)
	ingestStripeGOP(t, p, 0, 1)

	bs := singleBroadcastStats(t, r)
	if bs.StripeLegs != 2 || bs.StripedPrimaries != 1 {
		t.Errorf("StripeLegs = %d, StripedPrimaries = %d, want 2 and 1", bs.StripeLegs, bs.StripedPrimaries)
	}
	if bs.DVRSubscribers != 1 || bs.ReliableSubscribers != 1 {
		t.Errorf("DVRSubscribers = %d, ReliableSubscribers = %d, want 1 and 1: legs are not viewers",
			bs.DVRSubscribers, bs.ReliableSubscribers)
	}
	if bs.StripeSuppressedDatagrams != 0 {
		t.Errorf("StripeSuppressedDatagrams = %d: a DVR primary suppresses no datagrams", bs.StripeSuppressedDatagrams)
	}
	var legRows, primaryRows int
	for _, d := range bs.SubscriberDetails {
		if d.StripeOwner == "" || !d.DVR || d.DVRBufferMs != 3000 || d.StripeN != 3 {
			t.Errorf("row %+v: want stripeOwner, dvr, dvrBufferMs 3000 and stripeN 3", d)
		}
		switch {
		case d.StripeLeg && (d.StripeMember == 1 || d.StripeMember == 2):
			legRows++
		case !d.StripeLeg && d.Striped:
			primaryRows++
		default:
			t.Errorf("unexpected row %+v", d)
		}
	}
	if legRows != 2 || primaryRows != 1 {
		t.Errorf("rows: %d legs, %d primaries; want 2 and 1", legRows, primaryRows)
	}

	primary.Close()
	for j := 1; j < 3; j++ {
		code, closed := conns[j].getCloseInfo()
		if !closed || code != uint32(wire.CloseCodeStripeLegOrphaned) {
			t.Errorf("leg %d: closed=%v code=%d, want %d", j, closed, code, wire.CloseCodeStripeLegOrphaned)
		}
	}
}

// TestStripedDVRPrimaryEngagingBehindServesItsBacklog: a viewer engages
// striping because its primary is behind, so the primary's cursor sits
// seconds before the GOP its new legs start at. The GOPs in between are no
// leg's: the primary must serve them in full rather than skip by residue.
func TestStripedDVRPrimaryEngagingBehindServesItsBacklog(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pconn := &fakeSender{}
	block := make(chan struct{})
	pconn.setCarBlock(block) // the primary is stuck behind
	primary, err := r.SubscribeDVROwned(id, pconn, 30000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVROwned: %v", err)
	}
	const backlog, more = 10, 10
	for k := range backlog {
		ingestStripeGOP(t, p, k, 2)
	}
	legConn := &fakeSender{}
	if _, err := r.SubscribeStripeLeg(id, legConn, StripeLeg{N: 2, Member: 1, Owner: testOwner, BufferMs: 30000}, 0); err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	time.Sleep(2 * time.Millisecond)
	for k := backlog; k < backlog+more; k++ {
		ingestStripeGOP(t, p, k, 2)
	}
	ingestStripeGOP(t, p, backlog+more, 0)
	close(block)

	served := func() map[uint32]bool {
		m := make(map[uint32]bool)
		for _, c := range []*fakeSender{pconn, legConn} {
			for _, g := range servedGOPs(t, c) {
				m[g.kf] = true
			}
		}
		return m
	}
	waitUntilOK := waitUntilCond(10*time.Second, func() bool { return len(served()) == backlog+more+1 })
	got := served()
	for k := range backlog + more + 1 {
		if !got[gopKeyframeID(k)] {
			t.Errorf("GOP k=%d served by nobody", k)
		}
	}
	if !waitUntilOK {
		t.Logf("gave up waiting: %d of %d GOPs served", len(got), backlog+more+1)
	}
}

// waitUntilCond polls cond until it holds or the timeout elapses, leaving
// the verdict to the caller.
func waitUntilCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// TestStripedDVRSkippingIsNotStalling (D3): a healthy leg whose residue sees
// no GOP for longer than DVRProgressTimeout has had nothing to send, which
// is idleness, not unreachability. It must not be evicted.
func TestStripedDVRSkippingIsNotStalling(t *testing.T) {
	opts := stripedDVROpts()
	opts.DVRProgressTimeout = 300 * time.Millisecond
	r := NewRegistry(discardLog, opts)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	legConn := &fakeSender{}
	if _, err := r.SubscribeStripeLeg(id, legConn, StripeLeg{N: 2, Member: 1, Owner: testOwner, BufferMs: 30000}, 0); err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	// One second of GOPs that all belong to the primary.
	sent := 0
	for k := 0; sent < 20; k++ {
		if wire.StripeGOPResidue(gopKeyframeID(k), 2) != 0 {
			continue
		}
		ingestStripeGOP(t, p, k, 2)
		sent++
		time.Sleep(50 * time.Millisecond)
	}
	if _, closed := legConn.getCloseInfo(); closed {
		t.Fatal("a leg with nothing to send was evicted as stalled")
	}
	if n := len(legConn.receivedKeyframes()); n != 0 {
		t.Fatalf("the leg served %d of the primary's GOPs", n)
	}
}

// TestStripedDVRDeadLegIsStillEvicted is the other half: a leg whose every
// write fails keeps resyncing, and lands on other connections' GOPs about
// half the time. Skipping those must not count as progress, or the dead
// leg dodges DVRProgressTimeout forever.
func TestStripedDVRDeadLegIsStillEvicted(t *testing.T) {
	opts := stripedDVROpts()
	opts.DVRProgressTimeout = 3 * time.Second
	r := NewRegistry(discardLog, opts)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	legConn := &fakeSender{}
	legConn.setKfWriteErr(errors.New("peer gone"))
	legConn.setCarBlock(make(chan struct{}))
	if _, err := r.SubscribeStripeLeg(id, legConn, StripeLeg{N: 2, Member: 1, Owner: testOwner, BufferMs: MinDVRBufferMs}, 0); err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	evicted := func() bool {
		code, closed := legConn.getCloseInfo()
		return closed && code == uint32(wire.CloseCodeSubscriberUnresponsive)
	}
	for k := 0; k < 100 && !evicted(); k++ {
		ingestStripeGOP(t, p, k, 2)
		time.Sleep(100 * time.Millisecond)
	}
	if !evicted() {
		t.Fatal("a leg whose every write failed was never evicted")
	}
}

// TestStripedDVRWideningBehindLeavesNoHole (D11): the viewer widens ×2 → ×3
// make-before-break — new legs first, then the new StripeState, then the
// old leg closes — while its primary is behind. The GOPs between the
// primary's cursor and the new legs' start must still be served.
func TestStripedDVRWideningBehindLeavesNoHole(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pconn := &fakeSender{}
	block := make(chan struct{})
	pconn.setCarBlock(block)
	primary, err := r.SubscribeDVROwned(id, pconn, 30000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVROwned: %v", err)
	}
	oldLegConn := &fakeSender{}
	oldLeg, err := r.SubscribeStripeLeg(id, oldLegConn, StripeLeg{N: 2, Member: 1, Owner: testOwner, BufferMs: 30000}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	time.Sleep(2 * time.Millisecond)
	// 17 GOPs: k = 15 is residue 0 of 2 and residue 2 of 3, older than the
	// new legs' start (k = 16) and newer than the stuck primary's cursor
	// (k = 4), so only a primary that serves its backlog covers it.
	const before, after = 17, 10
	for k := range before {
		ingestStripeGOP(t, p, k, 2)
	}
	waitFor(t, 5*time.Second, func() bool {
		gs := servedGOPs(t, oldLegConn)
		return len(gs) > 0 && wire.StripeGOPResidue(gs[len(gs)-1].kf, 2) == 1 &&
			gs[len(gs)-1].kf >= gopKeyframeID(before-3)
	}, "the old leg to keep up")

	newConns := []*fakeSender{{}, {}}
	for j, c := range newConns {
		if _, err := r.SubscribeStripeLeg(id, c, StripeLeg{N: 3, Member: j + 1, Owner: testOwner, BufferMs: 30000}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(new %d): %v", j+1, err)
		}
	}
	time.Sleep(2 * time.Millisecond)
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	time.Sleep(2 * time.Millisecond)
	oldLeg.Close()
	for k := before; k < before+after; k++ {
		ingestStripeGOP(t, p, k, 2)
	}
	ingestStripeGOP(t, p, before+after, 0)
	close(block)

	all := append([]*fakeSender{pconn, oldLegConn}, newConns...)
	served := func() map[uint32]bool {
		m := make(map[uint32]bool)
		for _, c := range all {
			for _, g := range servedGOPs(t, c) {
				m[g.kf] = true
			}
		}
		return m
	}
	waitUntilCond(10*time.Second, func() bool { return len(served()) == before+after+1 })
	got := served()
	for k := range before + after + 1 {
		if !got[gopKeyframeID(k)] {
			t.Errorf("GOP k=%d served by nobody across the widening", k)
		}
	}
}
