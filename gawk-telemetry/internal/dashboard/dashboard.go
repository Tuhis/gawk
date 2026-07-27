// Package dashboard serves R28's live operator view (docs/33 TM8 + §4.8).
//
// Its single design goal: **someone who opens it during a live stream should
// see whether anything is wrong before they click anything.** The severity
// ordering, the live/ended grouping and the four-state health model all follow
// from that one sentence.
//
// The page is a React SPA built by `ui/` and embedded here (Go `embed`). §4.8.4
// originally specified hand-written HTML with no build step; the amendment and
// its reasoning are recorded in docs/33. What did NOT change is the constraint
// that actually mattered: **no external asset fetch**. Every byte the page needs
// is served by this binary, so it still works on a port-forward from a laptop
// with no network — asserted by a test over the built output, not assumed.
//
// It is the NON-droppable half of the item (D14): a live operational view is
// the thing an owner reaches for at 21:00 when a friend says "it's
// stuttering", and it is what makes the always-on collection worth having
// before any AI is involved.
package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The directory carries one committed file (README.md) so this compiles on a
// fresh clone: `//go:embed` fails outright against a directory with no matching
// files, which would make `go build ./...` depend on someone having run npm.
//
//go:embed dist
var dist embed.FS

const notBuilt = `<!doctype html><meta charset="utf-8"><title>gawk telemetry</title>
<body style="font:14px system-ui;padding:2rem;max-width:44rem">
<h1>Dashboard UI not built</h1>
<p>This binary was compiled without the dashboard bundle. The Go build does not
depend on npm, so that is a normal state for a fresh clone — but it means there
is no page to serve.</p>
<pre>cd gawk-telemetry/ui &amp;&amp; npm ci &amp;&amp; npm run build</pre>
<p>The read API and MCP endpoints on this listener are unaffected.</p>
`

// Handler serves the dashboard's static assets. The `/live` endpoint it polls
// is served by the read API on the same listener — both are on the NON-PUBLIC
// listener (D14): only ingest is routed publicly, because the read side
// aggregates every broadcast on the fleet.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	_, statErr := fs.Stat(sub, "index.html")
	built := statErr == nil

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No caching: an ops page showing yesterday's bundle after a redeploy
		// is a page that lies about what it is measuring. That also removes the
		// only thing content-hashed filenames would have bought, which is why
		// the build emits stable names instead.
		w.Header().Set("Cache-Control", "no-store")

		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, notBuilt)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		if b, err := fs.ReadFile(sub, name); err == nil {
			w.Header().Set("Content-Type", contentType(name))
			_, _ = w.Write(b)
			return
		}
		// SPA fallback: a path that is not a real asset serves the document, so
		// a deep link resolves client-side. The routes are hash-based (matching
		// the main app's convention) and the server never sees a fragment, but
		// the fallback costs nothing and lands a stray URL somewhere useful
		// instead of on a 404.
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	}), nil
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// Assets exposes the embedded files, so a test can assert what actually ships
// rather than trusting the author — most importantly that nothing in it reaches
// for another origin at runtime.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return dist
	}
	return sub
}
