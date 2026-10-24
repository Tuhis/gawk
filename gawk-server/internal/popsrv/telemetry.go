package popsrv

// Telemetry for PoP sessions (R77, docs/78). The fleet key never leaves the
// fleet: the link-home pod mints a PoP's tokens (D1), over home's broadcast
// key, marked with the PoP's issuer key (D3), for the broadcasts the PoP
// really serves (D4); and it keeps the PoP's relay-side view for the
// telemetry service, on an ops-only route (D7).

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// TelemetryOptions configures StartTelemetry.
type TelemetryOptions struct {
	// Key is the fleet telemetry key (wire.TelemetryKeySize bytes).
	Key []byte
	// ReportIntervalMs and AdvertiseURL are what home's own hellos carry,
	// told to the PoP in config so its hellos match.
	ReportIntervalMs uint16
	AdvertiseURL     string
	// Reservations names the PoP a broadcast is reserved for.
	Reservations Reservations
	// ServesDownlink reports whether pop holds a downlink leg for id; nil
	// refuses every viewer token for a home broadcast.
	ServesDownlink func(pop, id string) bool
	// Rate and Burst are the per-PoP token bucket (defaults 10/s, 100).
	Rate  float64
	Burst int
	// ObsMaxAge drops a PoP observation not refreshed in this long
	// (default 45 s: three of the PoP's 10 s reports, and some).
	ObsMaxAge time.Duration
}

// Telemetry is the home side of D1–D7 on one pod.
type Telemetry struct {
	m    *Manager
	opts TelemetryOptions

	mu      sync.Mutex
	buckets map[string]*tokenBucket
	obs     map[string]map[string]*popObs // pop → raw ID → latest snapshot
	refused map[string]uint64             // reason → count
}

type popObs struct {
	at          time.Time
	broadcast   json.RawMessage
	subscribers []json.RawMessage
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// StartTelemetry turns on minting for this pod's links and the PoP
// observation store. With a key that is not wire.TelemetryKeySize bytes it
// returns nil and nothing changes: home collects no telemetry, so neither
// does a PoP.
func (m *Manager) StartTelemetry(o TelemetryOptions) *Telemetry {
	if len(o.Key) != wire.TelemetryKeySize {
		return nil
	}
	if o.Rate <= 0 {
		o.Rate = 10
	}
	if o.Burst <= 0 {
		o.Burst = 100
	}
	if o.ObsMaxAge <= 0 {
		o.ObsMaxAge = 45 * time.Second
	}
	t := &Telemetry{
		m:       m,
		opts:    o,
		buckets: map[string]*tokenBucket{},
		obs:     map[string]map[string]*popObs{},
		refused: map[string]uint64{},
	}
	m.telemetry.Store(t)
	m.OnLinkRecord(poplink.TTelemetryMint, func(pop string, rec any) {
		if r, ok := rec.(*poplink.TelemetryMint); ok {
			// Off the reader goroutine: minting reads the reservation cache
			// and writes to the link, and handlers must not block.
			go t.mint(pop, *r)
		}
	})
	m.OnLinkRecord(poplink.TTelemetryObs, func(pop string, rec any) {
		if r, ok := rec.(*poplink.TelemetryObs); ok {
			t.observe(pop, r)
		}
	})
	return t
}

// Telemetry is the pod's PoP telemetry, or nil while home collects none.
func (m *Manager) Telemetry() *Telemetry { return m.telemetry.Load() }

// config is the telemetry part of a PoP's config record.
func (t *Telemetry) config() *poplink.TelemetryConfig {
	return &poplink.TelemetryConfig{
		ReportIntervalMs: t.opts.ReportIntervalMs,
		AdvertiseURL:     t.opts.AdvertiseURL,
	}
}

// mint answers one TelemetryMint (D4).
func (t *Telemetry) mint(pop string, r poplink.TelemetryMint) {
	refuse := func(reason string) {
		t.mu.Lock()
		t.refused[reason]++
		t.mu.Unlock()
		_ = t.m.SendToLink(pop, poplink.TelemetryRefused{ID: r.ID, Role: r.Role, Reason: reason})
	}
	l := t.m.local(pop)
	if l == nil {
		return
	}
	role := wire.TelemetryRole(r.Role)
	if !wire.ValidTelemetryRole(role) || r.ID == "" || !t.inScope(pop, r.ID, role) {
		refuse(poplink.TelemetryRefusedScope)
		return
	}
	n := r.N
	if n <= 0 {
		n = 1
	}
	n = min(n, poplink.MaxTelemetryMint)
	if n = t.take(pop, n); n == 0 {
		refuse(poplink.TelemetryRefusedRate)
		return
	}
	bk, err := hex.DecodeString(t.m.opts.Registry.ObfuscateID(r.ID))
	if err != nil {
		return
	}
	issuer := wire.TelemetryIssuerKey(t.opts.Key, l.uid)
	now := t.m.now()
	out := poplink.TelemetryTokens{ID: r.ID, Role: r.Role, BroadcastKey: hex.EncodeToString(bk)}
	for range n {
		tok, err := wire.MintTelemetrySessionTokenForIssuer(t.opts.Key, bk, role, now, issuer)
		if err != nil {
			t.m.log.Warn("pop telemetry mint failed", "pop", pop, "err", err)
			return
		}
		out.Tokens = append(out.Tokens, hex.EncodeToString(tok))
	}
	if err := t.m.SendToLink(pop, out); err != nil {
		t.m.log.Debug("pop telemetry tokens not sent", "pop", pop, "err", err)
	}
}

// inScope is D4: a broadcaster token only for the PoP's own reservation; a
// viewer token for that or for a home broadcast the PoP pulls down.
func (t *Telemetry) inScope(pop, id string, role wire.TelemetryRole) bool {
	if t.opts.Reservations != nil {
		if owner, ok := t.opts.Reservations.Owner(id); ok && owner == pop {
			return true
		}
	}
	return role == wire.TelemetryRoleViewer && t.opts.ServesDownlink != nil && t.opts.ServesDownlink(pop, id)
}

// take removes up to n tokens from pop's bucket and returns how many it got.
func (t *Telemetry) take(pop string, n int) int {
	now := t.m.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[pop]
	if b == nil {
		b = &tokenBucket{tokens: float64(t.opts.Burst), last: now}
		t.buckets[pop] = b
	}
	b.tokens = min(float64(t.opts.Burst), b.tokens+now.Sub(b.last).Seconds()*t.opts.Rate)
	b.last = now
	got := min(n, int(b.tokens))
	b.tokens -= float64(got)
	return got
}

// observe stores one part of a PoP's broadcast snapshot (D7).
func (t *Telemetry) observe(pop string, r *poplink.TelemetryObs) {
	if r.ID == "" {
		return
	}
	now := t.m.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	byID := t.obs[pop]
	if byID == nil {
		byID = map[string]*popObs{}
		t.obs[pop] = byID
	}
	o := byID[r.ID]
	if r.Part == 0 || o == nil {
		o = &popObs{broadcast: r.Broadcast}
		byID[r.ID] = o
	}
	o.at = now
	o.subscribers = append(o.subscribers, r.Subscribers...)
}

// Refused is how many mint requests were refused, by reason.
func (t *Telemetry) Refused() map[string]uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]uint64, len(t.refused))
	for k, v := range t.refused {
		out[k] = v
	}
	return out
}

// PopsDoc is GET /internal/telemetry/pops's body: the linked PoPs' latest
// relay-side views, by issuer key, each broadcast by HOME's broadcast key.
// No raw ID and no wall code: an ops route, but the telemetry service is
// not a place for joinable codes either.
type PopsDoc struct {
	Pops map[string]PopObservations `json:"pops"`
}

// PopObservations is one PoP's latest view.
type PopObservations struct {
	DisplayName string                     `json:"displayName,omitempty"`
	AtMs        int64                      `json:"atMs"`
	Broadcasts  map[string]json.RawMessage `json:"broadcasts"`
}

// Doc assembles PopsDoc for the PoPs this pod holds links for; a link held
// elsewhere, or gone, contributes nothing.
func (t *Telemetry) Doc() PopsDoc {
	now := t.m.now()
	doc := PopsDoc{Pops: map[string]PopObservations{}}
	t.mu.Lock()
	defer t.mu.Unlock()
	for pop, byID := range t.obs {
		l := t.m.local(pop)
		if l == nil {
			delete(t.obs, pop)
			continue
		}
		l.mu.Lock()
		name := l.displayName
		l.mu.Unlock()
		po := PopObservations{DisplayName: name, Broadcasts: map[string]json.RawMessage{}}
		for id, o := range byID {
			if now.Sub(o.at) > t.opts.ObsMaxAge {
				delete(byID, id)
				continue
			}
			b, ok := withSubscribers(o.broadcast, o.subscribers)
			if !ok {
				continue
			}
			po.Broadcasts[t.m.opts.Registry.ObfuscateID(id)] = b
			po.AtMs = max(po.AtMs, o.at.UnixMilli())
		}
		if len(po.Broadcasts) > 0 {
			doc.Pops[hex.EncodeToString(wire.TelemetryIssuerKey(t.opts.Key, l.uid))] = po
		}
	}
	return doc
}

// withSubscribers rejoins a snapshot's parts into one /statusz-shaped entry.
func withSubscribers(broadcast json.RawMessage, subs []json.RawMessage) (json.RawMessage, bool) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(broadcast, &entry); err != nil || entry == nil {
		return nil, false
	}
	if subs == nil {
		subs = []json.RawMessage{}
	}
	sb, err := json.Marshal(subs)
	if err != nil {
		return nil, false
	}
	entry["subscriberDetails"] = sb
	out, err := json.Marshal(entry)
	return out, err == nil
}

// Handler serves GET /internal/telemetry/pops (D7). Registered on the ops
// listener only: /statusz is also public, this is not.
func (t *Telemetry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(t.Doc())
	})
}
