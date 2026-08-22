package portal

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// built reports whether the Vite bundle is present. `go build` deliberately
// does not depend on npm, so a fresh clone compiles and serves a "not built"
// page — and the asset-level tests below have nothing to assert against.
func built() bool {
	_, err := fs.Stat(Assets(), "index.html")
	return err == nil
}

func requireBuilt(t *testing.T) {
	t.Helper()
	if !built() {
		t.Skip("portal bundle not built (cd ui && npm ci && npm run build)")
	}
}

func TestServesTheEmbeddedPage(t *testing.T) {
	requireBuilt(t)
	srv := serve(t)
	for _, path := range []string{"/", "/index.html", "/assets/app.js", "/assets/app.css"} {
		status, body, _ := get(t, srv, path)
		if status != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, status)
		}
		if len(body) == 0 {
			t.Errorf("%s served an empty body", path)
		}
	}
}

// A path that is not a real asset must land on the document so the client can
// route it. This matters more here than for a read-only dashboard: the OIDC
// redirect URI is `origin + pathname` (docs/42 §4.8), so the identity provider
// bounces the browser back to a path with a `?code=` query, and that bounce has
// to reach the app rather than a 404.
func TestUnknownPathsServeTheDocument(t *testing.T) {
	requireBuilt(t)
	srv := serve(t)
	_, index, _ := get(t, srv, "/")
	for _, path := range []string{"/broadcasts", "/callback?code=abc&state=xyz", "/anything"} {
		status, body, _ := get(t, srv, path)
		if status != http.StatusOK || body != index {
			t.Errorf("%s status = %d, served the document = %v; want the SPA fallback",
				path, status, body == index)
		}
	}
}

// A console showing yesterday's bundle after a redeploy is a console that lies
// about what it is acting on — and this bundle carries the OIDC flow, so a
// stale copy is a stale login too. This is also why the build emits STABLE
// asset names: content hashing exists to make far-future caching safe, and
// there is no caching here to make safe.
func TestAssetsAreNotCached(t *testing.T) {
	srv := serve(t)
	for _, path := range []string{"/", "/index.html"} {
		_, _, h := get(t, srv, path)
		if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s Cache-Control = %q, want no-store", path, cc)
		}
	}
}

// The embedded-assets rule (docs/42 §4.8, §4.9), ported from telemetry's
// dashboard test — and here it is a security property, not just an offline
// nicety. The portal's CSP is `default-src 'self'; connect-src 'self'
// <issuer>`: the identity provider is the ONLY sanctioned external origin. A
// stray CDN reference would not degrade gracefully, it would be blocked, and
// the page would fail to render for an operator who is trying to end a
// broadcast from a phone.
//
// Asserted where it actually matters — the document, which is the only place a
// browser can be told to go and get something before any of our code runs. A
// bundled React does contain absolute URLs (XML namespace identifiers, and a
// react.dev link inside an error message), and none of those is a fetch, so a
// blanket regex over the bundle would fail on things that are not the hazard.
func TestNoExternalAssetReferences(t *testing.T) {
	requireBuilt(t)
	b, err := fs.ReadFile(Assets(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)

	// Every src/href in the document must be relative.
	refs := regexp.MustCompile(`(?i)(?:src|href)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(html, -1)
	if len(refs) == 0 {
		t.Fatal("the document references no assets at all; the bundle is not wired in")
	}
	for _, m := range refs {
		if strings.HasPrefix(m[1], "http://") || strings.HasPrefix(m[1], "https://") ||
			strings.HasPrefix(m[1], "//") {
			t.Errorf("document references an absolute URL %q", m[1])
		}
	}

	// And nothing anywhere in the shipped output may reach a package registry
	// or font service, which is what a stray CDN import would look like.
	err = fs.WalkDir(Assets(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(Assets(), p)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{
			"cdn.jsdelivr", "unpkg.com", "cdnjs.", "fonts.googleapis", "fonts.gstatic",
			"esm.sh", "@import url(http",
		} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s references %q", p, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// No identity provider may be baked into the bundle. The issuer is a knob
// (`-oidc-issuer`) that the SPA learns at runtime from `/auth/config`, so a
// hard-coded one here would be a deployment silently pointing at somebody
// else's IdP — and it would also break the CSP, whose `connect-src` is built
// from the configured issuer at serve time (docs/42 §4.8).
func TestNoIdentityProviderIsCompiledIn(t *testing.T) {
	requireBuilt(t)
	err := fs.WalkDir(Assets(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		body, err := fs.ReadFile(Assets(), p)
		if err != nil {
			return err
		}
		// The bundle must reach the IdP only through paths it builds from the
		// runtime issuer; the well-known suffix may appear, an absolute issuer
		// URL may not.
		for _, forbidden := range []string{
			"https://accounts.google.com", "https://login.microsoftonline.com",
			"://keycloak", "https://auth.",
		} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s hard-codes an identity provider (%q)", p, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The Go build must never depend on npm having been run. A fresh clone
// compiles, serves, and says plainly what is missing — rather than failing to
// build, or serving a blank page that reads as a bug.
func TestServesAnHonestPageWhenTheBundleIsMissing(t *testing.T) {
	if built() {
		t.Skip("bundle is present; this covers the fresh-clone path")
	}
	srv := serve(t)
	status, body, _ := get(t, srv, "/")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "not built") {
		t.Error("the page does not say the UI is unbuilt")
	}
	if !strings.Contains(body, "gawk-admin/ui") {
		t.Error("the page does not name the build command")
	}
}

// The committed placeholder is what makes the above true. If it is ever
// removed, `//go:embed dist` stops compiling on a clone that has not run npm —
// a failure that would look like a broken checkout rather than a missing step.
func TestThePlaceholderIsCommitted(t *testing.T) {
	if _, err := fs.Stat(Assets(), "README.md"); err != nil {
		t.Error("dist/README.md is gone; //go:embed dist will not compile without a build")
	}
}

// No cookie may be set on any portal response (D17, §5). The portal has no
// server-side session and no CSRF machinery precisely because nothing here ever
// sets one; a `Set-Cookie` from the static handler would quietly reintroduce
// the surface the design removed.
func TestNoCookieIsEverSet(t *testing.T) {
	srv := serve(t)
	for _, path := range []string{"/", "/index.html", "/assets/app.js", "/deep/link"} {
		_, _, h := get(t, srv, path)
		if v := h.Values("Set-Cookie"); len(v) > 0 {
			t.Errorf("%s set a cookie: %v", path, v)
		}
	}
}
