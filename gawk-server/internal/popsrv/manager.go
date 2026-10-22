package popsrv

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// ErrNotHolder: SendToLink on a pod that does not hold the PoP's link.
var ErrNotHolder = errors.New("popsrv: this pod does not hold the PoP's link")

// LinkSession is a link session as the home side sees it.
type LinkSession = poplink.Session

// ReservationRenewer is the slice of the reservation store the link needs:
// the link-home pod renews a PoP's dormant reservations every LeaseRenew
// (docs/76 item 14). W2-cluster's popsrv.Reservations satisfies it; nil
// renews nothing.
type ReservationRenewer interface {
	RenewAll(ctx context.Context, pop string) error
}

// Options wires a Manager.
type Options struct {
	// Pod and PodAddr name this pod in status.lease (POD_NAME, POD_IP:port).
	Pod, PodAddr string
	Source       popsrc.Source
	Status       popsrc.StatusWriter
	// Coord is nil in single-pod mode.
	Coord    *cluster.Coordinator
	Registry *hub.Registry
	// Reservations renews the linked PoPs' reservations; nil skips it.
	Reservations ReservationRenewer
	// HomeOrigin is the home app origin config tells a PoP
	// (https://host[:port], no path).
	HomeOrigin string
	// HomeRooms reports home runs -rooms (item 16); ScreeningNotice mirrors
	// the R40 notice (item 32).
	HomeRooms, ScreeningNotice bool
	// ClusterPSK authenticates pod-to-pod /internal/pop dials (W3).
	ClusterPSK string
	Now        func() time.Time
	Log        *slog.Logger

	// LeaseRenew overrides poplink.LeaseRenew (tests).
	LeaseRenew time.Duration
	// HelloTimeout bounds the wait for a PoP's first record (default 10s).
	HelloTimeout time.Duration
	// ByeGrace is how long a Bye gets to reach the PoP before the 4006
	// close (default 1s; the PoP closes first once it has read it).
	ByeGrace time.Duration
}

// LinkedPop is one PoP with a live link somewhere in the fleet, as the
// offer matcher (W3-offers) reads it.
type LinkedPop struct {
	Name, UID, DisplayName string
	PublicAddrs            []netip.Prefix
	LanAddr                string
	PinnedHashes           []string
	NamedHost              string
	AutoSwitch             bool
}

// Manager is the home side of the PoP links on one pod.
type Manager struct {
	opts    Options
	log     *slog.Logger
	limiter *failLimiter

	mu    sync.Mutex
	links map[string]*link // by Pop name: the links THIS pod serves

	hmu      sync.RWMutex
	handlers map[string][]func(pop string, rec any)

	unsubscribe func()

	// offers is this pod's offer matcher (NewOffers), for the admin rows'
	// OffersSent; nil when none is wired.
	offers atomic.Pointer[Offers]
}

// New builds a Manager. It subscribes to the Pop source so a Re-pair,
// revoke or end date ends a live link at once.
func New(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.LeaseRenew <= 0 {
		o.LeaseRenew = poplink.LeaseRenew
	}
	if o.HelloTimeout <= 0 {
		o.HelloTimeout = 10 * time.Second
	}
	if o.ByeGrace <= 0 {
		o.ByeGrace = time.Second
	}
	m := &Manager{
		opts:     o,
		log:      o.Log.With("component", "pops"),
		limiter:  newFailLimiter(o.Now),
		links:    map[string]*link{},
		handlers: map[string][]func(string, any){},
	}
	if o.Source != nil {
		m.unsubscribe = o.Source.Subscribe(func(name string) {
			if l := m.local(name); l != nil {
				go l.recheck()
			}
		})
	}
	return m
}

// Close stops following the source. Live links end with their sessions.
func (m *Manager) Close() {
	if m.unsubscribe != nil {
		m.unsubscribe()
	}
}

// Source is the Pop source the manager reads.
func (m *Manager) Source() popsrc.Source { return m.opts.Source }

// OnLinkRecord registers fn for every record of type t (a poplink.T*
// name) arriving on a link this pod holds. Handlers run on the link's
// reader goroutine, in order: they must not block (hand slow work off).
// Registering is the extension point every Wave 3 subsystem uses.
func (m *Manager) OnLinkRecord(t string, fn func(pop string, rec any)) {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	m.handlers[t] = append(m.handlers[t], fn)
}

func (m *Manager) dispatch(pop, t string, rec any) {
	m.hmu.RLock()
	hs := m.handlers[t]
	m.hmu.RUnlock()
	for _, fn := range hs {
		fn(pop, rec)
	}
}

// SendToLink writes rec on the PoP's control stream. Only the link-home
// pod can: elsewhere it is ErrNotHolder (W3 forwards via /internal/pop).
func (m *Manager) SendToLink(pop string, rec any) error {
	l := m.local(pop)
	if l == nil {
		return ErrNotHolder
	}
	return l.write(rec)
}

// LinkHolder reports which pod holds the PoP's link: this pod's own links
// first, then status.lease as the source last saw it.
func (m *Manager) LinkHolder(pop string) (addr string, gen int64, ok bool) {
	if l := m.local(pop); l != nil {
		return m.opts.PodAddr, l.gen, true
	}
	if m.opts.Source == nil {
		return "", 0, false
	}
	p, found := m.opts.Source.Get(pop)
	if !found || p.Status.Lease == nil || p.Status.Lease.Holder == "" {
		return "", 0, false
	}
	return p.Status.Lease.Addr, p.Status.Lease.Generation, true
}

// Linked lists the PoPs linked anywhere in the fleet: this pod's links from
// memory, the others from status.link as the source last saw it.
func (m *Manager) Linked() []LinkedPop {
	m.mu.Lock()
	locals := make([]*link, 0, len(m.links))
	for _, l := range m.links {
		locals = append(locals, l)
	}
	m.mu.Unlock()
	var out []LinkedPop
	seen := map[string]bool{}
	for _, l := range locals {
		lp, ok := l.linkedPop()
		if ok {
			out = append(out, lp)
			seen[lp.Name] = true
		}
	}
	if m.opts.Source != nil {
		for _, p := range m.opts.Source.List() {
			if seen[p.Name] || p.Status.Link == nil || !p.Status.Link.Connected ||
				p.Status.Lease == nil || p.Status.Lease.Holder == "" || p.Spec.Revoked {
				continue
			}
			lp := LinkedPop{
				Name: p.Name, UID: string(p.UID), DisplayName: p.Spec.DisplayName,
				PublicAddrs:  parsePrefixes(p.Status.Link.PublicAddrs),
				LanAddr:      p.Status.Link.LanAddr,
				PinnedHashes: slices.Clone(p.Status.Link.PinnedHashes),
				AutoSwitch:   p.AutoSwitch(),
			}
			// The named host only once a certificate was issued for it:
			// status.tls.host is assigned before issuance, and an offer
			// must not send guests to a name the PoP cannot serve yet.
			if p.Status.TLS != nil && p.Status.TLS.Certificate != "" {
				lp.NamedHost = p.Status.TLS.Host
			}
			out = append(out, lp)
		}
	}
	slices.SortFunc(out, func(a, b LinkedPop) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out
}

func (m *Manager) local(name string) *link {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[name]
}

// register installs l as this pod's link for its PoP and returns the one it
// replaced (a PoP that relinked to the same pod).
func (m *Manager) register(l *link) *link {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.links[l.name]
	m.links[l.name] = l
	return old
}

func (m *Manager) unregister(l *link) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.links[l.name] == l {
		delete(m.links, l.name)
	}
}

// pop reads a Pop from the source.
func (m *Manager) pop(name string) (*pops.Pop, bool) {
	if m.opts.Source == nil {
		return nil, false
	}
	return m.opts.Source.Get(name)
}

func (m *Manager) now() time.Time { return m.opts.Now() }

// config builds the Config record for p.
func (m *Manager) config(p *pops.Pop) poplink.Config {
	c := poplink.Config{
		PopName:         p.Name,
		UID:             string(p.UID),
		DisplayName:     p.Spec.DisplayName,
		HomeOrigin:      m.opts.HomeOrigin,
		Uplink:          p.EffectiveUplink(),
		Downlink:        p.Spec.Downlink,
		MaxBroadcasts:   p.EffectiveLimits().MaxBroadcasts,
		AutoSwitch:      p.AutoSwitch(),
		ScreeningNotice: m.opts.ScreeningNotice,
		HomeRooms:       m.opts.HomeRooms,
	}
	if p.Spec.Wall != nil {
		c.WallCode = p.Spec.Wall.RoomCode
	}
	if p.Status.TLS != nil {
		c.NamedHost = p.Status.TLS.Host
	}
	if p.Spec.NotAfter != nil {
		t := p.Spec.NotAfter.Time
		c.NotAfter = &t
	}
	return c
}

// background is a context for the writes a link makes after its session
// ended (the lease release), bounded so a dead API server cannot hang it.
func background() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
