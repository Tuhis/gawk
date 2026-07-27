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
	// ok never wears a PROBLEM hue...
	if strings.Contains(css, ".sev-ok { color: var(--warn)") || strings.Contains(css, ".sev-ok { color: var(--bad)") {
		t.Error("ok spends a problem hue")
	}
	// ...but it is not the same colour as unknown either. "We checked and it is
	// healthy" and "nothing has ever reported" are different claims, and the
	// page's cardinal rule is that the second must never read as the first.
	// They shared one grey until 2026-07-27.
	if !strings.Contains(css, ".sev-ok { color: var(--ok); }") {
		t.Error("ok does not carry its own hue")
	}
	if !strings.Contains(css, ".sev-unknown { color: var(--dim); }") {
		t.Error("unknown should stay neutral grey — it is the absence of an answer")
	}
	// Both themes define it, or one of them renders ok as an unstyled default.
	if strings.Count(css, "--ok:") < 2 {
		t.Error("--ok is not defined for both the dark and light themes")
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

// The page rebuilds every card on each 2 s poll, which was snapping an expanded
// card shut under whoever was reading it — a session table takes longer than
// two seconds to read, so the detail view was effectively unreachable.
//
// This is a SOURCE-level guard, and deliberately a shallow one: this module has
// no JS runtime in its harness, so nothing here executes the page. It exists to
// fail loudly if the mechanism is deleted or the naming drifts. The behaviour
// itself was verified in a real browser against the served asset (expand
// survives repeated polls; a collapsed `bad` card stays collapsed; the override
// dissolves once the default agrees, so a recovered broadcast follows severity
// again; the map is pruned when a broadcast ages out).
func TestExpandedCardsSurviveARefresh(t *testing.T) {
	b, _ := fs.ReadFile(Assets(), "app.js")
	js := string(b)

	// The state is carried across the rebuild...
	if !strings.Contains(js, "captureOpenState") {
		t.Error("nothing reads the open/collapsed state off the outgoing DOM before the rebuild")
	}
	if !strings.Contains(js, "openOverrides") {
		t.Error("no store for the operator's expand/collapse choices")
	}
	// ...as a DISAGREEMENT with the severity default, not as raw open-state.
	// Storing the latter would pin a card open long after its fault cleared,
	// defeating the severity-driven default the whole page is built around.
	if !strings.Contains(js, "data-default") && !strings.Contains(js, "dataset.default") {
		t.Error("the default is not recorded on the card, so an override cannot be told from an untouched card")
	}
	// And it is bounded: one entry per broadcast that is still on the page.
	if !strings.Contains(js, "openOverrides.delete") {
		t.Error("the override map is never pruned")
	}
}

// The find-a-stream box hands the server a join credential, so the shape of the
// request is a security property, not a style choice: a code in a query string
// lands in browser history, the Referer header and every proxy log in between.
//
// Source-level guard, like the one above; the behaviour (resolve, highlight,
// survive a rebuild, hide itself when the server answers 501) was verified in a
// real browser against the served asset.
func TestFindByCodeNeverPutsTheCodeInAURL(t *testing.T) {
	b, _ := fs.ReadFile(Assets(), "app.js")
	js := string(b)

	if !strings.Contains(js, "v1/resolve") {
		t.Fatal("the page does not call the resolve endpoint")
	}
	if !strings.Contains(js, "method: 'POST'") {
		t.Error("resolve is not called with POST; the code would travel in the URL")
	}
	// The give-away shapes of a code pasted into a URL.
	for _, bad := range []string{"v1/resolve?", "resolve?code", "encodeURIComponent(code)"} {
		if strings.Contains(js, bad) {
			t.Errorf("found %q: the broadcast code must never enter a query string", bad)
		}
	}
	// And the resolved value is the digest the page already shows everywhere,
	// never the code itself, held in page state.
	if strings.Contains(js, "foundCode") {
		t.Error("the raw code is being held in page state; keep the digest instead")
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
