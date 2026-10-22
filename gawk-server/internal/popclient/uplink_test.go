package popclient

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// uplinkPoP is a linked PoP with a hub and a transport server; its leg
// dials go to legs (a fake leg per dial) unless the test routes them.
type uplinkPoP struct {
	c    *Client
	l    *homeLink
	f    *fakeHome
	r    *hub.Registry
	legs chan *fakeLeg
}

func newUplinkPoP(t *testing.T, cfg poplink.Config, mod ...func(*Options)) *uplinkPoP {
	t.Helper()
	f := newFakeHome(t)
	r, srv := relayParts(t)
	o := testOptions(t, f)
	o.Registry, o.Server = r, srv
	for _, m := range mod {
		m(&o)
	}
	p := &uplinkPoP{f: f, r: r, legs: make(chan *fakeLeg, 8)}
	f.route(p.legRoute)
	p.c, p.l = linkUp(t, f, o, cfg)
	return p
}

func (p *uplinkPoP) legRoute(d dialRecord) (Session, *http.Response, error) {
	if !strings.HasPrefix(d.url.Path, "/pop/publish/") {
		return nil, nil, nil
	}
	leg := newFakeLeg()
	p.legs <- leg
	return leg, nil, nil
}

func (p *uplinkPoP) nextLeg(t *testing.T) *fakeLeg {
	t.Helper()
	select {
	case l := <-p.legs:
		return l
	case <-time.After(waitFor):
		t.Fatal("no leg dial")
		return nil
	}
}

// publish starts a live broadcast in the PoP's hub.
func (p *uplinkPoP) publish(t *testing.T) (string, *hub.Publisher) {
	t.Helper()
	id, pub, err := p.r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	return id, pub
}

// reserve mints id through the gate with home answering result.
func (p *uplinkPoP) reserve(t *testing.T, id, result string) bool {
	t.Helper()
	local := make(chan bool, 1)
	go func() {
		lo, err := p.c.up.ReserveMint(t.Context(), id)
		if err != nil {
			t.Errorf("ReserveMint: %v", err)
		}
		local <- lo
	}()
	r := expect[*poplink.Reserve](t, p.l)
	if !slices.Equal(r.IDs, []string{id}) {
		t.Fatalf("reserve %v, want [%s]", r.IDs, id)
	}
	p.l.send(poplink.Reserved{ID: id, Result: result})
	select {
	case lo := <-local:
		return lo
	case <-time.After(waitFor):
		t.Fatal("ReserveMint did not return")
		return false
	}
}

func (p *uplinkPoP) row(id string) (BroadcastUplink, bool) {
	for _, b := range p.c.UplinkBroadcasts() {
		if b.ID == id {
			return b, true
		}
	}
	return BroadcastUplink{}, false
}

func (p *uplinkPoP) waitRow(t *testing.T, id string, ok func(BroadcastUplink, bool) bool) BroadcastUplink {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		b, found := p.row(id)
		if ok(b, found) {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: row never reached; last %+v (found %v)", id, b, found)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Reserve before announce (docs/76 §6.4): ok makes the mint forwardable,
// conflict and limit leave it local-only, and so does no answer.
func TestReserveMintLinked(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "assembly"})
	if p.reserve(t, "ABC234", poplink.ReservedOK) {
		t.Fatal("a reserved mint is local-only")
	}
	if p.reserve(t, "DEF567", poplink.ReservedConflict) != true {
		t.Fatal("a conflicting mint is forwardable")
	}
	if p.reserve(t, "GHJ892", poplink.ReservedLimit) != true {
		t.Fatal("a mint over the limit is forwardable")
	}
	rows := p.c.UplinkBroadcasts()
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.Reservation
	}
	want := map[string]string{"ABC234": resOK, "DEF567": resConflict, "GHJ892": resLimit}
	for id, res := range want {
		if got[id] != res {
			t.Errorf("%s reservation %q, want %q", id, got[id], res)
		}
	}
	if !slices.IsSortedFunc(rows, func(a, b BroadcastUplink) int { return strings.Compare(a.ID, b.ID) }) {
		t.Errorf("rows not sorted: %+v", rows)
	}

	// No answer: the mint's context ends and the mint is local, the waiter
	// dropped.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if lo, err := p.c.up.ReserveMint(ctx, "JKM345"); err != nil || !lo {
		t.Fatalf("unanswered mint = %v, %v", lo, err)
	}
	p.c.up.mu.Lock()
	n := len(p.c.up.waiters)
	p.c.up.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiters left after an unanswered mint", n)
	}
	// A late answer still makes it forwardable.
	p.l.send(poplink.Reserved{ID: "JKM345", Result: poplink.ReservedOK})
	p.waitRow(t, "JKM345", func(b BroadcastUplink, ok bool) bool { return ok && b.Reservation == resOK })
	// A reserved answer for an ID the uplink never saw is ignored.
	p.l.send(poplink.Reserved{ID: "NPQ678", Result: poplink.ReservedOK})
	time.Sleep(20 * time.Millisecond)
	if _, ok := p.row("NPQ678"); ok {
		t.Fatal("an unknown reserved answer created a broadcast")
	}
}

// Unlinked, a mint is local now; it is local-only only when the relay has
// a home to be local-only from. A linked gate whose Send fails starts the
// mint local too.
func TestReserveMintUnlinked(t *testing.T) {
	c := New(Options{Log: discard()})
	if lo, _ := c.up.ReserveMint(t.Context(), "ABC234"); lo {
		t.Fatal("a never-paired relay's mint is local-only")
	}
	c.stored = &storedCredential{Endpoint: "home.example:4433", Credential: "c"}
	if lo, _ := c.up.ReserveMint(t.Context(), "DEF567"); !lo {
		t.Fatal("an unlinked PoP's mint is forwardable at once")
	}
	// The state says linked but no control stream is up: Send fails.
	c.setState(func(s *LinkState) { s.Header = HeaderLinked })
	if lo, _ := c.up.ReserveMint(t.Context(), "GHJ892"); !lo {
		t.Fatal("a mint whose reserve was not sent is forwardable")
	}
	if len(c.up.waiters) != 0 {
		t.Fatal("a waiter left behind")
	}
	if got := c.up.key("ABC234"); got != "" {
		t.Fatalf("key without a registry = %q", got)
	}
}

// On a relink the PoP reserves what is pending and re-asserts what home
// holds, in one sorted record; conflicts and limits are not retried.
func TestRelinkReservesPending(t *testing.T) {
	f := newFakeHome(t)
	o := testOptions(t, f)
	withCredential(t, o, "")
	f.route(func(dialRecord) (Session, *http.Response, error) { return nil, nil, errors.New("down") })
	c := New(o)
	run(t, c)
	waitState(t, c, headerIs(HeaderOffline))
	for _, id := range []string{"GHJ892", "ABC234", "DEF567"} {
		if lo, _ := c.up.ReserveMint(t.Context(), id); !lo {
			t.Fatalf("%s: unlinked mint forwardable", id)
		}
	}
	c.dispatch(poplink.TReserved, &poplink.Reserved{ID: "DEF567", Result: poplink.ReservedConflict})
	c.dispatch(poplink.TReserved, &poplink.Reserved{ID: "GHJ892", Result: poplink.ReservedOK})
	f.route(nil)
	l := f.nextLink(t)
	l.send(poplink.Config{PopName: "p"})
	r := expect[*poplink.Reserve](t, l)
	if !slices.Equal(r.IDs, []string{"ABC234", "GHJ892"}) {
		t.Fatalf("relink reserve %v", r.IDs)
	}
}

// A reclaim of a broadcast the uplink does not know is reserved; a known
// one is not reserved again.
func TestPublisherLiveReservesReclaims(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p"})
	p.c.up.PublisherLive(nil, "ABC234")
	if r := expect[*poplink.Reserve](t, p.l); !slices.Equal(r.IDs, []string{"ABC234"}) {
		t.Fatalf("reclaim reserve %v", r.IDs)
	}
	p.c.up.PublisherLive(nil, "ABC234")
	none[*poplink.Reserve](t, p.l, 50*time.Millisecond)
}

// A want for a live, reserved broadcast opens the leg (docs/76 item 19):
// the dial carries the credential and the Pop, the leg counts in the
// stats while up, a repeated want is idempotent, and home's 4013 ends the
// want.
func TestWantOpensTheLegUntilNoDemand(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "assembly"})
	id, pub := p.publish(t)
	defer pub.Close()
	p.reserve(t, id, poplink.ReservedOK)
	if st := expect[*poplink.State](t, p.l); st.ID != id || !st.Live {
		t.Fatalf("state = %+v", st)
	}
	p.l.send(poplink.Want{ID: id})
	leg := p.nextLeg(t)
	d := p.f.dialsTo("/pop/publish/" + id)
	if len(d) != 1 || d[0].hdr.Get("Authorization") != "Bearer cred-1" || d[0].url.Query().Get("pop") != "assembly" {
		t.Fatalf("leg dials %+v", d)
	}
	row := p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Forwarded })
	if !row.Wanted || row.Carrier {
		t.Fatalf("forwarded row %+v (home sent no capabilities: a datagram leg)", row)
	}
	if s := p.c.UplinkStats(); s.Streams != 1 || s.Legs != 1 {
		t.Fatalf("uplink stats %+v", s)
	}
	if st := p.c.stats(); st.UplinkStreams != 1 {
		t.Fatalf("stats record %+v", st)
	}
	// The leg pings home at once: bytes counted.
	deadline := time.Now().Add(waitFor)
	for p.c.UplinkStats().Bytes == 0 || leg.datagrams() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the leg wrote nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.l.send(poplink.Want{ID: id})
	time.Sleep(30 * time.Millisecond)
	if n := len(p.f.dialsTo("/pop/publish/")); n != 1 {
		t.Fatalf("%d leg dials after a repeated want", n)
	}
	leg.homeClose(wire.CloseCodePopNoDemand)
	row = p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return !b.Wanted && !b.Forwarded })
	if s := p.c.UplinkStats(); s.Streams != 0 {
		t.Fatalf("stats after 4013: %+v", s)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(p.f.dialsTo("/pop/publish/")); n != 1 {
		t.Fatalf("redialled after 4013: %d dials", n)
	}
}

// A leg that ends without a reason is redialled while still wanted; one
// home ends with a terminal code is not.
func TestLegRedialsUntilTerminal(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p"})
	id, pub := p.publish(t)
	defer pub.Close()
	p.reserve(t, id, poplink.ReservedOK)
	p.l.send(poplink.Want{ID: id})
	leg := p.nextLeg(t)
	leg.homeClose(4999)
	leg2 := p.nextLeg(t)
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Forwarded })
	leg2.homeClose(wire.CloseCodeTerminatedByOperator)
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return !b.Wanted && !b.Forwarded })
	select {
	case <-p.legs:
		t.Fatal("redialled after a terminal close")
	case <-time.After(100 * time.Millisecond):
	}
}

// A want the PoP cannot meet is refused: unknown to the hub, not reserved
// at home, or over the uplink budget (which the console shows as
// BudgetFull until a stream frees up).
func TestWantRefusals(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p", Uplink: pops.Budget{MaxStreams: 1}})
	p.l.send(poplink.Want{ID: "ABC234"})
	if r := expect[*poplink.Refuse](t, p.l); r.ID != "ABC234" || r.Reason != poplink.RefuseUnknown {
		t.Fatalf("unknown want: %+v", r)
	}
	conflict, cpub := p.publish(t)
	defer cpub.Close()
	p.reserve(t, conflict, poplink.ReservedConflict)
	p.l.send(poplink.Want{ID: conflict})
	if r := expect[*poplink.Refuse](t, p.l); r.ID != conflict || r.Reason != poplink.RefuseUnknown {
		t.Fatalf("conflict want: %+v", r)
	}

	a, apub := p.publish(t)
	defer apub.Close()
	b, bpub := p.publish(t)
	defer bpub.Close()
	p.reserve(t, a, poplink.ReservedOK)
	p.reserve(t, b, poplink.ReservedOK)
	p.l.send(poplink.Want{ID: a})
	legA := p.nextLeg(t)
	p.waitRow(t, a, func(r BroadcastUplink, _ bool) bool { return r.Forwarded })
	p.l.send(poplink.Want{ID: b})
	if r := expect[*poplink.Refuse](t, p.l); r.ID != b || r.Reason != poplink.RefuseBudget {
		t.Fatalf("over-budget want: %+v", r)
	}
	if row, _ := p.row(b); !row.BudgetFull || row.Wanted {
		t.Fatalf("refused row %+v", row)
	}
	// The stream ends: the budget is no longer full.
	legA.homeClose(wire.CloseCodePopNoDemand)
	p.waitRow(t, b, func(r BroadcastUplink, _ bool) bool { return !r.BudgetFull })
	// Now admitted.
	p.l.send(poplink.Want{ID: b})
	p.nextLeg(t)
	p.waitRow(t, b, func(r BroadcastUplink, _ bool) bool { return r.Forwarded && !r.BudgetFull })
}

// A want for a broadcast whose broadcaster is away is kept: the leg dials
// when they are back, closes when they go away again, and an ended
// broadcast is released.
func TestWantWaitsForTheBroadcaster(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p"})
	id, pub := p.publish(t)
	p.reserve(t, id, poplink.ReservedOK)
	expect[*poplink.State](t, p.l)
	pub.Close()
	if st := expect[*poplink.State](t, p.l); st.ID != id || st.Live {
		t.Fatalf("away state %+v", st)
	}
	p.l.send(poplink.Want{ID: id})
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Wanted })
	select {
	case <-p.legs:
		t.Fatal("a leg for an away broadcast")
	case <-time.After(100 * time.Millisecond):
	}
	_, pub2, err := p.r.ResumePublish(id)
	if err != nil {
		t.Fatal(err)
	}
	leg := p.nextLeg(t)
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Forwarded })
	pub2.Close()
	// The broadcaster left: the PoP closes the leg and keeps the want.
	select {
	case <-leg.Context().Done():
	case <-time.After(waitFor):
		t.Fatal("the leg stayed open with the broadcaster away")
	}
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Wanted && !b.Forwarded })
	p.r.EndBroadcast(id)
	if r := expect[*poplink.Release](t, p.l); !slices.Equal(r.IDs, []string{id}) {
		t.Fatalf("release %+v", r)
	}
	p.waitRow(t, id, func(_ BroadcastUplink, found bool) bool { return !found })
}

// Home refusing a leg: 404/409 means the reservation is gone (reserve
// again, drop the want); 451/401/403/426 drop the want; anything else is
// retried.
func TestLegDialRefusals(t *testing.T) {
	for _, tc := range []struct {
		status    int
		reReserve bool
	}{
		{http.StatusNotFound, true},
		{http.StatusConflict, true},
		{http.StatusUnavailableForLegalReasons, false},
		{http.StatusForbidden, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			p := newUplinkPoP(t, poplink.Config{PopName: "p"})
			id, pub := p.publish(t)
			defer pub.Close()
			p.reserve(t, id, poplink.ReservedOK)
			p.f.route(func(d dialRecord) (Session, *http.Response, error) {
				if strings.HasPrefix(d.url.Path, "/pop/publish/") {
					return nil, &http.Response{StatusCode: tc.status, Header: http.Header{}}, errors.New("refused")
				}
				return nil, nil, nil
			})
			p.l.send(poplink.Want{ID: id})
			if tc.reReserve {
				if r := expect[*poplink.Reserve](t, p.l); !slices.Equal(r.IDs, []string{id}) {
					t.Fatalf("re-reserve %+v", r)
				}
				p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return !b.Wanted && b.Reservation == resPending })
				return
			}
			deadline := time.Now().Add(waitFor)
			for len(p.f.dialsTo("/pop/publish/")) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no leg dial")
				}
				time.Sleep(5 * time.Millisecond)
			}
			p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return !b.Wanted && b.Reservation == resOK })
			time.Sleep(legRetryMax / 10)
			if n := len(p.f.dialsTo("/pop/publish/")); n != 1 {
				t.Fatalf("%d dials for a terminal refusal", n)
			}
		})
	}
	t.Run("transient", func(t *testing.T) {
		p := newUplinkPoP(t, poplink.Config{PopName: "p"})
		id, pub := p.publish(t)
		defer pub.Close()
		p.reserve(t, id, poplink.ReservedOK)
		p.f.route(func(d dialRecord) (Session, *http.Response, error) {
			if strings.HasPrefix(d.url.Path, "/pop/publish/") {
				return nil, &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}, errors.New("draining")
			}
			return nil, nil, nil
		})
		p.l.send(poplink.Want{ID: id})
		deadline := time.Now().Add(waitFor)
		for len(p.f.dialsTo("/pop/publish/")) < 2 {
			if time.Now().After(deadline) {
				t.Fatal("a transient refusal was not retried")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if row, _ := p.row(id); !row.Wanted {
			t.Fatalf("want dropped on a transient refusal: %+v", row)
		}
	})
}

// A reserved answer that takes a forwardable broadcast back (limit after
// a home restart) closes its leg and drops the want.
func TestReservedLimitStopsTheLeg(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p"})
	id, pub := p.publish(t)
	defer pub.Close()
	p.reserve(t, id, poplink.ReservedOK)
	p.l.send(poplink.Want{ID: id})
	leg := p.nextLeg(t)
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Forwarded })
	p.l.send(poplink.Reserved{ID: id, Result: poplink.ReservedLimit})
	select {
	case <-leg.Context().Done():
	case <-time.After(waitFor):
		t.Fatal("the leg stayed open")
	}
	p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool {
		return b.Reservation == resLimit && !b.Wanted && !b.Forwarded
	})
}

// -pop-leg-delivery=datagram: the leg never asks for carriers.
func TestLegDatagramDelivery(t *testing.T) {
	p := newUplinkPoP(t, poplink.Config{PopName: "p"}, func(o *Options) {
		o.Relay.PopLegDelivery = config.PopLegDatagram
	})
	id, pub := p.publish(t)
	defer pub.Close()
	p.reserve(t, id, poplink.ReservedOK)
	p.l.send(poplink.Want{ID: id})
	p.nextLeg(t)
	if row := p.waitRow(t, id, func(b BroadcastUplink, _ bool) bool { return b.Forwarded }); row.Carrier {
		t.Fatalf("datagram delivery row %+v", row)
	}
}

// Item 27: the budget admits while the forwarded streams are under
// maxStreams and the forwarded rate plus the candidate's is at or under
// maxMbps.
func TestAdmitBudget(t *testing.T) {
	c := New(Options{Log: discard()})
	u := c.up
	c.setState(func(s *LinkState) {
		s.Config = &poplink.Config{Uplink: pops.Budget{MaxMbps: 10, MaxStreams: 2}}
	})
	now := time.Now()
	mbps := func(m float64) rateRing {
		var r rateRing
		r.add(now, 0)
		r.add(now.Add(time.Second), uint64(m*1e6/8))
		return r
	}
	up := &upBroadcast{id: "A", leg: func() {}, rate: mbps(6)}
	u.bcasts["A"] = up
	if got := u.admitLocked(&upBroadcast{rate: mbps(4)}); got != "" {
		t.Fatalf("6+4 of 10 Mbps refused: %q", got)
	}
	if got := u.admitLocked(&upBroadcast{rate: mbps(5)}); got != poplink.RefuseBudget {
		t.Fatalf("6+5 of 10 Mbps admitted: %q", got)
	}
	u.bcasts["B"] = &upBroadcast{id: "B", leg: func() {}}
	if got := u.admitLocked(&upBroadcast{}); got != poplink.RefuseBudget {
		t.Fatalf("a third stream of 2 admitted: %q", got)
	}
	// Defaults with no config: home's defaults.
	none := New(Options{Log: discard()})
	if got := none.up.budget(); got != (pops.Budget{MaxMbps: pops.DefaultUplinkMbps, MaxStreams: pops.DefaultUplinkStreams}) {
		t.Fatalf("default budget %+v", got)
	}
}

func TestRateRing(t *testing.T) {
	var r rateRing
	t0 := time.Unix(1000, 0)
	if r.bytesPerSec() != 0 {
		t.Fatal("empty ring has a rate")
	}
	r.add(t0, 100)
	if r.bytesPerSec() != 0 {
		t.Fatal("one sample has a rate")
	}
	r.add(t0.Add(2*time.Second), 300)
	if got := r.bytesPerSec(); got != 100 {
		t.Fatalf("rate = %v, want 100", got)
	}
	// Samples older than the window are cut, keeping one at its edge.
	for i := 3; i <= 20; i++ {
		r.add(t0.Add(time.Duration(i)*time.Second), uint64(300+(i-2)*1000))
	}
	if first := r.samples[0].at; t0.Add(20*time.Second).Sub(first) > rateWindow {
		t.Fatalf("window spans %v", t0.Add(20*time.Second).Sub(first))
	}
	if got := r.bytesPerSec(); got != 1000 {
		t.Fatalf("windowed rate = %v, want 1000", got)
	}
	// A counter that went backwards (a new publisher session) reads 0.
	r.add(t0.Add(21*time.Second), 5)
	if got := r.bytesPerSec(); got != 0 {
		t.Fatalf("rate after a reset = %v", got)
	}
	// Two samples at the same instant: no rate.
	var same rateRing
	same.add(t0, 0)
	same.add(t0, 10)
	if same.bytesPerSec() != 0 {
		t.Fatal("a zero-width window has a rate")
	}
}

// onReserved and onWant ignore records of the wrong type.
func TestUplinkIgnoresForeignRecords(t *testing.T) {
	c := New(Options{Log: discard()})
	c.up.onReserved(&poplink.Want{ID: "ABC234"})
	c.up.onWant(&poplink.Reserved{ID: "ABC234"})
	if len(c.up.bcasts) != 0 {
		t.Fatal("a foreign record changed the uplink")
	}
	// A tick with no registry does nothing.
	c.up.tick(time.Now())
}
