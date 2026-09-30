//go:build !duckdb

package sqlengine

import (
	"errors"
	"testing"
)

// `go build ./...` on a fresh clone lands here. The console being ON by default
// (UD18) must not mean a laptop build pretends to have an engine.
const compiledExpectation = false

// The service opens the isolated engine; it must say "no engine" the same way,
// without spawning a worker that could only say it second-hand.
func TestOpenIsolatedReportsNoEngine(t *testing.T) {
	e, err := OpenIsolated(Options{Root: t.TempDir()})
	if !errors.Is(err, ErrNoEngine) {
		t.Fatalf("OpenIsolated = %v, want ErrNoEngine", err)
	}
	if e != nil {
		t.Error("a build with no engine returned one")
	}
}

func TestOpenReportsNoEngine(t *testing.T) {
	e, err := Open(Options{Root: t.TempDir()})
	if !errors.Is(err, ErrNoEngine) {
		t.Fatalf("Open = %v, want ErrNoEngine", err)
	}
	if e != nil {
		t.Error("a build with no engine returned one")
	}
}
