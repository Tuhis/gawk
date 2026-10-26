package bridge

import (
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Close codes the bridge sends a browser (wire.go owns the values).
const (
	codeRemoved     = wire.CloseCodeVoiceRemoved     // 4015, terminal for voice
	codeUnavailable = wire.CloseCodeVoiceUnavailable // 4016, try again
	codeDraining    = wire.CloseCodeServerDraining   // 4002, rejoin at once
)

// The in-band close notice (docs/59), the relay's closenotice.go applied to
// voice: a webtransport-go close code does not reach Chrome (the close
// packet carries STOP_SENDING on the CONNECT stream ahead of the close
// capsule), so before every close a browser acts on — 4015, 4016, 4002 —
// the bridge states the code on its own server-opened uni stream
// (wire.SessionClosing) and closes a settle later. The settle matters: a
// notice and a close in one packet lose the notice.
const noticeWriteTimeout = 100 * time.Millisecond

// noticedCode reports whether a browser acts on code: every code the bridge
// closes a voice session with.
func noticedCode(code uint32) bool {
	switch code {
	case codeRemoved, codeUnavailable, codeDraining:
		return true
	}
	return false
}

// sendCloseNotice writes a SessionClosing on a new uni stream and reports
// whether it was handed to the transport.
func sendCloseNotice(sess *webtransport.Session, code uint32) bool {
	if !noticedCode(code) {
		return false
	}
	msg, err := wire.AppendSessionClosing(nil, code)
	if err != nil {
		return false
	}
	str, err := sess.OpenUniStream()
	if err != nil {
		return false
	}
	_ = str.SetWriteDeadline(time.Now().Add(noticeWriteTimeout))
	if _, err := str.Write(msg); err != nil {
		str.CancelWrite(0)
		return false
	}
	return str.Close() == nil
}

// closeWithNotice notices code, waits the settle and closes the session.
func closeWithNotice(sess *webtransport.Session, code uint32, reason string, settle time.Duration) {
	if sendCloseNotice(sess, code) {
		select {
		case <-time.After(settle):
		case <-sess.Context().Done():
		}
	}
	_ = sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
}
