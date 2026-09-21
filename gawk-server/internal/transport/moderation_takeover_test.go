package transport

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A ban landing inside the upgrade window must close the publisher with 4006,
// "terminated by operator" — that is what D6 exists for, and what an operator
// reading the logs and a broadcaster reading its own close code both depend
// on. TestPublishBanLandingInsideTheUpgradeWindowStillCloses pins that for a
// claim whose slot was free by the time the request arrived.
//
// This is the same window on the OTHER claim path, the one where the previous
// publisher session is still holding the slot: ResumePublish returns
// ErrPublisherActive with a nil publisher, the depose is deferred to
// TakeOverPublish after the upgrade, and the ban's kill has removed the hub in
// between. TakeOverPublish then fails with "not found", which reads as a GC'd
// broadcast and closes 4000 ("broadcast ended") — the wrong reason, reported
// before the 4006 re-check further down is ever reached.
//
// Keeping the first session open is what makes it deterministic: the other
// test closes its warm session client-side and then races the server's
// processing of that close, which is why it reported 4000 on CI (run
// 35627508457) roughly one run in a few dozen and passed everywhere else.
func TestPublishBanInTheUpgradeWindowClosesTerminatedEvenOnTheTakeoverPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)

	bans := moderation.NewSet()
	srv.SetModeration(bans)

	// LEFT OPEN, unlike the sibling test: the slot stays held, so the claim
	// below takes the takeover path rather than getting the publisher back
	// from ResumePublish.
	warm, warmID, warmToken := dialPublisherHandshake(t, ctx, port, clientTLS)
	defer warm.CloseWithError(0, "")

	hook := func(id string) {
		if id != warmID {
			t.Errorf("the hook saw ID %q, want %q", id, warmID)
		}
		rec := idBan(id, "fraudulent stream")
		// The source's own order: close the gate, then actuate. Actuating
		// removes the hub, which is what leaves TakeOverPublish with nothing.
		if err := bans.Upsert(rec); err != nil {
			t.Errorf("Upsert: %v", err)
			return
		}
		srv.HandleBanAdded(rec)
	}
	srv.testHookPostUpgradePublish.Store(&hook)

	url := fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, warmID, warmToken)
	_, sess, err := dialOnce(t, ctx, url, clientTLS)
	if err == nil {
		defer sess.CloseWithError(0, "")
		acceptCtx, acceptCancel := context.WithTimeout(ctx, 5*time.Second)
		for {
			if _, err = sess.AcceptUniStream(acceptCtx); err != nil {
				break
			}
		}
		acceptCancel()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the publisher that raced the ban is still connected: the kill missed it and nothing re-checked")
	}
	var se *webtransport.SessionError
	if !errors.As(err, &se) {
		t.Fatalf("publisher session ended with %v, want a WebTransport session close", err)
	}
	if se.ErrorCode != webtransport.SessionErrorCode(wire.CloseCodeTerminatedByOperator) {
		t.Fatalf("close code = %d, want %d (4006, terminated by operator — 4000 means the "+
			"takeover's not-found path reported a ban as an ended broadcast)",
			se.ErrorCode, wire.CloseCodeTerminatedByOperator)
	}
}
