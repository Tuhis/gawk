package dashboard

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
		t.Skip("dashboard bundle not built (cd ui && npm ci && npm run build)")
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

// A deep link that is not a real asset must land on the document so the client
// can route it, rather than on a 404.
func TestUnknownPathsServeTheDocument(t *testing.T) {
	requireBuilt(t)
	srv := serve(t)
	_, index, _ := get(t, srv, "/")
	for _, path := range []string{"/session/abc", "/b/1a2b3c", "/anything"} {
		status, body, _ := get(t, srv, path)
		if status != http.StatusOK || body != index {
			t.Errorf("%s status = %d, served the document = %v; want the SPA fallback",
				path, status, body == index)
		}
	}
}

// An ops page showing yesterday's bundle after a redeploy is a page that lies
// about what it is measuring. This is also why the build emits STABLE asset
// names: content hashing exists to make far-future caching safe, and there is
// no caching here to make safe.
func TestAssetsAreNotCached(t *testing.T) {
	srv := serve(t)
	for _, path := range []string{"/", "/index.html"} {
		_, _, h := get(t, srv, path)
		if cc := h.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s Cache-Control = %q, want no-store", path, cc)
		}
	}
}

// TM8's hard constraint, and the one §4.8.4 decision the SPA does NOT relax:
// no external asset fetch. The page must work on a port-forward from a laptop
// with no network.
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
}

// The committed placeholder is what makes the above true. If it is ever
// removed, `//go:embed dist` stops compiling on a clone that has not run npm —
// a failure that would look like a broken checkout rather than a missing step.
func TestThePlaceholderIsCommitted(t *testing.T) {
	if _, err := fs.Stat(Assets(), "README.md"); err != nil {
		t.Error("dist/README.md is gone; //go:embed dist will not compile without a build")
	}
}
