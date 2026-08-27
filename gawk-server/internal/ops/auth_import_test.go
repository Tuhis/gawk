package ops

// The dependency-containment rule for R39's OIDC library (docs/42 §5, and the
// AP3 brief): go-oidc is in this module for ONE reason — the ops listener's
// admin-API auth — and must not spread. go-jose comes in under it (nothing
// here imports it directly any more, so `go mod tidy` marks it indirect) and
// is listed too, because a direct import of it would be a hand-rolled JWS
// verification path by another name. The relay's data plane (transport, hub,
// wire, the moderation contract package) is a security surface whose
// dependency set is a property worth asserting, not a habit.
//
// A source walk rather than `go list -deps`: it needs no toolchain subprocess,
// it names the offending FILE when it fails, and it catches the import the
// moment it is written rather than once it links.
//
// The public oidcroles package is deliberately NOT an exception here. Sharing
// the roles-claim walk with gawk-admin could have widened this rule — a shared
// "verify this token" helper would have had to import go-oidc, and would then
// have pulled the library into a package the whole module (and gawk-admin) can
// import. It does not: oidcroles takes decoded claims (map[string]any) and
// knows nothing about JWTs, so the walk is shared while signature verification
// stays here, behind the one import this test guards. If a future change makes
// oidcroles reach for go-oidc, this test fails — and that failure is the
// design question, not a bookkeeping chore.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlyTheOpsAuthPathImportsAnOIDCLibrary(t *testing.T) {
	// Relative to this package's directory, which `go test` makes the cwd.
	const moduleRoot = "../.."
	forbidden := []string{
		"github.com/coreos/go-oidc",
		"github.com/go-jose/go-jose",
	}
	// The only files allowed to import them, module-root-relative.
	allowed := map[string]bool{
		"internal/ops/auth.go":       true,
		"internal/ops/admin_test.go": true, // the fake issuer harness
	}

	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendor and dot-directories — but never the walk root
			// itself, whose Name() is ".." and would abort the whole walk.
			name := d.Name()
			if path != moduleRoot && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		rel, relErr := filepath.Rel(moduleRoot, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(p, bad) && !allowed[rel] {
					t.Errorf(`%s imports %s.

Only the ops listener's admin-API auth path may reach an OIDC library
(docs/42 §5): the relay's data plane keeps its dependency set, and a token
verified anywhere but there is a second, drifting answer to "is this
credential good?". If this import is genuinely intended, extend `+"`allowed`"+`
above and say why in the review.`, rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	// A walk that silently found nothing would pass forever. The module has
	// well over a hundred Go files; anything near zero means the walk broke.
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files — the walk is not reaching the module tree", scanned)
	}
}
