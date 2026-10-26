//go:build duckdb

package sqlengine

// The learned schema: why the views no longer sniff the store on every query.
//
// A view over `read_json_auto(glob, union_by_name=1)` sniffs EVERY file under
// its glob each time it is bound — at CREATE VIEW, and again on every query
// that reads it — because union_by_name is what makes the JSON reader look at
// all files rather than a sample. The isolated engine runs each query in a
// fresh process, so nothing carried over: every query, the probe's pruned
// count included, paid for the whole 30-day tree, twice. On 2026-10-24 a
// rejoin storm doubled the sessions tree to ~4,600 files and the view never
// answered again (gotchas.md).
//
// So each view keeps what its files have said about their columns, on the data
// volume beside the spill directory, keyed by every file's size and mtime. A
// query sniffs only the files that are new or changed since — a handful, in
// steady state — merges their types in (coltype.go), and registers the view
// with that explicit column list, which binds without opening a file. A
// session file is appended to while its session is open and gzipped once,
// after which it never changes; the sweep's rename shows up as a new path.
//
// What this does NOT do is forget: a column whose last file was pruned stays
// in the list, reading NULL. Types only widen, as they did under union_by_name.
//
// Learning a cold store is spread over as many queries as it takes. Each one
// spends at most learnShare of its budget sniffing, saves what it learned, and
// — if files are still unsniffed — fails with errLearning, which says how far
// along it is. A partial column list is never registered: a file the list does
// not cover could hold a field the view would silently not have.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// learnBatch is how many files one sniff reads. Small enough that the batch a
// deadline interrupts is cheap to lose; large enough that the per-statement
// cost does not dominate.
var learnBatch = 64

// learnShare is the fraction of a query's remaining budget it may spend
// learning; the rest is the query's.
const learnShare = 2.0 / 3

// learnedVersion is the on-disk format. A file in any other format, or
// learned by another DuckDB, is relearned from scratch: a DuckDB upgrade can
// sniff the same file differently.
const learnedVersion = 1

// testHookSniff, when set, sees every batch before it is sniffed. Nil in
// production.
var testHookSniff func(files []string)

// learned is one view's schema and the files it covers.
type learned struct {
	Version int    `json:"version"`
	DuckDB  string `json:"duckdb"`
	// Columns is the row type as a STRUCT, unknowns written NULL.
	Columns string `json:"columns"`
	// Files maps each covered file, relative to Root, to [size, mtime ns].
	Files map[string][2]int64 `json:"files"`

	cols *colType
}

// errLearning is a view whose files are not all sniffed yet. It is not a
// failure of the view: the next query carries on from where this one stopped.
type errLearning struct {
	view        string
	done, total int
}

func (e *errLearning) Error() string {
	return fmt.Sprintf("sqlengine: %s: still learning the columns of its files (%d of %d read); the next query continues from there",
		e.view, e.done, e.total)
}

// SchemaDir is where the learned schemas live: a dot-directory at the data
// root, like SpillDir, so no view glob matches it.
func SchemaDir(root string) string { return filepath.Join(root, ".sql-schema") }

// columns is the explicit column list for view name over matches (its glob,
// expanded), learning whatever matches the stored schema does not cover.
// Learning stops at learnShare of the time to ctx's deadline or e.until,
// whichever is nearer.
func (e *engine) columns(ctx context.Context, name string, matches []string) (string, error) {
	l := e.learnedFor(name)
	stopAt := learnDeadline(ctx, e.until)

	type stamped struct {
		path, rel string
		stamp     [2]int64
	}
	var pending []stamped
	covered := make(map[string][2]int64, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			// Gone between the glob and now: the sweep renamed it, or
			// retention pruned it. The new path, if any, is its own match.
			continue
		}
		rel, err := filepath.Rel(e.opts.Root, m)
		if err != nil {
			return "", err
		}
		st := [2]int64{fi.Size(), fi.ModTime().UnixNano()}
		if l.Files[rel] == st || fi.Size() == 0 {
			// An empty file has nothing to say yet, and DuckDB sniffs one as
			// a lone `json` column; it is learned once it has lines.
			covered[rel] = st
			continue
		}
		pending = append(pending, stamped{m, rel, st})
	}
	// A file the schema covered that is gone — pruned, or renamed by the
	// sweep — is dropped from it; its columns stay.
	dirty := false
	for rel := range l.Files {
		if _, still := covered[rel]; !still {
			dirty = true
			break
		}
	}
	for rel := range covered {
		if _, had := l.Files[rel]; !had {
			dirty = true
			break
		}
	}
	l.Files = covered

	total := len(covered) + len(pending)
	for len(pending) > 0 && time.Now().Before(stopAt) {
		n := min(learnBatch, len(pending))
		batch := pending[:n]
		paths := make([]string, n)
		for i, p := range batch {
			paths[i] = p.path
		}
		t, err := e.sniff(ctx, paths)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			e.save(name, l, dirty)
			return "", fmt.Errorf("sqlengine: %s: reading the columns of %d new or changed files: %w", name, n, err)
		}
		l.cols = merge(l.cols, sniffed(t))
		for _, p := range batch {
			l.Files[p.rel] = p.stamp
		}
		pending = pending[n:]
		dirty = true
	}
	e.save(name, l, dirty)
	if len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", &errLearning{view: name, done: total - len(pending), total: total}
	}
	return columnsSpec(l.cols), nil
}

// learnDeadline is when learning stops: learnShare of the time to the nearer
// of ctx's deadline and until. With neither there is no end.
func learnDeadline(ctx context.Context, until time.Time) time.Time {
	end, ok := ctx.Deadline()
	if !until.IsZero() && (!ok || until.Before(end)) {
		end, ok = until, true
	}
	if !ok {
		return time.Now().Add(24 * time.Hour)
	}
	now := time.Now()
	return now.Add(time.Duration(float64(end.Sub(now)) * learnShare))
}

// sniff is DuckDB's verdict on paths, as one STRUCT. hive_partitioning is off:
// the partition keys are the view's, added at scan time, not columns to learn.
func (e *engine) sniff(ctx context.Context, paths []string) (*colType, error) {
	if testHookSniff != nil {
		testHookSniff(paths)
	}
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = sqlString(p)
	}
	rows, err := e.db.QueryContext(ctx, fmt.Sprintf(
		`DESCRIBE SELECT * FROM read_json_auto([%s], hive_partitioning=0, union_by_name=1, ignore_errors=true)`,
		strings.Join(quoted, ", ")))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := &colType{kind: structKind}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		name, _ := cells[0].(string)
		typ, _ := cells[1].(string)
		t, err := parseType(typ)
		if err != nil {
			return nil, err
		}
		out.fields = append(out.fields, colField{name, t})
	}
	return out, rows.Err()
}

// columnsSpec is read_json's `columns` argument for the row type t.
func columnsSpec(t *colType) string {
	parts := make([]string, len(t.fields))
	for i, f := range t.fields {
		parts[i] = sqlString(f.name) + ": " + sqlString(f.t.emit())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// learnedFor is view name's schema: this engine's copy, else the one on disk,
// else nothing learned yet.
func (e *engine) learnedFor(name string) *learned {
	if l, ok := e.learned[name]; ok {
		return l
	}
	fresh := &learned{Version: learnedVersion, DuckDB: e.duckdb, Files: map[string][2]int64{}, cols: &colType{kind: structKind}}
	e.learned[name] = fresh
	b, err := os.ReadFile(filepath.Join(SchemaDir(e.opts.Root), name+".json"))
	if err != nil {
		return fresh
	}
	var l learned
	if json.Unmarshal(b, &l) != nil || l.Version != learnedVersion || l.DuckDB != e.duckdb || l.Files == nil {
		return fresh
	}
	t, err := parseType(l.Columns)
	if err != nil || t.kind != structKind {
		return fresh
	}
	l.cols = t
	e.learned[name] = &l
	return &l
}

// save writes l if it changed. A failed write is not an error: the schema is
// a cache, and the next query relearns what this one could not keep.
func (e *engine) save(name string, l *learned, dirty bool) {
	if !dirty {
		return
	}
	l.Columns = l.cols.String()
	b, err := json.Marshal(l)
	if err != nil {
		return
	}
	dir := SchemaDir(e.opts.Root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if errors.Join(werr, cerr) != nil {
		_ = os.Remove(f.Name())
		return
	}
	if os.Rename(f.Name(), filepath.Join(dir, name+".json")) != nil {
		_ = os.Remove(f.Name())
	}
}
