package api

import (
	"net/http"
	"time"
)

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
	writeJSON(w, http.StatusOK, relaysPageJSON{Relays: out, Bus: a.busHealth(r)})
}

// busHealth renders the R50 bus section, or nil when no bus is configured.
func (a *API) busHealth(r *http.Request) *busJSON {
	if a.opts.Bus == nil {
		return nil
	}
	h := a.opts.Bus.Health(r.Context())
	if h == nil {
		return nil
	}
	out := &busJSON{Stream: h.Stream, Connected: h.Connected,
		Messages: h.Messages, Bytes: h.Bytes, Error: h.Error}
	for _, p := range h.Pods {
		out.Pods = append(out.Pods, busPodJSON{
			Pod:      p.Pod,
			LastSeen: p.LastSeen.UTC().Format(time.RFC3339),
			LastSeq:  p.LastSeq,
			Gaps:     p.Gaps,
		})
	}
	return out
}
