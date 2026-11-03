package transport

// The trusted publish proxy (R33, docs/83 D4). gawk-whip-gateway publishes
// for OBS users, so every one of its CONNECTs arrives from the gateway pod.
// Keyed on that peer, one abusive stream key would get the gateway IP-banned
// and the per-IP rate limit would throttle every OBS user at once; and the
// gateway, a pod inside the trustedCidrs the chart tells operators to set,
// would skip the rate limit entirely. So a CONNECT /publish… that presents
// -publish-proxy-token as `Authorization: Bearer` names its client in
// Gawk-Publisher-Ip, and every per-IP decision uses that address instead of
// the peer's.
//
// A non-matching bearer is not refused: the request is simply treated as an
// ordinary, unproxied publisher keyed on its peer address, and its
// Gawk-Publisher-Ip is ignored. Browsers cannot set request headers on a
// WebTransport CONNECT at all (the W3C constructor takes no headers), and
// gawk's native broadcasters never send Authorization, so nothing legitimate
// presents a wrong one — but refusing would turn an unrelated client header
// into an outage, and ignoring it gives the presenter exactly what it had
// without the header, never more.

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// PublisherIPHeader names the proxied publisher's address on a trusted
// publish proxy's CONNECT (docs/83 D4): one IP literal, v4 or v6, no port.
const PublisherIPHeader = "Gawk-Publisher-Ip"

// requestSource is the client a publish CONNECT is attributed to for every
// per-IP decision: the rate limit and its exemptions, the IP ban, the
// publisher-IP bookkeeping and the log "remote" field.
type requestSource struct {
	// addr is the canonical client address: the peer's, or with a valid
	// proxy token the Gawk-Publisher-Ip one. Invalid only when an unproxied
	// RemoteAddr does not parse (every caller treats that as "no IP").
	addr netip.Addr
	// remote is what logs name the client by: RemoteAddr unproxied, the
	// proxied address otherwise.
	remote string
	// proxied is set when the request presented the valid proxy token.
	proxied bool
	// strayBearer is set when the request carried an Authorization: Bearer
	// that is not the proxy token (ignored; logged once it passes the rate
	// limit, so a flood of them cannot flood the log).
	strayBearer bool
}

// peerSource attributes a request to its QUIC peer: every route but a
// proxied publish.
func peerSource(r *http.Request) requestSource {
	return requestSource{addr: remoteIP(r.RemoteAddr), remote: r.RemoteAddr}
}

// validProxyToken reports whether r presents -publish-proxy-token as a
// bearer credential. Constant-time; false whenever no token is configured.
func (s *Server) validProxyToken(r *http.Request) bool {
	want := s.cfg.PublishProxyToken
	if want == "" {
		return false
	}
	got, ok := bearer(r)
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// bearer returns the credential of an `Authorization: Bearer` header.
func bearer(r *http.Request) (string, bool) {
	scheme, cred, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(cred), true
}

// publishSource resolves who a publish CONNECT is from. ok false means a
// request with the valid proxy token named no usable publisher address — a
// missing, repeated or unparsable Gawk-Publisher-Ip — which the caller
// answers 400: falling back to the peer would hand a buggy or older gateway
// the trustedCidrs exemption D4 exists to close.
func (s *Server) publishSource(r *http.Request) (requestSource, bool) {
	if !s.validProxyToken(r) {
		src := peerSource(r)
		_, src.strayBearer = bearer(r)
		return src, true
	}
	addr, ok := parsePublisherIP(r.Header.Values(PublisherIPHeader))
	if !ok {
		return requestSource{proxied: true, remote: r.RemoteAddr}, false
	}
	return requestSource{addr: addr, remote: addr.String(), proxied: true}, true
}

// parsePublisherIP parses exactly one Gawk-Publisher-Ip value into a
// canonical address — zone stripped and v4-mapped v6 unmapped, as
// remoteIP does, so ::ffff:203.0.113.7 and 203.0.113.7 share one rate-limit
// bucket and one ban. Repeated headers are refused rather than picked from.
func parsePublisherIP(values []string) (netip.Addr, bool) {
	if len(values) != 1 {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil {
		return netip.Addr{}, false
	}
	addr = moderation.CanonicalAddr(addr)
	if addr.IsUnspecified() {
		return netip.Addr{}, false
	}
	return addr, true
}

// rateLimitedSource is rateLimited for a publish source: with a proxied
// source the loopback exemption, the trustedCidrs exemption and the bucket
// all key on the proxied address, never on the gateway peer's.
func (s *Server) rateLimitedSource(r *http.Request, src requestSource) bool {
	if !src.proxied {
		return s.rateLimited(r)
	}
	if s.limiter == nil {
		return false
	}
	if src.addr.IsLoopback() && !s.testHookRateLimitLoopback.Load() {
		return false
	}
	if addrInCIDRs(src.addr, s.cfg.TrustedCIDRs) {
		return false
	}
	if s.limiter.Allow(src.addr.String()) {
		return false
	}
	s.metrics.RateLimited()
	s.log.Warn("connection rate limited",
		"remote", src.remote, "proxy", r.RemoteAddr, "origin", r.Header.Get("Origin"), "path", r.URL.Path)
	return true
}

// addrInCIDRs reports whether a canonical address falls in any CIDR.
func addrInCIDRs(addr netip.Addr, cidrs []*net.IPNet) bool {
	if !addr.IsValid() {
		return false
	}
	ip := net.IP(addr.AsSlice())
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// isPublishPath reports whether path is a publish route (/publish or
// /publish/{id}) — the only routes a proxy token means anything on.
func isPublishPath(path string) bool {
	return path == "/publish" || strings.HasPrefix(path, "/publish/")
}
