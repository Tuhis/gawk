package webapp

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// A tree with nothing staged (every fresh checkout) serves the placeholder,
// so `go build ./cmd/gawk-relay` never needs Node.
func TestUnstagedServesPlaceholder(t *testing.T) {
	stage := fstest.MapFS{"dist/.gitkeep": {}}
	app, placeholder := pick(stage)
	if !placeholder {
		t.Fatal("an empty dist must report the placeholder")
	}
	b, err := fs.ReadFile(app, "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	if !strings.Contains(string(b), PlaceholderMarker) {
		t.Fatalf("placeholder index.html lacks the marker:\n%s", b)
	}
	if _, err := fs.Stat(app, ".gitkeep"); err == nil {
		t.Fatal("the placeholder FS exposes .gitkeep")
	}
}

// A staged build is served rooted at dist, and the tracking file stays hidden.
func TestStagedBuildIsServed(t *testing.T) {
	stage := fstest.MapFS{
		"dist/.gitkeep":          {},
		"dist/index.html":        {Data: []byte("<!doctype html><title>gawk</title>")},
		"dist/assets/index-1.js": {Data: []byte("console.log(1)")},
	}
	app, placeholder := pick(stage)
	if placeholder {
		t.Fatal("a staged build must not report the placeholder")
	}
	if _, err := fs.Stat(app, "assets/index-1.js"); err != nil {
		t.Fatalf("assets: %v", err)
	}
	if _, err := fs.Stat(app, ".gitkeep"); err == nil {
		t.Fatal("the staged FS exposes .gitkeep")
	}
	if _, err := fs.Stat(app, "."); err != nil {
		t.Fatalf("root: %v", err)
	}
}

// The real embed always yields an index.html, whichever case this build is,
// and never a static config.js: the console generates that one (docs/76
// §6.1 item 6), and a stale copy in the embed could shadow it.
func TestEmbedHasIndexAndNoStaticConfig(t *testing.T) {
	if _, err := fs.Stat(FS(), "index.html"); err != nil {
		t.Fatalf("embedded app has no index.html: %v", err)
	}
	if _, err := fs.Stat(FS(), "config.js"); err == nil {
		t.Fatal("the embed carries a static config.js; stage.sh must strip it")
	}
}

// Release builds set GAWK_RELAY_REQUIRE_APP=1 after stage.sh, so a binary
// that would ship the placeholder fails CI instead of reaching a party.
func TestReleaseBuildIsNotPlaceholder(t *testing.T) {
	if os.Getenv("GAWK_RELAY_REQUIRE_APP") != "1" {
		t.Skip("GAWK_RELAY_REQUIRE_APP unset: a dev tree may embed the placeholder")
	}
	if Placeholder() {
		t.Fatal("this build embeds the placeholder, not a staged gawk-app build")
	}
	b, err := fs.ReadFile(FS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	// stage.sh builds with a relative base, so the app works under /app/
	// (the console) and at / (the named tier) alike.
	if strings.Contains(string(b), `src="/assets/`) || strings.Contains(string(b), `href="/assets/`) {
		t.Fatalf("index.html references root-absolute assets; build with --base=./:\n%s", b)
	}
	if !strings.Contains(string(b), `src="./config.js"`) {
		t.Fatalf("index.html does not load ./config.js:\n%s", b)
	}
}
