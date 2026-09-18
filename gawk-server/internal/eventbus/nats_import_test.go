package eventbus

// The dependency-containment rule for the NATS client (docs/51 D8), the same
// shape as internal/ops/auth_import_test.go and for the same reason: the
// relay's data plane dependency set is a security property. A NATS client is a
// network client with reconnect logic and TLS; it belongs in one package, with
// one owner, behind a channel, where the media path cannot reach it and it
// cannot reach the media path.
//
// The nats-server module is on this list too. It is an embedded SERVER, pulled
// in as a test dependency here; an import of it anywhere else would mean the
// relay had grown a broker, which is not a thing to discover in review.

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
		"internal/eventbus/eventbus.go":         true,
		"internal/eventbus/eventbus_test.go":    true,
		"internal/eventbus/nats_import_test.go": false, // names them as strings only
	}

	scanned, offenders := walkImports(t, moduleRoot, forbidden, allowed)
	for _, o := range offenders {
		t.Errorf(`%s imports %s.

Only internal/eventbus may reach a NATS library (docs/51 D8). The hooks at the
relay's fan-out points hand it eventbus.Event values on a channel and never
touch the client. If this import is genuinely intended, extend `+"`allowed`"+`
above and say why in the review.`, o.file, o.path)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d Go files — the walk is not reaching the module tree", scanned)
	}
}

// TestWalkCatchesAPlantedImport keeps the walk itself honest: without it, a
// walk that stopped matching would pass forever.
func TestWalkCatchesAPlantedImport(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "hub"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := "package hub\n\nimport _ \"github.com/nats-io/nats.go\"\n"
	if err := os.WriteFile(filepath.Join(dir, "internal", "hub", "planted.go"), []byte(src), 0o600); err != nil {
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
			if path != root && (name == "vendor" || strings.HasPrefix(name, ".")) {
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
