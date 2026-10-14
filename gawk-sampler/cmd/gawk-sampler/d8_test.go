package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// D8 is enforced statically here (CF5: "a static check that no code path
// writes frame bytes to disk or logs"), across every non-test Go file of the
// module:
//
//  1. Nothing writes a file except fetch-model, which writes the model — the
//     one file the sampler ever creates. A frame can only reach a disk
//     through one of these calls.
//  2. In the packages a frame passes through, no frame-carrying value is an
//     argument to a logging, formatting or error-building call. Frames live
//     in variables with a fixed set of names (the list below); a new name for
//     frame bytes must be added to it, which is the point of the list.
//
// The engine's TestNoFrameBytesInLogsOrMetrics checks the same rule at run
// time, through every failure path.

// fileWriters are the calls that create or write files.
var fileWriters = map[string]bool{
	"os.Create": true, "os.CreateTemp": true, "os.OpenFile": true, "os.WriteFile": true,
	"os.Mkdir": true, "os.MkdirAll": true, "os.MkdirTemp": true, "os.Rename": true,
	"os.Link": true, "os.Symlink": true, "ioutil.WriteFile": true, "ioutil.TempFile": true,
}

// fileWriterAllowed is where a file may be written: fetch-model installs the
// weights.
var fileWriterAllowed = []string{"internal/fetchmodel/"}

// framePackages are the packages a keyframe or a decoded frame passes
// through.
var framePackages = []string{"internal/bitstream/", "internal/decode/", "internal/detect/", "internal/relay/", "internal/sampler/"}

// frameNames are the identifiers (and field names) that hold frame bytes in
// those packages.
var frameNames = map[string]bool{
	"msg": true, "payload": true, "frame": true, "Frame": true, "Data": true, "rgb": true,
	"tensor": true, "data": true, "body": true, "sample": true, "avcc": true, "extradata": true,
	"in": true, "out": true, "k": true, "f": true,
}

// sinks are the calls whose arguments end up in a log line, a formatted
// string or an error message.
func isSink(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	name := sel.Sel.Name
	if x, ok := sel.X.(*ast.Ident); ok {
		switch x.Name {
		case "fmt":
			return "fmt." + name, true
		case "errors":
			return "errors." + name, name == "New"
		case "slog":
			return "slog." + name, true
		}
	}
	switch name {
	case "Debug", "Info", "Warn", "Error", "Log", "LogAttrs", "With", "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
		return "logger." + name, true
	}
	return "", false
}

// frameValued reports whether expr is a frame-named value itself — not
// len(x), not a field of it that is not frame data, not a call result.
func frameValued(expr ast.Expr) (string, bool) {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name, frameNames[x.Name]
	case *ast.SelectorExpr:
		return x.Sel.Name, x.Sel.Name == "Frame" || x.Sel.Name == "Data" || x.Sel.Name == "Extradata"
	case *ast.SliceExpr:
		return frameValued(x.X)
	case *ast.IndexExpr:
		return "", false
	case *ast.StarExpr:
		return frameValued(x.X)
	}
	return "", false
}

func TestNoFrameBytesWrittenOrLogged(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/nativetest/") || strings.HasPrefix(rel, "internal/bitstream/bitstreamtest/") {
			return nil // test support
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		inFramePkg := false
		for _, p := range framePackages {
			inFramePkg = inFramePkg || strings.HasPrefix(rel, p)
		}
		writerOK := false
		for _, p := range fileWriterAllowed {
			writerOK = writerOK || strings.HasPrefix(rel, p)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && fileWriters[x.Name+"."+sel.Sel.Name] && !writerOK {
					t.Errorf("%s: %s.%s writes a file outside fetch-model (D8)", fset.Position(call.Pos()), x.Name, sel.Sel.Name)
				}
			}
			if !inFramePkg {
				return true
			}
			if sink, ok := isSink(call); ok {
				for _, arg := range call.Args {
					if name, ok := frameValued(arg); ok {
						t.Errorf("%s: %s is passed to %s — frame bytes must never reach a log or an error (D8)", fset.Position(arg.Pos()), name, sink)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 15 {
		t.Fatalf("only %d files checked: the walk is not seeing the module", checked)
	}
}
