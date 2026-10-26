package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/voice"
)

// linkWriteTimeout bounds one link message write.
const linkWriteTimeout = 5 * time.Second

// link is one internal link's stream: a session link this pod dialled, or an
// observer link the relay dialled. Messages are voice.Message, framed by
// voice.WriteMessage / ReadMessage on the one bidirectional stream.
type link struct {
	sess *webtransport.Session
	str  *webtransport.Stream
	wmu  sync.Mutex
}

func (l *link) send(m voice.Message) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_ = l.str.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
	return voice.WriteMessage(l.str, m)
}

func (l *link) read() (voice.Message, error) { return voice.ReadMessage(l.str) }

// finish half-closes the stream after a last message and waits up to
// settle for the peer to take it (it closes the link, or the settle runs
// out): a session close discards what the peer has not yet read, the same
// reason the browser's close notice settles.
func (l *link) finish(settle time.Duration) {
	_ = l.str.Close()
	select {
	case <-l.sess.Context().Done():
	case <-time.After(settle):
	}
}

func (l *link) close(reason string) { _ = l.sess.CloseWithError(0, reason) }

// LinkStatusError is a session-link dial the relay refused with an HTTP
// status (401 bad PSK, 404 rooms or voice off, …).
type LinkStatusError struct {
	Status int
	Err    error
}

func (e *LinkStatusError) Error() string {
	return fmt.Sprintf("session link refused: status %d: %v", e.Status, e.Err)
}

func (e *LinkStatusError) Unwrap() error { return e.Err }

// sessionLinkURL is the relay route for code, WITH the psk. Never logged.
func (s *Server) sessionLinkURL(code string) string {
	return s.opts.Relay.URL + voice.SessionPath + url.PathEscape(code) + "?psk=" + url.QueryEscape(s.opts.PSK)
}

// dialSessionLink opens the session link for a browser's room (D2).
func (s *Server) dialSessionLink(ctx context.Context, code string) (*link, error) {
	rsp, sess, err := s.linkTr.Dial(ctx, s.sessionLinkURL(code), http.Header{"Origin": []string{voice.LinkOrigin}})
	if err != nil {
		if rsp != nil {
			return nil, &LinkStatusError{Status: rsp.StatusCode, Err: err}
		}
		return nil, err
	}
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		_ = sess.CloseWithError(0, "")
		return nil, err
	}
	return &link{sess: sess, str: str}, nil
}
