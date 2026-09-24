package transport

import (
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R57 (docs/59): the in-band close notice.
//
// A webtransport-go close code never reaches Chrome — the library's close
// packet carries STOP_SENDING on the CONNECT stream ahead of the close
// capsule, and Chrome fails the session on it ("Connection lost.", no code;
// quic-go/webtransport-go#242, closed upstream as a Chromium bug). So before
// the relay closes a browser-facing session with a code that changes what
// the client does next, it states the code on its own uni stream
// (wire.SessionClosing) and closes closeNoticeSettle later.
//
// Only external publish/subscribe sessions get it: the edge client reads
// every uni stream on /internal/subscribe as a keyframe, and a room control
// session's RoomEnding already says what 4007 means.

// closeNoticeSettle is how long the close waits after the notice: the
// session close cancels every stream and discards what the peer has not yet
// read, so a notice and a close that share a packet lose the notice (the
// room registry's closeSettle, docs/gotchas.md).
const closeNoticeSettle = 250 * time.Millisecond

// closeNoticeWriteTimeout bounds the notice write. Six bytes on a fresh
// stream; a peer that cannot take them gets the bare close.
const closeNoticeWriteTimeout = 100 * time.Millisecond

// noticedCloseCode reports whether code is one a client acts on differently
// from a plain drop — the terminal ones. 4001/4002/4005 all mean "reconnect",
// which is what a client does with no code anyway.
func noticedCloseCode(code uint32) bool {
	switch code {
	case wire.CloseCodeBroadcastEnded, wire.CloseCodePublisherSuperseded, wire.CloseCodeTerminatedByOperator:
		return true
	}
	return false
}

// uniStreamOpener is the part of *webtransport.Session a notice needs.
// Anything that lacks it (a test fake) is closed without one.
type uniStreamOpener interface {
	OpenUniStream() (*webtransport.SendStream, error)
}

// sendCloseNotice writes a SessionClosing for code on a new uni stream and
// reports whether it was handed to the transport. Never blocks for long:
// OpenUniStream does not wait for stream credit, and the write is deadlined.
func sendCloseNotice(sess any, code uint32) bool {
	if !noticedCloseCode(code) {
		return false
	}
	opener, ok := sess.(uniStreamOpener)
	if !ok {
		return false
	}
	msg, err := wire.AppendSessionClosing(nil, code)
	if err != nil {
		return false
	}
	str, err := opener.OpenUniStream()
	if err != nil {
		return false
	}
	_ = str.SetWriteDeadline(time.Now().Add(closeNoticeWriteTimeout))
	if _, err := str.Write(msg); err != nil {
		str.CancelWrite(0)
		return false
	}
	return str.Close() == nil
}

// closeWithNotice notices code (when it is one) and closes sess after the
// settle, blocking the caller for it. For handlers that return right after
// closing — the session must not outlive its handler with a different code.
func closeWithNotice(sess drainSession, code uint32, reason string) {
	if sendCloseNotice(sess, code) {
		time.Sleep(closeNoticeSettle)
	}
	_ = sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
}

// closeWithNoticeAsync is closeWithNotice without the wait: the close lands
// closeNoticeSettle later on a timer. For callers that must not stall — the
// hub closing every viewer of a broadcast, a moderation kill — where the
// session's own handler keeps it alive until then, and the hub has already
// detached it (a deposed publisher's late frames drop, docs/06).
func closeWithNoticeAsync(sess drainSession, code uint32, reason string) {
	if !sendCloseNotice(sess, code) {
		_ = sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
		return
	}
	time.AfterFunc(closeNoticeSettle, func() {
		_ = sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
	})
}
