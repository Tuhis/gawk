// Package webapp is the gawk-app build that gawk-relay serves to the host's
// own machine at http://127.0.0.1:4480/app/ (docs/76 §6.1 item 6) and, on
// the named tier, over HTTPS on -ui-addr (§6.11 item 39).
//
// The build is staged into dist/ by stage.sh — the release image and the
// CI binaries run it — and is never committed. A fresh checkout has only
// dist/.gitkeep, and then FS serves a placeholder page instead, so
// `go build ./cmd/gawk-relay` works without Node and a dev build says what
// it is rather than serving a blank page.
//
// The staged build is made with a relative base (`vite build --base=./`), so
// the same bytes work under /app/ and at /. Its static config.js is removed:
// the console generates config.js per request (relayUrl, relayCertHashes,
// requirePublishSecret: false).
package webapp

import (
	"embed"
	"io/fs"
	"strings"
)

// all: so the pattern matches when dist holds only .gitkeep — a plain
// `dist` pattern refuses a directory with no embeddable files.
//
//go:embed all:dist
var staged embed.FS

//go:embed placeholder
var placeholder embed.FS

// PlaceholderMarker is in the placeholder's index.html and nowhere in a real
// gawk-app build. Placeholder() keys on the file's presence instead; the
// marker is for a human (or a script) looking at a served page.
const PlaceholderMarker = `<meta name="gawk-relay-webapp" content="placeholder">`

var (
	appFS         fs.FS
	isPlaceholder bool
)

func init() { appFS, isPlaceholder = pick(staged) }

// FS returns the web app rooted at its index.html: the staged gawk-app
// build, or the placeholder when none was staged.
func FS() fs.FS { return appFS }

// Placeholder reports whether this binary embeds the placeholder rather
// than a gawk-app build. The console shows it, and a release build's test
// refuses it (GAWK_RELAY_REQUIRE_APP).
func Placeholder() bool { return isPlaceholder }

// pick is FS's decision, over any tree shaped like the embed.
func pick(stage fs.FS) (fs.FS, bool) {
	if _, err := fs.Stat(stage, "dist/index.html"); err != nil {
		return mustSub(placeholder, "placeholder"), true
	}
	return hideDotfiles{mustSub(stage, "dist")}, false
}

// mustSub panics only on an invalid path, and both callers pass a literal.
func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// hideDotfiles keeps dist/.gitkeep (and any other dotfile all: pulled in)
// out of what is served.
type hideDotfiles struct{ fs.FS }

func (h hideDotfiles) Open(name string) (fs.File, error) {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") && part != "." {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
	}
	return h.FS.Open(name)
}
