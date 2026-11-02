package gateway

import (
	"net/http"
	"sync"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/publisher"
)

// WG5 client IP: "the chosen address is what reaches the rate limit and
// Gawk-Publisher-Ip", canonicalised (a v4-mapped entry is the same bucket);
// the per-IP POST limit answers 429 + Retry-After and records nothing.
func TestWG5ClientIPReachesRateLimitAndPublisherIPHeader(t *testing.T) {
	relay := newFakeRelay()
	var mu sync.Mutex
	var ips []string
	g := startGW(t, relay, func(o *Options) {
		o.TrustedProxyCIDRs = []string{"127.0.0.1/32"}
		o.POSTRate, o.POSTBurst = 0.001, 1
		o.Hooks.EngineConfig = func(c publisher.Config) {
			mu.Lock()
			ips = append(ips, c.Header.Get("Gawk-Publisher-Ip"))
			mu.Unlock()
		}
	})
	post := func(xff string) resp {
		_, offer := newClientPC(t, clientOpts{})
		return do(t, http.MethodPost, g.public.URL+"/whip", newKey(t).String(), "application/sdp", offer,
			http.Header{"X-Forwarded-For": {xff}})
	}
	if r := post("198.51.100.1, 203.0.113.7"); r.code != http.StatusCreated {
		t.Fatalf("first POST: %d", r.code)
	}
	mu.Lock()
	got := append([]string(nil), ips...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "203.0.113.7" {
		t.Fatalf("Gawk-Publisher-Ip %v, want the right-most untrusted entry 203.0.113.7", got)
	}
	r := post("::ffff:203.0.113.7")
	if r.code != http.StatusTooManyRequests || r.hdr.Get("Retry-After") == "" {
		t.Fatalf("same client (v4-mapped): %d Retry-After %q, want 429", r.code, r.hdr.Get("Retry-After"))
	}
	if r := post("203.0.113.8"); r.code != http.StatusCreated {
		t.Fatalf("another client: %d, want its own bucket", r.code)
	}
}

// WG5 client IP: "A header from an untrusted peer while the list is set
// increments the misconfiguration metric" (and is ignored); "With an empty
// trustedProxyCidrs a forged header is ignored and no request gets 400".
func TestWG5UntrustedForwardedForIsIgnoredAndCounted(t *testing.T) {
	relay := newFakeRelay()
	var ip string
	g := startGW(t, relay, func(o *Options) {
		o.TrustedProxyCIDRs = []string{"10.0.0.0/8"} // the test client isn't in it
		o.Hooks.EngineConfig = func(c publisher.Config) { ip = c.Header.Get("Gawk-Publisher-Ip") }
	})
	_, offer := newClientPC(t, clientOpts{})
	r := do(t, http.MethodPost, g.public.URL+"/whip", newKey(t).String(), "application/sdp", offer, http.Header{"X-Forwarded-For": {"203.0.113.9"}})
	if r.code != http.StatusCreated || ip != "127.0.0.1" {
		t.Fatalf("POST %d, publisher IP %q: want 201 from the peer address", r.code, ip)
	}
	if n := testutilCounter(g.srv.m.xff.WithLabelValues(xffUntrustedPeer)); n != 1 {
		t.Fatalf("misconfiguration metric %v, want 1", n)
	}

	open := startGW(t, relay, func(o *Options) {
		o.Hooks.EngineConfig = func(c publisher.Config) { ip = c.Header.Get("Gawk-Publisher-Ip") }
	})
	for _, xff := range []string{"junk", "203.0.113.9", ""} {
		_, offer := newClientPC(t, clientOpts{})
		h := http.Header{}
		if xff != "" {
			h.Set("X-Forwarded-For", xff)
		}
		r := do(t, http.MethodPost, open.public.URL+"/whip", newKey(t).String(), "application/sdp", offer, h)
		if r.code != http.StatusCreated || ip != "127.0.0.1" {
			t.Fatalf("empty trusted list, XFF %q: %d %q", xff, r.code, ip)
		}
	}
	if n := testutilCounter(open.srv.m.xff.WithLabelValues(xffUntrustedPeer)); n != 0 {
		t.Fatalf("empty list counted %v misconfigurations", n)
	}
}
