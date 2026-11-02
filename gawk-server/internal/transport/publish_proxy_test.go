package transport

// R33 WG3 (docs/83 D4, §8 row WG3): the trusted publish proxy. With the
// token, Gawk-Publisher-Ip drives the rate limit (exemptions included), the
// IP ban (pre- and post-upgrade) and the admin publisher-IP fields, and the
// Origin check is skipped; without it the header is ignored; a missing or
// unparsable header with a valid token is a 400, never the peer address; and
// no event carries the IP.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const proxyToken = "gw-proxy-token"

// proxyReq is connectReq from peer, presenting bearer (when non-empty) and
// the given Gawk-Publisher-Ip values.
func proxyReq(target, peer, bearer string, ips ...string) *http.Request {
	req := connectReq(target, nil)
	req.RemoteAddr = peer
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, ip := range ips {
		req.Header.Add(PublisherIPHeader, ip)
	}
	return req
}

func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// The gateway peer's own exemptions never apply to the publisher it names:
// from loopback, and from inside trustedCidrs, a proxied publisher is
// rate-limited on its own address — and a second publisher behind the same
// gateway has a bucket of its own. The log names the publisher, not the
// gateway.
func TestPublishProxyRateLimitsTheProxiedIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer string
		cfg  config.Config
	}{
		{"gateway on loopback", "127.0.0.1:50000", config.Config{}},
		{"gateway inside trustedCidrs", "10.42.1.7:50000", config.Config{TrustedCIDRs: mustCIDRs(t, "10.42.0.0/16")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.PublishProxyToken = proxyToken
			cfg.ConnRateLimit, cfg.ConnBurstLimit = 0.001, 2
			var logs syncBuffer
			r := hub.NewRegistry(discardLog, hub.Options{})
			sm := metrics.NewServerMetrics(prometheus.NewRegistry())
			srv := New(cfg, r, nil, slog.New(slog.NewTextHandler(&logs, nil)), sm)

			status := func(ip string) int {
				w := httptest.NewRecorder()
				srv.handlePublish(w, proxyReq("https://relay/publish", tc.peer, proxyToken, ip))
				return w.Code
			}
			for i := range 2 {
				if got := status("203.0.113.7"); got == http.StatusTooManyRequests {
					t.Fatalf("attempt %d within the burst was rate limited", i+1)
				}
			}
			if got := status("203.0.113.7"); got != http.StatusTooManyRequests {
				t.Fatalf("third attempt from 203.0.113.7 = %d, want 429: the gateway's exemption leaked to its publisher", got)
			}
			if got := status("198.51.100.9"); got == http.StatusTooManyRequests {
				t.Error("another publisher behind the same gateway shares the first one's bucket")
			}
			if got := sm.RateLimitedCount(); got != 1 {
				t.Errorf("rate_limited = %v, want 1", got)
			}
			if out := logs.String(); !strings.Contains(out, "remote=203.0.113.7") {
				t.Errorf("the rate-limit warning does not name the proxied publisher:\n%s", out)
			}
		})
	}
}

// The exemptions are decided on the proxied address too: a publisher whose
// own address is inside trustedCidrs is exempt, wherever the gateway sits.
func TestPublishProxyExemptionsFollowTheProxiedIP(t *testing.T) {
	srv, sm, _ := newOutcomeServer(t, config.Config{
		PublishProxyToken: proxyToken, ConnRateLimit: 0.001, ConnBurstLimit: 1,
		TrustedCIDRs: mustCIDRs(t, "192.168.1.0/24"),
	}, hub.Options{})
	for range 3 {
		w := httptest.NewRecorder()
		srv.handlePublish(w, proxyReq("https://relay/publish", "203.0.113.50:40000", proxyToken, "192.168.1.20"))
		if w.Code == http.StatusTooManyRequests {
			t.Fatal("a proxied publisher inside trustedCidrs was rate limited")
		}
	}
	if got := sm.RateLimitedCount(); got != 0 {
		t.Errorf("rate_limited = %v, want 0", got)
	}
}

// ::ffff:203.0.113.7 and 203.0.113.7 are one client: one bucket, one ban.
func TestPublishProxyCanonicalisesTheHeader(t *testing.T) {
	t.Run("one bucket", func(t *testing.T) {
		srv, _, _ := newOutcomeServer(t, config.Config{
			PublishProxyToken: proxyToken, ConnRateLimit: 0.001, ConnBurstLimit: 1,
		}, hub.Options{})
		w := httptest.NewRecorder()
		srv.handlePublish(w, proxyReq("https://relay/publish", "127.0.0.1:50000", proxyToken, "::ffff:203.0.113.7"))
		if w.Code == http.StatusTooManyRequests {
			t.Fatal("the first attempt was rate limited")
		}
		w = httptest.NewRecorder()
		srv.handlePublish(w, proxyReq("https://relay/publish", "127.0.0.1:50000", proxyToken, "203.0.113.7"))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("203.0.113.7 after ::ffff:203.0.113.7 = %d, want 429 (one bucket)", w.Code)
		}
	})
	t.Run("one ban", func(t *testing.T) {
		for _, header := range []string{"203.0.113.7", "::ffff:203.0.113.7"} {
			for _, target := range []string{"https://relay/publish", "https://relay/publish?mint=1"} {
				srv, sm, _ := newOutcomeServer(t, config.Config{PublishProxyToken: proxyToken}, hub.Options{})
				srv.SetModeration(banSet(t, ipBan("203.0.113.7/32", "abusive host")))
				w := httptest.NewRecorder()
				srv.handlePublish(w, proxyReq(target, "127.0.0.1:50000", proxyToken, header))
				if w.Code != http.StatusUnavailableForLegalReasons {
					t.Errorf("%s with header %s = %d, want 451", target, header, w.Code)
				}
				if got := sm.ConnectionCount("publish", metrics.OutcomeBanned); got != 1 {
					t.Errorf("publish/banned = %v, want 1", got)
				}
			}
		}
	})
}

// The ban is the proxied publisher's, never the gateway's: a banned gateway
// address does not block a clean publisher behind it, and on a claim the
// header address is what the ban sees too.
func TestPublishProxyBanKeysOnTheProxiedIP(t *testing.T) {
	srv, _, _ := newOutcomeServer(t, config.Config{PublishProxyToken: proxyToken}, hub.Options{})
	srv.SetModeration(banSet(t, ipBan("10.42.1.7/32", "gateway banned by mistake"), ipBan("203.0.113.0/24", "abuser")))

	w := httptest.NewRecorder()
	srv.handlePublish(w, proxyReq("https://relay/publish", "10.42.1.7:50000", proxyToken, "198.51.100.9"))
	if w.Code == http.StatusUnavailableForLegalReasons {
		t.Error("a clean publisher was refused for its gateway's address")
	}
	token := srv.resume.mint(bannedTestID)
	claim := proxyReq(fmt.Sprintf("https://relay/publish/%s?resume=%x", bannedTestID, token), "10.42.1.7:50000", proxyToken, "203.0.113.9")
	claim.SetPathValue("id", bannedTestID)
	w = httptest.NewRecorder()
	srv.handlePublish(w, claim)
	if w.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("a claim from a banned proxied IP = %d, want 451", w.Code)
	}
}

// With a valid token, a missing, unparsable, ported, unspecified or repeated
// header is a 400 — never a fallback to the peer address, which would hand
// the gateway's trustedCidrs/loopback exemption back to its publishers.
func TestPublishProxyBadHeaderIs400(t *testing.T) {
	for _, tc := range []struct {
		name string
		ips  []string
	}{
		{"missing", nil},
		{"empty", []string{""}},
		{"not an address", []string{"obs.example"}},
		{"with a port", []string{"203.0.113.7:4000"}},
		{"a CIDR", []string{"203.0.113.0/24"}},
		{"a list", []string{"203.0.113.7, 198.51.100.9"}},
		{"unspecified", []string{"::"}},
		{"repeated", []string{"203.0.113.7", "198.51.100.9"}},
	} {
		for _, target := range []string{"https://relay/publish", "https://relay/publish?mint=1", "https://relay/publish/" + bannedTestID} {
			t.Run(tc.name+" "+target, func(t *testing.T) {
				srv, sm, r := newOutcomeServer(t, config.Config{PublishProxyToken: proxyToken}, hub.Options{})
				req := proxyReq(target, "127.0.0.1:50000", proxyToken, tc.ips...)
				if strings.Contains(target, bannedTestID) {
					req.SetPathValue("id", bannedTestID)
				}
				w := httptest.NewRecorder()
				srv.handlePublish(w, req)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", w.Code)
				}
				if got := sm.ConnectionCount("publish", metrics.OutcomeBadRequest); got != 1 {
					t.Errorf("publish/bad_request = %v, want 1", got)
				}
				if len(r.Stats().Broadcasts) != 0 {
					t.Error("a refused proxy request created a broadcast")
				}
			})
		}
	}
}

// Without the token the header means nothing: not with no proxy token
// configured, not with a wrong bearer, not with no Authorization at all, and
// not on any route but publish. The peer address decides, as it always has.
func TestPublishProxyHeaderIgnoredWithoutTheToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		bearer     string
	}{
		{"no proxy configured", "", proxyToken},
		{"wrong bearer", proxyToken, "not-the-token"},
		{"no Authorization", proxyToken, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The header names a banned address; the peer is clean.
			srv, _, _ := newOutcomeServer(t, config.Config{
				PublishProxyToken: tc.configured, ConnRateLimit: 0.001, ConnBurstLimit: 1,
			}, hub.Options{})
			srv.SetModeration(banSet(t, ipBan("203.0.113.0/24", "abuser")))
			w := httptest.NewRecorder()
			srv.handlePublish(w, proxyReq("https://relay/publish", "198.51.100.20:40000", tc.bearer, "203.0.113.7"))
			if w.Code == http.StatusUnavailableForLegalReasons || w.Code == http.StatusBadRequest {
				t.Fatalf("status = %d: the header was honoured without the token", w.Code)
			}
			// ...and the limiter keys on the peer: a second attempt from the
			// same peer naming another address shares its bucket.
			w = httptest.NewRecorder()
			srv.handlePublish(w, proxyReq("https://relay/publish", "198.51.100.20:40000", tc.bearer, "192.0.2.1"))
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("second attempt from the same peer = %d, want 429 (peer bucket)", w.Code)
			}
		})
	}

	t.Run("subscribe route", func(t *testing.T) {
		srv, _, _ := newOutcomeServer(t, config.Config{
			PublishProxyToken: proxyToken, ConnRateLimit: 0.001, ConnBurstLimit: 1,
		}, hub.Options{})
		for i, ip := range []string{"203.0.113.7", "198.51.100.9"} {
			req := proxyReq("https://relay/subscribe/"+bannedTestID, "127.0.0.2:40000", proxyToken, ip)
			req.SetPathValue("id", bannedTestID)
			srv.testHookRateLimitLoopback.Store(true)
			w := httptest.NewRecorder()
			srv.handleSubscribe(w, req)
			if i == 1 && w.Code != http.StatusTooManyRequests {
				t.Fatalf("second subscribe from one peer = %d, want 429: the proxy token was honoured off the publish routes", w.Code)
			}
		}
	})
}

// dialHeaders dials once with extra request headers.
func dialHeaders(t *testing.T, ctx context.Context, url string, bs busServer, hdr http.Header) (*http.Response, *webtransport.Session, error) {
	t.Helper()
	d := webtransport.Transport{
		TLSClientConfig: bs.tls,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	t.Cleanup(func() { d.Close() })
	return d.Dial(ctx, url, hdr)
}

func proxyHeaders(token, ip string) http.Header {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	if ip != "" {
		h.Set(PublisherIPHeader, ip)
	}
	return h
}

// The proxied publisher end to end: the admin portal's publisher-IP field
// (PublisherRemote) records the header address, canonical, not the gateway
// peer's; the Origin allow-list is skipped for the token on publish routes
// only; and no event the hub emits — the bus's and the webhooks' only
// source — carries either address.
func TestPublishProxyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{
		PublishProxyToken: proxyToken,
		AllowedOrigins:    []string{"https://gawk.example"},
	})
	// Run the production origin shape: no loopback bypass.
	bs.srv.testHookOriginCheckLoopback.Store(true)

	publishURL := fmt.Sprintf("https://127.0.0.1:%d/publish", bs.port)

	// No token, no listed Origin: refused, as before R33.
	if rsp, sess, err := dialHeaders(t, ctx, publishURL, bs, proxyHeaders("", "")); err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("an unlisted-origin publish without the proxy token was accepted")
	} else if rsp != nil && rsp.StatusCode == http.StatusOK {
		t.Fatalf("unexpected 200 for an unlisted origin")
	}
	// A wrong token is no exemption either.
	if _, sess, err := dialHeaders(t, ctx, publishURL, bs, proxyHeaders("wrong", "203.0.113.7")); err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("a wrong proxy token was exempted from the Origin allow-list")
	}
	// The token is no exemption on subscribe.
	subURL := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", bs.port, bannedTestID)
	if _, sess, err := dialHeaders(t, ctx, subURL, bs, proxyHeaders(proxyToken, "203.0.113.7")); err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("the proxy token exempted a subscribe from the Origin allow-list")
	}

	// The gateway: token + header, no Origin at all.
	_, pub, err := dialHeaders(t, ctx, publishURL, bs, proxyHeaders(proxyToken, "::ffff:203.0.113.7"))
	if err != nil {
		t.Fatalf("proxied publish refused: %v", err)
	}
	id, token := readPublisherHandshake(t, ctx, pub)
	waitFor(t, 5*time.Second, func() bool { _, ok := bs.srv.PublisherRemote(id); return ok }, "publisher tracked")
	if got, _ := bs.srv.PublisherRemote(id); got.String() != "203.0.113.7" {
		t.Errorf("PublisherRemote = %v, want the proxied 203.0.113.7 (canonical), not the gateway's address", got)
	}
	pub.CloseWithError(0, "")

	// The claim path too: the reclaim through the gateway records the header.
	_, pub, err = dialHeaders(t, ctx, fmt.Sprintf("%s/%s?resume=%s", publishURL, id, token), bs, proxyHeaders(proxyToken, "198.51.100.9"))
	if err != nil {
		t.Fatalf("proxied reclaim refused: %v", err)
	}
	readPublisherHandshake(t, ctx, pub)
	waitFor(t, 5*time.Second, func() bool {
		got, ok := bs.srv.PublisherRemote(id)
		return ok && got.String() == "198.51.100.9"
	}, "the reclaim's proxied address recorded")
	pub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool { _, ok := bs.srv.PublisherRemote(id); return !ok }, "publisher untracked")

	evs := bs.bus.all()
	if len(evs) == 0 {
		t.Fatal("no events recorded: the assertion below would be vacuous")
	}
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %s: %v", ev.Type, err)
		}
		for _, ip := range []string{"203.0.113.7", "198.51.100.9", "127.0.0.1"} {
			if strings.Contains(string(b), ip) {
				t.Errorf("event %s carries the publisher address %s: %s", ev.Type, ip, b)
			}
		}
	}
}

// The post-upgrade ban re-check (the TOCTOU window) keys on the proxied
// address too: a ban on it landing during the upgrade closes the session
// with 4006, though the gateway peer is clean.
func TestPublishProxyPostUpgradeBanRecheckUsesTheProxiedIP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{PublishProxyToken: proxyToken})
	bans := moderation.NewSet()
	bs.srv.SetModeration(bans)
	hook := func(string) {
		if err := bans.Upsert(ipBan("203.0.113.7/32", "landed during the upgrade")); err != nil {
			t.Errorf("Upsert: %v", err)
		}
	}
	bs.srv.testHookPostUpgradePublish.Store(&hook)

	_, sess, err := dialHeaders(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", bs.port), bs, proxyHeaders(proxyToken, "203.0.113.7"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sess.CloseWithError(0, "")
	acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
	defer acceptCancel()
	for {
		if _, err = sess.AcceptUniStream(acceptCtx); err != nil {
			break
		}
	}
	var se *webtransport.SessionError
	if !errors.As(err, &se) || se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeTerminatedByOperator) {
		t.Fatalf("session ended with %v, want close 4006", err)
	}
}

// The gateway mints through the same proxy shape (D3 + D4 together): the
// mint-only gates see the proxied address.
func TestPublishProxyMintOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bs := startBusServer(t, ctx, config.Config{PublishProxyToken: proxyToken})
	_, sess, err := dialHeaders(t, ctx, mintOnlyURL(bs.port, ""), bs, proxyHeaders(proxyToken, "203.0.113.7"))
	if err != nil {
		t.Fatalf("proxied mint-only refused: %v", err)
	}
	res := readMintOnly(t, ctx, sess)
	if res.id == "" || res.closeCode != webtransport.SessionErrorCode(wire.CloseCodePublishMinted) {
		t.Fatalf("proxied mint-only: id %q close %d", res.id, res.closeCode)
	}
}

// bearer parsing: scheme case-insensitive, credential trimmed, anything
// else no bearer at all.
func TestBearerParsing(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"", "", false},
	} {
		r := httptest.NewRequest(http.MethodConnect, "https://relay/publish", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		got, ok := bearer(r)
		if got != tc.want || ok != tc.ok {
			t.Errorf("bearer(%q) = %q, %v; want %q, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
