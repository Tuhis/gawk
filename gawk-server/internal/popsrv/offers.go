package popsrv

// offers.go: LocalRelayOffer (0x19) matching and the lr=1 demand deferral
// (docs/76 items 21, 41-42).

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// OfferValidity is how long a client may treat an offer as fresh (the
// offer's validity field). An hour comfortably outlives a page load or a
// probe, and is far inside the rolling certificate's overlap, so both
// pinned hashes still verify for as long as the offer is valid.
const OfferValidity = time.Hour

// PopRelayPort is the port a PoP relay serves WebTransport on (and, for a
// named host, the HTTPS page: docs/76 item 39) unless the link reports
// another with its LAN address (gawk-relay -addr).
const PopRelayPort = poplink.DefaultRelayPort

// OffersOptions configures an Offers.
type OffersOptions struct {
	// Defer is -pop-offer-defer: how long a matching lr=1 session's demand
	// is held (docs/76 item 21). Zero turns the hold off.
	Defer time.Duration
	// Prom registers the offer counters; nil registers nothing.
	Prom prometheus.Registerer
	Log  *slog.Logger
}

// Offers is the home fleet's transport.Offerer: it compares a client
// session's remote address with every linked PoP's public addresses
// (IPv4 exactly, IPv6 on the /64, as RecordAddr keys them) and, on a match
// with a PoP that auto-switches guests, builds the LocalRelayOffer. It also
// answers W3-uplink's demand deferrer (Defer). Every pod runs one; the
// match reads Manager.Linked, which covers the links other pods hold
// through status.link.
type Offers struct {
	m       *Manager
	defer_  time.Duration
	log     *slog.Logger
	metrics *offerMetrics

	mu   sync.Mutex
	sent map[string]int // offers made, by PoP name
}

var _ transport.Offerer = (*Offers)(nil)

// NewOffers builds the matcher over m and hangs it on m, so m's admin rows
// carry this pod's offer counts.
func NewOffers(m *Manager, o OffersOptions) *Offers {
	if o.Log == nil {
		o.Log = m.log
	}
	of := &Offers{
		m:       m,
		defer_:  max(o.Defer, 0),
		log:     o.Log.With("component", "pop-offers"),
		metrics: newOfferMetrics(o.Prom),
		sent:    map[string]int{},
	}
	m.offers.Store(of)
	return of
}

// Offer implements transport.Offerer. lr does not change the answer: a
// client that will not follow an offer still gets one (it may show the
// chip, docs/76 item 43); lr only gates Defer.
func (o *Offers) Offer(remote netip.Addr, lr bool) (wire.LocalRelayOffer, bool) {
	lp, offer, ok := o.match(remote)
	if !ok {
		return wire.LocalRelayOffer{}, false
	}
	o.mu.Lock()
	o.sent[lp.Name]++
	o.mu.Unlock()
	o.metrics.offer()
	return offer, true
}

// Defer is the demand deferrer W3-uplink's pop_demand.go consults: how
// long to hold an outside viewer's demand for a reserved broadcast before
// asking the PoP to forward it. Only a session that announced lr=1 AND
// gets an offer is held, for -pop-offer-defer; everyone else is served at
// once (docs/76 item 21).
func (o *Offers) Defer(remote netip.Addr, lr bool) time.Duration {
	if !lr || o.defer_ <= 0 {
		return 0
	}
	if _, _, ok := o.match(remote); !ok {
		return 0
	}
	o.metrics.deferred()
	return o.defer_
}

// Sent is the number of offers this pod made for the PoP.
func (o *Offers) Sent(pop string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sent[pop]
}

// match finds the first linked PoP (by name) whose public addresses hold
// remote and that has somewhere to send a guest.
func (o *Offers) match(remote netip.Addr) (LinkedPop, wire.LocalRelayOffer, bool) {
	if !remote.IsValid() {
		return LinkedPop{}, wire.LocalRelayOffer{}, false
	}
	remote = remote.Unmap()
	for _, lp := range o.m.Linked() {
		if !lp.AutoSwitch || lp.UID == "" || !containsAddr(lp.PublicAddrs, remote) {
			continue
		}
		offer, ok := buildOffer(lp)
		if !ok {
			continue
		}
		if _, err := wire.AppendLocalRelayOffer(nil, offer); err != nil {
			o.log.Warn("pop offer skipped: encode failed", "pop", lp.Name, "err", err)
			continue
		}
		return lp, offer, true
	}
	return LinkedPop{}, wire.LocalRelayOffer{}, false
}

func containsAddr(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// buildOffer is the offer for lp: the named URL when the PoP holds a
// certificate for it (publicly trusted, so unpinned), then the IP-literal
// URL with the pinned hashes (docs/76 item 42). No candidate, no offer.
func buildOffer(lp LinkedPop) (wire.LocalRelayOffer, bool) {
	offer := wire.LocalRelayOffer{
		Handle:   lp.UID,
		Name:     truncateUTF8(lp.DisplayName, wire.MaxRelayIdentityNameLen),
		Validity: uint32(OfferValidity / time.Second),
	}
	if lp.NamedHost != "" {
		// The named host is the same relay listener, so the same port.
		offer.Candidates = append(offer.Candidates, wire.LocalRelayCandidate{
			URL: "https://" + net.JoinHostPort(lp.NamedHost, strconv.Itoa(relayPortOf(lp.LanAddr))),
		})
	}
	if hp, ok := lanHostPort(lp.LanAddr); ok {
		c := wire.LocalRelayCandidate{URL: "https://" + hp}
		for _, h := range lp.PinnedHashes {
			if len(c.Hashes) == wire.MaxLocalRelayHashes {
				break
			}
			if b, ok := decodeHash(h); ok {
				c.Hashes = append(c.Hashes, b)
			}
		}
		offer.Candidates = append(offer.Candidates, c)
	}
	return offer, len(offer.Candidates) > 0
}

// lanHostPort turns a reported LAN address (an IP literal, optionally with
// a port) into a URL host:port; anything else is not dialable by IP.
func lanHostPort(s string) (string, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil && ap.Port() != 0 {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return "", false
	}
	return netip.AddrPortFrom(a.Unmap(), PopRelayPort).String(), true
}

// relayPortOf is the port a reported LAN address carries, else
// PopRelayPort.
func relayPortOf(lan string) int {
	if ap, err := netip.ParseAddrPort(lan); err == nil && ap.Port() != 0 {
		return int(ap.Port())
	}
	return PopRelayPort
}

// decodeHash parses a hex SHA-256 certificate hash (tlsutil.CertHashHex).
func decodeHash(h string) ([32]byte, bool) {
	var out [32]byte
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != len(out) {
		return out, false
	}
	copy(out[:], b)
	return out, true
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// offerMetrics are the pod's offer counters. They carry no PoP label: a
// PoP's name is its default wall code, which R42 treats as joinable.
type offerMetrics struct {
	offers, defers prometheus.Counter
}

func newOfferMetrics(reg prometheus.Registerer) *offerMetrics {
	if reg == nil {
		return nil
	}
	m := &offerMetrics{
		offers: registerCounter(reg, prometheus.CounterOpts{
			Name: "gawk_pop_offers_total",
			Help: "LocalRelayOffers this pod made to clients on a linked PoP's network (R74).",
		}),
		defers: registerCounter(reg, prometheus.CounterOpts{
			Name: "gawk_pop_offer_defers_total",
			Help: "Outside-viewer demands this pod held for -pop-offer-defer because the client announced lr=1 and was offered a PoP (R74).",
		}),
	}
	return m
}

// registerCounter registers a counter, reusing one already registered
// under the same name (a re-wire on the same registry).
func registerCounter(reg prometheus.Registerer, opts prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(opts)
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(prometheus.Counter); ok {
				return existing
			}
		}
	}
	return c
}

func (m *offerMetrics) offer() {
	if m != nil {
		m.offers.Inc()
	}
}

func (m *offerMetrics) deferred() {
	if m != nil {
		m.defers.Inc()
	}
}
