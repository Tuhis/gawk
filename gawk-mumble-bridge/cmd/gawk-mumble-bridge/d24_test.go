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

// D24 ("no voice audio is ever written to disk, logged, recorded or exposed
// by gawk"; docs/80 §11.5) enforced statically across every non-test Go file
// of the module, the gawk-sampler D8 test's shape:
//
//  1. Nothing writes a file. The bridge has no reason to create one, so
//     there is no allow-list: a voice frame could only reach a disk through
//     one of these calls.
//  2. In the packages voice passes through, no audio- or ticket-carrying
//     value is an argument to a logging, formatting or error-building call.
//     Those values live in variables with a fixed set of names (below); a
//     new name for voice bytes or a ticket must be added to the list, which
//     is the point of the list.

var fileWriters = map[string]bool{
	"os.Create": true, "os.CreateTemp": true, "os.OpenFile": true, "os.WriteFile": true,
	"os.Mkdir": true, "os.MkdirAll": true, "os.MkdirTemp": true, "os.Rename": true,
	"os.Link": true, "os.Symlink": true, "ioutil.WriteFile": true, "ioutil.TempFile": true,
}

// voicePackages are where Opus packets, VoiceFrames and tickets travel.
var voicePackages = []string{"internal/mumble/", "internal/bridge/", "cmd/gawk-mumblesim/"}

// voiceNames hold audio bytes, voice packets or tickets in those packages.
var voiceNames = map[string]bool{
	"opus": true, "payload": true, "pkt": true, "packet": true, "p": true, "dg": true, "d": true,
	"buf": true, "frame": true, "body": true, "data": true, "rec": true, "msg": true,
	"hello": true, "ticket": true, "raw": true, "forged": true, "audio": true, "q": true,
}

// voiceFields are struct fields that hold audio or a ticket.
var voiceFields = map[string]bool{"Opus": true, "Ticket": true, "Payload": true}

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
		case "slog", "log":
			return x.Name + "." + name, true
		}
	}
	switch name {
	case "Debug", "Info", "Warn", "Error", "Log", "LogAttrs", "With", "DebugContext", "InfoContext", "WarnContext", "ErrorContext", "Printf", "Println", "Print":
		return "logger." + name, true
	}
	return "", false
}

func voiceValued(expr ast.Expr) (string, bool) {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name, voiceNames[x.Name]
	case *ast.SelectorExpr:
		return x.Sel.Name, voiceFields[x.Sel.Name]
	case *ast.SliceExpr:
		return voiceValued(x.X)
	case *ast.StarExpr:
		return voiceValued(x.X)
	case *ast.UnaryExpr:
		return voiceValued(x.X)
	}
	return "", false
}

func TestNoVoiceBytesWrittenOrLogged(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || d.Name() == "deploy" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/mumble/mumbletest/") {
			return nil // test support: a fake murmur
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		inVoice := false
		for _, p := range voicePackages {
			inVoice = inVoice || strings.HasPrefix(rel, p)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && fileWriters[x.Name+"."+sel.Sel.Name] {
					t.Errorf("%s: %s.%s writes a file (D24: the bridge never does)", fset.Position(call.Pos()), x.Name, sel.Sel.Name)
				}
			}
			if !inVoice {
				return true
			}
			if sink, ok := isSink(call); ok {
				for _, arg := range call.Args {
					if name, ok := voiceValued(arg); ok {
						t.Errorf("%s: %s is passed to %s — voice bytes and tickets never reach a log or an error (D24)", fset.Position(arg.Pos()), name, sink)
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

// The walk must catch what it claims to: a planted violation fails it.
func TestSourceWalkCatchesAViolation(t *testing.T) {
	src := `package x
import ("fmt"; "log/slog"; "os")
func f(opus []byte, p struct{ Opus []byte }, l *slog.Logger) {
	fmt.Printf("%x", opus)
	l.Info("x", "y", p.Opus)
	os.WriteFile("a", opus, 0o600)
}`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sinks, writes := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && fileWriters[x.Name+"."+sel.Sel.Name] {
				writes++
			}
		}
		if _, ok := isSink(call); ok {
			for _, a := range call.Args {
				if _, ok := voiceValued(a); ok {
					sinks++
				}
			}
		}
		return true
	})
	if sinks != 2 || writes != 1 {
		t.Fatalf("caught %d sinks and %d writes, want 2 and 1", sinks, writes)
	}
}
