package hub

// R87 SG1 (docs/91 §6): striped DVR delivery in the hub. One ring, one cursor
// per connection of a viewer's stripe; each connection serves exactly the
// GOPs of its residue, whole and in order.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
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
		// A drained leg's linger, short so the drain tests stay quick.
		StripeDrainLinger: 50 * time.Millisecond,
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
	primary, err := r.SubscribeDVR(id, conns[0], bufferMs, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
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
	st := *primary.dvrStripe.Load()
	st.until = expiry.UnixNano()
	primary.dvrStripe.Store(&st)
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
// max-catchup × live / N, so the whole stripe catches up no faster than one
// unstriped viewer, but never below dvrStripeMinConnCatchup × live.
func TestStripedDVRPacerShare(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	plain, err := r.SubscribeDVR(id, &fakeSender{}, 3000, 0, "")
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
	// At the default multiple of 4, live/3 would let a connection catch up
	// at only 1.33× live: the floor lifts it to 1.5×.
	floor := int(math.Ceil(dvrStripeMinConnCatchup * float64(live) / DefaultDVRMaxCatchup))
	for name, tc := range map[string]struct {
		s    *Subscriber
		want int
	}{
		"unstriped":       {plain, live},
		"striped primary": {primary, max(live/3, floor)},
		"leg":             {legs[0], max(live/3, floor)},
	} {
		if got := tc.s.dvrPaceBps(); got != tc.want {
			t.Errorf("%s: pace rate %d, want %d", name, got, tc.want)
		}
	}
	primary.ApplyStripeState(wire.StripeState{})
	if got := primary.dvrPaceBps(); got != live {
		t.Errorf("unstriped primary: pace rate %d, want the full %d", got, live)
	}

	// The floor (PR review round 1): N = 4 at the default multiple would be
	// exactly 1× live per connection, which cannot catch up; a multiple
	// below 1.5 caps the share at the unstriped rate.
	for _, c := range []struct {
		live, n  int
		multiple float64
		want     int
	}{
		{12_000_000, 2, 4, 6_000_000},
		{12_000_000, 4, 4, 4_500_000},
		{12_000_000, 4, 8, 3_000_000},
		{12_000_000, 3, 1, 12_000_000},
		{12_000_000, 4, 0, 3_000_000},
		{12_000_000, 1, 4, 12_000_000},
		{0, 4, 4, 0},
	} {
		if got := stripePaceBps(c.live, c.n, c.multiple); got != c.want {
			t.Errorf("stripePaceBps(%d, %d, %v) = %d, want %d", c.live, c.n, c.multiple, got, c.want)
		}
	}

	// The group bound: N pacers at their share hand out no more in a second
	// than one pacer at live, except where the floor lifts the share — and
	// then no more than N connections at the floor.
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
				if g.allow(1000, stripePaceBps(live, n, DefaultDVRMaxCatchup)) {
					groupBytes += 1000
				}
			}
			now = now.Add(time.Millisecond)
		}
		// N connections at the floor, plus each bucket's starting burst.
		floorBytes := int(dvrStripeMinConnCatchup*float64(n)*float64(live)*1.25) + n*1000
		if groupBytes > max(singleBytes+n*1000, floorBytes) {
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
			s, err := r.SubscribeDVR(id, &fakeSender{}, 3000, 0, tc.owner)
			if err != nil {
				t.Fatalf("SubscribeDVR: %v", err)
			}
			defer s.Close()
			s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
			if armed := s.stripeUntil.Load() != 0; armed != tc.wantArmed {
				t.Fatalf("armed = %v, want %v", armed, tc.wantArmed)
			}
			if _, n, _ := s.dvrStripeShare(time.Now().UnixNano()); tc.wantArmed != (n == 2) {
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
	primary, err := r.SubscribeDVR(id, pconn, 30000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
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
	primary, err := r.SubscribeDVR(id, pconn, 30000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
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

// TestDVRStripeStateIsPublishedWhole: a DVR primary's width, period start
// and deadline are one value, so the drain can never pair a new width with
// the old period's start (review R87 SG1, round 1). A refresh of the same
// width keeps the start; a new width or an expiry starts a new period.
func TestDVRStripeStateIsPublishedWhole(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, _, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	s, err := r.SubscribeDVR(id, &fakeSender{}, 3000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	defer s.Close()
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	first := s.dvrStripe.Load()
	if first == nil || first.n != 2 {
		t.Fatalf("armed state = %+v, want width 2", first)
	}
	time.Sleep(time.Millisecond)
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	if refresh := s.dvrStripe.Load(); refresh.since != first.since || refresh.until <= first.until {
		t.Errorf("refresh %+v after %+v: want the same start and a later deadline", refresh, first)
	}
	time.Sleep(time.Millisecond)
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	wider := s.dvrStripe.Load()
	if wider.n != 3 || wider.since <= first.since {
		t.Errorf("widened state %+v: want width 3 with a start after %d", wider, first.since)
	}
	if first.n != 2 {
		t.Error("a published state was mutated in place")
	}
	// The drain's reader sees exactly what was published.
	if _, n, since := s.dvrStripeShare(time.Now().UnixNano()); n != 3 || since != wider.since {
		t.Errorf("share = (%d, %d), want (3, %d)", n, since, wider.since)
	}
	// After an expiry, a re-arm at the same width is a new period.
	expired := *wider
	expired.until = time.Now().UnixNano() - 1
	s.dvrStripe.Store(&expired)
	if _, n, _ := s.dvrStripeShare(time.Now().UnixNano()); n != 1 {
		t.Errorf("expired state: width %d, want 1 (every GOP)", n)
	}
	time.Sleep(time.Millisecond)
	s.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	if rearm := s.dvrStripe.Load(); rearm.since <= wider.since {
		t.Errorf("re-arm after expiry kept the old start %d", wider.since)
	}
	s.ApplyStripeState(wire.StripeState{})
	if s.dvrStripe.Load() != nil {
		t.Error("an unstriped state left the DVR stripe state set")
	}
}

// TestStripedDVROverlappingWidths: the viewer keeps superseded legs open
// for buffer + 1 s after a width change, so one owner briefly holds old-N
// and new-N legs together. Each keeps serving its own residue (duplicates,
// no holes), the counters see every leg and one striped primary, and the
// primary's close still reaps all of them.
func TestStripedDVROverlappingWidths(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	primary, _, conns := subscribeStripe(t, r, id, 2, 30000)
	for j := 1; j < 3; j++ {
		c := &fakeSender{}
		if _, err := r.SubscribeStripeLeg(id, c, StripeLeg{N: 3, Member: j, Owner: testOwner, BufferMs: 30000}, 0); err != nil {
			t.Fatalf("SubscribeStripeLeg(N=3, %d): %v", j, err)
		}
		conns = append(conns, c)
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 3})
	time.Sleep(2 * time.Millisecond)
	const gops = 30
	for k := range gops {
		ingestStripeGOP(t, p, k, 1)
	}
	ingestStripeGOP(t, p, gops, 0)
	waitUntilCond(5*time.Second, func() bool {
		kfs := map[uint32]bool{}
		for _, c := range conns {
			for _, g := range servedGOPs(t, c) {
				kfs[g.kf] = true
			}
		}
		return len(kfs) == gops+1
	})
	widths := []int{3, 2, 3, 3}
	members := []int{0, 1, 1, 2}
	served := map[uint32]bool{}
	for i, c := range conns {
		for _, g := range servedGOPs(t, c) {
			served[g.kf] = true
			if got := wire.StripeGOPResidue(g.kf, widths[i]); got != members[i] {
				t.Errorf("connection %d (N=%d j=%d) served GOP %d of residue %d", i, widths[i], members[i], g.kf, got)
			}
		}
	}
	for k := range gops {
		if !served[gopKeyframeID(k)] {
			t.Errorf("GOP k=%d served by nobody with both widths open", k)
		}
	}
	bs := singleBroadcastStats(t, r)
	if bs.StripeLegs != 3 || bs.StripedPrimaries != 1 || bs.DVRSubscribers != 1 {
		t.Errorf("StripeLegs %d, StripedPrimaries %d, DVRSubscribers %d; want 3, 1, 1",
			bs.StripeLegs, bs.StripedPrimaries, bs.DVRSubscribers)
	}
	primary.Close()
	for i, c := range conns[1:] {
		if code, closed := c.getCloseInfo(); !closed || code != uint32(wire.CloseCodeStripeLegOrphaned) {
			t.Errorf("leg %d: closed=%v code=%d, want 4005", i+1, closed, code)
		}
	}
}

// subscribeDrainLeg dials DVR leg 1 of 2 for testOwner.
func subscribeDrainLeg(t *testing.T, r *Registry, id string, bufferMs int) (*Subscriber, *fakeSender) {
	t.Helper()
	c := &fakeSender{}
	leg, err := r.SubscribeStripeLeg(id, c, StripeLeg{N: 2, Member: 1, Owner: testOwner, BufferMs: bufferMs}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg: %v", err)
	}
	return leg, c
}

// drainState is the drain datagram: StripeState with Striped false.
var drainState = wire.StripeState{}

// legClosedDrained reports that the relay ended the leg as drained.
func legClosedDrained(c *fakeSender) bool {
	code, closed := c.getCloseInfo()
	return closed && code == uint32(wire.CloseCodeStripeLegDrained)
}

// assertServedUpTo fails if the leg served a GOP newer than GOP k or one
// outside its residue, and returns the GOPs it served.
func assertServedUpTo(t *testing.T, c *fakeSender, k int) map[uint32]bool {
	t.Helper()
	served := map[uint32]bool{}
	for _, g := range servedGOPs(t, c) {
		served[g.kf] = true
		if g.kf > gopKeyframeID(k) {
			t.Errorf("served GOP %d after a drain at GOP %d", g.kf, gopKeyframeID(k))
		}
		if wire.StripeGOPResidue(g.kf, 2) != 1 {
			t.Errorf("served GOP %d outside its residue", g.kf)
		}
	}
	return served
}

// TestStripedDVRLegDrain (review R87 round 2): a drain on a superseded DVR
// leg stops new GOPs, still delivers every GOP of its residue it owed up to
// the drain point, then ends the session.
func TestStripedDVRLegDrain(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	leg, c := subscribeDrainLeg(t, r, id, 30000)
	block := make(chan struct{})
	c.setCarBlock(block) // behind at drain time: it still owes GOPs
	const before, after = 12, 10
	for k := range before {
		ingestStripeGOP(t, p, k, 2)
	}
	leg.ApplyStripeState(drainState)
	if !leg.StripeDraining() {
		t.Fatal("the leg did not record the drain")
	}
	for k := before; k < before+after; k++ {
		ingestStripeGOP(t, p, k, 2)
	}
	close(block)
	waitFor(t, 10*time.Second, func() bool { return legClosedDrained(c) }, "the drained leg to be closed")
	served := assertServedUpTo(t, c, before-1)
	for k := range before {
		if kf := gopKeyframeID(k); wire.StripeGOPResidue(kf, 2) == 1 && !served[kf] {
			t.Errorf("owed GOP k=%d was not delivered before the close", k)
		}
	}
	for _, g := range servedGOPs(t, c) {
		if g.kf < gopKeyframeID(before-1) && len(g.records) != 2 {
			t.Errorf("owed GOP %d delivered %d of 2 records", g.kf, len(g.records))
		}
	}
	if leg.DVRResyncs() != 0 {
		t.Errorf("resyncs = %d on a ring that held everything", leg.DVRResyncs())
	}
	if bs := singleBroadcastStats(t, r); bs.StripeLegs != 0 || bs.StripeLegsReaped != 0 {
		t.Errorf("after the drain: StripeLegs %d, StripeLegsReaped %d; want 0 and 0 (a drain is not a reap)",
			bs.StripeLegs, bs.StripeLegsReaped)
	}
}

// TestStripedDVRLegDrainIsIdempotent: a repeated drain datagram (the client
// sends a burst) never moves the point.
func TestStripedDVRLegDrainIsIdempotent(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	leg, c := subscribeDrainLeg(t, r, id, 30000)
	block := make(chan struct{})
	c.setCarBlock(block)
	for k := range 6 {
		ingestStripeGOP(t, p, k, 1)
	}
	leg.ApplyStripeState(drainState)
	point := leg.dvrDrain.Load()
	for k := 6; k < 16; k++ {
		ingestStripeGOP(t, p, k, 1)
		leg.ApplyStripeState(drainState)
	}
	if got := leg.dvrDrain.Load(); got != point {
		t.Fatalf("drain point moved from %+v to %+v on a repeated drain", *point, *got)
	}
	close(block)
	waitFor(t, 10*time.Second, func() bool { return legClosedDrained(c) }, "the drained leg to be closed")
	assertServedUpTo(t, c, 5)
}

// TestStripedDVRDrainIgnoredElsewhere: an unstriped StripeState keeps its
// existing meaning on a primary, and is ignored on a live-mode leg.
func TestStripedDVRDrainIgnoredElsewhere(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	pconn := &fakeSender{}
	primary, err := r.SubscribeDVR(id, pconn, 30000, 0, testOwner)
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	liveConn := &fakeSender{}
	live, err := r.SubscribeStripeLeg(id, liveConn, StripeLeg{N: 2, Member: 1, Owner: testOwner}, 0)
	if err != nil {
		t.Fatalf("SubscribeStripeLeg(live): %v", err)
	}
	primary.ApplyStripeState(wire.StripeState{Striped: true, StripeN: 2})
	primary.ApplyStripeState(drainState)
	live.ApplyStripeState(drainState)
	if primary.StripeDraining() || live.StripeDraining() {
		t.Fatal("a primary or a live-mode leg recorded a drain")
	}
	if primary.dvrStripe.Load() != nil {
		t.Error("the primary's unstriped state did not clear its stripe")
	}
	for k := range 6 {
		ingestStripeGOP(t, p, k, 1)
	}
	waitFor(t, 5*time.Second, func() bool { return len(pconn.receivedKeyframes()) == 6 }, "the unstriped primary to serve every GOP")
	time.Sleep(50 * time.Millisecond)
	for name, c := range map[string]*fakeSender{"primary": pconn, "live leg": liveConn} {
		if _, closed := c.getCloseInfo(); closed {
			t.Errorf("the %s was closed by an unstriped StripeState", name)
		}
	}
}

// TestStripedDVRLegDrainFallingBehind (review R87 round 3): a draining leg
// whose cursor falls more than its buffer behind ends its drain there. It
// does not resync: the backlog it abandons was a superseded leg's, not frame
// loss of the viewer, so it must not count in dvrResyncs.
func TestStripedDVRLegDrainFallingBehind(t *testing.T) {
	r := NewRegistry(discardLog, stripedDVROpts())
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	leg, c := subscribeDrainLeg(t, r, id, MinDVRBufferMs)
	block := make(chan struct{})
	c.setCarBlock(block)
	defer close(block)
	const before = 4
	for k := range before {
		ingestStripeGOP(t, p, k, 2)
	}
	leg.ApplyStripeState(drainState)
	// Keep the broadcast going until the stuck cursor is more than its
	// buffer behind.
	for k := before; k < before+30 && !legClosedDrained(c); k++ {
		ingestStripeGOP(t, p, k, 2)
		time.Sleep(100 * time.Millisecond)
	}
	waitFor(t, 10*time.Second, func() bool { return legClosedDrained(c) }, "the drained leg to close once it fell behind")
	if n := leg.DVRResyncs(); n != 0 {
		t.Errorf("dvrResyncs = %d: a draining leg that fell behind counted a resync", n)
	}
	if n := singleBroadcastStats(t, r).DVRResyncs; n != 0 {
		t.Errorf("broadcast dvrResyncs = %d, want 0", n)
	}
	// Keyframes only: a parked carrier is cancelled and reopened on every
	// retry, so carriers no longer pair one-to-one with GOPs here.
	for _, msg := range c.receivedKeyframes() {
		hdr, err := wire.ParseStreamFrameHeader(msg)
		if err != nil {
			t.Fatalf("keyframe: %v", err)
		}
		if hdr.FrameID > gopKeyframeID(before-1) {
			t.Errorf("served GOP %d after a drain at GOP %d", hdr.FrameID, gopKeyframeID(before-1))
		}
	}
}

// TestStripedDVRLegDrainOnEmptyRing (review R87 round 3): a leg drained
// before its pod's ring ever held a GOP owes nothing. Repeated drains must
// neither panic nor spin, it must close once its linger passes, and GOPs
// that arrive after the drain are never served.
func TestStripedDVRLegDrainOnEmptyRing(t *testing.T) {
	opts := stripedDVROpts()
	opts.StripeDrainLinger = 300 * time.Millisecond
	r := NewRegistry(discardLog, opts)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	leg, c := subscribeDrainLeg(t, r, id, 30000)
	for range 3 {
		leg.ApplyStripeState(drainState)
	}
	if !leg.StripeDraining() {
		t.Fatal("a drain on an empty ring was not recorded")
	}
	for k := range 6 {
		ingestStripeGOP(t, p, k, 1)
		leg.ApplyStripeState(drainState)
	}
	waitFor(t, 5*time.Second, func() bool { return legClosedDrained(c) }, "the drained leg to close")
	if n := len(c.receivedKeyframes()); n != 0 {
		t.Errorf("a leg drained on an empty ring served %d GOPs", n)
	}
	// A drained leg's drain goroutine has ended: Close returns at once
	// rather than waiting on a spinning loop.
	done := make(chan struct{})
	go func() { leg.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung on the drained leg")
	}
}

// TestStripedDVRLegDrainLingers (review R87 round 3): once the last owed
// GOP is handed to QUIC, the leg finishes its carrier normally (FIN, not a
// reset) and lingers before closing, so the tail still in flight on a lossy
// link is delivered instead of being aborted by the session close.
func TestStripedDVRLegDrainLingers(t *testing.T) {
	opts := stripedDVROpts()
	opts.StripeDrainLinger = 400 * time.Millisecond
	r := NewRegistry(discardLog, opts)
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	leg, c := subscribeDrainLeg(t, r, id, 30000)
	const gops = 6
	for k := range gops {
		ingestStripeGOP(t, p, k, 2)
	}
	waitFor(t, 5*time.Second, func() bool {
		gs := servedGOPs(t, c)
		return len(gs) > 0 && len(gs[len(gs)-1].records) == 2 && gs[len(gs)-1].kf == lastOwnGOP(gops, 1)
	}, "the leg to serve its residue")
	leg.ApplyStripeState(drainState)
	drainedAt := time.Now()
	ingestStripeGOP(t, p, gops, 1) // completes the point GOP; the leg must not serve it
	waitFor(t, 5*time.Second, func() bool {
		cars := c.carrierStreams()
		closed, _ := cars[len(cars)-1].state()
		return closed
	}, "the last carrier to be finished")
	for i, car := range c.carrierStreams() {
		if closed, cancelled := car.state(); !closed || cancelled {
			t.Errorf("carrier %d: closed=%v cancelled=%v, want a clean FIN", i, closed, cancelled)
		}
	}
	if legClosedDrained(c) {
		t.Fatal("the leg closed without lingering")
	}
	waitFor(t, 5*time.Second, func() bool { return legClosedDrained(c) }, "the drained leg to close after its linger")
	if since := time.Since(drainedAt); since < 350*time.Millisecond {
		t.Errorf("closed %v after the drain, want the 400 ms linger", since)
	}
	assertServedUpTo(t, c, gops-1)
}

// lastOwnGOP is the newest of GOPs 0…gops−1 whose residue of 2 is member.
func lastOwnGOP(gops, member int) uint32 {
	for k := gops - 1; k >= 0; k-- {
		if kf := gopKeyframeID(k); wire.StripeGOPResidue(kf, 2) == member {
			return kf
		}
	}
	return 0
}

// TestStripeDrainLingerDefault pins the production linger: four smoothed
// RTTs (enough for a lost tail packet's retransmit to land), at least 1 s,
// at most 5 s.
func TestStripeDrainLingerDefault(t *testing.T) {
	for _, tc := range []struct {
		srtt, want time.Duration
	}{
		{0, time.Second},
		{40 * time.Millisecond, time.Second},
		{400 * time.Millisecond, 1600 * time.Millisecond},
		{3 * time.Second, 5 * time.Second},
	} {
		if got := stripeDrainLinger(tc.srtt); got != tc.want {
			t.Errorf("stripeDrainLinger(%v) = %v, want %v", tc.srtt, got, tc.want)
		}
	}
}

// TestDVRResyncLogNamesTheSubscriber: the debug resync line carries the
// subscriber's /statusz key and the lag at the moment of the resync, which
// is not the stale published one when the cursor fell off the tail.
func TestDVRResyncLogNamesTheSubscriber(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := NewRegistry(log, Options{DVR: DVROptions{Window: 30 * time.Second, MaxBytes: 512}})
	id, p, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	f := &fakeSender{}
	block := make(chan struct{})
	f.setCarBlock(block)
	defer close(block)
	sub, err := r.SubscribeDVR(id, f, 3000, 0, "")
	if err != nil {
		t.Fatalf("SubscribeDVR: %v", err)
	}
	for g := range 12 {
		ingestKeyframe(t, p, keyframeMsg(t, uint32(g*100), "vp8", "KEY"))
		for i := range 8 {
			p.HandleDatagram(chunkDgram(t, false, uint32(g*100+i+1), 0, 1, fmt.Sprintf("g%02d-d%d", g, i)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, 10*time.Second, func() bool { return sub.DVRResyncs() > 0 }, "a resync")
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "msg=\"dvr resync\"") {
			line = l
			break
		}
	}
	if !strings.Contains(line, "subscriber="+sub.statsKey) {
		t.Errorf("resync line %q does not name subscriber %s", line, sub.statsKey)
	}
	if strings.Contains(line, "lag_ms=0 ") || !strings.Contains(line, "lag_ms=") {
		t.Errorf("resync line %q carries no fresh lag", line)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a test reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
