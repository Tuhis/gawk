package transport

import "net/http"

// registerPopRoutes adds the R74 home-side /pop/* routes (docs/76) to the
// public mux. Called from New only with -pops on. Empty until the PoP link
// lands.
func (s *Server) registerPopRoutes(mux *http.ServeMux) {}
