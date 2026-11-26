package main

// docs/92 D10: gawk-mmproxy never logs a client address and never uses one
// as a metric label. Enforced statically across every non-test Go file of
// the module, in the shape of gawk-mumble-bridge's D24 test:
//
//  1. A client's address exists only in proxyproto (which decodes it) and
//     proxy (which hands it to the kernel). Neither imports a logger or fmt,
//     so neither can print it.
//  2. Nothing else imports proxyproto, so the decoded Header never leaves
//     those two packages.
//  3. Metric labels are typed (metrics.Result, metrics.Pool) and metrics
//     formats only their fixed names.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/Tuhis/gawk/gawk-mmproxy/"

var (
	addressPackages = map[string]bool{"internal/proxyproto": true, "internal/proxy": true}
	printers        = map[string]bool{"log": true, "log/slog": true, "fmt": true}
)

func TestNoClientAddressIsLoggedOrLabelled(t *testing.T) {
	root := "../.."
	files := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if addressPackages[rel] && printers[p] {
				t.Errorf("%s imports %s: the packages a client address passes through must not be able to print it (D10)", path, p)
			}
			if p == module+"internal/proxyproto" && !addressPackages[rel] {
				t.Errorf("%s imports proxyproto: the decoded header must not leave proxyproto and proxy (D10)", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 8 {
		t.Fatalf("walked only %d files; is the root right?", files)
	}
}
