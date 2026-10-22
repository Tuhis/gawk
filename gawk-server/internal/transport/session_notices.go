package transport

import (
	"net/http"

	"github.com/quic-go/webtransport-go"
)

// sessionKind names the client-facing route a session arrived on, for
// sendSessionNotices.
type sessionKind uint8

const (
	kindEcho sessionKind = iota
	kindPublish
	kindSubscribe
	kindRoom
)

// sendSessionNotices sends the per-session R74 notices (docs/76) at the tail
// of every client-facing session setup: /echo, /publish, a viewer's
// /subscribe (never a stripe leg or /internal/*) and room sessions. It must
// not block the caller. Empty until the notices land.
func (s *Server) sendSessionNotices(sess *webtransport.Session, r *http.Request, k sessionKind) {}
