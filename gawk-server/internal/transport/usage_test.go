package transport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// R59 (docs/61 D3): the mint path starts a broadcast and every claim
// continues one, so broadcasts per day can't be inflated by reconnects. The
// dial's client parameters become the labels.
func TestBroadcastsStartedCountsMintAsNewAndClaimAsResumed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(15), discardLog)
	sm := srv.metrics

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish?app=web&os=windows&browser=chromium", port), clientTLS)
	id, token := readPublisherHandshake(t, ctx, pub)
	pub.CloseWithError(0, "")

	resumed := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer resumed.CloseWithError(0, "")

	if got := sm.BroadcastsStartedCount(metrics.BroadcastNew, "web", "windows", "chromium"); got != 1 {
		t.Errorf("new{web,windows,chromium} = %v, want 1", got)
	}
	// The reclaim dial names no client: unknown, never blank.
	if got := sm.BroadcastsStartedCount(metrics.BroadcastResumed, "unknown", "unknown", "unknown"); got != 1 {
		t.Errorf("resumed{unknown...} = %v, want 1", got)
	}
	if got := sm.BroadcastsStartedCount(metrics.BroadcastNew, "unknown", "unknown", "unknown"); got != 0 {
		t.Errorf("the reclaim counted as new: %v", got)
	}
}

// R59 (docs/61 D4, D5): a join is a viewer's primary session. A stripe leg
// is not a viewer and a rejoin is the same viewer reconnecting; each counted
// session's length is observed when it closes.
func TestViewerJoinsCountPrimariesOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, stripedTestConfig(15), discardLog)
	sm := srv.metrics
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	defer pub.CloseWithError(0, "")

	base := fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id)
	const owner = "aabbccdd00112233"
	primary := dial(t, ctx, base+"?owner="+owner+"&app=web&os=ios&browser=safari", clientTLS)
	leg := dial(t, ctx, base+"?stripe=2&leg=0&owner="+owner+"&app=web&os=ios&browser=safari", clientTLS)
	rejoin := dial(t, ctx, base+"?delivery=reliable&rejoin=1&app=web&os=linux&browser=firefox", clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 3 }, "three sessions registered")

	if got := sm.ViewerJoinsCount(metrics.JoinFirst, "datagrams", "web", "ios", "safari"); got != 1 {
		t.Errorf("first{datagrams,web,ios,safari} = %v, want 1 (the leg must not count)", got)
	}
	if got := sm.ViewerJoinsCount(metrics.JoinRejoin, "reliable", "web", "linux", "firefox"); got != 1 {
		t.Errorf("rejoin{reliable,web,linux,firefox} = %v, want 1", got)
	}
	if got := sm.ViewerJoinsCount(metrics.JoinFirst, "reliable", "web", "linux", "firefox"); got != 0 {
		t.Errorf("the rejoin counted as first: %v", got)
	}

	leg.CloseWithError(0, "")
	primary.CloseWithError(0, "")
	rejoin.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool {
		return sm.ViewerSessionsObserved("datagrams") == 1 && sm.ViewerSessionsObserved("reliable") == 1
	}, "one datagrams and one reliable session observed ending (the leg is not one)")
}
