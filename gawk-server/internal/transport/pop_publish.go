package transport

import "net/http"

// handlePopPublish is CONNECT /pop/publish/{id}, a PoP's uplink leg for a reserved broadcast (docs/76 item 18).
// A stub until W3-uplink fills it: 501, so a PoP dialing a home that predates the
// leg reads a status rather than a hang.
func (s *Server) handlePopPublish(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}
