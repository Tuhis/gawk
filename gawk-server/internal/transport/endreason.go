package transport

import (
	"context"
	"time"

	"github.com/quic-go/quic-go"
)

// Why a session ended is the single most useful thing these logs record, and
// until now they threw it away in exactly the case that mattered.
//
// The mechanism, verified against quic-go v0.60. A handler's ReceiveDatagram
// returns context.Cause(r.Context()) when the request context ends. http3
// builds that context as WithCancel(connCtx) plus
// context.AfterFunc(str.Context(), cancel) (http3/server_conn.go) — a *plain*
// cancel, so the cause is discarded. And when a connection dies, quic-go tears
// the streams down inside handleCloseError, while c.ctxCancel(err) — the one
// call that carries the real reason — is a deferred call that only runs after
// Conn.run returns (connection.go). The stream context therefore wins the race
// essentially always, and every abrupt death is logged as a bare
// "context canceled": an idle timeout, a stateless reset and a peer's
// CONNECTION_CLOSE are indistinguishable.
//
// That is not academic. On 2026-07-27 a broadcast was garbage-collected with
// three viewers attached after its publisher session ended with reason
// "context canceled", and the logs could not say whether the broadcaster's
// uplink had gone away, whether its NAT had rebound onto a different pod (a
// stateless reset, in a fleet that shares a StatelessResetKey), or something
// else entirely.
//
// The fix is to keep hold of the QUIC connection's own context and read its
// cause instead. ConnContext is the seam http3 provides for exactly this.

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
// which is the case that needs no extra detail anyway. Small enough to be
// irrelevant next to any grace period, since it delays only the deferred
// release on a session that has already ended.
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
	// The connection's cancellation trails the stream's by a scheduling hair
	// (see above), so waiting is what makes this work at all rather than
	// working only when the race happens to fall the right way.
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
// own. errors.Is is deliberately not used: a wrapped cause that *contains* a
// cancellation (quic-go's application errors do not, but a future one might)
// is still more informative than the bare sentinel, and would be discarded by
// an errors.Is check here.
func isBareCancel(err error) bool { return err == context.Canceled }
