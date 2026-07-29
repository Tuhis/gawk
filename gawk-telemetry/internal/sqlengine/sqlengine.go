// Package sqlengine is TH10's ad-hoc query surface (docs/36 UD18, §8 Q1).
//
// # Why this package is split by a build tag
//
// The owner's decision was a SQL console **on by default**. The constraint
// attached to it is not a flag flip:
//
//   - `gawk-telemetry/go.mod` had zero third-party dependencies, and the image
//     built `CGO_ENABLED=0` into `distroless/static-debian12`.
//   - Every usable Go DuckDB driver is cgo.
//
// The resolution (§8 Q1, taken 2026-07-29) is a build tag. `go build ./...` on
// a fresh clone compiles the stub in `nodriver.go`, stays cgo-free, and the
// console reports itself unavailable — plainly, rather than rendering a broken
// editor. The DEPLOYED image is built `-tags duckdb` with cgo and a base that
// carries a libc, so the decision is delivered where it was asked for without
// making a laptop build depend on a C toolchain.
//
// # Why an allowlist rather than a read-only connection
//
// The engine must read the NDJSON partitions, so DuckDB's external-access
// switch has to stay on — which means read-only mode cannot be what stops a
// `COPY … TO` from writing over the data directory. The statement allowlist is
// what does. It is deliberately crude: one statement, and it must open with a
// verb that only reads. That refuses far more than it needs to, which is the
// correct bias for a console whose alternative is `curl` and a shell.
package sqlengine

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoEngine reports a build with no engine compiled in. Distinct from a
// query error: the console renders it as "this deployment has no engine",
// never as "your query was wrong".
var ErrNoEngine = errors.New("sqlengine: no query engine is compiled into this build (rebuild with -tags duckdb)")

// ErrRefused reports a statement the allowlist rejected.
var ErrRefused = errors.New("sqlengine: refused")

// DefaultRowLimit bounds a result set. A console answer a human reads, not an
// export path.
const DefaultRowLimit = 5000

// DefaultTimeout bounds a query. Long enough for a scan of a month of
// partitions, short enough that a cartesian accident does not pin a core on the
// pod that is also carrying ingest.
const DefaultTimeout = 30 * time.Second

// Result is one query's answer, shaped so the UI can either table it or feed it
// straight to a chart component (TH10: "an ad-hoc query is plottable rather
// than a table of numbers").
type Result struct {
	Columns   []string  `json:"columns"`
	Types     []string  `json:"types,omitempty"`
	Rows      [][]any   `json:"rows"`
	RowCount  int       `json:"rowCount"`
	Truncated bool      `json:"truncated,omitempty"`
	ElapsedMs int64     `json:"elapsedMs"`
	Views     []ViewDoc `json:"views,omitempty"`
}

// ViewDoc describes one registered view, so the console can say what is
// queryable without the operator having to know the on-disk layout.
type ViewDoc struct {
	Name string `json:"name"`
	Desc string `json:"description"`
	// Available is false when the underlying partition tree is empty or
	// unreadable — a view that could not be registered is stated, never
	// silently absent.
	Available bool `json:"available"`
}

// Engine runs read-only queries over the store's partitions.
type Engine interface {
	// Query runs one statement. It must return ErrRefused for anything the
	// allowlist rejects and ErrNoEngine where nothing is compiled in.
	Query(sql string) (*Result, error)
	// Views lists what is queryable.
	Views() []ViewDoc
	// Close releases the engine.
	Close() error
}

// Options configure an engine.
type Options struct {
	// Root is the data directory the views are registered over.
	Root string
	// RowLimit and Timeout default to the constants above.
	RowLimit int
	Timeout  time.Duration
}

// readOnlyVerbs is the allowlist. Everything DuckDB can use to write, attach,
// install or execute is absent by construction rather than by enumeration —
// which is the right way round, because the set of write verbs grows with the
// engine and the set of read verbs does not.
var readOnlyVerbs = []string{"select", "with", "describe", "summarize", "show", "explain", "table", "from", "pivot"}

// Check applies the allowlist and returns the statement to run.
//
// Exported and tested independently of any driver, so the refusal rules hold in
// a build that has no engine at all — the alternative is a security-relevant
// rule that only exists in the configuration nobody runs locally.
func Check(sql string) (string, error) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSuffix(s, ";")
	if s == "" {
		return "", fmt.Errorf("%w: empty query", ErrRefused)
	}
	// One statement. A trailing semicolon is stripped above; anything else means
	// a second statement is hiding behind the first, which is how an allowlist
	// on the FIRST verb gets walked straight past.
	if strings.Contains(s, ";") {
		return "", fmt.Errorf("%w: one statement per query", ErrRefused)
	}
	first := strings.ToLower(strings.Fields(s)[0])
	// A leading parenthesis is a bracketed SELECT; peel it before matching so
	// `(SELECT …) UNION …` is not refused for punctuation.
	first = strings.TrimLeft(first, "(")
	for _, v := range readOnlyVerbs {
		if first == v {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: %q is not a read-only statement; this console runs SELECT and friends only", ErrRefused, first)
}

func (o Options) rowLimit() int {
	if o.RowLimit <= 0 {
		return DefaultRowLimit
	}
	return o.RowLimit
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

// viewDocs is the catalogue the console shows. Shared by both builds so the
// stub can still tell an operator what WOULD be queryable, which is the
// difference between "not available here" and "broken".
func viewDocs(available func(string) bool) []ViewDoc {
	defs := []struct{ name, desc string }{
		{"sessions", "Every stored per-sample line: kind, sessionId, broadcastKey, role, tMs, receivedAtMs and the whole stats object. Hive-partitioned by date and broadcast."},
		{"rollups", "One permanent row per finished session: identity, config, series percentiles, counters, episodes, the relay join and the stored verdict."},
		{"relay", "Scraped relay observations, hive-partitioned by date, one file per pod."},
		{"annotations", "Operator notes (TH8). Permanent, and the only rows here a human wrote."},
	}
	out := make([]ViewDoc, 0, len(defs))
	for _, d := range defs {
		ok := true
		if available != nil {
			ok = available(d.name)
		}
		out = append(out, ViewDoc{Name: d.name, Desc: d.desc, Available: ok})
	}
	return out
}
