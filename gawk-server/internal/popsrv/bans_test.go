package popsrv

// The PP10 acceptance rows (docs/76 §11) in-process: a kill at home ends a
// party broadcast at the PoP with 4006, a banned ID cannot be reclaimed
// there, a revoke releases the reservations (outsiders see 4000 / 404, no
// offer, the PoP stays standalone), and the PoP's 0x11 carries the
// screening bit while home's does.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// moderated is the harness with the moderation reach wired as serve wires
// it: every home pod gets a ban set, the cluster reservation store and a
// forwarder chained on its ban callback; the PoP gets its own ban set and
// EnableModeration.
type moderated struct {
	f      *HomeFleet
	pop    *PoP
	sets   []*moderation.Set
	res    []Reservations
	fws    []*BanForwarder
	popBan *moderation.Set
	popMod *popclient.Moderation
	bans   atomic.Int32 // ban records the PoP received
}

func startModerated(t *testing.T, n int, opts ...HarnessOption) *moderated {
	t.Helper()
	f, pop := StartHomeAndPop(t, n, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &moderated{f: f, pop: pop}
	for _, pod := range f.Pods {
		set := moderation.NewSet()
		pod.Server.SetModeration(set)
		res := NewClusterReservations(pod.Coord)
		fw := pod.Manager.EnableModeration(ctx, ModerationOptions{Bans: set, Reservations: res, Sweep: 100 * time.Millisecond})
		// The home answers the PoP's reserve records, as serve wires it in
		// production: the PoP's mint gate waits on that answer before a
		// party broadcaster's identity is sent.
		pod.Manager.StartUplink(UplinkOptions{Reservations: res})
		pod.Server.AddBanObserver(fw.HandleBan)
		h.sets = append(h.sets, set)
		h.res = append(h.res, res)
		h.fws = append(h.fws, fw)
	}
	h.popBan = moderation.NewSet()
	pop.Server.SetModeration(h.popBan)
	h.popMod = pop.Client.EnableModeration(ctx, h.popBan)
	pop.Client.OnRecord(poplink.TBan, func(any) { h.bans.Add(1) })
	return h
}

// ban applies rec on every home pod as its ban source would: into the set,
// then the callback.
func (h *moderated) ban(rec moderation.Record) {
	for i, pod := range h.f.Pods {
		_ = h.sets[i].Upsert(rec)
		pod.Server.HandleBanAdded(rec)
	}
}

func killRecord(id string, cooldown time.Duration) moderation.Record {
	exp := time.Now().Add(cooldown)
	return moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id}, ExpiresAt: &exp, Reason: "kill"}
}

// popPublisher starts a broadcast at the PoP and returns its session and ID.
func popPublisher(t *testing.T, h *moderated) (*webtransport.Session, string) {
	t.Helper()
	sess := clientDial(t, h.f, "https://"+h.pop.Addr()+"/publish")
	id, _ := publisherHandshake(t, pairCtx(t), sess)
	return sess, id
}

// closeCode drains sess until it ends and returns the SessionClosing notice
// and the close code.
func closeCode(t *testing.T, sess *webtransport.Session) (notice uint32, code webtransport.SessionErrorCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		str, err := sess.AcceptUniStream(ctx)
		if err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				// Errorf, not Fatalf: callers run this on goroutines.
				t.Errorf("session ended with %v, want a WebTransport close", err)
				return notice, 0
			}
			return notice, se.ErrorCode
		}
		_ = str.SetReadDeadline(time.Now().Add(2 * time.Second))
		msg, _ := io.ReadAll(io.LimitReader(str, 1<<20))
		if len(msg) >= 2 && msg[1] == wire.TypeSessionClosing {
			notice, _ = wire.ParseSessionClosing(msg)
		}
	}
}

// dialStatus dials target and returns the refusal's status (200 when it
// upgraded).
func dialStatus(t *testing.T, h *moderated, target string) int {
	t.Helper()
	d := &webtransport.Transport{
		TLSClientConfig: h.f.ClientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	defer d.Close()
	rsp, sess, err := d.Dial(pairCtx(t), target, nil)
	if err != nil {
		if rsp == nil {
			t.Fatalf("dial %s: %v", target, err)
		}
		return rsp.StatusCode
	}
	_ = sess.CloseWithError(0, "")
	return http.StatusOK
}

// A kill in gawk-admin (an ID ban with a cooldown) on a reserved party
// broadcast ends it at the PoP with 4006 for its broadcaster and its
// viewers, in-band notice first; the ID's reclaim is then refused at the
// PoP (451) until the ban ends.
func TestAKillAtHomeEndsThePartyBroadcastWith4006(t *testing.T) {
	h := startModerated(t, 3)
	pairLinked(t, h.f, h.pop)
	pub, id := popPublisher(t, h)
	viewer := clientDial(t, h.f, "https://"+h.pop.Addr()+"/subscribe/"+id)
	WaitFor(t, "viewer registered", func() bool { return h.pop.Registry.Stats().Totals.Subscribers == 1 })

	holder := h.f.LinkHolder(popName)
	idx := podIndex(h.f, holder)
	// The PoP's mint reserved the ID through the home's uplink subsystem.
	WaitFor(t, "the reservation in the link-home pod's cache", func() bool {
		p, ok := h.res[idx].Owner(id)
		return ok && p == popName
	})

	var wg sync.WaitGroup
	var pubNotice, viewNotice uint32
	var pubCode, viewCode webtransport.SessionErrorCode
	wg.Add(2)
	go func() { defer wg.Done(); pubNotice, pubCode = closeCode(t, pub) }()
	go func() { defer wg.Done(); viewNotice, viewCode = closeCode(t, viewer) }()
	h.ban(killRecord(id, 5*time.Minute))
	wg.Wait()
	for who, got := range map[string][2]uint32{"broadcaster": {pubNotice, uint32(pubCode)}, "viewer": {viewNotice, uint32(viewCode)}} {
		if got[0] != wire.CloseCodeTerminatedByOperator || got[1] != wire.CloseCodeTerminatedByOperator {
			t.Errorf("%s: notice %d, close %d; want 4006 for both", who, got[0], got[1])
		}
	}
	if s := dialStatus(t, h, "https://"+h.pop.Addr()+"/publish/"+id); s != http.StatusUnavailableForLegalReasons {
		t.Fatalf("reclaim of the killed ID at the PoP = %d, want 451", s)
	}
	if got := h.popMod.Active(); len(got) != 1 || got[0].Target.Value != id || got[0].ExpiresAt == nil {
		t.Fatalf("PoP's forwarded bans = %+v, want the kill's cooldown", got)
	}
}

// A party-only broadcast with no reservation (the PoP was at its
// limits.maxBroadcasts) is still reached: the PoP named it on the link.
func TestAKillReachesAPartyOnlyBroadcastTheLinkNamed(t *testing.T) {
	h := startModerated(t, 1)
	pairLinked(t, h.f, h.pop)
	pub, id := popPublisher(t, h)
	if err := h.pop.Client.Send(poplink.State{ID: id, Live: true}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "home learned the ID", func() bool { return len(h.fws[0].owners(id)) == 1 })
	h.ban(killRecord(id, time.Minute))
	if notice, code := closeCode(t, pub); notice != wire.CloseCodeTerminatedByOperator || code != wire.CloseCodeTerminatedByOperator {
		t.Fatalf("party-only broadcaster: notice %d close %d, want 4006", notice, code)
	}
}

// The ban source re-fires every record on each resync: the PoP hears of a
// ban once per link, not once per resync. A relink (a drain) sends it once
// more, because a restarted PoP may have lost it. An expired record and an
// ID ban on a home broadcast never reach the PoP.
func TestForwardedBansAreIdempotentAcrossResyncs(t *testing.T) {
	h := startModerated(t, 2)
	pairLinked(t, h.f, h.pop)
	const id = "ABC234"
	holder := h.f.LinkHolder(popName)
	if _, err := h.res[podIndex(h.f, holder)].Reserve(pairCtx(t), id, popName, 0); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "reservation cached", func() bool { _, ok := h.res[podIndex(h.f, holder)].Owner(id); return ok })
	rec := moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id}}
	for range 5 {
		h.ban(rec)
	}
	WaitFor(t, "the ban reached the PoP", func() bool { return h.bans.Load() >= 1 })
	time.Sleep(500 * time.Millisecond) // five sweeps
	if n := h.bans.Load(); n != 1 {
		t.Fatalf("the PoP received %d ban records over 5 resyncs and 5 sweeps, want 1", n)
	}
	past := time.Now().Add(-time.Minute)
	h.ban(moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC235"}})
	h.ban(moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC236"}, ExpiresAt: &past})
	time.Sleep(300 * time.Millisecond)
	if n := h.bans.Load(); n != 1 {
		t.Fatalf("bans on IDs that are not the PoP's reached it (%d records)", n)
	}

	other := h.f.Pods[1-podIndex(h.f, holder)]
	h.f.Route(podIndex(h.f, other))
	holder.Drain(t)
	WaitFor(t, "relinked to the other pod", func() bool { return h.f.LinkHolder(popName) == other })
	WaitFor(t, "the ban re-sent on the new link", func() bool { return h.bans.Load() == 2 })
	time.Sleep(400 * time.Millisecond)
	if n := h.bans.Load(); n != 2 {
		t.Fatalf("after the relink the PoP received %d ban records, want 2", n)
	}
}

// An operator lifting a forwarded ban at home lifts it at the PoP too: a
// permanent ban banned by mistake, or a cooldown ended early, must not
// keep refusing the ID's reclaims there until the relay restarts. A ban
// the host set itself stays.
func TestLiftingAForwardedBanAtHomeLiftsItAtThePoP(t *testing.T) {
	h := startModerated(t, 1)
	pairLinked(t, h.f, h.pop)
	const id, own = "ABC234", "ABC235"
	for _, x := range []string{id, own} {
		if _, err := h.res[0].Reserve(pairCtx(t), x, popName, 0); err != nil {
			t.Fatal(err)
		}
	}
	WaitFor(t, "reservations cached", func() bool {
		_, a := h.res[0].Owner(id)
		_, b := h.res[0].Owner(own)
		return a && b
	})
	target := func(x string) moderation.Target {
		return moderation.Target{Type: moderation.TargetBroadcastID, Value: x}
	}
	h.ban(moderation.Record{Target: target(id)})
	h.ban(moderation.Record{Target: target(own)})
	// The host's own ban on the second ID, set at the PoP.
	if err := h.popBan.Upsert(moderation.Record{Target: target(own), CreatedBy: "host"}); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, "the bans reach the PoP", func() bool { return len(h.popMod.Active()) == 2 })

	for _, x := range []string{id, own} {
		if err := h.sets[0].Remove(target(x)); err != nil {
			t.Fatal(err)
		}
	}
	WaitFor(t, "the PoP lifts the forwarded ban", func() bool {
		_, banned := h.popBan.BannedID(id, time.Now())
		return !banned && len(h.popMod.Active()) == 0
	})
	time.Sleep(3 * time.Second) // past a reassert
	if _, banned := h.popBan.BannedID(id, time.Now()); banned {
		t.Fatal("the reassert put the lifted ban back")
	}
	if r, banned := h.popBan.BannedID(own, time.Now()); !banned || r.CreatedBy != "host" {
		t.Fatalf("the host's own ban = %+v, %v; want it kept", r, banned)
	}
}

// After a revoke the PoP's reservations are gone: an outside viewer
// waiting on one gets 4000 and a new dial 404; the PoP is no longer in
// Linked (so no offer names it), and it keeps serving its LAN standalone.
func TestARevokeReleasesTheReservationsAndLeavesThePoPStandalone(t *testing.T) {
	h := startModerated(t, 2)
	pairLinked(t, h.f, h.pop)
	_, id := popPublisher(t, h)
	holder := h.f.LinkHolder(popName)
	if _, err := h.res[podIndex(h.f, holder)].Reserve(pairCtx(t), id, popName, 0); err != nil {
		t.Fatal(err)
	}
	outside := h.f.Pods[1-podIndex(h.f, holder)]
	WaitFor(t, "reservation cached on the outside pod", func() bool { _, ok := h.res[podIndex(h.f, outside)].Owner(id); return ok })
	viewer := clientDial(t, h.f, "https://"+outside.Addr()+"/subscribe/"+id)
	WaitFor(t, "the outside viewer waits on the reservation", func() bool { return outside.Registry.Stats().Totals.Subscribers == 1 })
	if len(outside.Manager.Linked()) != 1 {
		t.Fatalf("before the revoke Linked = %+v", outside.Manager.Linked())
	}

	done := make(chan [2]uint32, 1)
	go func() { n, c := closeCode(t, viewer); done <- [2]uint32{n, uint32(c)} }()
	h.f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Revoked = true })

	var got [2]uint32
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the outside viewer was never closed after the revoke")
	}
	if got[1] != wire.CloseCodeBroadcastEnded {
		t.Errorf("outside viewer after the revoke: notice %d close %d, want 4000", got[0], got[1])
	}
	WaitFor(t, "the reservations are gone", func() bool {
		ids, err := h.f.Pods[0].Coord.ReservedIDs(context.Background(), popName)
		return err == nil && len(ids) == 0
	})
	WaitFor(t, "a new outside dial is refused with 404", func() bool {
		return dialStatus(t, h, "https://"+outside.Addr()+"/subscribe/"+id) == http.StatusNotFound
	})
	waitHeader(t, h.pop.Client, popclient.HeaderRevoked)
	for _, pod := range h.f.Pods {
		WaitFor(t, pod.Name+" stops listing the PoP", func() bool { return len(pod.Manager.Linked()) == 0 })
	}
	// Standalone: the PoP still serves its room, and the party broadcast is
	// still up there.
	if h.pop.Registry.Stats().Totals.Broadcasts != 1 {
		t.Fatalf("the PoP's own broadcast ended with the revoke: %+v", h.pop.Registry.Stats().Totals)
	}
	if s := dialStatus(t, h, "https://"+h.pop.Addr()+"/subscribe/"+id); s != http.StatusOK {
		t.Fatalf("a LAN viewer at the revoked PoP = %d, want 200", s)
	}
}

// The PoP's 0x11 carries the screening notice while linked to a home whose
// notice is on, and drops it once revoked; a home with it off leaves the
// PoP's identity bare.
func TestThePoPsIdentityMirrorsHomesScreeningNotice(t *testing.T) {
	h := startModerated(t, 1, WithHomeConfig(func(c *config.Config) { c.ScreeningNotice = true }))
	if n := popEchoNotices(t, h); n != 0 {
		t.Fatalf("an unpaired PoP's identity notices = %#x, want none", n)
	}
	pairLinked(t, h.f, h.pop)
	WaitFor(t, "the screening bit on the PoP's /echo", func() bool { return popEchoNotices(t, h)&wire.RelayNoticeScreening != 0 })
	pub := clientDial(t, h.f, "https://"+h.pop.Addr()+"/publish")
	if n, count := publishNotices(t, pub); count != 1 || n&wire.RelayNoticeScreening == 0 {
		t.Fatalf("a party broadcaster got %d identities with notices %#x, want one with the screening bit", count, n)
	}
	h.f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Revoked = true })
	waitHeader(t, h.pop.Client, popclient.HeaderRevoked)
	if n := popEchoNotices(t, h); n != 0 {
		t.Fatalf("a revoked PoP's identity notices = %#x, want none", n)
	}

	off := startModerated(t, 1)
	pairLinked(t, off.f, off.pop)
	time.Sleep(200 * time.Millisecond)
	if n := popEchoNotices(t, off); n != 0 {
		t.Fatalf("linked to a home without the notice, the PoP's notices = %#x", n)
	}
}

// A Pop whose end date passes while nobody holds its link still loses its
// reservations, on the sweep (no source event fires for a passing date).
func TestAPassedEndDateReleasesTheReservations(t *testing.T) {
	src := &fakeSource{pops: map[string]*pops.Pop{}}
	end := metav1.NewTime(time.Now().Add(150 * time.Millisecond))
	src.pops[popName] = &pops.Pop{ObjectMeta: metav1.ObjectMeta{Name: popName, UID: types.UID("u")}, Spec: pops.PopSpec{NotAfter: &end}}
	m := New(Options{Source: src})
	res := NewMemReservations()
	if r, _ := res.Reserve(context.Background(), "ABC234", popName, 0); r != ReserveOK {
		t.Fatal(r)
	}
	if r, _ := res.Reserve(context.Background(), "ABC235", "other", 0); r != ReserveOK {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.EnableModeration(ctx, ModerationOptions{Bans: moderation.NewSet(), Reservations: res, Sweep: 50 * time.Millisecond})
	WaitFor(t, "the expired PoP's reservation released", func() bool { return len(res.List(popName)) == 0 })
	if _, ok := res.Owner("ABC235"); !ok {
		t.Fatal("another PoP's reservation was released too")
	}
}

// A deleted Pop CR releases its reservations on the source event.
func TestADeletedPopReleasesItsReservations(t *testing.T) {
	src := &fakeSource{pops: map[string]*pops.Pop{popName: {ObjectMeta: metav1.ObjectMeta{Name: popName}}}}
	m := New(Options{Source: src})
	res := NewMemReservations()
	_, _ = res.Reserve(context.Background(), "ABC234", popName, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.EnableModeration(ctx, ModerationOptions{Bans: moderation.NewSet(), Reservations: res, Sweep: time.Hour})
	time.Sleep(50 * time.Millisecond)
	if len(res.List(popName)) != 1 {
		t.Fatal("a live Pop's reservation was released")
	}
	delete(src.pops, popName)
	for _, fn := range src.subs {
		fn(popName)
	}
	WaitFor(t, "the deleted PoP's reservation released", func() bool { return len(res.List(popName)) == 0 })
}

func podIndex(f *HomeFleet, p *HomePod) int {
	for i, q := range f.Pods {
		if q == p {
			return i
		}
	}
	panic(fmt.Sprintf("pod %v not in the fleet", p))
}

func popEchoNotices(t *testing.T, h *moderated) uint8 {
	t.Helper()
	sess := clientDial(t, h.f, "https://"+h.pop.Addr()+"/echo")
	defer sess.CloseWithError(0, "")
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			t.Fatalf("no 0x11 on the PoP's /echo: %v", err)
		}
		data, _ := io.ReadAll(st)
		if len(data) >= 2 && data[1] == wire.TypeRelayIdentity {
			id, err := wire.ParseRelayIdentity(data)
			if err != nil {
				t.Fatal(err)
			}
			return id.Notices
		}
	}
}

// publishNotices reads a publish session's uni streams for 1.5 s and
// returns the RelayIdentity notices and how many identities came.
func publishNotices(t *testing.T, sess *webtransport.Session) (notices uint8, count int) {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		st, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			return notices, count
		}
		data, _ := io.ReadAll(st)
		if len(data) >= 2 && data[1] == wire.TypeRelayIdentity {
			id, err := wire.ParseRelayIdentity(data)
			if err != nil {
				t.Fatal(err)
			}
			notices |= id.Notices
			count++
		}
	}
}
