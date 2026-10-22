package transport

import "net/http"

// handlePopSubscribe is CONNECT /pop/subscribe/{id}, a PoP's downlink leg for a home broadcast (docs/76 §6.6).
// A stub until W3-downlink fills it: 501, so a PoP dialing a home that predates the
// leg reads a status rather than a hang.
func (s *Server) handlePopSubscribe(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}
