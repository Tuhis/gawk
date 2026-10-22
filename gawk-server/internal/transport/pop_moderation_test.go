// R74 PP10 (docs/76 §6.9) on the transport: a linked PoP mirrors home's
// screening notice in its own RelayIdentity (item 32), the ban observers a
// home's PoP forwarder chains on, and the /pop/* routes' exemption from IP
// bans (item 30).
package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// SetScreeningNotice(true) on a relay started with the notice off gives its
// publish sessions exactly one RelayIdentity with the screening bit and
// /echo the bit too; turning it off again restores the -screening-notice=off
// shape (no identity on publish, no notices byte on /echo).
func TestScreeningNoticeOverrideMirrorsHome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, noticeConfig(false), discardLog)

	srv.SetScreeningNotice(true)
	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	_, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	ident, n := identityFrom(t, append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...))
	if n != 1 || ident.Notices&wire.RelayNoticeScreening == 0 {
		t.Fatalf("publish with the override on: %d identities %+v, want exactly one with the screening bit", n, ident)
	}
	echo := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	if ident, n := identityFrom(t, collectUniMessages(t, echo, 1500*time.Millisecond)); n != 1 || ident.Notices&wire.RelayNoticeScreening == 0 {
		t.Fatalf("echo with the override on: %d identities %+v, want the screening bit", n, ident)
	}

	srv.SetScreeningNotice(false)
	pub2 := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	_, _, consumed = readPublisherHandshakeMsgs(t, ctx, pub2)
	if _, n := identityFrom(t, append(consumed, collectUniMessages(t, pub2, 1500*time.Millisecond)...)); n != 0 {
		t.Fatalf("publish with the override off: %d identities, want none", n)
	}
	echo2 := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	if ident, n := identityFrom(t, collectUniMessages(t, echo2, 1500*time.Millisecond)); n != 1 || ident.Notices != 0 {
		t.Fatalf("echo with the override off: %d identities %+v, want one with no notices", n, ident)
	}
}

// A relay's own -screening-notice stays on whatever home says: the override
// only ever adds the disclosure, never removes the host's.
func TestScreeningNoticeOverrideNeverClearsTheRelaysOwn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, noticeConfig(true), discardLog)
	srv.SetScreeningNotice(false)
	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	_, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	ident, n := identityFrom(t, append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...))
	if n != 1 || ident.Notices&wire.RelayNoticeScreening == 0 {
		t.Fatalf("%d identities %+v, want exactly one with the screening bit", n, ident)
	}
}

// Every record HandleBanAdded sees reaches the observers, expired and IP
// ones included (the observer filters), each observer once per record.
func TestBanObserversSeeEveryRecord(t *testing.T) {
	srv := New(noticeConfig(false), hub.NewRegistry(discardLog, hub.Options{MaxSubscribers: 2}), nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	var mu sync.Mutex
	var got []moderation.Record
	srv.AddBanObserver(func(r moderation.Record) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	})
	past := time.Now().Add(-time.Hour)
	recs := []moderation.Record{
		{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC234"}},
		{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC235"}, ExpiresAt: &past},
		{Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}},
	}
	for _, r := range recs {
		srv.HandleBanAdded(r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(recs) {
		t.Fatalf("observers saw %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		if got[i].Target != recs[i].Target {
			t.Errorf("record %d = %+v, want %+v", i, got[i].Target, recs[i].Target)
		}
	}
}

// An IP ban on the PoP's public address does not refuse its /pop/* dials
// and does not cut a live link (docs/76 item 30): the credential is the
// PoP's identity, and the venue's address carries every guest. The same
// ban does refuse an ordinary /publish from that address, so the test is
// not vacuous.
func TestPopRoutesIgnoreIPBans(t *testing.T) {
	port, clientTLS, srv, home := startPopHome(t, nil)
	bans := moderation.NewSet()
	ban := moderation.Record{Target: moderation.Target{Type: moderation.TargetIP, Value: "127.0.0.0/8"}}
	if err := bans.Upsert(ban); err != nil {
		t.Fatal(err)
	}
	srv.SetModeration(bans)
	if _, ok := bans.BannedIP(netip.MustParseAddr("127.0.0.1"), time.Now()); !ok {
		t.Fatal("the ban does not cover the test's address")
	}

	auth := http.Header{"Authorization": {"Bearer good"}}
	link, _, err := popDial(t, port, clientTLS, "/pop/link?proto=1", auth)
	if err != nil {
		t.Fatalf("/pop/link under an IP ban: %v", err)
	}
	<-home.served
	if _, _, err := popDial(t, port, clientTLS, "/pop/addr?proto=1", auth); err != nil {
		t.Fatalf("/pop/addr under an IP ban: %v", err)
	}
	// The leg routes (W3-uplink/-downlink fill them): whatever they answer,
	// it is never the ban's 451.
	for _, path := range []string{"/pop/publish/ABC234?proto=1", "/pop/subscribe/ABC234?proto=1"} {
		_, rsp, _ := popDial(t, port, clientTLS, path, auth)
		if rsp != nil && rsp.StatusCode == http.StatusUnavailableForLegalReasons {
			t.Errorf("%s answered 451 under an IP ban", path)
		}
	}

	// The ban event fires while the link is up: nothing ends it.
	srv.HandleBanAdded(ban)
	select {
	case <-link.Context().Done():
		t.Fatalf("an IP ban event cut the PoP link: %v", context.Cause(link.Context()))
	case <-time.After(500 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &webtransport.Transport{
		TLSClientConfig: clientTLS,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	defer d.Close()
	rsp, _, err := d.Dial(ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), http.Header{"Origin": {"https://gawk.example"}})
	if err == nil || rsp == nil || rsp.StatusCode != http.StatusUnavailableForLegalReasons {
		t.Fatalf("an ordinary /publish from the banned address: %v %+v, want 451", err, rsp)
	}
}

// The leg routes skip the per-IP limiter too (docs/76 item 30): whatever
// W3-uplink/-downlink's handlers answer, it is never the limiter's 429,
// however fast the PoP dials. TestPopRoutesSkipThePerIPLimiter covers
// /pop/link; /echo proves the limiter is live.
func TestPopLegRoutesSkipThePerIPLimiter(t *testing.T) {
	port, clientTLS, srv, _ := startPopHome(t, func(c *config.Config) { c.ConnRateLimit = 0.001; c.ConnBurstLimit = 1 })
	srv.testHookRateLimitLoopback.Store(true)
	auth := http.Header{"Authorization": {"Bearer good"}}
	for i := range 3 {
		for _, path := range []string{"/pop/publish/ABC234?proto=1", "/pop/subscribe/ABC234?proto=1", "/pop/addr?proto=1"} {
			_, rsp, _ := popDial(t, port, clientTLS, path, auth)
			if rsp != nil && rsp.StatusCode == http.StatusTooManyRequests {
				t.Errorf("dial %d of %s answered 429", i, path)
			}
		}
	}
	var limited bool
	for range 3 {
		if _, rsp, err := popDial(t, port, clientTLS, "/echo", nil); err != nil && rsp != nil && rsp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the limiter never refused /echo; the exemption test proves nothing")
	}
}
