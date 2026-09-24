package transport

// R56 (docs/58 CN2): the in-band close notice. Chrome never reads a
// webtransport-go close code, so before the relay closes a browser-facing
// session with a code that changes what the client does next, it says the
// code on its own uni stream (wire.SessionClosing) and closes a settle
// interval later. These drive each terminal path through the real relay with
// a Go client — which DOES read close codes — and assert both halves: the
// notice arrives, and the close that follows carries the same code.

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// drainUntilClosed reads every server-opened uni stream until the session
// ends, and returns the SessionClosing code seen (0 if none), the close code
// and when each arrived.
func drainUntilClosed(t *testing.T, ctx context.Context, sess *webtransport.Session) (notice uint32, noticeAt time.Time, closeCode webtransport.SessionErrorCode, closedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		str, err := sess.AcceptUniStream(ctx)
		if err != nil {
			var se *webtransport.SessionError
			if !errors.As(err, &se) {
				t.Fatalf("session ended with %v, want a WebTransport session close", err)
			}
			return notice, noticeAt, se.ErrorCode, time.Now()
		}
		_ = str.SetReadDeadline(time.Now().Add(2 * time.Second))
		msg, _ := io.ReadAll(io.LimitReader(str, 1<<20))
		if len(msg) >= 2 && msg[1] == wire.TypeSessionClosing {
			code, perr := wire.ParseSessionClosing(msg)
			if perr != nil {
				t.Fatalf("SessionClosing unreadable: %v (% x)", perr, msg)
			}
			notice, noticeAt = code, time.Now()
		}
	}
}

func assertNoticedClose(t *testing.T, who string, want uint32, notice uint32, noticeAt time.Time, code webtransport.SessionErrorCode, closedAt time.Time) {
	t.Helper()
	if notice != want {
		t.Errorf("%s: SessionClosing code = %d, want %d — a browser would see a bare \"Connection lost.\"", who, notice, want)
	}
	if code != webtransport.SessionErrorCode(want) {
		t.Errorf("%s: close code = %d, want %d", who, code, want)
	}
	if notice == want && closedAt.Before(noticeAt) {
		t.Errorf("%s: the session closed before the notice was read", who)
	}
}

func TestBroadcastEndedSubscriberGetsCloseNotice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  100 * time.Millisecond,
	})
	pub, id := dialPublisherAndGetID(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")

	pub.CloseWithError(0, "") // grace (100 ms) then GC → 4000 to the viewer
	n, nAt, code, cAt := drainUntilClosed(t, ctx, sub)
	assertNoticedClose(t, "viewer", wire.CloseCodeBroadcastEnded, n, nAt, code, cAt)
}

func TestSupersededPublisherGetsCloseNotice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _ := startTestServer(t, ctx, 15)

	first, id, token := dialPublisherHandshake(t, ctx, port, clientTLS)
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherActive
	}, "first publisher registered")
	second := dialPublisherReclaim(t, ctx, port, id, token, clientTLS)
	defer second.CloseWithError(0, "")

	n, nAt, code, cAt := drainUntilClosed(t, ctx, first)
	assertNoticedClose(t, "deposed publisher", wire.CloseCodePublisherSuperseded, n, nAt, code, cAt)
}

func TestKilledBroadcastNoticesPublisherAndViewer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, config.Config{
		MaxSubscribers:  15,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		BroadcastGrace:  5 * time.Minute,
	}, discardLog)
	pub, id, _ := dialPublisherHandshake(t, ctx, port, clientTLS)
	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	waitFor(t, 5*time.Second, func() bool { return r.Stats().Totals.Subscribers == 1 }, "subscriber registered")
	waitFor(t, 5*time.Second, func() bool { _, ok := srv.PublisherRemote(id); return ok }, "publisher tracked")

	srv.HandleBanAdded(idBan(id, "notice test"))

	type result struct {
		n     uint32
		nAt   time.Time
		code  webtransport.SessionErrorCode
		cAt   time.Time
		label string
	}
	results := make(chan result, 2)
	for label, s := range map[string]*webtransport.Session{"publisher": pub, "viewer": sub} {
		go func() {
			n, nAt, code, cAt := drainUntilClosed(t, ctx, s)
			results <- result{n, nAt, code, cAt, label}
		}()
	}
	for range 2 {
		res := <-results
		assertNoticedClose(t, res.label, wire.CloseCodeTerminatedByOperator, res.n, res.nAt, res.code, res.cAt)
	}
}
