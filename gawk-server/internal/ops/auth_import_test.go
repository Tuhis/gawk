package ops

// The dependency-containment rule for R39's OIDC library (docs/42 §5, and the
// AP3 brief): go-oidc is in this module for ONE reason — verifying bearer
// tokens on the ops listener's admin API — and must not spread. go-jose comes
// in under it (nothing here imports it directly any more, so `go mod tidy`
// marks it indirect) and is listed too, because a direct import of it would be
// a hand-rolled JWS verification path by another name. The relay's data plane
// (transport, hub, wire, the moderation contract package) is a security
// surface whose dependency set is a property worth asserting, not a habit.
//
// A source walk rather than `go list -deps`: it needs no toolchain subprocess,
// it names the offending FILE when it fails, and it catches the import the
// moment it is written rather than once it links.
//
// R53 (docs/55 D2) is the design question this test used to warn about. The
// verifier is now a public package, gawk-server/oidcauth, shared with
// gawk-admin and gawk-telemetry, and it imports go-oidc — so the whole module
// COULD reach the library through it. The rule is extended rather than
// weakened: oidcauth (and its oidcauthtest subpackage) joins the forbidden
// list, internal/ops/auth.go stays the only relay file allowed to import it,
// and the oidcauth tree itself is the one place go-oidc may be imported
// directly. Transport, hub, wire and the media path still cannot reach a
// verifier, directly or through the shared package, and the failure still
// names the file.
//
// The public oidcroles package is deliberately NOT an exception here: it takes
// decoded claims (map[string]any) and knows nothing about JWTs. If a future
// change makes oidcroles reach for go-oidc or oidcauth, this test fails — and
// that failure is the design question, not a bookkeeping chore.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oidcForbidden are the import-path prefixes only the allowed files may reach.
// A prefix matches the path itself and anything below it.
var oidcForbidden = []string{
	"github.com/coreos/go-oidc",
	"github.com/go-jose/go-jose",
	"github.com/Tuhis/gawk/gawk-server/oidcauth",
}

// oidcAllowedFiles may import any of oidcForbidden, module-root-relative.
var oidcAllowedFiles = map[string]bool{
	"internal/ops/auth.go":       true,
	"internal/ops/admin_test.go": true, // the fake issuer harness (oidcauthtest)
}

// oidcAllowedTree is the one directory whose files may import any of them:
// the shared verifier itself, which is what wraps go-oidc for everyone else.
const oidcAllowedTree = "oidcauth/"

type oidcImport struct {
	file, path string
}

// findOIDCImports walks the Go module rooted at root and returns every import
// of a forbidden path from a file not allowed to make it, plus how many Go
// files it scanned.
func findOIDCImports(root string) ([]oidcImport, int, error) {
	fset := token.NewFileSet()
	var found []oidcImport
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendor, testdata and dot-directories — the toolchain never
			// builds them, and testdata holds this test's own fixture — but
			// never the walk root itself, whose Name() may be ".." and would
			// abort the whole walk.
			name := d.Name()
			if path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if oidcAllowedFiles[rel] || strings.HasPrefix(rel, oidcAllowedTree) {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range oidcForbidden {
				if strings.HasPrefix(p, bad) {
					found = append(found, oidcImport{file: rel, path: p})
				}
			}
		}
		return nil
	})
	return found, scanned, err
}

func TestOnlyTheOpsAuthPathImportsAnOIDCLibrary(t *testing.T) {
	// Relative to this package's directory, which `go test` makes the cwd.
	const moduleRoot = "../.."
	found, scanned, err := findOIDCImports(moduleRoot)
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	for _, imp := range found {
		t.Errorf(`%s imports %s.

Only the ops listener's admin-API auth path may reach an OIDC library, or the
shared verifier that wraps one (docs/42 §5, docs/55 D2): the relay's data
plane keeps its dependency set, and a token verified anywhere but there is a
second, drifting answer to "is this credential good?". If this import is
genuinely intended, extend `+"`oidcAllowedFiles`"+` and say why in the review.`, imp.file, imp.path)
	}
	// A walk that silently found nothing would pass forever. The module has
	// well over a hundred Go files; anything near zero means the walk broke.
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files — the walk is not reaching the module tree", scanned)
	}
}

// The test of the test (docs/55 TO1): a module tree in which a package under
// internal/hub imports oidcauth must FAIL the check, and the failure must name
// that file — while the same tree's allowed files (ops/auth.go, the oidcauth
// package's own go-oidc import) pass. The fixture lives under testdata/, which
// the toolchain never builds and the real walk skips, so the leak it carries
// cannot become a real import.
func TestTheContainmentCheckCatchesAHubPackageImportingOIDCAuth(t *testing.T) {
	found, scanned, err := findOIDCImports("testdata/oidc-containment")
	if err != nil {
		t.Fatalf("walking the fixture: %v", err)
	}
	if scanned != 4 {
		t.Fatalf("scanned %d fixture files, want 4 — the fixture tree is not where the test expects it", scanned)
	}
	want := []oidcImport{
		{file: "internal/hub/leak.go", path: "github.com/Tuhis/gawk/gawk-server/oidcauth"},
		{file: "internal/transport/leak.go", path: "github.com/Tuhis/gawk/gawk-server/oidcauth/oidcauthtest"},
	}
	if len(found) != len(want) {
		t.Fatalf("found %v, want exactly %v", found, want)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("violation %d = %+v, want %+v", i, found[i], want[i])
		}
	}
}
