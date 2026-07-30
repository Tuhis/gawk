//go:build !duckdb

package sqlengine

import (
	"errors"
	"testing"
)

// `go build ./...` on a fresh clone lands here. The console being ON by default
// (UD18) must not mean a laptop build pretends to have an engine.
const compiledExpectation = false

func TestOpenReportsNoEngine(t *testing.T) {
	e, err := Open(Options{Root: t.TempDir()})
	if !errors.Is(err, ErrNoEngine) {
		t.Fatalf("Open = %v, want ErrNoEngine", err)
	}
	if e != nil {
		t.Error("a build with no engine returned one")
	}
}
