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

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServesTheEmbeddedPage(t *testing.T) {
	srv := serve(t)
	for _, path := range []string{"/", "/index.html", "/app.js"} {
		status, body := get(t, srv, path)
		if status != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, status)
		}
		if len(body) == 0 {
			t.Errorf("%s served an empty body", path)
		}
	}
}

// TM8's hard constraint: NO external asset fetch. The dashboard must work on a
// port-forward from a laptop with no network, so nothing may reference a CDN,
// a font service or any other origin. Asserted by scanning what is actually
// shipped rather than by trusting the author.
func TestNoExternalAssetReferences(t *testing.T) {
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	err := fs.WalkDir(Assets(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(Assets(), path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			// Comments may legitimately mention a URL-ish thing; what matters
			// is that nothing FETCHES one.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
				strings.HasPrefix(trimmed, "<!--") {
				continue
			}
			if m := external.FindString(line); m != "" {
				t.Errorf("%s references an external origin %q:\n  %s", path, m, trimmed)
			}
		}
		// Nothing may be loaded from a package registry either.
		for _, forbidden := range []string{"cdn.", "unpkg", "jsdelivr", "googleapis", "@import url("} {
			if strings.Contains(string(b), forbidden) {
				t.Errorf("%s references %q", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Severity must never be encoded by colour alone: a glyph AND a text label
// have to be in the DOM for every state, so the page survives a colour-blind
// reader, a greyscale screenshot pasted into a chat, and the CSS failing to
// load.
func TestSeverityIsNeverColourOnly(t *testing.T) {
	b, err := fs.ReadFile(Assets(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	// A glyph per state...
	for _, state := range []string{"ok", "warn", "bad", "unknown"} {
		if !strings.Contains(js, state+":") {
			t.Errorf("no glyph mapping for severity %q", state)
		}
	}
	// ...and the state WORD rendered alongside it.
	if !strings.Contains(js, "GLYPH[s] + ' ' + s") {
		t.Error("severity is not rendered as glyph + word; colour would be carrying it alone")
	}
	// Lifecycle is labelled in words too, and in the past tense once ended.
	if !strings.Contains(js, "'ended '") || !strings.Contains(js, "'LIVE'") {
		t.Error("lifecycle is not labelled in words")
	}
}

// Colour carries exactly two channels: severity → hue, lifecycle → contrast.
// An ended `bad` must keep its red (you can see last night went badly) while
// recession stops it out-shouting a live `warn` above it.
func TestColourChannelsAreSeparate(t *testing.T) {
	b, err := fs.ReadFile(Assets(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if !strings.Contains(css, ".sev-warn") || !strings.Contains(css, ".sev-bad") {
		t.Error("no severity hue classes")
	}
	// ok/unknown spend no hue.
	if strings.Contains(css, ".sev-ok { color: var(--warn)") || strings.Contains(css, ".sev-ok { color: var(--bad)") {
		t.Error("ok spends severity hue")
	}
	// Lifecycle rides contrast (opacity), NOT hue suppression.
	if !strings.Contains(css, ".ended") || !strings.Contains(css, "opacity") {
		t.Error("ended rows are not recessed by contrast")
	}
	if strings.Contains(css, ".ended .sev-bad { color:") {
		t.Error("an ended row's severity hue is overridden; it must keep its red")
	}
}

// The page polls one endpoint rather than holding a connection: no connection
// state to lose, survives any proxy, debuggable with curl.
func TestPagePollsRatherThanStreams(t *testing.T) {
	b, _ := fs.ReadFile(Assets(), "app.js")
	js := string(b)
	if !strings.Contains(js, "setInterval(poll") {
		t.Error("the page does not poll")
	}
	for _, streaming := range []string{"EventSource", "new WebSocket"} {
		if strings.Contains(js, streaming) {
			t.Errorf("the page uses %s; polling was chosen deliberately", streaming)
		}
	}
	// A failed poll must keep the last good state and SAY the feed is stale,
	// rather than blanking the page.
	if !strings.Contains(js, "feed unavailable") {
		t.Error("a failed poll does not surface as a stale feed")
	}
}

// The dashboard is never cached: an ops page showing yesterday's bundle after
// a redeploy is a page that lies about what it is measuring.
func TestAssetsAreNotCached(t *testing.T) {
	srv := serve(t)
	resp, err := http.Get(srv.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
}
