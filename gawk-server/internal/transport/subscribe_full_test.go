package transport

// A viewer refused for capacity is told so (BUGS.md "Streamer offline card
// is misleading when the relay rejects the join", fixed 2026-11-06). A
// refused CONNECT's HTTP status never reaches a browser, so a pre-upgrade
// 429 read exactly like "no such broadcast" and the viewer showed
// "Streamer offline" for a live stream. The relay now upgrades a capacity
// refusal and closes it with a code per reason, noticed in-band (docs/59):
// 4018 BroadcastFull for the broadcast's own limit, 4019 RelayFull for the
// relay's. "No such broadcast" stays a pre-upgrade 404, and a stripe leg's
// refusal stays a pre-upgrade 429 (the primary's fallback handles it, and a
// leg reads no streams).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func TestNoticedCloseCodeCapacityRefusals(t *testing.T) {
	for _, code := range []uint32{wire.CloseCodeBroadcastFull, wire.CloseCodeRelayFull} {
		if !noticedCloseCode(code) {
			t.Errorf("%d is not noticed — Chrome would never read it", code)
		}
	}
}

func TestSubscribeAtBroadcastLimitClosesWithBroadcastFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 1)

	_, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	_ = dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	// The second viewer's dial succeeds: the refusal comes as a close.
	refused := dialSubscriber(t, ctx, port, id, clientTLS)
	n, nAt, code, cAt := drainUntilClosed(t, ctx, refused)
	assertNoticedClose(t, "refused viewer", wire.CloseCodeBroadcastFull, n, nAt, code, cAt)
	if got := r.Stats().Totals.Subscribers; got != 1 {
		t.Errorf("subscribers = %d after a refusal, want 1", got)
	}
}

func TestSubscribeAtRelayLimitClosesWithRelayFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:      15,
		MaxTotalSubscribers: 1,
		MaxIdleTimeout:      30 * time.Second,
		KeepAlivePeriod:     10 * time.Second,
		BroadcastGrace:      5 * time.Minute,
	})

	_, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	_ = dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	refused := dialSubscriber(t, ctx, port, id, clientTLS)
	n, nAt, code, cAt := drainUntilClosed(t, ctx, refused)
	assertNoticedClose(t, "refused viewer", wire.CloseCodeRelayFull, n, nAt, code, cAt)
}

// The race: the broadcast had room at the pre-upgrade check and filled up
// before this session registered. Same code as the pre-upgrade refusal,
// noticed too — it used to be a bare CloseWithError(429).
func TestSubscribeLostRaceToCapacityClosesWithBroadcastFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  1,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)

	_, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	hook := func(string) { _, _ = r.Subscribe(id, &fakeMinimalConn{}) }
	srv.testHookPostUpgradeSubscribe.Store(&hook)

	refused := dialSubscriber(t, ctx, port, id, clientTLS)
	n, nAt, code, cAt := drainUntilClosed(t, ctx, refused)
	assertNoticedClose(t, "raced viewer", wire.CloseCodeBroadcastFull, n, nAt, code, cAt)
}

// "No such broadcast" stays the cheapest answer: a pre-upgrade 404 with no
// session, which the viewer's hedged "Streamer offline" copy is right for.
func TestSubscribeUnknownBroadcastStays404PreUpgrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 15)

	url := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, "ZZZZZZ")
	rsp, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		sess.CloseWithError(0, "")
		t.Fatal("subscribe to an unknown broadcast upgraded, want a pre-upgrade 404")
	}
	if rsp == nil || rsp.StatusCode != http.StatusNotFound {
		t.Fatalf("response = %v (err %v), want status 404", rsp, err)
	}
}

// A stripe leg at a full broadcast is still refused pre-upgrade: the viewer
// reads no streams on a leg, and a refused leg just leaves it unstriped.
func TestStripeLegAtCapacityStays429PreUpgrade(t *testing.T) {
	srv, sm, r := newOutcomeServer(t, config.Config{StripedDelivery: true}, hub.Options{MaxSubscribers: 1})
	id, pub, err := r.StartPublish("")
	if err != nil {
		t.Fatalf("StartPublish: %v", err)
	}
	defer pub.Close()
	if _, err := r.Subscribe(id, &fakeMinimalConn{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	w := httptest.NewRecorder()
	srv.handleSubscribe(w, connectReq("https://relay/subscribe/"+id+"?stripe=2&leg=1&owner=aabbccdd00112233", map[string]string{"id": id}))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := sm.ConnectionCount("subscribe", metrics.OutcomeLimitRejected); got != 1 {
		t.Errorf("subscribe/limit_rejected = %v, want 1", got)
	}
}
