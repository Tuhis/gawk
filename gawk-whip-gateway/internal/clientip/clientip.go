// Package clientip decides which address a POST /whip came from (docs/83
// D4, "Where the gateway gets the client IP").
//
// POST /whip arrives through the public Ingress, so RemoteAddr is the
// ingress controller for every OBS user; X-Forwarded-For is
// attacker-controlled unless the peer is a proxy we trust. So the header is
// read only when RemoteAddr is inside the trusted-proxy list, and walked from
// the right, past trusted hops, to the first untrusted entry. Every address
// is canonicalised with gawk-server's moderation.CanonicalAddr (the relay's
// own rule) before any CIDR match and before it becomes Gawk-Publisher-Ip,
// so ::ffff:203.0.113.7 and 203.0.113.7 are one rate-limit bucket and one
// ban target.
package clientip

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// Outcome says how the address was found; the gateway turns the two
// anomalies into metrics.
type Outcome int

const (
	// FromRemoteAddr: no trusted proxy involved; the peer is the client.
	FromRemoteAddr Outcome = iota
	// FromForwarded: the peer is a trusted proxy and the walk found the
	// client in X-Forwarded-For.
	FromForwarded
	// UntrustedForwarded: the peer is outside a non-empty trusted list but
	// sent X-Forwarded-For anyway. The header is ignored and RemoteAddr
	// used, but it is the silent misconfiguration D4 wants counted (e.g. a
	// dual-stack ingress whose v6 address the list misses).
	UntrustedForwarded
)

// ErrUnknown means the peer is a trusted proxy but the header is missing or
// the walk stopped on an unparsable entry: the POST gets 400
// client_ip_unknown, so a misconfigured Ingress fails loudly instead of
// collapsing every user into one shared bucket.
var ErrUnknown = errors.New("clientip: client address unknown")

// Resolver holds the trusted-proxy list.
type Resolver struct {
	trusted []netip.Prefix
}

// New parses the trusted-proxy CIDRs (bare addresses become /32 or /128).
// Prefixes are canonicalised like addresses, so a v4-mapped entry matches
// the plain v4 address it means.
func New(cidrs []string) (*Resolver, error) {
	r := &Resolver{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := moderation.ParsePrefix(c)
		if err != nil {
			return nil, err
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// Trusting reports whether any proxy is trusted.
func (r *Resolver) Trusting() bool { return len(r.trusted) > 0 }

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve returns the client address for req.
func (r *Resolver) Resolve(req *http.Request) (netip.Addr, Outcome, error) {
	peer, err := parseEntry(req.RemoteAddr)
	if err != nil {
		// net/http always fills RemoteAddr as host:port; anything else is a
		// test harness or a broken listener, never a client choice.
		return netip.Addr{}, FromRemoteAddr, ErrUnknown
	}
	lines := req.Header.Values("X-Forwarded-For")
	if len(r.trusted) == 0 || !r.isTrusted(peer) {
		if len(r.trusted) > 0 && len(lines) > 0 {
			return peer, UntrustedForwarded, nil
		}
		return peer, FromRemoteAddr, nil
	}
	// Every header line, joined in order, comma-split and trimmed.
	var entries []string
	for _, l := range lines {
		for _, e := range strings.Split(l, ",") {
			entries = append(entries, strings.TrimSpace(e))
		}
	}
	if len(entries) == 0 {
		return netip.Addr{}, FromForwarded, ErrUnknown
	}
	var leftmost netip.Addr
	for i := len(entries) - 1; i >= 0; i-- {
		a, err := parseEntry(entries[i])
		if err != nil {
			// Stop, never skip: skipping would let an attacker shift the
			// walk with junk.
			return netip.Addr{}, FromForwarded, ErrUnknown
		}
		if !r.isTrusted(a) {
			return a, FromForwarded, nil
		}
		leftmost = a
	}
	// Every entry trusted: the left-most. Only a client inside the trusted
	// range can produce such a chain (the proxy appends what it saw), so it
	// can only name another in-range address.
	return leftmost, FromForwarded, nil
}

// parseEntry parses one address: bare v4/v6, or v4:port, or [v6]:port.
func parseEntry(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, ErrUnknown
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return moderation.CanonicalAddr(a), nil
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return netip.Addr{}, ErrUnknown
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, ErrUnknown
	}
	return moderation.CanonicalAddr(a), nil
}
