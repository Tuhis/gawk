package events

// The dependency-containment rule for the JSON Schema validator (docs/52 D7).
//
// github.com/santhosh-tekuri/jsonschema is in this module for ONE reason: the
// tests in this package prove the vectors validate against the schemas. The
// production code never validates — producers marshal typed structs, and the
// tests prove the structs match the schemas — so the validator must never be
// imported from a non-test file anywhere in the module. The relay's
// dependency set is a security property (docs/51 D8), and `go version -m` on
// the built binary showing no `jsonschema` is EC1's acceptance criterion;
// this test is the same fact asserted at the source, where it names the file.
//
// The same shape as internal/ops/auth_import_test.go: a source walk rather
// than `go list -deps`, so it needs no toolchain subprocess and fails the
// moment the import is written.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheValidatorIsTestOnly(t *testing.T) {
	const moduleRoot = ".."
	const forbidden = "github.com/santhosh-tekuri/jsonschema"

	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != moduleRoot && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, forbidden) {
				rel, _ := filepath.Rel(moduleRoot, path)
				t.Errorf(`%s imports %s.

The JSON Schema validator is a TEST dependency (docs/52 D7): production code
marshals typed structs and never validates, and the relay binary must not
link a parser it does not use. Move the validation into a _test.go file.`, filepath.ToSlash(rel), p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d non-test Go files — the walk is not reaching the module tree", scanned)
	}
}
