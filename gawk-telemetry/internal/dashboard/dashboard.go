// Package dashboard serves R28's live operator view (docs/33 TM8 + §4.8).
//
// Its single design goal: **someone who opens it during a live stream should
// see whether anything is wrong before they click anything.** The severity
// ordering, the live/ended grouping and the four-state health model all follow
// from that one sentence.
//
// Assets are embedded (Go `embed`) with **no build step and no external asset
// fetch** — the page must work on a port-forward from a laptop with no
// network. That is asserted by a test that loads it with nothing else
// reachable.
//
// It is the NON-droppable half of the item (D14): a live operational view is
// the thing an owner reaches for at 21:00 when a friend says "it's
// stuttering", and it is what makes the always-on collection worth having
// before any AI is involved.
package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// Handler serves the dashboard's static assets. The `/live` endpoint it polls
// is served by the read API on the same listener — both are on the NON-PUBLIC
// listener (D14): only ingest is routed publicly, because the read side
// aggregates every broadcast on the fleet.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	// Routed explicitly rather than through http.FileServer: the FileServer
	// canonicalizes "/index.html" back to "./", which against a handler that
	// rewrites "/" to "/index.html" is an infinite redirect. There are two
	// files; naming them is clearer than fighting the canonicalizer.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No caching: an ops page showing yesterday's bundle after a redeploy
		// is a page that lies about what it is measuring.
		w.Header().Set("Cache-Control", "no-store")

		name := strings.TrimPrefix(r.URL.Path, "/")
		// The page is deep-linkable by HASH route (matching the main app's
		// convention), so any unrecognized path serves the same document and
		// the client routes on the fragment — which the server never sees.
		if name == "" || name == "index.html" || !known(name) {
			serveFile(w, sub, "index.html", "text/html; charset=utf-8")
			return
		}
		serveFile(w, sub, name, contentType(name))
	}), nil
}

func known(name string) bool {
	return name == "index.html" || name == "app.js"
}

func contentType(name string) string {
	if strings.HasSuffix(name, ".js") {
		return "text/javascript; charset=utf-8"
	}
	return "text/html; charset=utf-8"
}

func serveFile(w http.ResponseWriter, sub fs.FS, name, ctype string) {
	b, err := fs.ReadFile(sub, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

// Assets exposes the embedded files, so a test can assert the page references
// nothing it does not ship.
func Assets() fs.FS {
	sub, _ := fs.Sub(assets, "assets")
	return sub
}
