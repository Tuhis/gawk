package transport

import "net/http"

// handleInternalPop is CONNECT /internal/pop/{name}, pod to link-home pod: a want or a record for a PoP's link (docs/76 item 17).
// A stub until W3-uplink fills it: 501, so a PoP dialing a home that predates the
// leg reads a status rather than a hang.
func (s *Server) handleInternalPop(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
}
