package eventbus

// The dependency-containment rule for the NATS client in gawk-admin
// (docs/51 D8), the mirror of the relay's own. One package owns the client,
// with one owner and one place to look when a connection misbehaves; the API,
// the store and the reconciler see decoded events and nothing else.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlyTheEventBusImportsNATS(t *testing.T) {
	const moduleRoot = "../.."
	forbidden := []string{
		"github.com/nats-io/nats.go",
		"github.com/nats-io/nats-server",
	}
	allowed := map[string]bool{
		"internal/eventbus/eventbus.go":      true,
		"internal/eventbus/eventbus_test.go": true,
		// The insecure switch is a nats.Option, so it lives with the client.
		"internal/eventbus/tls.go": true,
	}

	scanned, offenders := walkImports(t, moduleRoot, forbidden, allowed)
	for _, o := range offenders {
		t.Errorf(`%s imports %s.

Only internal/eventbus may reach a NATS library (docs/51 D8). Everything else
consumes decoded events through the Ingester interface. If this import is
genuinely intended, extend `+"`allowed`"+` above and say why in the review.`, o.file, o.path)
	}
	if scanned < 30 {
		t.Fatalf("scanned only %d Go files — the walk is not reaching the module tree", scanned)
	}
}

// TestWalkCatchesAPlantedImport keeps the walk honest: without it, a walk that
// stopped matching would pass forever.
func TestWalkCatchesAPlantedImport(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "api"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := "package api\n\nimport _ \"github.com/nats-io/nats.go\"\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "api", "planted.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, offenders := walkImports(t, dir, []string{"github.com/nats-io/nats.go"}, nil)
	if len(offenders) != 1 {
		t.Fatalf("planted import not detected: %v", offenders)
	}
}

type offender struct{ file, path string }

func walkImports(t *testing.T, root string, forbidden []string, allowed map[string]bool) (int, []offender) {
	t.Helper()
	fset := token.NewFileSet()
	scanned := 0
	var out []offender
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
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
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(p, bad) && !allowed[rel] {
					out = append(out, offender{rel, p})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return scanned, out
}
