package popclient

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The PoP's uplink (docs/76 §6.4-6.5, §6.8; PP8):
//
//   - Reserve before announce. A local /publish mint on a linked PoP waits
//     for home's reserved{id} before the broadcaster sees the ID
//     (transport's MintGate): ok makes the broadcast forwardable; conflict
//     and limit leave it local-only ("party-only") until it ends. Unlinked,
//     or with no answer in time, the mint is local and reserved on relink.
//   - A want from home is admitted when the broadcast is live here and the
//     budget allows: fewer forwarded streams than maxStreams, and the
//     forwarded broadcasts' 10-second ingress rate plus the candidate's at
//     or under maxMbps (item 27). Admission dials /pop/publish/{id} — the
//     leg — and a refusal is a refuse{budget} record.
//   - The leg is open exactly while the broadcast is wanted, live and
//     admitted (item 19): it closes when the broadcaster goes away and is
//     redialled when they return, or when it dies; home's 4013 PopNoDemand
//     ends the want.
//   - state records tell home each forwardable broadcast's live/away state;
//     release records give up the reservation of one that ended.
//
// Not a byte leaves for home without a want: the leg is the only uplink.

// Uplink timers.
const (
	// reserveTimeout bounds a mint's wait for reserved{id}: one WAN round
	// trip normally; past it the broadcast starts local-only and a late ok
	// still makes it forwardable (a home that predates reservations never
	// answers, and must not hold every mint for long).
	reserveTimeout = 2 * time.Second
	// uplinkTick paces the live/away scan and the rate sampling.
	uplinkTick = 500 * time.Millisecond
	// rateWindow is the admission rate's window (item 27).
	rateWindow = 10 * time.Second
	// legRetryMin and legRetryMax bound the redial backoff of a wanted leg.
	legRetryMin = 250 * time.Millisecond
	legRetryMax = 5 * time.Second
)

// Reservation states of a PoP broadcast.
const (
	// resPending: minted unlinked (or unanswered); reserved on relink.
	resPending = ""
	// resOK: reserved at home: forwardable.
	resOK = poplink.ReservedOK
	// resConflict and resLimit: local-only until the broadcast ends.
	resConflict = poplink.ReservedConflict
	resLimit    = poplink.ReservedLimit
)

// UplinkStats is the uplink as the console and home's stats record show it.
type UplinkStats struct {
	// Streams and Mbps are the legs up now and their ingress rate.
	Streams int
	Mbps    float64
	// Budget is the effective ceiling: home's, lowered by the host.
	Budget pops.Budget
	// Bytes counts every byte the legs wrote, ever.
	Bytes uint64
	// Legs counts leg sessions dialled, ever.
	Legs uint64
}

// BroadcastUplink is one local broadcast's uplink state.
type BroadcastUplink struct {
	ID string
	// Reservation is "ok", "conflict", "limit", or "" (not reserved yet).
	Reservation string
	Wanted      bool
	Forwarded   bool
	// Carrier is the delivery of the live leg.
	Carrier bool
}

type uplink struct {
	c *Client

	bytes atomic.Uint64
	legs  atomic.Uint64

	mu       sync.Mutex
	ctx      context.Context
	bcasts   map[string]*upBroadcast
	waiters  map[string][]chan string
	linked   bool
	stopSubs func()
}

type upBroadcast struct {
	id      string
	res     string
	wanted  bool
	live    bool
	sent    bool // a state record for the current live value went out
	carrier bool

	leg     context.CancelFunc
	legDone chan struct{}
	legUp   bool

	rate rateRing
}

func newUplink(c *Client) *uplink {
	u := &uplink{c: c, bcasts: map[string]*upBroadcast{}, waiters: map[string][]chan string{}}
	if c.opts.Server != nil {
		c.opts.Server.SetMintGate(u)
		c.opts.Server.AddPublishObserver(u)
	}
	c.OnRecord(poplink.TReserved, u.onReserved)
	c.OnRecord(poplink.TWant, u.onWant)
	u.stopSubs = c.Subscribe(u.onState)
	return u
}

// run is the uplink's loop: rate samples, live/away transitions, leg
// starts and stops. It ends with ctx, taking every leg with it.
func (u *uplink) run(ctx context.Context) {
	u.mu.Lock()
	u.ctx = ctx
	u.mu.Unlock()
	t := time.NewTicker(uplinkTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			u.stopAll()
			return
		case <-t.C:
		}
		u.tick(time.Now())
	}
}

// ReserveMint implements transport.MintGate.
func (u *uplink) ReserveMint(ctx context.Context, id string) (bool, error) {
	u.track(id)
	if !u.isLinked() {
		// Unlinked: local now, reserved on relink. A relay that was never
		// paired has no home to be local-only from.
		return u.c.hasCredential(), nil
	}
	ch := make(chan string, 1)
	u.mu.Lock()
	u.waiters[id] = append(u.waiters[id], ch)
	u.mu.Unlock()
	if err := u.c.Send(poplink.Reserve{IDs: []string{id}}); err != nil {
		u.dropWaiter(id, ch)
		return true, nil
	}
	t := time.NewTimer(reserveTimeout)
	defer t.Stop()
	select {
	case res := <-ch:
		return res != resOK, nil
	case <-ctx.Done():
	case <-t.C:
		u.c.log.Info("no reservation answer from home in time; the broadcast starts local-only", "broadcast_key", u.key(id))
	}
	u.dropWaiter(id, ch)
	return true, nil
}

// PublisherLive implements transport.PublishObserver: a reclaim (a
// broadcaster's resume, or a relay restart's unknown-ID create) of a
// broadcast this uplink does not know yet is reserved like an unlinked
// mint.
func (u *uplink) PublisherLive(_ *http.Request, id string) {
	u.mu.Lock()
	_, known := u.bcasts[id]
	u.mu.Unlock()
	if known {
		return
	}
	u.track(id)
	if u.isLinked() {
		if err := u.c.Send(poplink.Reserve{IDs: []string{id}}); err != nil {
			u.c.log.Debug("reserve not sent", "err", err)
		}
	}
}

func (u *uplink) track(id string) *upBroadcast {
	u.mu.Lock()
	defer u.mu.Unlock()
	b := u.bcasts[id]
	if b == nil {
		b = &upBroadcast{id: id}
		u.bcasts[id] = b
	}
	return b
}

func (u *uplink) dropWaiter(id string, ch chan string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	ws := u.waiters[id]
	if i := slices.Index(ws, ch); i >= 0 {
		ws = slices.Delete(ws, i, i+1)
	}
	if len(ws) == 0 {
		delete(u.waiters, id)
	} else {
		u.waiters[id] = ws
	}
}

func (u *uplink) isLinked() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.linked
}

// onState follows the link: on every (re)link, reserve what is pending,
// re-assert what is held (a single-pod home forgets reservations on a
// restart), and repeat each broadcast's state.
func (u *uplink) onState(st LinkState) {
	linked := st.Header == HeaderLinked
	u.mu.Lock()
	was := u.linked
	u.linked = linked
	var ids []string
	if linked && !was {
		for id, b := range u.bcasts {
			if b.res == resPending || b.res == resOK {
				ids = append(ids, id)
			}
			b.sent = false
		}
	}
	u.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	slices.Sort(ids)
	go func() {
		if err := u.c.Send(poplink.Reserve{IDs: ids}); err != nil {
			u.c.log.Debug("relink reserve not sent", "err", err)
		}
	}()
}

// onReserved applies home's answer for one ID.
func (u *uplink) onReserved(rec any) {
	r, ok := rec.(*poplink.Reserved)
	if !ok {
		return
	}
	u.mu.Lock()
	b := u.bcasts[r.ID]
	if b != nil {
		b.res = r.Result
		b.sent = false
		if r.Result != resOK {
			b.wanted = false
		}
	}
	ws := u.waiters[r.ID]
	delete(u.waiters, r.ID)
	u.mu.Unlock()
	for _, ch := range ws {
		ch <- r.Result
	}
	switch r.Result {
	case resLimit:
		u.c.log.Info("over this PoP's limit at home: the broadcast is party-only", "broadcast_key", u.key(r.ID))
	case resConflict:
		u.c.log.Info("broadcast ID taken at home: the broadcast is local-only", "broadcast_key", u.key(r.ID))
	}
	if b != nil && r.Result != resOK {
		u.stopLeg(b)
	}
}

// onWant admits or refuses one want.
func (u *uplink) onWant(rec any) {
	w, ok := rec.(*poplink.Want)
	if !ok {
		return
	}
	live, _, known := u.c.opts.Registry.BroadcastState(w.ID)
	u.mu.Lock()
	b := u.bcasts[w.ID]
	if !known || b == nil || b.res != resOK {
		u.mu.Unlock()
		u.refuse(w.ID, poplink.RefuseUnknown)
		return
	}
	if b.wanted {
		u.mu.Unlock()
		return // idempotent: one leg per broadcast
	}
	if live {
		if reason := u.admitLocked(b); reason != "" {
			u.mu.Unlock()
			u.refuse(w.ID, reason)
			return
		}
	}
	// Admitted; or not live, in which case the want is kept and the leg
	// dials when the broadcaster is back (item 19).
	b.wanted = true
	start := live && b.leg == nil
	if start {
		u.startLegLocked(b)
	}
	u.mu.Unlock()
	u.c.log.Info("outside demand for a party broadcast", "broadcast_key", u.key(w.ID), "live", live)
}

func (u *uplink) refuse(id, reason string) {
	if err := u.c.Send(poplink.Refuse{ID: id, Reason: reason}); err != nil {
		u.c.log.Debug("refuse not sent", "err", err)
	}
	if reason == poplink.RefuseBudget {
		u.c.log.Info("outside viewing refused: the uplink budget is full", "broadcast_key", u.key(id))
	}
}

// admitLocked is the budget check (item 27): "" admits, else the refusal.
func (u *uplink) admitLocked(cand *upBroadcast) string {
	budget := u.budget()
	streams := 0
	var rate float64
	for _, b := range u.bcasts {
		if b.leg != nil {
			streams++
			rate += b.rate.bytesPerSec()
		}
	}
	if budget.MaxStreams > 0 && streams >= budget.MaxStreams {
		return poplink.RefuseBudget
	}
	if budget.MaxMbps > 0 && (rate+cand.rate.bytesPerSec())*8/1e6 > float64(budget.MaxMbps) {
		return poplink.RefuseBudget
	}
	return ""
}

// budget is the effective uplink ceiling: home's (the Pop spec, defaulted),
// lowered by the host's -uplink-budget, never raised.
func (u *uplink) budget() pops.Budget {
	b := pops.Budget{MaxMbps: pops.DefaultUplinkMbps, MaxStreams: pops.DefaultUplinkStreams}
	if cfg := u.c.State().Config; cfg != nil {
		if cfg.Uplink.MaxMbps > 0 {
			b.MaxMbps = cfg.Uplink.MaxMbps
		}
		if cfg.Uplink.MaxStreams > 0 {
			b.MaxStreams = cfg.Uplink.MaxStreams
		}
	}
	host := u.c.hostUplink()
	if host.Mbps > 0 && host.Mbps < b.MaxMbps {
		b.MaxMbps = host.Mbps
	}
	if host.Streams > 0 && host.Streams < b.MaxStreams {
		b.MaxStreams = host.Streams
	}
	return b
}

// tick samples rates and reconciles every broadcast with its hub.
func (u *uplink) tick(now time.Time) {
	reg := u.c.opts.Registry
	if reg == nil {
		return
	}
	stats := reg.Stats()
	var states []poplink.State
	var released []string
	u.mu.Lock()
	for id, b := range u.bcasts {
		live, _, known := reg.BroadcastState(id)
		if !known {
			u.stopLegLocked(b)
			if b.res == resOK {
				released = append(released, id)
			}
			delete(u.bcasts, id)
			continue
		}
		if st, ok := stats.Broadcasts[reg.ObfuscateID(id)]; ok {
			b.rate.add(now, st.IngressDatagramBytes+st.KeyframeBytesIn)
		}
		if b.res == resOK && (live != b.live || !b.sent) && u.linked {
			states = append(states, poplink.State{ID: id, Live: live})
			b.sent = true
		}
		b.live = live
		switch {
		case b.leg != nil && !live:
			// The broadcaster went away: close the leg, keep the want.
			u.stopLegLocked(b)
		case b.leg == nil && live && b.wanted && b.res == resOK && u.linked:
			u.startLegLocked(b)
		}
	}
	u.mu.Unlock()
	for _, st := range states {
		if err := u.c.Send(st); err != nil {
			u.c.log.Debug("state not sent", "err", err)
		}
	}
	if len(released) > 0 {
		slices.Sort(released)
		if err := u.c.Send(poplink.Release{IDs: released}); err != nil {
			u.c.log.Debug("release not sent", "err", err)
		}
	}
}

// startLegLocked starts b's leg loop. Caller holds u.mu.
func (u *uplink) startLegLocked(b *upBroadcast) {
	if b.leg != nil || u.ctx == nil {
		return
	}
	ctx, cancel := context.WithCancel(u.ctx)
	b.leg = cancel
	b.legDone = make(chan struct{})
	go u.legLoop(ctx, b, b.legDone)
}

func (u *uplink) stopLeg(b *upBroadcast) {
	u.mu.Lock()
	u.stopLegLocked(b)
	u.mu.Unlock()
}

// stopLegLocked cancels b's leg loop without waiting for it.
func (u *uplink) stopLegLocked(b *upBroadcast) {
	if b.leg != nil {
		b.leg()
		b.leg = nil
		b.legUp = false
	}
}

func (u *uplink) stopAll() {
	u.mu.Lock()
	var done []chan struct{}
	for _, b := range u.bcasts {
		if b.leg != nil {
			done = append(done, b.legDone)
		}
		u.stopLegLocked(b)
	}
	u.mu.Unlock()
	for _, d := range done {
		<-d
	}
}

// legLoop holds b's leg: dial, serve, and redial while still wanted, until
// 4013, a refusal that needs a new reservation, or cancellation.
func (u *uplink) legLoop(ctx context.Context, b *upBroadcast, done chan struct{}) {
	defer close(done)
	delay := legRetryMin
	for ctx.Err() == nil {
		end, retry := u.legOnce(ctx, b)
		switch end {
		case legNoDemand:
			u.mu.Lock()
			b.wanted = false
			if b.legDone == done {
				b.leg, b.legUp = nil, false
			}
			u.mu.Unlock()
			u.c.log.Info("home has no more outside demand; the broadcast stays local", "broadcast_key", u.key(b.id))
			return
		case legNotReserved:
			// Home no longer holds the reservation (a revoke, or the
			// cluster janitor): reserve it again; the next want redials.
			u.mu.Lock()
			b.wanted, b.res = false, resPending
			if b.legDone == done {
				b.leg, b.legUp = nil, false
			}
			u.mu.Unlock()
			if err := u.c.Send(poplink.Reserve{IDs: []string{b.id}}); err != nil {
				u.c.log.Debug("re-reserve not sent", "err", err)
			}
			return
		case legTerminal:
			u.mu.Lock()
			b.wanted = false
			if b.legDone == done {
				b.leg, b.legUp = nil, false
			}
			u.mu.Unlock()
			return
		}
		if !retry {
			delay = legRetryMin
		}
		t := time.NewTimer(delay/2 + rand.N(delay))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(delay*2, legRetryMax)
	}
}

type legEnd uint8

const (
	legEnded legEnd = iota
	legNoDemand
	legNotReserved
	legTerminal
)

// legOnce is one leg session. retry reports a failure to connect (backoff
// grows) rather than a leg that ran and ended.
func (u *uplink) legOnce(ctx context.Context, b *upBroadcast) (end legEnd, retry bool) {
	u.c.mu.Lock()
	st := u.c.stored
	u.c.mu.Unlock()
	if st == nil {
		return legTerminal, false
	}
	q := url.Values{"proto": {poplink.ProtoQuery()}}
	if st.Pop != "" {
		q.Set("pop", st.Pop)
	}
	h := http.Header{"Origin": {poplink.Origin}, "Authorization": {"Bearer " + st.Credential}}
	target := "https://" + st.Endpoint + "/pop/publish/" + b.id + "?" + q.Encode()
	sess, rsp, err := u.c.dial(ctx, "udp", target, h)
	if err != nil {
		if rsp != nil {
			switch rsp.StatusCode {
			case http.StatusNotFound, http.StatusConflict:
				u.c.log.Info("home refused the leg: not reserved for this PoP", "broadcast_key", u.key(b.id), "status", rsp.StatusCode)
				return legNotReserved, false
			case http.StatusUnavailableForLegalReasons, http.StatusUnauthorized, http.StatusForbidden, http.StatusUpgradeRequired:
				u.c.log.Info("home refused the leg", "broadcast_key", u.key(b.id), "status", rsp.StatusCode)
				return legTerminal, false
			}
		}
		u.c.log.Debug("leg dial failed", "broadcast_key", u.key(b.id), "err", err)
		return legEnded, true
	}
	u.legs.Add(1)
	carrier := u.c.opts.Relay.PopLegDelivery != config.PopLegDatagram
	err = u.c.opts.Server.ServePopUplinkLeg(ctx, sess, b.id, transport.PopLegOptions{
		Carrier: carrier,
		Bytes:   &u.bytes,
		OnStart: func(carrier bool) {
			u.mu.Lock()
			b.legUp, b.carrier = true, carrier
			u.mu.Unlock()
			u.c.log.Info("uplink leg up", "broadcast_key", u.key(b.id), "carrier", carrier)
		},
	})
	u.mu.Lock()
	b.legUp = false
	u.mu.Unlock()
	u.c.log.Debug("uplink leg ended", "broadcast_key", u.key(b.id), "err", err)
	if code, ok := poplink.CloseCode(err); ok {
		switch code {
		case wire.CloseCodePopNoDemand:
			return legNoDemand, false
		case wire.CloseCodeTerminatedByOperator, wire.CloseCodeBroadcastEnded:
			return legTerminal, false
		}
	}
	if errors.Is(err, transport.ErrNotLegSession) {
		return legTerminal, false
	}
	return legEnded, false
}

// Stats is the uplink's use now.
func (u *uplink) stats() UplinkStats {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := UplinkStats{Budget: u.budget(), Bytes: u.bytes.Load(), Legs: u.legs.Load()}
	for _, b := range u.bcasts {
		if b.legUp {
			s.Streams++
			s.Mbps += b.rate.bytesPerSec() * 8 / 1e6
		}
	}
	return s
}

func (u *uplink) broadcasts() []BroadcastUplink {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]BroadcastUplink, 0, len(u.bcasts))
	for id, b := range u.bcasts {
		out = append(out, BroadcastUplink{ID: id, Reservation: b.res, Wanted: b.wanted, Forwarded: b.legUp, Carrier: b.carrier})
	}
	slices.SortFunc(out, func(a, b BroadcastUplink) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

func (u *uplink) key(id string) string {
	if u.c.opts.Registry == nil {
		return ""
	}
	return u.c.opts.Registry.ObfuscateID(id)
}

// UplinkStats is the uplink's use now: legs, rate, budget and the byte
// counter (zero until home first wants a broadcast).
func (c *Client) UplinkStats() UplinkStats { return c.up.stats() }

// UplinkBroadcasts lists every local broadcast the uplink tracks.
func (c *Client) UplinkBroadcasts() []BroadcastUplink { return c.up.broadcasts() }

// rateRing is a broadcast's ingress byte counter sampled over rateWindow.
type rateRing struct {
	samples []rateSample
}

type rateSample struct {
	at    time.Time
	bytes uint64
}

func (r *rateRing) add(now time.Time, total uint64) {
	r.samples = append(r.samples, rateSample{now, total})
	cut := 0
	for cut < len(r.samples)-1 && now.Sub(r.samples[cut+1].at) >= rateWindow {
		cut++
	}
	r.samples = r.samples[cut:]
}

// bytesPerSec is the rate over the window (0 with fewer than two samples).
func (r *rateRing) bytesPerSec() float64 {
	if len(r.samples) < 2 {
		return 0
	}
	first, last := r.samples[0], r.samples[len(r.samples)-1]
	dt := last.at.Sub(first.at).Seconds()
	if dt <= 0 || last.bytes < first.bytes {
		return 0
	}
	return float64(last.bytes-first.bytes) / dt
}
