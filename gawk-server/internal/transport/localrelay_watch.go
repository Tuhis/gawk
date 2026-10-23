package transport

import (
	"bytes"
	"log/slog"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// LocalRelayWatcher is an optional LocalRelayProvider extension (R74 PP11):
// a publish session's on-wall state follows Hide, Show and the link, so
// the provider says when a broadcast's 0x18 may have changed and the
// session gets it again. changed may run on any goroutine and must not
// block; cancel stops it.
type LocalRelayWatcher interface {
	WatchLocalRelayInfo(publishID string, changed func()) (cancel func())
}

// watchLocalRelayInfo re-sends a publish session's 0x18 whenever its
// encoding changes, until the session ends. first is the message already
// sent.
func (s *Server) watchLocalRelayInfo(sess *webtransport.Session, p LocalRelayProvider, publishID string, first []byte, log *slog.Logger) {
	w, ok := p.(LocalRelayWatcher)
	if !ok || publishID == "" {
		return
	}
	last := first
	pending := make(chan struct{}, 1)
	cancel := w.WatchLocalRelayInfo(publishID, func() {
		select {
		case pending <- struct{}{}:
		default:
		}
	})
	go func() {
		defer cancel()
		for {
			select {
			case <-sess.Context().Done():
				return
			case <-pending:
			}
			msg, err := wire.AppendLocalRelayInfo(nil, p.LocalRelayInfo(publishID))
			if err != nil {
				log.Warn("local relay info update skipped: encode failed", "err", err)
				continue
			}
			if bytes.Equal(msg, last) {
				continue
			}
			last = msg
			if err := sendUniMessage(sess, msg); err != nil {
				log.Debug("local relay info update not sent", "err", err)
			}
		}
	}()
}
