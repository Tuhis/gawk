package api

import "net/http"

// handleListRelays is the read-only effective-config view (D10).
//
// There is NO write path here and there must never be one: GitOps stays the
// only mutation channel for relay configuration (CLAUDE.md's deploy model).
// The read view exists for exactly one question — "which pod has the stale
// flag?" — and answers it from each pod's own sanitized config, secrets
// already redacted at the source (§4.5).
func (a *API) handleListRelays(w http.ResponseWriter, r *http.Request) {
	if a.opts.Fleet == nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "relay enumeration is not configured")
		return
	}
	snap, err := a.opts.Fleet.Snapshot(r.Context())
	if err != nil {
		a.log.Error("relay enumeration failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "the relay fleet could not be enumerated")
		return
	}
	out := make([]relayJSON, 0, len(snap.Pods))
	for _, p := range snap.Pods {
		row := relayJSON{Pod: p.Name, Reachable: p.Reachable, Version: p.Version, Config: p.Config, Error: p.Err}
		if row.Error == "" && p.ConfigErr != "" {
			// The pod answered for broadcasts but not for config: say so
			// rather than rendering an empty config table as if it were the
			// truth.
			row.Error = p.ConfigErr
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"relays": out})
}
