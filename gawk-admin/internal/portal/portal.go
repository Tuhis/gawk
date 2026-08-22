// Package portal serves the moderation SPA (R39, docs/42 §4.9).
//
// The page is a React SPA built by `ui/` and embedded here (Go `embed`), on the
// same listener as `/api/v1` and `/auth/config` — one port, one origin, one
// TLS certificate (§4.6). That co-location is not tidiness: the portal's CSP is
// `default-src 'self'; connect-src 'self' <issuer origin>` (§4.8), so the API
// the page talks to has to BE 'self', and the identity provider is the only
// other origin the browser is allowed to reach.
//
// Which makes the constraint this package's test enforces a security property
// rather than an offline nicety: **nothing in the bundle may fetch from another
// origin**. A CDN script tag would be blocked by the CSP at runtime, and the
// portal would simply fail to render for the operator who is, at that moment,
// trying to end a broadcast from a phone. The test asserts it against the built
// output rather than trusting the author.
//
// The mechanics are gawk-telemetry's dashboard package, ported deliberately
// rather than rediscovered: a committed `dist/README.md` so `go build ./...`
// never depends on npm, a "not built" page that says so plainly, SPA fallback
// routing, and `Cache-Control: no-store`.
package portal

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

const notBuilt = `<!doctype html><meta charset="utf-8"><title>gawk admin</title>
<body style="font:14px system-ui;padding:2rem;max-width:44rem">
<h1>Portal UI not built</h1>
<p>This binary was compiled without the portal bundle. The Go build does not
depend on npm, so that is a normal state for a fresh clone — but it means there
is no page to serve.</p>
<pre>cd gawk-admin/ui &amp;&amp; npm ci &amp;&amp; npm run build</pre>
<p>The API on this listener is unaffected: <code>/api/v1</code> still answers,
and a Ban CR applied with <code>kubectl</code> still enforces.</p>
`

// Handler serves the portal's static assets.
//
// It is mounted last, as the catch-all: `/api/v1`, `/auth/config`, `/healthz`
// and `/readyz` are routed ahead of it, and everything else is either an asset
// or a client-side route.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	_, statErr := fs.Stat(sub, "index.html")
	built := statErr == nil

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No caching. A moderation console showing yesterday's bundle after a
		// redeploy is a console that lies about what it is acting on — and the
		// bundle carries the OIDC flow, so a stale copy is a stale login too.
		// That also removes the only thing content-hashed filenames would have
		// bought, which is why the build emits stable names instead.
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
		// a deep link resolves client-side. The routes are hash-based, and a
		// server never sees a fragment — but the OIDC redirect URI is
		// `origin + pathname`, so the IdP can legitimately bounce the browser
		// back to a path this server has no file for, and that bounce must land
		// on the app rather than on a 404.
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "portal asset missing", http.StatusInternalServerError)
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
