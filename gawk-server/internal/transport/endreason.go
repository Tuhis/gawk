package transport

import (
	"context"
	"time"

	"github.com/quic-go/quic-go"
)

// Session-end reasons are recovered from the QUIC connection's context
// (verified against quic-go v0.60). A handler's ReceiveDatagram returns
// context.Cause(r.Context()), but http3 cancels the request context with a
// plain cancel via context.AfterFunc(str.Context(), cancel), discarding the
// cause; and quic-go tears streams down in handleCloseError before the
// deferred c.ctxCancel(err) that carries the real reason runs. The stream
// context wins that race essentially always, so without this every abrupt
// death — idle timeout, stateless reset (e.g. a NAT rebind onto another pod),
// peer CONNECTION_CLOSE — logs as a bare "context canceled". ConnContext is
// the http3 seam that lets handlers read the connection's cause instead.

// connContextKey carries the QUIC connection's context down to each handler.
type connContextKey struct{}

// withConnContext is the http3.Server.ConnContext hook. It stores the
// connection's context — not the *quic.Conn — so the value is a plain
// context.Context that tests can supply without a QUIC stack.
func withConnContext(ctx context.Context, conn *quic.Conn) context.Context {
	return context.WithValue(ctx, connContextKey{}, conn.Context())
}

// connCauseWait bounds how long sessionEndReason waits for the connection's
// context to catch up with the stream's. It only ever elapses when the stream
// ended on its own (a clean session close capsule) while the connection lives,
// which needs no extra detail anyway. It delays only the deferred release of
// an already-ended session.
var connCauseWait = 100 * time.Millisecond

// sessionEndReason turns a handler's read error into the most specific reason
// available. Anything other than a bare cancellation is already the truth and
// is returned untouched.
func sessionEndReason(ctx context.Context, err error) error {
	if err == nil || !isBareCancel(err) {
		return err
	}
	connCtx, ok := ctx.Value(connContextKey{}).(context.Context)
	if !ok || connCtx == nil {
		return err
	}
	// The connection's cancellation trails the stream's (see above), so wait
	// for it rather than depend on the race.
	if connCtx.Err() == nil {
		timer := time.NewTimer(connCauseWait)
		defer timer.Stop()
		select {
		case <-connCtx.Done():
		case <-timer.C:
		}
	}
	if cause := context.Cause(connCtx); cause != nil && !isBareCancel(cause) {
		return cause
	}
	return err
}

// isBareCancel reports whether err is a cancellation carrying no reason of its
// own. errors.Is is deliberately not used: a wrapped cause that contains a
// cancellation is still more informative than the bare sentinel.
func isBareCancel(err error) bool { return err == context.Canceled }
