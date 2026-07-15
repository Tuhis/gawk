package engine

import (
	"context"
	"crypto/tls"
	"io"
	"net/url"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// RelaySession is the slice of a WebTransport session the engine uses. It is a
// seam, not an abstraction for its own sake: the send policy (docs/19
// Decision 12) is defined by what happens when sends *fail* — a queue-full
// datagram, a stream that stalls past the next keyframe, a
// DatagramTooLargeError — and a real relay will not produce those on demand.
// Every one of those paths is a unit test against a fake implementing this.
type RelaySession interface {
	SendDatagram(b []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	OpenUniStream() (SendStream, error)
	AcceptUniStream(ctx context.Context) (ReceiveStream, error)
	CloseWithError(code webtransport.SessionErrorCode, msg string) error
	// Context is cancelled when the session ends, for any reason.
	Context() context.Context
}

// SendStream is one outgoing unidirectional stream (a keyframe).
type SendStream interface {
	io.Writer
	io.Closer
	// CancelWrite abandons the stream. This is how a superseded keyframe dies
	// (Decision 12: ≤1 in flight).
	CancelWrite(code webtransport.StreamErrorCode)
}

// ReceiveStream is one incoming unidirectional stream (the announce).
type ReceiveStream interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// wtSession adapts *webtransport.Session to RelaySession. The methods exist
// only to convert concrete stream types into interfaces — carefully, since
// returning a typed nil pointer in an interface would produce a non-nil
// interface holding nil, and the caller's `if str == nil` would not fire.
type wtSession struct{ s *webtransport.Session }

func (w wtSession) SendDatagram(b []byte) error { return w.s.SendDatagram(b) }

func (w wtSession) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return w.s.ReceiveDatagram(ctx)
}

func (w wtSession) OpenUniStream() (SendStream, error) {
	str, err := w.s.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return str, nil
}

func (w wtSession) AcceptUniStream(ctx context.Context) (ReceiveStream, error) {
	str, err := w.s.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return str, nil
}

func (w wtSession) CloseWithError(code webtransport.SessionErrorCode, msg string) error {
	return w.s.CloseWithError(code, msg)
}

func (w wtSession) Context() context.Context { return w.s.Context() }

// DialFunc dials the relay. Injectable so tests can supply a fake session
// without a QUIC stack; the default is dialRelay below.
//
// It returns the HTTP status separately from the error because that is the
// whole point of Decision 10: webtransport-go surfaces the status the browser
// cannot see. Status is 0 when the failure never reached HTTP.
type DialFunc func(ctx context.Context, rawURL string, insecure bool) (sess RelaySession, status int, err error)

// PublishURL builds the CONNECT URL for a broadcast.
//
// The secret travels as a query parameter, not a header. That is not a
// shortcut: it is how the secret already travels for the browser broadcaster,
// because the WebTransport JS API cannot set request headers (R2, docs/07),
// and the relay reads r.URL.Query().Get("secret"). A native client could send
// a header, but then the two broadcasters would authenticate differently and
// the relay would need two paths.
func PublishURL(relayURL, broadcastID, secret string) (string, error) {
	base, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	path := "/publish"
	if broadcastID != "" {
		path += "/" + broadcastID
	}
	u, err := base.Parse(path)
	if err != nil {
		return "", err
	}
	if secret != "" {
		q := u.Query()
		q.Set("secret", secret)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// dialRelay is the production DialFunc.
func dialRelay(ctx context.Context, rawURL string, insecure bool) (RelaySession, int, error) {
	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			// A native client trusts the homelab CA directly, so the
			// browser's serverCertificateHashes 14-day certificate dance
			// (docs/02) simply does not apply here. -insecure exists for the
			// dev cert only.
			InsecureSkipVerify: insecure, //nolint:gosec // opt-in via -insecure, dev certs only
			NextProtos:         []string{http3.NextProtoH3},
		},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			// Keep the session alive across idle gaps the same way the relay
			// does for viewers (D1): a broadcaster mid-menu can go seconds
			// without a keyframe worth sending.
			KeepAlivePeriod: 10 * time.Second,
		},
	}
	rsp, sess, err := d.Dial(ctx, rawURL, nil)
	status := 0
	if rsp != nil {
		status = rsp.StatusCode
	}
	if err != nil {
		return nil, status, err
	}
	return wtSession{s: sess}, status, nil
}
