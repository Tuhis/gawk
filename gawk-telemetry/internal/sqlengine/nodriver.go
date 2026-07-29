//go:build !duckdb

package sqlengine

// The cgo-free build (§8 Q1's resolution).
//
// `go build ./...` on a fresh clone lands here: no third-party dependency is
// linked, `CGO_ENABLED=0` still holds, and the console — which is on by
// default — says plainly that this deployment has no engine. That is a
// different message from a query error, and the UI renders it as such rather
// than as a broken editor.

// Open reports that no engine is compiled in.
func Open(Options) (Engine, error) { return nil, ErrNoEngine }

// Compiled reports whether this build carries a query engine. The console asks
// before it renders.
func Compiled() bool { return false }

// Views is what WOULD be queryable in a build that has an engine. Answering
// with the catalogue rather than with nothing is deliberate: "there is no
// engine here" and "there is nothing to query" are different facts, and an
// operator on a laptop build should still be able to see which one they hit.
func Views() []ViewDoc { return viewDocs(func(string) bool { return false }) }
