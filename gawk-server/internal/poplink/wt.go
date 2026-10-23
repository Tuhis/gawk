package poplink

import (
	"context"
	"errors"
	"io"

	"github.com/quic-go/webtransport-go"
)

// WrapSession adapts a WebTransport session to Session: both ends of a link
// see the same narrow interface, so the home and PoP logic run against
// in-memory fakes in their unit tests.
func WrapSession(s *webtransport.Session) Session { return wtSession{s} }

type wtSession struct{ *webtransport.Session }

func (s wtSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return s.Session.OpenStreamSync(ctx)
}

func (s wtSession) AcceptStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return s.Session.AcceptStream(ctx)
}

func (s wtSession) CloseWithError(code uint32, msg string) error {
	return s.Session.CloseWithError(webtransport.SessionErrorCode(code), msg)
}

// CloseCode reports the application close code a session ended with, as
// seen from the reading side (ok false for anything but a peer's or our own
// CloseWithError).
func CloseCode(err error) (code uint32, ok bool) {
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return uint32(se.ErrorCode), true
	}
	return 0, false
}
