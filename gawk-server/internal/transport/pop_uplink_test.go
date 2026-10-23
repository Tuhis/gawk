package transport_test

// R74 PP8 in-process acceptance (docs/76 §11): the uplink on demand, end to
// end, on popsrv's harness (three home pods over shared fake clientsets and
// one PoP relay). An external test package: popsrv imports transport.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const upPop = "assembly-2026"

// uplinkFleet is the harness with the uplink wired on every home pod, as
// serve's wirePopUplink does.
type uplinkFleet struct {
	f   *popsrv.HomeFleet
	pop *popsrv.PoP
}

type fleetOpts struct {
	home  func(*config.Config)
	popO  func(*popclient.Options)
	extra []popsrv.HarnessOption
}

func startUplinkFleet(t *testing.T, n int, o fleetOpts) *uplinkFleet {
	t.Helper()
	opts := append([]popsrv.HarnessOption{
		popsrv.WithHomeConfig(func(c *config.Config) {
			c.UplinkCarriers = true
			c.PopDemandTimeout = 2 * time.Second
			if o.home != nil {
				o.home(c)
			}
		}),
	}, o.extra...)
	if o.popO != nil {
		opts = append(opts, popsrv.WithPopClient(o.popO))
	}
	f, pop := popsrv.StartHomeAndPop(t, n, opts...)
	for _, pod := range f.Pods {
		u := pod.Manager.StartUplink(popsrv.UplinkOptions{
			Reservations:  popsrv.NewClusterReservations(pod.Coord),
			DemandTimeout: 2 * time.Second,
		})
		pod.Server.SetPopDemand(u)
	}
	// serve.Run starts each relay's viewer-count pump; the harness does not.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, pod := range f.Pods {
		go pod.Registry.RunViewerCountPump(ctx)
	}
	go pop.Registry.RunViewerCountPump(ctx)
	return &uplinkFleet{f: f, pop: pop}
}

func (u *uplinkFleet) pair(t *testing.T) {
	t.Helper()
	s := u.f.CreatePop(t, upPop, "Assembly 2026", time.Hour)
	if err := u.pop.Client.Pair(testCtx(t), s); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	popsrv.WaitFor(t, "linked", func() bool { return u.pop.Client.State().Header == popclient.HeaderLinked })
}

// reserved waits until every pod's lease cache shows id as the PoP's.
func (u *uplinkFleet) reserved(t *testing.T, id string) {
	t.Helper()
	for _, pod := range u.f.Pods {
		popsrv.WaitFor(t, pod.Name+" sees the reservation", func() bool {
			o, _, _, ok := pod.Coord.Lookup(id)
			return ok && o.Pop == upPop
		})
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func dialWT(t *testing.T, tlsCfg *tls.Config, target string) (*webtransport.Session, error) {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: tlsCfg,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	rsp, sess, err := d.Dial(testCtx(t), target, nil)
	if err != nil {
		if rsp != nil {
			return nil, &statusErr{rsp.StatusCode}
		}
		return nil, err
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	return sess, nil
}

type statusErr struct{ code int }

func (e *statusErr) Error() string { return http.StatusText(e.code) }

// publisher is a broadcaster on the PoP.
type publisher struct {
	sess *webtransport.Session
	id   string
	// counts are the ViewerCount values the relay pushed.
	mu     sync.Mutex
	counts []uint32
}

func publishOnPoP(t *testing.T, u *uplinkFleet) *publisher {
	t.Helper()
	sess, err := dialWT(t, u.f.ClientTLS, "https://"+u.pop.Addr()+"/publish")
	if err != nil {
		t.Fatalf("publish dial: %v", err)
	}
	p := &publisher{sess: sess}
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()
	for p.id == "" {
		st, err := sess.AcceptUniStream(ctx)
		if err != nil {
			t.Fatalf("no announce: %v", err)
		}
		data, _ := io.ReadAll(st)
		if _, typ, err := wire.PeekType(data); err == nil && typ == wire.TypeBroadcastAnnounce {
			p.id, _ = wire.ParseBroadcastAnnounce(data)
		}
	}
	go func() {
		for {
			d, err := sess.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			if n, err := wire.ParseViewerCount(d); err == nil {
				p.mu.Lock()
				p.counts = append(p.counts, n)
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *publisher) lastCount() (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.counts) == 0 {
		return 0, false
	}
	return p.counts[len(p.counts)-1], true
}

func (p *publisher) sendKeyframe(t *testing.T, msg []byte) {
	t.Helper()
	st, err := p.sess.OpenUniStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(msg); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
}

func keyframeMsg(t *testing.T, frameID uint32) []byte {
	t.Helper()
	cfg, err := wire.AppendDecoderConfig(nil, wire.DecoderConfig{Codec: "avc1.42E01E", Extradata: []byte{1, 0x42, 0xE0, 0x1E}})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{byte(frameID), 0x5A}, 4000)
	msg, err := wire.AppendStreamFrameHeader(nil, wire.StreamFrameHeader{
		Keyframe: true, FrameID: frameID, TimestampUs: uint64(frameID) * 16_667,
		ConfigLen: uint32(len(cfg)), PayloadLen: uint32(len(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append(msg, cfg...), payload...)
}

func deltaChunks(t *testing.T, frameID uint32, n int) [][]byte {
	t.Helper()
	var out [][]byte
	for i := range n {
		d, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{
			FrameID: frameID, ChunkIndex: uint16(i), ChunkCount: uint16(n), TimestampUs: uint64(frameID) * 16_667,
		}, bytes.Repeat([]byte{byte(frameID), byte(i)}, 400))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// viewer records what a subscriber session receives.
type viewer struct {
	sess      *webtransport.Session
	keyframes chan []byte
	mu        sync.Mutex
	dgrams    [][]byte
	closing   atomic.Uint32 // the in-band SessionClosing code
}

func subscribe(t *testing.T, tlsCfg *tls.Config, addr, id string) (*viewer, error) {
	t.Helper()
	sess, err := dialWT(t, tlsCfg, "https://"+addr+"/subscribe/"+id)
	if err != nil {
		return nil, err
	}
	v := &viewer{sess: sess, keyframes: make(chan []byte, 16)}
	go func() {
		for {
			st, err := sess.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				data, _ := io.ReadAll(st)
				_, typ, err := wire.PeekType(data)
				if err != nil {
					return
				}
				switch typ {
				case wire.TypeStreamFrame:
					v.keyframes <- data
				case wire.TypeSessionClosing:
					if code, err := wire.ParseSessionClosing(data); err == nil {
						v.closing.Store(code)
					}
				}
			}()
		}
	}()
	go func() {
		for {
			d, err := sess.ReceiveDatagram(context.Background())
			if err != nil {
				return
			}
			v.mu.Lock()
			v.dgrams = append(v.dgrams, d)
			v.mu.Unlock()
		}
	}()
	return v, nil
}

func (v *viewer) nextKeyframe(t *testing.T, within time.Duration) []byte {
	t.Helper()
	select {
	case k := <-v.keyframes:
		return k
	case <-time.After(within):
		t.Fatalf("no keyframe within %v", within)
	}
	return nil
}

func (v *viewer) has(d []byte) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, g := range v.dgrams {
		if bytes.Equal(g, d) {
			return true
		}
	}
	return false
}

func (v *viewer) hasType(typ byte) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, g := range v.dgrams {
		if len(g) >= 2 && g[1] == typ {
			return true
		}
	}
	return false
}

// closeCode waits for the session's end and returns its code.
func (v *viewer) closeCode(t *testing.T, within time.Duration) uint32 {
	t.Helper()
	select {
	case <-v.sess.Context().Done():
	case <-time.After(within):
		t.Fatalf("session still open after %v", within)
	}
	// A closed session answers every call with its SessionError.
	var se *webtransport.SessionError
	if _, err := v.sess.AcceptStream(context.Background()); errors.As(err, &se) {
		return uint32(se.ErrorCode)
	}
	return 0
}

// sendDeltasUntilSeen sends one delta frame's chunks until the viewer has
// all of them (datagrams on loopback are near-lossless, but not lossless),
// and fails if they never come.
func sendDeltasUntilSeen(t *testing.T, p *publisher, v *viewer, frameID uint32) [][]byte {
	t.Helper()
	chunks := deltaChunks(t, frameID, 3)
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, c := range chunks {
			_ = p.sess.SendDatagram(c)
		}
		time.Sleep(100 * time.Millisecond)
		all := true
		for _, c := range chunks {
			all = all && v.has(c)
		}
		if all {
			return chunks
		}
		if time.Now().After(deadline) {
			t.Fatalf("frame %d's deltas never reached the viewer byte-identical", frameID)
		}
	}
}

// TestOutsideViewerGetsTheBroadcastByteIdenticalOnlyOnDemand: a PoP's
// broadcast costs the venue nothing until an outside viewer asks (the leg
// is the only uplink, and nothing is dialled before the want); the viewer,
// on a pod other than the leg's, then starts from a byte-identical prime
// and receives byte-identical deltas; the counts are right on both sides;
// and the leg's delivery follows home's CapUplinkCarriers and the PoP's
// -pop-leg-delivery.
func TestOutsideViewerGetsTheBroadcastByteIdenticalOnlyOnDemand(t *testing.T) {
	for _, tc := range []struct {
		name         string
		homeCarriers bool
		delivery     string
		wantCarrier  bool
	}{
		{"carrier", true, "", true},
		{"home without uplink carriers", false, "", false},
		{"pop-leg-delivery=datagram", true, config.PopLegDatagram, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := startUplinkFleet(t, 3, fleetOpts{
				home: func(c *config.Config) { c.UplinkCarriers = tc.homeCarriers },
				popO: func(o *popclient.Options) { o.Relay.PopLegDelivery = tc.delivery },
			})
			u.f.Route(0)
			u.pair(t)
			u.f.Route(2) // legs land on home-2; the outside viewer is on home-1

			p := publishOnPoP(t, u)
			u.reserved(t, p.id)
			kf := keyframeMsg(t, 1)
			p.sendKeyframe(t, kf)
			_ = p.sess.SendDatagram(wire.AppendClockMapping(nil, 1_234_567))
			popsrv.WaitFor(t, "the PoP hub caches the keyframe", func() bool {
				st, ok := u.pop.Registry.Stats().Broadcasts[u.pop.Registry.ObfuscateID(p.id)]
				return ok && st.CachedKeyframeID == 1
			})
			// No demand: not a byte up, not a leg dialled.
			time.Sleep(time.Second)
			if s := u.pop.Client.UplinkStats(); s.Bytes != 0 || s.Legs != 0 {
				t.Fatalf("uplink before any demand: %+v, want zero bytes and no leg", s)
			}

			v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
			if err != nil {
				t.Fatalf("outside viewer refused: %v", err)
			}
			got := v.nextKeyframe(t, 15*time.Second)
			if !bytes.Equal(got, kf) {
				t.Fatalf("prime keyframe differs: %d bytes, want %d", len(got), len(kf))
			}
			for f := uint32(2); f <= 4; f++ {
				sendDeltasUntilSeen(t, p, v, f)
			}
			popsrv.WaitFor(t, "a rewritten clock mapping reaches the viewer", func() bool { return v.hasType(wire.TypeClockMapping) })

			var bu popclient.BroadcastUplink
			for _, b := range u.pop.Client.UplinkBroadcasts() {
				if b.ID == p.id {
					bu = b
				}
			}
			if !bu.Forwarded || bu.Carrier != tc.wantCarrier || bu.Reservation != "ok" {
				t.Fatalf("uplink state = %+v, want forwarded, carrier %v", bu, tc.wantCarrier)
			}
			st := u.f.Pods[2].Registry.Stats().Broadcasts[u.f.Pods[2].Registry.ObfuscateID(p.id)]
			if (st.UplinkCarriers > 0) != tc.wantCarrier {
				t.Fatalf("home-2 accepted %d uplink carriers, want carriers %v", st.UplinkCarriers, tc.wantCarrier)
			}
			if s := u.pop.Client.UplinkStats(); s.Legs != 1 || s.Bytes == 0 || s.Streams != 1 {
				t.Fatalf("uplink with one outside viewer: %+v", s)
			}

			// Counts, both ways: home's G is the outside viewer; the PoP's
			// broadcaster sees its local viewers plus G_home.
			popsrv.WaitFor(t, "home-2's G counts the outside viewer", func() bool {
				_, g, _ := u.f.Pods[2].Registry.BroadcastState(p.id)
				return g == 1
			})
			popsrv.WaitFor(t, "the broadcaster sees the outside viewer", func() bool {
				n, ok := p.lastCount()
				return ok && n == 1
			})
			if _, err := subscribe(t, u.f.ClientTLS, u.pop.Addr(), p.id); err != nil {
				t.Fatalf("party viewer: %v", err)
			}
			popsrv.WaitFor(t, "the broadcaster sees local + G_home", func() bool {
				n, _ := p.lastCount()
				return n == 2
			})
			time.Sleep(1500 * time.Millisecond)
			if _, g, _ := u.f.Pods[2].Registry.BroadcastState(p.id); g != 1 {
				t.Fatalf("home's G = %d after a party viewer joined, want 1 (the uplink carries no party count)", g)
			}
		})
	}
}

// TestLingerClosesTheLegWith4013AndHandsBackTheReservation: with no
// downstream on the leg's pod for -pop-uplink-linger, home closes the leg
// with 4013 and the Lease is a dormant reservation again; the PoP keeps the
// broadcast local, and the next outside viewer wants it again.
func TestLingerClosesTheLegWith4013AndHandsBackTheReservation(t *testing.T) {
	u := startUplinkFleet(t, 2, fleetOpts{home: func(c *config.Config) { c.PopUplinkLinger = 2 * time.Second }})
	u.f.Route(0)
	u.pair(t)
	u.f.Route(1)
	p := publishOnPoP(t, u)
	u.reserved(t, p.id)
	kf := keyframeMsg(t, 1)
	p.sendKeyframe(t, kf)

	// The viewer is on the leg's pod, so its departure leaves no edge pull
	// lingering there.
	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.nextKeyframe(t, 15*time.Second); !bytes.Equal(got, kf) {
		t.Fatal("prime differs")
	}
	popsrv.WaitFor(t, "home-1 holds the lease", func() bool {
		o, _, _, _ := u.f.Pods[0].Coord.Lookup(p.id)
		return o.Holder == u.f.Pods[1].Name
	})
	gen0, _ := u.f.Pods[1].Coord.OriginGeneration(p.id)
	v.sess.CloseWithError(0, "bye")

	popsrv.WaitFor(t, "the PoP's want ends with 4013", func() bool {
		for _, b := range u.pop.Client.UplinkBroadcasts() {
			if b.ID == p.id {
				return !b.Wanted && !b.Forwarded
			}
		}
		return false
	})
	popsrv.WaitFor(t, "the lease is a dormant reservation again", func() bool {
		o, _, _, ok := u.f.Pods[0].Coord.Lookup(p.id)
		return ok && o.Holder == "" && o.Pop == upPop && o.Generation == gen0
	})
	popsrv.WaitFor(t, "the lingered hub leaves home-1", func() bool {
		_, _, known := u.f.Pods[1].Registry.BroadcastState(p.id)
		return !known
	})
	if _, _, known := u.pop.Registry.BroadcastState(p.id); !known {
		t.Fatal("the broadcast is gone at the PoP")
	}

	// Demand again: a fresh want, a second leg.
	v2, err := subscribe(t, u.f.ClientTLS, u.f.Pods[0].Addr(), p.id)
	if err != nil {
		t.Fatal(err)
	}
	if got := v2.nextKeyframe(t, 15*time.Second); !bytes.Equal(got, kf) {
		t.Fatal("prime differs on the second leg")
	}
	if s := u.pop.Client.UplinkStats(); s.Legs != 2 {
		t.Fatalf("legs = %d, want 2", s.Legs)
	}
}

// TestAnEndedLegsLingerHandsTheLeaseBack: a leg that ends without 4013
// (here the PoP host goes away) leaves the hub in grace with the Lease held
// on the leg's pod; once nobody is downstream for -pop-uplink-linger, the
// linger hands the Lease back to a dormant reservation instead of leaving
// it to the grace deadline, where the janitor would delete it.
func TestAnEndedLegsLingerHandsTheLeaseBack(t *testing.T) {
	u := startUplinkFleet(t, 2, fleetOpts{home: func(c *config.Config) { c.PopUplinkLinger = 2 * time.Second }})
	u.f.Route(0)
	u.pair(t)
	u.f.Route(1)
	p := publishOnPoP(t, u)
	u.reserved(t, p.id)
	kf := keyframeMsg(t, 1)
	p.sendKeyframe(t, kf)
	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.nextKeyframe(t, 15*time.Second); !bytes.Equal(got, kf) {
		t.Fatal("prime differs")
	}
	popsrv.WaitFor(t, "home-1 holds the lease", func() bool {
		o, _, _, _ := u.f.Pods[0].Coord.Lookup(p.id)
		return o.Holder == u.f.Pods[1].Name
	})
	gen0, _ := u.f.Pods[1].Coord.OriginGeneration(p.id)

	// The PoP host goes: its leg ends with no 4013, and the hub on home-1
	// enters its ordinary grace with the Lease still held there.
	u.pop.Stop(t)
	popsrv.WaitFor(t, "the lease is graced on home-1", func() bool {
		o, inGrace, _, ok := u.f.Pods[0].Coord.Lookup(p.id)
		return ok && inGrace && o.Holder == u.f.Pods[1].Name
	})
	v.sess.CloseWithError(0, "bye")

	popsrv.WaitFor(t, "the lease is a dormant reservation again", func() bool {
		o, _, _, ok := u.f.Pods[0].Coord.Lookup(p.id)
		return ok && o.Holder == "" && o.Pop == upPop && o.Generation == gen0
	})
	popsrv.WaitFor(t, "the lingered hub leaves home-1", func() bool {
		_, _, known := u.f.Pods[1].Registry.BroadcastState(p.id)
		return !known
	})
}

// TestATransientLegClaimFailureKeepsTheWaitingViewers: the leg's Lease
// claim failing for a reason other than ownership (an API blip) refuses the
// leg with 503 and nothing more. The outside viewers waiting on the pod's
// reservation hub stay, the pull resumes, and the PoP's redial serves them.
func TestATransientLegClaimFailureKeepsTheWaitingViewers(t *testing.T) {
	u := startUplinkFleet(t, 2, fleetOpts{})
	u.f.Route(0)
	u.pair(t)
	u.f.Route(1)
	var failed atomic.Bool
	// The pods' coordinators already call the fake; PrependReactor does not
	// take its lock (Invokes does), so take it here.
	u.f.Leases.Lock()
	u.f.Leases.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		l, ok := a.(k8stesting.UpdateAction).GetObject().(*coordv1.Lease)
		if ok && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == u.f.Pods[1].Name &&
			l.Labels[cluster.PopLabelKey] != "" && l.Spec.AcquireTime != nil && failed.CompareAndSwap(false, true) {
			return true, nil, apierrors.NewInternalError(errors.New("etcd blip"))
		}
		return false, nil, nil
	})
	u.f.Leases.Unlock()
	p := publishOnPoP(t, u)
	u.reserved(t, p.id)
	kf := keyframeMsg(t, 1)
	p.sendKeyframe(t, kf)

	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
	if err != nil {
		t.Fatalf("outside viewer refused: %v", err)
	}
	got := v.nextKeyframe(t, 20*time.Second)
	if !bytes.Equal(got, kf) {
		t.Fatalf("prime differs: %d bytes, want %d", len(got), len(kf))
	}
	if !failed.Load() {
		t.Fatal("the leg's claim never failed; the test did not exercise the path")
	}
	if v.sess.Context().Err() != nil {
		t.Fatal("the viewer's session ended")
	}
}

// TestFourthStreamIsRefusedWith4012: the PoP's default budget forwards
// three streams; the fourth outside broadcast's viewer is told 4012
// PopUplinkFull, in-band first (docs/76 item 28).
func TestFourthStreamIsRefusedWith4012(t *testing.T) {
	u := startUplinkFleet(t, 2, fleetOpts{})
	u.f.Route(0)
	u.pair(t)
	u.f.Route(1)
	var pubs []*publisher
	for range 4 {
		p := publishOnPoP(t, u)
		u.reserved(t, p.id)
		p.sendKeyframe(t, keyframeMsg(t, 1))
		pubs = append(pubs, p)
	}
	for i, p := range pubs[:3] {
		v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[0].Addr(), p.id)
		if err != nil {
			t.Fatalf("viewer %d: %v", i, err)
		}
		v.nextKeyframe(t, 15*time.Second)
	}
	if s := u.pop.Client.UplinkStats(); s.Streams != pops.DefaultUplinkStreams {
		t.Fatalf("streams = %d, want %d", s.Streams, pops.DefaultUplinkStreams)
	}
	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[0].Addr(), pubs[3].id)
	if err != nil {
		t.Fatalf("the fourth viewer was refused before the upgrade: %v", err)
	}
	if code := v.closeCode(t, 15*time.Second); code != wire.CloseCodePopUplinkFull {
		t.Fatalf("fourth viewer closed with %d, want %d", code, wire.CloseCodePopUplinkFull)
	}
	if got := v.closing.Load(); got != wire.CloseCodePopUplinkFull {
		t.Errorf("in-band SessionClosing = %d, want %d", got, wire.CloseCodePopUplinkFull)
	}
	if s := u.pop.Client.UplinkStats(); s.Legs != 3 {
		t.Fatalf("legs = %d after the refusal, want 3", s.Legs)
	}
	// The console's "Party-only (budget full)" reads this (docs/76 §7.1):
	// the refused broadcast only, while the budget stays full.
	for _, b := range u.pop.Client.UplinkBroadcasts() {
		if want := b.ID == pubs[3].id; b.BudgetFull != want {
			t.Errorf("broadcast %s (forwarded %v): BudgetFull = %v, want %v", b.ID, b.Forwarded, b.BudgetFull, want)
		}
	}
}

// TestMintPastTheLimitStaysPartyOnly: a mint past spec.limits.maxBroadcasts
// is answered reserved{limit} and stays local-only — no reservation, so an
// outside viewer gets a plain 404 and the PoP never forwards it.
func TestMintPastTheLimitStaysPartyOnly(t *testing.T) {
	u := startUplinkFleet(t, 1, fleetOpts{})
	u.pair(t)
	u.f.UpdateSpec(t, upPop, func(s *pops.PopSpec) { s.Limits.MaxBroadcasts = 1 })
	first := publishOnPoP(t, u)
	u.reserved(t, first.id)
	second := publishOnPoP(t, u)
	popsrv.WaitFor(t, "the second mint is party-only", func() bool {
		for _, b := range u.pop.Client.UplinkBroadcasts() {
			if b.ID == second.id {
				return b.Reservation == "limit"
			}
		}
		return false
	})
	if _, _, _, ok := u.f.Pods[0].Coord.Lookup(second.id); ok {
		t.Fatal("a lease exists for the party-only broadcast")
	}
	_, err := subscribe(t, u.f.ClientTLS, u.f.Pods[0].Addr(), second.id)
	var se *statusErr
	if !errors.As(err, &se) || se.code != http.StatusNotFound {
		t.Fatalf("outside viewer of a party-only broadcast: %v, want 404", err)
	}
	if s := u.pop.Client.UplinkStats(); s.Legs != 0 {
		t.Fatalf("legs = %d", s.Legs)
	}
}

// TestUnlinkedMintIsReservedOnRelink: a mint with no link starts at once,
// local; when the link comes up the PoP reserves it at home, and from then
// on it is joinable from outside like any reserved broadcast (docs/76 item
// 15).
func TestUnlinkedMintIsReservedOnRelink(t *testing.T) {
	u := startUplinkFleet(t, 2, fleetOpts{})
	p := publishOnPoP(t, u) // not paired yet: the mint does not wait on home
	kf := keyframeMsg(t, 1)
	p.sendKeyframe(t, kf)
	for _, b := range u.pop.Client.UplinkBroadcasts() {
		if b.ID == p.id && b.Reservation != "" {
			t.Fatalf("unlinked mint already %q", b.Reservation)
		}
	}

	u.f.Route(0)
	u.pair(t)
	u.reserved(t, p.id)
	popsrv.WaitFor(t, "the PoP records the reservation", func() bool {
		for _, b := range u.pop.Client.UplinkBroadcasts() {
			if b.ID == p.id {
				return b.Reservation == "ok"
			}
		}
		return false
	})
	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
	if err != nil {
		t.Fatalf("outside viewer refused: %v", err)
	}
	if got := v.nextKeyframe(t, 15*time.Second); !bytes.Equal(got, kf) {
		t.Fatal("prime differs")
	}
}

// TestTenSecondOutageDoesNotEndTheLeg: the leg dials with the link's QUIC
// timers (15 s idle, 5 s keepalive), so a 10 s blackout of the venue's
// uplink delays the stream without ending the leg (docs/76 item 18).
func TestTenSecondOutageDoesNotEndTheLeg(t *testing.T) {
	if testing.Short() {
		t.Skip("10 s outage")
	}
	var proxy *udpBlackhole
	u := startUplinkFleet(t, 2, fleetOpts{popO: func(o *popclient.Options) {
		orig := o.Dial
		o.Dial = func(ctx context.Context, network, target string, h http.Header) (popclient.Session, *http.Response, error) {
			if strings.Contains(target, "/pop/publish/") && proxy != nil {
				target = strings.Replace(target, popsrv.HarnessEndpoint, proxy.addr(), 1)
			}
			return orig(ctx, network, target, h)
		}
	}})
	proxy = newUDPBlackhole(t, u.f.Pods[1].Addr())
	u.f.Route(0)
	u.pair(t)
	p := publishOnPoP(t, u)
	u.reserved(t, p.id)
	kf := keyframeMsg(t, 1)
	p.sendKeyframe(t, kf)
	v, err := subscribe(t, u.f.ClientTLS, u.f.Pods[1].Addr(), p.id)
	if err != nil {
		t.Fatal(err)
	}
	v.nextKeyframe(t, 15*time.Second)
	sendDeltasUntilSeen(t, p, v, 2)

	proxy.blackout(10 * time.Second)
	sendDeltasUntilSeen(t, p, v, 3)
	if s := u.pop.Client.UplinkStats(); s.Legs != 1 {
		t.Fatalf("legs = %d after a 10 s outage, want the one leg", s.Legs)
	}
	if v.sess.Context().Err() != nil {
		t.Fatal("the outside viewer's session ended")
	}
}

// udpBlackhole is a UDP proxy in front of one home pod that can drop every
// packet both ways for a while.
type udpBlackhole struct {
	conn     *net.UDPConn
	upstream *net.UDPAddr
	until    atomic.Int64
	mu       sync.Mutex
	peers    map[string]*net.UDPConn
}

func newUDPBlackhole(t *testing.T, upstream string) *udpBlackhole {
	t.Helper()
	up, err := net.ResolveUDPAddr("udp", upstream)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	b := &udpBlackhole{conn: c, upstream: up, peers: map[string]*net.UDPConn{}}
	t.Cleanup(func() {
		c.Close()
		b.mu.Lock()
		for _, p := range b.peers {
			p.Close()
		}
		b.mu.Unlock()
	})
	go b.run()
	return b
}

func (b *udpBlackhole) addr() string { return b.conn.LocalAddr().String() }

func (b *udpBlackhole) blackout(d time.Duration) {
	b.until.Store(time.Now().Add(d).UnixNano())
	time.Sleep(d)
}

func (b *udpBlackhole) dark() bool { return time.Now().UnixNano() < b.until.Load() }

func (b *udpBlackhole) run() {
	buf := make([]byte, 65536)
	for {
		n, from, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		b.mu.Lock()
		peer := b.peers[from.String()]
		if peer == nil {
			peer, err = net.DialUDP("udp", nil, b.upstream)
			if err != nil {
				b.mu.Unlock()
				continue
			}
			b.peers[from.String()] = peer
			go b.back(peer, from)
		}
		b.mu.Unlock()
		if !b.dark() {
			_, _ = peer.Write(buf[:n])
		}
	}
}

func (b *udpBlackhole) back(peer *net.UDPConn, to *net.UDPAddr) {
	buf := make([]byte, 65536)
	for {
		n, err := peer.Read(buf)
		if err != nil {
			return
		}
		if !b.dark() {
			_, _ = b.conn.WriteToUDP(buf[:n], to)
		}
	}
}
