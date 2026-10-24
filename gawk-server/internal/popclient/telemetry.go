package popclient

// The PoP's side of R77 (docs/78): telemetry identities minted by home
// (D1, D5), and the PoP's relay-side view sent home (D7).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const (
	// telemetryPoolSize is how many tokens a pool asks for at a time, and
	// telemetryPoolLow the level below which it asks again (D5): a session
	// takes one from the pool without a round trip home.
	telemetryPoolSize = 4
	telemetryPoolLow  = 2
	// telemetryRetry is how soon a refused pool asks again: a new party
	// broadcast's reservation, or a downlink viewer's leg slot, reaches the
	// link-home pod a moment after the session starts, and a scope refusal
	// in that moment is not the answer (docs/78 D5).
	telemetryRetry = 500 * time.Millisecond
	// telemetryAskExpiry is how long a mint request may go unanswered before
	// the pool asks again: home does not always answer (the link dropped
	// after the request was written, or home failed), and an answer that
	// never comes must not stop the pool for good (review of #563).
	telemetryAskExpiry = 4 * telemetryRetry
	// telemetryPoolIdle drops a pool nobody has drawn from in this long.
	telemetryPoolIdle = 10 * time.Minute
	// telemetryObsBudget keeps a TelemetryObs record well under
	// poplink.MaxRecordBytes.
	telemetryObsBudget = 48 << 10
)

type telemetryPool struct {
	broadcastKey []byte
	tokens       [][]byte
	asked        bool // a TelemetryMint is outstanding
	askedAt      time.Time
	refused      bool // home refused the last one
	refusedAt    time.Time
	used         time.Time
	ready        chan struct{} // closed and replaced when tokens or a refusal arrive
}

type telemetry struct {
	c *Client
	// send writes a record home: c.Send, or a test's.
	send  func(rec any) error
	mu    sync.Mutex
	pools map[string]*telemetryPool
}

func newTelemetry(c *Client) *telemetry {
	t := &telemetry{c: c, send: c.Send, pools: map[string]*telemetryPool{}}
	c.OnRecord(poplink.TTelemetryTokens, func(rec any) {
		if r, ok := rec.(*poplink.TelemetryTokens); ok {
			t.tokens(r)
		}
	})
	c.OnRecord(poplink.TTelemetryRefused, func(rec any) {
		if r, ok := rec.(*poplink.TelemetryRefused); ok {
			t.refused(r)
		}
	})
	return t
}

func poolKey(id string, role wire.TelemetryRole) string { return id + "/" + string(role) }

// TelemetryHello implements transport.PopTelemetry (docs/78 D5).
func (c *Client) TelemetryHello(ctx context.Context, id string, role wire.TelemetryRole) (wire.TelemetryHello, string, bool) {
	cfg := c.State().Config
	if cfg == nil || cfg.Telemetry == nil {
		return wire.TelemetryHello{}, "", false
	}
	tc := *cfg.Telemetry
	tok, bk, ok := c.tel.take(ctx, id, role)
	if !ok {
		return wire.TelemetryHello{}, "", false
	}
	return wire.TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: max(tc.ReportIntervalMs, 250),
		Token:            tok,
		BroadcastKey:     bk,
	}, tc.AdvertiseURL, true
}

// take hands out one token for (id, role), asking home for more when the
// pool runs low, and waits for them until ctx is done when it is empty.
func (t *telemetry) take(ctx context.Context, id string, role wire.TelemetryRole) ([]byte, []byte, bool) {
	key := poolKey(id, role)
	for {
		t.mu.Lock()
		t.sweepLocked()
		p := t.pools[key]
		if p == nil {
			p = &telemetryPool{ready: make(chan struct{})}
			t.pools[key] = p
		}
		p.used = time.Now()
		outstanding := p.asked && time.Since(p.askedAt) < telemetryAskExpiry
		if len(p.tokens) < telemetryPoolLow && !outstanding && (!p.refused || time.Since(p.refusedAt) >= telemetryRetry) {
			p.asked = true
			p.askedAt = time.Now()
			p.refused = false
			if err := t.send(poplink.TelemetryMint{ID: id, Role: string(role), N: telemetryPoolSize}); err != nil {
				p.asked = false
				if errors.Is(err, ErrNotLinked) && len(p.tokens) == 0 {
					t.mu.Unlock()
					return nil, nil, false
				}
			}
		}
		if len(p.tokens) > 0 {
			tok := p.tokens[0]
			p.tokens = p.tokens[1:]
			bk := p.broadcastKey
			t.mu.Unlock()
			return tok, bk, true
		}
		ready := p.ready
		t.mu.Unlock()
		retry := time.NewTimer(telemetryRetry)
		select {
		case <-ctx.Done():
			retry.Stop()
			return nil, nil, false
		case <-ready:
		case <-retry.C:
		}
		retry.Stop()
	}
}

func (t *telemetry) tokens(r *poplink.TelemetryTokens) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pools[poolKey(r.ID, wire.TelemetryRole(r.Role))]
	if p == nil {
		return // nobody asked, or the pool went idle
	}
	// Any answer settles the request, a malformed one too: the pool asks
	// again rather than waiting on an answer that has come.
	p.asked = false
	if bk, err := hex.DecodeString(r.BroadcastKey); err == nil && len(bk) == wire.TelemetryBroadcastKeySize {
		for _, s := range r.Tokens {
			if b, err := hex.DecodeString(s); err == nil && len(b) == wire.TelemetrySessionTokenSize {
				p.tokens = append(p.tokens, b)
			}
		}
		p.broadcastKey = bk
	}
	close(p.ready)
	p.ready = make(chan struct{})
}

func (t *telemetry) refused(r *poplink.TelemetryRefused) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pools[poolKey(r.ID, wire.TelemetryRole(r.Role))]
	if p == nil {
		return
	}
	p.asked = false
	p.refused = true
	p.refusedAt = time.Now()
	close(p.ready)
	p.ready = make(chan struct{})
	t.c.log.Debug("home refused telemetry tokens", "role", r.Role, "reason", r.Reason)
}

func (t *telemetry) sweepLocked() {
	now := time.Now()
	for k, p := range t.pools {
		if now.Sub(p.used) > telemetryPoolIdle {
			delete(t.pools, k)
		}
	}
}

// sendObservations sends home this PoP's relay-side view of every broadcast
// it holds (docs/78 D7), while home collects telemetry: each broadcast's
// /statusz entry by raw ID, its subscriber details split over parts so each
// record stays under poplink.MaxRecordBytes.
func (c *Client) sendObservations() {
	if cfg := c.State().Config; cfg == nil || cfg.Telemetry == nil || c.opts.Registry == nil {
		return
	}
	for id, st := range c.opts.Registry.BroadcastStatsByRawID() {
		subs := st.SubscriberDetails
		st.SubscriberDetails = nil
		bb, err := json.Marshal(st)
		if err != nil {
			continue
		}
		rec := poplink.TelemetryObs{ID: id, Broadcast: bb}
		size := len(bb)
		for _, sub := range subs {
			sb, err := json.Marshal(sub)
			if err != nil {
				continue
			}
			if size+len(sb) > telemetryObsBudget && len(rec.Subscribers) > 0 {
				if c.Send(rec) != nil {
					return
				}
				rec = poplink.TelemetryObs{ID: id, Part: rec.Part + 1}
				size = 0
			}
			rec.Subscribers = append(rec.Subscribers, sb)
			size += len(sb)
		}
		if c.Send(rec) != nil {
			return
		}
	}
}
