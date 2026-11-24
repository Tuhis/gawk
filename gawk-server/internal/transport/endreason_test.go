package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
)

func requestCtx(connCtx context.Context) context.Context {
	return context.WithValue(context.Background(), connContextKey{}, connCtx)
}

// A read error that already says something keeps saying it.
func TestSessionEndReasonKeepsASpecificError(t *testing.T) {
	connCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("timeout: no recent network activity"))

	want := errors.New("EOF")
	if got := sessionEndReason(requestCtx(connCtx), want); got != want {
		t.Errorf("sessionEndReason = %v, want the original %v", got, want)
	}
}

// The case the incident turned on: a bare cancellation is replaced by the
// connection's real cause.
func TestSessionEndReasonRecoversTheConnectionCause(t *testing.T) {
	cause := errors.New("timeout: no recent network activity")
	connCtx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)

	if got := sessionEndReason(requestCtx(connCtx), context.Canceled); got != cause {
		t.Errorf("sessionEndReason = %v, want %v", got, cause)
	}
}

// quic-go cancels the request stream's context before the connection's, so a
// reason that is not available *yet* must still be recovered. Without the
// wait this test fails and the logs go back to saying "context canceled".
func TestSessionEndReasonWaitsForALateConnectionCause(t *testing.T) {
	cause := errors.New("stateless reset")
	connCtx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel(cause)
	}()

	if got := sessionEndReason(requestCtx(connCtx), context.Canceled); got != cause {
		t.Errorf("sessionEndReason = %v, want %v", got, cause)
	}
}

// A live connection has no cause to offer — the session ended on its own.
// Bounded, so the handler's deferred release is not held up.
func TestSessionEndReasonGivesUpOnALiveConnection(t *testing.T) {
	connCtx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	start := time.Now()
	got := sessionEndReason(requestCtx(connCtx), context.Canceled)
	if got != context.Canceled {
		t.Errorf("sessionEndReason = %v, want the original cancellation", got)
	}
	if elapsed := time.Since(start); elapsed > connCauseWait*4 {
		t.Errorf("waited %v for a live connection, want ~%v", elapsed, connCauseWait)
	}
}

// A connection cancelled without a cause of its own adds nothing, and must not
// turn one bare cancellation into another.
func TestSessionEndReasonIgnoresACauselessCancel(t *testing.T) {
	connCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := sessionEndReason(requestCtx(connCtx), context.Canceled); got != context.Canceled {
		t.Errorf("sessionEndReason = %v, want the original cancellation", got)
	}
}

// Handlers must survive a request that never went through ConnContext (every
// test that builds a bare httptest-style request).
func TestSessionEndReasonSurvivesAMissingConnContext(t *testing.T) {
	if got := sessionEndReason(context.Background(), context.Canceled); got != context.Canceled {
		t.Errorf("sessionEndReason = %v, want the original cancellation", got)
	}
}

// R87 SG0: the adapter reports PathStats only when ConnContext stored a
// reader, and then reports exactly what quic-go said.
func TestAdapterPathStats(t *testing.T) {
	tests := []struct {
		name   string
		ctx    context.Context
		wantOK bool
		want   hub.PathStats
	}{
		{"no reader in context", context.Background(), false, hub.PathStats{}},
		{"reader in context", context.WithValue(context.Background(), connStatsKey{}, connStatsFunc(func() quic.ConnectionStats {
			return quic.ConnectionStats{SmoothedRTT: 42 * time.Millisecond, MinRTT: 40 * time.Millisecond, BytesSent: 9, PacketsSent: 3, PacketsLost: 1}
		})), true, hub.PathStats{SmoothedRTT: 42 * time.Millisecond, MinRTT: 40 * time.Millisecond, BytesSent: 9, PacketsSent: 3, PacketsLost: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &webtransportSessionAdapter{stats: pathStatsFrom(tc.ctx)}
			got, ok := a.PathStats()
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("PathStats() = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
