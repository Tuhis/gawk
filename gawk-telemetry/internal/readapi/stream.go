package readapi

// UD22: live delivery over Server-Sent Events, with the existing poll as the
// fallback.
//
// The projection is UNCHANGED — only its delivery is. At UD12's scale
// (~50 broadcasts / ~200 viewers) the `/live` payload carries findings and
// metrics for every session, and re-sending all of it every 2 s to every open
// tab is the pressure this relieves. TH11's watch plus several open views
// multiply it.
//
// Three properties make SSE the right shape here rather than WebSockets:
// it is one-way (which is what this is), it survives an HTTP proxy and a
// port-forward unchanged — the whole deployment surface — and a client that
// loses it falls back to the poll it already had, so nothing depends on the
// stream existing.
//
// **Only changes are sent.** The projection is hashed after marshalling and an
// identical snapshot is skipped, so an idle fleet costs a heartbeat rather than
// a payload. That is the actual saving; the transport alone would not have been
// worth an endpoint.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
)

// StreamInterval is how often the stream re-evaluates the projection. Matched
// to the projection's own 2 s refresh: sampling faster would send the same
// bytes twice, and slower would make the stream worse than the poll it
// replaces.
const StreamInterval = 2 * time.Second

// StreamHeartbeat bounds how long the connection can go silent. Proxies and
// browsers drop an idle stream; a comment frame is not an event and does not
// reach the page's handler, so an idle fleet stays quiet AND stays connected.
const StreamHeartbeat = 20 * time.Second

// streamFingerprint hashes what a snapshot SAYS, ignoring when it said it.
//
// `Snapshot.AtMs` is a fresh clock reading on every call, so hashing the
// marshalled response verbatim would make every tick look like a change — and
// the change-only rule, which is the whole reason this endpoint exists rather
// than the poll it replaces, would silently buy nothing. Zeroing the clock
// before hashing is the difference between "only changes are sent" being a
// property and being a comment.
func streamFingerprint(snap live.Snapshot) [sha256.Size]byte {
	snap.AtMs = 0
	b, err := json.Marshal(snap)
	if err != nil {
		// Unhashable is treated as "changed": re-sending is wasteful, and
		// sitting on a projection nobody can hash would be wrong.
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(b)
}

func (a *API) handleLiveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher means no streaming. Saying so lets the client fall back
		// immediately instead of hanging on a response that will never arrive.
		http.Error(w, "streaming is not supported by this server", http.StatusNotImplemented)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold every event
	// until the buffer filled — turning a live stream into a batched one. The
	// internal Ingress is the deployment surface, so this is not optional.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Retry tells the browser's EventSource how long to wait before
	// reconnecting. Matched to the poll cadence so a dropped stream degrades to
	// something no worse than polling.
	//
	// Deliberately NOT flushed on its own: the retry hint and the first
	// snapshot belong in one write, so a client's first read carries data
	// rather than only a directive about reconnecting.
	fmt.Fprintf(w, "retry: %d\n\n", StreamInterval.Milliseconds())

	var lastHash [sha256.Size]byte
	lastSent := time.Now()
	send := func(force bool) bool {
		snap := a.LiveSnapshot()
		b, err := json.Marshal(snap)
		if err != nil {
			return true
		}
		sum := streamFingerprint(snap)
		if !force && sum == lastHash {
			if time.Since(lastSent) < StreamHeartbeat {
				return true
			}
			// A comment frame: keeps the connection (and any proxy) alive
			// without delivering an event the page would re-render for.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return false
			}
			flusher.Flush()
			lastSent = time.Now()
			return true
		}
		lastHash, lastSent = sum, time.Now()
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send(true) {
		return
	}
	t := time.NewTicker(StreamInterval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send(false) {
				return
			}
		}
	}
}
