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
	"os"
	"path/filepath"
	"strconv"
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

	// The engine's budget. It shares a process with public ingest, so what it
	// may use is stated rather than left to DuckDB's defaults — which are
	// ~80 % of the container's memory and one thread per core, and which made
	// every unpruned `sessions` query fail with an OOM (BUGS.md, 2026-08-20).
	//
	// MemoryLimit bounds DuckDB's buffer manager, in bytes; 0 leaves DuckDB's
	// own default, so a caller wanting the container-derived value passes
	// AutoMemoryLimit's result.
	MemoryLimit int64
	// Threads caps DuckDB's worker threads; 0 means DefaultThreads. It is the
	// knob that matters most: the JSON reader holds a 32 MiB buffer per
	// thread, so the thread count alone sets the floor under a scan.
	Threads int
	// SpillLimit caps the spill directory, in bytes; 0 disables spilling. The
	// directory is SpillDir(Root), on the data volume because the image's root
	// filesystem is read-only — which is also why it must be capped: DuckDB's
	// own default is 90 % of the free space on the volume ingest writes to.
	SpillLimit int64
}

// DefaultThreads is the engine's worker count. Two keep an unpruned scan to
// ~64 MiB of reader buffers and still use more than one core; an operator
// console trades latency for not competing with ingest.
const DefaultThreads = 2

// DefaultSpillLimit is the spill cap a deployment gets unless it says
// otherwise.
const DefaultSpillLimit int64 = 512 << 20

// minAutoMemoryLimit is the floor under AutoMemoryLimit: below it DuckDB
// cannot hold DefaultThreads' reader buffers plus a working set.
const minAutoMemoryLimit int64 = 128 << 20

// SpillDir is where the engine spills. A dot-directory at the data root, so no
// store walk (sessions/, relay/, rollups/) ever sees it, and no view glob
// matches it.
func SpillDir(root string) string { return filepath.Join(root, ".sql-spill") }

// AutoMemoryLimit is a quarter of the container's memory limit, read from the
// cgroup filesystem mounted at cgroupRoot (v2 first, then v1), with a floor of
// 128 MiB. It returns 0 when no limit is set, which leaves DuckDB's default in
// place — right for a laptop, and the only honest answer without a limit.
//
// A quarter, not DuckDB's 80 %: the rest of the process is ingest, the live
// projection and the Go heap, and a console query must fail rather than take
// the pod over its limit — an OOMKill there drops public ingest fleet-wide.
func AutoMemoryLimit(cgroupRoot string) int64 {
	for _, f := range []string{
		filepath.Join(cgroupRoot, "memory.max"),
		filepath.Join(cgroupRoot, "memory", "memory.limit_in_bytes"),
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		limit, err := strconv.ParseInt(s, 10, 64)
		// "max" (v2) and v1's page-aligned near-MaxInt64 both mean "no limit".
		if err != nil || limit <= 0 || limit >= 1<<62 {
			return 0
		}
		if q := limit / 4; q > minAutoMemoryLimit {
			return q
		}
		return minAutoMemoryLimit
	}
	return 0
}

// ParseSize reads a byte count as an operator writes one: a bare number of
// bytes, or a number with a B/KB/MB/GB (powers of 1000) or KiB/MiB/GiB
// (powers of 1024) suffix — the same spellings DuckDB accepts.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30},
		{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"b", 1},
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			lower = strings.TrimSpace(strings.TrimSuffix(lower, u.suffix))
			mult = u.mult
			break
		}
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("sqlengine: %q is not a size (want e.g. 512MiB)", s)
	}
	return n * mult, nil
}

func (o Options) threads() int {
	if o.Threads <= 0 {
		return DefaultThreads
	}
	return o.Threads
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
